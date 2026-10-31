package tunnel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Headers the connector sets on the requests it passes to the client. The
// client trusts them only on its tunnel socket; the connector drops any a
// browser sends.
const (
	DeviceHeader  = "Codeflower-Remote-Device"
	WorkersHeader = "Codeflower-Remote-Workers"
)

// Notice is something to tell the user (a desktop notification).
type Notice struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

type Options struct {
	ConfigDirectory string
	// Version is this code flower's, sent to the tunnel.
	Version string
	// DialApp reaches the client's page for remote browsers (its socket).
	DialApp func(ctx context.Context) (net.Conn, error)
	Notify  func(Notice)
	Log     io.Writer
	// Connected, if set, is told the address of the page through the tunnel
	// each time the connector connects.
	Connected func(pageURL string)
}

const (
	pingInterval  = 30 * time.Second
	watchInterval = time.Second
	dialBackLimit = 15 * time.Second
	requestLimit  = 2 * time.Minute
	idleLimit     = 30 * time.Minute
	maxBody       = 16 << 20
	maxFrame      = MaxPlaintext + 16 + 1
	// accessNoticeEvery: how often the same device's use is announced from
	// the same address; a new address is announced at once.
	accessNoticeEvery = time.Hour
)

type connector struct {
	options Options
	app     *http.Client

	mu        sync.Mutex
	state     *State
	streams   map[*stream]struct{}
	announced map[string]announcement
	// warned: notices already shown for this problem, so a reconnect loop
	// does not repeat them.
	warned map[string]time.Time
}

