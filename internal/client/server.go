// Package client serves the code flower client: a page on 127.0.0.1, opened
// in the browser, from which the user starts sandboxes on this computer (when
// a container engine can run them, else it explains how to install one) and
// connects to sandboxes running elsewhere by their codeflower:// line. Pages
// talk to a sandbox's agent through the client, which adds the sandbox's
// token, so the token never reaches the browser.
package client

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"codeflower-launcher/internal/config"
	"codeflower-launcher/internal/sandbox"
)

//go:embed page
var page embed.FS

// Options configure the window.
type Options struct {
	// Address to listen on; 127.0.0.1 with any free port by default.
	Address string
	// ProjectsDirectory holds projects started by name.
	ProjectsDirectory string
	// ConfigDirectory holds config.yaml and the skills always mounted.
	ConfigDirectory string
	// Version is shown in the page.
	Version string
	// OpenBrowser opens the page; false prints its address only.
	OpenBrowser bool
	// Ready receives the page's address (with its secret) once listening.
	Ready func(url string)
	// Log receives progress messages for the terminal.
	Log io.Writer
	// Image, or the build of the published image ImageTag names, is used
	// when the page names neither ($CODEFLOWER_IMAGE, $CODEFLOWER_IMAGE_TAG);
	// when all are empty the latest published image is downloaded on first
	// start.
	Image    string
	ImageTag string
	// ConnectOnly turns off starting sandboxes here, e.g. on a login node;
	// the client only connects to sandboxes running elsewhere.
	ConnectOnly bool
	// UIDirectory serves the page from a folder instead of the built-in one
	// (development).
	UIDirectory string
	// WrapUp is how long quitting waits at most for the chats in the
	// sandboxes to wrap up; DefaultWrapUp when zero.
	WrapUp time.Duration
	// Started receives the control of the running client, for the terminal
	// and the tray, once it listens.
	Started func(*Control)
	// Notify, if set, is told when a chat in a connected sandbox waits for
	// the user or ends its turn (desktop notifications).
	Notify func(Notice)
}

const cookieName = "codeflower-launcher"

// Server serves the page and owns the sandboxes it started.
type Server struct {
	options  Options
	secret   string
	host     string
	settings *settingsStore
	added    *workerStore
	// loopback: the client listens on this computer only, so the page may
	// arrive by any loopback name and port (e.g. through ssh -L).
	loopback bool
	// detectEngines is sandbox.DetectEngines, replaced in tests.
	detectEngines func(context.Context) []sandbox.EngineStatus

	mu       sync.Mutex
	workers  map[string]*worker
	order    []string
	lastSeen time.Time
	// quitting is a wrap-up in progress before the client quits.
	quitting *quitting
	// quit closes when the client quits and stops its sandboxes.
	quit     chan struct{}
	quitOnce sync.Once
	cpu      cpuSampler
}

func randomSecret() string {
	bytes := make([]byte, 24)
	_, _ = rand.Read(bytes)
	return base64.RawURLEncoding.EncodeToString(bytes)
}

