package sandbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Running is a sandbox that runs on this computer, as its run registers it
// for the app to find: whoever started it (the app, `codeflower run`,
// `sandbox` or `task` in a terminal) and with whichever --config.
type Running struct {
	ID string `json:"id"`
	// ConnectionFile is where the bridge writes its connection line; a task
	// has none.
	ConnectionFile string `json:"connectionFile"`
	Project        string `json:"project"`
	State          string `json:"state"`
	Agent          string `json:"agent"`
	Engine         string `json:"engine"`
	// Kind is worker, run (a worker with an initial prompt) or task.
	Kind string `json:"kind"`
	// PID is the codeflower process that runs the sandbox and stops it on SIGTERM.
	PID     int       `json:"pid"`
	Started time.Time `json:"started"`
	// NoTunnel keeps the sandbox from browsers paired through the tunnel
	// (`--no-tunnel`); the computer's own page still lists it.
	NoTunnel bool `json:"noTunnel,omitempty"`
	// Host is the computer it runs on: a home folder shared between
	// computers (a cluster's) holds the runs of every one, and PID means
	// something only on Host.
	Host string `json:"host,omitempty"`
}

// RunningDirectory holds one file per running sandbox, in the system's config
// folder next to the app's workers.json. It is a variable for tests.
var RunningDirectory = func() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "codeflower", "running")
}

// register lists the run in RunningDirectory until the returned function
// removes it. Failing to register only hides the run from the app.
func register(running Running) func() {
	dir := RunningDirectory()
	if dir == "" || os.MkdirAll(dir, 0o700) != nil {
		return func() {}
	}
	running.Host, _ = os.Hostname()
	data, err := json.MarshalIndent(running, "", "  ")
	if err != nil {
		return func() {}
	}
	path := filepath.Join(dir, running.ID+".json")
	temporary, err := os.CreateTemp(dir, ".running-*.json")
	if err != nil {
		return func() {}
	}
	defer os.Remove(temporary.Name())
	_, err = temporary.Write(append(data, '\n'))
	if closeErr := temporary.Close(); err != nil || closeErr != nil {
		return func() {}
	}
	if os.Rename(temporary.Name(), path) != nil {
		return func() {}
	}
	return func() { os.Remove(path) }
}

// ListRunning reads the runs registered on this computer. A run whose
// process is gone left its file behind (killed, or the computer slept through
// a crash); those files are removed. Runs on other computers that share the
// folder are left alone.
func ListRunning() []Running {
	dir := RunningDirectory()
	if dir == "" {
		return nil
	}
	host, _ := os.Hostname()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var result []Running
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue
		}
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var running Running
		if json.Unmarshal(data, &running) != nil || running.ID == "" {
			continue
		}
		if running.Host != "" && running.Host != host {
			continue
		}
		if !processAlive(running.PID) {
			os.Remove(path)
			continue
		}
		result = append(result, running)
	}
	return result
}

// ReadConnection reads the line a running sandbox's bridge wrote, once it
// listens.
func (r Running) ReadConnection() (Connection, bool) {
	if r.ConnectionFile == "" {
		return Connection{}, false
	}
	data, err := os.ReadFile(r.ConnectionFile)
	if err != nil {
		return Connection{}, false
	}
	var connection Connection
	if json.Unmarshal(data, &connection) != nil || connection.URL == "" {
		return Connection{}, false
	}
	return connection, true
}
