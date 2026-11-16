package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestReproduceFolderIsRemovedWhenTheRunEnds(t *testing.T) {
	t.Setenv("TMPDIR", tempDir(t))
	folder, err := NewReproduceFolder()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(filepath.Base(folder.Path), reproduceFolderPrefix) || folder.ID == "" {
		t.Fatalf("folder %+v", folder)
	}
	project, err := folder.Folder("project")
	if err != nil {
		t.Fatal(err)
	}
	// A package manager's read-only folder in the sandbox's cache.
	locked := filepath.Join(project, "cache", "mod")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "file"), []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	if err := folder.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(folder.Path); !os.IsNotExist(err) {
		t.Fatalf("the folder is still there: %v", err)
	}
}

func TestKeptReproduceFolderStays(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no sweep without file locks")
	}
	t.Setenv("TMPDIR", tempDir(t))
	folder, err := NewReproduceFolder()
	if err != nil {
		t.Fatal(err)
	}
	if err := folder.Keep(); err != nil {
		t.Fatal(err)
	}
	if err := folder.Close(); err != nil {
		t.Fatal(err)
	}
	if removed := RemoveStaleReproductions(context.Background(), EngineStatus{}); len(removed) != 0 {
		t.Fatalf("removed a kept folder: %v", removed)
	}
	if _, err := os.Stat(folder.Path); err != nil {
		t.Fatal(err)
	}
}

func TestStaleReproductionsAreRemoved(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no sweep without file locks")
	}
	base := tempDir(t)
	t.Setenv("TMPDIR", base)
	running, err := NewReproduceFolder()
	if err != nil {
		t.Fatal(err)
	}
	defer running.Close()
	// A killed run: its lock file is there, but no process holds it.
	killed := filepath.Join(base, reproduceFolderPrefix+"111")
	if err := os.MkdirAll(filepath.Join(killed, "project", "env"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(killed, reproduceLock), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Without a lock (app 0.34.0, or a run that has just created its folder):
	// only once it is old.
	old := filepath.Join(base, "codeflower-archive-222")
	recent := filepath.Join(base, reproduceFolderPrefix+"333")
	other := filepath.Join(base, "codeflower-client-444")
	for _, path := range []string{old, recent, other} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	day := time.Now().Add(-25 * time.Hour)
	for _, path := range []string{old, other} {
		if err := os.Chtimes(path, day, day); err != nil {
			t.Fatal(err)
		}
	}

	removed := RemoveStaleReproductions(context.Background(), EngineStatus{})
	if len(removed) != 2 {
		t.Fatalf("removed %v", removed)
	}
	for _, path := range []string{killed, old} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s is still there", path)
		}
	}
	for _, path := range []string{running.Path, recent, other} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s was removed", path)
		}
	}
}
