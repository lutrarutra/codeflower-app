package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// Progress hears how a long copy, move or removal of a project goes, a few
// times a second: what it does now ("Copying", "Removing") and how many
// files and folders it has done at that so far. It may be nil.
type Progress func(doing string, files int64)

// removeSlots is how many folders a removal works on at once. Removing
// waits on the disk, not the processor, and network file systems (NFS,
// GPFS, Lustre) remove many files in flight much faster than one at a time.
const removeSlots = 16

// readBatch is how many entries of a folder are read at once, so a folder
// of millions of files is never held in memory.
const readBatch = 1024

// progressEvery is how often a Progress hears. A variable for tests.
var progressEvery = 200 * time.Millisecond

// meter counts the files of a copy, move or removal for a Progress.
type meter struct {
	files atomic.Int64
	doing atomic.Pointer[string]
}

// newMeter starts telling progress (when not nil) what the meter counts,
// until stop is called.
func newMeter(progress Progress) (m *meter, stop func()) {
	m = &meter{}
	m.phase("")
	if progress == nil {
		return m, func() {}
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(progressEvery)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				progress(*m.doing.Load(), m.files.Load())
			}
		}
	}()
	return m, func() { close(done); wg.Wait() }
}

// phase starts counting something else.
func (m *meter) phase(doing string) {
	m.doing.Store(&doing)
	m.files.Store(0)
}

// work starts removing or copying, until ctx ends or the work fails.
func (m *meter) work(ctx context.Context) *treeWork {
	ctx, cancel := context.WithCancelCause(ctx)
	return &treeWork{meter: m, ctx: ctx, cancel: cancel, slots: make(chan struct{}, removeSlots)}
}

// treeWork removes and copies folders. A removal stops at its first error,
// which then stops the rest of the work too.
type treeWork struct {
	*meter
	ctx    context.Context
	cancel context.CancelCauseFunc
	slots  chan struct{}
}

// err is why the work stopped, or nil.
func (w *treeWork) err() error {
	if w.ctx.Err() == nil {
		return nil
	}
	return context.Cause(w.ctx)
}

func (w *treeWork) fail(err error) { w.cancel(err) }

// remove removes a path and everything below it, also below read-only
// folders (Go's module cache, for one, makes its folders read-only). A
// file's own permissions do not matter: removing it changes its folder.
// Folders are worked on removeSlots at once.
func (w *treeWork) remove(path string) error {
	if err := w.err(); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	// The path's own parent is not made writable: it is not being removed.
	if info.IsDir() {
		w.removeFolder(path, "")
	} else {
		w.unlink(path, "")
	}
	return w.err()
}

// removeFolder removes a folder, a batch of entries at a time: its files
// here, its folders here or, while slots are free, alongside. A batch's
// folders are gone before the next batch is read, so reading the folder
// again starts at what is left.
func (w *treeWork) removeFolder(path, parent string) {
	for w.ctx.Err() == nil {
		entries, err := w.read(path)
		if err != nil {
			w.fail(err)
			return
		}
		if len(entries) == 0 {
			break
		}
		var wg sync.WaitGroup
		for _, entry := range entries {
			if w.ctx.Err() != nil {
				break
			}
			child := filepath.Join(path, entry.Name())
			if !entry.IsDir() {
				w.unlink(child, path)
				continue
			}
			select {
			case w.slots <- struct{}{}:
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer func() { <-w.slots }()
					w.removeFolder(child, path)
				}()
			default:
				w.removeFolder(child, path)
			}
		}
		wg.Wait()
	}
	if w.ctx.Err() == nil {
		w.unlink(path, parent)
	}
}

// read reads the next batch of a folder's entries, making the folder
// readable first if it is not.
func (w *treeWork) read(path string) ([]fs.DirEntry, error) {
	entries, err := readSome(path)
	if errors.Is(err, fs.ErrPermission) {
		_ = os.Chmod(path, 0o700)
		entries, err = readSome(path)
	}
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return entries, err
}

func readSome(path string) ([]fs.DirEntry, error) {
	folder, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer folder.Close()
	entries, err := folder.ReadDir(readBatch)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	return entries, err
}

// unlink removes a file or an empty folder, making its folder parent
// writable first if it has to ("" leaves it as it is).
func (w *treeWork) unlink(path, parent string) {
	err := os.Remove(path)
	if errors.Is(err, fs.ErrPermission) {
		if parent != "" {
			_ = os.Chmod(parent, 0o700)
		}
		// Windows does not remove read-only files. Elsewhere a file's mode
		// is left alone: hard links (conda's, for one) share it.
		if runtime.GOOS == "windows" {
			_ = os.Chmod(path, 0o600)
		}
		err = os.Remove(path)
	}
	switch {
	case err == nil:
		w.files.Add(1)
	case !errors.Is(err, fs.ErrNotExist):
		w.fail(err)
	}
}

// copy copies a folder with its permissions and links; skip names
// top-level folders that are copied empty.
func (w *treeWork) copy(source, target string, skip map[string]bool) error {
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
		if err := w.err(); err != nil {
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
		w.files.Add(1)
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

// removeTree removes a path like os.RemoveAll, also below read-only folders.
func removeTree(path string) error {
	m, stop := newMeter(nil)
	defer stop()
	return removeError(m.work(context.Background()).remove(path))
}

// removeError explains a removal that was not allowed.
func removeError(err error) error {
	if err == nil || !errors.Is(err, fs.ErrPermission) {
		return err
	}
	return fmt.Errorf("%w\nFiles the sandbox's engine made as another user (root, or a Podman sub-user) "+
		"may need that user to remove them", err)
}
