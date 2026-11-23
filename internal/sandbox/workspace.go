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
// in its settings. What is on another file system is copied, into a hidden
// folder that takes its name once the copy is complete, and the original is
// removed only once the settings name the new place. A move that fails, or
// that ctx stops, keeps what it copied for the same move to continue, and
// one stopped while removing the original finishes that (Transfer).
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
		if err := apartFromEnv(to, p.Env); err != nil {
			return Project{}, err
		}
	}
	transfer, resuming, err := p.openTransfer(TransferMoveWorkspace)
	if err != nil {
		return Project{}, err
	}
	if err := p.checkResumes(transfer, resuming, to); err != nil {
		return Project{}, err
	}
	m, stop := newMeter(progress)
	defer stop()
	work := m.work(ctx)
	moved := p
	moved.Workspace = ""
	if dir != "" {
		moved.Workspace = to
	}
	// Complete but for removing the original.
	if resuming && len(transfer.Partial) == 0 && len(transfer.Removing) > 0 {
		return moved, finishRemoving(work, m, p.State, transfer, 0)
	}
	if from == to {
		return Project{}, fmt.Errorf("the workspace of the project %s is in %s already", p.Name, to)
	}
	// An empty folder in the way, such as the project's mount point, goes
	// when the workspace takes its place.
	if exists(to) && !emptyFolder(to) {
		return Project{}, fmt.Errorf("%s already exists and is not empty", to)
	}
	if err := transfer.checkStaging(to); err != nil {
		return Project{}, err
	}
	transfer.To = to
	staging := stagingPath(to)
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		return Project{}, err
	}
	copied := false
	switch {
	case transfer.ours(staging):
		copied = true
	case !exists(from):
		if err := privateDirectory(staging); err != nil {
			return Project{}, err
		}
	default:
		if err := moveRename(from, staging); err != nil {
			if !errors.Is(err, syscall.EXDEV) {
				return Project{}, fmt.Errorf("moving %s: %w", from, err)
			}
			copied = true
		}
	}
	var copiedEntries int64
	if copied {
		if !transfer.ours(staging) {
			transfer.Partial = append(transfer.Partial, staging)
			// Before copying, so even a move that is killed is found again.
			if err := writeTransfer(p.State, transfer); err != nil {
				return Project{}, err
			}
		}
		m.phase("Copying")
		// A workspace is mostly data files: counting them is cheap next to
		// copying them, and gives the copy a total.
		stopCount := m.count(from)
		err := work.copy(from, staging, nil)
		stopCount()
		if err != nil {
			return Project{}, fmt.Errorf("copying %s: %w", from, err)
		}
		copiedEntries = m.files.Load()
	}
	// Complete: the copy takes its name, and the settings the new place.
	putBack := func(err error) (Project, error) {
		if exists(to) && !exists(staging) {
			os.Rename(to, staging)
		}
		if !copied {
			os.Rename(staging, from)
		}
		privateDirectory(own)
		return Project{}, err
	}
	if exists(to) {
		if err := os.Remove(to); err != nil {
			return putBack(err)
		}
	}
	if err := os.Rename(staging, to); err != nil {
		return putBack(err)
	}
	if err := saveWorkspaceDir(p.State, dir); err != nil {
		return putBack(fmt.Errorf("could not save where the workspace is: %w", err))
	}
	if !copied {
		// The mount point of the folder elsewhere.
		if from == own {
			if err := privateDirectory(own); err != nil {
				return moved, err
			}
		}
		return moved, nil
	}
	// The original goes: hidden first, so the project's mount point is
	// there again at once.
	leftover := RemovingPath(from)
	if err := os.Rename(from, leftover); err != nil {
		return moved, fmt.Errorf("the workspace is in %s now, but its original %s is left: %w", to, from, err)
	}
	if from == own {
		if err := privateDirectory(own); err != nil {
			return moved, err
		}
	}
	transfer.Partial, transfer.Removing = nil, []string{leftover}
	if err := writeTransfer(p.State, transfer); err != nil {
		return moved, err
	}
	return moved, finishRemoving(work, m, p.State, transfer, copiedEntries)
}

// finishRemoving removes the original of a moved workspace, then the
// project's Transfer.
func finishRemoving(work *treeWork, m *meter, state string, transfer Transfer, entries int64) error {
	if entries > 0 {
		m.phaseOf("Removing the original of", entries)
	} else {
		m.phase("Removing the original of")
	}
	for _, path := range transfer.Removing {
		if !isRemoving(path) {
			continue // never anything but what the move hid
		}
		if err := work.remove(path); err != nil {
			return fmt.Errorf("the workspace is in %s now, but removing its original stopped: %w; "+
				"run the same command again to finish", transfer.To, removeError(err))
		}
	}
	removeTransfer(state)
	return nil
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
