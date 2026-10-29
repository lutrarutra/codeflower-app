package sandbox

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ProviderHosts are the services each agent signs in to and works with; a
// network-isolated sandbox can always reach them.
var ProviderHosts = map[string][]string{
	"claude": {"api.anthropic.com", "claude.ai", "platform.claude.com", "console.anthropic.com"},
	"codex":  {"api.openai.com", "auth.openai.com", "chatgpt.com"},
	"dsh":    {"api.deepseek.com"},
}

const allowlistHeader = `# Hosts a network-isolated code flower sandbox may reach, one per line:
#   example.org          example.org on ports 443 and 80
#   *.example.org        every subdomain of example.org (not example.org itself)
#   example.org:8443     example.org on port 8443 only
# The agent's own service is always allowed. Every host here is also a place
# the agent could send your data to: prefer hosts that only serve downloads.
# Changes apply immediately, without restarting the sandbox.
`

// DefaultAllowlist is the project's allowlist file: next to its state
// directory, so the sandbox, which can write its state, cannot change it.
func DefaultAllowlist(state string) string {
	return filepath.Clean(state) + ".allowed-hosts.txt"
}

// PrepareAllowlist returns the file's absolute path, creating it with an
// explanatory header if it does not exist.
func PrepareAllowlist(path string) (string, error) {
	path, err := filepath.Abs(expandHome(path))
	if err != nil {
		return "", err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		if info, err := os.Stat(path); err != nil || info.IsDir() {
			return "", fmt.Errorf("allowlist must be a file: %s", path)
		}
		return path, nil
	}
	if err != nil {
		return "", fmt.Errorf("allowlist: %w", err)
	}
	defer file.Close()
	if _, err := file.WriteString(allowlistHeader); err != nil {
		return "", fmt.Errorf("allowlist: %w", err)
	}
	return path, nil
}

// hostRule allows one host, or with wildcard every subdomain of it, on port
// (0: 443 and 80).
type hostRule struct {
	host     string
	wildcard bool
	port     int
}

func (r hostRule) String() string {
	text := r.host
	if r.wildcard {
		text = "*." + text
	}
	if r.port != 0 {
		text = net.JoinHostPort(text, strconv.Itoa(r.port))
	}
	return text
}

var hostCharacters = regexp.MustCompile(`^[a-z0-9_-]+(\.[a-z0-9_-]+)*$`)

// normalizeHost lower-cases a host name and drops a trailing dot; IP
// addresses are returned in canonical form. It reports false for anything
// that is neither.
func normalizeHost(host string) (string, bool) {
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if ip := net.ParseIP(host); ip != nil {
		return ip.String(), true
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if len(host) > 253 || !hostCharacters.MatchString(host) {
		return "", false
	}
	return host, true
}

func parseHostRule(line string) (hostRule, error) {
	var rule hostRule
	host := line
	if strings.HasPrefix(line, "[") || strings.Count(line, ":") == 1 {
		name, port, err := net.SplitHostPort(line)
		if err != nil {
			return rule, err
		}
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return rule, fmt.Errorf("bad port %q", port)
		}
		host, rule.port = name, number
	}
	if rest, ok := strings.CutPrefix(host, "*."); ok {
		host, rule.wildcard = rest, true
	}
	name, ok := normalizeHost(host)
	if !ok || (rule.wildcard && net.ParseIP(name) != nil) {
		return rule, fmt.Errorf("not a host name: %q", line)
	}
	rule.host = name
	return rule, nil
}

// parseAllowlist reads one rule per line; # starts a comment. Lines that are
// not rules are returned as problems and otherwise ignored.
func parseAllowlist(text string) ([]hostRule, []string) {
	var rules []hostRule
	var problems []string
	for number, line := range strings.Split(text, "\n") {
		if index := strings.Index(line, "#"); index >= 0 {
			line = line[:index]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		rule, err := parseHostRule(line)
		if err != nil {
			problems = append(problems, fmt.Sprintf("line %d: %v", number+1, err))
			continue
		}
		rules = append(rules, rule)
	}
	return rules, problems
}

func (r hostRule) matches(host string, port int) bool {
	if r.port == 0 {
		if port != 443 && port != 80 {
			return false
		}
	} else if port != r.port {
		return false
	}
	if r.wildcard {
		return strings.HasSuffix(host, "."+r.host)
	}
	return host == r.host
}

// allowlist is the provider's hosts plus the user's file, which is read
// again whenever it changes.
type allowlist struct {
	path     string
	provider []hostRule
	log      func(string)

	mutex    sync.Mutex
	modified time.Time
	size     int64
	content  []byte
	user     []hostRule
}

func newAllowlist(path string, provider []string, log func(string)) *allowlist {
	list := &allowlist{path: path, log: log}
	for _, host := range provider {
		if rule, err := parseHostRule(host); err == nil {
			list.provider = append(list.provider, rule)
		}
	}
	list.refresh()
	return list
}

// refresh rereads the file when its size or modification time changed. A
// missing file allows only the provider's hosts.
func (a *allowlist) refresh() {
	info, err := os.Stat(a.path)
	if err != nil {
		if a.content != nil || a.modified.IsZero() {
			a.log(fmt.Sprintf("network: cannot read the allowlist %s: %v; only the agent's service is allowed", a.path, err))
		}
		a.content, a.user, a.modified = nil, nil, time.Unix(1, 0)
		return
	}
	if info.ModTime().Equal(a.modified) && info.Size() == a.size {
		return
	}
	content, err := os.ReadFile(a.path)
	if err != nil {
		return
	}
	a.modified, a.size = info.ModTime(), info.Size()
	if bytes.Equal(content, a.content) {
		return
	}
	rules, problems := parseAllowlist(string(content))
	for _, problem := range problems {
		a.log(fmt.Sprintf("network: ignoring %s in %s", problem, a.path))
	}
	a.content, a.user = content, rules
}

// allows reports whether host:port may be reached, and whether the user's
// file allows it (only the user can allow hosts on private networks).
func (a *allowlist) allows(host string, port int) (allowed, byUser bool) {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	a.refresh()
	for _, rule := range a.user {
		if rule.matches(host, port) {
			return true, true
		}
	}
	for _, rule := range a.provider {
		if rule.matches(host, port) {
			return true, false
		}
	}
	return false, false
}

func (a *allowlist) rules() (provider, user []string) {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	a.refresh()
	provider, user = []string{}, []string{}
	for _, rule := range a.provider {
		provider = append(provider, rule.String())
	}
	for _, rule := range a.user {
		user = append(user, rule.String())
	}
	return provider, user
}
