package jobs

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Codes of a refused or failed request, which codeflower-job turns into its
// exit status.
const (
	CodeInvalid   = "invalid"
	CodeLimit     = "limit"
	CodeDenied    = "denied"
	CodeScheduler = "scheduler"
	CodeUnknown   = "unknown"
)

// RequestError is why a request was refused, for the agent to read.
type RequestError struct {
	Code    string
	Message string
}

func (e *RequestError) Error() string { return e.Message }

func invalid(format string, args ...any) error {
	return &RequestError{Code: CodeInvalid, Message: fmt.Sprintf(format, args...)}
}

func overLimit(format string, args ...any) error {
	return &RequestError{Code: CodeLimit, Message: fmt.Sprintf(format, args...)}
}

// Request is what the bridge puts in the spool for the app (docs/jobs.md).
// Everything in it comes from the sandbox and is checked before use.
type Request struct {
	ID     string `json:"id"`
	Action string `json:"action"`
	// Agent and SessionID are the chat that asked; Approved says who allowed
	// it (user, chat or policy).
	Agent     string `json:"agent,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
	Approved  string `json:"approved,omitempty"`

	Name    string            `json:"name,omitempty"`
	CPUs    int               `json:"cpus,omitempty"`
	Memory  string            `json:"memory,omitempty"`
	Time    string            `json:"time,omitempty"`
	GPUs    int               `json:"gpus,omitempty"`
	GPUType string            `json:"gpuType,omitempty"`
	Queue   string            `json:"queue,omitempty"`
	Array   string            `json:"array,omitempty"`
	After   []string          `json:"after,omitempty"`
	Workdir string            `json:"workdir,omitempty"`
	Command []string          `json:"command,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Notify  *bool             `json:"notify,omitempty"`

	// Job is the job to cancel: its codeflower or scheduler id.
	Job string `json:"job,omitempty"`
}

// Actions a request may ask for.
const (
	ActionSubmit = "submit"
	ActionCancel = "cancel"
)

// Job is a checked submission, with the defaults filled in.
type Job struct {
	Name      string
	Resources Resources
	GPUType   string
	Array     *Array
	// After are codeflower ids of this project's jobs.
	After   []string
	Workdir string
	Command []string
	Env     map[string]string
}

var (
	namePattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	programPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)
	envPattern     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	idPattern      = regexp.MustCompile(`^[A-Za-z0-9._\[\]-]{1,64}$`)
)

const (
	maxArgs      = 512
	maxArgBytes  = 128 << 10
	maxEnv       = 64
	maxEnvValue  = 16 << 10
	maxAfterJobs = 32
)

// Check turns a request into a job, or says why it is refused. roots are
// the folders of the sandbox the job's command and folder must be in; active
// is how many of the project's jobs are queued or running.
func (p Policy) Check(request Request, profile *Profile, roots []string, active int) (Job, error) {
	job := Job{Resources: p.Defaults, After: request.After}
	if active >= p.Limits.Active {
		return job, overLimit("the project has %d jobs queued or running, the most it may have (jobs.limits.active)", active)
	}
	if err := checkCommand(request.Command, roots); err != nil {
		return job, err
	}
	job.Command = request.Command
	workdir, err := underRoots("the job's folder (--workdir)", request.Workdir, roots)
	if err != nil {
		return job, err
	}
	job.Workdir = workdir
	job.Name = request.Name
	if job.Name == "" {
		job.Name = defaultName(request.Command[0])
	}
	if !namePattern.MatchString(job.Name) {
		return job, invalid("the name %q may have only letters, digits, '.', '_' and '-' (at most 64)", job.Name)
	}

	if request.Queue != "" {
		if !profile.Supported("queue") {
			return job, invalid("the %s scheduler has no queues", profile.Name)
		}
		if !namePattern.MatchString(request.Queue) {
			return job, invalid("%q is not a queue name", request.Queue)
		}
		job.Resources.Queue = request.Queue
	}
	var queue *Queue
	if job.Resources.Queue != "" && len(p.Queues) > 0 {
		if queue = p.queue(job.Resources.Queue); queue == nil {
			return job, invalid("the queue %s is not one jobs may use: %s", job.Resources.Queue, p.queueNames())
		}
	}

	if request.CPUs < 0 || request.GPUs < 0 {
		return job, invalid("CPUs and GPUs must be positive")
	}
	if request.CPUs > 0 {
		job.Resources.CPUs = request.CPUs
	}
	if request.Memory != "" {
		if job.Resources.Memory, err = ParseMemory(request.Memory); err != nil {
			return job, invalid("memory: %v", err)
		}
	}
	if request.Time != "" {
		if job.Resources.Time, err = ParseDuration(request.Time); err != nil {
			return job, invalid("time: %v", err)
		}
	}
	job.Resources.GPUs = request.GPUs
	if job.Resources.GPUs > 0 && !profile.Supported("gpus") {
		return job, invalid("the %s scheduler profile cannot ask for GPUs", profile.Name)
	}
	if request.GPUType != "" {
		if job.Resources.GPUs == 0 {
			return job, invalid("a GPU type needs a number of GPUs, e.g. --gpus 1:%s", request.GPUType)
		}
		if !namePattern.MatchString(request.GPUType) {
			return job, invalid("%q is not a GPU type", request.GPUType)
		}
		if queue != nil && len(queue.GPUTypes) > 0 && !slices.Contains(queue.GPUTypes, request.GPUType) {
			return job, invalid("the queue %s has GPUs of type %s, not %s", queue.Name, strings.Join(queue.GPUTypes, ", "), request.GPUType)
		}
		job.GPUType = request.GPUType
	}
	if err := p.checkLimits(job.Resources, queue); err != nil {
		return job, err
	}

	if request.Array != "" {
		if !profile.Supported("array") {
			return job, invalid("the %s scheduler profile cannot submit job arrays", profile.Name)
		}
		if job.Array, err = ParseArray(request.Array, p.Limits.Array); err != nil {
			if _, ok := err.(*RequestError); ok {
				return job, err
			}
			return job, invalid("%v", err)
		}
	}
	if len(request.After) > 0 {
		if !profile.Supported("after") {
			return job, invalid("the %s scheduler profile cannot make a job wait for others", profile.Name)
		}
		if len(request.After) > maxAfterJobs {
			return job, invalid("a job may wait for at most %d others", maxAfterJobs)
		}
		for _, id := range request.After {
			if !idPattern.MatchString(id) {
				return job, invalid("%q is not a job id", id)
			}
		}
	}

	if len(request.Env) > maxEnv {
		return job, invalid("a job may set at most %d variables", maxEnv)
	}
	for name, value := range request.Env {
		if !envPattern.MatchString(name) {
			return job, invalid("%q is not a variable name", name)
		}
		if strings.HasPrefix(name, "CODEFLOWER_") || slices.Contains(profile.PassEnv, name) || name == profile.ArrayIndexEnv {
			return job, invalid("the variable %s is set by codeflower or the scheduler", name)
		}
		if strings.ContainsRune(value, 0) || len(value) > maxEnvValue {
			return job, invalid("the value of %s is too long or not text", name)
		}
	}
	job.Env = request.Env
	return job, nil
}

