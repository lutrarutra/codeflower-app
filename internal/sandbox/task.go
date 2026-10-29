package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// Task is what `codeflower task` asks the sandbox to do, written once to
// task.json in the task's folder. Each run of it is an attempt
// (attempts/<n>/attempt.json); the sandbox writes the attempt's TaskResult
// next to it, and session.json once it started the task's session.
type Task struct {
	Task  string `json:"task"`
	Agent string `json:"agent"`
	// Output is the sandbox folder for the results, e.g. /data/output/study42.
	Output    string `json:"output"`
	Report    bool   `json:"report"`
	MaxRounds int    `json:"maxRounds"`
	// Model and Effort are values of the agent's model and effort settings,
	// e.g. opus and high; Options sets other settings by their id.
	Model     string            `json:"model,omitempty"`
	Effort    string            `json:"effort,omitempty"`
	Options   map[string]string `json:"options,omitempty"`
	CreatedAt string            `json:"createdAt"`
	// Mounts are the sandbox's folders in their order, and Allowlist the
	// isolated network's allowlist (empty when the network was not isolated):
	// a continued task gets the same.
	Mounts    []TaskMount `json:"mounts"`
	Allowlist string      `json:"allowlist,omitempty"`
	// Engine and Image are what the task was started with; a continued task
	// uses them unless it is given others.
	Engine string `json:"engine,omitempty"`
	Image  string `json:"image,omitempty"`
}

