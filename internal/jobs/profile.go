// Package jobs lets a sandbox's agent have batch jobs submitted by the app,
// each of which runs in a sandbox of its own (docs/jobs.md). A scheduler
// profile says how to write a batch script and run the scheduler's commands;
// the broker takes the sandbox's requests, checks them against the config's
// limits, submits them and follows their states.
package jobs

import (
	"embed"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"text/template"

	"codeflower-app/internal/config"
)

//go:embed profiles/*.yaml
var builtinProfiles embed.FS

// Builtin are the profiles the app has; Examples among them are untested on a
// real cluster, which the app says when one is used.
var (
	Builtin  = []string{"slurm", "local", "pbs", "lsf", "sge", "htcondor"}
	Examples = []string{"pbs", "lsf", "sge", "htcondor"}
)

// States a job can be in. Queued and running jobs are active; the others are final.
const (
	StateSubmitting = "submitting"
	StateQueued     = "queued"
	StateRunning    = "running"
	StateDone       = "done"
	StateFailed     = "failed"
	StateCancelled  = "cancelled"
	// StateLost: the scheduler no longer knows the job, and it left no result
	// (killed before its sandbox ended, e.g. by its time limit).
	StateLost = "lost"
)

var stateCategories = []string{StateQueued, StateRunning, StateDone, StateFailed, StateCancelled}

// Active says whether a state is not final.
func Active(state string) bool {
	return state == StateSubmitting || state == StateQueued || state == StateRunning
}

// Features a profile's script may support; a job that needs another is refused.
var Features = []string{"queue", "account", "gpus", "array", "after"}

// Profile is a compiled scheduler profile.
type Profile struct {
	Name string
	// Local runs the script on this computer instead of submitting it.
	Local         bool
	Example       bool
	Detect        []string
	ArrayIndexEnv string
	PassEnv       []string
	Supports      []string

	script     *template.Template
	submitFile *template.Template
	submit     command
	status     command
	history    command
	cancel     command
	states     []stateRule
}

type command struct {
	args  []*template.Template
	stdin bool
	id    *regexp.Regexp
	line  *regexp.Regexp
}

func (c command) set() bool { return len(c.args) > 0 }

type stateRule struct {
	category, pattern string
}

// Supported says whether the profile's script can express a feature.
func (p *Profile) Supported(feature string) bool {
	return slices.Contains(p.Supports, feature)
}

// HasHistory says whether finished jobs can be looked up.
func (p *Profile) HasHistory() bool { return p.history.set() }

// Category sorts a scheduler's state into StateQueued, StateRunning,
// StateDone, StateFailed or StateCancelled: an exact state first, then the
// longest pattern that matches; anything unknown is failed.
func (p *Profile) Category(state string) string {
	for _, rule := range p.states {
		if prefix, ok := strings.CutSuffix(rule.pattern, "*"); ok {
			if strings.HasPrefix(state, prefix) {
				return rule.category
			}
		} else if rule.pattern == state {
			return rule.category
		}
	}
	return StateFailed
}

// LoadProfile finds a scheduler's profile: the config's own (jobs.schedulers
// or <config dir>/schedulers/NAME.yaml) before a built-in one of that name.
func LoadProfile(cfg *config.Config, name string) (*Profile, error) {
	raw, err := rawProfile(cfg, name, 0)
	if err != nil {
		return nil, err
	}
	profile, err := compile(name, raw)
	if err != nil {
		return nil, fmt.Errorf("scheduler profile %s: %w", name, err)
	}
	return profile, nil
}

func rawProfile(cfg *config.Config, name string, depth int) (config.SchedulerProfile, error) {
	if depth > 5 {
		return config.SchedulerProfile{}, fmt.Errorf("scheduler profile %s: extends: goes round in a circle", name)
	}
	own, found, err := cfg.SchedulerProfile(name)
	if err != nil {
		return own, err
	}
	if !found {
		return builtinProfile(name)
	}
	if own.Extends == "" {
		return own, nil
	}
	var base config.SchedulerProfile
	if own.Extends == name {
		base, err = builtinProfile(name)
	} else {
		base, err = rawProfile(cfg, own.Extends, depth+1)
	}
	if err != nil {
		return own, fmt.Errorf("scheduler profile %s: extends %s: %w", name, own.Extends, err)
	}
	return merge(base, own), nil
}

func builtinProfile(name string) (config.SchedulerProfile, error) {
	if !slices.Contains(Builtin, name) {
		return config.SchedulerProfile{}, fmt.Errorf("unknown scheduler %q: use %s, or describe it under jobs.schedulers (docs: batch jobs)",
			name, strings.Join(Builtin, ", "))
	}
	data, err := builtinProfiles.ReadFile("profiles/" + name + ".yaml")
	if err != nil {
		return config.SchedulerProfile{}, err
	}
	return config.ParseSchedulerProfile(data)
}

