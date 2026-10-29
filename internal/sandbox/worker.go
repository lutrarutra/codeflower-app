package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Agents a worker can run.
var Agents = []string{"claude", "codex", "dsh"}

var agentLabels = map[string]string{
	"claude": "Claude Code",
	"codex":  "Codex",
	"dsh":    "DeepSeek Harness",
}

// AgentLabel is the agent's product name.
func AgentLabel(agent string) string {
	if label, ok := agentLabels[agent]; ok {
		return label
	}
	return agent
}

// Where the user's folders appear in the sandbox, each in a subfolder named
// after it, e.g. /data/input/study42.
const (
	InputsRoot  = "/data/input"
	OutputsRoot = "/data/output"
	CodeRoot    = "/data/code"
)

// NewInstanceID returns a random UUIDv4.
func NewInstanceID() string {
	bytes := make([]byte, 16)
	_, _ = rand.Read(bytes)
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	text := hex.EncodeToString(bytes)
	return text[0:8] + "-" + text[8:12] + "-" + text[12:16] + "-" + text[16:20] + "-" + text[20:32]
}

// NewToken returns the secret VS Code connects with.
func NewToken() string {
	bytes := make([]byte, 32)
	_, _ = rand.Read(bytes)
	return base64.RawURLEncoding.EncodeToString(bytes)
}

// Worker is one sandbox with its agent bridge.
type Worker struct {
	Agent  string
	Engine EngineStatus
	Image  string
	// State is the prepared, absolute project directory.
	State         string
	Binds         []Bind
	ListenHost    string
	AdvertiseHost string
	Port          int
	// InspectPort publishes the bridge's Node inspector (development, Docker).
	InspectPort int
	Stdout      io.Writer
	Stderr      io.Writer
}

// Connection is what the bridge writes once it listens: the line VS Code
// connects with.
type Connection struct {
	ID    string `json:"id"`
	URL   string `json:"url"`
	Host  string `json:"host"`
	Port  int    `json:"port"`
	Agent string `json:"agent"`
}

// ResolveImage checks that an Apptainer image file exists; Docker and Podman
// image names are checked by the engine when it starts.
func ResolveImage(image, engine string) (string, error) {
	if engine != EngineApptainer {
		if image == "" {
			return DefaultImage, nil
		}
		return image, nil
	}
	if image == "" {
		return "", fmt.Errorf("Apptainer needs the sandbox image file: --image codeflower-sandbox.sif (or set CODEFLOWER_IMAGE)")
	}
	path, err := filepath.Abs(expandHome(image))
	if err != nil {
		return "", err
	}
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return "", fmt.Errorf("apptainer image does not exist: %s", path)
	}
	return path, nil
}

// Run starts the worker and blocks until it exits; cancelling ctx stops it
// gracefully. ready is called once with the connection line.
func (w Worker) Run(ctx context.Context, ready func(Connection)) (int, error) {
	if w.Stdout == nil {
		w.Stdout = os.Stdout
	}
	if w.Stderr == nil {
		w.Stderr = os.Stderr
	}
	if err := checkTargets(w.Binds); err != nil {
		return 1, err
	}
	containerized := w.Engine.Name != EngineApptainer
	port := w.Port
	if containerized && port == 0 {
		// Docker and Podman publish a fixed port, so choose it up front.
		var err error
		if port, err = FreePort(); err != nil {
			return 1, err
		}
	}
	advertise := w.AdvertiseHost
	if advertise == "" {
		if containerized {
			advertise = "127.0.0.1"
		} else {
			advertise = DefaultAdvertiseHost()
		}
	}
	listen := w.ListenHost
	if listen == "" {
		listen = "0.0.0.0"
	}
	instanceID := NewInstanceID()
	values := WorkerEnvironment(WorkerSettings{
		Agent:         w.Agent,
		InstanceID:    instanceID,
		Token:         NewToken(),
		ListenHost:    listen,
		AdvertiseHost: advertise,
		Port:          port,
		HostState:     w.State,
		Engine:        w.Engine.Name,
		SlurmJobID:    os.Getenv("SLURM_JOB_ID"),
		LauncherPID:   os.Getpid(),
		Binds:         w.Binds,
	})
	if w.InspectPort > 0 {
		values["CODEFLOWER_BRIDGE_INSPECT"] = fmt.Sprintf("0.0.0.0:%d", BridgeInspectPort)
	}
	connectionFile := filepath.Join(w.State, "instances", instanceID+".json")
	defer os.Remove(connectionFile)

	if ready != nil {
		watching, stopWatching := context.WithCancel(ctx)
		defer stopWatching()
		go watchConnection(watching, connectionFile, ready)
	}

	name := "codeflower-" + instanceID[:8]
	switch w.Engine.Name {
	case EngineDocker:
		return RunDocker(ctx, DockerRun{
			Name: name, Image: w.Image, State: w.State, Binds: w.Binds, Env: values,
			Port: port, InspectPort: w.InspectPort, Stdout: w.Stdout, Stderr: w.Stderr,
		})
	case EnginePodman:
		return RunPodman(ctx, w.Engine.Executable, PodmanRun{
			Name: name, Image: w.Image, State: w.State, Binds: w.Binds, Env: values,
			Port: port, Stdout: w.Stdout, Stderr: w.Stderr,
		})
	case EngineApptainer:
		args, err := ApptainerCommand(w.Engine.Executable, w.Image, w.State, w.Binds)
		if err != nil {
			return 1, err
		}
		return RunApptainer(ctx, args, values, w.Stdout, w.Stderr)
	}
	return 1, fmt.Errorf("unknown engine %q", w.Engine.Name)
}

func watchConnection(ctx context.Context, path string, ready func(Connection)) {
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			var connection Connection
			if json.Unmarshal(data, &connection) != nil || connection.URL == "" {
				continue
			}
			ready(connection)
			return
		}
	}
}
