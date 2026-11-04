package sandbox

import (
	"reflect"
	"slices"
	"testing"

	"codeflower-app/internal/config"
)

func TestMCPServersJSON(t *testing.T) {
	servers := []config.MCPServer{
		{Name: "docs", URL: "https://mcp.example.org/mcp", Headers: map[string]string{"X-B": "2", "Authorization": "Bearer t"}},
		{Name: "events", URL: "http://events.example.org:8080/sse", Transport: "sse"},
		{Name: "files", Command: "files-mcp", Args: []string{"--root", "/workspace"}, Env: map[string]string{"TOKEN": "t"}},
	}
	want := `[{"type":"http","name":"docs","url":"https://mcp.example.org/mcp","headers":[{"name":"Authorization","value":"Bearer t"},{"name":"X-B","value":"2"}]},` +
		`{"type":"sse","name":"events","url":"http://events.example.org:8080/sse"},` +
		`{"name":"files","command":"files-mcp","args":["--root","/workspace"],"env":[{"name":"TOKEN","value":"t"}]}]`
	if got := MCPServersJSON(servers); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if got := MCPHosts(servers); !reflect.DeepEqual(got, []string{"mcp.example.org", "events.example.org:8080"}) {
		t.Fatalf("got %v", got)
	}
	worker := Worker{Agent: "codex", Task: "task-1", MCPServers: servers}
	hosts := worker.providerHosts()
	if hosts[len(hosts)-1] != "events.example.org:8080" || len(hosts) != len(ProviderHosts["codex"])+2 {
		t.Fatalf("got %v", hosts)
	}
	// A sandbox for windows reaches every agent's services, each once.
	worker.Task = ""
	if hosts := worker.providerHosts(); !slices.Contains(hosts, "api.anthropic.com") || !slices.Contains(hosts, "api.deepseek.com") ||
		len(hosts) != len(ProviderHostsOf(Agents))+2 {
		t.Fatalf("got %v", hosts)
	}
}
