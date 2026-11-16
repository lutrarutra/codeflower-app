package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
)

const (
	reproduceFolderPrefix = "codeflower-reproduce-"
	reproduceLock         = ".lock"
	reproduceKept         = ".kept"
	// ReproduceContainerPrefix starts the name of every container of a
	// reproduction: codeflower-repro-ID-PHASE.
	ReproduceContainerPrefix = "codeflower-repro-"
	// A folder without a lock is only taken for a stale one when it is this
	// old: a run creates the lock right after the folder, and app 0.34.0 made
	// folders without one.
	unlockedFolderAge = 24 * time.Hour
)

// ReproduceFolder is the temporary folder of one reproduction: the unpacked
// archive, the downloads, the throwaway project and the engine's scratch
// space, removed together when the run ends. The run holds a lock on it while
// it lasts, so a later run tells the folder of a run that was killed (its lock
// is free) from one still running, and removes it with its containers.
type ReproduceFolder struct {
	Path string
	// ID names the run's containers (ReproduceContainerPrefix + ID + "-" + phase).
	ID     string
	unlock func()
	kept   bool
}

// NewReproduceFolder creates and locks a run's folder in the temporary folder.
func NewReproduceFolder() (*ReproduceFolder, error) {
	path, err := privateTemp(reproduceFolderPrefix)
	if err != nil {
		return nil, err
	}
	unlock, err := tryLockFile(filepath.Join(path, reproduceLock))
	if err != nil {
		os.RemoveAll(path)
		return nil, err
	}
	return &ReproduceFolder{Path: path, ID: strings.TrimPrefix(filepath.Base(path), reproduceFolderPrefix), unlock: unlock}, nil
}

// Folder creates the named folder in the run's folder and returns its path.
func (f *ReproduceFolder) Folder(name string) (string, error) {
	path := filepath.Join(f.Path, name)
	return path, os.MkdirAll(path, 0o700)
}

// Keep leaves the folder in place when the run ends (--keep-state). Later
// runs do not remove a kept folder.
func (f *ReproduceFolder) Keep() error {
	f.kept = true
	return os.WriteFile(filepath.Join(f.Path, reproduceKept), nil, 0o600)
}

// Close ends the run: it releases the lock and removes the folder, unless it
// is kept.
func (f *ReproduceFolder) Close() error {
	f.unlock()
	if f.kept {
		_ = os.Remove(filepath.Join(f.Path, reproduceLock))
		return nil
	}
	return removeTree(f.Path)
}

// RemoveStaleReproductions removes what reproductions that were killed before
// they could clean up left behind: their folders (whose lock no process
// holds), and with Docker or Podman their containers, which go on running
// without the app. Kept folders stay. It returns the folders it removed.
func RemoveStaleReproductions(ctx context.Context, engine EngineStatus) []string {
	if runtime.GOOS == "windows" {
		// No file locks there: a running reproduction's folder looks stale.
		return nil
	}
	base, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		base = os.TempDir()
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	var removed []string
	for _, entry := range entries {
		path := filepath.Join(base, entry.Name())
		if !entry.IsDir() || !staleReproduction(path, entry.Name()) {
			continue
		}
		if id, ok := strings.CutPrefix(entry.Name(), reproduceFolderPrefix); ok {
			removeReproduceContainers(ctx, engine, id)
		}
		if removeTree(path) == nil {
			removed = append(removed, path)
		}
	}
	return removed
}

// staleReproduction says whether a folder in the temporary folder was left
// by a reproduction that is no longer running.
func staleReproduction(path, name string) bool {
	// codeflower-archive- and the downloads folder are app 0.34.0's names.
	if !strings.HasPrefix(name, reproduceFolderPrefix) && !strings.HasPrefix(name, "codeflower-archive-") {
		return false
	}
	if _, err := os.Lstat(filepath.Join(path, reproduceKept)); err == nil {
		return false
	}
	lock := filepath.Join(path, reproduceLock)
	if _, err := os.Lstat(lock); err != nil {
		info, err := os.Lstat(path)
		return err == nil && time.Since(info.ModTime()) > unlockedFolderAge
	}
	unlock, err := tryLockFile(lock)
	if err != nil {
		return false
	}
	unlock()
	return true
}

// removeReproduceContainers removes the containers of a run that was killed.
// Apptainer's run as children of the app, so they end with it.
func removeReproduceContainers(ctx context.Context, engine EngineStatus, id string) {
	if id == "" {
		return
	}
	prefix := ReproduceContainerPrefix + id + "-"
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	switch engine.Name {
	case EngineDocker:
		docker, err := newDockerClient()
		if err != nil {
			return
		}
		defer docker.Close()
		found, err := docker.ContainerList(ctx, container.ListOptions{All: true, Filters: filters.NewArgs(filters.Arg("name", prefix))})
		if err != nil {
			return
		}
		for _, item := range found {
			for _, name := range item.Names {
				if strings.HasPrefix(strings.TrimPrefix(name, "/"), prefix) {
					_ = docker.ContainerRemove(ctx, item.ID, container.RemoveOptions{Force: true})
					break
				}
			}
		}
	case EnginePodman:
		output, err := exec.CommandContext(ctx, engine.Executable, "ps", "-a", "--format", "{{.Names}}").Output()
		if err != nil {
			return
		}
		for _, name := range strings.Fields(string(output)) {
			if strings.HasPrefix(name, prefix) {
				_ = exec.CommandContext(ctx, engine.Executable, "rm", "-f", name).Run()
			}
		}
	}
}