// Run keeps the connector online until ctx ends: connected while signed in
// and not turned off, reconnecting with backoff, and stopping (with a notice)
// when the tunnel needs a newer code flower or revoked this connector.
func Run(ctx context.Context, options Options) error {
	if options.Log == nil {
		options.Log = io.Discard
	}
	if options.Notify == nil {
		options.Notify = func(Notice) {}
	}
	c := &connector{
		options:   options,
		streams:   map[*stream]struct{}{},
		announced: map[string]announcement{},
		warned:    map[string]time.Time{},
		app: &http.Client{
			Transport: &http.Transport{
				DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return options.DialApp(ctx) },
				DisableKeepAlives: true,
			},
		},
	}
	backoff := time.Second
	for ctx.Err() == nil {
		state, err := LoadState(options.ConfigDirectory)
		if err != nil || state.Off {
			if err != nil && !errors.Is(err, ErrNotSignedIn) {
				fmt.Fprintf(options.Log, "tunnel: %v\n", err)
			}
			c.waitForChange(ctx, state)
			continue
		}
		started := time.Now()
		err = c.session(ctx, state)
		if ctx.Err() != nil {
			return nil
		}
		var protocol *ProtocolError
		var status *StatusError
		switch {
		case errors.Is(err, errStateChanged):
			backoff = time.Second
			continue
		case errors.As(err, &protocol):
			if protocol.UpgradeRequired() {
				c.warnOnce("upgrade", Notice{Title: "Update code flower to use the tunnel", Body: `The tunnel was updated and no longer works with this version. Run "codeflower --update", then start the client again.`})
			} else {
				c.warnOnce("tunnel-old", Notice{Title: "The tunnel is older than code flower", Body: "Remote access stays off until the tunnel's operator updates it."})
			}
			fmt.Fprintf(options.Log, "tunnel: %v\n", err)
			c.waitForChange(ctx, state)
			continue
		case errors.As(err, &status) && status.Status == http.StatusUnauthorized:
			c.warnOnce("revoked", Notice{Title: "Remote access was revoked", Body: `This computer is no longer signed in to the tunnel. Run "codeflower tunnel login" to sign in again.`})
			c.waitForChange(ctx, state)
			continue
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		fmt.Fprintf(options.Log, "tunnel: %v; reconnecting in %s\n", err, backoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
	return nil
}

var errStateChanged = errors.New("the tunnel settings changed")

func (c *connector) warnOnce(key string, notice Notice) {
	c.mu.Lock()
	last, seen := c.warned[key]
	if seen && time.Since(last) < 24*time.Hour {
		c.mu.Unlock()
		return
	}
	c.warned[key] = time.Now()
	c.mu.Unlock()
	c.options.Notify(notice)
}

// waitForChange waits until the state file differs from `seen` (a login,
// turning the tunnel on again) or ctx ends.
func (c *connector) waitForChange(ctx context.Context, seen *State) {
	ticker := time.NewTicker(watchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			current, err := LoadState(c.options.ConfigDirectory)
			if (seen == nil) != (err != nil) || (seen != nil && current != nil && identityChanged(seen, current)) {
				return
			}
		}
	}
}

// identityChanged: a change that needs a new connection.
func identityChanged(old, current *State) bool {
	return old.URL != current.URL || old.ConnectorID != current.ConnectorID || old.Token != current.Token ||
		old.PrivateKey != current.PrivateKey || old.Off != current.Off
}

func (c *connector) currentState() *State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// session is one connection of the control socket.
func (c *connector) session(ctx context.Context, state *State) error {
	api, err := NewAPI(state.URL, c.options.Version)
	if err != nil {
		return err
	}
	info, err := api.VersionInfo(ctx)
	if err != nil {
		return err
	}
	if after, deprecated := info.DeprecatedFor(Protocol); deprecated {
		c.warnOnce("deprecated", Notice{
			Title: "Update code flower soon",
			Body:  fmt.Sprintf(`The tunnel stops working with this version after %s. Run "codeflower --update".`, after),
		})
	}
	c.mu.Lock()
	c.state = state
	c.mu.Unlock()

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	header := api.Header()
	header.Set("Authorization", "Bearer "+state.Token)
	control, response, err := websocket.Dial(ctx, api.WebSocketURL("/connect"), &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		if response != nil {
			if refused := CheckResponse(response); refused != nil {
				return refused
			}
		}
		return err
	}
	defer control.CloseNow()
	fmt.Fprintf(c.options.Log, "tunnel: connected to %s as %s\n", api.Base.Host, state.ConnectorID)
	if c.options.Connected != nil {
		c.options.Connected(PageURL(state))
	}
	// Once connected, earlier problems are over and may be told again.
	c.mu.Lock()
	clear(c.warned)
	c.mu.Unlock()

	go func() {
		ticker := time.NewTicker(pingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := control.Write(ctx, websocket.MessageText, []byte("ping")); err != nil {
					cancel(err)
					return
				}
			}
		}
	}()
	go c.watch(ctx, cancel, state)

	for {
		kind, data, err := control.Read(ctx)
		if err != nil {
			if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
				return cause
			}
			return err
		}
		if kind != websocket.MessageText || string(data) == "pong" {
			continue
		}
		var message struct {
			Type   string `json:"type"`
			Stream string `json:"stream"`
			IP     string `json:"ip"`
		}
		if json.Unmarshal(data, &message) == nil && message.Type == "open" && message.Stream != "" {
			go c.serveStream(ctx, api, state, message.Stream, browserAddress(message.IP))
		}
	}
}

// watch follows the state file: identity changes and turning off end the
// session; revoked devices lose their open streams at once.
func (c *connector) watch(ctx context.Context, cancel context.CancelCauseFunc, started *State) {
	ticker := time.NewTicker(watchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		current, err := LoadState(c.options.ConfigDirectory)
		if err != nil || identityChanged(started, current) {
			c.closeStreams(func(*stream) bool { return true }, "The tunnel settings changed")
			cancel(errStateChanged)
			return
		}
		c.mu.Lock()
		c.state = current
		c.mu.Unlock()
		c.closeStreams(func(s *stream) bool { _, paired := current.Device(s.device.Key); return !paired }, "This device was unpaired")
	}
}

