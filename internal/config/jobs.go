package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Jobs configures the batch jobs a sandbox's agent may ask the app to submit
// (codeflower-job in the sandbox). Each job runs in a sandbox of its own, with
// the same image, folders and network as the sandbox that asked
// (docs/jobs.md). Jobs are off unless scheduler names one.
type Jobs struct {
	// Scheduler: off (the default), auto (the one whose commands are found
	// here), slurm, local (background jobs on this computer), or the name of a
	// profile under schedulers: or in <config dir>/schedulers/NAME.yaml.
	Scheduler string `yaml:"scheduler"`
	// Approve: ask (the default) shows each submission in the chat for the
	// user to allow or deny; never submits what is within the limits without
	// asking, as unattended tasks need.
	Approve string `yaml:"approve"`
	// Engine runs the jobs' sandboxes: docker, podman or apptainer (default:
	// the sandbox's own). Apptainer starts an image file on a shared folder
	// fastest on clusters whose Podman keeps images per job.
	Engine string `yaml:"engine"`
	// Codeflower is the app on the nodes jobs run on (default: this app's
	// path, which must be on a folder they share).
	Codeflower string `yaml:"codeflower"`
	// Setup: shell lines the batch script runs before the job's sandbox
	// starts, e.g. `module load apptainer`.
	Setup string `yaml:"setup"`
	// Defaults are what a job gets that does not ask for it.
	Defaults JobDefaults `yaml:"defaults"`
	// Limits are the most a job, and all of a project's jobs, may ask for.
	Limits JobLimits `yaml:"limits"`
	// Queues, when set, are the only queues (Slurm partitions) jobs may use,
	// each with its own limits; they are listed to the agent.
	Queues []JobQueue `yaml:"queues"`
	// Account is charged for the jobs (Slurm's --account), when set.
	Account string `yaml:"account"`
	// Extra are scheduler options added to every job, as the profile's
	// script writes them (Slurm: e.g. --export=NONE, --qos=normal).
	Extra []string `yaml:"extra"`
	// Notes are told to the agent with the queues and limits: what to know
	// about this cluster when submitting.
	Notes string `yaml:"notes"`
	// EnvWritable lets jobs change the project's software environments; by
	// default they mount them read-only, so jobs running at once cannot break
	// them.
	EnvWritable bool `yaml:"env_writable"`
	// EngineArgs are container engine flags for the jobs' sandboxes, as
	// --engine-args takes them, e.g. '--gpus all'.
	EngineArgs string `yaml:"engine_args"`
	// OnStop: keep (the default) leaves a project's jobs running when its
	// sandbox stops, and the next sandbox of the project follows them again;
	// cancel cancels them.
	OnStop string `yaml:"on_stop"`
	// Poll is how often the jobs' states are asked for (default 1m, at least 15s).
	Poll string `yaml:"poll"`
	// Schedulers are profiles of other schedulers, by name: how to write
	// their batch scripts and run their commands (docs/jobs.md).
	Schedulers map[string]SchedulerProfile `yaml:"schedulers"`
}

// JobDefaults are what a job gets that does not ask for it.
type JobDefaults struct {
	CPUs int `yaml:"cpus"`
	// Memory, e.g. 4G.
	Memory string `yaml:"memory"`
	// Time, e.g. 1h or 1-00:00:00.
	Time  string `yaml:"time"`
	Queue string `yaml:"queue"`
}

// JobLimits are the most a job may ask for; Active and Array bound how many
// jobs a project has queued or running and how big an array may be.
type JobLimits struct {
	CPUs   int    `yaml:"cpus"`
	Memory string `yaml:"memory"`
	Time   string `yaml:"time"`
	GPUs   int    `yaml:"gpus"`
	// Active: jobs of a project queued or running at once (default 20).
	Active int `yaml:"active"`
	// Array: tasks of one job array (default 100).
	Array int `yaml:"array"`
}

// JobQueue is a queue jobs may use, with its own limits within Limits.
type JobQueue struct {
	Name   string `yaml:"name"`
	CPUs   int    `yaml:"cpus"`
	Memory string `yaml:"memory"`
	Time   string `yaml:"time"`
	// GPUs a job of the queue may ask for; 0: none.
	GPUs int `yaml:"gpus"`
	// GPUTypes the queue offers, if jobs may name one.
	GPUTypes    []string `yaml:"gpu_types"`
	Description string   `yaml:"description"`
}

