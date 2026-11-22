package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// The bridge and the app talk through a spool in the project's run folder,
// one folder per sandbox (instance): the bridge writes a request as
// requests/ID.json, the app answers with responses/ID.json. A folder works
// with every engine and system, where sockets shared with the host do not.
const (
	ContainerSpool = "/codeflower-state/run/jobs"
	spoolRequests  = "requests"
	spoolResponses = "responses"
	maxRequest     = 1 << 20
)

// SpoolDir is a sandbox's spool in its project.
func SpoolDir(state, instance string) string {
	return filepath.Join(state, "run", "jobs", instance)
}

// Base is what every job of a sandbox shares.
type Base struct {
	// Project is the project folder; Instance the sandbox's id.
	Project  string
	Instance string
	Engine   string
	Image    string
	// ImageArchive is where a saved copy of a Podman image goes, if any.
	ImageArchive string
	Mounts       []Mount
	Isolated     bool
	Allowlist    string
	// Roots are the sandbox's folders a job's command and folder may be in.
	Roots []string
}

// Response is the app's answer to a request.
type Response struct {
	OK    bool    `json:"ok"`
	Code  string  `json:"code,omitempty"`
	Error string  `json:"error,omitempty"`
	Job   *Record `json:"job,omitempty"`
}

// Tracker follows a project's jobs: it submits, asks the scheduler about
// them and cancels them. The broker uses it for a sandbox, `codeflower jobs`
// on its own.
type Tracker struct {
	Settings *Settings
	Ledger   Ledger
	Log      func(string)

	backend backend
	mutex   sync.Mutex
	// missing counts the polls a finished job has had no result; one without
	// a result after missingPolls is lost.
	missing   map[string]int
	lastError string
	// arrays are the active arrays whose tasks FollowTasks reads.
	arrays map[string]bool
}

const missingPolls = 2

// NewTracker opens a project's ledger for its scheduler.
func NewTracker(settings *Settings, state string, log func(string)) (*Tracker, error) {
	if log == nil {
		log = func(string) {}
	}
	ledger, err := OpenLedger(state)
	if err != nil {
		return nil, err
	}
	return &Tracker{Settings: settings, Ledger: ledger, Log: log, backend: newBackend(settings.Profile), missing: map[string]int{}}, nil
}

// safePath is what a project's path may hold for jobs: batch scripts write
// it in scheduler directives, which do not quote.
var safePath = regexp.MustCompile(`^[A-Za-z0-9._/@+=,:~-]+$`)

// Broker serves one sandbox's requests.
type Broker struct {
	*Tracker
	Base  Base
	spool string
	// soon makes the next poll come early, after a submission.
	soon bool
}

// NewBroker prepares the spool of a sandbox about to start.
func NewBroker(settings *Settings, base Base, log func(string)) (*Broker, error) {
	if !safePath.MatchString(base.Project) {
		return nil, fmt.Errorf("the project's path %s has characters batch scripts cannot hold (spaces, quotes, …)", base.Project)
	}
	tracker, err := NewTracker(settings, base.Project, log)
	if err != nil {
		return nil, err
	}
	broker := &Broker{Tracker: tracker, Base: base, spool: SpoolDir(base.Project, base.Instance)}
	for _, name := range []string{spoolRequests, spoolResponses} {
		if err := os.MkdirAll(filepath.Join(broker.spool, name), 0o700); err != nil {
			return nil, fmt.Errorf("could not create the jobs spool: %w", err)
		}
	}
	return broker, nil
}