func (c *connector) closeStreams(match func(*stream) bool, reason string) {
	c.mu.Lock()
	var closing []*stream
	for s := range c.streams {
		if s.device.Key != "" && match(s) {
			closing = append(closing, s)
		}
	}
	c.mu.Unlock()
	for _, s := range closing {
		s.ws.Close(4401, reason)
	}
}

// stream is one encrypted stream with a browser.
type stream struct {
	ws      *websocket.Conn
	channel *Channel
	device  Device
	sendMu  sync.Mutex
}

func (s *stream) send(ctx context.Context, frame []byte) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	sealed, err := s.channel.Seal(frame)
	if err != nil {
		return err
	}
	return s.ws.Write(ctx, websocket.MessageBinary, sealed)
}

func (s *stream) sendJSON(ctx context.Context, kind byte, value any) error {
	frame, err := JSONFrame(kind, value)
	if err != nil {
		return err
	}
	return s.send(ctx, frame)
}

func (s *stream) receive(ctx context.Context) (byte, []byte, error) {
	kind, data, err := s.ws.Read(ctx)
	if err != nil {
		return 0, nil, err
	}
	if kind != websocket.MessageBinary {
		return 0, nil, errors.New("a text frame on a stream")
	}
	plaintext, err := s.channel.Open(data)
	if err != nil {
		return 0, nil, errors.New("a frame failed to decrypt")
	}
	return Unframe(plaintext)
}

// browserAddress is the address the tunnel reports for a browser, if it is one.
// Only the tunnel vouches for it, so it is shown, never trusted.
func browserAddress(reported string) string {
	if addr, err := netip.ParseAddr(reported); err == nil {
		return addr.Unmap().String()
	}
	return ""
}

func (c *connector) serveStream(ctx context.Context, api *API, state *State, id, ip string) {
	header := api.Header()
	header.Set("Authorization", "Bearer "+state.Token)
	dialing, cancel := context.WithTimeout(ctx, dialBackLimit)
	ws, _, err := websocket.Dial(dialing, api.WebSocketURL("/connect/streams/"+url.PathEscape(id)), &websocket.DialOptions{HTTPHeader: header})
	cancel()
	if err != nil {
		fmt.Fprintf(c.options.Log, "tunnel: could not open stream: %v\n", err)
		return
	}
	defer ws.CloseNow()
	ws.SetReadLimit(maxFrame)

	// The tunnel's `ready` goes to the browser; the first frame here is its handshake.
	reading, cancel := context.WithTimeout(ctx, dialBackLimit)
	kind, first, err := ws.Read(reading)
	cancel()
	if err != nil || kind != websocket.MessageBinary {
		return
	}
	keys, err := state.Keypair()
	if err != nil {
		return
	}
	responder, err := Accept(keys, state.ConnectorID, first)
	if err != nil {
		ws.Close(4403, "Handshake failed")
		return
	}
	device, refusal := c.authorize(responder)
	answer, channel, err := responder.Answer(Welcome{Error: refusal})
	if err != nil {
		return
	}
	if err := ws.Write(ctx, websocket.MessageBinary, answer); err != nil || refusal != "" {
		ws.Close(4403, "Not paired")
		return
	}
	s := &stream{ws: ws, channel: channel, device: device}
	c.mu.Lock()
	c.streams[s] = struct{}{}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.streams, s)
		c.mu.Unlock()
	}()

	kind2, data, err := s.receive(ctx)
	if err != nil || kind2 != KindOpen {
		ws.Close(1002, "Expected a request")
		return
	}
	var open OpenRequest
	if err := json.Unmarshal(data, &open); err != nil || !strings.HasPrefix(open.Path, "/") || strings.HasPrefix(open.Path, "//") {
		ws.Close(1002, "Bad request")
		return
	}
	c.announce(device, ip)
	switch open.Kind {
	case "http":
		c.serveHTTP(ctx, s, open)
	case "ws":
		c.serveWebSocket(ctx, s, open)
	default:
		ws.Close(1002, "Unknown request kind")
	}
}