func (p Policy) checkLimits(resources Resources, queue *Queue) error {
	type bound struct {
		what    string
		value   int64
		limit   int64
		section string
		format  func(int64) string
	}
	count := func(value int64) string { return fmt.Sprint(value) }
	memory := func(value int64) string { return FormatMemory(value) }
	duration := func(value int64) string { return FormatDuration(durationOf(value)) }
	bounds := []bound{
		{"CPUs", int64(resources.CPUs), int64(p.Limits.CPUs), "jobs.limits.cpus", count},
		{"memory", resources.Memory, p.Limits.Memory, "jobs.limits.memory", memory},
		{"time", int64(resources.Time), int64(p.Limits.Time), "jobs.limits.time", duration},
		{"GPUs", int64(resources.GPUs), int64(p.Limits.GPUs), "jobs.limits.gpus", count},
	}
	if queue != nil {
		name := "the queue " + queue.Name
		bounds = append(bounds,
			bound{"CPUs", int64(resources.CPUs), int64(queue.CPUs), name, count},
			bound{"memory", resources.Memory, queue.Memory, name, memory},
			bound{"time", int64(resources.Time), int64(queue.Time), name, duration})
		if resources.GPUs > queue.GPUs {
			if queue.GPUs == 0 {
				return overLimit("the queue %s has no GPUs: choose another queue", queue.Name)
			}
			return overLimit("%d GPUs is more than %s allows (%d)", resources.GPUs, name, queue.GPUs)
		}
	}
	for _, b := range bounds {
		if b.limit > 0 && b.value > b.limit {
			return overLimit("%s %s is more than %s allows (%s)", b.what, b.format(b.value), b.section, b.format(b.limit))
		}
	}
	if resources.CPUs < 1 || resources.Memory <= 0 || resources.Time <= 0 {
		return invalid("a job needs CPUs, memory and time")
	}
	return nil
}

func (p Policy) queueNames() string {
	names := make([]string, len(p.Queues))
	for index, queue := range p.Queues {
		names[index] = queue.Name
	}
	return strings.Join(names, ", ")
}

// checkCommand allows a program on the sandbox's PATH, or a path in one of
// its folders, and arguments without NUL.
func checkCommand(command []string, roots []string) error {
	if len(command) == 0 || command[0] == "" {
		return invalid("the job needs a command: codeflower-job submit [OPTIONS] -- COMMAND [ARGS]")
	}
	if len(command) > maxArgs {
		return invalid("the command has more than %d arguments", maxArgs)
	}
	size := 0
	for _, arg := range command {
		if strings.ContainsRune(arg, 0) {
			return invalid("the command's arguments must be text")
		}
		size += len(arg)
	}
	if size > maxArgBytes {
		return invalid("the command is longer than %d KB", maxArgBytes>>10)
	}
	program := command[0]
	if strings.HasPrefix(program, "/") {
		_, err := underRoots("the command", program, roots)
		return err
	}
	if !programPattern.MatchString(program) {
		return invalid("the command must be a program on PATH (e.g. python) or an absolute path in one of the sandbox's folders, not %q", program)
	}
	return nil
}

// underRoots cleans an absolute path and checks it is in one of the roots.
func underRoots(what, value string, roots []string) (string, error) {
	if !strings.HasPrefix(value, "/") || strings.ContainsRune(value, 0) {
		return "", invalid("%s must be an absolute path in the sandbox, not %q", what, value)
	}
	cleaned := path.Clean(value)
	for _, root := range roots {
		if cleaned == root || strings.HasPrefix(cleaned, root+"/") {
			return cleaned, nil
		}
	}
	return "", invalid("%s %s is not in a folder jobs see: %s", what, cleaned, strings.Join(roots, ", "))
}

// defaultName is a job's name from its command: the program's file name.
func defaultName(program string) string {
	name := path.Base(program)
	var builder strings.Builder
	for _, r := range name {
		if r < 128 && (r == '.' || r == '_' || r == '-' || ('0' <= r && r <= '9') || ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z')) {
			builder.WriteRune(r)
		}
	}
	result := strings.TrimLeft(builder.String(), "._-")
	if len(result) > 64 {
		result = result[:64]
	}
	if result == "" {
		return "job"
	}
	return result
}

func durationOf(value int64) time.Duration { return time.Duration(value) }
