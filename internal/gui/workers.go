package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"codeflower-launcher/internal/sandbox"
)

func fsSub(files fs.FS, dir string) (fs.FS, error) {
	return fs.Sub(files, dir)
}

const maxLogLines = 400

// The connection line has its own copy button; the log does not repeat the token.
var tokenPattern = regexp.MustCompile(`token=[A-Za-z0-9_-]+`)

// worker is a sandbox the window started.
type worker struct {
	ID      string   `json:"id"`
	Agent   string   `json:"agent"`
	Label   string   `json:"label"`
	Engine  string   `json:"engine"`
	Project string   `json:"project"`
	State   string   `json:"state"`
	Inputs  []string `json:"inputs"`
	Outputs []string `json:"outputs"`
	Code    []string `json:"code"`
	Skills  []string `json:"skills"`
	// Allowlist is the file of allowed hosts when the network is isolated.
	Allowlist string    `json:"allowlist,omitempty"`
	Started   time.Time `json:"started"`

	mu       sync.Mutex
	status   string // starting, ready, stopping, stopped, failed
	url      string
	err      string
	code     int
	log      []string
	activity *activity
	// partial holds output up to its next newline.
	partial bytes.Buffer
	cancel  context.CancelFunc
	done    chan struct{}
}

type workerView struct {
	*worker
	Status   string    `json:"status"`
	URL      string    `json:"url,omitempty"`
	Error    string    `json:"error,omitempty"`
	ExitCode int       `json:"exitCode"`
	Log      []string  `json:"log"`
	Activity *activity `json:"activity,omitempty"`
}

func (w *worker) view() workerView {
	w.mu.Lock()
	defer w.mu.Unlock()
	return workerView{worker: w, Status: w.status, URL: w.url, Error: w.err, ExitCode: w.code, Log: slicesClone(w.log), Activity: w.activity}
}

// activity is what the sandbox's agent does, as its bridge reports at /status:
// how many chats work or wait for the user, and whether VS Code is connected.
type activity struct {
	Connected bool           `json:"connected"`
	Working   int            `json:"working"`
	Waiting   int            `json:"waiting"`
	Chats     []chatActivity `json:"chats"`
	// Missing from bridges older than usage tracking.
	Usage *usage `json:"usage,omitempty"`
}

// usage is the agent's login, its subscription quota, and the tokens and
// cost of the sandbox so far.
type usage struct {
	Auth struct {
		Kind  string `json:"kind"`
		Plan  string `json:"plan,omitempty"`
		Label string `json:"label,omitempty"`
	} `json:"auth"`
	Limits []usageLimit `json:"limits"`
	// The agent's cost is its own estimate at API prices (Claude Code).
	CostEstimate bool `json:"costEstimate"`
	Totals       struct {
		Tokens struct {
			Input       int64 `json:"inputTokens"`
			Output      int64 `json:"outputTokens"`
			CachedRead  int64 `json:"cachedReadTokens"`
			CachedWrite int64 `json:"cachedWriteTokens"`
			Total       int64 `json:"totalTokens"`
		} `json:"tokens"`
		Cost *struct {
			Amount   float64 `json:"amount"`
			Currency string  `json:"currency"`
		} `json:"cost,omitempty"`
	} `json:"totals"`
}

type usageLimit struct {
	ID          string   `json:"id"`
	Label       string   `json:"label"`
	UsedPercent *float64 `json:"usedPercent"`
	ResetsAt    *string  `json:"resetsAt"`
	Status      string   `json:"status,omitempty"`
}

type chatActivity struct {
	Title string `json:"title"`
	// State is working or waiting; WaitingFor is plan, permission or question.
	State      string `json:"state"`
	WaitingFor string `json:"waitingFor,omitempty"`
}

const activityInterval = 3 * time.Second

// watchActivity polls the bridge until ctx ends. The token stays here; the
// page only gets the counts and titles.
func (w *worker) watchActivity(ctx context.Context, connection sandbox.Connection) {
	client := &http.Client{Timeout: 2 * time.Second}
	url := "http://" + net.JoinHostPort(connection.Host, strconv.Itoa(connection.Port)) + "/status"
	ticker := time.NewTicker(activityInterval)
	defer ticker.Stop()
	defer func() {
		w.mu.Lock()
		w.activity = nil
		w.mu.Unlock()
	}()
	for {
		current, err := fetchActivity(ctx, client, url, connection.Token)
		w.mu.Lock()
		if err != nil || ctx.Err() != nil {
			// A stopped sandbox reports nothing.
			current = nil
		}
		w.activity = current
		w.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func fetchActivity(ctx context.Context, client *http.Client, url, token string) (*activity, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the sandbox answered %s", response.Status)
	}
	var current activity
	if err := json.NewDecoder(response.Body).Decode(&current); err != nil {
		return nil, err
	}
	if current.Chats == nil {
		current.Chats = []chatActivity{}
	}
	return &current, nil
}

func slicesClone(lines []string) []string {
	return append([]string(nil), lines...)
}

// Write collects the sandbox's output as log lines.
func (w *worker) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.partial.Write(data)
	for {
		line, err := w.partial.ReadString('\n')
		if err != nil {
			// No newline yet: keep the rest for the next write.
			rest := line
			w.partial.Reset()
			w.partial.WriteString(rest)
			break
		}
		w.appendLine(strings.TrimRight(line, "\r\n"))
	}
	return len(data), nil
}

