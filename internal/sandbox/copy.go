package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// moveRename moves a path within a file system; across them it fails with
// syscall.EXDEV, and a move copies instead. A variable for tests, which
// have only one file system.
var moveRename = os.Rename

// copySlots is how many files a copy writes at once: network file systems
// (NFS, GPFS, Lustre) move many files in flight much faster than one at a
// time.
const copySlots = 8

// copyChunk is how much of a file is copied between looks at progress and
// at whether to stop, so a large file shows its progress and stops soon.
const copyChunk = 64 << 20

// partialSuffix marks what a copy has not finished: a file being written
// (written under this name, then renamed), and a folder being copied
// (stagingPath), which only gets its own name once complete.
const partialSuffix = ".codeflower-partial"

// copy copies a folder with its permissions, modification times, links and
// hard links (within it); skip names top-level folders that are copied
// empty. It continues what a stopped copy into target left: a file already
// there with the same size and modification time is kept, as rsync does,
// others are written again, and what the source no longer has goes, so
// running it again finishes the copy.
func (w *treeWork) copy(source, target string, skip map[string]bool) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	// What the copy is in target, when it continues a stopped one.
	var copied map[string]bool
	if exists(target) {
		copied = map[string]bool{}
	}
	type folder struct {
		path     string
		mode     fs.FileMode
		modified time.Time
	}
	type file struct {
		source, target string
		info           fs.FileInfo
		// first is where the copy of a file hard linked to this one is.
		first string
	}
	var folders []folder
	var links []file
	firsts := map[[2]uint64]string{}

	jobs := make(chan file, copySlots*4)
	var workers sync.WaitGroup
	for range copySlots {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for job := range jobs {
				if w.ctx.Err() != nil {
					continue
				}
				if err := w.copyFile(job.source, job.target, job.info); err != nil {
					w.fail(fmt.Errorf("copying %s: %w", job.source, err))
				}
			}
		}()
	}
	walked := filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
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
		if copied != nil {
			copied[destination] = true
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.Type()&fs.ModeSymlink != 0:
			w.files.Add(1)
			return copyLink(path, destination)
		case entry.IsDir():
			w.files.Add(1)
			// Writable while copying into it; its own mode once its contents are in.
			folders = append(folders, folder{destination, info.Mode().Perm(), info.ModTime()})
			return makeFolder(destination)
		case info.Mode().IsRegular():
			if _, id, linked := usage(info); linked {
				if first, seen := firsts[id]; seen {
					links = append(links, file{path, destination, info, first})
					return nil
				}
				firsts[id] = destination
			}
			select {
			case jobs <- file{source: path, target: destination, info: info}:
			case <-w.ctx.Done():
				return w.err()
			}
			return nil
		default:
			// Sockets and pipes belong to a run, not the project.
			return nil
		}
	})
	close(jobs)
	workers.Wait()
	if err := w.err(); err != nil {
		return err
	}
	if walked != nil {
		w.fail(walked)
		return walked
	}
	// Once the files they link to are complete.
	for _, link := range links {
		if err := w.err(); err != nil {
			return err
		}
		if err := w.linkFile(link.source, link.target, link.first, link.info); err != nil {
			w.fail(err)
			return err
		}
	}
	if copied != nil {
		if err := pruneCopy(target, copied); err != nil {
			w.fail(err)
			return err
		}
	}
	for index := len(folders) - 1; index >= 0; index-- {
		folder := folders[index]
		if err := os.Chmod(folder.path, folder.mode); err != nil {
			return err
		}
		if err := os.Chtimes(folder.path, folder.modified, folder.modified); err != nil {
			return err
		}
	}
	return nil
}

