package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// acrossFileSystems makes every move in the test copy, as between file
// systems.
func acrossFileSystems(t *testing.T) {
	t.Helper()
	before := moveRename
	moveRename = func(from, to string) error {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: syscall.EXDEV}
	}
	t.Cleanup(func() { moveRename = before })
}

// writableTempDir is a temporary folder that is removed also when copies
// in it kept read-only folders.
func writableTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() { removeTree(dir) })
	return dir
}

var past = time.Date(2024, 3, 1, 12, 30, 15, 123456789, time.UTC)

// dataTree makes a folder like a workspace: data files with old
// modification times, a symbolic link, a hard link and a read-only folder.
func dataTree(t *testing.T, root string) {
	t.Helper()
	writeFile(t, filepath.Join(root, "raw", "counts.csv"), strings.Repeat("1,2\n", 1000))
	writeFile(t, filepath.Join(root, "results", "table.csv"), "a,b")
	writeFile(t, filepath.Join(root, "Snakefile"), "rule all:")
	for _, path := range []string{"raw/counts.csv", "results/table.csv", "Snakefile"} {
		if err := os.Chtimes(filepath.Join(root, path), past, past); err != nil {
			t.Fatal(err)
		}
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink("results/table.csv", filepath.Join(root, "latest")); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(filepath.Join(root, "raw", "counts.csv"), filepath.Join(root, "results", "counts-link.csv")); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(filepath.Join(root, "raw"), past, past); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "raw"), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(root, "raw"), 0o700) })
}

func TestACopyKeepsTimesLinksAndModes(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	dataTree(t, source)
	target := filepath.Join(t.TempDir(), "target")
	t.Cleanup(func() { os.Chmod(filepath.Join(target, "raw"), 0o700) })
	m, stop := newMeter(nil)
	defer stop()
	if err := m.work(context.Background()).copy(source, target, nil); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(target, "raw", "counts.csv"))
	if err != nil || !info.ModTime().Equal(past) {
		t.Fatalf("modification time %v, %v", info.ModTime(), err)
	}
	if folder, _ := os.Stat(filepath.Join(target, "raw")); folder.Mode().Perm() != 0o555 || !folder.ModTime().Equal(past) {
		t.Fatalf("folder %v %v", folder.Mode(), folder.ModTime())
	}
	if runtime.GOOS != "windows" {
		if link, _ := os.Readlink(filepath.Join(target, "latest")); link != "results/table.csv" {
			t.Fatalf("link %q", link)
		}
		a, _ := os.Stat(filepath.Join(target, "raw", "counts.csv"))
		b, _ := os.Stat(filepath.Join(target, "results", "counts-link.csv"))
		if !os.SameFile(a, b) {
			t.Fatal("the hard link became two files")
		}
	}
	if m.bytes.Load() != 4000+3+9+4000 {
		t.Fatalf("counted %d bytes", m.bytes.Load())
	}
}

// A copy run again over what a stopped one left writes only what is
// missing or changed, and a half-written file is never taken as complete.
func TestACopyContinuesWhereItStopped(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	dataTree(t, source)
	target := filepath.Join(t.TempDir(), "target")
	t.Cleanup(func() { os.Chmod(filepath.Join(target, "raw"), 0o700) })
	m, stop := newMeter(nil)
	defer stop()
	if err := m.work(context.Background()).copy(source, target, nil); err != nil {
		t.Fatal(err)
	}
	// As a stopped copy leaves it: a file missing, one cut short under its
	// partial name, and one in place with other contents.
	os.Remove(filepath.Join(target, "Snakefile"))
	writeFile(t, partialName(filepath.Join(target, "Snakefile")), "ru")
	writeFile(t, filepath.Join(target, "results", "table.csv"), "x,y")

	m.phase("Copying")
	if err := m.work(context.Background()).copy(source, target, nil); err != nil {
		t.Fatal(err)
	}
	// table.csv has the same size, but not the same time: written again.
	if written := m.written.Load(); written != 9+3 {
		t.Fatalf("wrote %d bytes again, want only the missing and changed files' 12", written)
	}
	if readFile(t, filepath.Join(target, "Snakefile")) != "rule all:" || readFile(t, filepath.Join(target, "results", "table.csv")) != "a,b" {
		t.Fatal("not copied again")
	}
	if exists(partialName(filepath.Join(target, "Snakefile"))) {
		t.Fatal("the partial file is left")
	}
}

