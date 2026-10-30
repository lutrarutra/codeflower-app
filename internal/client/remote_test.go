package client

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"codeflower-app/internal/sandbox"
)

func TestAddressesParseLikeTheExtension(t *testing.T) {
	connection, err := parseAddress("worker ready:\n  codeflower://10.20.3.17:41234?token=Qm9-x_y  (keep it private)")
	if err != nil || connection.Host != "10.20.3.17" || connection.Port != 41234 || connection.Token != "Qm9-x_y" ||
		connection.URL != "codeflower://10.20.3.17:41234?token=Qm9-x_y" {
		t.Fatalf("%+v %v", connection, err)
	}
	if connection, err := parseAddress("WS://node1:8080/acp?token=abc"); err != nil || connection.Host != "node1" || connection.Port != 8080 {
		t.Fatalf("ws address: %+v %v", connection, err)
	}
	for text, want := range map[string]string{
		"nothing to see":               "paste the codeflower",
		"codeflower://node1:8080":      "no token",
		"codeflower://node1?token=abc": "no port",
	} {
		if _, err := parseAddress(text); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", text, err, want)
		}
	}
}

// fakeBridge answers /status and accepts /acp with the token, then echoes
// each line; "bye" ends with a WebSocket close frame of code 4001.
func fakeBridge(t *testing.T, token string) (address string, headers chan http.Header) {
	t.Helper()
	headers = make(chan http.Header, 4)
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/status":
			w.Write([]byte(`{"agent":"claude","agentLabel":"Claude Code","connected":false,"working":0,"waiting":0,"chats":[]}`))
			return
		case connectionPath:
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
		headers <- r.Header.Clone()
		conn, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"))
		for {
			line, err := buffered.ReadString('\n')
			if err != nil {
				return
			}
			conn.Write([]byte("echo:" + line))
			if line == "bye\n" {
				conn.Write([]byte{0x88, 0x02, 0x0f, 0xa1})
				return
			}
		}
	}))
	t.Cleanup(bridge.Close)
	return "codeflower://" + strings.TrimPrefix(bridge.URL, "http://") + "?token=" + token, headers
}

// openRelay opens a page's WebSocket to the client, as a browser would.
func openRelay(t *testing.T, server *httptest.Server, id, origin string, withCookie bool) (net.Conn, *bufio.Reader, *http.Response) {
	t.Helper()
	host := strings.TrimPrefix(server.URL, "http://")
	conn, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	request := "GET /api/workers/" + id + "/acp HTTP/1.1\r\nHost: " + host + "\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"
	if origin != "" {
		request += "Origin: " + origin + "\r\n"
	}
	if withCookie {
		request += "Cookie: " + cookieName + "=secret-key\r\n"
	}
	conn.Write([]byte(request + "\r\n"))
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	return conn, reader, response
}

func listed(t *testing.T, server *httptest.Server) []map[string]any {
	t.Helper()
	request, _ := http.NewRequest("GET", server.URL+"/api/workers", nil)
	request.AddCookie(&http.Cookie{Name: cookieName, Value: "secret-key"})
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var workers []map[string]any
	_ = json.NewDecoder(response.Body).Decode(&workers)
	return workers
}

func waitFor(t *testing.T, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func connectTo(t *testing.T, server *httptest.Server, address string) map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"address": "the worker printed: " + address})
	response, data := authorized(t, server, "POST", "/api/workers/connect", string(body))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("connect: %d %v", response.StatusCode, data)
	}
	return data
}

func TestPagesReachAnAddedWorkerThroughTheRelay(t *testing.T) {
	_, server := testServer(t)
	address, headers := fakeBridge(t, "bridge-token")
	id := connectTo(t, server, address)["id"].(string)
	waitFor(t, "the worker to answer", func() bool {
		workers := listed(t, server)
		return len(workers) == 1 && workers[0]["status"] == "ready" && workers[0]["label"] == "Claude Code"
	})
	if workers := listed(t, server); workers[0]["source"] != "added" {
		t.Fatalf("worker %v", workers[0])
	}

	origin := "http://" + strings.TrimPrefix(server.URL, "http://")
	conn, reader, response := openRelay(t, server, id, origin, true)
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("relay: %d", response.StatusCode)
	}
	seen := <-headers
	if seen.Get("Authorization") != "Bearer bridge-token" || seen.Get("Cookie") != "" || seen.Get("Origin") != "" {
		t.Fatalf("the bridge got %v", seen)
	}
	conn.Write([]byte("hello\n"))
	if line, _ := reader.ReadString('\n'); line != "echo:hello\n" {
		t.Fatalf("echo %q", line)
	}
	if relays := listed(t, server)[0]["relays"]; relays != 1.0 {
		t.Fatalf("relays %v", relays)
	}
	// The bridge's close frame (4001, taken over) reaches the page as sent.
	conn.Write([]byte("bye\n"))
	reader.ReadString('\n')
	frame := make([]byte, 4)
	if _, err := io.ReadFull(reader, frame); err != nil || string(frame) != string([]byte{0x88, 0x02, 0x0f, 0xa1}) {
		t.Fatalf("close frame %x %v", frame, err)
	}

	// Forgetting the worker closes the pages' relays.
	conn, reader, response = openRelay(t, server, id, origin, true)
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("second relay: %d", response.StatusCode)
	}
	<-headers
	if response, data := authorized(t, server, "POST", "/api/workers/"+id+"/stop", "{}"); response.StatusCode != http.StatusConflict {
		t.Fatalf("stopping an added worker: %d %v", response.StatusCode, data)
	}
	if response, data := authorized(t, server, "DELETE", "/api/workers/"+id, "{}"); response.StatusCode != http.StatusOK {
		t.Fatalf("forget: %d %v", response.StatusCode, data)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := reader.ReadByte(); err == nil {
		t.Fatal("the relay stayed open after the worker was forgotten")
	}
}

