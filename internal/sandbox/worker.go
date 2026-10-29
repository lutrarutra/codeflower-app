package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
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
	SkillsRoot  = "/data/skills"
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
// APIKeyVariable carries the API key of the config to the bridge, which
// removes it from its environment again.
const APIKeyVariable = "CODEFLOWER_API_KEY"

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
	// Isolation, when set, cuts the sandbox off the network except for what
	// the launcher's proxy allows.
	Isolation *Isolation
	// APIKey, when set, is given to the agent's bridge in the environment (never
	// in the command line); it stores the key in the agent's own files.
	APIKey string
	// Task, when set, is the id of a task in the project's tasks folder: the
	// agent works on it by itself, and nothing listens for VS Code.
	Task string
	// InitialPrompt, when set, is the id of a run prompt to send in a new
	// session while keeping the worker available to VS Code.
	InitialPrompt string
	// TaskInteractive enables terminal sign-in before the task when stdin is a terminal.
	TaskInteractive bool
	Stdin           io.Reader
	Stdout          io.Writer
	Stderr          io.Writer
}

// ErrImageWithoutTasks means the sandbox started a worker for VS Code instead
// of running its task: the image is older than tasks.
var ErrImageWithoutTasks = errors.New("this sandbox image cannot run tasks (it started a worker for VS Code instead); " +
	"update it with `codeflower pull`, or choose a newer --image")

// ErrImageWithoutInitialPrompt means the sandbox started a worker but ignored
// the run's initial prompt.
var ErrImageWithoutInitialPrompt = errors.New("this sandbox image cannot start a run with an initial prompt; " +
	"update it with `codeflower pull`, or choose a newer --image")

// TaskFile is where the sandbox finds a task's description.
func TaskFile(task string) string {
	return ContainerState + "/tasks/" + task + "/task.json"
}

