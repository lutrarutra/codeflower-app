package config

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	"codeflower-app/internal/agents"
)

// PluginEntry is one of an agent's own plugins under plugins: (docs/plugins.md).
type PluginEntry struct {
	// Source is agent-neutral: marketplace:NAME@CATALOG, github:OWNER/REPO[#REF],
	// git:URL[#REF], npm:PACKAGE@VERSION, url:https://….zip|.tgz[#sha256=…] or
	// path:/absolute; each agent takes the kinds its descriptor names.
	Source string `yaml:"source" json:"source"`
	// Enabled defaults to true; a project can switch an entry off.
	Enabled *bool `yaml:"enabled" json:"-"`
}

// IsEnabled is whether the entry is on.
func (e PluginEntry) IsEnabled() bool {
	return e.Enabled == nil || *e.Enabled
}

// The same rules as the bridge's plugins.js.
var (
	pluginName    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	githubOwner   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)
	githubRepo    = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	pluginRef     = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)
	gitURL        = regexp.MustCompile(`^(https://|ssh://|git@)[^\s#]+$`)
	npmPackage    = regexp.MustCompile(`^(@[a-z0-9-~][a-z0-9-._~]*/)?[a-z0-9-~][a-z0-9-._~]{0,213}$`)
	npmVersion    = regexp.MustCompile(`^[A-Za-z0-9.^~<>=*+-]{1,64}$`)
	archiveURL    = regexp.MustCompile(`^https://[^\s#]+\.(zip|tgz|tar\.gz)$`)
	sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// PluginSourceKinds are every kind of source.
var PluginSourceKinds = []string{"marketplace", "github", "git", "npm", "url", "path"}

func checkRef(ref, text string) error {
	if !pluginRef.MatchString(ref) || strings.HasPrefix(ref, "-") || slices.Contains(strings.Split(ref, "/"), "..") {
		return fmt.Errorf("invalid ref in %s", text)
	}
	return nil
}

// ParsePluginSource checks a source and returns its kind and canonical text.
func ParsePluginSource(text string) (kind, source string, err error) {
	text = strings.TrimSpace(text)
	if text == "" || strings.ContainsAny(text, "\x00\r\n") {
		return "", "", fmt.Errorf("a plugin source is required")
	}
	kind, rest, _ := strings.Cut(text, ":")
	body, fragment, hasFragment := strings.Cut(rest, "#")
	switch kind {
	case "marketplace":
		at := strings.LastIndex(rest, "@")
		if at < 1 || !pluginName.MatchString(rest[:at]) || !pluginName.MatchString(rest[at+1:]) {
			return "", "", fmt.Errorf("expected marketplace:NAME@CATALOG, not %s", text)
		}
		return kind, "marketplace:" + rest, nil
	case "github":
		parts := strings.Split(body, "/")
		if len(parts) != 2 || !githubOwner.MatchString(parts[0]) || !githubRepo.MatchString(parts[1]) || parts[1] == "." || parts[1] == ".." {
			return "", "", fmt.Errorf("expected github:OWNER/REPO[#REF], not %s", text)
		}
		source = "github:" + strings.ToLower(parts[0]+"/"+strings.TrimSuffix(parts[1], ".git"))
		if hasFragment {
			if err := checkRef(fragment, text); err != nil {
				return "", "", err
			}
			source += "#" + fragment
		}
		return kind, source, nil
	case "git":
		if !gitURL.MatchString(body) {
			return "", "", fmt.Errorf("expected git:https://…, git:ssh://… or git:git@host:path, not %s", text)
		}
		source = "git:" + body
		if hasFragment {
			if err := checkRef(fragment, text); err != nil {
				return "", "", err
			}
			source += "#" + fragment
		}
		return kind, source, nil
	case "npm":
		at := strings.LastIndex(rest, "@")
		if at < 1 || !npmPackage.MatchString(rest[:at]) || !npmVersion.MatchString(rest[at+1:]) {
			return "", "", fmt.Errorf("expected npm:PACKAGE@VERSION, not %s", text)
		}
		return kind, "npm:" + rest, nil
	case "url":
		if !archiveURL.MatchString(body) {
			return "", "", fmt.Errorf("expected url:https://….zip, .tgz or .tar.gz, not %s", text)
		}
		source = "url:" + body
		if hasFragment {
			sum, ok := strings.CutPrefix(fragment, "sha256=")
			sum = strings.ToLower(sum)
			if !ok || !sha256Pattern.MatchString(sum) {
				return "", "", fmt.Errorf("expected #sha256=<64 hex digits> in %s", text)
			}
			source += "#sha256=" + sum
		}
		return kind, source, nil
	case "path":
		if !strings.HasPrefix(rest, "/") {
			return "", "", fmt.Errorf("expected path:/absolute/path, not %s", text)
		}
		return kind, "path:" + path.Clean(rest), nil
	}
	return "", "", fmt.Errorf("a plugin source starts with %s:, not %s", strings.Join(PluginSourceKinds, ":, "), text)
}

// AgentPlugins are the config's plugins for one agent, with canonical sources.
func (c *Config) AgentPlugins(agent string) []PluginEntry {
	if c == nil {
		return nil
	}
	var result []PluginEntry
	for _, entry := range c.Plugins[agent] {
		if _, source, err := ParsePluginSource(entry.Source); err == nil {
			entry.Source = source
			result = append(result, entry)
		}
	}
	return result
}

// checkPlugins refuses unknown agents, bad sources and kinds an agent does not take.
func (c *Config) checkPlugins() error {
	for agent, entries := range c.Plugins {
		descriptor, ok := agents.Get(agent)
		if !ok {
			return fmt.Errorf("%s: unknown agent %q under plugins: use %s", c.Path, agent, strings.Join(agents.IDs(), ", "))
		}
		if len(descriptor.Plugins.Kinds) == 0 && len(entries) > 0 {
			return fmt.Errorf("%s: plugins.%s: %s has no plugins in code flower yet", c.Path, agent, descriptor.Name)
		}
		seen := map[string]bool{}
		for _, entry := range entries {
			kind, source, err := ParsePluginSource(entry.Source)
			if err != nil {
				return fmt.Errorf("%s: plugins.%s: %w", c.Path, agent, err)
			}
			if !slices.Contains(descriptor.Plugins.Kinds, kind) {
				return fmt.Errorf("%s: plugins.%s: %s takes plugins from %s:, not %s:", c.Path, agent, descriptor.Name,
					strings.Join(descriptor.Plugins.Kinds, ":, "), kind)
			}
			if seen[source] {
				return fmt.Errorf("%s: plugins.%s: %s is listed twice", c.Path, agent, source)
			}
			seen[source] = true
		}
	}
	return nil
}