// merge lays a profile's fields over those of the one it extends.
func merge(base, own config.SchedulerProfile) config.SchedulerProfile {
	if own.Detect != nil {
		base.Detect = own.Detect
	}
	if own.Script != "" {
		base.Script = own.Script
	}
	if own.SubmitFile != "" {
		base.SubmitFile = own.SubmitFile
	}
	for _, pair := range []struct{ base, own *config.ProfileCommand }{
		{&base.Submit, &own.Submit}, {&base.Status, &own.Status}, {&base.History, &own.History}, {&base.Cancel, &own.Cancel},
	} {
		if len(pair.own.Command) > 0 {
			*pair.base = *pair.own
		}
	}
	if own.States != nil {
		base.States = own.States
	}
	if own.ArrayIndexEnv != "" {
		base.ArrayIndexEnv = own.ArrayIndexEnv
	}
	if own.PassEnv != nil {
		base.PassEnv = own.PassEnv
	}
	if own.Supports != nil {
		base.Supports = own.Supports
	}
	return base
}

var variableName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func compile(name string, raw config.SchedulerProfile) (*Profile, error) {
	profile := &Profile{Name: name, Local: name == "local" && raw.Submit.Command == nil, Example: slices.Contains(Examples, name),
		Detect: raw.Detect, ArrayIndexEnv: raw.ArrayIndexEnv, PassEnv: raw.PassEnv, Supports: raw.Supports}
	if raw.Script == "" {
		return nil, fmt.Errorf("script: is missing")
	}
	var err error
	if profile.script, err = parseTemplate("script", raw.Script); err != nil {
		return nil, err
	}
	if raw.SubmitFile != "" {
		if profile.submitFile, err = parseTemplate("submit_file", raw.SubmitFile); err != nil {
			return nil, err
		}
	}
	for _, part := range []struct {
		name     string
		raw      config.ProfileCommand
		target   *command
		required bool
		regex    string
		groups   []string
	}{
		{"submit", raw.Submit, &profile.submit, true, raw.Submit.ID, nil},
		{"status", raw.Status, &profile.status, true, raw.Status.Line, []string{"id", "state"}},
		{"history", raw.History, &profile.history, false, raw.History.Line, []string{"id", "state"}},
		{"cancel", raw.Cancel, &profile.cancel, true, "", nil},
	} {
		if profile.Local {
			break
		}
		if len(part.raw.Command) == 0 {
			if part.required {
				return nil, fmt.Errorf("%s: needs a command", part.name)
			}
			continue
		}
		for index, arg := range part.raw.Command {
			parsed, err := parseTemplate(fmt.Sprintf("%s.command[%d]", part.name, index), arg)
			if err != nil {
				return nil, err
			}
			part.target.args = append(part.target.args, parsed)
		}
		part.target.stdin = part.raw.Stdin
		if part.name == "submit" && part.raw.ID != "" {
			if part.target.id, err = regexp.Compile(part.raw.ID); err != nil {
				return nil, fmt.Errorf("submit.id: %w", err)
			}
			if part.target.id.NumSubexp() < 1 {
				return nil, fmt.Errorf("submit.id needs a group, e.g. '^(\\d+)'")
			}
		}
		if part.groups != nil {
			if part.regex == "" {
				return nil, fmt.Errorf("%s.line is missing", part.name)
			}
			if part.target.line, err = regexp.Compile(part.regex); err != nil {
				return nil, fmt.Errorf("%s.line: %w", part.name, err)
			}
			for _, group := range part.groups {
				if part.target.line.SubexpIndex(group) < 0 {
					return nil, fmt.Errorf("%s.line needs a group named %s, as (?P<%s>…)", part.name, group, group)
				}
			}
		}
	}
	if len(raw.States) == 0 {
		return nil, fmt.Errorf("states: is missing")
	}
	for _, category := range stateCategories {
		for _, pattern := range raw.States[category] {
			profile.states = append(profile.states, stateRule{category, pattern})
		}
	}
	// Exact states before patterns, and longer patterns before shorter ones.
	slices.SortStableFunc(profile.states, func(a, b stateRule) int {
		return patternRank(b.pattern) - patternRank(a.pattern)
	})
	for category := range raw.States {
		if !slices.Contains(stateCategories, category) {
			return nil, fmt.Errorf("states: unknown category %q: use %s", category, strings.Join(stateCategories, ", "))
		}
	}
	for _, feature := range raw.Supports {
		if !slices.Contains(Features, feature) {
			return nil, fmt.Errorf("supports: unknown %q: use %s", feature, strings.Join(Features, ", "))
		}
	}
	for _, variable := range append(slices.Clone(raw.PassEnv), raw.ArrayIndexEnv) {
		if variable != "" && !variableName.MatchString(variable) {
			return nil, fmt.Errorf("%q is not a variable name", variable)
		}
	}
	return profile, nil
}

func parseTemplate(name, text string) (*template.Template, error) {
	parsed, err := template.New(name).Funcs(templateFuncs).Option("missingkey=error").Parse(text)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return parsed, nil
}

func patternRank(pattern string) int {
	if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
		return len(prefix)
	}
	return 1 << 20
}

// Programs are the commands the profile runs, for `codeflower check`.
func (p *Profile) Programs() []string {
	var programs []string
	for _, c := range []command{p.submit, p.status, p.history, p.cancel} {
		if !c.set() {
			continue
		}
		if program, err := render(c.args[0], ScriptData{}); err == nil && program != "" && !slices.Contains(programs, program) {
			programs = append(programs, program)
		}
	}
	return programs
}
