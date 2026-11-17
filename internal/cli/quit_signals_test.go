package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
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

// fakeBridge answers /wrap-up with chats and /status with its wrap-up state.
type fakeBridge struct {
	mu     sync.Mutex
	chats  int
	code   int
	state  string
	asked  int
	server *httptest.Server
}

func newFakeBridge(t *testing.T, chats int) *fakeBridge {
	bridge := &fakeBridge{chats: chats, code: http.StatusAccepted, state: "running"}
	bridge.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		bridge.mu.Lock()
		defer bridge.mu.Unlock()
		switch r.URL.Path {
		case "/wrap-up":
			bridge.asked++
			w.WriteHeader(bridge.code)
			_ = json.NewEncoder(w).Encode(map[string]int{"chats": bridge.chats})
		case "/status":
			state := bridge.state
			if bridge.chats == 0 {
				state = "done"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"wrapUp": map[string]any{"reason": "quit", "state": state}})
		}
	}))
	t.Cleanup(bridge.server.Close)
	return bridge
}

func (b *fakeBridge) connection(t *testing.T) sandbox.Connection {
	host, port, err := net.SplitHostPort(strings.TrimPrefix(b.server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	number, _ := strconv.Atoi(port)
	return sandbox.Connection{Host: host, Port: number, Token: "secret"}
}

func (b *fakeBridge) set(state string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state = state
}

func (b *fakeBridge) askedCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.asked
}

type quitHarness struct {
	ctx     context.Context
	quit    *sandboxQuit
	signals chan os.Signal
	log     *syncBuffer
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func startQuit(t *testing.T, wrapUp time.Duration) *quitHarness {
	signals := make(chan os.Signal, 4)
	log := &syncBuffer{}
	ctx, quit, stop := watchQuit(context.Background(), signals, log, wrapUp, 10*time.Millisecond)
	t.Cleanup(stop)
	return &quitHarness{ctx: ctx, quit: quit, signals: signals, log: log}
}

func (h *quitHarness) stopped(t *testing.T, within time.Duration) bool {
	t.Helper()
	select {
	case <-h.ctx.Done():
		return true
	case <-time.After(within):
		return false
	}
}

func (h *quitHarness) killed() bool {
	select {
	case <-h.quit.kill:
		return true
	default:
		return false
	}
}

func TestQuitBeforeConnectedStopsAtOnce(t *testing.T) {
	h := startQuit(t, time.Minute)
	h.signals <- os.Interrupt
	if !h.stopped(t, time.Second) {
		t.Fatal("Ctrl+C before the worker listens did not stop it")
	}
	h.signals <- os.Interrupt
	waitFor(t, h.killed)
}

func TestQuitWithNoAgentWorkingStopsAtOnce(t *testing.T) {
	bridge := newFakeBridge(t, 0)
	h := startQuit(t, time.Minute)
	h.quit.connected(bridge.connection(t))
	h.signals <- os.Interrupt
	if !h.stopped(t, time.Second) {
		t.Fatal("Ctrl+C with no agent working did not stop the sandbox")
	}
	if !strings.Contains(h.log.String(), "No agent is working") || h.killed() {
		t.Fatalf("log %q, killed %v", h.log.String(), h.killed())
	}
}

func TestQuitWrapsUpWorkingChatsThenStops(t *testing.T) {
	bridge := newFakeBridge(t, 2)
	h := startQuit(t, time.Minute)
	h.quit.connected(bridge.connection(t))
	h.signals <- os.Interrupt
	waitFor(t, func() bool { return strings.Contains(h.log.String(), "Asking 2 working chats") })
	if h.stopped(t, 100*time.Millisecond) {
		t.Fatal("the sandbox stopped while its chats wrap up")
	}
	bridge.set("done")
	if !h.stopped(t, time.Second) {
		t.Fatal("the sandbox did not stop once its chats wrapped up")
	}
	if !strings.Contains(h.log.String(), "The chats wrapped up") {
		t.Fatalf("log %q", h.log.String())
	}
}

func TestQuitSingleCtrlCDuringWrapUpKeepsWaiting(t *testing.T) {
	defer func(previous time.Duration) { doublePress = previous }(doublePress)
	doublePress = 50 * time.Millisecond
	bridge := newFakeBridge(t, 1)
	h := startQuit(t, time.Minute)
	h.quit.connected(bridge.connection(t))
	h.signals <- os.Interrupt
	waitFor(t, func() bool { return strings.Contains(h.log.String(), "Asking 1 working chat ") })
	time.Sleep(2 * doublePress)
	h.signals <- os.Interrupt
	waitFor(t, func() bool { return strings.Contains(h.log.String(), "still wrapping up") })
	if h.stopped(t, 100*time.Millisecond) {
		t.Fatal("a single Ctrl+C during the wrap-up stopped the sandbox")
	}
	if bridge.askedCount() != 1 {
		t.Fatalf("the bridge was asked %d times", bridge.askedCount())
	}
}

func TestQuitDoubleCtrlCStopsNow(t *testing.T) {
	bridge := newFakeBridge(t, 1)
	h := startQuit(t, time.Minute)
	h.quit.connected(bridge.connection(t))
	h.signals <- os.Interrupt
	h.signals <- os.Interrupt
	if !h.stopped(t, time.Second) {
		t.Fatal("Ctrl+C twice did not stop the sandbox")
	}
	if h.killed() {
		t.Fatal("Ctrl+C twice killed the sandbox instead of stopping it")
	}
	h.signals <- os.Interrupt
	waitFor(t, h.killed)
}

func TestQuitDoubleCtrlCDuringWrapUpStopsNow(t *testing.T) {
	bridge := newFakeBridge(t, 1)
	h := startQuit(t, time.Minute)
	h.quit.connected(bridge.connection(t))
	h.signals <- os.Interrupt
	waitFor(t, func() bool { return strings.Contains(h.log.String(), "Asking 1 working chat ") })
	h.signals <- os.Interrupt
	if !h.stopped(t, time.Second) {
		t.Fatal("a second Ctrl+C soon after did not stop the sandbox")
	}
	if !strings.Contains(h.log.String(), "without waiting for the chats") {
		t.Fatalf("log %q", h.log.String())
	}
}

func TestQuitWrapUpTimesOut(t *testing.T) {
	bridge := newFakeBridge(t, 1)
	h := startQuit(t, 200*time.Millisecond)
	h.quit.connected(bridge.connection(t))
	h.signals <- os.Interrupt
	if !h.stopped(t, 2*time.Second) {
		t.Fatal("the sandbox did not stop when the wrap-up took too long")
	}
	if !strings.Contains(h.log.String(), "took too long") {
		t.Fatalf("log %q", h.log.String())
	}
}

func TestQuitOlderImageStopsAtOnce(t *testing.T) {
	bridge := newFakeBridge(t, 1)
	bridge.code = http.StatusNotFound
	h := startQuit(t, time.Minute)
	h.quit.connected(bridge.connection(t))
	h.signals <- os.Interrupt
	if !h.stopped(t, time.Second) {
		t.Fatal("a bridge that cannot wrap up kept the sandbox running")
	}
	if !strings.Contains(h.log.String(), "older than wrap-ups") {
		t.Fatalf("log %q", h.log.String())
	}
}

func TestQuitTerminateStopsWithoutWrapUp(t *testing.T) {
	for _, received := range []os.Signal{syscall.SIGTERM, syscall.SIGHUP} {
		bridge := newFakeBridge(t, 1)
		h := startQuit(t, time.Minute)
		h.quit.connected(bridge.connection(t))
		h.signals <- received
		if !h.stopped(t, time.Second) {
			t.Fatalf("%v did not stop the sandbox", received)
		}
		if bridge.askedCount() != 0 {
			t.Fatalf("%v asked the chats to wrap up", received)
		}
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
