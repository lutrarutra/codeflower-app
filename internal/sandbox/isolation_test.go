package sandbox

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/network"
)

// socketDir is short, as socket paths are limited to about 100 bytes. Tests
// that need it are skipped on Windows, where network isolation is refused.
func socketDir(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("network isolation needs Linux")
	}
	dir, err := os.MkdirTemp("/tmp", "cf-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func writeAllowlist(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	// The allowlist notices changes by modification time and size.
	later := time.Now().Add(time.Duration(len(content)) * time.Second)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
}

func TestAllowlistRulesMatchWholeNamesAndPorts(t *testing.T) {
	text := `
# comment
PyPI.org.            # trailing dot and case
*.hf.co
example.org:8443
10.1.2.3
not a host
*.10.0.0.1
example.net:http
`
	if _, problems := parseAllowlist(text); len(problems) != 3 {
		t.Fatalf("problems = %v", problems)
	}
	path := filepath.Join(tempDir(t), "hosts.txt")
	writeAllowlist(t, path, text)
	var logs []string
	list := newAllowlist(path, nil, func(message string) { logs = append(logs, message) })
	if len(logs) != 3 {
		t.Errorf("the bad lines were not reported: %v", logs)
	}
	for _, check := range []struct {
		host string
		port int
		want bool
	}{
		{"pypi.org", 443, true},
		{"pypi.org", 80, true},
		{"pypi.org", 22, false},
		{"evilpypi.org", 443, false},
		{"files.pypi.org", 443, false},
		{"cdn.hf.co", 443, true},
		{"a.b.hf.co", 443, true},
		{"hf.co", 443, false},
		{"evilhf.co", 443, false},
		{"example.org", 8443, true},
		{"example.org", 443, false},
		{"10.1.2.3", 443, true},
		{"10.1.2.4", 443, false},
	} {
		if allowed, _ := list.allows(check.host, check.port); allowed != check.want {
			t.Errorf("allows(%s, %d) = %v, want %v", check.host, check.port, allowed, check.want)
		}
	}
	if host, ok := normalizeHost("Api.Anthropic.COM."); !ok || host != "api.anthropic.com" {
		t.Errorf("normalizeHost = %q, %v", host, ok)
	}
	for _, bad := range []string{"", "a b", "a/b", "user@host", "a..b"} {
		if _, ok := normalizeHost(bad); ok {
			t.Errorf("normalizeHost accepted %q", bad)
		}
	}
}

func TestAllowlistPresetsExpandToTheirHosts(t *testing.T) {
	rules, problems := parseAllowlist("@pypi\n@NCBI   # case does not matter\n@nonsense\n")
	if len(problems) != 1 || !strings.Contains(problems[0], "@huggingface") {
		t.Fatalf("problems = %v", problems)
	}
	var hosts []string
	for _, rule := range rules {
		hosts = append(hosts, rule.String())
	}
	if got := strings.Join(hosts, " "); got != "pypi.org files.pythonhosted.org *.ncbi.nlm.nih.gov" {
		t.Errorf("rules = %s", got)
	}
	for _, preset := range AllowlistPresets {
		for _, host := range preset.Hosts {
			if _, err := parseHostRule(host); err != nil {
				t.Errorf("preset %s: %v", preset.Name, err)
			}
		}
		if !strings.Contains(allowlistHeader(), "# @"+preset.Name+" ") {
			t.Errorf("the header does not list @%s", preset.Name)
		}
	}
	// The header lists the presets commented out: a new file allows nothing.
	if rules, problems := parseAllowlist(allowlistHeader()); len(rules) != 0 || len(problems) != 0 {
		t.Errorf("the header allows %v, problems %v", rules, problems)
	}
}

func TestAllowlistIsReadAgainWhenItChanges(t *testing.T) {
	path := filepath.Join(tempDir(t), "hosts.txt")
	writeAllowlist(t, path, "pypi.org\n")
	list := newAllowlist(path, ProviderHosts["claude"], func(string) {})
	if allowed, byUser := list.allows("pypi.org", 443); !allowed || !byUser {
		t.Fatal("pypi.org is not allowed by the file")
	}
	if allowed, byUser := list.allows("api.anthropic.com", 443); !allowed || byUser {
		t.Fatal("the provider's host is not allowed by the launcher")
	}
	writeAllowlist(t, path, "conda.anaconda.org\n")
	if allowed, _ := list.allows("pypi.org", 443); allowed {
		t.Fatal("a removed host is still allowed")
	}
	if allowed, _ := list.allows("conda.anaconda.org", 443); !allowed {
		t.Fatal("an added host is not allowed")
	}
	os.Remove(path)
	if allowed, _ := list.allows("conda.anaconda.org", 443); allowed {
		t.Fatal("a deleted file still allows hosts")
	}
}

