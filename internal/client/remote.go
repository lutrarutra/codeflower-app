package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"codeflower-app/internal/tunnel"
)

// The page for browsers that reach the client through the tunnel. The
// connector authenticated them (end-to-end, see internal/tunnel) and passes
// their requests over a private socket, with the device's name and the
// workers it may use. Remote browsers get less than the local page:
//
//   - always: the page, chats (sessions and the relay), stopping a sandbox,
//     and what the page reads to show them
//   - only with manage (`tunnel: {manage: true}`): the folder browser,
//     starting sandboxes and forgetting them
//   - never: connecting to new addresses (the client would reach any address
//     inside the network for a remote caller) and quitting
type remote struct {
	server *Server
	manage bool
}

// remoteScope is what the connector says a device may use.
type remoteScope struct {
	device string
	// workers is nil for all workers.
	workers []string
}

func scopeOf(r *http.Request) remoteScope {
	device, _ := url.QueryUnescape(r.Header.Get(tunnel.DeviceHeader))
	scope := remoteScope{device: device}
	if list := r.Header.Get(tunnel.WorkersHeader); list != "*" {
		scope.workers = strings.Split(list, ",")
	}
	return scope
}

func (scope remoteScope) allows(id string) bool {
	return scope.workers == nil || slices.Contains(scope.workers, id)
}

func (h *remote) routes() http.Handler {
	s := h.server
	mux := http.NewServeMux()
	assets, _ := fsSub(page, "page")
	if s.options.UIDirectory != "" {
		assets = os.DirFS(s.options.UIDirectory)
	}
	mux.Handle("GET /", http.FileServerFS(assets))
	mux.HandleFunc("GET /frame", func(w http.ResponseWriter, _ *http.Request) { serveFrame(w) })
	mux.HandleFunc("GET /api/system", h.system)
	mux.HandleFunc("GET /api/resources", s.resources)
	mux.HandleFunc("GET /api/quit", s.quitStatus)
	mux.HandleFunc("GET /api/workers", h.listWorkers)
	mux.Handle("GET /api/workers/{id}/sessions", h.scoped(s.workerSessions))
	mux.Handle("GET /api/workers/{id}/acp", h.scoped(s.relayToWorker))
	mux.Handle("POST /api/workers/{id}/stop", h.scoped(s.stopWorker))
	if h.manage {
		mux.HandleFunc("GET /api/folders", s.folders)
		mux.HandleFunc("POST /api/folders", s.createFolder)
		mux.HandleFunc("POST /api/workers", s.startWorker)
		mux.Handle("DELETE /api/workers/{id}", h.scoped(s.forgetWorker))
	} else {
		off := refuse(manageOff)
		for _, pattern := range []string{"GET /api/folders", "POST /api/folders", "POST /api/workers", "DELETE /api/workers/{id}"} {
			mux.HandleFunc(pattern, off)
		}
	}
	local := refuse("This is only possible on the computer itself, not through the tunnel.")
	for _, pattern := range []string{"POST /api/workers/connect", "POST /api/quit", "DELETE /api/quit"} {
		mux.HandleFunc(pattern, local)
	}
	return h.guard(mux)
}

const manageOff = `Starting sandboxes through the tunnel is off. To allow it, set "tunnel: {manage: true}" in config.yaml on that computer and restart codeflower client.`

func refuse(message string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusForbidden, errors.New(message))
	}
}

// system is the client's, with what remote browsers may do: never connect to
// new addresses, and start sandboxes only with manage.
func (h *remote) system(w http.ResponseWriter, r *http.Request) {
	recorder := httptest.NewRecorder()
	h.server.system(recorder, r)
	var info map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &info); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	capabilities, _ := info["capabilities"].(map[string]any)
	if capabilities == nil {
		capabilities = map[string]any{}
	}
	capabilities["connect"] = false
	if !h.manage {
		capabilities["start"] = false
		capabilities["connectOnly"] = true
		capabilities["startDisabledReason"] = manageOff
	}
	info["capabilities"] = capabilities
	writeJSON(w, recorder.Code, info)
}

func (h *remote) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path != "/frame" {
			w.Header().Set("X-Frame-Options", "DENY")
			w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'")
		}
		if r.Method != http.MethodGet && r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "expected JSON", http.StatusUnsupportedMediaType)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *remote) scoped(handler http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !scopeOf(r).allows(r.PathValue("id")) {
			writeError(w, http.StatusForbidden, errors.New("this device may not use this sandbox"))
			return
		}
		handler(w, r)
	})
}

var tokenInText = regexp.MustCompile(`token=[^&\s"']+`)

// listWorkers lists only the workers the device may use, with the bridges'
// tokens blanked out of their addresses: remote browsers never need them (the
// page only checks that a worker has an address).
func (h *remote) listWorkers(w http.ResponseWriter, r *http.Request) {
	recorder := httptest.NewRecorder()
	h.server.listWorkers(recorder, r)
	var views []map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &views); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	scope := scopeOf(r)
	shown := make([]map[string]any, 0, len(views))
	for _, view := range views {
		id, _ := view["id"].(string)
		if !scope.allows(id) {
			continue
		}
		if address, ok := view["url"].(string); ok {
			view["url"] = tokenInText.ReplaceAllString(address, "token=…")
		}
		if lines, ok := view["log"].([]any); ok {
			for index, line := range lines {
				if text, ok := line.(string); ok {
					lines[index] = tokenInText.ReplaceAllString(text, "token=…")
				}
			}
		}
		shown = append(shown, view)
	}
	writeJSON(w, http.StatusOK, shown)
}

// serveRemote serves the remote page on the connector's socket until ctx ends.
func (s *Server) serveRemote(ctx context.Context, path string, manage bool) error {
	_ = os.Remove(path)
	if err := os.MkdirAll(tunnelDirectory(path), 0o700); err != nil {
		return err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("could not open the tunnel socket: %w", err)
	}
	_ = os.Chmod(path, 0o600)
	handler := (&remote{server: s, manage: manage}).routes()
	httpServer := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdown)
		_ = os.Remove(path)
	}()
	if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func tunnelDirectory(socket string) string {
	return socket[:strings.LastIndexAny(socket, `/\`)+1]
}

// remoteWorkers is the running client's workers (ID and label), for pairing a
// browser with chosen workers.
type RemoteWorker struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Agent string `json:"agent"`
}

// RunningWorkers asks the client running for this config directory for its workers.
func RunningWorkers(configDirectory string) ([]RemoteWorker, error) {
	address, ok := Running(configDirectory)
	if !ok {
		return nil, errors.New(`code flower client is not running: start it with "codeflower client"`)
	}
	parsed, err := url.Parse(address)
	if err != nil {
		return nil, err
	}
	request, _ := http.NewRequest(http.MethodGet, (&url.URL{Scheme: parsed.Scheme, Host: parsed.Host, Path: "/api/workers"}).String(), nil)
	request.AddCookie(&http.Cookie{Name: cookieName, Value: parsed.Query().Get("key")})
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the client answered %s", response.Status)
	}
	var buffer bytes.Buffer
	_, _ = buffer.ReadFrom(response.Body)
	var workers []RemoteWorker
	return workers, json.Unmarshal(buffer.Bytes(), &workers)
}
