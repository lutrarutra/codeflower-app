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
	state, err := filepath.Abs(expandHome(dir))
	if err != nil {
		return "", err
	}
	// Apptainer binds are SOURCE:TARGET; a Windows drive letter is not part of that.
	if strings.Contains(strings.TrimPrefix(state, filepath.VolumeName(state)), ":") {
		return "", fmt.Errorf("state path must not contain ':'")
	}
	if err := privateDirectory(state); err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(state); err == nil {
		state = resolved
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
