package sandbox

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func TestTasksAreWrittenPrivatelyAndTheirResultsRead(t *testing.T) {
	state, err := PrepareState(tempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	binds := []Bind{{Source: "/home/u/results", Target: OutputsRoot + "/study42"}, {Source: "/home/u/data", Target: "/data/input/data", ReadOnly: true}}
	task := Task{Task: "Cluster the cells", Agent: "claude", Output: OutputsRoot + "/study42", Report: true, MaxRounds: 3,
		Model: "opus", Effort: "high", Options: map[string]string{"fast": "on"}, Mounts: TaskMounts(binds),
		Allowlist: "/home/u/allowed.txt", Engine: "auto", Image: "codeflower-sandbox:dev"}
	if err := WriteTask(state, "abc", task); err != nil {
		t.Fatal(err)
	}
	directory := TaskDirectory(state, "abc")
	file := filepath.Join(directory, "task.json")
	if info, err := os.Stat(file); err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("task.json: %v %v", info, err)
	}
	written, err := ReadTask(directory)
	if err != nil || !reflect.DeepEqual(written, task) || !reflect.DeepEqual(written.Binds(), binds) {
		t.Fatalf("task.json holds %+v, %v", written, err)
	}

	if n, err := NextAttempt(directory); err != nil || n != 1 {
		t.Fatalf("the first attempt is %d, %v", n, err)
	}
	attempt := TaskAttempt{Attempt: 1, Progress: "json", MaxRounds: 3, WrapUpAt: "2026-10-29T12:00:00Z"}
	if err := WriteAttempt(directory, attempt); err != nil {
		t.Fatal(err)
	}
	if err := WriteAttempt(directory, attempt); err == nil {
		t.Fatal("wrote attempt 1 twice")
	}
	attemptFile := filepath.Join(directory, "attempts", "1", "attempt.json")
	if info, err := os.Stat(attemptFile); err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("attempt.json: %v %v", info, err)
	}
	var writtenAttempt TaskAttempt
	data, _ := os.ReadFile(attemptFile)
	if err := json.Unmarshal(data, &writtenAttempt); err != nil || !reflect.DeepEqual(writtenAttempt, attempt) {
		t.Fatalf("attempt.json holds %+v, %v", writtenAttempt, err)
	}
	if AttemptFile("abc", 2) != ContainerState+"/tasks/abc/attempts/2/attempt.json" {
		t.Fatalf("the sandbox looks for the attempt at %s", AttemptFile("abc", 2))
	}
	if err := os.MkdirAll(filepath.Join(directory, "attempts", "7"), 0o700); err != nil {
		t.Fatal(err)
	}
	if n, err := NextAttempt(directory); err != nil || n != 8 {
		t.Fatalf("the attempt after 7 is %d, %v", n, err)
	}

	if _, err := ReadAttemptResult(directory, 1); err == nil {
		t.Fatal("read a result the sandbox never wrote")
	}
	result := `{"status":"unmet","rounds":2,"attempt":1,"continuation":{"kind":"interrupted","followUp":true},"wrapUp":{"reason":"time","at":"2026-10-29T12:00:01Z"},"report":{"path":"/data/output/study42/report.html","exists":true,"checked":true},"usage":{"auth":{"kind":"subscription","plan":"max"},"limits":[{"id":"five_hour","label":"5-hour","usedPercent":25}],"costEstimate":true,"totals":{"tokens":{"totalTokens":170},"cost":{"amount":0.25,"currency":"USD"}}}}`
	if err := os.WriteFile(filepath.Join(directory, "attempts", "1", "result.json"), []byte(result), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadAttemptResult(directory, 1)
	if err != nil || got.Status != "unmet" || got.Rounds != 2 || got.Report == nil || !got.Report.Checked ||
		got.Usage == nil || got.Usage.Auth.Plan != "max" || got.Usage.Totals.Tokens.Total != 170 ||
		got.Usage.Totals.Cost == nil || got.Usage.Totals.Cost.Amount != 0.25 || got.Attempt != 1 ||
		got.Continuation == nil || *got.Continuation != (TaskContinuation{Kind: "interrupted", FollowUp: true}) ||
		got.WrapUp == nil || got.WrapUp.Reason != "time" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestTaskFoldersAreFoundFromTheirPath(t *testing.T) {
	state, err := PrepareState(tempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteTask(state, "abc", Task{Task: "Cluster the cells"}); err != nil {
		t.Fatal(err)
	}
	directory, project, id, err := ResolveTaskDirectory(filepath.Join(state, "tasks", "abc") + string(filepath.Separator))
	if err != nil || directory != filepath.Join(state, "tasks", "abc") || project != state || id != "abc" {
		t.Fatalf("got %q %q %q, %v", directory, project, id, err)
	}
	for _, path := range []string{filepath.Join(state, "tasks", "missing"), filepath.Join(state, "tasks"), state} {
		if _, _, _, err := ResolveTaskDirectory(path); err == nil {
			t.Errorf("%s was taken for a task folder", path)
		}
	}
	other := filepath.Join(state, "elsewhere", "abc")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "task.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := ResolveTaskDirectory(other); err == nil {
		t.Error("a task.json outside a tasks folder was taken for a task")
	}
}

func TestATaskRunsOnceAtATime(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tasks are not locked on Windows")
	}
	directory := tempDir(t)
	unlock, err := LockTask(directory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LockTask(directory); !errors.Is(err, ErrTaskRunning) {
		t.Fatalf("a second run got %v", err)
	}
	unlock()
	again, err := LockTask(directory)
	if err != nil {
		t.Fatalf("the task stayed locked: %v", err)
	}
	again()
}

func TestTaskResultsMapToExitCodes(t *testing.T) {
	for status, code := range map[string]int{
		"completed": 0, "unmet": 2, "not_signed_in": 3, "cancelled": 130, "failed": 1, "": 1,
	} {
		if got := (TaskResult{Status: status}).ExitCode(); got != code {
			t.Errorf("%q exits %d, want %d", status, got, code)
		}
	}
}
