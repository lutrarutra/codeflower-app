package client

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// connectionPath is where the bridge accepts ACP over WebSocket
// (CONNECTION_PATH in the bridge's server.js).
const connectionPath = "/acp"

// relay passes a page's WebSocket through to a worker's bridge. Browsers
// cannot send the bridge's Authorization header, and the token must not reach
// the page, so the client adds it here. Frames and close codes pass through
// untouched: the protocol is the page's business, as it is the extension's.
func (s *Server) relay(w http.ResponseWriter, r *http.Request) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		writeError(w, http.StatusBadRequest, errors.New("expected a WebSocket"))
		return
	}
	// The cookie is strict, but a WebSocket is not bound by CORS: refuse pages
	// of other origins outright.
	origin, err := url.Parse(r.Header.Get("Origin"))
	if err != nil || origin.Host != r.Host {
		writeError(w, http.StatusForbidden, errors.New("wrong origin"))
		return
	}
	item := s.find(r.PathValue("id"))
	if item == nil {
		writeError(w, http.StatusNotFound, errors.New("no such sandbox"))
		return
	}
	item.mu.Lock()
	connection := item.connection
	item.mu.Unlock()
	if connection == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("the sandbox is not running"))
		return
	}
	target := &url.URL{Scheme: "http", Host: net.JoinHostPort(connection.Host, strconv.Itoa(connection.Port)), Path: connectionPath}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.Out.URL = target
			request.Out.Host = ""
			request.Out.Header.Del("Cookie")
			request.Out.Header.Del("Origin")
			request.Out.Header.Set("Authorization", "Bearer "+connection.Token)
		},
		// Straight to the node: a cluster's HTTPS_PROXY cannot reach compute nodes.
		Transport: &http.Transport{
			Proxy:             nil,
			DialContext:       item.relays.dial,
			DisableKeepAlives: true,
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeError(w, http.StatusBadGateway, errors.New("could not reach the sandbox"))
		},
	}
	proxy.ServeHTTP(w, r)
}

// relays are the bridge connections open for a worker's pages, so that
// stopping or forgetting the worker, or quitting, closes them: the HTTP
// server does not close connections it handed over to a WebSocket.
type relays struct {
	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

var dialer = &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}

func (set *relays) dial(ctx context.Context, network, address string) (net.Conn, error) {
	conn, err := dialer.DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	tracked := &trackedConn{Conn: conn, set: set}
	set.mu.Lock()
	if set.conns == nil {
		set.conns = map[net.Conn]struct{}{}
	}
	set.conns[tracked] = struct{}{}
	set.mu.Unlock()
	return tracked, nil
}

func (set *relays) count() int {
	set.mu.Lock()
	defer set.mu.Unlock()
	return len(set.conns)
}

func (set *relays) closeAll() {
	set.mu.Lock()
	conns := make([]net.Conn, 0, len(set.conns))
	for conn := range set.conns {
		conns = append(conns, conn)
	}
	set.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

type trackedConn struct {
	net.Conn
	set  *relays
	once sync.Once
}

func (c *trackedConn) Close() error {
	c.once.Do(func() {
		c.set.mu.Lock()
		delete(c.set.conns, c)
		c.set.mu.Unlock()
	})
	return c.Conn.Close()
}