func TestPrepareAllowlistWritesTheHeaderOnce(t *testing.T) {
	path := filepath.Join(tempDir(t), "project.allowed-hosts.txt")
	got, err := PrepareAllowlist(path)
	if err != nil || got != path {
		t.Fatalf("PrepareAllowlist = %q, %v", got, err)
	}
	content, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(content), "# Hosts a network-isolated") {
		t.Fatalf("no header: %q", content)
	}
	if rules, problems := parseAllowlist(string(content)); len(rules) != 0 || len(problems) != 0 {
		t.Fatalf("the header is not all comments: %v %v", rules, problems)
	}
	os.WriteFile(path, []byte("pypi.org\n"), 0o600)
	if _, err := PrepareAllowlist(path); err != nil {
		t.Fatal(err)
	}
	if content, _ := os.ReadFile(path); string(content) != "pypi.org\n" {
		t.Fatalf("the user's file was changed: %q", content)
	}
	if _, err := PrepareAllowlist(filepath.Dir(path)); err == nil {
		t.Fatal("a folder was accepted as the allowlist")
	}
	if got := DefaultAllowlist("/home/ana/.codeflower/projects/study42/"); got != filepath.FromSlash("/home/ana/.codeflower/projects/study42.allowed-hosts.txt") {
		t.Fatalf("DefaultAllowlist = %q", got)
	}
}

// testProxy runs an egress proxy with a fake resolver on a Unix socket.
type testProxy struct {
	socket  string
	logs    []string
	lookups []string
	mutex   sync.Mutex
}

func startTestProxy(t *testing.T, allowed string, resolve map[string]string, upstream *url.URL) *testProxy {
	t.Helper()
	dir := socketDir(t)
	path := filepath.Join(dir, "hosts.txt")
	writeAllowlist(t, path, allowed)
	test := &testProxy{socket: filepath.Join(dir, egressSocket)}
	log := func(message string) {
		test.mutex.Lock()
		defer test.mutex.Unlock()
		test.logs = append(test.logs, message)
	}
	proxy := newEgressProxy(newAllowlist(path, []string{"api.example.com"}, log), log)
	proxy.lookup = func(_ context.Context, host string) ([]net.IPAddr, error) {
		test.mutex.Lock()
		test.lookups = append(test.lookups, host)
		test.mutex.Unlock()
		if ip, ok := resolve[host]; ok {
			return []net.IPAddr{{IP: net.ParseIP(ip)}}, nil
		}
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	proxy.upstream = func(*url.URL) (*url.URL, error) { return upstream, nil }
	listener, err := net.Listen("unix", test.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				proxy.handle(conn)
			}()
		}
	}()
	return test
}

// connect sends CONNECT and returns the status line and the open connection.
func (p *testProxy) connect(t *testing.T, target string) (int, string, net.Conn) {
	t.Helper()
	conn, err := net.Dial("unix", p.socket)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(conn, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\n\r\n")
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		conn.Close()
		return response.StatusCode, string(body), nil
	}
	return response.StatusCode, "", conn
}

func echoServer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				io.Copy(conn, conn)
			}()
		}
	}()
	return listener.Addr().String()
}

func TestProxyTunnelsOnlyToAllowedHosts(t *testing.T) {
	echo := echoServer(t)
	_, port, _ := net.SplitHostPort(echo)
	proxy := startTestProxy(t, "local.test:"+port+"\n", map[string]string{"local.test": "127.0.0.1"}, nil)

	status, _, conn := proxy.connect(t, "local.test:"+port)
	if status != http.StatusOK {
		t.Fatalf("allowed host: status %d", status)
	}
	io.WriteString(conn, "hello")
	reply := make([]byte, 5)
	if _, err := io.ReadFull(conn, reply); err != nil || string(reply) != "hello" {
		t.Fatalf("tunnel echoed %q, %v", reply, err)
	}
	conn.Close()

	status, body, _ := proxy.connect(t, "exfil.example.org:443")
	if status != http.StatusForbidden || !strings.Contains(body, "exfil.example.org:443 is not on the allowlist") {
		t.Fatalf("refused host: %d %q", status, body)
	}
	if slices.Contains(proxy.lookups, "exfil.example.org") {
		t.Fatal("a refused name was looked up")
	}
	if len(proxy.logs) == 0 || !strings.Contains(proxy.logs[len(proxy.logs)-1], "refused exfil.example.org:443") {
		t.Fatalf("the refusal was not logged: %v", proxy.logs)
	}

	// The allowed port only.
	if status, _, _ := proxy.connect(t, "local.test:443"); status != http.StatusForbidden {
		t.Fatalf("another port of an allowed host: status %d", status)
	}
}