// pruneCopy removes from the copy in target what a stopped copy left there
// that is not part of the copy (copied): what was removed or renamed in the
// source since, and files it was writing.
func pruneCopy(target string, copied map[string]bool) error {
	return filepath.WalkDir(target, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if copied[path] {
			return nil
		}
		if err := removeTree(path); err != nil {
			return err
		}
		if entry.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
}

// copyFile copies a regular file for a copy, unless a stopped copy left it
// there complete.
func (w *treeWork) copyFile(source, target string, info fs.FileInfo) error {
	if existing, err := os.Lstat(target); err == nil {
		if sameFile(existing, info) {
			w.bytes.Add(info.Size())
			w.files.Add(1)
			return nil
		}
		if existing.IsDir() {
			if err := removeTree(target); err != nil {
				return err
			}
		}
	}
	if err := copyContents(source, target, info, func(written int64) error {
		w.bytes.Add(written)
		w.written.Add(written)
		return w.err()
	}); err != nil {
		return err
	}
	w.files.Add(1)
	return nil
}

// linkFile makes target a hard link to first, the copy of a file it is
// hard linked to, as in the original; a copy where links do not work.
func (w *treeWork) linkFile(source, target, first string, info fs.FileInfo) error {
	firstInfo, err := os.Lstat(first)
	if err != nil {
		return err
	}
	if existing, err := os.Lstat(target); err == nil {
		if os.SameFile(existing, firstInfo) {
			w.bytes.Add(info.Size())
			w.files.Add(1)
			return nil
		}
		if err := removeTree(target); err != nil {
			return err
		}
	}
	if err := os.Link(first, target); err != nil {
		return w.copyFile(source, target, info)
	}
	w.bytes.Add(info.Size())
	w.files.Add(1)
	return nil
}

// copyContents writes a copy of the file source, with its mode and
// modification time, to target: under a partial name first, so a stopped
// copy never leaves a file that looks complete. written hears each chunk
// and may stop the copy with an error.
func copyContents(source, target string, info fs.FileInfo, written func(int64) error) error {
	partial := partialName(target)
	// Possibly left read-only by a stopped copy.
	if err := os.Remove(partial); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	for {
		// io.CopyN keeps the kernel's own copying (copy_file_range) where it can.
		n, err := io.CopyN(out, in, copyChunk)
		if n > 0 && written != nil {
			if stop := written(n); stop != nil {
				out.Close()
				return stop
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			out.Close()
			return err
		}
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Chmod(partial, info.Mode().Perm()); err != nil {
		return err
	}
	if err := os.Chtimes(partial, info.ModTime(), info.ModTime()); err != nil {
		return err
	}
	return os.Rename(partial, target)
}

// copyFile copies a single file, such as a project's settings, with its
// mode and modification time, replacing target.
func copyFile(source, target string) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	return copyContents(source, target, info, nil)
}

// copyLink makes a symbolic link like the one at path, unless it is there.
func copyLink(path, target string) error {
	link, err := os.Readlink(path)
	if err != nil {
		return err
	}
	if existing, err := os.Lstat(target); err == nil {
		if existing.Mode()&fs.ModeSymlink != 0 {
			if current, err := os.Readlink(target); err == nil && current == link {
				return nil
			}
		}
		if err := removeTree(target); err != nil {
			return err
		}
	}
	return os.Symlink(link, target)
}

// makeFolder makes a folder to copy into, or makes one a stopped copy left
// writable again.
func makeFolder(path string) error {
	existing, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return os.Mkdir(path, 0o700)
	case err != nil:
		return err
	case existing.IsDir():
		return os.Chmod(path, 0o700)
	}
	if err := removeTree(path); err != nil {
		return err
	}
	return os.Mkdir(path, 0o700)
}

// sameFile says whether a copy is complete: a regular file of the same size
// and modification time. Times compare to the second where a file system
// keeps no finer ones.
func sameFile(copied, original fs.FileInfo) bool {
	if !copied.Mode().IsRegular() || copied.Size() != original.Size() {
		return false
	}
	a, b := copied.ModTime(), original.ModTime()
	if a.Equal(b) {
		return true
	}
	return (a.Nanosecond() == 0 || b.Nanosecond() == 0) && a.Truncate(time.Second).Equal(b.Truncate(time.Second))
}

// partialName is where a file is written before it is complete: hidden
// next to it.
func partialName(path string) string {
	base := filepath.Base(path)
	// Within the 255 bytes a name may have.
	if len(base) > 200 {
		sum := sha256.Sum256([]byte(base))
		base = hex.EncodeToString(sum[:8])
	}
	return filepath.Join(filepath.Dir(path), "."+base+partialSuffix)
}

// stagingPath is where a folder is copied before the copy is complete:
// hidden next to where it goes, so an unfinished copy is never taken for
// the real one (a project in the list, a workspace a sandbox mounts).
func stagingPath(path string) string {
	return filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+partialSuffix)
}

func isStaging(path string) bool {
	base := filepath.Base(path)
	return len(base) > len(partialSuffix) && base[0] == '.' && base[len(base)-len(partialSuffix):] == partialSuffix
}
