package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestRunsAreListedWhileTheirProcessRuns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sleep")
	}
	dir := t.TempDir()
	RunningDirectory = func() string { return dir }
	t.Cleanup(func() { RunningDirectory = func() string { return "" } })

	unregister := register(Running{ID: "mine", PID: os.Getpid(), Kind: "worker"})
	// A run whose process is gone left its file behind.
	sleeper := exec.Command("sleep", "0")
	if err := sleeper.Run(); err != nil {
		t.Fatal(err)
	}
	register(Running{ID: "gone", PID: sleeper.Process.Pid})

	runs := ListRunning()
	if len(runs) != 1 || runs[0].ID != "mine" || runs[0].Kind != "worker" {
		t.Fatalf("runs %+v", runs)
	}
	if _, err := os.Stat(filepath.Join(dir, "gone.json")); !os.IsNotExist(err) {
		t.Fatalf("the ended run's file is still there: %v", err)
	}
	if info, err := os.Stat(filepath.Join(dir, "mine.json")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("registration %v %v", info, err)
	}
	unregister()
	if runs := ListRunning(); len(runs) != 0 {
		t.Fatalf("after the run ended: %+v", runs)
	}
}

func TestARunsConnectionIsReadOnceTheBridgeWroteIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance.json")
	run := Running{ConnectionFile: path}
	if _, ok := run.ReadConnection(); ok {
		t.Fatal("connection before the bridge wrote it")
	}
	os.WriteFile(path, []byte(`{"url":"codeflower://127.0.0.1:4000?token=t","host":"127.0.0.1","port":4000,"token":"t"}`), 0o600)
	if connection, ok := run.ReadConnection(); !ok || connection.Port != 4000 || connection.Token != "t" {
		t.Fatalf("%+v %v", connection, ok)
	}
	if _, ok := (Running{Kind: "task"}).ReadConnection(); ok {
		t.Fatal("a task has no connection")
	}
}