func TestTheRelayAdmitsOnlyThisClientsPages(t *testing.T) {
	_, server := testServer(t)
	address, _ := fakeBridge(t, "bridge-token")
	id := connectTo(t, server, address)["id"].(string)
	origin := "http://" + strings.TrimPrefix(server.URL, "http://")

	for name, test := range map[string]struct {
		id, origin string
		cookie     bool
		want       int
	}{
		"another site":   {id, "https://attacker.example", true, http.StatusForbidden},
		"no origin":      {id, "", true, http.StatusForbidden},
		"no cookie":      {id, origin, false, http.StatusForbidden},
		"unknown worker": {"nope", origin, true, http.StatusNotFound},
	} {
		if _, _, response := openRelay(t, server, test.id, test.origin, test.cookie); response.StatusCode != test.want {
			t.Errorf("%s: %d, want %d", name, response.StatusCode, test.want)
		}
	}
	if response, _ := authorized(t, server, "GET", "/api/workers/"+id+"/acp", ""); response.StatusCode != http.StatusBadRequest {
		t.Errorf("plain GET: %d", response.StatusCode)
	}
}

func TestAddedWorkersAreRememberedPrivately(t *testing.T) {
	first, server := testServer(t)
	address, _ := fakeBridge(t, "old-token")
	id := connectTo(t, server, address)["id"].(string)

	// Pasting the line again, with the worker's new token, updates the entry.
	again := connectTo(t, server, strings.Replace(address, "old-token", "new-token", 1))
	if again["id"] != id || len(listed(t, server)) != 1 {
		t.Fatalf("pasted again: %v", again)
	}
	info, err := os.Stat(first.added.path)
	if err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("store %v %v", info, err)
	}
	data, _ := os.ReadFile(first.added.path)
	if !strings.Contains(string(data), "new-token") || strings.Contains(string(data), "old-token") {
		t.Fatalf("store %s", data)
	}

	// The next run lists it under the same id.
	next := &Server{added: first.added, workers: map[string]*worker{}}
	next.loadAdded()
	t.Cleanup(func() { next.workers[id].cancel() })
	if item := next.workers[id]; item == nil || item.Source != sourceAdded || item.connection.Token != "new-token" {
		t.Fatalf("loaded %+v", next.workers)
	}
}

func TestLoopbackNamesWorkThroughPortForwarding(t *testing.T) {
	server, web := testServer(t)
	for host, want := range map[string]int{
		"localhost:9999":   200,
		"127.0.0.1:1":      200,
		"[::1]:8080":       200,
		"localhost":        200,
		"attacker.example": 403,
		"evil.example:" + strings.Split(web.URL, ":")[2]: 403,
	} {
		if got := hostStatus(t, web, host); got != want {
			t.Errorf("%s: %d, want %d", host, got, want)
		}
	}
	server.loopback = false
	if got := hostStatus(t, web, "localhost:9999"); got != 403 {
		t.Errorf("another loopback name off loopback: %d", got)
	}
}

func hostStatus(t *testing.T, server *httptest.Server, host string) int {
	t.Helper()
	request, _ := http.NewRequest("GET", server.URL+"/api/workers", nil)
	request.Host = host
	request.AddCookie(&http.Cookie{Name: cookieName, Value: "secret-key"})
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	return response.StatusCode
}

func TestWithoutAnEngineTheClientStillConnects(t *testing.T) {
	server, web := testServer(t)
	server.detectEngines = func(context.Context) []sandbox.EngineStatus {
		return []sandbox.EngineStatus{{Name: "docker", Label: "Docker"}, {Name: "podman", Label: "Podman"}, {Name: "apptainer", Label: "Apptainer"}}
	}
	response, data := authorized(t, web, "GET", "/api/system", "")
	capabilities := data["capabilities"].(map[string]any)
	if response.StatusCode != 200 || capabilities["start"] != false || capabilities["connect"] != true ||
		capabilities["startDisabledReason"] == "" || data["guide"] == nil {
		t.Fatalf("system %d %v", response.StatusCode, data)
	}
	response, data = authorized(t, web, "POST", "/api/workers", `{"agent":"claude"}`)
	if response.StatusCode != 412 || data["guide"] == nil || data["error"] == "" {
		t.Fatalf("start %d %v", response.StatusCode, data)
	}
	address, _ := fakeBridge(t, "bridge-token")
	connectTo(t, web, address)
}

func TestConnectOnlyNeverLooksForEngines(t *testing.T) {
	server, web := testServer(t)
	server.options.ConnectOnly = true
	server.detectEngines = func(context.Context) []sandbox.EngineStatus {
		t.Error("engines were looked for")
		return nil
	}
	response, data := authorized(t, web, "GET", "/api/system", "")
	capabilities := data["capabilities"].(map[string]any)
	if response.StatusCode != 200 || capabilities["connectOnly"] != true || capabilities["start"] != false {
		t.Fatalf("system %d %v", response.StatusCode, data)
	}
	if response, data := authorized(t, web, "POST", "/api/workers", `{"agent":"claude"}`); response.StatusCode != 403 {
		t.Fatalf("start %d %v", response.StatusCode, data)
	}
}
