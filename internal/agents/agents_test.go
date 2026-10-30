package agents

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestTheDefaultAgentComesFirst(t *testing.T) {
	if ids := IDs(); len(ids) < 3 || ids[0] != "claude" {
		t.Fatalf("agents %v", ids)
	}
	if Label("codex") != "Codex" || Label("nope") != "nope" {
		t.Fatal("labels")
	}
}

// The copies here must be the sandbox's own, which the bridge reads; run
// `make agents` after changing them there.
func TestTheDescriptorsAreTheSandboxes(t *testing.T) {
	source := filepath.Join("..", "..", "..", "sandbox", "dsh-plugins", "acp-ws", "src", "agents")
	folders, err := filepath.Glob(filepath.Join(source, "*", "agent.json"))
	if err != nil || len(folders) == 0 {
		t.Skip("no sandbox checkout next to the app")
	}
	if len(folders) != len(IDs()) {
		t.Fatalf("the sandbox has %d agents, the app %d: run make agents", len(folders), len(IDs()))
	}
	for _, file := range folders {
		id := filepath.Base(filepath.Dir(file))
		want, _ := os.ReadFile(file)
		got, err := files.ReadFile("descriptors/" + id + ".json")
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("descriptors/%s.json differs from the sandbox's: run make agents", id)
		}
	}
}