func TestProxyRefusesProviderNamesPointingAtPrivateAddresses(t *testing.T) {
	proxy := startTestProxy(t, "", map[string]string{"api.example.com": "169.254.169.254"}, nil)
	status, body, _ := proxy.connect(t, "api.example.com:443")
	if status != http.StatusForbidden || !strings.Contains(body, "private address") {
		t.Fatalf("status %d %q", status, body)
	}
	// IP literals are hosts like any other: refused unless listed.
	if status, _, _ := proxy.connect(t, "127.0.0.1:22"); status != http.StatusForbidden {
		t.Fatalf("IP literal: status %d", status)
	}
}

func TestProxyAnswersWithTheAllowlist(t *testing.T) {
	proxy := startTestProxy(t, "pypi.org\n*.hf.co\n", nil, nil)
	conn, err := net.Dial("unix", proxy.socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	io.WriteString(conn, "GET /allowlist HTTP/1.1\r\nHost: codeflower\r\n\r\n")
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	for _, want := range []string{`"agent":["api.example.com"]`, `"allowed":["pypi.org","*.hf.co"]`, `hosts.txt"`, `"name":"pypi"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("missing %s in %s", want, body)
		}
	}
}

func TestProxyChainsThroughTheHostsProxy(t *testing.T) {
	// The site's proxy: answers CONNECT, then echoes.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	requested := make(chan string, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		request, err := http.ReadRequest(reader)
		if err != nil {
			return
		}
		requested <- request.Method + " " + request.Host + " " + request.Header.Get("Proxy-Authorization")
		io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n")
		io.Copy(conn, reader)
	}()
	upstream := &url.URL{Scheme: "http", Host: listener.Addr().String(), User: url.UserPassword("ana", "secret")}
	// Compute nodes often cannot resolve names themselves: the site's proxy does.
	proxy := startTestProxy(t, "pypi.org\n", nil, upstream)
	status, _, conn := proxy.connect(t, "pypi.org:443")
	if status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	defer conn.Close()
	if got := <-requested; got != "CONNECT pypi.org:443 Basic YW5hOnNlY3JldA==" {
		t.Fatalf("the site's proxy got %q", got)
	}
	io.WriteString(conn, "ping")
	reply := make([]byte, 4)
	if _, err := io.ReadFull(conn, reply); err != nil || string(reply) != "ping" {
		t.Fatalf("tunnel echoed %q, %v", reply, err)
	}
}

func TestGatewayForwardsVSCodeToTheBridgeAndCleansUp(t *testing.T) {
	dir := socketDir(t)
	bridge, err := net.Listen("unix", filepath.Join(dir, bridgeSocket))
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	go func() {
		for {
			conn, err := bridge.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				io.Copy(conn, conn)
			}()
		}
	}()
	list := newAllowlist(filepath.Join(dir, "missing.txt"), nil, func(string) {})
	gate, err := startGateway(dir, "127.0.0.1:0", list, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(dir, egressSocket)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("egress socket: %v %v", info, err)
	}
	if err := gate.bridgeAnswers(); err != nil {
		t.Fatal(err)
	}
	client, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", itoa(gate.Port())))
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(client, "hello")
	reply := make([]byte, 5)
	if _, err := io.ReadFull(client, reply); err != nil || string(reply) != "hello" {
		t.Fatalf("forward echoed %q, %v", reply, err)
	}

	gate.Close()
	if _, err := client.Read(reply); err == nil {
		t.Fatal("an open connection survived stopping")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the network folder is left behind: %v", err)
	}
}

func TestNetDirectoryIsPrivateAndStaleOnesAreRemoved(t *testing.T) {
	base := socketDir(t)
	t.Setenv("XDG_RUNTIME_DIR", base)
	stale := filepath.Join(base, netDirectoryPrefix+"deadbeef")
	os.Mkdir(stale, 0o700)
	os.WriteFile(filepath.Join(stale, egressSocket), nil, 0o600)
	unrelated := filepath.Join(base, netDirectoryPrefix+"someone")
	os.Mkdir(unrelated, 0o700)
	os.WriteFile(filepath.Join(unrelated, "notes.txt"), nil, 0o600)

	dir, err := newNetDirectory("0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("network folder: %v %v", info, err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("a stale folder was kept")
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatal("a folder that is not ours was removed")
	}
}

func TestIsolatedEnginesHaveNoNetworkAndNoPorts(t *testing.T) {
	dir := tempDir(t)
	net := "/tmp/codeflower-net-01234567"

	args, err := ApptainerCommand("apptainer", "x.sif", dir, dir, nil, net)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--net --network none --bind "+net+":"+ContainerNet) {
		t.Fatalf("apptainer: %v", args)
	}

	config, host, err := DockerConfig(DockerRun{Image: "img", State: dir, Port: 7781, NetDirectory: net})
	if err != nil {
		t.Fatal(err)
	}
	if host.NetworkMode != network.NetworkNone || len(host.PortBindings) != 0 || len(config.ExposedPorts) != 0 {
		t.Fatalf("docker: mode %q, ports %v", host.NetworkMode, host.PortBindings)
	}
	if last := host.Mounts[len(host.Mounts)-1]; last.Source != net || last.Target != ContainerNet {
		t.Fatalf("docker mount: %+v", last)
	}

	args, err = PodmanArgs("podman", PodmanRun{Name: "n", Image: "img", State: dir, Port: 7781, NetDirectory: net})
	if err != nil {
		t.Fatal(err)
	}
	joined = strings.Join(args, " ")
	if !strings.Contains(joined, "--network none") || strings.Contains(joined, "--publish") ||
		!strings.Contains(joined, "source="+net+",target="+ContainerNet) {
		t.Fatalf("podman: %v", args)
	}

	values := map[string]string{}
	isolatedEnvironment(values)
	if values["HTTPS_PROXY"] != SandboxProxy || values["CODEFLOWER_NETWORK"] != "isolated" ||
		values["CODEFLOWER_LISTEN_SOCKET"] != ContainerNet+"/"+bridgeSocket {
		t.Fatalf("environment: %v", values)
	}
	if _, err := NewBind(dir, ContainerNet+"/x", false); err == nil {
		t.Fatal("a bind over the network folder was accepted")
	}
}

func TestSkillsFoldersMountReadOnly(t *testing.T) {
	base := tempDir(t)
	skills := filepath.Join(base, "my-skills")
	os.Mkdir(skills, 0o755)
	binds, err := Folders{Skills: []string{skills}}.Binds(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(binds) != 1 || binds[0].Target != SkillsRoot+"/my-skills" || !binds[0].ReadOnly {
		t.Fatalf("binds = %+v", binds)
	}
	if _, err := (Folders{Code: []string{skills}, Skills: []string{skills}}).Binds(nil); err == nil ||
		!strings.Contains(err.Error(), "as a skills folder") {
		t.Fatalf("a folder chosen twice: %v", err)
	}
	if err := checkTargets([]Bind{{Source: base, Target: SkillsRoot}}); err == nil {
		t.Fatal("mounting over /data/skills was accepted")
	}
}

func TestIsolationNeedsLinuxForContainerEngines(t *testing.T) {
	if err := IsolationProblem(EngineApptainer); err != nil {
		t.Fatalf("apptainer: %v", err)
	}
	for _, engine := range []string{EngineDocker, EnginePodman} {
		err := IsolationProblem(engine)
		if (runtime.GOOS == "linux") != (err == nil) {
			t.Errorf("%s on %s: %v", engine, runtime.GOOS, err)
		}
	}
}

func TestGatewayForATaskHasOnlyTheEgressProxy(t *testing.T) {
	dir := socketDir(t)
	list := newAllowlist(filepath.Join(dir, "missing.txt"), nil, func(string) {})
	gate, err := startGateway(dir, "", list, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if gate.Port() != 0 {
		t.Fatalf("a task's gateway listens on port %d", gate.Port())
	}
	if _, err := os.Stat(filepath.Join(dir, egressSocket)); err != nil {
		t.Fatal(err)
	}
	gate.Close()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the network folder is left behind: %v", err)
	}
}