// Connection is what the bridge writes once it listens: the line VS Code
// connects with.
type Connection struct {
	ID            string `json:"id"`
	URL           string `json:"url"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	Agent         string `json:"agent"`
	InitialPrompt bool   `json:"initialPrompt,omitempty"`
	// Token also authorizes the bridge's /status.
	Token string `json:"token"`
}

// ResolveImage checks that an Apptainer image file the user named exists;
// Docker and Podman image names, and the published image's files, are checked
// by CheckImage.
func ResolveImage(image, engine string) (string, error) {
	if tag, ok := publishedTag(image); ok {
		if engine == EngineApptainer {
			return publishedSIF(tag), nil
		}
		return ImageRepository + ":" + tag, nil
	}
	if engine != EngineApptainer {
		return image, nil
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
	if w.Isolation != nil && w.InspectPort > 0 {
		return 1, fmt.Errorf("the inspector cannot be published from a network-isolated sandbox")
	}
	containerized := w.Engine.Name != EngineApptainer
	port := w.Port
	if containerized && port == 0 && w.Isolation == nil && w.Task == "" {
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

	// An isolated sandbox has no network: the launcher holds VS Code's port
	// and is its only way out, for as long as it runs.
	var netDirectory string
	var gate *gateway
	if w.Isolation != nil {
		if w.Engine.Name == EngineApptainer {
			if err := apptainerAllowsIsolation(ctx, w.Engine.Executable, w.Image); err != nil {
				return 1, err
			}
		}
		var err error
		if netDirectory, err = newNetDirectory(instanceID); err != nil {
			return 1, err
		}
		ingress := ""
		if w.Task == "" {
			ingressHost := listen
			if containerized {
				ingressHost = "127.0.0.1"
			}
			ingress = net.JoinHostPort(ingressHost, itoa(port))
		}
		log := func(message string) { fmt.Fprintln(w.Stderr, message) }
		list := newAllowlist(w.Isolation.Allowlist, ProviderHosts[w.Agent], log)
		gate, err = startGateway(netDirectory, ingress, list, log)
		if err != nil {
			os.RemoveAll(netDirectory)
			return 1, err
		}
		defer gate.Close()
		port = gate.Port()
	}

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
	if w.APIKey != "" {
		values[APIKeyVariable] = w.APIKey
	}
	if w.InspectPort > 0 {
		values["CODEFLOWER_BRIDGE_INSPECT"] = fmt.Sprintf("0.0.0.0:%d", BridgeInspectPort)
	}
	if gate != nil {
		isolatedEnvironment(values)
	}
	if w.Task != "" {
		values["CODEFLOWER_TASK_FILE"] = TaskFile(w.Task)
		if w.TaskInteractive {
			values["CODEFLOWER_TASK_INTERACTIVE"] = "1"
		}
	}
	if w.InitialPrompt != "" {
		values["CODEFLOWER_INITIAL_PROMPT_FILE"] = RunPromptFile(w.InitialPrompt)
	}
	connectionFile := filepath.Join(w.State, "instances", instanceID+".json")
	defer os.Remove(connectionFile)

	// Stopping the run with a cause reports that cause instead of the exit.
	running, stopRunning := context.WithCancelCause(ctx)
	defer stopRunning(nil)
	if w.Task != "" {
		// A task never writes a connection file; an image that does not know
		// tasks starts a worker for VS Code, which would wait forever.
		watching, stopWatching := context.WithCancel(ctx)
		defer stopWatching()
		go watchConnection(watching, connectionFile, func(Connection) { stopRunning(ErrImageWithoutTasks) })
	} else if ready != nil || gate != nil || w.InitialPrompt != "" {
		watching, stopWatching := context.WithCancel(ctx)
		defer stopWatching()
		go watchConnection(watching, connectionFile, func(connection Connection) {
			if w.InitialPrompt != "" && !connection.InitialPrompt {
				stopRunning(ErrImageWithoutInitialPrompt)
				return
			}
			if gate != nil {
				if err := gate.bridgeAnswers(); err != nil {
					stopRunning(fmt.Errorf("the launcher cannot reach the isolated sandbox through %s: %v\n"+
						"Docker Desktop on macOS and Windows cannot share sockets with the host; "+
						"isolate the network on Linux, or run without --isolate-network", netDirectory, err))
					return
				}
			}
			if ready != nil {
				ready(connection)
			}
		})
	}

	code, err := w.runEngine(running, instanceID, values, port, netDirectory)
	// Only causes the worker set itself are errors: the caller's interrupt or
	// deadline (whose cause names the signal) is a normal way to stop.
	if cause := context.Cause(running); err == nil && cause != nil && ctx.Err() == nil && !errors.Is(cause, context.Canceled) {
		return 1, cause
	}
	return code, err
}

func (w Worker) runEngine(ctx context.Context, instanceID string, values map[string]string, port int, netDirectory string) (int, error) {
	name := "codeflower-" + instanceID[:8]
	switch w.Engine.Name {
	case EngineDocker:
		return RunDocker(ctx, DockerRun{
			Name: name, Image: w.Image, State: w.State, Binds: w.Binds, Env: values,
			Port: port, InspectPort: w.InspectPort, NetDirectory: netDirectory, Stdin: w.Stdin, Stdout: w.Stdout, Stderr: w.Stderr,
		})
	case EnginePodman:
		return RunPodman(ctx, w.Engine.Executable, PodmanRun{
			Name: name, Image: w.Image, State: w.State, Binds: w.Binds, Env: values,
			Port: port, NetDirectory: netDirectory, Stdin: w.Stdin, Stdout: w.Stdout, Stderr: w.Stderr,
		})
	case EngineApptainer:
		args, err := ApptainerCommand(w.Engine.Executable, w.Image, w.State, w.Binds, netDirectory)
		if err != nil {
			return 1, err
		}
		return RunApptainer(ctx, args, values, w.Stdin, w.Stdout, w.Stderr)
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