// Run serves the window until the user quits or ctx ends, then stops every
// sandbox it started.
func Run(ctx context.Context, options Options) error {
	if options.Address == "" {
		options.Address = "127.0.0.1:0"
	}
	if options.Log == nil {
		options.Log = io.Discard
	}
	listener, err := net.Listen("tcp", options.Address)
	if err != nil {
		return fmt.Errorf("could not open the launcher window: %w", err)
	}
	server := &Server{
		options:       options,
		secret:        randomSecret(),
		host:          listener.Addr().String(),
		settings:      newSettingsStore(),
		added:         newWorkerStore(),
		workers:       map[string]*worker{},
		detectEngines: sandbox.DetectEngines,
		lastSeen:      time.Now(),
		quit:          make(chan struct{}),
	}
	if address, ok := listener.Addr().(*net.TCPAddr); ok {
		server.loopback = address.IP.IsLoopback()
	}
	server.loadAdded()
	httpServer := &http.Server{Handler: server.routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = httpServer.Serve(listener) }()

	url := fmt.Sprintf("http://%s/?key=%s", server.host, server.secret)
	forget := recordInstance(options.ConfigDirectory, url)
	defer forget()
	if options.Ready != nil {
		options.Ready(url)
	}
	if options.Started != nil {
		options.Started(&Control{server: server, URL: url})
	}
	if options.OpenBrowser {
		if err := openBrowser(url); err != nil {
			fmt.Fprintf(options.Log, "Could not open a browser (%v); open the address above yourself.\n", err)
		}
	}

	select {
	case <-ctx.Done():
	case <-server.quit:
	}
	// A client started from now on is a new one.
	forget()
	server.stopAll()
	shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdown)
	return nil
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	assets, _ := fsSub(page, "page")
	if s.options.UIDirectory != "" {
		assets = os.DirFS(s.options.UIDirectory)
	}
	mux.Handle("GET /", http.FileServerFS(assets))
	mux.HandleFunc("GET /api/system", s.system)
	mux.HandleFunc("GET /api/resources", s.resources)
	mux.HandleFunc("GET /api/folders", s.folders)
	mux.HandleFunc("POST /api/folders", s.createFolder)
	mux.HandleFunc("GET /api/workers", s.listWorkers)
	mux.HandleFunc("POST /api/workers", s.startWorker)
	mux.HandleFunc("POST /api/workers/connect", s.connectWorker)
	mux.HandleFunc("GET /api/workers/{id}/acp", s.relay)
	mux.HandleFunc("POST /api/workers/{id}/stop", s.stopWorker)
	mux.HandleFunc("DELETE /api/workers/{id}", s.forgetWorker)
	mux.HandleFunc("GET /api/quit", s.quitStatus)
	mux.HandleFunc("POST /api/quit", s.quitHandler)
	mux.HandleFunc("DELETE /api/quit", s.cancelQuit)
	return s.guard(mux)
}

// guard admits only this user's browser: the page is opened with a secret,
// which becomes a cookie. Other local users, and other websites (by DNS
// rebinding or cross-site requests), cannot drive the launcher.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.allowedHost(r.Host) {
			http.Error(w, "wrong host", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Frame-Options", "DENY")
		// connect-src: the relay's WebSockets, on this origin only. Interactive
		// reports will need frames of their own once the chat is served here.
		w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'")
		if key := r.URL.Query().Get("key"); key != "" && r.URL.Path == "/" {
			if subtle.ConstantTimeCompare([]byte(key), []byte(s.secret)) == 1 {
				http.SetCookie(w, &http.Cookie{
					Name: cookieName, Value: s.secret, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
				})
				// The rest of the query stays, e.g. quit=1 from the tray.
				query := r.URL.Query()
				query.Del("key")
				target := "/"
				if len(query) > 0 {
					target += "?" + query.Encode()
				}
				http.Redirect(w, r, target, http.StatusSeeOther)
				return
			}
		}
		cookie, err := r.Cookie(cookieName)
		if err != nil || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(s.secret)) != 1 {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprintln(w, "Open the code flower launcher with the address it printed, or start it again.")
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "expected JSON", http.StatusUnsupportedMediaType)
			return
		}
		s.mu.Lock()
		s.lastSeen = time.Now()
		s.mu.Unlock()
		next.ServeHTTP(w, r)
	})
}

// allowedHost guards against DNS rebinding: another website's name never
// reaches the client. On loopback any loopback name and port is this
// computer, e.g. localhost:8080 forwarded by ssh -L.
func (s *Server) allowedHost(host string) bool {
	if host == s.host {
		return true
	}
	if !s.loopback {
		return false
	}
	name, _, err := net.SplitHostPort(host)
	if err != nil {
		name = strings.Trim(host, "[]")
	}
	return name == "localhost" || name == "127.0.0.1" || name == "::1"
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func readJSON(r *http.Request, value any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 64*1024))
	decoder.DisallowUnknownFields()
	return decoder.Decode(value)
}

type agentInfo struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type imageStatus struct {
	Name  string `json:"name"`
	Ready bool   `json:"ready"`
	// Download: the published image is missing and is downloaded on start.
	Download bool           `json:"download,omitempty"`
	Error    string         `json:"error,omitempty"`
	Help     []sandbox.Step `json:"help,omitempty"`
}

