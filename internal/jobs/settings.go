package jobs

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"codeflower-app/internal/config"
)

// Settings are the config's jobs: section, ready to use.
type Settings struct {
	Scheduler string
	Profile   *Profile
	Policy    Policy
	// Engine runs the jobs' sandboxes; empty: the sandbox's own.
	Engine string
	// Codeflower is the app the batch scripts run.
	Codeflower  string
	Setup       string
	Account     string
	Extra       []string
	Notes       string
	EnvWritable bool
	EngineArgs  string
	OnStop      string
	Poll        time.Duration
}

// Policy is what jobs may ask for, and what they get when they do not.
type Policy struct {
	// Approve is ask or never.
	Approve  string
	Defaults Resources
	// Limits: zero values leave it to the scheduler. Active and Array always
	// have one.
	Limits Limits
	Queues []Queue
}

// Resources are a job's request, or the defaults.
type Resources struct {
	CPUs   int           `json:"cpus,omitempty"`
	Memory int64         `json:"memory,omitempty"`
	Time   time.Duration `json:"time,omitempty"`
	GPUs   int           `json:"gpus,omitempty"`
	Queue  string        `json:"queue,omitempty"`
}

// Limits bound a job, and a project's jobs.
type Limits struct {
	CPUs   int
	Memory int64
	Time   time.Duration
	GPUs   int
	Active int
	Array  int
}

// Queue is one of the queues jobs may use.
type Queue struct {
	Name        string
	CPUs        int
	Memory      int64
	Time        time.Duration
	GPUs        int
	GPUTypes    []string
	Description string
}

// Default limits on how many jobs a project has and how big an array is.
const (
	DefaultActive = 20
	DefaultArray  = 100
	defaultPoll   = time.Minute
	minimumPoll   = 15 * time.Second
)

// lookPath finds commands for scheduler: auto; tests replace it.
var lookPath = exec.LookPath

// Setup reads the config's jobs: section. override, when set, is --jobs.
// It returns nil when jobs are off.
func Setup(cfg *config.Config, override string) (*Settings, error) {
	section := config.Jobs{}
	if cfg != nil {
		section = cfg.Jobs
	}
	name := section.Scheduler
	if override != "" {
		name = override
	}
	switch name {
	case "", "off":
		return nil, nil
	case "auto":
		detected, err := detect(cfg)
		if err != nil || detected == "" {
			return nil, err
		}
		name = detected
	}
	if runtime.GOOS == "windows" {
		return nil, fmt.Errorf("batch jobs need Linux or macOS")
	}
	profile, err := LoadProfile(cfg, name)
	if err != nil {
		return nil, err
	}
	settings := &Settings{Scheduler: name, Profile: profile, Engine: section.Engine, Setup: strings.TrimRight(section.Setup, "\n"),
		Account: section.Account, Extra: section.Extra, Notes: strings.TrimSpace(section.Notes), EnvWritable: section.EnvWritable,
		EngineArgs: section.EngineArgs, OnStop: section.OnStop, Poll: defaultPoll}
	if settings.OnStop == "" {
		settings.OnStop = "keep"
	}
	if settings.Account != "" && !profile.Supported("account") {
		return nil, fmt.Errorf("jobs.account: the %s scheduler profile cannot set an account", name)
	}
	if !profile.Supported("queue") && (len(section.Queues) > 0 || section.Defaults.Queue != "") {
		return nil, fmt.Errorf("jobs.queues: the %s scheduler profile has no queues", name)
	}
	if section.Poll != "" {
		poll, err := ParseDuration(section.Poll)
		if err != nil {
			return nil, fmt.Errorf("jobs.poll: %w", err)
		}
		settings.Poll = max(poll, minimumPoll)
	}
	if settings.Codeflower = section.Codeflower; settings.Codeflower == "" {
		executable, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("jobs: cannot find this app's path for the batch scripts; set jobs.codeflower: %w", err)
		}
		if resolved, err := filepath.EvalSymlinks(executable); err == nil {
			executable = resolved
		}
		settings.Codeflower = executable
	} else {
		settings.Codeflower = expandPath(settings.Codeflower)
	}
	if settings.Policy, err = policy(section); err != nil {
		return nil, err
	}
	return settings, nil
}

