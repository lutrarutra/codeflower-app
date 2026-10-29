package client

import (
	"path/filepath"
	"testing"
)

func TestTheWindowRemembersNetworkIsolation(t *testing.T) {
	store := &settingsStore{path: filepath.Join(t.TempDir(), "launcher.json")}
	store.save(Settings{Agent: "claude", IsolateNetwork: true})
	if !store.load().IsolateNetwork {
		t.Fatal("network isolation was forgotten")
	}
}

func TestSystemSaysWhereIsolationCannotWork(t *testing.T) {
	_, server := testServer(t)
	response, data := authorized(t, server, "GET", "/api/system", "")
	if response.StatusCode != 200 {
		t.Fatalf("status %d", response.StatusCode)
	}
	if _, ok := data["isolation"].(map[string]any); !ok {
		t.Fatalf("no isolation map: %v", data["isolation"])
	}
}
