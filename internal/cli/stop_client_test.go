package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClient answers /api/quit as a running client does: busy chats wrap
// up until a quit without wrapping up (Stop now) arrives, then it is gone.
type fakeClient struct {
	mu        sync.Mutex
	sandboxes int
	busy      bool
	gone      bool
	quits     []bool
}

func (f *fakeClient) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer secret" || f.gone {
		http.Error(w, "no", http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodGet:
		_ = json.NewEncoder(w).Encode(map[string]int{"sandboxes": f.sandboxes})
	case http.MethodPost:
		var request struct {
			WrapUp bool `json:"wrapUp"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		f.quits = append(f.quits, request.WrapUp)
		if !request.WrapUp || !f.busy {
			f.gone = true
		}
		_ = json.NewEncoder(w).Encode(map[string]int{})
	}
}

func startFakeClient(t *testing.T, fake *fakeClient) string {
	t.Helper()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	directory := t.TempDir()
	record, _ := json.Marshal(map[string]any{"url": server.URL + "/?key=secret", "version": "v0.42.0"})
	if err := os.WriteFile(filepath.Join(directory, "client.json"), record, 0o600); err != nil {
		t.Fatal(err)
	}
	return directory
}

func TestStopWithoutAClientDoesNothing(t *testing.T) {
	var out bytes.Buffer
	if err := stopClient(&out, t.TempDir(), false, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "No code flower client is running") {
		t.Fatalf("output: %s", out.String())
	}
}

func TestStopAsksTheChatsToWrapUpFirst(t *testing.T) {
	fake := &fakeClient{sandboxes: 2}
	directory := startFakeClient(t, fake)
	var out bytes.Buffer
	if err := stopClient(&out, directory, false, nil); err != nil {
		t.Fatal(err)
	}
	if len(fake.quits) != 1 || !fake.quits[0] {
		t.Fatalf("quits: %v", fake.quits)
	}
	if !strings.Contains(out.String(), "the 2 sandboxes it started to wrap up") || !strings.Contains(out.String(), "Stopped code flower client v0.42.0") {
		t.Fatalf("output: %s", out.String())
	}
}

func TestStopNowStopsTheSandboxesAtOnce(t *testing.T) {
	fake := &fakeClient{sandboxes: 1, busy: true}
	directory := startFakeClient(t, fake)
	var out bytes.Buffer
	if err := stopClient(&out, directory, true, nil); err != nil {
		t.Fatal(err)
	}
	if len(fake.quits) != 1 || fake.quits[0] {
		t.Fatalf("quits: %v", fake.quits)
	}
	if !strings.Contains(out.String(), "Stopping the 1 sandbox it started") {
		t.Fatalf("output: %s", out.String())
	}
}

func TestCtrlCDuringTheWrapUpStopsThemNow(t *testing.T) {
	fake := &fakeClient{sandboxes: 1, busy: true}
	directory := startFakeClient(t, fake)
	interrupts := make(chan os.Signal, 1)
	go func() {
		// Once the wrap-up began.
		for {
			fake.mu.Lock()
			began := len(fake.quits) > 0
			fake.mu.Unlock()
			if began {
				interrupts <- os.Interrupt
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	var out bytes.Buffer
	if err := stopClient(&out, directory, false, interrupts); err != nil {
		t.Fatal(err)
	}
	if len(fake.quits) != 2 || !fake.quits[0] || fake.quits[1] {
		t.Fatalf("quits: %v", fake.quits)
	}
	if !strings.Contains(out.String(), "Stopping them now") || !strings.Contains(out.String(), "Stopped code flower client") {
		t.Fatalf("output: %s", out.String())
	}
}
