package sandbox

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A network-isolated sandbox has no network of its own. The launcher shares
// a folder with it holding two Unix sockets: the bridge listens on one, which
// the launcher forwards VS Code's connections to, and the launcher's proxy
// listens on the other, the sandbox's only way out. Nothing else on the
// machine sees either: the launcher opens no port for the proxy.
const (
	ContainerNet = "/codeflower-net"
	bridgeSocket = "bridge.sock"
	egressSocket = "egress.sock"
	// SandboxProxy is where the bridge offers the egress socket to the agent.
	SandboxProxy = "http://127.0.0.1:3128"
)

// Isolation cuts the sandbox off the network except for its agent's service
// and the hosts in the allowlist file.
type Isolation struct {
	// Allowlist is the absolute path of the user's allowlist file.
	Allowlist string
}

// isolatedEnvironment routes the sandbox's traffic through the launcher.
func isolatedEnvironment(values map[string]string) {
	for _, name := range []string{"HTTPS_PROXY", "HTTP_PROXY", "https_proxy", "http_proxy"} {
		values[name] = SandboxProxy
	}
	values["NO_PROXY"] = "localhost,127.0.0.1,::1"
	values["no_proxy"] = values["NO_PROXY"]
	// Node's own fetch ignores the variables above unless asked.
	values["NODE_USE_ENV_PROXY"] = "1"
	values["CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"] = "1"
	values["CODEFLOWER_NETWORK"] = "isolated"
	values["CODEFLOWER_LISTEN_SOCKET"] = ContainerNet + "/" + bridgeSocket
	values["CODEFLOWER_EGRESS_SOCKET"] = ContainerNet + "/" + egressSocket
}

// IsolationProblem says why an engine cannot isolate the network here, or
// returns nil. Docker and Podman outside Linux run in a VM, which cannot
// connect sockets shared with the launcher; Apptainer is checked when the
// sandbox starts, as sites decide whether users may isolate it.
func IsolationProblem(engine string) error {
	if engine != EngineApptainer && runtime.GOOS != "linux" {
		return fmt.Errorf("network isolation needs Linux: %s on %s runs in a VM that cannot share the launcher's sockets",
			engineLabel(engine), systemLabel())
	}
	return nil
}

func engineLabel(engine string) string {
	if engine == EnginePodman {
		return "Podman"
	}
	return "Docker"
}

func systemLabel() string {
	if runtime.GOOS == "darwin" {
		return "macOS"
	}
	if runtime.GOOS == "windows" {
		return "Windows"
	}
	return runtime.GOOS
}

const netDirectoryPrefix = "codeflower-net-"

// newNetDirectory makes the private folder shared with the sandbox. It is
// local, as sockets on network filesystems do not work, and short, as socket
// paths are limited to about 100 bytes.
func newNetDirectory(instanceID string) (string, error) {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = os.TempDir()
	}
	if len(base) > 60 && runtime.GOOS != "windows" {
		base = "/tmp"
	}
	removeStaleNetDirectories(base)
	directory := filepath.Join(base, netDirectoryPrefix+instanceID[:8])
	if err := os.Mkdir(directory, 0o700); err != nil {
		return "", fmt.Errorf("could not create the sandbox's network folder: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		os.Remove(directory)
		return "", err
	}
	return directory, nil
}

// removeStaleNetDirectories removes folders left by launchers that were
// killed: their proxy socket no longer answers.
func removeStaleNetDirectories(base string) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), netDirectoryPrefix) {
			continue
		}
		directory := filepath.Join(base, entry.Name())
		contents, err := os.ReadDir(directory)
		if err != nil {
			continue
		}
		ours := len(contents) > 0
		for _, item := range contents {
			if item.Name() != bridgeSocket && item.Name() != egressSocket {
				ours = false
			}
		}
		if !ours {
			continue
		}
		if conn, err := net.DialTimeout("unix", filepath.Join(directory, egressSocket), time.Second); err == nil {
			conn.Close()
			continue
		}
		_ = os.RemoveAll(directory)
	}
}

// connections tracks open connections so stopping closes them all.
type connections struct {
	mutex  sync.Mutex
	open   map[net.Conn]struct{}
	closed bool
}

