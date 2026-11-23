package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"codeflower-app/internal/jobs"
)

// testProject makes a project with saved settings, an allowlist, an env
// folder and a run's leftovers.
func testProject(t *testing.T) (Project, string, string) {
	t.Helper()
	dir := t.TempDir()
	// Before TempDir's own cleanup, which cannot enter read-only folders.
	t.Cleanup(func() { removeTree(dir) })
	RunningDirectory = func() string { return filepath.Join(dir, "running") }
	t.Cleanup(func() { RunningDirectory = func() string { return "" } })
	projects := filepath.Join(dir, "projects")
	envDir := filepath.Join(dir, "env")
	state, err := PrepareState(filepath.Join(projects, "study42"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareEnv(EnvFolder(envDir, state), state); err != nil {
		t.Fatal(err)
	}
	write := func(path, content string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(state, "sessions", "chat.json"), "chat", 0o600)
	write(filepath.Join(state, "instances", "old.json"), "connection", 0o600)
	write(filepath.Join(state, "run", "image-key"), "secret", 0o600)
	write(filepath.Join(EnvFolder(envDir, state), "env", "lib.so"), "lib", 0o755)
	write(DefaultAllowlist(state), "example.org\n", 0o600)
	if runtime.GOOS != "windows" {
		if err := os.Symlink("chat.json", filepath.Join(state, "sessions", "latest")); err != nil {
			t.Fatal(err)
		}
	}
	// Like Go's module cache: a read-only folder.
	readOnly := filepath.Join(state, "cache", "mod")
	if err := os.MkdirAll(readOnly, 0o700); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(readOnly, "go.mod"), "module x", 0o444)
	if err := os.Chmod(readOnly, 0o555); err != nil {
		t.Fatal(err)
	}
	settings := Settings{Engine: "docker", Port: 41234, EnvDir: envDir}
	if err := settings.Renew(); err != nil {
		t.Fatal(err)
	}
	if err := WriteSettings(SettingsFile(state), "study42", settings); err != nil {
		t.Fatal(err)
	}
	project, err := OpenProject(state, envDir)
	if err != nil {
		t.Fatal(err)
	}
	if project.Env != EnvFolder(envDir, state) {
		t.Fatalf("env folder %q", project.Env)
	}
	return project, projects, envDir
}

func TestProjectsAreListed(t *testing.T) {
	_, projects, _ := testProject(t)
	// Not projects: a file and a folder without the layout.
	os.WriteFile(filepath.Join(projects, "notes.txt"), nil, 0o600)
	os.MkdirAll(filepath.Join(projects, "other"), 0o700)
	list, err := ListProjects(projects, "")
	if err != nil || len(list) != 1 || list[0].Name != "study42" {
		t.Fatalf("%+v %v", list, err)
	}
	if list, err := ListProjects(filepath.Join(projects, "none"), ""); err != nil || list != nil {
		t.Fatalf("missing directory: %+v %v", list, err)
	}
	if _, err := OpenProject(filepath.Join(projects, "other"), ""); err == nil {
		t.Fatal("a folder without the layout opened as a project")
	}
}

func TestACopyHasItsOwnKeyAndNoRuns(t *testing.T) {
	project, projects, envDir := testProject(t)
	copied, err := CopyProject(context.Background(), project, filepath.Join(projects, "study42-v2"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if copied.Env != EnvFolder(envDir, copied.State) {
		t.Fatalf("env %q", copied.Env)
	}
	if data, err := os.ReadFile(filepath.Join(copied.State, "sessions", "chat.json")); err != nil || string(data) != "chat" {
		t.Fatalf("chat %q %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(copied.Env, "env", "lib.so")); err != nil || string(data) != "lib" {
		t.Fatalf("env %q %v", data, err)
	}
	if _, err := os.Stat(DefaultAllowlist(copied.State)); err != nil {
		t.Fatalf("allowlist: %v", err)
	}
	for _, left := range []string{"instances/old.json", "run/image-key"} {
		if _, err := os.Stat(filepath.Join(copied.State, left)); !os.IsNotExist(err) {
			t.Errorf("%s was copied: %v", left, err)
		}
	}
	if info, err := os.Stat(filepath.Join(copied.State, "run")); err != nil || !info.IsDir() {
		t.Errorf("run/ is missing: %v", err)
	}
	if runtime.GOOS != "windows" {
		if link, err := os.Readlink(filepath.Join(copied.State, "sessions", "latest")); err != nil || link != "chat.json" {
			t.Errorf("link %q %v", link, err)
		}
		if info, _ := os.Stat(filepath.Join(copied.State, "cache", "mod")); info.Mode().Perm() != 0o555 {
			t.Errorf("folder mode %v", info.Mode())
		}
		if info, _ := os.Stat(copied.State); info.Mode().Perm() != 0o700 {
			t.Errorf("project mode %v", info.Mode())
		}
	}
	original, _ := ReadSettings(project.Settings())
	settings, err := ReadSettings(copied.Settings())
	if err != nil || settings.Key == original.Key || settings.Port != 0 || settings.Engine != "docker" {
		t.Fatalf("copy's settings %+v %v", settings, err)
	}
	if certificate, err := settings.TLS(); err != nil || certificate.CertPEM == nil {
		t.Fatalf("copy's certificate: %v", err)
	}

	if _, err := CopyProject(context.Background(), project, copied.State, "", nil); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("copy over a project: %v", err)
	}
	if _, err := CopyProject(context.Background(), project, filepath.Join(project.State, "inner"), "", nil); err == nil {
		t.Fatal("copied into itself")
	}
}

func TestAMoveTakesEverythingAlong(t *testing.T) {
	project, projects, envDir := testProject(t)
	before, _ := ReadSettings(project.Settings())
	moved, err := MoveProject(context.Background(), project, filepath.Join(projects, "renamed"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{project.State, project.Settings(), project.Allowlist(), project.Env} {
		if _, err := os.Lstat(gone); !os.IsNotExist(err) {
			t.Errorf("%s is still there: %v", gone, err)
		}
	}
	if moved.Env != EnvFolder(envDir, moved.State) || !IsProject(moved.State) {
		t.Fatalf("moved %+v", moved)
	}
	after, err := ReadSettings(moved.Settings())
	if err != nil || after.Key != before.Key || after.Port != before.Port {
		t.Fatalf("the moved project's key or port changed: %+v %v", after, err)
	}
	if _, err := os.Stat(DefaultAllowlist(moved.State)); err != nil {
		t.Fatalf("allowlist: %v", err)
	}
}

func TestARunningProjectIsLeftAlone(t *testing.T) {
	project, projects, _ := testProject(t)
	unregister := register(Running{ID: "now", State: project.State, PID: os.Getpid(), Kind: "worker"})
	defer unregister()
	for name, err := range map[string]error{
		"copy": func() error {
			_, err := CopyProject(context.Background(), project, filepath.Join(projects, "c"), "", nil)
			return err
		}(),
		"move": func() error {
			_, err := MoveProject(context.Background(), project, filepath.Join(projects, "m"), "", nil)
			return err
		}(),
		"remove": RemoveProject(context.Background(), project, nil),
	} {
		if err == nil || !strings.Contains(err.Error(), "while a sandbox uses it") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if !IsProject(project.State) {
		t.Fatal("the running project is gone")
	}
}

func TestRemovingAProjectRemovesItsFiles(t *testing.T) {
	project, _, _ := testProject(t)
	if err := RemoveProject(context.Background(), project, nil); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{project.State, project.Settings(), project.Allowlist(), project.Env} {
		if _, err := os.Lstat(gone); !os.IsNotExist(err) {
			t.Errorf("%s is still there: %v", gone, err)
		}
	}
}

func TestASizeCountsEnvAndHardLinksOnce(t *testing.T) {
	project, _, _ := testProject(t)
	before, complete := project.Size()
	if !complete || before <= 0 {
		t.Fatalf("size %d, complete %v", before, complete)
	}
	big := filepath.Join(project.Env, "env", "big")
	if err := os.WriteFile(big, make([]byte, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	withBig, _ := project.Size()
	if withBig-before < 1<<20 {
		t.Fatalf("the env folder's file is not counted: %d then %d", before, withBig)
	}
	if runtime.GOOS == "windows" {
		return
	}
	if err := os.Link(big, filepath.Join(project.State, "home", "big")); err != nil {
		t.Fatal(err)
	}
	if linked, _ := project.Size(); linked-withBig >= 1<<20 {
		t.Fatalf("a hard link counts twice: %d then %d", withBig, linked)
	}
}

func TestBatchJobsGoWithTheProject(t *testing.T) {
	project, projects, _ := testProject(t)
	ledger, err := jobs.OpenLedger(project.State)
	if err != nil {
		t.Fatal(err)
	}
	id, err := ledger.Create()
	if err != nil {
		t.Fatal(err)
	}
	record := &jobs.Record{ID: id, Name: "align", State: jobs.StateRunning}
	if err := ledger.Save(record); err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"move": func() error {
			_, err := MoveProject(context.Background(), project, filepath.Join(projects, "m"), "", nil)
			return err
		}(),
		"remove": RemoveProject(context.Background(), project, nil),
	} {
		if err == nil || !strings.Contains(err.Error(), "batch jobs are queued or running") {
			t.Errorf("%s: %v", name, err)
		}
	}
	record.State = jobs.StateDone
	if err := ledger.Save(record); err != nil {
		t.Fatal(err)
	}
	moved, err := MoveProject(context.Background(), project, filepath.Join(projects, "moved"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(jobs.LedgerDir(moved.State), id, jobs.RecordFile)); err != nil {
		t.Fatalf("the jobs did not move along: %v", err)
	}
	if err := RemoveProject(context.Background(), moved, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(jobs.LedgerDir(moved.State)); !os.IsNotExist(err) {
		t.Fatalf("the jobs folder is still there: %v", err)
	}
}

// A project moved to a folder of the same name elsewhere keeps its env
// folder where it is, instead of finding it in its own way.
func TestAMoveToAnotherFolderOfTheSameNameKeepsTheEnvFolder(t *testing.T) {
	project, projects, envDir := testProject(t)
	target := filepath.Join(filepath.Dir(projects), "elsewhere", "study42")
	moved, err := MoveProject(context.Background(), project, target, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if moved.Env != project.Env || moved.Env != EnvFolder(envDir, moved.State) {
		t.Fatalf("env folder %s, was %s", moved.Env, project.Env)
	}
	if _, err := os.Stat(filepath.Join(moved.Env, "env", "lib.so")); err != nil {
		t.Fatalf("the env folder lost its files: %v", err)
	}
	if _, err := os.Lstat(project.State); !os.IsNotExist(err) {
		t.Errorf("%s is still there: %v", project.State, err)
	}
	if _, err := CopyProject(context.Background(), moved, filepath.Join(projects, "study42"), "", nil); err == nil ||
		!strings.Contains(err.Error(), "share the env folder") {
		t.Fatalf("copied onto the original's env folder: %v", err)
	}
}