// TaskMount is a Bind as task.json keeps it.
type TaskMount struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"readOnly"`
}

// TaskMounts are binds as task.json keeps them.
func TaskMounts(binds []Bind) []TaskMount {
	mounts := make([]TaskMount, 0, len(binds))
	for _, bind := range binds {
		mounts = append(mounts, TaskMount{Source: bind.Source, Target: bind.Target, ReadOnly: bind.ReadOnly})
	}
	return mounts
}

// Binds are the task's mounts again.
func (t Task) Binds() []Bind {
	binds := make([]Bind, 0, len(t.Mounts))
	for _, mount := range t.Mounts {
		binds = append(binds, Bind{Source: mount.Source, Target: mount.Target, ReadOnly: mount.ReadOnly})
	}
	return binds
}

// TaskAttempt is one run of a task. Its settings, when set, replace the
// task's for this run.
type TaskAttempt struct {
	Attempt int `json:"attempt"`
	// FollowUp is more work the user asks for in a continued task.
	FollowUp  string            `json:"followUp,omitempty"`
	Progress  string            `json:"progress,omitempty"`
	MaxRounds int               `json:"maxRounds,omitempty"`
	Model     string            `json:"model,omitempty"`
	Effort    string            `json:"effort,omitempty"`
	Options   map[string]string `json:"options,omitempty"`
	// WrapUpAt is when the agent is told to write up what it has, before
	// --timeout stops the run.
	WrapUpAt  string `json:"wrapUpAt,omitempty"`
	CreatedAt string `json:"createdAt"`
}

// TaskResult is how a task ended.
type TaskResult struct {
	// Status is completed, unmet, failed, cancelled or not_signed_in.
	Status     string      `json:"status"`
	Message    string      `json:"message,omitempty"`
	StopReason string      `json:"stopReason,omitempty"`
	SessionKey string      `json:"sessionKey,omitempty"`
	Rounds     int         `json:"rounds"`
	StartedAt  string      `json:"startedAt"`
	EndedAt    string      `json:"endedAt"`
	Report     *TaskReport `json:"report,omitempty"`
	// Settings are the model, effort and requested settings the session ran with.
	Settings map[string]string `json:"settings,omitempty"`
	Usage    *TaskUsage        `json:"usage,omitempty"`
	Attempt  int               `json:"attempt,omitempty"`
	// Continuation says why a later attempt continued the session.
	Continuation *TaskContinuation `json:"continuation,omitempty"`
	// WrapUp is set when the agent was told to write up what it had.
	WrapUp *TaskWrapUp `json:"wrapUp,omitempty"`
}

// TaskContinuation is how an attempt went on from the one before.
type TaskContinuation struct {
	// Kind is restart (no session had been started), interrupted, error,
	// unmet or completed: how the attempt before ended.
	Kind     string `json:"kind"`
	FollowUp bool   `json:"followUp"`
}

// TaskWrapUp is why and when a task was wrapped up.
type TaskWrapUp struct {
	// Reason is time or rounds.
	Reason string `json:"reason"`
	At     string `json:"at"`
}

// TaskUsage is the account quota and token/cost totals reported during a task.
type TaskUsage struct {
	Auth struct {
		Kind  string `json:"kind"`
		Plan  string `json:"plan,omitempty"`
		Label string `json:"label,omitempty"`
	} `json:"auth"`
	Limits          []TaskUsageLimit `json:"limits"`
	LimitsUpdatedAt string           `json:"limitsUpdatedAt,omitempty"`
	Credits         *TaskCredits     `json:"credits,omitempty"`
	CostEstimate    bool             `json:"costEstimate"`
	Totals          struct {
		Tokens TaskTokens `json:"tokens"`
		Cost   *TaskCost  `json:"cost,omitempty"`
	} `json:"totals"`
}

type TaskUsageLimit struct {
	ID          string   `json:"id"`
	Label       string   `json:"label"`
	UsedPercent *float64 `json:"usedPercent"`
	ResetsAt    *string  `json:"resetsAt"`
	Status      string   `json:"status,omitempty"`
}

type TaskCredits struct {
	Unlimited bool   `json:"unlimited"`
	Balance   string `json:"balance,omitempty"`
}

type TaskTokens struct {
	Input       int64 `json:"inputTokens"`
	Output      int64 `json:"outputTokens"`
	CachedRead  int64 `json:"cachedReadTokens"`
	CachedWrite int64 `json:"cachedWriteTokens"`
	Thought     int64 `json:"thoughtTokens"`
	Total       int64 `json:"totalTokens"`
}

type TaskCost struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
}

// TaskReport is the report a task had to write, as the sandbox found it.
type TaskReport struct {
	Path    string `json:"path"`
	Exists  bool   `json:"exists"`
	Checked bool   `json:"checked"`
	Check   string `json:"check,omitempty"`
}

// Task exit codes; an interrupted task exits like an interrupted command.
const (
	TaskCompleted   = 0
	TaskFailed      = 1
	TaskUnmet       = 2
	TaskNotSignedIn = 3
	TaskInterrupted = 130
)

// ExitCode is the launcher's exit code for the result.
func (r TaskResult) ExitCode() int {
	switch r.Status {
	case "completed":
		return TaskCompleted
	case "unmet":
		return TaskUnmet
	case "not_signed_in":
		return TaskNotSignedIn
	case "cancelled":
		return TaskInterrupted
	}
	return TaskFailed
}

// ErrTaskRunning means another run of the task holds its lock.
var ErrTaskRunning = errors.New("this task is already running: wait for that run to end")

// TaskDirectory is a task's folder in the prepared project.
func TaskDirectory(state, id string) string {
	return filepath.Join(state, "tasks", id)
}

// AttemptDirectory is the folder of a task's attempt n.
func AttemptDirectory(directory string, n int) string {
	return filepath.Join(directory, "attempts", strconv.Itoa(n))
}

// AttemptFile is where the sandbox finds attempt n of a task.
func AttemptFile(task string, n int) string {
	return fmt.Sprintf("%s/tasks/%s/attempts/%d/attempt.json", ContainerState, task, n)
}

// WriteTask creates the task's folder and its task.json, privately.
func WriteTask(state, id string, task Task) error {
	directory := TaskDirectory(state, id)
	if err := privateDirectory(directory); err != nil {
		return err
	}
	data, err := json.MarshalIndent(task, "", "  ")
	if err != nil {
		return err
	}
	return writeOnce(filepath.Join(directory, "task.json"), append(data, '\n'))
}

// ReadTask reads the task.json in a task's folder.
func ReadTask(directory string) (Task, error) {
	var task Task
	data, err := os.ReadFile(filepath.Join(directory, "task.json"))
	if err != nil {
		return task, err
	}
	if err := json.Unmarshal(data, &task); err != nil {
		return task, fmt.Errorf("%s: %w", filepath.Join(directory, "task.json"), err)
	}
	return task, nil
}

// ResolveTaskDirectory checks that path is a task's folder, <project>/tasks/<id>,
// and returns its absolute path, the project and the task's id.
func ResolveTaskDirectory(path string) (directory, state, id string, err error) {
	directory, err = filepath.Abs(path)
	if err == nil {
		directory, err = filepath.EvalSymlinks(directory)
	}
	if err != nil {
		return "", "", "", fmt.Errorf("no task folder at %s: %w", path, err)
	}
	if _, err := os.Stat(filepath.Join(directory, "task.json")); err != nil || filepath.Base(filepath.Dir(directory)) != "tasks" {
		return "", "", "", fmt.Errorf("%s is not a task folder: give the folder `codeflower task` printed, PROJECT/tasks/ID", path)
	}
	return directory, filepath.Dir(filepath.Dir(directory)), filepath.Base(directory), nil
}

// NextAttempt is the number of a task's next attempt: one more than the last.
func NextAttempt(directory string) (int, error) {
	entries, err := os.ReadDir(filepath.Join(directory, "attempts"))
	if errors.Is(err, os.ErrNotExist) {
		return 1, nil
	}
	if err != nil {
		return 0, err
	}
	last := 0
	for _, entry := range entries {
		if n, err := strconv.Atoi(entry.Name()); err == nil && entry.IsDir() && n > last {
			last = n
		}
	}
	return last + 1, nil
}

// WriteAttempt creates the attempt's folder and its attempt.json, privately;
// an attempt that is there already is an error.
func WriteAttempt(directory string, attempt TaskAttempt) error {
	folder := AttemptDirectory(directory, attempt.Attempt)
	if err := privateDirectory(filepath.Join(directory, "attempts")); err != nil {
		return err
	}
	if err := os.Mkdir(folder, 0o700); err != nil {
		return fmt.Errorf("could not start attempt %d of the task: %w", attempt.Attempt, err)
	}
	data, err := json.MarshalIndent(attempt, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(folder, "attempt.json"), append(data, '\n'), 0o600)
}

// ReadAttemptResult reads what the sandbox wrote when attempt n ended.
func ReadAttemptResult(directory string, n int) (TaskResult, error) {
	var result TaskResult
	data, err := os.ReadFile(filepath.Join(AttemptDirectory(directory, n), "result.json"))
	if err != nil {
		return result, err
	}
	return result, json.Unmarshal(data, &result)
}

// LockTask keeps other runs of the task from starting until unlock is called.
func LockTask(directory string) (unlock func(), err error) {
	return tryLockFile(filepath.Join(directory, "lock"))
}
