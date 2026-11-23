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
// times a second. It may be nil.
type Progress func(ProgressReport)

// ProgressReport is how far a copy, move or removal has come.
type ProgressReport struct {
	// Doing is what it does now: "Copying", "Removing", ….
	Doing string
	// Files are the files and folders done so far, and Bytes the bytes of
	// the files: copied, or found copied by an earlier, stopped attempt.
	Files, Bytes int64
	// Written are the bytes this attempt wrote, for its speed.
	Written int64
	// TotalFiles and TotalBytes are what there is to do, as far as counted
	// yet; Counted says the count is complete. Both zero when nothing is
	// counted, as counting costs a walk of its own.
	TotalFiles, TotalBytes int64
	Counted                bool
	// Elapsed is the time since Doing started.
	Elapsed time.Duration
}

// removeSlots is how many folders a removal works on at once. Removing
// waits on the disk, not the processor, and network file systems (NFS,
// GPFS, Lustre) remove many files in flight much faster than one at a time.
const removeSlots = 16

// readBatch is how many entries of a folder are read at once, so a folder
// of millions of files is never held in memory.
const readBatch = 1024

// progressEvery is how often a Progress hears. A variable for tests.
var progressEvery = 200 * time.Millisecond

// meter counts the files and bytes of a copy, move or removal for a
// Progress.
type meter struct {
	files, bytes, written  atomic.Int64
	totalFiles, totalBytes atomic.Int64
	counted                atomic.Bool
	started                atomic.Int64
	doing                  atomic.Pointer[string]
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
				progress(m.report())
			}
		}
	}()
	return m, func() { close(done); wg.Wait() }
}

func (m *meter) report() ProgressReport {
	return ProgressReport{
		Doing: *m.doing.Load(), Files: m.files.Load(), Bytes: m.bytes.Load(), Written: m.written.Load(),
		TotalFiles: m.totalFiles.Load(), TotalBytes: m.totalBytes.Load(), Counted: m.counted.Load(),
		Elapsed: time.Since(time.Unix(0, m.started.Load())),
	}
}

// phase starts counting something else.
func (m *meter) phase(doing string) {
	m.doing.Store(&doing)
	for _, count := range []*atomic.Int64{&m.files, &m.bytes, &m.written, &m.totalFiles, &m.totalBytes} {
		count.Store(0)
	}
	m.counted.Store(false)
	m.started.Store(time.Now().UnixNano())
}

// phaseOf starts counting something else whose files are known, such as
// the originals of what was just copied.
func (m *meter) phaseOf(doing string, files int64) {
	m.phase(doing)
	m.totalFiles.Store(files)
	m.counted.Store(true)
}

// count totals the files and bytes below path, alongside the work that
// does them, until it is through or stop is called. Only where the walk is
// cheap next to the work: a few large files, not millions of small ones.
func (m *meter) count(path string) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		err := filepath.WalkDir(path, func(_ string, entry fs.DirEntry, err error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				return nil
			}
			m.totalFiles.Add(1)
			if entry.Type().IsRegular() {
				if info, err := entry.Info(); err == nil {
					m.totalBytes.Add(info.Size())
				}
			}
			return nil
		})
		if err == nil {
			m.counted.Store(true)
		}
	}()
	return func() { cancel(); <-done }
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
