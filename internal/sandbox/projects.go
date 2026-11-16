package sandbox

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

// Project is a project folder and the files that belong to it: its saved
// settings and its allowlist next to it, and its folder in an env directory.
type Project struct {
	Name string
	// State is the project folder, absolute and resolved.
	State string
	// Env is its folder in the env directory, when it has one there.
	Env string
}

// OpenProject finds the project in the folder state; envDir is the env
// directory to look for its env folder in ("" for none).
func OpenProject(state, envDir string) (Project, error) {
	absolute, err := filepath.Abs(expandHome(state))
	if err != nil {
		return Project{}, err
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		absolute = resolved
	}
	if !IsProject(absolute) {
		return Project{}, fmt.Errorf("no project in %s", absolute)
	}
	project := Project{Name: filepath.Base(absolute), State: absolute}
	if envDir != "" {
		if env := EnvFolder(expandHome(envDir), absolute); isDirectory(env) {
			project.Env = env
		}
	}
	return project, nil
}

// IsProject says whether a folder is a project: it has the layout
// PrepareState makes.
func IsProject(folder string) bool {
	return isDirectory(filepath.Join(folder, "sessions")) && isDirectory(filepath.Join(folder, "home"))
}

func isDirectory(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// Settings is the project's settings file.
func (p Project) Settings() string { return SettingsFile(p.State) }

// Allowlist is the project's default allowlist file.
func (p Project) Allowlist() string { return DefaultAllowlist(p.State) }

// files are the project's paths and where each goes when the project's
// folder becomes target (and its env folder envTarget).
func (p Project) files(target, envTarget string) [][2]string {
	moves := [][2]string{
		{p.State, target},
		{p.Settings(), SettingsFile(target)},
		{p.Allowlist(), DefaultAllowlist(target)},
	}
	if p.Env != "" {
		moves = append(moves, [2]string{p.Env, envTarget})
	}
	return moves
}

// Running are the project's sandboxes that run on this computer.
func (p Project) Running() []Running {
	var result []Running
	for _, running := range ListRunning() {
		state := running.State
		if resolved, err := filepath.EvalSymlinks(state); err == nil {
			state = resolved
		}
		if state == p.State {
			result = append(result, running)
		}
	}
	return result
}

// checkStopped refuses to change a project while a sandbox uses it.
func (p Project) checkStopped(what string) error {
	if running := p.Running(); len(running) > 0 {
		return fmt.Errorf("cannot %s the project %s while a sandbox uses it (process %d); stop it first",
			what, p.Name, running[0].PID)
	}
	return nil
}

// LastStarted is when the project's sandbox last started (its settings were
// saved), else when its folder last changed.
func (p Project) LastStarted() time.Time {
	for _, path := range []string{p.Settings(), p.State} {
		if info, err := os.Stat(path); err == nil {
			return info.ModTime()
		}
	}
	return time.Time{}
}

// ListProjects lists the projects in a projects directory, by name.
func ListProjects(directory, envDir string) ([]Project, error) {
	entries, err := os.ReadDir(expandHome(directory))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var projects []Project
	for _, entry := range entries {
		path := filepath.Join(expandHome(directory), entry.Name())
		if !isDirectory(path) || !IsProject(path) {
			continue
		}
		if project, err := OpenProject(path, envDir); err == nil {
			projects = append(projects, project)
		}
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].Name < projects[j].Name })
	return projects, nil
}

// destination checks that a project can be made at target, and returns the
// absolute target and its env folder.
func (p Project) destination(target, envDir string) (string, string, error) {
	absolute, err := filepath.Abs(expandHome(target))
	if err != nil {
		return "", "", err
	}
	outside := func(path string) error {
		if path == p.State {
			return fmt.Errorf("%s is the project itself", path)
		}
		if rel, err := filepath.Rel(p.State, path); err == nil && filepath.IsLocal(rel) {
			return fmt.Errorf("%s is inside the project %s", path, p.State)
		}
		return nil
	}
	// Before making its parent, and once resolved like the project's own path.
	if err := outside(absolute); err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
		return "", "", err
	}
	if parent, err := filepath.EvalSymlinks(filepath.Dir(absolute)); err == nil {
		absolute = filepath.Join(parent, filepath.Base(absolute))
	}
	if err := outside(absolute); err != nil {
		return "", "", err
	}
	envTarget := ""
	if p.Env != "" {
		envTarget = EnvFolder(filepath.Dir(p.Env), absolute)
		if envDir != "" {
			envTarget = EnvFolder(expandHome(envDir), absolute)
		}
	}
	for _, move := range p.files(absolute, envTarget) {
		if exists(move[0]) && exists(move[1]) {
			return "", "", fmt.Errorf("%s already exists", move[1])
		}
	}
	return absolute, envTarget, nil
}

