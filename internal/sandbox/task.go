package sandbox

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Task is what `codeflower task` asks the sandbox to do. The sandbox reads it
// from TaskFile and writes its TaskResult next to it.
type Task struct {
	Task  string `json:"task"`
	Agent string `json:"agent"`
	// Output is the sandbox folder for the results, e.g. /data/output/study42.
	Output    string `json:"output"`
	Progress  string `json:"progress,omitempty"`
	Report    bool   `json:"report"`
	MaxRounds int    `json:"maxRounds"`
	// Model and Effort are values of the agent's model and effort settings,
	// e.g. opus and high; Options sets other settings by their id.
	Model     string            `json:"model,omitempty"`
	Effort    string            `json:"effort,omitempty"`
	Options   map[string]string `json:"options,omitempty"`
	CreatedAt string            `json:"createdAt"`
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

// TaskDirectory is a task's folder in the prepared project.
func TaskDirectory(state, id string) string {
	return filepath.Join(state, "tasks", id)
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

// ReadTaskResult reads what the sandbox wrote when the task ended.
func ReadTaskResult(state, id string) (TaskResult, error) {
	var result TaskResult
	data, err := os.ReadFile(filepath.Join(TaskDirectory(state, id), "result.json"))
	if err != nil {
		return result, err
	}
	return result, json.Unmarshal(data, &result)
}
