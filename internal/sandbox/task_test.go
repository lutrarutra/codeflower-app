package sandbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestTasksAreWrittenPrivatelyAndTheirResultsRead(t *testing.T) {
	state, err := PrepareState(tempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	task := Task{Task: "Cluster the cells", Agent: "claude", Output: OutputsRoot + "/study42", Report: true, MaxRounds: 3,
		Model: "opus", Effort: "high", Options: map[string]string{"fast": "on"}}
	if err := WriteTask(state, "abc", task); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(state, "tasks", "abc", "task.json")
	if info, err := os.Stat(file); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("task.json: %v %v", info, err)
	}
	var written Task
	data, _ := os.ReadFile(file)
	if err := json.Unmarshal(data, &written); err != nil || !reflect.DeepEqual(written, task) {
		t.Fatalf("task.json holds %+v, %v", written, err)
	}
	if TaskFile("abc") != ContainerState+"/tasks/abc/task.json" {
		t.Fatalf("the sandbox looks for the task at %s", TaskFile("abc"))
	}

	if _, err := ReadTaskResult(state, "abc"); err == nil {
		t.Fatal("read a result the sandbox never wrote")
	}
	result := `{"status":"unmet","rounds":2,"report":{"path":"/data/output/study42/report.html","exists":true,"checked":true}}`
	if err := os.WriteFile(filepath.Join(state, "tasks", "abc", "result.json"), []byte(result), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadTaskResult(state, "abc")
	if err != nil || got.Status != "unmet" || got.Rounds != 2 || got.Report == nil || !got.Report.Checked {
		t.Fatalf("got %+v, %v", got, err)
	}
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