// detect finds the scheduler whose commands are on PATH: the config's
// profiles first, then the built-in ones; "" when there is none.
func detect(cfg *config.Config) (string, error) {
	var names []string
	if cfg != nil {
		for name := range cfg.Jobs.Schedulers {
			names = append(names, name)
		}
		slices.Sort(names)
	}
	for _, name := range Builtin {
		if name != "local" && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	for _, name := range names {
		raw, err := rawProfile(cfg, name, 0)
		if err != nil {
			return "", err
		}
		if len(raw.Detect) == 0 {
			continue
		}
		found := true
		for _, command := range raw.Detect {
			if _, err := lookPath(command); err != nil {
				found = false
				break
			}
		}
		if found {
			return name, nil
		}
	}
	return "", nil
}

func policy(section config.Jobs) (Policy, error) {
	result := Policy{Approve: section.Approve}
	if result.Approve == "" {
		result.Approve = "ask"
	}
	var err error
	defaults := section.Defaults
	result.Defaults = Resources{CPUs: 1, Memory: 4 << 30, Time: time.Hour, Queue: defaults.Queue}
	if defaults.CPUs < 0 {
		return result, fmt.Errorf("jobs.defaults.cpus must be positive")
	}
	if defaults.CPUs > 0 {
		result.Defaults.CPUs = defaults.CPUs
	}
	if defaults.Memory != "" {
		if result.Defaults.Memory, err = ParseMemory(defaults.Memory); err != nil {
			return result, fmt.Errorf("jobs.defaults.memory: %w", err)
		}
	}
	if defaults.Time != "" {
		if result.Defaults.Time, err = ParseDuration(defaults.Time); err != nil {
			return result, fmt.Errorf("jobs.defaults.time: %w", err)
		}
	}
	limits := section.Limits
	if limits.CPUs < 0 || limits.GPUs < 0 || limits.Active < 0 || limits.Array < 0 {
		return result, fmt.Errorf("jobs.limits: numbers must be positive")
	}
	result.Limits = Limits{CPUs: limits.CPUs, GPUs: limits.GPUs, Active: limits.Active, Array: limits.Array}
	if result.Limits.Active == 0 {
		result.Limits.Active = DefaultActive
	}
	if result.Limits.Array == 0 {
		result.Limits.Array = DefaultArray
	}
	if limits.Memory != "" {
		if result.Limits.Memory, err = ParseMemory(limits.Memory); err != nil {
			return result, fmt.Errorf("jobs.limits.memory: %w", err)
		}
	}
	if limits.Time != "" {
		if result.Limits.Time, err = ParseDuration(limits.Time); err != nil {
			return result, fmt.Errorf("jobs.limits.time: %w", err)
		}
	}
	for _, raw := range section.Queues {
		if !namePattern.MatchString(raw.Name) {
			return result, fmt.Errorf("jobs.queues: %q is not a valid queue name", raw.Name)
		}
		queue := Queue{Name: raw.Name, CPUs: raw.CPUs, GPUs: raw.GPUs, GPUTypes: raw.GPUTypes, Description: strings.TrimSpace(raw.Description)}
		if raw.Memory != "" {
			if queue.Memory, err = ParseMemory(raw.Memory); err != nil {
				return result, fmt.Errorf("jobs.queues %s: memory: %w", raw.Name, err)
			}
		}
		if raw.Time != "" {
			if queue.Time, err = ParseDuration(raw.Time); err != nil {
				return result, fmt.Errorf("jobs.queues %s: time: %w", raw.Name, err)
			}
		}
		for _, kind := range raw.GPUTypes {
			if !namePattern.MatchString(kind) {
				return result, fmt.Errorf("jobs.queues %s: %q is not a valid GPU type", raw.Name, kind)
			}
		}
		result.Queues = append(result.Queues, queue)
	}
	if result.Defaults.Queue != "" {
		if !namePattern.MatchString(result.Defaults.Queue) {
			return result, fmt.Errorf("jobs.defaults.queue: %q is not a valid queue name", result.Defaults.Queue)
		}
		if len(result.Queues) > 0 && result.queue(result.Defaults.Queue) == nil {
			return result, fmt.Errorf("jobs.defaults.queue: %s is not under jobs.queues", result.Defaults.Queue)
		}
	}
	return result, nil
}

func (p Policy) queue(name string) *Queue {
	for index := range p.Queues {
		if p.Queues[index].Name == name {
			return &p.Queues[index]
		}
	}
	return nil
}

// Info is what the sandbox's agent is told about jobs (CODEFLOWER_JOBS):
// codeflower-doctor, codeflower-job info and the bridge read it.
type Info struct {
	Scheduler   string      `json:"scheduler"`
	Example     bool        `json:"example,omitempty"`
	Approve     string      `json:"approve"`
	Supports    []string    `json:"supports"`
	Defaults    InfoSize    `json:"defaults"`
	Limits      InfoSize    `json:"limits"`
	Active      int         `json:"active"`
	Array       int         `json:"array"`
	Queues      []InfoQueue `json:"queues,omitempty"`
	Notes       string      `json:"notes,omitempty"`
	OnStop      string      `json:"onStop"`
	EnvWritable bool        `json:"envWritable,omitempty"`
}

// InfoSize is resources as people read them.
type InfoSize struct {
	CPUs   int    `json:"cpus,omitempty"`
	Memory string `json:"memory,omitempty"`
	Time   string `json:"time,omitempty"`
	GPUs   int    `json:"gpus,omitempty"`
	Queue  string `json:"queue,omitempty"`
}

// InfoQueue is a queue as the agent is told it.
type InfoQueue struct {
	Name        string   `json:"name"`
	CPUs        int      `json:"cpus,omitempty"`
	Memory      string   `json:"memory,omitempty"`
	Time        string   `json:"time,omitempty"`
	GPUs        int      `json:"gpus,omitempty"`
	GPUTypes    []string `json:"gpuTypes,omitempty"`
	Description string   `json:"description,omitempty"`
}

// Info describes the settings for the agent.
func (s *Settings) Info() Info {
	p := s.Policy
	info := Info{Scheduler: s.Scheduler, Example: s.Profile.Example, Approve: p.Approve, Supports: s.Profile.Supports,
		Defaults: InfoSize{CPUs: p.Defaults.CPUs, Memory: FormatMemory(p.Defaults.Memory), Time: FormatDuration(p.Defaults.Time), Queue: p.Defaults.Queue},
		Limits:   InfoSize{CPUs: p.Limits.CPUs, Memory: FormatMemory(p.Limits.Memory), Time: FormatDuration(p.Limits.Time), GPUs: p.Limits.GPUs},
		Active:   p.Limits.Active, Array: p.Limits.Array, Notes: s.Notes, OnStop: s.OnStop, EnvWritable: s.EnvWritable}
	if info.Supports == nil {
		info.Supports = []string{}
	}
	for _, queue := range p.Queues {
		info.Queues = append(info.Queues, InfoQueue{Name: queue.Name, CPUs: queue.CPUs, Memory: FormatMemory(queue.Memory),
			Time: FormatDuration(queue.Time), GPUs: queue.GPUs, GPUTypes: queue.GPUTypes, Description: queue.Description})
	}
	return info
}

// InfoVariable carries Info to the sandbox; SpoolVariable is where its
// bridge (or, without one, codeflower-job) writes requests for the app.
const (
	InfoVariable  = "CODEFLOWER_JOBS"
	SpoolVariable = "CODEFLOWER_JOBS_SPOOL"
)

// InfoJSON is Info for InfoVariable.
func (s *Settings) InfoJSON() string {
	data, _ := json.Marshal(s.Info())
	return string(data)
}

// Summary is one line for the app's output when a sandbox starts.
func (s *Settings) Summary() string {
	parts := []string{s.Scheduler}
	if s.Profile.Example {
		parts[0] += " (example profile, untested)"
	}
	if s.Policy.Approve == "ask" {
		parts = append(parts, "asked in the chat")
	} else {
		parts = append(parts, "not asked")
	}
	parts = append(parts, fmt.Sprintf("at most %d at once", s.Policy.Limits.Active))
	if s.Engine != "" {
		parts = append(parts, "run with "+s.Engine)
	}
	return strings.Join(parts, ", ")
}

// ParseMemory reads a size such as 16G, 512M or 1.5T (binary units; a plain
// number is in MiB, as schedulers take it).
func ParseMemory(given string) (int64, error) {
	text := strings.TrimSpace(strings.ToUpper(given))
	text = strings.TrimSuffix(strings.TrimSuffix(text, "B"), "I")
	multiplier := float64(1 << 20)
	if text != "" {
		switch text[len(text)-1] {
		case 'K':
			multiplier = 1 << 10
		case 'M':
			multiplier = 1 << 20
		case 'G':
			multiplier = 1 << 30
		case 'T':
			multiplier = 1 << 40
		}
		if text[len(text)-1] >= 'A' {
			text = text[:len(text)-1]
		}
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil || value <= 0 || value > 1<<20 {
		return 0, fmt.Errorf("%q is not a size such as 16G or 512M", given)
	}
	return int64(value * multiplier), nil
}

// FormatMemory writes a size as G or M; empty for zero.
func FormatMemory(bytes int64) string {
	switch {
	case bytes == 0:
		return ""
	case bytes%(1<<30) == 0:
		return strconv.FormatInt(bytes>>30, 10) + "G"
	default:
		return strconv.FormatInt(ceilDiv(bytes, 1<<20), 10) + "M"
	}
}

var clockDays = regexp.MustCompile(`^[0-9]+-[0-9]+$`)

// ParseDuration reads 90m, 2h, 1h30m, 3d, D-HH:MM:SS, HH:MM:SS, MM:SS, or a
// number of minutes, as Slurm does.
func ParseDuration(given string) (time.Duration, error) {
	text := strings.TrimSpace(given)
	invalid := fmt.Errorf("%q is not a time such as 2h, 90m, 3d or 1-12:00:00", given)
	if text == "" {
		return 0, invalid
	}
	var result time.Duration
	if days, ok := strings.CutSuffix(text, "d"); ok {
		number, err := strconv.Atoi(days)
		if err != nil || number <= 0 {
			return 0, invalid
		}
		result = time.Duration(number) * 24 * time.Hour
	} else if minutes, err := strconv.Atoi(text); err == nil {
		result = time.Duration(minutes) * time.Minute
	} else if strings.Contains(text, ":") || clockDays.MatchString(text) {
		days := 0
		if before, after, ok := strings.Cut(text, "-"); ok {
			if days, err = strconv.Atoi(before); err != nil || days < 0 {
				return 0, invalid
			}
			text = after
		}
		fields := strings.Split(text, ":")
		if len(fields) > 3 {
			return 0, invalid
		}
		var seconds int
		for _, field := range fields {
			number, err := strconv.Atoi(field)
			if err != nil || number < 0 {
				return 0, invalid
			}
			seconds = seconds*60 + number
		}
		if days > 0 && len(fields) == 2 {
			// D-HH:MM
			seconds *= 60
		} else if days > 0 && len(fields) == 1 {
			// D-HH
			seconds *= 3600
		}
		result = time.Duration(days)*24*time.Hour + time.Duration(seconds)*time.Second
	} else {
		parsed, err := time.ParseDuration(text)
		if err != nil {
			return 0, invalid
		}
		result = parsed
	}
	if result <= 0 || result > 365*24*time.Hour {
		return 0, invalid
	}
	return result, nil
}

// FormatDuration writes a time limit briefly: 1h30m, 3d, 45m; empty for zero.
func FormatDuration(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	if d%(24*time.Hour) == 0 {
		return strconv.Itoa(int(d/(24*time.Hour))) + "d"
	}
	text := d.Round(time.Second).String()
	if strings.HasSuffix(text, "m0s") {
		text = strings.TrimSuffix(text, "0s")
	}
	if strings.HasSuffix(text, "h0m") {
		text = strings.TrimSuffix(text, "0m")
	}
	return text
}

func expandPath(value string) string {
	value = os.ExpandEnv(value)
	if value == "~" || strings.HasPrefix(value, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return home + value[1:]
		}
	}
	return value
}
