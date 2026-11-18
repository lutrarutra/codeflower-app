package client

import (
	"encoding/base64"
	"os"
	"strings"
	"testing"
)

func TestDescribeTellsWhatARestartNeeds(t *testing.T) {
	directory := t.TempDir()
	if _, ok := Describe(directory); ok {
		t.Fatal("described a client before any started")
	}
	server, httpServer := testServer(t)
	page := httpServer.URL + "/?key=secret-key"
	recordInstance(directory, instance{URL: page, Version: "v0.40.0", Background: true, Args: []string{"--background", "--ui", "/tmp/page"}})
	found, ok := Describe(directory)
	if !ok || found.URL != page || found.Version != "v0.40.0" || !found.Background || strings.Join(found.Args, " ") != "--background --ui /tmp/page" ||
		found.Sandboxes != 0 || found.PID != os.Getpid() {
		t.Fatalf("described %+v %v", found, ok)
	}
	// What it started stops when it quits; what was added keeps running.
	addWorker(server, "started", sourceStarted, nil, 0)
	addWorker(server, "added", "added", nil, 0)
	if found, _ := Describe(directory); found.Sandboxes != 1 {
		t.Fatalf("sandboxes it started: %d", found.Sandboxes)
	}

	// A client from before v0.41.0 recorded neither: its version comes from its API.
	server.options.Version = "v0.39.0"
	recordInstance(directory, instance{URL: page})
	if found, ok := Describe(directory); !ok || found.Version != "v0.39.0" {
		t.Fatalf("an older client's version: %+v", found)
	}
}

func TestTheVersionEndpointSaysTheRelease(t *testing.T) {
	server, httpServer := testServer(t)
	server.options.Version = "v0.41.0"
	response, data := authorized(t, httpServer, "GET", "/api/version", "")
	if response.StatusCode != 200 || data["version"] != "v0.41.0" {
		t.Fatalf("%d %v", response.StatusCode, data)
	}
}

func TestARestartedClientInheritsItsSecretOnce(t *testing.T) {
	secret := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("s", 24)))
	t.Setenv(SecretVariable, secret)
	if got := inheritedSecret(); got != secret {
		t.Fatalf("inherited %q", got)
	}
	// Taken out of the environment, so the sandboxes it starts do not get it.
	if _, set := os.LookupEnv(SecretVariable); set {
		t.Fatal("the secret stayed in the environment")
	}
	for _, weak := range []string{"", "short", "not base64 !!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!"} {
		t.Setenv(SecretVariable, weak)
		if got := inheritedSecret(); got != "" {
			t.Fatalf("took %q", weak)
		}
	}
}