func (c *connections) add(conn net.Conn) bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.closed {
		conn.Close()
		return false
	}
	if c.open == nil {
		c.open = map[net.Conn]struct{}{}
	}
	c.open[conn] = struct{}{}
	return true
}

func (c *connections) remove(conn net.Conn) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	delete(c.open, conn)
	conn.Close()
}

func (c *connections) closeAll() {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.closed = true
	for conn := range c.open {
		conn.Close()
	}
}

// pipe copies both ways until both sides finish. client may hold bytes
// already read from clientConn.
func pipe(clientConn net.Conn, client io.Reader, target net.Conn) {
	done := make(chan struct{}, 2)
	copyHalf := func(to net.Conn, from io.Reader) {
		_, _ = io.Copy(to, from)
		if half, ok := to.(interface{ CloseWrite() error }); ok {
			_ = half.CloseWrite()
		} else {
			to.Close()
		}
		done <- struct{}{}
	}
	go copyHalf(target, client)
	go copyHalf(clientConn, target)
	<-done
	<-done
}

// gateway is the launcher's side of an isolated sandbox.
type gateway struct {
	directory string
	ingress   net.Listener
	egress    net.Listener
	proxy     *egressProxy
	conns     connections
	stopped   sync.WaitGroup
}

// startGateway listens on ingressAddress for VS Code, unless it is empty (a
// task, which nobody connects to), and on the egress socket for the sandbox.
func startGateway(directory, ingressAddress string, list *allowlist, log func(string)) (*gateway, error) {
	var ingress net.Listener
	if ingressAddress != "" {
		var err error
		if ingress, err = net.Listen("tcp", ingressAddress); err != nil {
			return nil, fmt.Errorf("could not listen on %s: %w", ingressAddress, err)
		}
	}
	egress, err := net.Listen("unix", filepath.Join(directory, egressSocket))
	if err != nil {
		if ingress != nil {
			ingress.Close()
		}
		return nil, fmt.Errorf("could not start the sandbox's proxy: %w", err)
	}
	_ = os.Chmod(filepath.Join(directory, egressSocket), 0o600)
	g := &gateway{
		directory: directory,
		ingress:   ingress,
		egress:    egress,
		proxy:     newEgressProxy(list, log),
	}
	if ingress != nil {
		g.stopped.Add(1)
		go g.accept(ingress, g.forwardToBridge)
	}
	g.stopped.Add(1)
	go g.accept(egress, g.proxy.handle)
	return g, nil
}

// Port is the TCP port VS Code connects to, or 0 without one.
func (g *gateway) Port() int {
	if g.ingress == nil {
		return 0
	}
	return g.ingress.Addr().(*net.TCPAddr).Port
}

func (g *gateway) accept(listener net.Listener, handle func(net.Conn)) {
	defer g.stopped.Done()
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		if !g.conns.add(conn) {
			continue
		}
		go func() {
			defer g.conns.remove(conn)
			handle(conn)
		}()
	}
}

func (g *gateway) forwardToBridge(conn net.Conn) {
	bridge, err := net.DialTimeout("unix", filepath.Join(g.directory, bridgeSocket), 5*time.Second)
	if err != nil {
		return
	}
	if !g.conns.add(bridge) {
		return
	}
	defer g.conns.remove(bridge)
	pipe(conn, conn, bridge)
}

// bridgeAnswers checks that the launcher can reach the bridge's socket,
// which fails where the engine cannot share sockets with the host.
func (g *gateway) bridgeAnswers() error {
	conn, err := net.DialTimeout("unix", filepath.Join(g.directory, bridgeSocket), 5*time.Second)
	if err != nil {
		return err
	}
	return conn.Close()
}

// Close stops listening, closes every connection, and removes the folder.
func (g *gateway) Close() {
	if g.ingress != nil {
		g.ingress.Close()
	}
	g.egress.Close()
	g.conns.closeAll()
	g.stopped.Wait()
	_ = os.RemoveAll(g.directory)
}

// refusal is an error the proxy reports to the sandbox's client.
type refusal struct {
	status  int
	message string
}

