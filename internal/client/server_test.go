package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"codeflower-app/internal/sandbox"
)

func testServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	server := &Server{
		options:  Options{ProjectsDirectory: t.TempDir(), Log: io.Discard},
		secret:   "secret-key",
		settings: &settingsStore{path: filepath.Join(t.TempDir(), "app.json")},
		added:    &workerStore{path: filepath.Join(t.TempDir(), "workers.json")},
		workers:  map[string]*worker{},
		quit:     make(chan struct{}),
		loopback: true,

		detectEngines: sandbox.DetectEngines,
	}
	t.Cleanup(server.stopAll)
	http := httptest.NewServer(nil)
	server.host = strings.TrimPrefix(http.URL, "http://")
	http.Config.Handler = server.routes()
	t.Cleanup(http.Close)
	return server, http
}

func noRedirects(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }

func TestOnlyTheOpeningBrowserMayUseTheApp(t *testing.T) {
	_, server := testServer(t)
	client := &http.Client{CheckRedirect: noRedirects}
	get := func(path string, header http.Header) int {
		t.Helper()
		request, _ := http.NewRequest("GET", server.URL+path, nil)
		for name, values := range header {
			request.Header[name] = values
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		return response.StatusCode
	}
	bearer := http.Header{"Authorization": {"Bearer secret-key"}}

	// The API needs the secret as a bearer token; neither nothing, another
	// secret nor a cookie (which browsers send to every port) will do.
	for name, header := range map[string]http.Header{
		"nothing":        nil,
		"another secret": {"Authorization": {"Bearer wrong"}},
		"a cookie":       {"Cookie": {"codeflower-app=secret-key"}},
	} {
		if status := get("/api/workers", header); status != http.StatusForbidden {
			t.Errorf("with %s: %d", name, status)
		}
	}
	if status := get("/api/workers", bearer); status != http.StatusOK {
		t.Fatalf("with the secret: %d", status)
	}

	// The page itself holds nothing: it loads without the secret, and sets no cookie.
	request, _ := http.NewRequest("GET", server.URL+"/?key=secret-key", nil)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode == http.StatusForbidden || len(response.Cookies()) != 0 {
		t.Fatalf("the page: status %d, cookies %v", response.StatusCode, response.Cookies())
	}
	// A link with another secret says so.
	if status := get("/?key=wrong", nil); status != http.StatusForbidden {
		t.Errorf("a wrong link: %d", status)
	}

	// Another host name (DNS rebinding) is refused even with the secret.
	request, _ = http.NewRequest("GET", server.URL+"/api/workers", nil)
	request.Host = "attacker.example"
	request.Header.Set("Authorization", "Bearer secret-key")
	if response, _ := client.Do(request); response.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign host: %d", response.StatusCode)
	}

	// Cross-site form posts cannot send JSON.
	request, _ = http.NewRequest("POST", server.URL+"/api/quit", strings.NewReader("x=1"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Authorization", "Bearer secret-key")
	if response, _ := client.Do(request); response.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("form post: %d", response.StatusCode)
	}
}

// ticketFor asks the client for a chat's WebSocket ticket, as the page does.
func ticketFor(t *testing.T, server *httptest.Server) string {
	t.Helper()
	response, data := authorized(t, server, "POST", "/api/ticket", "{}")
	ticket, _ := data["ticket"].(string)
	if response.StatusCode != http.StatusOK || ticket == "" {
		t.Fatalf("ticket: status %d, %v", response.StatusCode, data)
	}
	return ticket
}

func TestTicketsOpenOneChatOnce(t *testing.T) {
	server, httpServer := testServer(t)
	ticket := ticketFor(t, httpServer)
	path := "/api/workers/x/acp?ticket=" + ticket
	valid := func() bool {
		request := httptest.NewRequest("GET", path, nil)
		return server.validTicket(request)
	}
	if !valid() {
		t.Fatal("a new ticket was refused")
	}
	if valid() {
		t.Fatal("a ticket worked twice")
	}
	// Only for chats, and only until it expires.
	ticket = ticketFor(t, httpServer)
	if server.validTicket(httptest.NewRequest("GET", "/api/workers?ticket="+ticket, nil)) {
		t.Fatal("a ticket opened another API")
	}
	server.ticketsMu.Lock()
	server.tickets[ticket] = time.Now().Add(-time.Second)
	server.ticketsMu.Unlock()
	if server.validTicket(httptest.NewRequest("GET", "/api/workers/x/acp?ticket="+ticket, nil)) {
		t.Fatal("an expired ticket worked")
	}
}

func authorized(t *testing.T, server *httptest.Server, method, path, body string) (*http.Response, map[string]any) {
	t.Helper()
	request, _ := http.NewRequest(method, server.URL+path, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Authorization", "Bearer secret-key")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var data map[string]any
	_ = json.NewDecoder(response.Body).Decode(&data)
	return response, data
}

func TestFolderBrowserListsFoldersAndCreatesThem(t *testing.T) {
	_, server := testServer(t)
	dir := t.TempDir()
	for _, name := range []string{"beta", "Alpha", ".hidden"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "counts.mtx"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	response, data := authorized(t, server, "GET", "/api/folders?path="+dir, "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %v", response.StatusCode, data)
	}
	folders := data["folders"].([]any)
	if len(folders) != 2 || folders[0].(map[string]any)["name"] != "Alpha" || data["files"].(float64) != 1 {
		t.Fatalf("listing: %v", data)
	}

	body, _ := json.Marshal(map[string]string{"parent": dir, "name": "results"})
	if response, data := authorized(t, server, "POST", "/api/folders", string(body)); response.StatusCode != http.StatusOK {
		t.Fatalf("create: %d %v", response.StatusCode, data)
	}
	if info, err := os.Stat(filepath.Join(dir, "results")); err != nil || !info.IsDir() {
		t.Fatal("folder was not created")
	}
	body, _ = json.Marshal(map[string]string{"parent": dir, "name": "../escape"})
	if response, _ := authorized(t, server, "POST", "/api/folders", string(body)); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("a name with slashes was accepted: %d", response.StatusCode)
	}
}

func TestStartChecksTheChoicesBeforeAnyContainerRuns(t *testing.T) {
	_, server := testServer(t)
	dir := t.TempDir()
	cases := map[string]map[string]any{
		"choose an agent":         {"agent": "gemini"},
		"project name":            {"agent": "claude", "project": "../x"},
		"input folder":            {"agent": "claude", "inputs": []string{filepath.Join(dir, "missing")}},
		"choose each folder once": {"agent": "claude", "inputs": []string{dir}, "code": []string{dir}},
	}
	for want, request := range cases {
		body, _ := json.Marshal(request)
		response, data := authorized(t, server, "POST", "/api/workers", string(body))
		if response.StatusCode != http.StatusBadRequest || !strings.Contains(data["error"].(string), want) {
			t.Errorf("%v: %d %v, want %q", request, response.StatusCode, data, want)
		}
	}
}

func TestSettingsFromSingleFolderAppsBecomeLists(t *testing.T) {
	store := &settingsStore{path: filepath.Join(t.TempDir(), "app.json")}
	if err := os.WriteFile(store.path, []byte(`{"agent":"codex","input":"/d/in","output":"/d/out"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	settings := store.load()
	if settings.Agent != "codex" || !slices.Equal(settings.Inputs, []string{"/d/in"}) ||
		!slices.Equal(settings.Outputs, []string{"/d/out"}) || settings.Code == nil {
		t.Fatalf("settings %+v", settings)
	}
}

func TestResourcesReportEveryCoreAndTheMemory(t *testing.T) {
	_, server := testServer(t)
	for range 2 {
		response, data := authorized(t, server, "GET", "/api/resources", "")
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status %d: %v", response.StatusCode, data)
		}
		memory := data["memory"].(map[string]any)
		if len(data["cores"].([]any)) == 0 || memory["total"].(float64) <= 0 || memory["used"].(float64) > memory["total"].(float64) {
			t.Fatalf("resources: %v", data)
		}
	}
}

func TestWorkerLogsHideTheToken(t *testing.T) {
	item := &worker{}
	_, _ = item.Write([]byte("ready:\n  codeflower://127.0.0.1:1?token=abcDEF_123-x\npartial"))
	_, _ = item.Write([]byte(" line\n"))
	view := item.view()
	if strings.Join(view.Log, "|") != "ready:|  codeflower://127.0.0.1:1?token=…|partial line" {
		t.Fatalf("log: %q", view.Log)
	}
}

// The bridge also prints its token URL-encoded (the link to this client) and
// as the page's key.
func TestWorkerLogsHideTheTokenInLinks(t *testing.T) {
	token := "Qm9vb_x-Y29kZWZsb3dlcg"
	item := &worker{token: token}
	_, _ = item.Write([]byte("  http://127.0.0.1:7720/#add=codeflower%3A%2F%2F10.0.0.5%3A41234%3Ftoken%3D" + token + "\n" +
		"  http://10.0.0.5:41234/?key=" + token + "\n" +
		"the token is " + token + "\n"))
	for _, line := range item.view().Log {
		if strings.Contains(line, token) || strings.Contains(line, "Qm9vb") {
			t.Fatalf("token in the log: %q", line)
		}
	}
	// Without the token known, the pattern still catches both forms.
	for _, line := range []string{"#add=codeflower%3A%2F%2Fh%3A1%3Ftoken%3DSECRET", "/?key=SECRET&x=1"} {
		if strings.Contains(redact(line, ""), "SECRET") {
			t.Fatalf("not redacted: %q", redact(line, ""))
		}
	}
}

func TestShowsWhatTheSandboxAgentDoesWithoutItsToken(t *testing.T) {
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" || r.Header.Get("Authorization") != "Bearer bridge-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"agent":"claude","connected":false,"running":true,"working":1,"waiting":1,"chats":[` +
			`{"key":"claude-1","title":"Cluster the cells","state":"working"},` +
			`{"key":"claude-2","title":"Plot QC","state":"waiting","waitingFor":"plan"}],` +
			`"usage":{"auth":{"kind":"subscription","plan":"max"},"costEstimate":true,"limitsUpdatedAt":null,` +
			`"limits":[{"id":"five_hour","label":"5-hour","usedPercent":37.5,"resetsAt":"2027-01-15T08:00:00.000Z","status":"allowed"}],` +
			`"totals":{"tokens":{"inputTokens":100,"outputTokens":20,"cachedReadTokens":50,"cachedWriteTokens":0,"thoughtTokens":0,"totalTokens":170},` +
			`"cost":{"amount":0.25,"currency":"USD"}}}}`))
	}))
	t.Cleanup(bridge.Close)
	host, port, _ := strings.Cut(strings.TrimPrefix(bridge.URL, "http://"), ":")
	portNumber, _ := strconv.Atoi(port)

	item := &worker{ID: "w1", status: "ready", done: make(chan struct{}),
		connection: &sandbox.Connection{Host: host, Port: portNumber, Token: "bridge-token"}}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		item.watchActivity(ctx, nil)
		close(stopped)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for item.view().Activity == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	view := item.view()
	if view.Activity == nil || view.Activity.Working != 1 || view.Activity.Waiting != 1 || view.Activity.Connected {
		t.Fatalf("activity %+v", view.Activity)
	}
	if chat := view.Activity.Chats[1]; chat.Title != "Plot QC" || chat.WaitingFor != "plan" {
		t.Fatalf("chat %+v", chat)
	}
	if u := view.Activity.Usage; u == nil || u.Auth.Kind != "subscription" || len(u.Limits) != 1 ||
		*u.Limits[0].UsedPercent != 37.5 || u.Totals.Tokens.Total != 170 || u.Totals.Cost.Amount != 0.25 {
		t.Fatalf("usage %+v", view.Activity.Usage)
	}
	data, _ := json.Marshal(view)
	if strings.Contains(string(data), "bridge-token") || !strings.Contains(string(data), `"waitingFor":"plan"`) {
		t.Fatalf("view %s", data)
	}

	cancel()
	<-stopped
	if item.view().Activity != nil {
		t.Fatal("a stopped watch keeps reporting activity")
	}

	if _, err := fetchActivity(context.Background(), http.DefaultClient, bridge.URL+"/status", "wrong"); err == nil {
		t.Fatal("a wrong token was accepted")
	}
}

func TestReportFrameIsServedWithoutTheCookieUnderItsOwnPolicy(t *testing.T) {
	_, server := testServer(t)
	response, err := http.Get(server.URL + "/frame?load=1")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	policy := response.Header.Get("Content-Security-Policy")
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "cf-frame-ready") {
		t.Fatalf("status %d: %s", response.StatusCode, body)
	}
	if !strings.HasPrefix(policy, "default-src 'none'") || !strings.Contains(policy, "frame-ancestors 'self'") || strings.Contains(policy, "connect-src") {
		t.Fatalf("policy %q", policy)
	}
	if response.Header.Get("X-Frame-Options") != "" || len(response.Cookies()) != 0 {
		t.Fatalf("headers %v", response.Header)
	}

	// Another host name is still refused, and the page itself still may not be framed.
	request, _ := http.NewRequest("GET", server.URL+"/frame", nil)
	request.Host = "attacker.example"
	if response, _ := http.DefaultClient.Do(request); response.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign host: %d", response.StatusCode)
	}
	response, _ = authorized(t, server, "GET", "/", "")
	if response.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("page headers %v", response.Header)
	}
}
