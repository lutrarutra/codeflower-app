package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/docker/docker/client"
)

// newDockerClient connects to the engine the docker command would use.
// client.FromEnv reads only DOCKER_HOST and falls back to
// /var/run/docker.sock, but Docker Desktop on macOS (and on Linux, and
// rootless Docker or Colima) serves its socket elsewhere and points the
// command at it through the current Docker context.
func newDockerClient() (*client.Client, error) {
	options := []client.Opt{client.FromEnv, client.WithAPIVersionNegotiation()}
	if os.Getenv("DOCKER_HOST") == "" {
		if host := dockerContextHost(); host != "" {
			options = append(options, client.WithHost(host))
		}
	}
	return client.NewClientWithOpts(options...)
}

// dockerSocket is the local socket the client connects to, or "" when it
// is not a unix socket.
func dockerSocket() string {
	host := os.Getenv("DOCKER_HOST")
	if host == "" {
		host = dockerContextHost()
	}
	if host == "" {
		return "/var/run/docker.sock"
	}
	if socket, ok := strings.CutPrefix(host, "unix://"); ok {
		return socket
	}
	return ""
}

// dockerContextHost is the endpoint of the current Docker context, chosen
// by DOCKER_CONTEXT or the config's currentContext like the docker command
// does, or "" for the default context or when there is none.
func dockerContextHost() string {
	directory := os.Getenv("DOCKER_CONFIG")
	if directory == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		directory = filepath.Join(home, ".docker")
	}
	name := os.Getenv("DOCKER_CONTEXT")
	if name == "" {
		var config struct {
			CurrentContext string `json:"currentContext"`
		}
		if data, err := os.ReadFile(filepath.Join(directory, "config.json")); err == nil {
			_ = json.Unmarshal(data, &config)
		}
		name = config.CurrentContext
	}
	if name == "" || name == "default" {
		return ""
	}
	// The context store names each context's folder by its name's SHA-256.
	digest := sha256.Sum256([]byte(name))
	data, err := os.ReadFile(filepath.Join(directory, "contexts", "meta", hex.EncodeToString(digest[:]), "meta.json"))
	if err != nil {
		return ""
	}
	var meta struct {
		Endpoints map[string]struct {
			Host string `json:"Host"`
		} `json:"Endpoints"`
	}
	if json.Unmarshal(data, &meta) != nil {
		return ""
	}
	return meta.Endpoints["docker"].Host
}