// authorize: a paired browser, or one claiming a pairing link. Returns the
// refusal for the welcome otherwise.
func (c *connector) authorize(responder *Responder) (Device, string) {
	key := EncodeKey(responder.Peer)
	state := c.currentState()
	if state == nil || state.Off {
		return Device{}, "off"
	}
	if device, ok := state.Device(key); ok {
		return device, ""
	}
	if responder.Hello.Pair == "" {
		return Device{}, "not_paired"
	}
	var device Device
	err := Update(c.options.ConfigDirectory, func(current *State) error {
		var err error
		device, err = current.ClaimPairing(responder.Hello.Pair, key, responder.Hello.Device, time.Now())
		return err
	})
	if err != nil {
		return Device{}, "pairing_expired"
	}
	if updated, err := LoadState(c.options.ConfigDirectory); err == nil {
		c.mu.Lock()
		c.state = updated
		c.mu.Unlock()
	}
	c.options.Notify(Notice{
		Title: "New device paired with code flower",
		Body:  fmt.Sprintf(`%q can now use code flower through the tunnel. Unpair it with "codeflower tunnel revoke".`, device.Label),
	})
	return device, ""
}

type announcement struct {
	at time.Time
	ip string
}

// announce tells the user when a device uses the tunnel and from where: at
// most hourly per device, unless it comes from another address.
func (c *connector) announce(device Device, ip string) {
	c.mu.Lock()
	last, seen := c.announced[device.Key]
	if seen && last.ip == ip && time.Since(last.at) < accessNoticeEvery {
		c.mu.Unlock()
		return
	}
	c.announced[device.Key] = announcement{at: time.Now(), ip: ip}
	c.mu.Unlock()
	body := fmt.Sprintf("%q is using code flower through the tunnel.", device.Label)
	if ip != "" {
		body = fmt.Sprintf("%q connected from %s.", device.Label, ip)
	}
	fmt.Fprintf(c.options.Log, "tunnel: %s\n", body)
	c.options.Notify(Notice{Title: "Remote access to code flower", Body: body})
}

// trusted are the headers the client reads to scope a remote request.
func trusted(device Device) http.Header {
	header := http.Header{}
	header.Set(DeviceHeader, url.QueryEscape(device.Label))
	if len(device.Workers) == 0 {
		header.Set(WorkersHeader, "*")
	} else {
		header.Set(WorkersHeader, strings.Join(device.Workers, ","))
	}
	return header
}

var hopByHop = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Connection": true, "Transfer-Encoding": true, "Upgrade": true,
	"Te": true, "Trailer": true, "Host": true, "Cookie": true, "Content-Length": true,
}

