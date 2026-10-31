package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"codeflower-app/internal/sandbox"
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
		same := item.address == address
		if same && item.Source == sourceAdded {
			// The worker restarted with a new token, or the line was pasted again.
			item.connection, item.url = &connection, connection.URL
			if item.status == "unauthorized" {
				item.status, item.err = "connecting", ""
			}
			item.wake()
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

// maxName is the longest name a sandbox can have in the list, in characters.
const maxName = 80

// updateWorker renames a sandbox in the list, and moves an added one to
// another address or token: a new line it printed, or its host, port or token
// alone (a worker restarted elsewhere, or with a new token). The pages'
// connections to its old address close, and they connect again.
func (s *Server) updateWorker(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name    *string `json:"name"`
		Address string  `json:"address"`
		Host    string  `json:"host"`
		Port    int     `json:"port"`
		Token   string  `json:"token"`
	}
	if err := readJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	item := s.find(r.PathValue("id"))
	if item == nil {
		writeError(w, http.StatusNotFound, errors.New("no such sandbox"))
		return
	}
	var name string
	if request.Name != nil {
		name = strings.Join(strings.Fields(*request.Name), " ")
		if utf8.RuneCountInString(name) > maxName {
			writeError(w, http.StatusBadRequest, fmt.Errorf("the name is longer than %d characters", maxName))
			return
		}
	}
	var moved *sandbox.Connection
	if request.Address != "" || request.Host != "" || request.Port != 0 || request.Token != "" {
		if item.Source != sourceAdded {
			writeError(w, http.StatusConflict, errors.New("only a sandbox added by its line can change its address or token"))
			return
		}
		item.mu.Lock()
		current := *item.connection
		item.mu.Unlock()
		connection, err := movedConnection(current, request.Address, request.Host, request.Port, request.Token)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		moved = &connection
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if moved != nil {
		address := net.JoinHostPort(moved.Host, strconv.Itoa(moved.Port))
		for _, id := range s.order {
			other := s.workers[id]
			if other == item || other.Source != sourceAdded {
				continue
			}
			other.mu.Lock()
			taken := other.address == address
			other.mu.Unlock()
			if taken {
				writeError(w, http.StatusConflict, errors.New("another sandbox in the list has this address"))
				return
			}
		}
	}
	item.mu.Lock()
	if request.Name != nil {
		item.name = name
	}
	changed := moved != nil && moved.URL != item.url
	if changed {
		item.connection, item.url = moved, moved.URL
		item.address = net.JoinHostPort(moved.Host, strconv.Itoa(moved.Port))
		item.status, item.err, item.activity = "connecting", "", nil
	}
	item.mu.Unlock()
	if changed {
		item.relays.closeAll()
		item.wake()
	}
	if item.Source == sourceAdded {
		s.saveAdded()
	}
	writeJSON(w, http.StatusOK, item.view())
}

// movedConnection is current with what the user changed: a whole new line,
// or its host, port or token.
func movedConnection(current sandbox.Connection, line, host string, port int, token string) (sandbox.Connection, error) {
	if line != "" {
		return parseAddress(line)
	}
	if host = strings.Trim(strings.TrimSpace(host), "[]"); host != "" {
		if strings.ContainsAny(host, " /?#@\\") {
			return sandbox.Connection{}, errors.New("the host is not valid: give a name or an IP address, without port or path")
		}
		current.Host = host
	}
	if port != 0 {
		if port < 0 || port > 65535 {
			return sandbox.Connection{}, errors.New("the port must be between 1 and 65535")
		}
		current.Port = port
	}
	if token = strings.TrimSpace(token); token != "" {
		current.Token = token
	}
	return parseAddress("codeflower://" + net.JoinHostPort(current.Host, strconv.Itoa(current.Port)) + "?token=" + url.QueryEscape(current.Token))
}

// wake makes an added worker's watcher ask it at once.
func (w *worker) wake() {
	select {
	case w.moved <- struct{}{}:
	default:
	}
}

// addWorker lists an added worker and starts watching it; s.mu is held.
func (s *Server) addWorker(saved savedWorker, connection sandbox.Connection) *worker {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	close(done)
	item := &worker{
		ID:         saved.ID,
		Source:     sourceAdded,
		address:    net.JoinHostPort(connection.Host, strconv.Itoa(connection.Port)),
		name:       saved.Name,
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
		moved:      make(chan struct{}, 1),
	}
	s.workers[item.ID] = item
	s.order = append(s.order, item.ID)
	go item.watchActivity(ctx, s.noticesFor(item))
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
		saved = append(saved, savedWorker{ID: item.ID, Address: item.url, Name: item.name, Agent: item.agent, Label: item.label, Added: item.Started})
		item.mu.Unlock()
	}
	s.added.save(saved)
}

// savedWorker is an added worker as the client remembers it between runs.
// Address is the whole codeflower:// line, token included.
type savedWorker struct {
	ID      string    `json:"id"`
	Address string    `json:"address"`
	Name    string    `json:"name,omitempty"`
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