// SchedulerProfile tells the app how to use a batch scheduler: a template of
// the batch script, and the commands that submit, list, look up and cancel
// jobs. Each command is a list of arguments, each a template of its own; none
// goes through a shell.
type SchedulerProfile struct {
	// Extends names a profile whose fields this one starts from, e.g. slurm.
	Extends string `yaml:"extends"`
	// Detect are commands whose presence on PATH means this scheduler, for
	// scheduler: auto.
	Detect []string `yaml:"detect"`
	// Script is the batch script, a Go text/template (docs/jobs.md lists its
	// fields and functions).
	Script string `yaml:"script"`
	// SubmitFile, when set, is a second template written next to the script
	// (as .SubmitFile), for schedulers that take a description of the job
	// (HTCondor) rather than a script.
	SubmitFile string         `yaml:"submit_file"`
	Submit     ProfileCommand `yaml:"submit"`
	Status     ProfileCommand `yaml:"status"`
	// History, optional, looks up finished jobs (Slurm's sacct).
	History ProfileCommand `yaml:"history"`
	Cancel  ProfileCommand `yaml:"cancel"`
	// States sort the scheduler's states into queued, running, done, failed
	// and cancelled; a trailing * matches any rest, and "*" alone anything.
	States map[string][]string `yaml:"states"`
	// ArrayIndexEnv is the variable that holds an array task's index in the job.
	ArrayIndexEnv string `yaml:"array_index_env"`
	// PassEnv are the scheduler's variables the job's sandbox gets.
	PassEnv []string `yaml:"pass_env"`
	// Supports are what the script can express of array, after, gpus, account
	// and queue; a job that asks for anything else is refused.
	Supports []string `yaml:"supports"`
}

// ProfileCommand is one of a profile's commands.
type ProfileCommand struct {
	// Command is the program and its arguments, each a template.
	Command []string `yaml:"command"`
	// Stdin feeds the batch script to the command (LSF's bsub < script).
	Stdin bool `yaml:"stdin"`
	// ID, for submit, is a regular expression whose first group is the job's id
	// in the command's output (default: the whole first line).
	ID string `yaml:"id"`
	// Line, for status and history, is a regular expression matched against
	// each line of the output, with the named groups id and state (and index
	// and exit for history).
	Line string `yaml:"line"`
}

// JobSchedulerNames are the values scheduler: takes besides a profile's name.
var JobSchedulerNames = []string{"off", "auto", "slurm", "local"}

var profileName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// checkJobs checks what can be checked without the profiles' templates,
// which the jobs package compiles.
func (c *Config) checkJobs() error {
	jobs := c.Jobs
	switch jobs.Approve {
	case "", "ask", "never":
	default:
		return fmt.Errorf("%s: jobs.approve must be ask or never", c.Path)
	}
	switch jobs.OnStop {
	case "", "keep", "cancel":
	default:
		return fmt.Errorf("%s: jobs.on_stop must be keep or cancel", c.Path)
	}
	switch jobs.Engine {
	case "", "docker", "podman", "apptainer":
	default:
		return fmt.Errorf("%s: jobs.engine must be docker, podman or apptainer", c.Path)
	}
	for name := range jobs.Schedulers {
		if !profileName.MatchString(name) {
			return fmt.Errorf("%s: jobs.schedulers: %q is not a valid name (lowercase letters, digits, - and _)", c.Path, name)
		}
		for _, reserved := range JobSchedulerNames {
			if name == reserved && name != "slurm" {
				return fmt.Errorf("%s: jobs.schedulers: %q is reserved", c.Path, name)
			}
		}
	}
	seen := map[string]bool{}
	for _, queue := range jobs.Queues {
		if queue.Name == "" {
			return fmt.Errorf("%s: jobs.queues: every queue needs a name", c.Path)
		}
		if seen[queue.Name] {
			return fmt.Errorf("%s: jobs.queues: %q is listed twice", c.Path, queue.Name)
		}
		seen[queue.Name] = true
	}
	for _, line := range jobs.Extra {
		if strings.ContainsAny(line, "\n\r") {
			return fmt.Errorf("%s: jobs.extra: options must be on one line", c.Path)
		}
	}
	return nil
}

// SchedulersFolder holds profiles as NAME.yaml, besides jobs.schedulers.
const SchedulersFolder = "schedulers"

// SchedulerProfile returns the profile of a scheduler named in the config or
// in the schedulers folder; found is false when neither has it.
func (c *Config) SchedulerProfile(name string) (profile SchedulerProfile, found bool, err error) {
	if c == nil {
		return profile, false, nil
	}
	if profile, ok := c.Jobs.Schedulers[name]; ok {
		return profile, true, nil
	}
	if c.Directory == "" || !profileName.MatchString(name) {
		return profile, false, nil
	}
	path := filepath.Join(c.Directory, SchedulersFolder, name+".yaml")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return profile, false, nil
	}
	if err != nil {
		return profile, false, err
	}
	profile, err = ParseSchedulerProfile(data)
	if err != nil {
		return profile, false, fmt.Errorf("%s: %w", path, err)
	}
	return profile, true, nil
}

// ParseSchedulerProfile reads a profile from YAML; unknown keys are errors.
func ParseSchedulerProfile(data []byte) (SchedulerProfile, error) {
	var profile SchedulerProfile
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&profile); err != nil {
		return profile, err
	}
	return profile, nil
}
