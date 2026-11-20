package jobs

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"text/template"
	"time"
)

// argSeparator splits one templated argument into several (the args
// function), e.g. a list of job ids for a command that takes each as its own.
const argSeparator = "\x00"

var templateFuncs = template.FuncMap{
	// q quotes a value for a POSIX shell.
	"q": ShellQuote,
	// kb, mb and gb are a byte count in KiB, MiB and GiB, rounded up.
	"kb": func(bytes int64) int64 { return ceilDiv(bytes, 1<<10) },
	"mb": func(bytes int64) int64 { return ceilDiv(bytes, 1<<20) },
	"gb": func(bytes int64) int64 { return ceilDiv(bytes, 1<<30) },
	// dhms is a duration as D-HH:MM:SS (Slurm), hms as HH:MM:SS with as many
	// hours as needed (PBS, Grid Engine).
	"dhms": func(d time.Duration) string {
		seconds := int64(math.Ceil(d.Seconds()))
		return fmt.Sprintf("%d-%02d:%02d:%02d", seconds/86400, seconds%86400/3600, seconds%3600/60, seconds%60)
	},
	"hms": func(d time.Duration) string {
		seconds := int64(math.Ceil(d.Seconds()))
		return fmt.Sprintf("%02d:%02d:%02d", seconds/3600, seconds%3600/60, seconds%60)
	},
	"minutes": func(d time.Duration) int64 { return int64(math.Ceil(d.Minutes())) },
	"seconds": func(d time.Duration) int64 { return int64(math.Ceil(d.Seconds())) },
	"join":    func(values []string, separator string) string { return strings.Join(values, separator) },
	// args makes each value a command argument of its own.
	"args": func(values []string) string { return strings.Join(values, argSeparator) },
	"div": func(a int64, b int) int64 {
		if b <= 0 {
			return a
		}
		return ceilDiv(a, int64(b))
	},
	// fail stops with a message, for what a scheduler cannot express.
	"fail": func(message string) (string, error) { return "", errors.New(message) },
}

func ceilDiv(a, b int64) int64 {
	return (a + b - 1) / b
}

// ShellQuote quotes a value for a POSIX shell: as is when it is plainly safe,
// else in single quotes.
func ShellQuote(value string) string {
	if value != "" && strings.Trim(value, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-./=:@%+,") == "" {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// ScriptData is what a profile's templates see.
type ScriptData struct {
	// JobID is the job's codeflower id (j-…); Name the name it was given.
	JobID string
	Name  string
	CPUs  int
	// Memory in bytes, Time the time limit.
	Memory  int64
	Time    time.Duration
	GPUs    int
	GPUType string
	Queue   string
	Account string
	Array   *Array
	// After are the scheduler's ids of the jobs this one waits for.
	After []string
	// Extra are the config's jobs.extra options.
	Extra []string
	// Setup is the config's jobs.setup.
	Setup string
	// Run is the command line that starts the job's sandbox, quoted.
	Run string
	// Directory is the job's folder beside the project, holding Script and
	// SubmitFile; LogDir the job's folder in the project, where the
	// scheduler's output goes (Output).
	Directory  string
	Script     string
	SubmitFile string
	LogDir     string
	Output     string
	// User is the user's login name.
	User string
	// IDs and ID are the scheduler's ids, for history and cancel.
	IDs []string
	ID  string
}

func render(tmpl *template.Template, data ScriptData) (string, error) {
	var buffer bytes.Buffer
	if err := tmpl.Execute(&buffer, data); err != nil {
		return "", err
	}
	return buffer.String(), nil
}

// commandLine renders a command's arguments; an argument made with args
// becomes several, and an empty one made of nothing but args none.
func commandLine(c command, data ScriptData) ([]string, error) {
	var line []string
	for _, arg := range c.args {
		value, err := render(arg, data)
		if err != nil {
			return nil, err
		}
		if strings.Contains(value, argSeparator) {
			for _, part := range strings.Split(value, argSeparator) {
				if part != "" {
					line = append(line, part)
				}
			}
			continue
		}
		line = append(line, value)
	}
	return line, nil
}

// Array is a job array: the tasks' indices, and how many may run at once.
type Array struct {
	// Spec is the indices as the job gave them, without the limit, e.g. 1-10,20.
	Spec string
	// Limit is how many tasks may run at once; 0: as many as the scheduler lets.
	Limit   int
	Indices []int
	Count   int
	// Simple says the indices are one range, First-Last by Step.
	Simple            bool
	First, Last, Step int
}

const maxArrayIndex = 1_000_000

// ParseArray reads FIRST-LAST[:STEP], single indices and lists of both,
// separated by commas, with an optional %LIMIT; at most max tasks.
func ParseArray(text string, max int) (*Array, error) {
	text = strings.TrimSpace(text)
	spec, limitText, hasLimit := strings.Cut(text, "%")
	array := &Array{Spec: spec}
	if hasLimit {
		limit, err := strconv.Atoi(limitText)
		if err != nil || limit < 1 {
			return nil, fmt.Errorf("array %q: the limit after %% must be a positive number", text)
		}
		array.Limit = limit
	}
	seen := map[int]bool{}
	parts := strings.Split(spec, ",")
	for _, part := range parts {
		first, last, step, err := parseRange(part)
		if err != nil {
			return nil, fmt.Errorf("array %q: %w", text, err)
		}
		if len(parts) == 1 {
			array.Simple, array.First, array.Last, array.Step = true, first, last, step
		}
		for index := first; index <= last; index += step {
			if seen[index] {
				continue
			}
			seen[index] = true
			array.Indices = append(array.Indices, index)
			if max > 0 && len(array.Indices) > max {
				return nil, &RequestError{Code: CodeLimit, Message: fmt.Sprintf("array %q has more than %d tasks, the limit", text, max)}
			}
		}
	}
	array.Count = len(array.Indices)
	if array.Count == 0 {
		return nil, fmt.Errorf("array %q has no tasks", text)
	}
	return array, nil
}

func parseRange(text string) (first, last, step int, err error) {
	text = strings.TrimSpace(text)
	rangeText, stepText, hasStep := strings.Cut(text, ":")
	step = 1
	if hasStep {
		if step, err = strconv.Atoi(stepText); err != nil || step < 1 {
			return 0, 0, 0, fmt.Errorf("%q: the step after : must be a positive number", text)
		}
	}
	firstText, lastText, isRange := strings.Cut(rangeText, "-")
	if first, err = strconv.Atoi(firstText); err != nil || first < 0 || first > maxArrayIndex {
		return 0, 0, 0, fmt.Errorf("%q: indices are numbers from 0 to %d", text, maxArrayIndex)
	}
	last = first
	if isRange {
		if last, err = strconv.Atoi(lastText); err != nil || last < first || last > maxArrayIndex {
			return 0, 0, 0, fmt.Errorf("%q: a range is FIRST-LAST with FIRST <= LAST <= %d", text, maxArrayIndex)
		}
	} else if hasStep {
		return 0, 0, 0, fmt.Errorf("%q: a step needs a range, e.g. 1-100:2", text)
	}
	return first, last, step, nil
}
