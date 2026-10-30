package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"codeflower-app/internal/sandbox"
)

// wrapUpBridge answers /wrap-up with answer, and /status with the wrap-up
// running until finish is called.
type wrapUpBridge struct {
	mu       sync.Mutex
	answer   int
	asked    []map[string]any
	finished bool
}

func newWrapUpBridge(t *testing.T, answer int) (*wrapUpBridge, *sandbox.Connection) {
	t.Helper()
	bridge := &wrapUpBridge{answer: answer}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer bridge-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		bridge.mu.Lock()
		defer bridge.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/wrap-up":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			bridge.asked = append(bridge.asked, body)
			w.WriteHeader(bridge.answer)
			w.Write([]byte(`{"chats":1}`))
		case r.URL.Path == "/status":
			state := "running"
			if bridge.finished {
				state = "done"
			}
			wrapUp := ""
			if len(bridge.asked) > 0 && bridge.answer == http.StatusAccepted {
				wrapUp = fmt.Sprintf(`,"wrapUp":{"reason":"quit","state":%q,"chats":1}`, state)
			}
			fmt.Fprintf(w, `{"agent":"claude","working":1,"waiting":0,"chats":[]%s}`, wrapUp)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	host, port, _ := strings.Cut(strings.TrimPrefix(server.URL, "http://"), ":")
	portNumber, _ := strconv.Atoi(port)
	return bridge, &sandbox.Connection{Host: host, Port: portNumber, Token: "bridge-token"}
}

func (b *wrapUpBridge) finish() {
	b.mu.Lock()
	b.finished = true
	b.mu.Unlock()
}

func (b *wrapUpBridge) requests() []map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]map[string]any(nil), b.asked...)
}

// addWorker adds a running sandbox with this many working chats, as last
// polled; it is done once cancelled.
func addWorker(s *Server, id, source string, connection *sandbox.Connection, working int) *worker {
	ctx, cancel := context.WithCancel(context.Background())
	item := &worker{ID: id, Source: source, status: "ready", connection: connection, done: make(chan struct{}), cancel: cancel,
		activity: &activity{Working: working, Chats: []chatActivity{}}}
	go func() {
		<-ctx.Done()
		close(item.done)
	}()
	s.mu.Lock()
	s.workers[id] = item
	s.order = append(s.order, id)
	s.mu.Unlock()
	return item
}

func stopped(item *worker) bool {
	select {
	case <-item.done:
		return true
	default:
		return false
	}
}

func quitClosed(s *Server) bool {
	select {
	case <-s.quit:
		return true
	default:
		return false
	}
}

func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen in time", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func fastWrapUpPoll(t *testing.T) {
	previous := wrapUpPoll
	wrapUpPoll = 10 * time.Millisecond
	t.Cleanup(func() { wrapUpPoll = previous })
}

func TestQuittingWrapsUpTheChatsInItsOwnSandboxesFirst(t *testing.T) {
	fastWrapUpPoll(t)
	server, httpServer := testServer(t)
	bridge, connection := newWrapUpBridge(t, http.StatusAccepted)
	busy := addWorker(server, "busy", sourceStarted, connection, 2)
	idle := addWorker(server, "idle", sourceStarted, nil, 0)
	addWorker(server, "elsewhere", sourceAdded, nil, 3)

	response, plan := authorized(t, httpServer, "GET", "/api/quit", "")
	if response.StatusCode != http.StatusOK || plan["sandboxes"] != 2.0 || plan["working"] != 2.0 || plan["busy"] != 1.0 ||
		plan["elsewhere"] != 1.0 || plan["minutes"] != 10.0 || plan["quitting"] != nil {
		t.Fatalf("plan %d %v", response.StatusCode, plan)
	}

	response, plan = authorized(t, httpServer, "POST", "/api/quit", "{}")
	if response.StatusCode != http.StatusOK || plan["quitting"] == nil || plan["stopping"] != false {
		t.Fatalf("quit %d %v", response.StatusCode, plan)
	}
	eventually(t, "the wrap-up request", func() bool { return len(bridge.requests()) == 1 })
	if asked := bridge.requests()[0]; asked["reason"] != "quit" || asked["minutes"] != 10.0 {
		t.Fatalf("asked %v", asked)
	}
	views := listViews(t, httpServer)
	if views["busy"] != quitAsked || views["idle"] != "" {
		t.Fatalf("views %v", views)
	}
	time.Sleep(50 * time.Millisecond)
	if stopped(busy) || stopped(idle) || quitClosed(server) {
		t.Fatal("stopped before the chats wrapped up")
	}

	bridge.finish()
	eventually(t, "the quit", func() bool { return quitClosed(server) })
	if !stopped(busy) || server.quitPlan().Quitting.Workers["busy"] != quitDone {
		t.Fatalf("the wrapped-up sandbox was not stopped: %+v", server.quitPlan().Quitting)
	}
	// Run stops the rest.
	server.stopAll()
	if !stopped(idle) {
		t.Fatal("the idle sandbox kept running")
	}
}

func listViews(t *testing.T, httpServer *httptest.Server) map[string]string {
	t.Helper()
	request, _ := http.NewRequest("GET", httpServer.URL+"/api/workers", nil)
	request.AddCookie(&http.Cookie{Name: cookieName, Value: "secret-key"})
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var views []struct {
		ID       string `json:"id"`
		Quitting string `json:"quitting"`
	}
	if err := json.NewDecoder(response.Body).Decode(&views); err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, view := range views {
		states[view.ID] = view.Quitting
	}
	return states
}