// Run serves requests and follows the project's jobs until ctx ends; then it
// cancels this sandbox's jobs if the config says so, and removes the spool.
func (b *Broker) Run(ctx context.Context) {
	defer os.RemoveAll(b.spool)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	nextPoll := time.Now()
	nextTasks := time.Now()
	for {
		select {
		case <-ctx.Done():
			b.stop()
			return
		case <-ticker.C:
		}
		b.handleRequests(ctx)
		if b.soon {
			b.soon = false
			if soon := time.Now().Add(10 * time.Second); soon.Before(nextPoll) {
				nextPoll = soon
			}
		}
		if !time.Now().Before(nextPoll) {
			b.Poll(ctx)
			nextPoll = time.Now().Add(b.Settings.Poll)
		}
		if !time.Now().Before(nextTasks) {
			b.FollowTasks()
			nextTasks = time.Now().Add(tasksEvery)
		}
	}
}

func (b *Broker) stop() {
	if b.Settings.OnStop != "cancel" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	records, _ := b.Ledger.Records()
	for _, record := range records {
		if Active(record.State) && record.Instance == b.Base.Instance {
			if _, err := b.Cancel(ctx, record.ID); err != nil {
				b.Log(fmt.Sprintf("jobs: could not cancel %s (%s): %v", record.ID, record.Name, err))
			}
		}
	}
}