// CopyProject copies the project to the folder target, with its settings,
// allowlist and env folder. The copy gets a new key and certificate, so
// links to the original do not open it, and no port, which the original
// keeps. Runs' connection files and keys (instances/, run/) are left out.
func CopyProject(p Project, target, envDir string) (copied Project, err error) {
	if err := p.checkStopped("copy"); err != nil {
		return Project{}, err
	}
	target, envTarget, err := p.destination(target, envDir)
	if err != nil {
		return Project{}, err
	}
	// A failed copy leaves nothing behind.
	defer func() {
		if err != nil {
			for _, path := range []string{target, SettingsFile(target), DefaultAllowlist(target), envTarget} {
				if path != "" {
					removeTree(path)
				}
			}
		}
	}()
	if err := copyTree(p.State, target, map[string]bool{"instances": true, "run": true}); err != nil {
		return Project{}, fmt.Errorf("copying the project: %w", err)
	}
	if exists(p.Allowlist()) {
		if err := copyFile(p.Allowlist(), DefaultAllowlist(target)); err != nil {
			return Project{}, err
		}
	}
	if p.Env != "" {
		if err := copyTree(p.Env, envTarget, nil); err != nil {
			return Project{}, fmt.Errorf("copying the project's env folder: %w", err)
		}
	}
	if settings, err := ReadSettings(p.Settings()); err == nil {
		settings.Port = 0
		if err := settings.Renew(); err != nil {
			return Project{}, err
		}
		if err := WriteSettings(SettingsFile(target), filepath.Base(target), settings); err != nil {
			return Project{}, err
		}
	} else if !errors.Is(err, ErrNoSettings) {
		return Project{}, err
	}
	return Project{Name: filepath.Base(target), State: target, Env: envTarget}, nil
}

// MoveProject moves (or renames) the project to the folder target, with its
// settings, allowlist and env folder. Its key and port stay: links to it
// still open it.
func MoveProject(p Project, target, envDir string) (Project, error) {
	if err := p.checkStopped("move"); err != nil {
		return Project{}, err
	}
	target, envTarget, err := p.destination(target, envDir)
	if err != nil {
		return Project{}, err
	}
	for _, move := range p.files(target, envTarget) {
		if !exists(move[0]) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(move[1]), 0o700); err != nil {
			return Project{}, err
		}
		if err := moveTree(move[0], move[1]); err != nil {
			return Project{}, fmt.Errorf("moving %s: %w", move[0], err)
		}
	}
	return Project{Name: filepath.Base(target), State: target, Env: envTarget}, nil
}

// RemoveProject deletes the project: its folder (chats, logins, workspace,
// tasks), settings, allowlist and env folder.
func RemoveProject(p Project) error {
	if err := p.checkStopped("remove"); err != nil {
		return err
	}
	paths := []string{p.Settings(), p.Allowlist()}
	if p.Env != "" {
		paths = append(paths, p.Env)
	}
	// The folder last, so a failure leaves something to remove again.
	paths = append(paths, p.State)
	for _, path := range paths {
		if err := removeTree(path); err != nil {
			return err
		}
	}
	return nil
}

// moveTree renames a path, or copies and removes it across file systems.
func moveTree(source, target string) error {
	err := os.Rename(source, target)
	if err == nil || !errors.Is(err, syscall.EXDEV) {
		return err
	}
	if err := copyTree(source, target, nil); err != nil {
		removeTree(target)
		return err
	}
	return removeTree(source)
}

// removeTree removes a path like os.RemoveAll, also below read-only folders
// (Go's module cache, for one, makes its folders read-only).
func removeTree(path string) error {
	err := os.RemoveAll(path)
	if err == nil {
		return nil
	}
	_ = filepath.WalkDir(path, func(child string, entry fs.DirEntry, walkErr error) error {
		if walkErr == nil && entry.IsDir() {
			_ = os.Chmod(child, 0o700)
		}
		return nil
	})
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("%w\nFiles the sandbox's engine made as another user (root, or a Podman sub-user) "+
			"may need that user to remove them", err)
	}
	return nil
}

// copyTree copies a folder with its permissions and links; skip names
// top-level folders that are copied empty.
func copyTree(source, target string, skip map[string]bool) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	type folder struct {
		path string
		mode fs.FileMode
	}
	var folders []folder
	err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if parent := filepath.Dir(rel); parent != "." && skip[parent] {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		destination := filepath.Join(target, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, destination)
		case entry.IsDir():
			// Writable while copying into it; its own mode once its contents are in.
			folders = append(folders, folder{destination, info.Mode().Perm()})
			return os.Mkdir(destination, 0o700)
		case info.Mode().IsRegular():
			return copyFile(path, destination)
		default:
			// Sockets and pipes belong to a run, not the project.
			return nil
		}
	})
	if err != nil {
		return err
	}
	for index := len(folders) - 1; index >= 0; index-- {
		if err := os.Chmod(folders[index].path, folders[index].mode); err != nil {
			return err
		}
	}
	return nil
}

// copyFile copies a regular file with its permissions.
func copyFile(source, target string) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm()|0o200)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(target, info.Mode().Perm())
}