func TestQuittingWithNothingWorkingStopsAtOnce(t *testing.T) {
	server, httpServer := testServer(t)
	addWorker(server, "idle", sourceStarted, nil, 0)
	if _, plan := authorized(t, httpServer, "POST", "/api/quit", "{}"); plan["quitting"] != nil || plan["stopping"] != true {
		t.Fatalf("plan %v", plan)
	}
}

func TestAWrapUpCanBeSkippedOrCalledOff(t *testing.T) {
	fastWrapUpPoll(t)
	server, httpServer := testServer(t)
	bridge, connection := newWrapUpBridge(t, http.StatusAccepted)
	busy := addWorker(server, "busy", sourceStarted, connection, 1)

	authorized(t, httpServer, "POST", "/api/quit", "{}")
	eventually(t, "the wrap-up request", func() bool { return len(bridge.requests()) == 1 })
	if response, plan := authorized(t, httpServer, "DELETE", "/api/quit", "{}"); response.StatusCode != http.StatusOK || plan["quitting"] != nil {
		t.Fatalf("keep running %d %v", response.StatusCode, plan)
	}
	bridge.finish()
	time.Sleep(50 * time.Millisecond)
	if stopped(busy) || quitClosed(server) {
		t.Fatal("a called-off quit stopped the sandbox")
	}

	// Quit again, then stop now.
	bridge.mu.Lock()
	bridge.finished = false
	bridge.mu.Unlock()
	authorized(t, httpServer, "POST", "/api/quit", `{"wrapUp":true}`)
	eventually(t, "the second wrap-up request", func() bool { return len(bridge.requests()) == 2 })
	if quitClosed(server) {
		t.Fatal("quit before the chats wrapped up")
	}
	if _, plan := authorized(t, httpServer, "POST", "/api/quit", `{"wrapUp":false}`); plan["stopping"] != true {
		t.Fatalf("stop now %v", plan)
	}
	if response, _ := authorized(t, httpServer, "DELETE", "/api/quit", "{}"); response.StatusCode != http.StatusConflict {
		t.Fatalf("keep running after stop now: %d", response.StatusCode)
	}
}

func TestOlderSandboxesAndSlowWrapUpsAreStoppedAnyway(t *testing.T) {
	fastWrapUpPoll(t)
	server, httpServer := testServer(t)
	_, old := newWrapUpBridge(t, http.StatusNotFound)
	addWorker(server, "old", sourceStarted, old, 1)
	authorized(t, httpServer, "POST", "/api/quit", "{}")
	eventually(t, "the quit", func() bool { return quitClosed(server) })
	if state := server.quitPlan().Quitting.Workers["old"]; state != quitUnsupported {
		t.Fatalf("old bridge: %s", state)
	}

	server, httpServer = testServer(t)
	server.options.WrapUp = 100 * time.Millisecond
	_, slow := newWrapUpBridge(t, http.StatusAccepted)
	addWorker(server, "slow", sourceStarted, slow, 1)
	authorized(t, httpServer, "POST", "/api/quit", "{}")
	eventually(t, "the quit at the deadline", func() bool { return quitClosed(server) })
	if state := server.quitPlan().Quitting.Workers["slow"]; state != quitAsked {
		t.Fatalf("slow bridge: %s", state)
	}
}

func TestCtrlCWrapsUpAndAgainStopsNow(t *testing.T) {
	fastWrapUpPoll(t)
	server, _ := testServer(t)
	bridge, connection := newWrapUpBridge(t, http.StatusAccepted)
	addWorker(server, "busy", sourceStarted, connection, 1)
	signals := make(chan os.Signal)
	var log bytes.Buffer
	handled := make(chan struct{})
	go func() {
		HandleSignals(&Control{server: server, URL: "http://client"}, signals, &log)
		close(handled)
	}()

	signals <- os.Interrupt
	eventually(t, "the wrap-up request", func() bool { return len(bridge.requests()) == 1 })
	if quitClosed(server) {
		t.Fatal("the first Ctrl+C stopped the sandboxes")
	}
	signals <- os.Interrupt
	eventually(t, "the quit", func() bool { return quitClosed(server) })
	close(signals)
	<-handled
	if !strings.Contains(log.String(), "Press Ctrl+C again") {
		t.Fatalf("log %q", log.String())
	}

	// SIGTERM stops at once.
	server, _ = testServer(t)
	addWorker(server, "busy", sourceStarted, connection, 1)
	signals = make(chan os.Signal, 1)
	signals <- syscall.SIGTERM
	close(signals)
	HandleSignals(&Control{server: server}, signals, &log)
	if !quitClosed(server) || server.quitPlan().Quitting != nil {
		t.Fatal("SIGTERM did not stop at once")
	}
}

func TestTheKeyRedirectKeepsTheRestOfTheQuery(t *testing.T) {
	_, httpServer := testServer(t)
	client := &http.Client{CheckRedirect: noRedirects}
	response, err := client.Get(httpServer.URL + "/?key=secret-key&quit=1")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if location := response.Header.Get("Location"); location != "/?quit=1" {
		t.Fatalf("location %q", location)
	}
}
