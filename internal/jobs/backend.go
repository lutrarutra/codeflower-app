package jobs

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"time"
)

// backend submits jobs to a scheduler and asks it about them.
type backend interface {
	// submit submits a job whose script (and submit file) are written, and
	// returns the scheduler's id.
	submit(ctx context.Context, data ScriptData, record *Record) (string, error)
	// status returns the category of each record the scheduler still lists
	// (by record id); one it does not list has finished.
	status(ctx context.Context, records []*Record) (map[string]string, error)
	// history returns how the tasks of finished records ended, by record id
	// and array index (-1 for no array), where the scheduler keeps it.
	history(ctx context.Context, records []*Record) (map[string]map[int]taskEnd, error)
	cancel(ctx context.Context, record *Record) error
}

type taskEnd struct {
	category string
	exit     *int
}

// commandTimeout bounds every scheduler command.
const commandTimeout = 2 * time.Minute

// runCommand runs a scheduler command; tests replace it.
var runCommand = func(ctx context.Context, args []string, stdin []byte) (stdout, stderr []byte, err error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, args[0], args[1:]...)
	var out, errOut bytes.Buffer
	command.Stdout, command.Stderr = &out, &errOut
	if stdin != nil {
		command.Stdin = bytes.NewReader(stdin)
	}
	err = command.Run()
	return out.Bytes(), errOut.Bytes(), err
}

func newBackend(profile *Profile) backend {
	if profile.Local {
		return localBackend{}
	}
	return commandBackend{profile: profile}
}

type commandBackend struct {
	profile *Profile
}

func (b commandBackend) run(ctx context.Context, c command, data ScriptData, stdin []byte) ([]byte, error) {
	line, err := commandLine(c, data)
	if err != nil {
		return nil, err
	}
	if len(line) == 0 || line[0] == "" {
		return nil, fmt.Errorf("the command is empty")
	}
	stdout, stderr, err := runCommand(ctx, line, stdin)
	if err != nil {
		message := strings.TrimSpace(string(stderr))
		if message == "" {
			message = strings.TrimSpace(string(stdout))
		}
		if message != "" {
			return stdout, fmt.Errorf("%s: %v: %s", line[0], err, lastLines(message, 5))
		}
		return stdout, fmt.Errorf("%s: %v", line[0], err)
	}
	return stdout, nil
}

func (b commandBackend) submit(ctx context.Context, data ScriptData, record *Record) (string, error) {
	var stdin []byte
	if b.profile.submit.stdin {
		script, err := os.ReadFile(data.Script)
		if err != nil {
			return "", err
		}
		stdin = script
	}
	output, err := b.run(ctx, b.profile.submit, data, stdin)
	if err != nil {
		return "", err
	}
	text := strings.TrimSpace(string(output))
	id := firstLine(text)
	if b.profile.submit.id != nil {
		match := b.profile.submit.id.FindStringSubmatch(text)
		if match == nil {
			return "", fmt.Errorf("could not find the job's id in what the scheduler printed: %s", lastLines(text, 3))
		}
		id = match[1]
	}
	id = strings.TrimSpace(id)
	if !idPattern.MatchString(id) {
		return "", fmt.Errorf("the scheduler printed %q, which is no job id", lastLines(text, 3))
	}
	return id, nil
}

func (b commandBackend) data(records []*Record) (ScriptData, map[string]*Record) {
	data := ScriptData{User: currentUser()}
	bySchedulerID := map[string]*Record{}
	for _, record := range records {
		if record.SchedulerID == "" {
			continue
		}
		data.IDs = append(data.IDs, record.SchedulerID)
		bySchedulerID[record.SchedulerID] = record
	}
	return data, bySchedulerID
}

func (b commandBackend) status(ctx context.Context, records []*Record) (map[string]string, error) {
	data, bySchedulerID := b.data(records)
	states := map[string]string{}
	if len(data.IDs) == 0 {
		return states, nil
	}
	output, err := b.run(ctx, b.profile.status, data, nil)
	if err != nil {
		return nil, err
	}
	line := b.profile.status.line
	idGroup, stateGroup := line.SubexpIndex("id"), line.SubexpIndex("state")
	for _, text := range strings.Split(string(output), "\n") {
		match := line.FindStringSubmatch(strings.TrimRight(text, "\r"))
		if match == nil {
			continue
		}
		record := bySchedulerID[match[idGroup]]
		if record == nil {
			continue
		}
		category := b.profile.Category(match[stateGroup])
		states[record.ID] = moreActive(states[record.ID], category)
	}
	return states, nil
}

// moreActive is the state of a job whose tasks are in two: running before
// queued before the final ones.
func moreActive(a, b string) string {
	rank := func(state string) int {
		switch state {
		case StateRunning:
			return 3
		case StateQueued:
			return 2
		case "":
			return 0
		}
		return 1
	}
	if rank(b) > rank(a) {
		return b
	}
	return a
}

func (b commandBackend) history(ctx context.Context, records []*Record) (map[string]map[int]taskEnd, error) {
	ends := map[string]map[int]taskEnd{}
	if !b.profile.history.set() {
		return ends, nil
	}
	data, bySchedulerID := b.data(records)
	if len(data.IDs) == 0 {
		return ends, nil
	}
	output, err := b.run(ctx, b.profile.history, data, nil)
	if err != nil {
		return nil, err
	}
	line := b.profile.history.line
	idGroup, stateGroup := line.SubexpIndex("id"), line.SubexpIndex("state")
	indexGroup, exitGroup := line.SubexpIndex("index"), line.SubexpIndex("exit")
	for _, text := range strings.Split(string(output), "\n") {
		match := line.FindStringSubmatch(strings.TrimRight(text, "\r"))
		if match == nil {
			continue
		}
		record := bySchedulerID[match[idGroup]]
		if record == nil {
			continue
		}
		index := -1
		if indexGroup >= 0 && match[indexGroup] != "" {
			if number, err := strconv.Atoi(match[indexGroup]); err == nil {
				index = number
			}
		}
		end := taskEnd{category: b.profile.Category(match[stateGroup])}
		if exitGroup >= 0 && match[exitGroup] != "" {
			if number, err := strconv.Atoi(match[exitGroup]); err == nil {
				end.exit = &number
			}
		}
		if ends[record.ID] == nil {
			ends[record.ID] = map[int]taskEnd{}
		}
		ends[record.ID][index] = end
	}
	return ends, nil
}

func (b commandBackend) cancel(ctx context.Context, record *Record) error {
	if record.SchedulerID == "" {
		return fmt.Errorf("the job was never submitted")
	}
	_, err := b.run(ctx, b.profile.cancel, ScriptData{ID: record.SchedulerID, IDs: []string{record.SchedulerID}, User: currentUser()}, nil)
	return err
}

func currentUser() string {
	if current, err := user.Current(); err == nil {
		return current.Username
	}
	return os.Getenv("USER")
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	return line
}

func lastLines(text string, count int) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) > count {
		lines = lines[len(lines)-count:]
	}
	return strings.Join(lines, " / ")
}
