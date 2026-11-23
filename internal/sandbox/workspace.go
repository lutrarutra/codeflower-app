package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// A project's workspace is in the project, or, with a workspace directory
// (--workspace-dir, workspace_dir in the config), in its folder there
// (WorkspaceFolder), e.g. on storage with more room than a home directory.
//
// Where it is is the project's own: its saved settings (Settings.
// WorkspaceDir) say so, and the config's workspace_dir only chooses it for
// new projects. Otherwise setting it would start every existing project on
// an empty workspace. MoveWorkspace moves an existing project's.

// SavedWorkspaceDir is the workspace directory the project's settings name,
// or "" for a workspace in the project (also when it has no settings).
func SavedWorkspaceDir(state string) (string, error) {
	settings, err := ReadSettings(SettingsFile(state))
	if errors.Is(err, ErrNoSettings) {
		return "", nil
	}
	return settings.WorkspaceDir, err
}

// saveWorkspaceDir records the project's workspace directory in its settings
// ("" for in the project), making the file when the project has none yet.
func saveWorkspaceDir(state, dir string) error {
	file := SettingsFile(state)
	settings, err := ReadSettings(file)
	if err != nil && !errors.Is(err, ErrNoSettings) {
		return err
	}
	settings.WorkspaceDir = dir
	return WriteSettings(file, filepath.Base(state), settings)
}

// ChooseWorkspace decides the workspace directory of the project in state
// ("" for in the project), and saves it in the project's settings when it
// chooses one. created says the project is new (it was not a project before
// PrepareState); configured is the config's workspace_dir, and given is
// --workspace-dir. The saved settings decide for an existing project:
// --workspace-dir may only repeat them, or choose for a project whose
// workspace is still empty.
func ChooseWorkspace(state string, created bool, configured, given string) (string, error) {
	saved, err := SavedWorkspaceDir(state)
	if err != nil {
		return "", err
	}
	if given != "" {
		given, err = absoluteFolder(given)
		if err != nil {
			return "", err
		}
		if given == saved {
			return saved, nil
		}
		current := filepath.Join(state, "workspace")
		if saved != "" {
			current = WorkspaceFolder(saved, state)
		}
		if !created && !emptyFolder(current) {
			return "", fmt.Errorf("the workspace of the project %s is in %s, not in %s; to move it there, stop its "+
				"sandboxes and run: codeflower projects move-workspace %s %s",
				filepath.Base(state), current, WorkspaceFolder(given, state), state, given)
		}
		return given, saveWorkspaceDir(state, given)
	}
	if saved != "" || !created || configured == "" {
		return saved, nil
	}
	return configured, saveWorkspaceDir(state, configured)
}

// MoveWorkspace moves the project's workspace to its folder in the workspace
// directory dir, or back into the project when dir is "", and records that
// in its settings. What is on another file system is copied, and the
// original is removed only once the settings name the new place: a move
// that fails, or that ctx stops, before then is undone.
func MoveWorkspace(ctx context.Context, p Project, dir string, progress Progress) (Project, error) {
	if err := p.checkStopped("move the workspace of"); err != nil {
		return Project{}, err
	}
	if err := p.checkNoActiveJobs("move the workspace of"); err != nil {
		return Project{}, err
	}
	own := filepath.Join(p.State, "workspace")
	from := own
	if p.Workspace != "" {
		from = p.Workspace
	}
	to := own
	if dir != "" {
		absolute, err := absoluteFolder(dir)
		if err != nil {
			return Project{}, err
		}
		if err := os.MkdirAll(absolute, 0o700); err != nil {
			return Project{}, err
		}
		if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
			absolute = resolved
		}
		dir = absolute
		to = WorkspaceFolder(dir, p.State)
		if to == p.State || isWithin(to, p.State) || isWithin(p.State, to) {
			return Project{}, fmt.Errorf("the workspace folder %s must be outside the project %s", to, p.State)
		}
	}
	if from == to {
		return Project{}, fmt.Errorf("the workspace of the project %s is in %s already", p.Name, to)
	}
	// An empty folder in the way, such as the project's mount point, goes.
	if exists(to) {
		if !emptyFolder(to) {
			return Project{}, fmt.Errorf("%s already exists and is not empty", to)
		}
		if err := os.Remove(to); err != nil {
			return Project{}, err
		}
	}
	m, stop := newMeter(progress)
	defer stop()
	work := m.work(ctx)
	copied := false
	undo := func(err error) (Project, error) {
		if copied {
			m.phase("Removing the partial copy of")
			m.work(context.Background()).remove(to)
		} else if exists(to) {
			os.Rename(to, from)
		}
		if to == own || from == own {
			privateDirectory(own)
		}
		return Project{}, err
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		return undo(err)
	}
	if !exists(from) {
		if err := privateDirectory(to); err != nil {
			return undo(err)
		}
	} else if err := os.Rename(from, to); err != nil {
		if !errors.Is(err, syscall.EXDEV) {
			return undo(fmt.Errorf("moving %s: %w", from, err))
		}
		copied = true
		m.phase("Copying")
		if err := work.copy(from, to, nil); err != nil {
			return undo(fmt.Errorf("moving %s: %w", from, err))
		}
	}
	if err := saveWorkspaceDir(p.State, dir); err != nil {
		return undo(fmt.Errorf("could not save where the workspace is: %w", err))
	}
	moved := p
	moved.Workspace = ""
	if dir != "" {
		moved.Workspace = to
	}
	if copied {
		m.phase("Removing the original of")
		if err := work.remove(from); err != nil {
			return moved, fmt.Errorf("the workspace is in %s now, but removing its original %s stopped: %w",
				to, from, removeError(err))
		}
	}
	// The mount point of the folder elsewhere.
	if from == own {
		if err := privateDirectory(own); err != nil {
			return moved, err
		}
	}
	return moved, nil
}

// absoluteFolder is a folder as an absolute path, ~ expanded.
func absoluteFolder(folder string) (string, error) {
	return filepath.Abs(expandHome(folder))
}

func isWithin(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != "." && filepath.IsLocal(rel)
}

// emptyFolder says whether a folder does not exist or has nothing in it.
func emptyFolder(folder string) bool {
	entries, err := os.ReadDir(folder)
	return errors.Is(err, os.ErrNotExist) || (err == nil && len(entries) == 0)
}