func (r refusal) Error() string { return r.message }

// egressProxy is an HTTP proxy for CONNECT tunnels and plain HTTP that only
// reaches allowed hosts. It checks a name before looking it up, so a refused
// name never reaches a DNS server.
type egressProxy struct {
	allowlist *allowlist
	log       func(string)
	lookup    func(ctx context.Context, host string) ([]net.IPAddr, error)
	dial      func(ctx context.Context, network, address string) (net.Conn, error)
	// upstream is the host's own proxy for a URL, if it needs one.
	upstream func(target *url.URL) (*url.URL, error)
}

func newEgressProxy(list *allowlist, log func(string)) *egressProxy {
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	return &egressProxy{
		allowlist: list,
		log:       log,
		lookup:    net.DefaultResolver.LookupIPAddr,
		dial:      dialer.DialContext,
		upstream: func(target *url.URL) (*url.URL, error) {
			return http.ProxyFromEnvironment(&http.Request{URL: target})
		},
	}
}

func (p *egressProxy) handle(conn net.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	reader := bufio.NewReader(conn)
	request, err := http.ReadRequest(reader)
	if err != nil {
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	ctx := context.Background()

	switch {
	case request.Method == http.MethodConnect:
		host, port, err := splitHostPort(request.Host, 0)
		if err != nil {
			p.respond(conn, refusal{http.StatusBadRequest, "codeflower: CONNECT needs host:port"})
			return
		}
		target, err := p.connect(ctx, host, port, true)
		if err != nil {
			p.respond(conn, err)
			return
		}
		defer target.Close()
		if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
			return
		}
		pipe(conn, reader, target)

	case request.URL.IsAbs():
		if request.URL.Scheme != "http" {
			p.respond(conn, refusal{http.StatusBadRequest, "codeflower: only http:// requests and CONNECT tunnels are proxied"})
			return
		}
		host, port, err := splitHostPort(request.URL.Host, 80)
		if err != nil {
			p.respond(conn, refusal{http.StatusBadRequest, "codeflower: bad host " + request.URL.Host})
			return
		}
		target, err := p.connect(ctx, host, port, false)
		if err != nil {
			p.respond(conn, err)
			return
		}
		defer target.Close()
		request.Header.Del("Proxy-Connection")
		request.Header.Del("Proxy-Authorization")
		// One request per connection, so a client cannot reuse it for another host.
		request.Close = true
		if _, ok := target.(upstreamConn); ok {
			err = request.WriteProxy(target)
		} else {
			err = request.Write(target)
		}
		if err != nil {
			return
		}
		_, _ = io.Copy(conn, target)

	case request.Method == http.MethodGet && request.URL.Path == "/allowlist":
		provider, user := p.allowlist.rules()
		body, _ := json.Marshal(map[string]any{"file": p.allowlist.path, "agent": provider, "allowed": user,
			"presets": AllowlistPresets})
		writeResponse(conn, http.StatusOK, "application/json", string(body))

	default:
		p.respond(conn, refusal{http.StatusBadRequest, "codeflower: this is the sandbox's network proxy"})
	}
}

func (p *egressProxy) respond(conn net.Conn, err error) {
	var refused refusal
	if !errors.As(err, &refused) {
		refused = refusal{http.StatusBadGateway, "codeflower: " + err.Error()}
	}
	writeResponse(conn, refused.status, "text/plain; charset=utf-8", refused.message+"\n")
}

func writeResponse(conn net.Conn, status int, contentType, body string) {
	fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nContent-Type: %s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		status, http.StatusText(status), contentType, len(body), body)
}

func splitHostPort(value string, defaultPort int) (string, int, error) {
	host, portText, err := net.SplitHostPort(value)
	if err != nil {
		if defaultPort == 0 {
			return "", 0, err
		}
		host, portText = value, strconv.Itoa(defaultPort)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("bad port %q", portText)
	}
	return host, port, nil
}

// upstreamConn is a connection to the host's own proxy that expects a
// proxied HTTP request rather than tunnelled bytes.
type upstreamConn struct {
	net.Conn
}