func TestAStoppedWorkspaceMoveContinues(t *testing.T) {
	acrossFileSystems(t)
	project, _, _ := testProject(t)
	dataTree(t, filepath.Join(project.State, "workspace"))
	workspaces := filepath.Join(resolved(t, writableTempDir(t)), "workspaces")
	to := WorkspaceFolder(workspaces, project.State)

	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := MoveWorkspace(stopped, project, workspaces, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("stopped move: %v", err)
	}
	transfer, found, err := ReadTransfer(project.State)
	if err != nil || !found || transfer.Operation != TransferMoveWorkspace || transfer.To != to ||
		len(transfer.Partial) != 1 || transfer.Partial[0] != stagingPath(to) {
		t.Fatalf("transfer %+v %v %v", transfer, found, err)
	}
	if exists(to) || readFile(t, filepath.Join(project.State, "workspace", "Snakefile")) != "rule all:" {
		t.Fatal("a stopped move changed the project")
	}
	if saved, _ := SavedWorkspaceDir(project.State); saved != "" {
		t.Fatalf("the settings name %q", saved)
	}
	// Another operation waits for it.
	if _, err := MoveProject(context.Background(), project, filepath.Join(t.TempDir(), "x"), "", nil); !errors.As(err, &PendingTransferError{}) {
		t.Fatalf("moved the project over an unfinished workspace move: %v", err)
	}
	// So does the same one to somewhere else.
	if _, err := MoveWorkspace(context.Background(), project, t.TempDir(), nil); !errors.As(err, &PendingTransferError{}) {
		t.Fatalf("moved the workspace elsewhere: %v", err)
	}

	moved, err := MoveWorkspace(context.Background(), project, workspaces, nil)
	if err != nil {
		t.Fatal(err)
	}
	if moved.Workspace != to || exists(stagingPath(to)) {
		t.Fatalf("moved %+v", moved)
	}
	info, err := os.Stat(filepath.Join(to, "raw", "counts.csv"))
	if err != nil || !info.ModTime().Equal(past) {
		t.Fatalf("modification time %v, %v", info.ModTime(), err)
	}
	if !emptyFolder(filepath.Join(project.State, "workspace")) {
		t.Fatal("the original is left in the project")
	}
	if exists(RemovingPath(filepath.Join(project.State, "workspace"))) {
		t.Fatal("the hidden original is left")
	}
	if _, found, _ := ReadTransfer(project.State); found {
		t.Fatal("the transfer is left")
	}
	if saved, _ := SavedWorkspaceDir(project.State); saved != workspaces {
		t.Fatalf("the settings name %q", saved)
	}
}

func TestAnUnfinishedMoveCanBeDropped(t *testing.T) {
	acrossFileSystems(t)
	project, _, _ := testProject(t)
	writeFile(t, filepath.Join(project.State, "workspace", "a.txt"), "a")
	workspaces := t.TempDir()
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	MoveWorkspace(stopped, project, workspaces, nil)
	transfer, found, _ := ReadTransfer(project.State)
	if !found {
		t.Fatal("no transfer")
	}
	// What it had copied so far.
	writeFile(t, filepath.Join(transfer.Partial[0], "a.txt"), "a")
	if _, err := AbortTransfer(context.Background(), project, nil); err != nil {
		t.Fatal(err)
	}
	if exists(transfer.Partial[0]) || exists(TransferFile(project.State)) {
		t.Fatal("the partial copy or the transfer is left")
	}
	if readFile(t, filepath.Join(project.State, "workspace", "a.txt")) != "a" {
		t.Fatal("the workspace changed")
	}
	if _, err := AbortTransfer(context.Background(), project, nil); err == nil {
		t.Fatal("dropped a transfer that is not there")
	}
}

