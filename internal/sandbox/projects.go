package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"codeflower-app/internal/jobs"
)

// Project is a project folder and the files that belong to it: its saved
// settings and its allowlist next to it, and its folder in an env directory.
type Project struct {
	Name string
	// State is the project folder, absolute and resolved.
	State string
	// Env is its folder in the env directory, when it has one there.
	Env string
	// Workspace is its workspace folder in a workspace directory, when its
	// settings put it there; else its workspace is in State.
	Workspace string
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
	project.Workspace = savedWorkspace(absolute)
	return project, nil
}

// savedWorkspace is the project's workspace folder elsewhere, when its
// settings name one and it is there.
func savedWorkspace(state string) string {
	dir, err := SavedWorkspaceDir(state)
	if err != nil || dir == "" {
		return ""
	}
	if workspace := WorkspaceFolder(dir, state); exists(workspace) {
		return workspace
	}
	return ""
}

// ProjectExists says whether the folder dir (~ expanded) is a project yet.
func ProjectExists(dir string) bool {
	return IsProject(expandHome(dir))
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

// Jobs is the project's jobs folder (its batch jobs' ledger).
func (p Project) Jobs() string { return jobs.LedgerDir(p.State) }

// checkNoActiveJobs refuses to move or remove a project whose batch jobs
// still run: they write into its folders.
func (p Project) checkNoActiveJobs(what string) error {
	records, _ := jobs.Ledger{Dir: p.Jobs(), Project: p.State}.Records()
	active := 0
	for _, record := range records {
		if jobs.Active(record.State) {
			active++
		}
	}
	if active > 0 {
		return fmt.Errorf("cannot %s the project %s while %d of its batch jobs are queued or running; "+
			"cancel them (codeflower jobs cancel) or wait for them, then check with codeflower jobs list", what, p.Name, active)
	}
	return nil
}

// files are the project's paths and where each goes when the project's
// folder becomes target (and its env and workspace folders envTarget and
// workspaceTarget).
func (p Project) files(target, envTarget, workspaceTarget string) [][2]string {
	moves := [][2]string{
		{p.State, target},
		{p.Settings(), SettingsFile(target)},
		{p.Allowlist(), DefaultAllowlist(target)},
		{p.Jobs(), jobs.LedgerDir(target)},
	}
	if p.Env != "" {
		moves = append(moves, [2]string{p.Env, envTarget})
	}
	if p.Workspace != "" {
		moves = append(moves, [2]string{p.Workspace, workspaceTarget})
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

// Size is the disk space the project itself takes: its folder, settings,
// allowlist and batch jobs, not an env or workspace folder kept elsewhere
// (FolderSize tells those). Symbolic links are not followed, hard-linked
// files count once, and files it cannot read are left out (complete is then
// false).
func (p Project) Size() (total int64, complete bool) {
	return diskUsage(p.State, p.Settings(), p.Allowlist(), p.Jobs())
}

// FolderSize is the disk space a folder takes, such as a project's env or
// workspace folder, counted as Size counts.
func FolderSize(path string) (total int64, complete bool) {
	return diskUsage(path)
}

func diskUsage(paths ...string) (total int64, complete bool) {
	complete = true
	seen := map[[2]uint64]bool{}
	for _, path := range paths {
		if !exists(path) {
			continue
		}
		err := filepath.WalkDir(path, func(_ string, entry fs.DirEntry, err error) error {
			if err != nil {
				complete = false
				if entry != nil && entry.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				complete = false
				return nil
			}
			bytes, inode, linked := usage(info)
			if linked {
				if seen[inode] {
					return nil
				}
				seen[inode] = true
			}
			total += bytes
			return nil
		})
		if err != nil {
			complete = false
		}
	}
	return total, complete
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
		if isRemoving(path) || isStaging(path) || !isDirectory(path) || !IsProject(path) {
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
// absolute target and its env and workspace folders.
func (p Project) destination(target, envDir string) (string, string, string, error) {
	absolute, err := filepath.Abs(expandHome(target))
	if err != nil {
		return "", "", "", err
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
		return "", "", "", err
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
		return "", "", "", err
	}
	if parent, err := filepath.EvalSymlinks(filepath.Dir(absolute)); err == nil {
		absolute = filepath.Join(parent, filepath.Base(absolute))
	}
	if err := outside(absolute); err != nil {
		return "", "", "", err
	}
	envTarget := ""
	if p.Env != "" {
		envTarget = EnvFolder(filepath.Dir(p.Env), absolute)
		if envDir != "" {
			envTarget = EnvFolder(expandHome(envDir), absolute)
		}
	}
	workspaceTarget := ""
	if p.Workspace != "" {
		workspaceTarget = WorkspaceFolder(filepath.Dir(p.Workspace), absolute)
	}
	for _, move := range p.files(absolute, envTarget, workspaceTarget) {
		// The env and workspace folders stay put when the folder's name does.
		if move[0] == move[1] {
			continue
		}
		if exists(move[0]) && exists(move[1]) {
			return "", "", "", fmt.Errorf("%s already exists", move[1])
		}
	}
	return absolute, envTarget, workspaceTarget, nil
}

// CopyProject copies the project to the folder target, with its settings,
// allowlist, env folder and workspace folder. The copy gets a new key and
// certificate, so links to the original do not open it, and no port, which
// the original keeps. Runs' connection files and keys (instances/, run/)
// are left out. The folders are copied into hidden ones that take their
// names once all are complete: a copy that fails, or that ctx stops, keeps
// them for the same copy to continue (Transfer).
func CopyProject(ctx context.Context, p Project, target, envDir string, progress Progress) (Project, error) {
	if err := p.checkStopped("copy"); err != nil {
		return Project{}, err
	}
	transfer, resuming, err := p.openTransfer(TransferCopy)
	if err != nil {
		return Project{}, err
	}
	target, envTarget, workspaceTarget, err := p.destination(target, envDir)
	if err != nil {
		return Project{}, err
	}
	if err := p.checkResumes(transfer, resuming, target); err != nil {
		return Project{}, err
	}
	if envTarget != "" && envTarget == p.Env {
		return Project{}, fmt.Errorf("the copy would share the env folder %s with the original: "+
			"give it another folder name, or another env folder (--env-dir)", p.Env)
	}
	if workspaceTarget != "" && workspaceTarget == p.Workspace {
		return Project{}, fmt.Errorf("the copy would share the workspace folder %s with the original: "+
			"give it another folder name", p.Workspace)
	}
	if err := transfer.checkStaging(target, envTarget, workspaceTarget); err != nil {
		return Project{}, err
	}
	type part struct {
		from, to string
		skip     map[string]bool
	}
	parts := []part{{p.State, target, map[string]bool{"instances": true, "run": true}}}
	if p.Env != "" {
		parts = append(parts, part{p.Env, envTarget, nil})
	}
	if p.Workspace != "" {
		parts = append(parts, part{p.Workspace, workspaceTarget, nil})
	}
	transfer.To = target
	for _, part := range parts {
		if !transfer.ours(stagingPath(part.to)) {
			transfer.Partial = append(transfer.Partial, stagingPath(part.to))
		}
	}
	// Before copying, so even a copy that is killed is found again.
	if err := writeTransfer(p.State, transfer); err != nil {
		return Project{}, err
	}
	m, stop := newMeter(progress)
	defer stop()
	work := m.work(ctx)
	for _, part := range parts {
		m.phase("Copying")
		if err := work.copy(part.from, stagingPath(part.to), part.skip); err != nil {
			return Project{}, fmt.Errorf("copying %s: %w", part.from, err)
		}
	}
	// All complete: the copies take their names. Until the settings are
	// written too, a failure puts them back, for the same copy to continue.
	renamed := 0
	var written []string
	undo := func(err error) (Project, error) {
		for _, file := range written {
			os.Remove(file)
		}
		for back := renamed - 1; back >= 0; back-- {
			os.Rename(parts[back].to, stagingPath(parts[back].to))
		}
		return Project{}, err
	}
	for _, part := range parts {
		if err := os.Rename(stagingPath(part.to), part.to); err != nil {
			return undo(err)
		}
		renamed++
	}
	if exists(p.Allowlist()) {
		written = append(written, DefaultAllowlist(target))
		if err := copyFile(p.Allowlist(), DefaultAllowlist(target)); err != nil {
			return undo(err)
		}
	}
	if settings, err := ReadSettings(p.Settings()); err == nil {
		settings.Port = 0
		if err := settings.Renew(); err != nil {
			return undo(err)
		}
		written = append(written, SettingsFile(target))
		if err := WriteSettings(SettingsFile(target), filepath.Base(target), settings); err != nil {
			return undo(err)
		}
	} else if !errors.Is(err, ErrNoSettings) {
		return undo(err)
	}
	removeTransfer(p.State)
	return Project{Name: filepath.Base(target), State: target, Env: envTarget, Workspace: workspaceTarget}, nil
}

// MoveProject moves (or renames) the project to the folder target, with its
// settings, allowlist, env folder and workspace folder. Its key and port
// stay: links to it still open it. What is on another file system is
// copied, into hidden folders that take their names once everything is
// copied, and the originals are removed only then. A move that fails, or
// that ctx stops, before then puts back what it renamed and keeps what it
// copied for the same move to continue (Transfer). One stopped while
// removing the originals leaves them for LeftoverRemoval, and returns the
// moved project with the error.
func MoveProject(ctx context.Context, p Project, target, envDir string, progress Progress) (Project, error) {
	if err := p.checkStopped("move"); err != nil {
		return Project{}, err
	}
	if err := p.checkNoActiveJobs("move"); err != nil {
		return Project{}, err
	}
	transfer, resuming, err := p.openTransfer(TransferMove)
	if err != nil {
		return Project{}, err
	}
	target, envTarget, workspaceTarget, err := p.destination(target, envDir)
	if err != nil {
		return Project{}, err
	}
	if err := p.checkResumes(transfer, resuming, target); err != nil {
		return Project{}, err
	}
	moves := p.files(target, envTarget, workspaceTarget)
	for _, move := range moves {
		if move[0] != move[1] {
			if err := transfer.checkStaging(move[1]); err != nil {
				return Project{}, err
			}
		}
	}
	transfer.To = target
	m, stop := newMeter(progress)
	defer stop()
	type step struct{ from, to string }
	// renamed are undone by renaming them back, and copied files removed;
	// copied folders stay hidden for the next attempt.
	var renamed, copiedFiles, staged []step
	undo := func(err error) (Project, error) {
		for index := len(renamed) - 1; index >= 0; index-- {
			os.Rename(renamed[index].to, renamed[index].from)
		}
		for _, file := range copiedFiles {
			os.Remove(file.to)
		}
		if len(transfer.Partial) == 0 {
			removeTransfer(p.State)
		}
		return Project{}, err
	}
	work := m.work(ctx)
	var copiedEntries int64
	for _, move := range moves {
		if !exists(move[0]) || move[0] == move[1] {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(move[1]), 0o700); err != nil {
			return undo(err)
		}
		staging := stagingPath(move[1])
		if !transfer.ours(staging) {
			err := moveRename(move[0], move[1])
			if err == nil {
				renamed = append(renamed, step{move[0], move[1]})
				continue
			}
			if !errors.Is(err, syscall.EXDEV) {
				return undo(fmt.Errorf("moving %s: %w", move[0], err))
			}
		}
		info, err := os.Lstat(move[0])
		if err != nil {
			return undo(err)
		}
		if !info.IsDir() {
			if err := copyFile(move[0], move[1]); err != nil {
				return undo(fmt.Errorf("moving %s: %w", move[0], err))
			}
			copiedFiles = append(copiedFiles, step{move[0], move[1]})
			continue
		}
		if !transfer.ours(staging) {
			transfer.Partial = append(transfer.Partial, staging)
			// Before copying, so even a move that is killed is found again.
			if err := writeTransfer(p.State, transfer); err != nil {
				return undo(err)
			}
		}
		m.phase("Copying")
		if err := work.copy(move[0], staging, nil); err != nil {
			return undo(fmt.Errorf("moving %s: %w", move[0], err))
		}
		copiedEntries += m.files.Load()
		staged = append(staged, step{move[0], move[1]})
	}
	// Everything is copied: the copies take their names.
	for index, folder := range staged {
		if err := os.Rename(stagingPath(folder.to), folder.to); err != nil {
			for back := index - 1; back >= 0; back-- {
				os.Rename(staged[back].to, stagingPath(staged[back].to))
			}
			return undo(err)
		}
	}
	removeTransfer(p.State)
	moved := Project{Name: filepath.Base(target), State: target, Env: envTarget, Workspace: workspaceTarget}
	// What was copied is still in its old place (and only that).
	m.phaseOf("Removing the original of", copiedEntries)
	original := p
	if envTarget == p.Env {
		original.Env = "" // stayed put: it is the moved project's
	}
	if workspaceTarget == p.Workspace {
		original.Workspace = ""
	}
	for _, path := range original.toRemove() {
		if err := work.remove(path); err != nil {
			return moved, fmt.Errorf("the project is in %s now, but removing its original stopped: %w",
				moved.State, removeError(err))
		}
	}
	return moved, nil
}

// RemoveProject deletes the project: its folder (chats, logins, workspace,
// tasks), settings, allowlist and env folder. It first renames them to
// hidden ones next to them (RemovingPath), so the project is gone at once,
// and a removal that fails, or that ctx stops, leaves only those, for
// LeftoverRemoval to find and RemoveProject to finish.
func RemoveProject(ctx context.Context, p Project, progress Progress) error {
	if err := p.checkStopped("remove"); err != nil {
		return err
	}
	if err := p.checkNoActiveJobs("remove"); err != nil {
		return err
	}
	m, stop := newMeter(progress)
	defer stop()
	m.phase("Removing")
	work := m.work(ctx)
	// An unfinished copy or move's partial copies go too.
	if transfer, found, _ := ReadTransfer(p.State); found {
		for _, path := range append(transfer.Partial, transfer.Removing...) {
			if isStaging(path) || isRemoving(path) {
				if err := work.remove(path); err != nil {
					return removeError(err)
				}
			}
		}
		removeTransfer(p.State)
	}
	for _, path := range p.toRemove() {
		if err := work.remove(path); err != nil {
			return removeError(err)
		}
	}
	return nil
}

// toRemove renames what exists of the project to its RemovingPath (a
// project LeftoverRemoval found stays where it is), and returns the paths
// in the order to remove them: the settings, which say where the env
// folder is, last.
func (p Project) toRemove() []string {
	trash := p
	if !isRemoving(p.State) {
		trash = Project{Name: p.Name, State: RemovingPath(p.State)}
		if p.Env != "" {
			trash.Env = RemovingPath(p.Env)
		}
		if p.Workspace != "" {
			trash.Workspace = RemovingPath(p.Workspace)
		}
	}
	from := []string{p.Env, p.Workspace, p.State, p.Jobs(), p.Allowlist(), p.Settings()}
	to := []string{trash.Env, trash.Workspace, trash.State, trash.Jobs(), trash.Allowlist(), trash.Settings()}
	var paths []string
	for index, path := range from {
		if path == "" || !exists(path) {
			continue
		}
		// A leftover of an earlier removal in the way: removed in place.
		if to[index] != path && !exists(to[index]) && os.Rename(path, to[index]) == nil {
			path = to[index]
		}
		paths = append(paths, path)
	}
	return paths
}

// RemovingPath is where a project's folder (or env folder) is while it is
// removed: a hidden folder next to it, which ListProjects leaves out.
func RemovingPath(path string) string {
	return filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+removingSuffix)
}

const removingSuffix = ".removing"

func isRemoving(path string) bool {
	base := filepath.Base(path)
	return strings.HasPrefix(base, ".") && strings.HasSuffix(base, removingSuffix)
}

// LeftoverRemoval finds what a removal (or move) of the project in the
// folder state left when it stopped, for RemoveProject to finish; envDir is
// the env directory its env folder was in ("" for none). found is false
// when nothing is left.
func LeftoverRemoval(state, envDir string) (p Project, found bool) {
	absolute, err := filepath.Abs(expandHome(state))
	if err != nil {
		return Project{}, false
	}
	if parent, err := filepath.EvalSymlinks(filepath.Dir(absolute)); err == nil {
		absolute = filepath.Join(parent, filepath.Base(absolute))
	}
	p = Project{Name: filepath.Base(absolute), State: RemovingPath(absolute)}
	if envDir != "" {
		if env := EnvFolder(expandHome(envDir), p.State); exists(env) {
			p.Env = env
		}
	}
	// The settings, which say where the workspace folder is, go last.
	if dir, err := SavedWorkspaceDir(p.State); err == nil && dir != "" {
		if workspace := WorkspaceFolder(dir, p.State); exists(workspace) {
			p.Workspace = workspace
		}
	}
	for _, path := range []string{p.State, p.Settings(), p.Allowlist(), p.Jobs(), p.Env, p.Workspace} {
		if path != "" && exists(path) {
			return p, true
		}
	}
	return p, false
}
