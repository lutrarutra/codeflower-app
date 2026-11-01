package config

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
)

// MCPServer is a Model Context Protocol server the agent of every sandbox may
// use: a command that runs in the sandbox (stdio), or a server at a URL.
type MCPServer struct {
	// Name is its key under mcp_servers.
	Name    string            `yaml:"-"`
	Command string            `yaml:"command"`
	Args    []string          `yaml:"args"`
	Env     map[string]string `yaml:"env"`
	URL     string            `yaml:"url"`
	// Transport of a server at a URL: http (streamable HTTP, the default) or
	// sse, which Codex and dsh do not support.
	Transport string            `yaml:"transport"`
	Headers   map[string]string `yaml:"headers"`
}

// The names every agent accepts as they are (dsh rewrites others).
var mcpName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// Host is the host of a server at a URL, which an isolated sandbox may reach,
// with its port when the URL names one (the proxy allows 443 and 80 else).
func (s MCPServer) Host() string {
	if s.URL == "" {
		return ""
	}
	parsed, err := url.Parse(s.URL)
	if err != nil {
		return ""
	}
	if parsed.Port() != "" {
		return parsed.Host
	}
	return parsed.Hostname()
}

func (s MCPServer) check() error {
	if !mcpName.MatchString(s.Name) {
		return fmt.Errorf("mcp_servers: %q: names are 1 to 32 letters, digits, - or _", s.Name)
	}
	where := "mcp_servers." + s.Name
	switch {
	case (s.Command == "") == (s.URL == ""):
		return fmt.Errorf("%s: needs either a command or a url", where)
	case s.Command != "" && (s.Transport != "" || len(s.Headers) > 0):
		return fmt.Errorf("%s: transport and headers are for a url", where)
	case s.URL != "" && (len(s.Args) > 0 || len(s.Env) > 0):
		return fmt.Errorf("%s: args and env are for a command", where)
	case s.Transport != "" && s.Transport != "http" && s.Transport != "sse":
		return fmt.Errorf("%s: transport is http or sse", where)
	}
	if s.URL != "" {
		parsed, err := url.Parse(s.URL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
			return fmt.Errorf("%s: url must be an http or https address", where)
		}
	}
	return nil
}

// MCPServers are the configured servers, by name.
func (c *Config) MCPServers() []MCPServer {
	if c == nil {
		return nil
	}
	servers := make([]MCPServer, 0, len(c.MCP))
	for name, server := range c.MCP {
		server.Name = name
		servers = append(servers, server)
	}
	sort.Slice(servers, func(i, j int) bool { return servers[i].Name < servers[j].Name })
	return servers
}

func (c *Config) checkMCP() error {
	for _, server := range c.MCPServers() {
		if err := server.check(); err != nil {
			return fmt.Errorf("%s: %w", c.Path, err)
		}
	}
	return nil
}

// hasMCPSecrets: env and headers usually carry tokens.
func (c *Config) hasMCPSecrets() bool {
	for _, server := range c.MCP {
		if len(server.Env) > 0 || len(server.Headers) > 0 {
			return true
		}
	}
	return false
}
