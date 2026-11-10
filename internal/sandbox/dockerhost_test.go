package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/docker/docker/client"
)

func writeDockerContext(t *testing.T, directory, current, name, host string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, "config.json"), []byte(`{"currentContext":"`+current+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(name))
	meta := filepath.Join(directory, "contexts", "meta", hex.EncodeToString(digest[:]))
	if err := os.MkdirAll(meta, 0o700); err != nil {
		t.Fatal(err)
	}
	data := `{"Name":"` + name + `","Endpoints":{"docker":{"Host":"` + host + `","SkipTLSVerify":false}}}`
	if err := os.WriteFile(filepath.Join(meta, "meta.json"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDockerUsesTheCurrentContextLikeTheDockerCommand(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("DOCKER_CONFIG", directory)
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	writeDockerContext(t, directory, "desktop-linux", "desktop-linux", "unix:///Users/me/.docker/run/docker.sock")

	if host := dockerContextHost(); host != "unix:///Users/me/.docker/run/docker.sock" {
		t.Fatalf("context host = %q", host)
	}
	if socket := dockerSocket(); socket != "/Users/me/.docker/run/docker.sock" {
		t.Fatalf("socket = %q", socket)
	}
	docker, err := newDockerClient()
	if err != nil {
		t.Fatal(err)
	}
	if docker.DaemonHost() != "unix:///Users/me/.docker/run/docker.sock" {
		t.Fatalf("client host = %q", docker.DaemonHost())
	}

	t.Setenv("DOCKER_HOST", "unix:///elsewhere.sock")
	docker, err = newDockerClient()
	if err != nil {
		t.Fatal(err)
	}
	if docker.DaemonHost() != "unix:///elsewhere.sock" {
		t.Fatalf("DOCKER_HOST should win, got %q", docker.DaemonHost())
	}

	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "default")
	if host := dockerContextHost(); host != "" {
		t.Fatalf("default context should use the built-in host, got %q", host)
	}
	if socket := dockerSocket(); socket != "/var/run/docker.sock" {
		t.Fatalf("socket = %q", socket)
	}
}

func TestDockerWithoutContextsUsesTheDefaultSocket(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	if host := dockerContextHost(); host != "" {
		t.Fatalf("host = %q", host)
	}
	docker, err := newDockerClient()
	if err != nil {
		t.Fatal(err)
	}
	if docker.DaemonHost() != client.DefaultDockerHost {
		t.Fatalf("client host = %q", docker.DaemonHost())
	}
}