var requestName = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}\.json$`)

func (b *Broker) handleRequests(ctx context.Context) {
	directory := filepath.Join(b.spool, spoolRequests)
	entries, err := os.ReadDir(directory)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if !requestName.MatchString(name) || !entry.Type().IsRegular() {
			continue
		}
		path := filepath.Join(directory, name)
		data, err := readLimited(path, maxRequest)
		os.Remove(path)
		response := Response{}
		var request Request
		switch {
		case err != nil:
			response = failure(&RequestError{Code: CodeInvalid, Message: "the request is too big or unreadable"})
		case json.Unmarshal(data, &request) != nil:
			response = failure(invalid("the request is not JSON"))
		default:
			response = b.handle(ctx, request)
		}
		encoded, _ := json.Marshal(response)
		if err := writeAtomically(filepath.Join(b.spool, spoolResponses, name), append(encoded, '\n'), 0o600); err != nil {
			b.Log(fmt.Sprintf("jobs: could not answer a request: %v", err))
		}
	}
}

func readLimited(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("too big")
	}
	return data, nil
}

func (b *Broker) handle(ctx context.Context, request Request) Response {
	switch request.Action {
	case ActionSubmit:
		record, err := b.Submit(ctx, request)
		if err != nil {
			response := failure(err)
			response.Job = record
			return response
		}
		b.soon = true
		return Response{OK: true, Job: record}
	case ActionCancel:
		record, err := b.Cancel(ctx, request.Job)
		if err != nil {
			return failure(err)
		}
		return Response{OK: true, Job: record}
	}
	return failure(invalid("unknown action %q", request.Action))
}

func failure(err error) Response {
	var requestError *RequestError
	if errors.As(err, &requestError) {
		return Response{Code: requestError.Code, Error: requestError.Message}
	}
	return Response{Code: CodeScheduler, Error: err.Error()}
}

// Submit checks a request and submits it as a job of the project.
func (b *Broker) Submit(ctx context.Context, request Request) (*Record, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	settings, profile := b.Settings, b.Settings.Profile
	records, err := b.Ledger.Records()
	if err != nil {
		return nil, err
	}
	active := 0
	for _, record := range records {
		if Active(record.State) {
			active++
		}
	}
	job, err := settings.Policy.Check(request, profile, b.Base.Roots, active)
	if err != nil {
		return nil, err
	}
	var after []string
	for _, id := range job.After {
		waited, err := b.Ledger.Find(id)
		if err != nil {
			return nil, invalid("the job waits for %s, which is not one of this project's jobs", id)
		}
		if waited.SchedulerID == "" || waited.Scheduler != settings.Scheduler || slices.Contains([]string{StateFailed, StateCancelled, StateLost}, waited.State) {
			return nil, invalid("the job waits for %s (%s), which is %s", waited.ID, waited.Name, waited.State)
		}
		after = append(after, waited.SchedulerID)
	}

	id, err := b.Ledger.Create()
	if err != nil {
		return nil, err
	}
	notify := request.Notify == nil || *request.Notify
	record := &Record{ID: id, Name: job.Name, Scheduler: settings.Scheduler, State: StateSubmitting, Resources: job.Resources,
		GPUType: job.GPUType, After: job.After, Command: job.Command, Workdir: job.Workdir,
		Logs: ContainerFolder + "/" + id, Agent: request.Agent, SessionID: request.SessionID, Instance: b.Base.Instance,
		Notify: notify, Approved: request.Approved, Submitted: time.Now().UTC().Truncate(time.Second)}
	indexEnv := []string{"CODEFLOWER_JOB_INDEX"}
	if profile.ArrayIndexEnv != "" {
		indexEnv = []string{profile.ArrayIndexEnv, "CODEFLOWER_JOB_INDEX"}
	}
	spec := Spec{ID: id, Name: job.Name, Project: b.Base.Project, Engine: b.Base.Engine, Image: b.Base.Image,
		ImageArchive: b.Base.ImageArchive, Mounts: b.Base.Mounts, Isolated: b.Base.Isolated, Allowlist: b.Base.Allowlist,
		EngineArgs: settings.EngineArgs, Command: job.Command, Workdir: job.Workdir, Env: job.Env, GPUs: job.Resources.GPUs,
		PassEnv: profile.PassEnv, IndexEnv: indexEnv, Ledger: b.Ledger.JobDir(id), Logs: b.Ledger.LogDir(id)}
	if job.Array != nil {
		record.Array, record.Tasks = job.Array.Spec, job.Array.Count
		if job.Array.Limit > 0 {
			record.Array += fmt.Sprintf("%%%d", job.Array.Limit)
		}
		spec.Indices = job.Array.Indices
	}
	specJSON, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := b.Ledger.WriteFile(id, SpecFile, append(specJSON, '\n'), 0o600); err != nil {
		return nil, err
	}
	data := ScriptData{JobID: id, Name: job.Name, CPUs: job.Resources.CPUs, Memory: job.Resources.Memory, Time: job.Resources.Time,
		GPUs: job.Resources.GPUs, GPUType: job.GPUType, Queue: job.Resources.Queue, Account: settings.Account, Array: job.Array,
		After: after, Extra: settings.Extra, Setup: settings.Setup,
		Run:       ShellQuote(settings.Codeflower) + " job-run " + ShellQuote(filepath.Join(b.Ledger.JobDir(id), SpecFile)),
		Directory: b.Ledger.JobDir(id), Script: filepath.Join(b.Ledger.JobDir(id), ScriptFile), LogDir: b.Ledger.LogDir(id),
		Output: filepath.Join(b.Ledger.LogDir(id), "scheduler.log"), User: currentUser()}
	if profile.submitFile != nil {
		data.SubmitFile = filepath.Join(b.Ledger.JobDir(id), SubmitFile)
	}
	fail := func(err error) (*Record, error) {
		record.State, record.Error = StateFailed, err.Error()
		now := time.Now().UTC().Truncate(time.Second)
		record.Ended = &now
		if saveErr := b.Ledger.Save(record); saveErr != nil {
			b.Log(fmt.Sprintf("jobs: %v", saveErr))
		}
		return record, &RequestError{Code: CodeScheduler, Message: err.Error()}
	}
	script, err := render(profile.script, data)
	if err != nil {
		return fail(fmt.Errorf("could not write the batch script: %w", templateError(err)))
	}
	if err := b.Ledger.WriteFile(id, ScriptFile, []byte(script), 0o700); err != nil {
		return fail(err)
	}
	if profile.submitFile != nil {
		description, err := render(profile.submitFile, data)
		if err != nil {
			return fail(fmt.Errorf("could not write the submit file: %w", templateError(err)))
		}
		if err := b.Ledger.WriteFile(id, SubmitFile, []byte(description), 0o600); err != nil {
			return fail(err)
		}
	}
	if err := b.Ledger.Save(record); err != nil {
		return nil, err
	}
	schedulerID, err := b.backend.submit(ctx, data, record)
	if err != nil {
		return fail(fmt.Errorf("the scheduler refused the job: %w", err))
	}
	record.SchedulerID, record.State = schedulerID, StateQueued
	record.TaskStates = b.taskStates(record, "")
	b.followArray(record)
	if err := b.Ledger.Save(record); err != nil {
		return record, err
	}
	b.Log(fmt.Sprintf("jobs: submitted %s (%s) as %s job %s", record.ID, record.Name, settings.Scheduler, schedulerID))
	return record, nil
}

// templateError keeps the message of a template's fail, without the
// template's position.
func templateError(err error) error {
	message := err.Error()
	if index := strings.LastIndex(message, "error calling fail: "); index >= 0 {
		return errors.New(message[index+len("error calling fail: "):])
	}
	return err
}

// Cancel cancels a job of the project by its codeflower or scheduler id.
func (t *Tracker) Cancel(ctx context.Context, id string) (*Record, error) {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	record, err := t.Ledger.Find(id)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, &RequestError{Code: CodeUnknown, Message: fmt.Sprintf("no job %s in this project", id)}
		}
		return nil, err
	}
	if !Active(record.State) {
		return record, nil
	}
	if record.Scheduler != t.Settings.Scheduler {
		return record, &RequestError{Code: CodeScheduler, Message: fmt.Sprintf("the job was submitted to %s, not %s", record.Scheduler, t.Settings.Scheduler)}
	}
	if err := t.backend.cancel(ctx, record); err != nil {
		return record, &RequestError{Code: CodeScheduler, Message: fmt.Sprintf("could not cancel %s: %v", record.ID, err)}
	}
	now := time.Now().UTC().Truncate(time.Second)
	record.State, record.Ended = StateCancelled, &now
	// Its tasks that ended keep how they did; the others were cancelled.
	record.TaskStates = t.taskStates(record, StateCancelled)
	if err := t.Ledger.Save(record); err != nil {
		return record, err
	}
	t.Log(fmt.Sprintf("jobs: cancelled %s (%s)", record.ID, record.Name))
	return record, nil
}

// Poll asks the scheduler about the project's active jobs and records what
// changed.
func (t *Tracker) Poll(ctx context.Context) {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	records, err := t.Ledger.Records()
	if err != nil {
		t.report(err)
		return
	}
	var active []*Record
	for _, record := range records {
		if !Active(record.State) || record.Scheduler != t.Settings.Scheduler {
			continue
		}
		if record.SchedulerID == "" {
			// Its submission was cut short (the app stopped).
			if time.Since(record.Submitted) > 10*time.Minute {
				record.State, record.Error = StateFailed, "its submission was interrupted"
				t.save(record)
			}
			continue
		}
		active = append(active, record)
		t.followArray(record)
	}
	if len(active) == 0 {
		return
	}
	states, err := t.backend.status(ctx, active)
	if err != nil {
		t.report(fmt.Errorf("could not ask the scheduler about the jobs: %w", err))
		return
	}
	t.lastError = ""
	now := time.Now().UTC().Truncate(time.Second)
	var ended []*Record
	hints := map[string]string{}
	for _, record := range active {
		state, listed := states[record.ID]
		switch {
		case listed && (state == StateQueued || state == StateRunning):
			delete(t.missing, record.ID)
			if state != record.State {
				record.State = state
				if state == StateRunning && record.Started == nil {
					record.Started = &now
				}
				t.save(record)
			}
		case listed:
			hints[record.ID] = state
			ended = append(ended, record)
		default:
			ended = append(ended, record)
		}
	}
	if len(ended) > 0 {
		t.finish(ctx, ended, hints)
	}
}

func (t *Tracker) report(err error) {
	if err.Error() != t.lastError {
		t.lastError = err.Error()
		t.Log("jobs: " + err.Error())
	}
}

func (t *Tracker) save(record *Record) {
	if err := t.Ledger.Save(record); err != nil {
		t.report(err)
	}
}

// finish records how jobs the scheduler no longer runs ended: from the
// results their sandboxes wrote, else the scheduler's history, else as lost
// once no result has come for a few polls.
func (t *Tracker) finish(ctx context.Context, records []*Record, hints map[string]string) {
	type pending struct {
		record  *Record
		indices []int
		results map[int]Result
	}
	var waiting []pending
	var missingResults []*Record
	for _, record := range records {
		indices := []int{-1}
		if record.Array != "" {
			if array, err := ParseArray(record.Array, 0); err == nil {
				indices = array.Indices
			}
		}
		results, _ := t.Ledger.Results(record.ID)
		item := pending{record, indices, results}
		waiting = append(waiting, item)
		for _, index := range indices {
			if _, ok := results[index]; !ok {
				missingResults = append(missingResults, record)
				break
			}
		}
	}
	history := map[string]map[int]taskEnd{}
	if len(missingResults) > 0 && t.Settings.Profile.HasHistory() {
		var err error
		if history, err = t.backend.history(ctx, missingResults); err != nil {
			t.report(fmt.Errorf("could not look up finished jobs: %w", err))
			history = map[string]map[int]taskEnd{}
		}
	}
	for _, item := range waiting {
		record := item.record
		outcome := Outcome{}
		tasks := map[int]string{}
		var exit *int
		complete := true
		var started, ended time.Time
		for _, index := range item.indices {
			if result, ok := item.results[index]; ok {
				if result.ExitCode == 0 {
					outcome.Done++
					tasks[index] = StateDone
				} else {
					outcome.Failed++
					tasks[index] = StateFailed
				}
				code := result.ExitCode
				exit = &code
				if started.IsZero() || result.Started.Before(started) {
					started = result.Started
				}
				if result.Ended.After(ended) {
					ended = result.Ended
				}
				continue
			}
			end, known := history[record.ID][index]
			if !known && index >= 0 {
				// The history may list the array as a whole.
				end, known = history[record.ID][-1]
			}
			category := end.category
			if !known {
				category = hints[record.ID]
			}
			switch category {
			case StateDone:
				outcome.Done++
				exit = end.exit
			case StateCancelled:
				outcome.Cancelled++
			case StateFailed:
				outcome.Failed++
				exit = end.exit
			case StateQueued, StateRunning:
				// The history is ahead of the queue: ask again next time.
				complete = false
			default:
				outcome.Lost++
				category = StateLost
			}
			tasks[index] = category
		}
		if outcome.Lost > 0 && t.missing[record.ID] < missingPolls {
			// Results written on another node may take a moment to show here.
			t.missing[record.ID]++
			continue
		}
		if !complete {
			continue
		}
		delete(t.missing, record.ID)
		switch {
		case outcome.Done == len(item.indices):
			record.State = StateDone
		case outcome.Cancelled > 0:
			record.State = StateCancelled
		case outcome.Lost == len(item.indices):
			record.State = StateLost
			record.Error = "the scheduler no longer knows the job, and its sandbox left no result (stopped by its time limit or memory, or cancelled outside codeflower?); see its scheduler log"
		default:
			record.State = StateFailed
		}
		if record.Array != "" {
			record.Outcome = &outcome
			record.TaskStates = tasks
		} else {
			record.ExitCode = exit
		}
		now := time.Now().UTC().Truncate(time.Second)
		if !started.IsZero() && record.Started == nil {
			start := started.UTC().Truncate(time.Second)
			record.Started = &start
		}
		if ended.IsZero() {
			ended = now
		}
		end := ended.UTC().Truncate(time.Second)
		record.Ended = &end
		t.save(record)
		t.Log(fmt.Sprintf("jobs: %s (%s) %s", record.ID, record.Name, record.State))
	}
}
