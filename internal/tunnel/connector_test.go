package tunnel

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeTunnel pairs sockets like codeflower-tunnel's hub, for one connector,
// without accounts: enough to run a real connector against.
type fakeTunnel struct {
	*httptest.Server
	minProtocol int

	mu       sync.Mutex
	control  *websocket.Conn
	browsers map[string]*websocket.Conn
	next     int
	// ip: the browser address the open message reports, if set.
	ip string
	// statuses: the status frames the connector sent.
	statuses []string
}

func newFakeTunnel(t *testing.T) *fakeTunnel {
	t.Helper()
	tunnel := &fakeTunnel{browsers: map[string]*websocket.Conn{}, minProtocol: 1}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(VersionInfo{Protocol: 1, MinProtocol: tunnel.minProtocol})
	})
	mux.HandleFunc("GET /connect", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(ProtocolHeader) == "" || tunnel.minProtocol > Protocol {
			w.WriteHeader(http.StatusUpgradeRequired)
			json.NewEncoder(w).Encode(map[string]string{"error": "upgrade_required", "message": "update"})
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		tunnel.mu.Lock()
		tunnel.control = conn
		tunnel.mu.Unlock()
		for {
			_, data, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			if strings.Contains(string(data), `"status"`) {
				tunnel.mu.Lock()
				tunnel.statuses = append(tunnel.statuses, string(data))
				tunnel.mu.Unlock()
			}
		}
	})
	mux.HandleFunc("GET /connect/streams/{id}", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		conn.SetReadLimit(maxFrame)
		tunnel.mu.Lock()
		browser := tunnel.browsers[r.PathValue("id")]
		tunnel.mu.Unlock()
		ctx := context.Background()
		browser.Write(ctx, websocket.MessageText, []byte(`{"type":"ready"}`))
		pipe(ctx, conn, browser)
	})
	mux.HandleFunc("GET /c/stream", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		conn.SetReadLimit(maxFrame)
		tunnel.mu.Lock()
		tunnel.next++
		id := strings.Repeat("s", tunnel.next)
		tunnel.browsers[id] = conn
		control := tunnel.control
		message, _ := json.Marshal(map[string]string{"type": "open", "stream": id, "ip": tunnel.ip})
		tunnel.mu.Unlock()
		control.Write(context.Background(), websocket.MessageText, message)
		<-r.Context().Done()
	})
	tunnel.Server = httptest.NewServer(mux)
	t.Cleanup(tunnel.Close)
	return tunnel
}

// pipe copies frames from a to b until either closes, then closes both.
func pipe(ctx context.Context, a, b *websocket.Conn) {
	defer a.CloseNow()
	defer b.CloseNow()
	go func() {
		for {
			kind, data, err := b.Read(ctx)
			if err != nil {
				a.Close(websocket.CloseStatus(err), "")
				return
			}
			a.Write(ctx, kind, data)
		}
	}()
	for {
		kind, data, err := a.Read(ctx)
		if err != nil {
			b.Close(websocket.CloseStatus(err), "")
			return
		}
		b.Write(ctx, kind, data)
	}
}

func (f *fakeTunnel) lastStatus() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.statuses) == 0 {
		return ""
	}
	return f.statuses[len(f.statuses)-1]
}

func (f *fakeTunnel) online() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.control != nil
}

// fakeWorkers is what the fake app's /api/workers lists.
var fakeWorkers atomic.Value

// fakeApp is the client's remote page: /echo answers with what it saw,
// /api/workers lists fakeWorkers, and /api/workers/w1/acp echoes WebSocket
// messages.
func fakeApp(t *testing.T, socket string) {
	t.Helper()
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"method": r.Method, "body": string(body), "device": r.Header.Get(DeviceHeader), "workers": r.Header.Get(WorkersHeader),
		})
	})
	mux.HandleFunc("GET /api/workers", func(w http.ResponseWriter, r *http.Request) {
		workers, _ := fakeWorkers.Load().(string)
		if workers == "" || r.Header.Get(WorkersHeader) != "*" {
			workers = "[]"
		}
		io.WriteString(w, workers)
	})
	mux.HandleFunc("/api/workers/w1/acp", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		conn.SetReadLimit(MaxMessage)
		for {
			kind, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			conn.Write(r.Context(), kind, data)
		}
	})
	server := &http.Server{Handler: mux}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
}

type rig struct {
	tunnel    *fakeTunnel
	directory string
	state     *State
	notices   chan Notice
}

func startRig(t *testing.T) *rig {
	t.Helper()
	r, _ := startConnector(t, nil)
	waitUntil(t, "the connector is online", r.tunnel.online)
	return r
}