type systemInfo struct {
	Version  string                 `json:"version"`
	System   string                 `json:"system"`
	Home     string                 `json:"home"`
	Projects string                 `json:"projects"`
	Engines  []sandbox.EngineStatus `json:"engines"`
	// Engine is the one "auto" chooses; empty when none can run sandboxes.
	Engine   string               `json:"engine"`
	Problem  string               `json:"problem,omitempty"`
	Fix      []sandbox.Step       `json:"fix,omitempty"`
	Guide    sandbox.InstallGuide `json:"guide"`
	Image    *imageStatus         `json:"image,omitempty"`
	Agents   []agentInfo          `json:"agents"`
	Settings Settings             `json:"settings"`
	// ConfigSkills are the skills folders mounted from the config in every sandbox.
	ConfigSkills    []string `json:"configSkills"`
	ConfigDirectory string   `json:"configDirectory,omitempty"`
	// Isolation says, per engine, why it cannot isolate the network here.
	Isolation map[string]string `json:"isolation"`
	// Capabilities say what the client can do here, so pages need no engine
	// logic of their own.
	Capabilities capabilities `json:"capabilities"`
}

type capabilities struct {
	// Connect to sandboxes running elsewhere: always.
	Connect bool `json:"connect"`
	// Start sandboxes on this computer: when an engine can run them.
	Start bool `json:"start"`
	// StartDisabledReason says why not.
	StartDisabledReason string `json:"startDisabledReason,omitempty"`
	// ConnectOnly: starting is turned off, and the page hides it.
	ConnectOnly bool `json:"connectOnly"`
}

func (s *Server) system(w http.ResponseWriter, r *http.Request) {
	settings := s.settings.load()
	if s.options.ConnectOnly {
		// Engines are never looked for: nothing starts here.
		writeJSON(w, http.StatusOK, systemInfo{
			Version: s.options.Version, Engines: []sandbox.EngineStatus{}, Agents: []agentInfo{}, Settings: settings,
			ConfigSkills: []string{}, Isolation: map[string]string{},
			Capabilities: capabilities{
				Connect: true, ConnectOnly: true,
				StartDisabledReason: "This client only connects to sandboxes running elsewhere.",
			},
		})
		return
	}
	statuses := s.detectEngines(r.Context())
	home, _ := os.UserHomeDir()
	info := systemInfo{
		Version:         s.options.Version,
		Home:            home,
		Projects:        s.options.ProjectsDirectory,
		Engines:         statuses,
		Guide:           sandbox.Guide(),
		Settings:        settings,
		ConfigDirectory: s.options.ConfigDirectory,
	}
	if loaded, err := config.Load(s.options.ConfigDirectory); err == nil {
		info.ConfigSkills, _ = loaded.SkillFolders()
	}
	if info.ConfigSkills == nil {
		info.ConfigSkills = []string{}
	}
	info.System = info.Guide.System
	info.Isolation = map[string]string{}
	for _, engine := range statuses {
		if err := sandbox.IsolationProblem(engine.Name); err != nil {
			info.Isolation[engine.Name] = err.Error()
		}
	}
	for _, agent := range sandbox.Agents {
		info.Agents = append(info.Agents, agentInfo{ID: agent, Label: sandbox.AgentLabel(agent)})
	}
	requested := r.URL.Query().Get("engine")
	if requested == "" {
		requested = settings.Engine
	}
	name, tag := r.URL.Query().Get("image"), r.URL.Query().Get("imageTag")
	if name == "" && tag == "" {
		name, tag = settings.Image, settings.ImageTag
	}
	image, err := s.image(name, tag)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	info.Capabilities = capabilities{Connect: true}
	engine, err := sandbox.ChooseEngine(statuses, requested, image)
	if err != nil {
		info.Problem = err.Error()
		if engine.Installed {
			info.Problem = engine.Problem
			info.Fix = engine.Fix
		}
		info.Capabilities.StartDisabledReason = info.Problem
	} else {
		info.Capabilities.Start = true
		info.Engine = engine.Name
		resolved, _ := sandbox.ResolveImage(image, engine.Name)
		status := &imageStatus{Name: resolved, Ready: true}
		if err := sandbox.CheckImage(r.Context(), engine, image); err != nil {
			status.Ready = false
			status.Download = sandbox.IsPublished(image) && errors.Is(err, sandbox.ErrNoImage)
			status.Error = err.Error()
			status.Help = sandbox.ImageHelp(engine.Name, image)
		}
		info.Image = status
	}
	writeJSON(w, http.StatusOK, info)
}

// image is the image the page asks for, by name or as a build of the
// published image; when it names neither, the launcher's options apply.
// Empty is the latest published image.
func (s *Server) image(name, tag string) (string, error) {
	name, tag = strings.TrimSpace(name), strings.TrimSpace(tag)
	if name == "" && tag == "" {
		name, tag = s.options.Image, s.options.ImageTag
	}
	if tag == "" {
		return name, nil
	}
	if name != "" {
		return "", errors.New("choose a sandbox image or a version of the published one, not both")
	}
	return sandbox.PublishedImage(tag)
}

type folderEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type folderListing struct {
	Path    string        `json:"path"`
	Parent  string        `json:"parent,omitempty"`
	Folders []folderEntry `json:"folders"`
	Files   int           `json:"files"`
	Places  []folderEntry `json:"places"`
}

// places are where browsing starts: home, common folders, and drives.
func places() []folderEntry {
	var result []folderEntry
	home, err := os.UserHomeDir()
	if err == nil {
		result = append(result, folderEntry{Name: "Home", Path: home})
		for _, name := range []string{"Desktop", "Documents", "Downloads"} {
			if info, err := os.Stat(filepath.Join(home, name)); err == nil && info.IsDir() {
				result = append(result, folderEntry{Name: name, Path: filepath.Join(home, name)})
			}
		}
	}
	switch runtime.GOOS {
	case "windows":
		for letter := 'A'; letter <= 'Z'; letter++ {
			root := string(letter) + `:\`
			if _, err := os.Stat(root); err == nil {
				result = append(result, folderEntry{Name: string(letter) + ":", Path: root})
			}
		}
	case "darwin":
		result = append(result, folderEntry{Name: "Volumes", Path: "/Volumes"})
	default:
		if sandbox.IsWSL() {
			result = append(result, folderEntry{Name: "Windows drives", Path: "/mnt"})
		}
		result = append(result, folderEntry{Name: "Computer", Path: "/"})
	}
	return result
}

func (s *Server) folders(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		path, _ = os.UserHomeDir()
	}
	path, err := filepath.Abs(path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("cannot open %s: %w", path, errors.Unwrap(err)))
		return
	}
	listing := folderListing{Path: path, Folders: []folderEntry{}, Places: places()}
	if parent := filepath.Dir(path); parent != path {
		listing.Parent = parent
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		full := filepath.Join(path, name)
		isDir := entry.IsDir()
		if entry.Type()&os.ModeSymlink != 0 {
			if info, err := os.Stat(full); err == nil {
				isDir = info.IsDir()
			}
		}
		if isDir {
			listing.Folders = append(listing.Folders, folderEntry{Name: name, Path: full})
		} else {
			listing.Files++
		}
	}
	sort.Slice(listing.Folders, func(i, j int) bool {
		return strings.ToLower(listing.Folders[i].Name) < strings.ToLower(listing.Folders[j].Name)
	})
	writeJSON(w, http.StatusOK, listing)
}

func (s *Server) createFolder(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Parent string `json:"parent"`
		Name   string `json:"name"`
	}
	if err := readJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	name := strings.TrimSpace(request.Name)
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\:`) {
		writeError(w, http.StatusBadRequest, errors.New("choose a folder name without slashes or colons"))
		return
	}
	path := filepath.Join(request.Parent, name)
	if err := os.Mkdir(path, 0o755); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("could not create the folder: %w", errors.Unwrap(err)))
		return
	}
	writeJSON(w, http.StatusOK, folderEntry{Name: name, Path: path})
}

// OpenBrowser opens the client's page (or any address) in the user's
// browser, e.g. from the tray or for a client already running.
func OpenBrowser(url string) error { return openBrowser(url) }

// openBrowser opens the page in the user's browser.
func openBrowser(url string) error {
	var candidates [][]string
	switch runtime.GOOS {
	case "darwin":
		candidates = [][]string{{"open", url}}
	case "windows":
		candidates = [][]string{{"rundll32", "url.dll,FileProtocolHandler", url}}
	default:
		if sandbox.IsWSL() {
			// The Windows browser reaches WSL's 127.0.0.1 through localhost forwarding.
			candidates = append(candidates, []string{"wslview", url}, []string{"explorer.exe", url})
		}
		candidates = append(candidates, []string{"xdg-open", url}, []string{"sensible-browser", url})
	}
	var lastError error = errors.New("no browser found")
	for _, candidate := range candidates {
		if _, err := exec.LookPath(candidate[0]); err != nil {
			continue
		}
		command := exec.Command(candidate[0], candidate[1:]...)
		if err := command.Start(); err != nil {
			lastError = err
			continue
		}
		go func() { _ = command.Wait() }()
		return nil
	}
	return lastError
}

func validAgent(agent string) bool {
	return slices.Contains(sandbox.Agents, agent)
}
