package client

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"codeflower-app/internal/sandbox"
)

func TestSandboxesStartedOnThisComputerAreListedWithoutTheirLine(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stops the run with SIGTERM")
	}
	server, page := testServer(t)
	address, _ := fakeBridge(t, "bridge-token")
	connection, err := parseAddress(address)
	if err != nil {
		t.Fatal(err)
	}

	// `codeflower interactive` in a terminal: its process, its run's
	// registration, and the connection file its bridge writes later.
	process := exec.Command("sleep", "60")
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	ended := make(chan struct{})
	go func() { process.Wait(); close(ended) }()
	t.Cleanup(func() { process.Process.Kill() })
	connectionFile := filepath.Join(t.TempDir(), "instance.json")
	terminal := sandbox.Running{
		ID: "0123456789ab", ConnectionFile: connectionFile, Project: "study", State: "/projects/study",
		Agent: "claude", Engine: "docker", Kind: "worker", PID: process.Process.Pid, Started: time.Now(),
	}
	// This client's own sandboxes are listed as started, not again as local.
	own := sandbox.Running{ID: "fedcba987654", Kind: "worker", PID: os.Getpid()}

	server.syncLocal([]sandbox.Running{terminal, own})
	workers := listed(t, page)
	if len(workers) != 1 || workers[0]["source"] != "local" || workers[0]["status"] != "starting" ||
		workers[0]["project"] != "study" || workers[0]["label"] != "Claude Code" {
		t.Fatalf("before the bridge listens: %v", workers)
	}
	id := workers[0]["id"].(string)

	data, _ := json.Marshal(connection)
	os.WriteFile(connectionFile, data, 0o600)
	server.syncLocal([]sandbox.Running{terminal, own})
	waitFor(t, "the local sandbox to answer", func() bool {
		workers := listed(t, page)
		return len(workers) == 1 && workers[0]["status"] == "ready" && workers[0]["url"] == address
	})

	// Its line is not forgotten, it is stopped: the run gets SIGTERM.
	if response, _ := authorized(t, page, "DELETE", "/api/workers/"+id, "{}"); response.StatusCode != 409 {
		t.Fatalf("forgetting a local sandbox: %d", response.StatusCode)
	}
	if response, data := authorized(t, page, "POST", "/api/workers/"+id+"/stop", "{}"); response.StatusCode != 200 || data["status"] != "stopping" {
		t.Fatalf("stop: %d %v", response.StatusCode, data)
	}
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the run did not get SIGTERM")
	}

	// Once the run has ended (and unregistered), the list drops it.
	server.syncLocal(nil)
	if workers := listed(t, page); len(workers) != 0 {
		t.Fatalf("after the run ended: %v", workers)
	}
}

func TestLocalTasksAreListedWithoutAChat(t *testing.T) {
	server, page := testServer(t)
	server.syncLocal([]sandbox.Running{{ID: "task-run-1", Kind: "task", PID: os.Getpid() + 100000, Agent: "codex", Project: "nightly"}})
	workers := listed(t, page)
	if len(workers) != 1 || workers[0]["status"] != "task" || workers[0]["kind"] != "task" || !strings.HasPrefix(workers[0]["id"].(string), "local-") {
		t.Fatalf("%v", workers)
	}
}
