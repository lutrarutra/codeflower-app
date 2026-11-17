package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// TestRemovingAWideReadOnlyTree removes more entries per folder than a
// batch holds, folders among them, below read-only folders and files.
func TestRemovingAWideReadOnlyTree(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tree")
	t.Cleanup(func() { os.RemoveAll(root) })
	wide := filepath.Join(root, "wide")
	entries := int64(1)
	mkdir := func(path string) {
		t.Helper()
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		entries++
	}
	mkdir(wide)
	for index := range readBatch + 100 {
		path := filepath.Join(wide, fmt.Sprintf("f%d", index))
		if err := os.WriteFile(path, nil, 0o444); err != nil {
			t.Fatal(err)
		}
		entries++
		if index%2 == 0 {
			folder := filepath.Join(wide, fmt.Sprintf("d%d", index))
			mkdir(folder)
			for depth := range 3 {
				folder = filepath.Join(folder, fmt.Sprint(depth))
				mkdir(folder)
			}
			if err := os.WriteFile(filepath.Join(folder, "leaf"), nil, 0o444); err != nil {
				t.Fatal(err)
			}
			entries++
			if index%10 == 0 {
				os.Chmod(folder, 0o555)
				os.Chmod(filepath.Dir(folder), 0o555)
			}
		}
	}
	if err := os.Chmod(wide, 0o555); err != nil {
		t.Fatal(err)
	}
	m, stop := newMeter(nil)
	defer stop()
	if err := m.work(context.Background()).remove(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatalf("still there: %v", err)
	}
	if removed := m.files.Load(); removed != entries {
		t.Errorf("counted %d removed, made %d", removed, entries)
	}
}

func TestProgressHearsTheCount(t *testing.T) {
	before := progressEvery
	progressEvery = time.Millisecond
	t.Cleanup(func() { progressEvery = before })
	var heard atomic.Int64
	m, stop := newMeter(func(doing string, files int64) {
		if doing == "Removing" {
			heard.Store(files)
		}
	})
	m.phase("Removing")
	m.files.Add(7)
	deadline := time.Now().Add(5 * time.Second)
	for heard.Load() != 7 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	stop()
	if heard.Load() != 7 {
		t.Fatalf("heard %d", heard.Load())
	}
}

func TestAStoppedRemovalIsFinishedLater(t *testing.T) {
	project, projects, envDir := testProject(t)
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	if err := RemoveProject(stopped, project, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("stopped removal: %v", err)
	}
	listed, err := ListProjects(projects, envDir)
	if err != nil || len(listed) != 0 {
		t.Fatalf("listed %v, %v", listed, err)
	}
	if IsProject(project.State) {
		t.Fatal("the project is still in its place")
	}
	leftover, found := LeftoverRemoval(project.State, envDir)
	if !found || leftover.Env == "" || leftover.Name != "study42" {
		t.Fatalf("leftover %+v, found %v", leftover, found)
	}
	if err := RemoveProject(context.Background(), leftover, nil); err != nil {
		t.Fatal(err)
	}
	if _, found := LeftoverRemoval(project.State, envDir); found {
		t.Fatal("a leftover is still there")
	}
	for _, folder := range []string{projects, envDir} {
		if entries, _ := os.ReadDir(folder); len(entries) != 0 {
			t.Errorf("%s still has %v", folder, entries)
		}
	}
}

func TestAStoppedCopyIsRemoved(t *testing.T) {
	project, projects, _ := testProject(t)
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	target := filepath.Join(projects, "copy")
	if _, err := CopyProject(stopped, project, target, "", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("stopped copy: %v", err)
	}
	for _, path := range []string{target, SettingsFile(target), DefaultAllowlist(target), EnvFolder(filepath.Dir(project.Env), target)} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("%s is left: %v", path, err)
		}
	}
	if !IsProject(project.State) {
		t.Fatal("the original is gone")
	}
}
