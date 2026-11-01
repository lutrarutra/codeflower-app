package config

import (
	"strings"
	"testing"
)

func TestPluginSourcesFollowTheBridgesRules(t *testing.T) {
	for text, want := range map[string]string{
		"marketplace:commit-commands@claude-plugins-official":             "marketplace:commit-commands@claude-plugins-official",
		" github:My-Org/My-Plugin.git#v1.2.0 ":                            "github:my-org/my-plugin#v1.2.0",
		"git:https://example.org/x.git#main":                              "git:https://example.org/x.git#main",
		"npm:@scope/pkg@1.0.4":                                            "npm:@scope/pkg@1.0.4",
		"url:https://example.org/p.zip#sha256=" + strings.Repeat("A", 64): "url:https://example.org/p.zip#sha256=" + strings.Repeat("a", 64),
		"path:/data/plugins/mine/":                                        "path:/data/plugins/mine",
	} {
		if _, got, err := ParsePluginSource(text); err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", text, got, err, want)
		}
	}
	for _, text := range []string{"commit-commands", "marketplace:x", "github:org", "github:org/repo#../x", "git:git://example.org/x",
		"npm:pkg", "url:http://example.org/p.zip", "url:https://example.org/p.exe", "path:relative", "ftp:x"} {
		if _, _, err := ParsePluginSource(text); err == nil {
			t.Errorf("accepted %q", text)
		}
	}
}

func TestPluginsAreCheckedAgainstTheAgents(t *testing.T) {
	directory := t.TempDir()
	write(t, directory, `
plugins:
  claude:
    - source: marketplace:commit-commands@claude-plugins-official
    - source: github:Org/Mine
      enabled: false
`, 0o644)
	loaded, err := Load(directory)
	if err != nil {
		t.Fatal(err)
	}
	entries := loaded.AgentPlugins("claude")
	if len(entries) != 2 || entries[1].Source != "github:org/mine" || entries[1].IsEnabled() || !entries[0].IsEnabled() {
		t.Fatalf("got %+v", entries)
	}
	for content, want := range map[string]string{
		"plugins:\n  gemini: [{source: 'github:a/b'}]\n":                         "unknown agent",
		"plugins:\n  claude: [{source: 'npm:x@1'}]\n":                            "not npm:",
		"plugins:\n  claude: [{source: 'nonsense'}]\n":                           "plugin source starts with",
		"plugins:\n  claude: [{source: 'github:a/b'}, {source: 'github:A/B'}]\n": "listed twice",
		"plugins:\n  codex: [{source: 'github:a/b'}]\n":                          "no plugins in code flower yet",
	} {
		directory := t.TempDir()
		write(t, directory, content, 0o600)
		if _, err := Load(directory); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", content, err, want)
		}
	}
}