// A move stopped while removing the original finishes that when run again.
func TestAMoveStoppedWhileRemovingFinishes(t *testing.T) {
	project, _, _ := projectWithWorkspace(t)
	leftover := RemovingPath(filepath.Join(project.State, "workspace"))
	writeFile(t, filepath.Join(leftover, "old.csv"), "old")
	if err := writeTransfer(project.State, Transfer{Operation: TransferMoveWorkspace, To: project.Workspace, Removing: []string{leftover}}); err != nil {
		t.Fatal(err)
	}
	if _, err := AbortTransfer(context.Background(), project, nil); err == nil {
		t.Fatal("dropped a move that is complete")
	}
	moved, err := MoveWorkspace(context.Background(), project, filepath.Dir(project.Workspace), nil)
	if err != nil || moved.Workspace != project.Workspace {
		t.Fatalf("moved %+v %v", moved, err)
	}
	if exists(leftover) || exists(TransferFile(project.State)) {
		t.Fatal("the original or the transfer is left")
	}
}

func TestAStoppedProjectMoveContinuesAndIsNotListedMeanwhile(t *testing.T) {
	acrossFileSystems(t)
	project, projects, envDir := testProject(t)
	elsewhere := filepath.Join(resolved(t, writableTempDir(t)), "projects")
	target := filepath.Join(elsewhere, "study42")
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := MoveProject(stopped, project, target, "", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("stopped move: %v", err)
	}
	if !IsProject(project.State) || !exists(project.Settings()) || exists(target) || exists(SettingsFile(target)) {
		t.Fatal("a stopped move changed the project")
	}
	if listed, _ := ListProjects(elsewhere, envDir); len(listed) != 0 {
		t.Fatalf("the partial copy is listed: %+v", listed)
	}
	if _, found, _ := ReadTransfer(project.State); !found {
		t.Fatal("no transfer")
	}
	moved, err := MoveProject(context.Background(), project, target, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !IsProject(moved.State) || exists(project.State) || exists(TransferFile(project.State)) || exists(stagingPath(target)) {
		t.Fatalf("moved %+v", moved)
	}
	if listed, _ := ListProjects(projects, envDir); len(listed) != 0 {
		t.Fatalf("the original is listed: %+v", listed)
	}
}

func TestAStoppedProjectCopyContinues(t *testing.T) {
	project, projects, _ := testProject(t)
	target := filepath.Join(projects, "copied")
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := CopyProject(stopped, project, target, "", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("stopped copy: %v", err)
	}
	if exists(target) {
		t.Fatal("a stopped copy is in place")
	}
	copied, err := CopyProject(context.Background(), project, target, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !IsProject(copied.State) || exists(stagingPath(target)) || exists(TransferFile(project.State)) {
		t.Fatalf("copied %+v", copied)
	}
}

func TestRemovingAProjectDropsItsUnfinishedCopy(t *testing.T) {
	acrossFileSystems(t)
	project, _, _ := testProject(t)
	writeFile(t, filepath.Join(project.State, "workspace", "a.txt"), "a")
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	workspaces := t.TempDir()
	MoveWorkspace(stopped, project, workspaces, nil)
	transfer, _, _ := ReadTransfer(project.State)
	writeFile(t, filepath.Join(transfer.Partial[0], "a.txt"), "a")
	if err := RemoveProject(context.Background(), project, nil); err != nil {
		t.Fatal(err)
	}
	if exists(transfer.Partial[0]) || exists(TransferFile(project.State)) {
		t.Fatal("the unfinished copy outlived its project")
	}
}
