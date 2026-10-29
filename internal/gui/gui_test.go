package gui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"codeflower-launcher/internal/sandbox"
)

func testServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	server := &Server{
		options:  Options{ProjectsDirectory: t.TempDir()},
		secret:   "secret-key",
		settings: &settingsStore{path: filepath.Join(t.TempDir(), "launcher.json")},
		workers:  map[string]*worker{},
		quit:     make(chan struct{}),
	}
	http := httptest.NewServer(nil)
	server.host = strings.TrimPrefix(http.URL, "http://")
	http.Config.Handler = server.routes()
	t.Cleanup(http.Close)
	return server, http
}

func noRedirects(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }

func TestOnlyTheOpeningBrowserMayUseTheLauncher(t *testing.T) {
	_, server := testServer(t)
	client := &http.Client{CheckRedirect: noRedirects}

	for _, path := range []string{"/api/system", "/", "/?key=wrong"} {
		response, err := client.Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusForbidden {
			t.Errorf("%s without the secret: %d", path, response.StatusCode)
		}
	}

	// The secret becomes a strict, HTTP-only cookie.
	response, err := client.Get(server.URL + "/?key=secret-key")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	cookies := response.Cookies()
	if response.StatusCode != http.StatusSeeOther || len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("status %d, cookies %+v", response.StatusCode, cookies)
	}

	// Another host name (DNS rebinding) is refused even with the cookie.
	request, _ := http.NewRequest("GET", server.URL+"/api/workers", nil)
	request.Host = "attacker.example"
	request.AddCookie(cookies[0])
	if response, _ := client.Do(request); response.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign host: %d", response.StatusCode)
	}

	// Cross-site form posts cannot send JSON.
	request, _ = http.NewRequest("POST", server.URL+"/api/quit", strings.NewReader("x=1"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(cookies[0])
	if response, _ := client.Do(request); response.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("form post: %d", response.StatusCode)
	}

	request, _ = http.NewRequest("GET", server.URL+"/api/workers", nil)
	request.AddCookie(cookies[0])
	if response, _ := client.Do(request); response.StatusCode != http.StatusOK {
		t.Fatalf("with the cookie: %d", response.StatusCode)
	}
}

func authorized(t *testing.T, server *httptest.Server, method, path, body string) (*http.Response, map[string]any) {
	t.Helper()
	request, _ := http.NewRequest(method, server.URL+path, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	request.AddCookie(&http.Cookie{Name: cookieName, Value: "secret-key"})
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

func TestSettingsFromSingleFolderLaunchersBecomeLists(t *testing.T) {
	store := &settingsStore{path: filepath.Join(t.TempDir(), "launcher.json")}
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

	item := &worker{ID: "w1", status: "ready", done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		item.watchActivity(ctx, sandbox.Connection{Host: host, Port: portNumber, Token: "bridge-token"})
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