func (c *connector) serveHTTP(ctx context.Context, s *stream, open OpenRequest) {
	var body bytes.Buffer
	for {
		kind, data, err := s.receive(ctx)
		if err != nil {
			return
		}
		if kind == KindEnd {
			break
		}
		if kind != KindData || body.Len()+len(data) > maxBody {
			s.ws.Close(1009, "Request body too large or malformed")
			return
		}
		body.Write(data)
	}
	ctx, cancel := context.WithTimeout(ctx, requestLimit)
	defer cancel()
	method := open.Method
	if method == "" {
		method = http.MethodGet
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://codeflower"+open.Path, &body)
	if err != nil {
		s.sendJSON(ctx, KindClose, CloseFrame{Code: 1002, Reason: "Bad request"})
		return
	}
	for name, value := range open.Headers {
		canonical := http.CanonicalHeaderKey(name)
		if hopByHop[canonical] || canonical == DeviceHeader || canonical == WorkersHeader {
			continue
		}
		request.Header.Set(canonical, value)
	}
	for name, values := range trusted(s.device) {
		request.Header[name] = values
	}
	response, err := c.app.Do(request)
	if err != nil {
		s.sendJSON(ctx, KindClose, CloseFrame{Code: 1011, Reason: "code flower did not answer"})
		return
	}
	defer response.Body.Close()
	head := Head{Status: response.StatusCode, Headers: map[string]string{}}
	for name, values := range response.Header {
		if !hopByHop[name] && name != "Set-Cookie" {
			head.Headers[name] = strings.Join(values, ", ")
		}
	}
	if err := s.sendJSON(ctx, KindHead, head); err != nil {
		return
	}
	buffer := make([]byte, MaxChunk)
	for {
		n, err := response.Body.Read(buffer)
		if n > 0 {
			if s.send(ctx, Frame(KindData, buffer[:n])) != nil {
				return
			}
		}
		if err != nil {
			break
		}
	}
	if s.send(ctx, Frame(KindEnd, nil)) == nil {
		s.ws.Close(websocket.StatusNormalClosure, "")
	}
}

var acpPath = regexp.MustCompile(`^/api/workers/[^/]+/acp$`)

func (c *connector) serveWebSocket(ctx context.Context, s *stream, open OpenRequest) {
	if !acpPath.MatchString(open.Path) {
		s.sendJSON(ctx, KindClose, CloseFrame{Code: 1008, Reason: "Only chats are reachable this way"})
		s.ws.Close(1008, "")
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	app, response, err := websocket.Dial(ctx, "ws://codeflower"+open.Path, &websocket.DialOptions{HTTPClient: c.app, HTTPHeader: trusted(s.device)})
	if err != nil {
		code, reason := 1011, "Could not reach the sandbox"
		if response != nil && response.StatusCode == http.StatusForbidden {
			code, reason = 4403, "This device may not use this sandbox"
		} else if response != nil && response.StatusCode == http.StatusNotFound {
			code, reason = 4404, "No such sandbox"
		}
		s.sendJSON(ctx, KindClose, CloseFrame{Code: code, Reason: reason})
		s.ws.Close(websocket.StatusNormalClosure, "")
		return
	}
	defer app.CloseNow()
	app.SetReadLimit(MaxMessage)
	if s.send(ctx, Frame(KindAccepted, []byte("{}"))) != nil {
		return
	}

	activity := make(chan struct{}, 1)
	touch := func() {
		select {
		case activity <- struct{}{}:
		default:
		}
	}
	go func() {
		timer := time.NewTimer(idleLimit)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-activity:
				timer.Reset(idleLimit)
			case <-timer.C:
				s.sendJSON(ctx, KindClose, CloseFrame{Code: 4408, Reason: "Closed after 30 idle minutes"})
				cancel()
				return
			}
		}
	}()

	// App → browser.
	go func() {
		defer cancel()
		for {
			kind, data, err := app.Read(ctx)
			if err != nil {
				code := int(websocket.CloseStatus(err))
				if code <= 0 {
					code = 1000
				}
				s.sendJSON(ctx, KindClose, CloseFrame{Code: code})
				return
			}
			touch()
			for _, frame := range Parts(kind == websocket.MessageBinary, data) {
				if s.send(ctx, frame) != nil {
					return
				}
			}
		}
	}()

	// Browser → app.
	var message []byte
	for {
		kind, data, err := s.receive(ctx)
		if err != nil {
			return
		}
		touch()
		switch kind {
		case KindTextPart, KindBinaryPart:
			if len(message)+len(data) > MaxMessage {
				s.ws.Close(1009, "Message too large")
				return
			}
			message = append(message, data...)
		case KindText, KindBinary:
			whole := append(message, data...)
			message = nil
			messageType := websocket.MessageText
			if kind == KindBinary {
				messageType = websocket.MessageBinary
			}
			if err := app.Write(ctx, messageType, whole); err != nil {
				return
			}
		case KindClose:
			var frame CloseFrame
			_ = json.Unmarshal(data, &frame)
			app.Close(websocket.StatusCode(validCloseCode(frame.Code)), frame.Reason)
			s.ws.Close(websocket.StatusNormalClosure, "")
			return
		default:
			s.ws.Close(1002, "Unexpected frame")
			return
		}
	}
}

func validCloseCode(code int) int {
	if code == 1000 || (code >= 3000 && code <= 4999) {
		return code
	}
	return 1000
}
