package sandbox

import (
	"encoding/json"
	"sort"

	"codeflower-app/internal/config"
)

// MCPServersVariable carries the config's MCP servers to the bridge, as a
// JSON list in ACP's McpServer shape; the bridge removes it from its
// environment and gives the servers to the agent in every session.
const MCPServersVariable = "CODEFLOWER_MCP_SERVERS"

type acpPair struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type acpServer struct {
	Type    string    `json:"type,omitempty"`
	Name    string    `json:"name"`
	Command string    `json:"command,omitempty"`
	Args    []string  `json:"args,omitempty"`
	Env     []acpPair `json:"env,omitempty"`
	URL     string    `json:"url,omitempty"`
	Headers []acpPair `json:"headers,omitempty"`
}

func acpPairs(values map[string]string) []acpPair {
	pairs := make([]acpPair, 0, len(values))
	for name, value := range values {
		pairs = append(pairs, acpPair{Name: name, Value: value})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].Name < pairs[j].Name })
	return pairs
}

// MCPServersJSON is the value of MCPServersVariable.
func MCPServersJSON(servers []config.MCPServer) string {
	list := make([]acpServer, 0, len(servers))
	for _, server := range servers {
		if server.URL != "" {
			transport := server.Transport
			if transport == "" {
				transport = "http"
			}
			list = append(list, acpServer{Type: transport, Name: server.Name, URL: server.URL, Headers: acpPairs(server.Headers)})
			continue
		}
		list = append(list, acpServer{Name: server.Name, Command: server.Command, Args: server.Args, Env: acpPairs(server.Env)})
	}
	data, _ := json.Marshal(list)
	return string(data)
}

// MCPHosts are the hosts of the servers at a URL, which an isolated sandbox
// reaches like the agents' services.
func MCPHosts(servers []config.MCPServer) []string {
	var hosts []string
	for _, server := range servers {
		if host := server.Host(); host != "" {
			hosts = append(hosts, host)
		}
	}
	return hosts
}