// startConnector runs a connector against a fake tunnel, with the state
// `prepare` changed, until the returned function or the test ends.
func startConnector(t *testing.T, prepare func(*State)) (*rig, func()) {
	t.Helper()
	tunnel := newFakeTunnel(t)
	// Unix socket paths are short: not under t.TempDir()'s long names.
	directory, err := mkShortTemp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeAll(directory) })
	keys, _ := GenerateKeypair()
	state := &State{URL: tunnel.URL, ConnectorID: "eu_test", Token: "token", Region: "eu",
		PrivateKey: EncodeKey(keys.Private), PublicKey: EncodeKey(keys.Public)}
	if prepare != nil {
		prepare(state)
	}
	if err := state.Save(directory); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(Directory(directory), "app.sock")
	fakeApp(t, socket)
	notices := make(chan Notice, 16)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go Run(ctx, Options{
		ConfigDirectory: directory, Version: "test",
		DialApp: func(ctx context.Context) (net.Conn, error) { return (&net.Dialer{}).DialContext(ctx, "unix", socket) },
		Notify:  func(notice Notice) { notices <- notice },
	})
	return &rig{tunnel: tunnel, directory: directory, state: state, notices: notices}, cancel
}

func waitUntil(t *testing.T, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// browser is a test browser: a stream through the fake tunnel, handshake done.
type browser struct {
	ws      *websocket.Conn
	channel *Channel
	welcome Welcome
}

func (r *rig) open(t *testing.T, keys Keypair, hello Hello) *browser {
	t.Helper()
	ctx := context.Background()
	ws, _, err := websocket.Dial(ctx, strings.Replace(r.tunnel.URL, "http", "ws", 1)+"/c/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	ws.SetReadLimit(maxFrame)
	t.Cleanup(func() { ws.CloseNow() })
	if _, ready, err := ws.Read(ctx); err != nil || string(ready) != `{"type":"ready"}` {
		t.Fatalf("ready: %s %v", ready, err)
	}
	connectorKey, _ := DecodeKey(r.state.PublicKey)
	initiator, first, err := Dial(keys, r.state.ConnectorID, connectorKey, hello)
	if err != nil {
		t.Fatal(err)
	}
	ws.Write(ctx, websocket.MessageBinary, first)
	_, second, err := ws.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	welcome, channel, err := initiator.Finish(second)
	if err != nil {
		t.Fatal(err)
	}
	return &browser{ws: ws, channel: channel, welcome: welcome}
}

func (b *browser) send(t *testing.T, frame []byte) {
	t.Helper()
	sealed, _ := b.channel.Seal(frame)
	if err := b.ws.Write(context.Background(), websocket.MessageBinary, sealed); err != nil {
		t.Fatal(err)
	}
}

func (b *browser) receive(t *testing.T) (byte, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := b.ws.Read(ctx)
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	plaintext, err := b.channel.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	kind, payload, _ := Unframe(plaintext)
	return kind, payload
}

func (r *rig) pairingSecret(t *testing.T) string {
	t.Helper()
	secret := "secret-" + time.Now().Format("150405.000000000")
	if err := Update(r.directory, func(state *State) error {
		state.Pairings = append(state.Pairings, Pairing{SecretHash: HashSecret(secret), ExpiresAt: time.Now().Add(time.Minute)})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return secret
}

func TestTheTunnelIsToldHowManySandboxesRunWhenThatChanges(t *testing.T) {
	interval := statusInterval
	statusInterval = 20 * time.Millisecond
	t.Cleanup(func() { statusInterval = interval; fakeWorkers.Store("") })
	fakeWorkers.Store(`[{"status":"ready"},{"status":"task"},{"status":"stopped"},{"status":"starting"}]`)
	r := startRig(t)
	waitUntil(t, "two sandboxes are reported", func() bool { return r.tunnel.lastStatus() == `{"sandboxes":2,"type":"status"}` })
	fakeWorkers.Store(`[]`)
	waitUntil(t, "none are reported", func() bool { return r.tunnel.lastStatus() == `{"sandboxes":0,"type":"status"}` })
	r.tunnel.mu.Lock()
	defer r.tunnel.mu.Unlock()
	if len(r.tunnel.statuses) != 2 {
		t.Fatalf("the status is sent only when it changes, got %v", r.tunnel.statuses)
	}
}

func TestUnpairedBrowsersAreRefusedAndAPairingLinkWorksOnce(t *testing.T) {
	r := startRig(t)
	stranger, _ := GenerateKeypair()
	if b := r.open(t, stranger, Hello{}); b.welcome.Error != "not_paired" {
		t.Fatalf("an unpaired browser got %+v", b.welcome)
	}

	secret := r.pairingSecret(t)
	phone, _ := GenerateKeypair()
	if b := r.open(t, phone, Hello{Pair: secret, Device: "Phone"}); b.welcome.Error != "" {
		t.Fatalf("pairing failed: %+v", b.welcome)
	}
	select {
	case notice := <-r.notices:
		if !strings.Contains(notice.Title, "paired") {
			t.Fatalf("notice %+v", notice)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no notice about the new device")
	}
	state, _ := LoadState(r.directory)
	if _, paired := state.Device(EncodeKey(phone.Public)); !paired || len(state.Pairings) != 0 {
		t.Fatalf("state after pairing: %+v", state)
	}
	other, _ := GenerateKeypair()
	if b := r.open(t, other, Hello{Pair: secret}); b.welcome.Error != "pairing_expired" {
		t.Fatalf("a used link paired again: %+v", b.welcome)
	}
	// Paired, the phone needs no secret any more.
	if b := r.open(t, phone, Hello{}); b.welcome.Error != "" {
		t.Fatalf("the paired phone was refused: %+v", b.welcome)
	}
}

func pairedPhone(t *testing.T, r *rig) Keypair {
	t.Helper()
	phone, _ := GenerateKeypair()
	if b := r.open(t, phone, Hello{Pair: r.pairingSecret(t), Device: "Phone"}); b.welcome.Error != "" {
		t.Fatal(b.welcome.Error)
	}
	return phone
}

func TestRequestsReachTheAppWithTheDevicesScopeAndNotTheBrowsersClaims(t *testing.T) {
	r := startRig(t)
	phone := pairedPhone(t, r)
	b := r.open(t, phone, Hello{})
	open, _ := JSONFrame(KindOpen, OpenRequest{Kind: "http", Method: "POST", Path: "/echo",
		Headers: map[string]string{"Content-Type": "application/json", WorkersHeader: "*", "codeflower-remote-device": "Spoofed"}})
	b.send(t, open)
	b.send(t, Frame(KindData, []byte(`{"hello":`)))
	b.send(t, Frame(KindData, []byte(`1}`)))
	b.send(t, Frame(KindEnd, nil))

	kind, data := b.receive(t)
	var head Head
	if kind != KindHead || json.Unmarshal(data, &head) != nil || head.Status != 200 {
		t.Fatalf("head: %d %s", kind, data)
	}
	var body bytes.Buffer
	for {
		kind, data := b.receive(t)
		if kind == KindEnd {
			break
		}
		body.Write(data)
	}
	var seen map[string]string
	json.Unmarshal(body.Bytes(), &seen)
	if seen["method"] != "POST" || seen["body"] != `{"hello":1}` || seen["device"] != "Phone" || seen["workers"] != "*" {
		t.Fatalf("the app saw %v", seen)
	}
}

func TestDeviceUseIsAnnouncedWithItsAddressAgainWhenTheAddressChanges(t *testing.T) {
	r := startRig(t)
	phone := pairedPhone(t, r)
	<-r.notices // paired
	request := func(ip string) {
		r.tunnel.mu.Lock()
		r.tunnel.ip = ip
		r.tunnel.mu.Unlock()
		b := r.open(t, phone, Hello{})
		open, _ := JSONFrame(KindOpen, OpenRequest{Kind: "http", Method: "GET", Path: "/echo"})
		b.send(t, open)
		b.send(t, Frame(KindEnd, nil))
		if kind, _ := b.receive(t); kind != KindHead {
			t.Fatalf("expected a response, got kind %d", kind)
		}
	}
	expect := func(body string) {
		t.Helper()
		select {
		case notice := <-r.notices:
			if notice.Body != body {
				t.Fatalf("notice %q, want %q", notice.Body, body)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("no notice %q", body)
		}
	}
	request("::ffff:203.0.113.4")
	expect(`"Phone" connected from 203.0.113.4.`)
	request("203.0.113.4")
	request("2001:db8::1")
	expect(`"Phone" connected from 2001:db8::1.`)
	// Not an address: the notice leaves it out.
	request("<b>nope</b>")
	expect(`"Phone" is using code flower through the tunnel.`)
	select {
	case notice := <-r.notices:
		t.Fatalf("unexpected notice %+v", notice)
	default:
	}
}

func TestChatsCarryLongMessagesAndCloseWhenTheDeviceIsRevoked(t *testing.T) {
	r := startRig(t)
	phone := pairedPhone(t, r)
	b := r.open(t, phone, Hello{})
	open, _ := JSONFrame(KindOpen, OpenRequest{Kind: "ws", Path: "/api/workers/w1/acp"})
	b.send(t, open)
	if kind, _ := b.receive(t); kind != KindAccepted {
		t.Fatalf("expected the WebSocket to open, got kind %d", kind)
	}
	long := strings.Repeat("x", MaxChunk*2+10)
	for _, frame := range Parts(false, []byte(long)) {
		b.send(t, frame)
	}
	var echoed []byte
	for {
		kind, data := b.receive(t)
		echoed = append(echoed, data...)
		if kind == KindText {
			break
		}
		if kind != KindTextPart {
			t.Fatalf("unexpected kind %d", kind)
		}
	}
	if string(echoed) != long {
		t.Fatalf("echoed %d bytes, want %d", len(echoed), len(long))
	}

	if err := Update(r.directory, func(state *State) error { state.Devices = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := b.ws.Read(ctx)
	if status := websocket.CloseStatus(err); status != 4401 {
		t.Fatalf("after revoking, the stream ended with %v (%d), want 4401", err, status)
	}
}

func TestOnlyChatsAreReachableAsWebSockets(t *testing.T) {
	r := startRig(t)
	b := r.open(t, pairedPhone(t, r), Hello{})
	open, _ := JSONFrame(KindOpen, OpenRequest{Kind: "ws", Path: "/api/workers/connect"})
	b.send(t, open)
	kind, data := b.receive(t)
	var frame CloseFrame
	if kind != KindClose || json.Unmarshal(data, &frame) != nil || frame.Code != 1008 {
		t.Fatalf("got %d %s", kind, data)
	}
}

func TestATunnelThatNeedsANewerAppIsReportedOnceAndNotRetriedInALoop(t *testing.T) {
	tunnel := newFakeTunnel(t)
	tunnel.minProtocol = Protocol + 1
	directory, _ := mkShortTemp()
	t.Cleanup(func() { removeAll(directory) })
	keys, _ := GenerateKeypair()
	(&State{URL: tunnel.URL, ConnectorID: "c", Token: "t", PrivateKey: EncodeKey(keys.Private), PublicKey: EncodeKey(keys.Public)}).Save(directory)
	notices := make(chan Notice, 16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, Options{ConfigDirectory: directory, DialApp: func(context.Context) (net.Conn, error) { return nil, io.EOF },
		Notify: func(notice Notice) { notices <- notice }})
	select {
	case notice := <-notices:
		if !strings.Contains(notice.Body, "codeflower --update") {
			t.Fatalf("notice %+v", notice)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no update notice")
	}
	select {
	case notice := <-notices:
		t.Fatalf("a second notice: %+v", notice)
	case <-time.After(1500 * time.Millisecond):
	}
}

func TestAConnectorWaitsWhileAnotherComputerHoldsTheTunnelAndTakesItOverOnRelease(t *testing.T) {
	r, stop := startConnector(t, func(state *State) { state.Host, state.HeldAt = "node-b.cluster", time.Now() })
	time.Sleep(1500 * time.Millisecond)
	if r.tunnel.online() {
		t.Fatal("connected while node-b holds the tunnel")
	}
	// node-b's client quit.
	if err := Update(r.directory, func(state *State) error { state.HeldAt = time.Time{}; return nil }); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the connector takes the tunnel over", r.tunnel.online)
	host, _ := os.Hostname()
	if state, _ := LoadState(r.directory); state.Host != host || state.HeldAt.IsZero() {
		t.Fatalf("the hold is %q at %v", state.Host, state.HeldAt)
	}
	stop()
	waitUntil(t, "the hold is released", func() bool {
		state, _ := LoadState(r.directory)
		return state.HeldAt.IsZero() && state.MayConnect(time.Now())
	})
}

func TestAHoldThatWasNotRenewedLapses(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		state State
		want  bool
	}{
		{State{}, true},
		{State{Host: "node-b.cluster", HeldAt: now.Add(-time.Minute)}, false},
		{State{Host: "node-b.cluster", HeldAt: now.Add(-HoldLimit - time.Second)}, true},
		{State{Host: "node-b.cluster"}, true},
	} {
		if got := c.state.MayConnect(now); got != c.want {
			t.Errorf("%+v: got %v", c.state, got)
		}
	}
	host, _ := os.Hostname()
	if !(&State{Host: host, HeldAt: now}).MayConnect(now) {
		t.Error("this computer's own hold refused it")
	}
}

func TestOnlyChatSocketsAreReachable(t *testing.T) {
	for path, want := range map[string]bool{
		"/api/workers/local-1a2b/acp":               true,
		"/api/workers/local-1a2b/acp?agent=codex":   true,
		"/api/workers/local-1a2b/acp?agent=x&y=1":   false,
		"/api/workers/local-1a2b/acp?ticket=secret": false,
		"/api/workers/local-1a2b/files":             false,
		"/api/workers/a/b/acp":                      false,
	} {
		if got := acpPath.MatchString(path); got != want {
			t.Errorf("%s: %v, want %v", path, got, want)
		}
	}
}
