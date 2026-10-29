package client

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"codeflower-launcher/internal/sandbox"
)

// connectWorker adds a worker running elsewhere (a batch job, another
// computer) by the codeflower:// line it printed, as the extension does.
func (s *Server) connectWorker(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Address string `json:"address"`
	}
	if err := readJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	connection, err := parseAddress(request.Address)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	address := net.JoinHostPort(connection.Host, strconv.Itoa(connection.Port))

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range s.order {
		item := s.workers[id]
		item.mu.Lock()
		same := item.Address == address ||
			(item.connection != nil && net.JoinHostPort(item.connection.Host, strconv.Itoa(item.connection.Port)) == address)
		if same && item.Source == sourceAdded {
			// The worker restarted with a new token, or the line was pasted again.
			item.connection, item.url = &connection, connection.URL
			if item.status == "unauthorized" {
				item.status, item.err = "connecting", ""
			}
		}
		item.mu.Unlock()
		if same {
			s.saveAdded()
			writeJSON(w, http.StatusOK, item.view())
			return
		}
	}
	item := s.addWorker(savedWorker{ID: sandbox.NewInstanceID()[:8], Address: connection.URL, Added: time.Now()}, connection)
	s.saveAdded()
	writeJSON(w, http.StatusOK, item.view())
}

// addWorker lists an added worker and starts watching it; s.mu is held.
func (s *Server) addWorker(saved savedWorker, connection sandbox.Connection) *worker {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	close(done)
	item := &worker{
		ID:         saved.ID,
		Source:     sourceAdded,
		Address:    net.JoinHostPort(connection.Host, strconv.Itoa(connection.Port)),
		Inputs:     []string{},
		Outputs:    []string{},
		Code:       []string{},
		Skills:     []string{},
		Started:    saved.Added,
		agent:      saved.Agent,
		label:      saved.Label,
		status:     "connecting",
		url:        connection.URL,
		connection: &connection,
		cancel:     cancel,
		done:       done,
	}
	s.workers[item.ID] = item
	s.order = append(s.order, item.ID)
	go item.watchActivity(ctx)
	return item
}

// loadAdded lists the workers added in earlier runs.
func (s *Server) loadAdded() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, saved := range s.added.load() {
		connection, err := parseAddress(saved.Address)
		if err != nil || saved.ID == "" || s.workers[saved.ID] != nil {
			continue
		}
		s.addWorker(saved, connection)
	}
}

// saveAdded stores the added workers, in list order; s.mu is held.
func (s *Server) saveAdded() {
	saved := []savedWorker{}
	for _, id := range s.order {
		item := s.workers[id]
		if item.Source != sourceAdded {
			continue
		}
		item.mu.Lock()
		saved = append(saved, savedWorker{ID: item.ID, Address: item.url, Agent: item.agent, Label: item.label, Added: item.Started})
		item.mu.Unlock()
	}
	s.added.save(saved)
}

// savedWorker is an added worker as the client remembers it between runs.
// Address is the whole codeflower:// line, token included.
type savedWorker struct {
	ID      string    `json:"id"`
	Address string    `json:"address"`
	Agent   string    `json:"agent,omitempty"`
	Label   string    `json:"label,omitempty"`
	Added   time.Time `json:"added"`
}

// workerStore keeps the added workers in the system's config folder
// (~/.config, ~/Library/Application Support or %AppData%), private to the
// user, as the extension keeps them in VS Code's secret storage.
type workerStore struct {
	path string
}

func newWorkerStore() *workerStore {
	dir, err := os.UserConfigDir()
	if err != nil {
		return &workerStore{}
	}
	return &workerStore{path: filepath.Join(dir, "codeflower", "workers.json")}
}

func (store *workerStore) load() []savedWorker {
	var saved struct {
		Workers []savedWorker `json:"workers"`
	}
	if store.path == "" {
		return nil
	}
	if data, err := os.ReadFile(store.path); err == nil {
		_ = json.Unmarshal(data, &saved)
	}
	return saved.Workers
}

func (store *workerStore) save(workers []savedWorker) {
	if store.path == "" {
		return
	}
	data, err := json.MarshalIndent(map[string]any{"workers": workers}, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(store.path), 0o700); err != nil {
		return
	}
	// The tokens must never be readable by others, even briefly: write a
	// private file next to it and rename it into place.
	temporary, err := os.CreateTemp(filepath.Dir(store.path), ".workers-*.json")
	if err != nil {
		return
	}
	defer os.Remove(temporary.Name())
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		temporary.Close()
		return
	}
	if err := temporary.Close(); err != nil {
		return
	}
	_ = os.Rename(temporary.Name(), store.path)
}
