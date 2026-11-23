package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"codeflower-app/internal/agents"
)

// ContainerState is where the project directory appears inside the sandbox.
const ContainerState = "/codeflower-state"

// Entrypoint prepares the project environment and starts the bridge.
const Entrypoint = "/opt/codeflower/entrypoint.sh"

// StateDirectories are created privately in every project.
var StateDirectories = []string{
	"instances",
	"sessions",
	"run",
	"workspace",
	"env",
	"cache",
	"home",
	"tasks",
	"runs",
}

func privateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("state path must be a real directory: %s", path)
	}
	return os.Chmod(path, 0o700)
}

// privateDirectories creates each directory of the slash-separated path
// below base privately.
func privateDirectories(base, path string) error {
	current := base
	for _, part := range strings.Split(path, "/") {
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			return fmt.Errorf("path leaves its folder: %s", path)
		}
		current = filepath.Join(current, part)
		if err := privateDirectory(current); err != nil {
			return err
		}
	}
	return nil
}

// writeOnce creates a file with private permissions unless it already exists,
// so the user's later edits are kept.
func writeOnce(path string, content []byte) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, content, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

// PrepareState creates the project layout and its default files, and returns
// the absolute project path.
func PrepareState(dir string) (string, error) {
	state, err := privateRoot(dir, "state")
	if err != nil {
		return "", err
	}
	for _, name := range StateDirectories {
		if err := privateDirectory(filepath.Join(state, name)); err != nil {
			return "", err
		}
	}

	// Each agent's home, with the files its descriptor writes once (e.g. Codex
	// runs without its own bubblewrap sandbox, since Apptainer is the boundary).
	for _, descriptor := range agents.All() {
		if err := privateDirectories(state, descriptor.Home.Path); err != nil {
			return "", err
		}
		home := filepath.Join(state, filepath.FromSlash(descriptor.Home.Path))
		for name, raw := range descriptor.Files {
			content, err := agents.FileContent(raw)
			if err != nil {
				return "", fmt.Errorf("agent %s, file %s: %w", descriptor.ID, name, err)
			}
			if err := privateDirectories(home, filepath.ToSlash(filepath.Dir(name))); err != nil {
				return "", err
			}
			if err := writeOnce(filepath.Join(home, filepath.FromSlash(name)), content); err != nil {
				return "", err
			}
		}
	}
	return state, nil
}

// privateRoot creates dir with owner-only permissions and returns its
// absolute, resolved path.
func privateRoot(dir, what string) (string, error) {
	root, err := filepath.Abs(expandHome(dir))
	if err != nil {
		return "", err
	}
	// Apptainer binds are SOURCE:TARGET; a Windows drive letter is not part of that.
	if strings.Contains(strings.TrimPrefix(root, filepath.VolumeName(root)), ":") {
		return "", fmt.Errorf("%s path must not contain ':'", what)
	}
	if err := privateDirectory(root); err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	return root, nil
}

// EnvFolder is the project's folder in an env directory (--env-dir), named
// after the project.
func EnvFolder(envDir, state string) string {
	return filepath.Join(envDir, filepath.Base(state))
}

// EnvPaths are the project's folders that an env folder (--env-dir) holds
// instead: what can be installed, downloaded or computed again. Each has the
// same path in the env folder as in the project.
var EnvPaths = []string{"env", "cache", "browser", "thumbnails", "home/.cache"}

// PrepareEnv creates a project's env folder (EnvFolder) with the EnvPaths
// that EnvBinds mount over the project's own, and the mount points in the
// project, and returns the env folder's absolute path. The project's
// env/exports, the environment's recipe, stays in the project, so a purged
// env folder can be rebuilt from it.
func PrepareEnv(dir, state string) (string, error) {
	env, err := privateRoot(dir, "env")
	if err != nil {
		return "", err
	}
	if env == state || strings.HasPrefix(env, state+string(filepath.Separator)) ||
		strings.HasPrefix(state, env+string(filepath.Separator)) {
		return "", fmt.Errorf("the env folder %s must be outside the project %s", env, state)
	}
	// env/exports is the mount point of the project's own.
	for _, base := range []string{env, state} {
		for _, name := range append([]string{"env/exports"}, EnvPaths...) {
			if err := privateDirectories(base, name); err != nil {
				return "", err
			}
		}
	}
	return env, nil
}

// EnvBinds mount a project's env folder (PrepareEnv) over its EnvPaths, and
// its own env/exports back over the env folder's. None when env is empty:
// everything is in the project.
func EnvBinds(env, state string) []Bind {
	if env == "" {
		return nil
	}
	var binds []Bind
	for _, name := range EnvPaths {
		binds = append(binds, Bind{Source: filepath.Join(env, filepath.FromSlash(name)), Target: ContainerState + "/" + name})
		if name == "env" {
			binds = append(binds, Bind{Source: filepath.Join(state, "env", "exports"), Target: ContainerState + "/env/exports"})
		}
	}
	return binds
}

// WorkspaceFolder is the project's workspace in a workspace directory
// (--workspace-dir), named after the project.
func WorkspaceFolder(workspaceDir, state string) string {
	return filepath.Join(workspaceDir, filepath.Base(state))
}

// ContainerWorkspace is the agent's working directory in the sandbox.
const ContainerWorkspace = ContainerState + "/workspace"

// PrepareWorkspace creates a project's workspace folder (WorkspaceFolder),
// which WorkspaceBinds mount over the project's own workspace, and returns
// its absolute path. Unlike an env folder, it holds what cannot be made
// again: the agent's scripts, notes, downloads and results.
func PrepareWorkspace(dir, state string) (string, error) {
	workspace, err := privateRoot(dir, "workspace")
	if err != nil {
		return "", err
	}
	if workspace == state || strings.HasPrefix(workspace, state+string(filepath.Separator)) ||
		strings.HasPrefix(state, workspace+string(filepath.Separator)) {
		return "", fmt.Errorf("the workspace folder %s must be outside the project %s", workspace, state)
	}
	// The mount point.
	if err := privateDirectory(filepath.Join(state, "workspace")); err != nil {
		return "", err
	}
	return workspace, nil
}

// WorkspaceBinds mount a project's workspace folder (PrepareWorkspace) over
// its own workspace. None when workspace is empty: it is in the project.
func WorkspaceBinds(workspace string) []Bind {
	if workspace == "" {
		return nil
	}
	return []Bind{{Source: workspace, Target: ContainerWorkspace}}
}

// ProjectBinds are a project's env and workspace folders' binds, which go
// over the project's own folders, before the user's.
func ProjectBinds(env, workspace, state string) []Bind {
	return append(EnvBinds(env, state), WorkspaceBinds(workspace)...)
}