func (w *worker) appendLine(line string) {
	w.log = append(w.log, tokenPattern.ReplaceAllString(line, "token=…"))
	if len(w.log) > maxLogLines {
		w.log = w.log[len(w.log)-maxLogLines:]
	}
}

func (w *worker) setStatus(status string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.status = status
}

type startRequest struct {
	Agent   string   `json:"agent"`
	Inputs  []string `json:"inputs"`
	Outputs []string `json:"outputs"`
	Code    []string `json:"code"`
	Skills  []string `json:"skills"`
	Project string   `json:"project"`
	Engine  string   `json:"engine"`
	Image   string   `json:"image"`
	// ImageTag picks a build of the published image instead of Image.
	ImageTag string `json:"imageTag"`
	// IsolateNetwork limits the sandbox to its agent's service and the
	// project's allowlist.
	IsolateNetwork bool `json:"isolateNetwork"`
}

var projectName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

func (s *Server) startWorker(w http.ResponseWriter, r *http.Request) {
	var request startRequest
	if err := readJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	request.Project = strings.TrimSpace(request.Project)
	if request.Project == "" {
		request.Project = "default"
	}
	if !validAgent(request.Agent) {
		writeError(w, http.StatusBadRequest, errors.New("choose an agent"))
		return
	}
	if !projectName.MatchString(request.Project) {
		writeError(w, http.StatusBadRequest, errors.New("the project name may use letters, digits, '.', '-' and '_'"))
		return
	}
	state := filepath.Join(s.options.ProjectsDirectory, request.Project)

	s.mu.Lock()
	for _, other := range s.workers {
		view := other.view()
		if other.State == state && (view.Status == "starting" || view.Status == "ready") {
			s.mu.Unlock()
			writeError(w, http.StatusConflict, fmt.Errorf("project %q is already running; stop it first or choose another project name", request.Project))
			return
		}
	}
	s.mu.Unlock()

	binds, err := sandbox.Folders{
		Inputs: request.Inputs, Outputs: request.Outputs, Code: request.Code, Skills: request.Skills,
	}.Binds(nil)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	named, err := s.image(request.Image, request.ImageTag)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	engine, err := sandbox.ChooseEngine(sandbox.DetectEngines(r.Context()), request.Engine, named)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	image, err := sandbox.ResolveImage(named, engine.Name)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if request.IsolateNetwork {
		if err := sandbox.IsolationProblem(engine.Name); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	// The published image is downloaded when the sandbox starts, with its log.
	if err := sandbox.CheckImage(r.Context(), engine, named); err != nil && (!sandbox.IsPublished(named) || !errors.Is(err, sandbox.ErrNoImage)) {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	state, err = sandbox.PrepareState(state)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var isolation *sandbox.Isolation
	if request.IsolateNetwork {
		allowlist, err := sandbox.PrepareAllowlist(sandbox.DefaultAllowlist(state))
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		isolation = &sandbox.Isolation{Allowlist: allowlist}
	}
	s.settings.save(Settings{
		Agent: request.Agent, Inputs: request.Inputs, Outputs: request.Outputs, Code: request.Code,
		Skills: request.Skills, Project: request.Project, Engine: request.Engine, Image: request.Image, ImageTag: request.ImageTag,
		IsolateNetwork: request.IsolateNetwork,
	})

	ctx, cancel := context.WithCancel(context.Background())
	item := &worker{
		ID:      sandbox.NewInstanceID()[:8],
		Agent:   request.Agent,
		Label:   sandbox.AgentLabel(request.Agent),
		Engine:  engine.Label,
		Project: request.Project,
		State:   state,
		Inputs:  nonNil(request.Inputs),
		Outputs: nonNil(request.Outputs),
		Code:    nonNil(request.Code),
		Skills:  nonNil(request.Skills),
		Started: time.Now(),
		status:  "starting",
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	item.appendLine(fmt.Sprintf("Starting %s with %s (%s)…", item.Label, engine.Label, image))
	if isolation != nil {
		item.Allowlist = isolation.Allowlist
		item.appendLine(fmt.Sprintf("Network isolated: only %s and the hosts in %s",
			strings.Join(sandbox.ProviderHosts[request.Agent], ", "), isolation.Allowlist))
	}
	s.mu.Lock()
	s.workers[item.ID] = item
	s.order = append(s.order, item.ID)
	s.mu.Unlock()
	fmt.Fprintf(s.options.Log, "Starting %s for project %s\n", item.Label, request.Project)

	go func() {
		defer close(item.done)
		watching, stopWatching := context.WithCancel(ctx)
		code, err := 1, error(nil)
		if sandbox.IsPublished(named) {
			err = sandbox.EnsureImage(ctx, engine, named, item)
		}
		if err == nil {
			code, err = sandbox.Worker{
				Agent: request.Agent, Engine: engine, Image: image, State: state, Binds: binds,
				Isolation: isolation, Stdout: item, Stderr: item,
			}.Run(ctx, func(connection sandbox.Connection) {
				item.mu.Lock()
				item.url = connection.URL
				if item.status == "starting" {
					item.status = "ready"
				}
				item.mu.Unlock()
				go item.watchActivity(watching, connection)
			})
		}
		stopWatching()
		item.mu.Lock()
		defer item.mu.Unlock()
		if rest := item.partial.String(); rest != "" {
			item.appendLine(rest)
			item.partial.Reset()
		}
		item.url = ""
		item.code = code
		switch {
		case err != nil && item.status == "stopping" && ctx.Err() != nil:
			// Stopped while the image was downloading.
			item.status = "stopped"
		case err != nil:
			item.status, item.err = "failed", err.Error()
		case item.status == "stopping" || code == 0:
			item.status = "stopped"
		default:
			item.status, item.err = "failed", fmt.Sprintf("The sandbox stopped unexpectedly (exit status %d). See its log.", code)
		}
	}()
	writeJSON(w, http.StatusOK, item.view())
}

func (s *Server) listWorkers(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	views := make([]workerView, 0, len(s.order))
	for index := len(s.order) - 1; index >= 0; index-- {
		views = append(views, s.workers[s.order[index]].view())
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, views)
}

func (s *Server) find(id string) *worker {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.workers[id]
}

func (s *Server) stopWorker(w http.ResponseWriter, r *http.Request) {
	item := s.find(r.PathValue("id"))
	if item == nil {
		writeError(w, http.StatusNotFound, errors.New("no such sandbox"))
		return
	}
	item.mu.Lock()
	if item.status == "starting" || item.status == "ready" {
		item.status = "stopping"
		item.appendLine("Stopping…")
	}
	item.mu.Unlock()
	item.cancel()
	writeJSON(w, http.StatusOK, item.view())
}

func (s *Server) forgetWorker(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	item := s.find(id)
	if item == nil {
		writeError(w, http.StatusNotFound, errors.New("no such sandbox"))
		return
	}
	select {
	case <-item.done:
	default:
		writeError(w, http.StatusConflict, errors.New("stop the sandbox first"))
		return
	}
	s.mu.Lock()
	delete(s.workers, id)
	s.order = removeString(s.order, id)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func removeString(values []string, value string) []string {
	result := values[:0]
	for _, item := range values {
		if item != value {
			result = append(result, item)
		}
	}
	return result
}

// stopAll stops every sandbox and waits for them (at most 30 s).
func (s *Server) stopAll() {
	s.mu.Lock()
	items := make([]*worker, 0, len(s.workers))
	for _, item := range s.workers {
		items = append(items, item)
	}
	s.mu.Unlock()
	if len(items) > 0 {
		fmt.Fprintf(s.options.Log, "Stopping %d sandbox(es)…\n", len(items))
	}
	deadline := time.After(30 * time.Second)
	for _, item := range items {
		item.cancel()
	}
	for _, item := range items {
		select {
		case <-item.done:
		case <-deadline:
			return
		}
	}
}

func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// Settings are the choices the window remembers between runs.
type Settings struct {
	Agent   string   `json:"agent,omitempty"`
	Inputs  []string `json:"inputs"`
	Outputs []string `json:"outputs"`
	Code    []string `json:"code"`
	Skills  []string `json:"skills"`
	Project string   `json:"project,omitempty"`
	Engine  string   `json:"engine,omitempty"`
	Image   string   `json:"image,omitempty"`
	// ImageTag is the version of the published image (empty: the latest).
	ImageTag string `json:"imageTag,omitempty"`

	IsolateNetwork bool `json:"isolateNetwork,omitempty"`
}

type settingsStore struct {
	path string
}

func newSettingsStore() *settingsStore {
	dir, err := os.UserConfigDir()
	if err != nil {
		return &settingsStore{}
	}
	return &settingsStore{path: filepath.Join(dir, "codeflower", "launcher.json")}
}

func (store *settingsStore) load() Settings {
	var settings Settings
	if store.path == "" {
		return settings
	}
	if data, err := os.ReadFile(store.path); err == nil {
		_ = json.Unmarshal(data, &settings)
		// Launchers before multiple folders saved one input and one output.
		var single struct{ Input, Output string }
		if json.Unmarshal(data, &single) == nil {
			if settings.Inputs == nil && single.Input != "" {
				settings.Inputs = []string{single.Input}
			}
			if settings.Outputs == nil && single.Output != "" {
				settings.Outputs = []string{single.Output}
			}
		}
	}
	settings.Inputs, settings.Outputs, settings.Code = nonNil(settings.Inputs), nonNil(settings.Outputs), nonNil(settings.Code)
	settings.Skills = nonNil(settings.Skills)
	return settings
}

func (store *settingsStore) save(settings Settings) {
	if store.path == "" {
		return
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(store.path), 0o700); err != nil {
		return
	}
	_ = os.WriteFile(store.path, append(data, '\n'), 0o600)
}