// connect checks host:port against the allowlist and opens a connection to
// it, directly or through the host's proxy. tunnel is false for plain HTTP.
func (p *egressProxy) connect(ctx context.Context, rawHost string, port int, tunnel bool) (net.Conn, error) {
	host, ok := normalizeHost(rawHost)
	if !ok {
		return nil, refusal{http.StatusBadRequest, "codeflower: not a host name: " + rawHost}
	}
	address := net.JoinHostPort(host, strconv.Itoa(port))
	allowed, byUser := p.allowlist.allows(host, port)
	if !allowed {
		p.log(fmt.Sprintf("network: refused %s (not on the allowlist %s)", address, p.allowlist.path))
		return nil, refusal{http.StatusForbidden,
			fmt.Sprintf("codeflower: %s is not on the allowlist (%s)", address, p.allowlist.path)}
	}

	scheme := "https"
	if !tunnel {
		scheme = "http"
	}
	upstream, err := p.upstream(&url.URL{Scheme: scheme, Host: address})
	if err != nil {
		return nil, err
	}

	var addresses []net.IPAddr
	if ip := net.ParseIP(host); ip != nil {
		addresses = []net.IPAddr{{IP: ip}}
	} else if found, err := p.lookup(ctx, host); err == nil {
		addresses = found
	} else if upstream == nil {
		return nil, refusal{http.StatusBadGateway, fmt.Sprintf("codeflower: could not resolve %s: %v", host, err)}
	}
	// Only the user can allow hosts on private networks, such as a model
	// server on the cluster; an allowed name pointing there is refused.
	for _, found := range addresses {
		if !byUser && !public(found.IP) {
			p.log(fmt.Sprintf("network: refused %s (resolves to the private address %s)", address, found.IP))
			return nil, refusal{http.StatusForbidden,
				fmt.Sprintf("codeflower: %s resolves to the private address %s", host, found.IP)}
		}
	}

	if upstream != nil {
		return p.dialUpstream(ctx, upstream, address, tunnel)
	}
	var lastError error
	for _, found := range addresses {
		conn, err := p.dial(ctx, "tcp", net.JoinHostPort(found.IP.String(), strconv.Itoa(port)))
		if err == nil {
			return conn, nil
		}
		lastError = err
	}
	return nil, refusal{http.StatusBadGateway, fmt.Sprintf("codeflower: could not reach %s: %v", address, lastError)}
}

var sharedAddressSpace = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

func public(ip net.IP) bool {
	return !(sharedAddressSpace.Contains(ip) || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast())
}

// dialUpstream reaches address through the host's own proxy, as compute
// nodes often must.
func (p *egressProxy) dialUpstream(ctx context.Context, proxy *url.URL, address string, tunnel bool) (net.Conn, error) {
	if proxy.Scheme != "http" {
		return nil, fmt.Errorf("the host's proxy %s is not an http:// proxy", proxy.Redacted())
	}
	proxyAddress := proxy.Host
	if proxy.Port() == "" {
		proxyAddress = net.JoinHostPort(proxy.Hostname(), "80")
	}
	conn, err := p.dial(ctx, "tcp", proxyAddress)
	if err != nil {
		return nil, fmt.Errorf("could not reach the host's proxy %s: %w", proxy.Redacted(), err)
	}
	if !tunnel {
		return upstreamConn{conn}, nil
	}
	request := "CONNECT " + address + " HTTP/1.1\r\nHost: " + address + "\r\n"
	if proxy.User != nil {
		password, _ := proxy.User.Password()
		credentials := base64.StdEncoding.EncodeToString([]byte(proxy.User.Username() + ":" + password))
		request += "Proxy-Authorization: Basic " + credentials + "\r\n"
	}
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := io.WriteString(conn, request+"\r\n"); err != nil {
		conn.Close()
		return nil, err
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("the host's proxy did not answer: %w", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		conn.Close()
		return nil, fmt.Errorf("the host's proxy refused %s: %s", address, response.Status)
	}
	_ = conn.SetDeadline(time.Time{})
	if reader.Buffered() > 0 {
		conn.Close()
		return nil, fmt.Errorf("the host's proxy sent unexpected data")
	}
	return conn, nil
}
