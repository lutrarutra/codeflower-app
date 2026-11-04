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
	"slices"
	"time"

	"codeflower-app/internal/agents"
	"codeflower-app/internal/config"
)

// Agents a worker can run, the default first, from their descriptors.
var Agents = agents.IDs()

// AgentLabel is the agent's product name.
func AgentLabel(agent string) string {
	return agents.Label(agent)
}

// Where the user's folders appear in the sandbox, each in a subfolder named
// after it, e.g. /data/input/study42.
const (
	InputsRoot  = "/data/input"
	OutputsRoot = "/data/output"
	CodeRoot    = "/data/code"
	SkillsRoot  = "/data/skills"
	// IconsRoot is the user's icon gallery, shared by all their projects.
	IconsRoot = "/data/icons"
)

// IconsBind mounts the icon gallery, creating its folder when needed.
func IconsBind(folder string) (Bind, error) {
	if err := os.MkdirAll(expandHome(folder), 0o755); err != nil {
		return Bind{}, fmt.Errorf("icon gallery: %w", err)
	}
	return NewBind(folder, IconsRoot, false)
}

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

// APIKeysVariable carries the config's API keys to the bridge, as JSON by
// agent, which removes it from its environment again.
const APIKeysVariable = "CODEFLOWER_API_KEYS"

// Worker is one sandbox with its agent bridge. A worker for windows runs
// every agent; a task, a terminal UI and a plugins fetch run run one.
type Worker struct {
	// Agent is the one agent of a task, a terminal UI or a plugins fetch run,
	// and the agent of a run's initial prompt.
	Agent  string
	Engine EngineStatus
	Image  string
	// State is the prepared, absolute project directory.
	State string
	// Env, when set, is the project's prepared env folder (PrepareEnv),
	// mounted over the project's software environment and caches.
	Env           string
	Binds         []Bind
	ListenHost    string
	AdvertiseHost string
	Port          int
	// Token is the secret VS Code connects with; NewToken() when empty. The
	// client sets it up front, so it can keep it out of the log from the
	// first line.
	Token string
	// InspectPort publishes the bridge's Node inspector (development, Docker).
	InspectPort int
	// ServePage makes the worker also serve the chat page on its port
	// (CODEFLOWER_PAGE), opened by the link it prints.
	ServePage bool
	// Isolation, when set, cuts the sandbox off the network except for what
	// the app's proxy allows.
	Isolation *Isolation
	// APIKeys, by agent, are given to the bridge in the environment (never in
	// the command line); it stores each in its agent's own files.
	APIKeys map[string]string
	// Images, when set, lets codeflower-sketch generate icons: the key is
	// given in a private file for the worker's lifetime.
	Images *config.ImageGeneration
	// MCPServers from the config are given to the agent in every session; an
	// isolated sandbox reaches the hosts of those at a URL.
	MCPServers []config.MCPServer
	// Task, when set, is the id of a task in the project's tasks folder: the
	// agent works on its attempt TaskAttempt by itself, and nothing listens
	// for VS Code.
	Task        string
	TaskAttempt int
	// InitialPrompt, when set, is the id of a run prompt to send in a new
	// session while keeping the worker available to VS Code.
	InitialPrompt string
	// TaskInteractive enables terminal sign-in before the task when stdin is a terminal.
	TaskInteractive bool
	// TUI runs the agent's own terminal UI on the user's terminal instead of
	// the bridge (`codeflower tui`): no port, nothing to connect to, and the
	// sandbox ends with it. Its value is the chat to open: "new", "last",
	// "pick" (the agent's own list) or a session id.
	TUI string
	// TUIAgain is the command that opens a chat of the terminal UI again, up
	// to its id, which the sandbox prints when the terminal UI ends in place
	// of the agent's own command.
	TUIAgain string
	// NoTunnel keeps the sandbox from browsers paired through the tunnel.
	NoTunnel bool
	// Plugins are the config's plugins, by agent (docs/plugins.md). Those
	// never fetched are fetched before the worker starts.
	Plugins map[string][]config.PluginEntry
	// PluginsFetch makes this the fetch run (FetchPlugins): no port, and an
	// isolated one reaches only the agent's plugin hosts.
	PluginsFetch bool
	// Resources the user gave replace those of the batch job the app runs in.
	Resources Resources
	Stdin     io.Reader
	Stdout    io.Writer
	Stderr    io.Writer
	// Kill, once closed, kills the sandbox at once instead of waiting for it
	// to stop after Run's ctx ended (the second Ctrl+C).
	Kill <-chan struct{}
}

// ErrImageWithoutTasks means the sandbox started a worker for VS Code instead
// of running its task: the image is older than tasks.
var ErrImageWithoutTasks = errors.New("this sandbox image cannot run tasks (it started a worker for VS Code instead); " +
	"update it with `codeflower pull`, or choose a newer --image")

// ErrImageWithoutTUI means the sandbox started a worker for VS Code instead
// of the agent's terminal UI: the image is older than `codeflower tui`.
var ErrImageWithoutTUI = errors.New("this sandbox image cannot run the agent's terminal UI (it started a worker for VS Code instead); " +
	"update it with `codeflower pull`, or choose a newer --image")

// ErrImageWithoutInitialPrompt means the sandbox started a worker but ignored
// the run's initial prompt.
var ErrImageWithoutInitialPrompt = errors.New("this sandbox image cannot start a run with an initial prompt; " +
	"update it with `codeflower pull`, or choose a newer --image")

// Connection is what the bridge writes once it listens: the line VS Code
// connects with.
type Connection struct {
	ID            string `json:"id"`
	URL           string `json:"url"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	InitialPrompt bool   `json:"initialPrompt,omitempty"`
	// Token also authorizes the bridge's /status.
	Token string `json:"token"`
	// Fingerprint is the bridge's TLS certificate's (see tls.go); empty when
	// it serves plain HTTP (with the page, or an older image).
	Fingerprint string `json:"fingerprint,omitempty"`
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
	if !w.PluginsFetch {
		for _, agent := range w.agentsRun() {
			if HasPlugins(agent) {
				w.fetchMissingPlugins(ctx, agent)
			}
		}
	}
	// Tasks, fetch runs and terminal UIs do not listen for windows.
	listens := w.Task == "" && !w.PluginsFetch && w.TUI == ""
	// Mounted for the engine only: VS Code lists the user's folders.
	claude, err := w.claudeBinds(ctx)
	if err != nil {
		return 1, err
	}
	if w.Isolation != nil && w.InspectPort > 0 {
		return 1, fmt.Errorf("the inspector cannot be published from a network-isolated sandbox")
	}
	containerized := w.Engine.Name != EngineApptainer
	port := w.Port
	if containerized && port == 0 && w.Isolation == nil && listens {
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
	if listen == "" || containerized {
		// Docker and Podman publish the port on the host's 127.0.0.1 only;
		// inside the container, a loopback address would be out of their reach.
		listen = "0.0.0.0"
	}
	instanceID := NewInstanceID()
	resources := DetectResources(ctx, os.Getenv).With(w.Resources)
	if !resources.Empty() {
		fmt.Fprintf(w.Stderr, "  resources %s\n", resources.Summary())
	}

	// An isolated sandbox has no network: the app holds VS Code's port
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
		if listens {
			ingressHost := listen
			if containerized {
				ingressHost = "127.0.0.1"
			}
			ingress = net.JoinHostPort(ingressHost, itoa(port))
		}
		log := func(message string) { fmt.Fprintln(w.Stderr, message) }
		allowlistFile := w.Isolation.Allowlist
		if w.PluginsFetch {
			// Only the hosts the agent's installer needs, not the user's.
			allowlistFile = ""
		}
		list := newAllowlist(allowlistFile, w.providerHosts(), log)
		gate, err = startGateway(netDirectory, ingress, list, log)
		if err != nil {
			os.RemoveAll(netDirectory)
			return 1, err
		}
		defer gate.Close()
		port = gate.Port()
	}

	token := w.Token
	if token == "" {
		token = NewToken()
	}
	values := WorkerEnvironment(WorkerSettings{
		Agent:         w.oneAgent(),
		InstanceID:    instanceID,
		Token:         token,
		ListenHost:    listen,
		AdvertiseHost: advertise,
		Port:          port,
		HostState:     w.State,
		Engine:        w.Engine.Name,
		SlurmJobID:    os.Getenv("SLURM_JOB_ID"),
		AppPID:        os.Getpid(),
		Binds:         w.Binds,
	})
	resources.environment(values)
	if keys := w.apiKeys(); len(keys) > 0 {
		values[APIKeysVariable] = keys
	}
	if len(w.MCPServers) > 0 {
		values[MCPServersVariable] = MCPServersJSON(w.MCPServers)
	}
	if plugins := w.pluginsRun(); len(plugins) > 0 {
		values[PluginsVariable] = PluginsJSON(plugins)
	}
	if w.PluginsFetch {
		values[PluginsFetchVariable] = "1"
	}
	// env/ and cache/ are separate mounts then, which hard links cannot cross.
	if w.Env != "" {
		values["UV_LINK_MODE"] = "copy"
	}
	for _, bind := range w.Binds {
		if bind.Target == IconsRoot {
			values["CODEFLOWER_ICONS"] = IconsRoot
		}
	}
	if w.Images != nil {
		keyFile, err := writeImageKey(w.State, instanceID, w.Images.Key)
		if err != nil {
			return 1, err
		}
		defer os.Remove(keyFile)
		values["CODEFLOWER_IMAGE_PROVIDER"] = w.Images.Provider
		values["CODEFLOWER_IMAGE_MODEL"] = w.Images.Model
		values["CODEFLOWER_IMAGE_KEY_FILE"] = ContainerState + "/run/" + filepath.Base(keyFile)
	}
	if w.ServePage {
		// The page is plain HTTP, for a TLS reverse proxy in front.
		values["CODEFLOWER_PAGE"] = "1"
	} else if listens {
		certificate, err := NewCertificate()
		if err != nil {
			return 1, fmt.Errorf("could not make the worker's TLS certificate: %w", err)
		}
		values[TLSCertVariable] = string(certificate.CertPEM)
		values[TLSKeyVariable] = string(certificate.KeyPEM)
	}
	if w.InspectPort > 0 {
		values["CODEFLOWER_BRIDGE_INSPECT"] = fmt.Sprintf("0.0.0.0:%d", BridgeInspectPort)
	}
	if gate != nil {
		isolatedEnvironment(values)
	}
	if w.Task != "" {
		values["CODEFLOWER_TASK_ATTEMPT_FILE"] = AttemptFile(w.Task, w.TaskAttempt)
		if w.TaskInteractive {
			values["CODEFLOWER_TASK_INTERACTIVE"] = "1"
		}
	}
	if w.InitialPrompt != "" {
		values["CODEFLOWER_INITIAL_PROMPT_FILE"] = RunPromptFile(w.InitialPrompt)
		values["CODEFLOWER_INITIAL_PROMPT_AGENT"] = w.Agent
	}
	if w.TUI != "" {
		values["CODEFLOWER_TUI"] = "1"
		values["CODEFLOWER_TUI_RESUME"] = w.TUI
		if w.TUIAgain != "" {
			values["CODEFLOWER_TUI_AGAIN"] = w.TUIAgain
		}
	}
	connectionFile := filepath.Join(w.State, "instances", instanceID+".json")
	defer os.Remove(connectionFile)
	kind := "worker"
	switch {
	case w.Task != "":
		kind = "task"
	case w.InitialPrompt != "":
		kind = "run"
	}
	// A terminal UI is no sandbox the app or the tunnel can open.
	if !w.PluginsFetch && w.TUI == "" {
		defer register(Running{
			ID: instanceID, ConnectionFile: connectionFile, Project: filepath.Base(w.State), State: w.State,
			Agent: w.oneAgent(), Engine: w.Engine.Name, Kind: kind, PID: os.Getpid(), Started: time.Now(), NoTunnel: w.NoTunnel,
		})()
	}

	// Stopping the run with a cause reports that cause instead of the exit.
	running, stopRunning := context.WithCancelCause(ctx)
	defer stopRunning(nil)
	if !listens {
		// A task or fetch run never writes a connection file; an image that
		// does not know them starts a worker for VS Code, which would wait forever.
		tooOld := ErrImageWithoutTasks
		if w.PluginsFetch {
			tooOld = ErrImageWithoutPlugins
		} else if w.TUI != "" {
			tooOld = ErrImageWithoutTUI
		}
		watching, stopWatching := context.WithCancel(ctx)
		defer stopWatching()
		go watchConnection(watching, connectionFile, func(Connection) { stopRunning(tooOld) })
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
					stopRunning(fmt.Errorf("the app cannot reach the isolated sandbox through %s: %v\n"+
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

	w.Binds = append(slices.Clip(w.Binds), claude...)
	code, err := w.runEngine(running, instanceID, values, port, netDirectory, resources)
	// Only causes the worker set itself are errors: the caller's interrupt or
	// deadline (whose cause names the signal) is a normal way to stop.
	if cause := context.Cause(running); err == nil && cause != nil && ctx.Err() == nil && !errors.Is(cause, context.Canceled) {
		return 1, cause
	}
	return code, err
}

// agentsRun are the agents the sandbox runs: one for a task, a terminal UI
// or a fetch run, every one for windows.
func (w Worker) agentsRun() []string {
	if agent := w.oneAgent(); agent != "" {
		return []string{agent}
	}
	return Agents
}

// oneAgent is the agent of a sandbox that runs one, else "".
func (w Worker) oneAgent() string {
	if w.Task != "" || w.TUI != "" || w.PluginsFetch {
		return w.Agent
	}
	return ""
}

// apiKeys is APIKeysVariable's value: the keys of the agents the sandbox runs.
func (w Worker) apiKeys() string {
	keys := map[string]string{}
	for _, agent := range w.agentsRun() {
		if key := w.APIKeys[agent]; key != "" {
			keys[agent] = key
		}
	}
	if len(keys) == 0 {
		return ""
	}
	data, _ := json.Marshal(keys)
	return string(data)
}

// pluginsRun are the config's plugins of the agents the sandbox runs.
func (w Worker) pluginsRun() map[string][]config.PluginEntry {
	plugins := map[string][]config.PluginEntry{}
	for _, agent := range w.agentsRun() {
		if entries := w.Plugins[agent]; len(entries) > 0 {
			plugins[agent] = entries
		}
	}
	return plugins
}

// providerHosts are the hosts an isolated sandbox always reaches: the
// services of the agents it runs, the image model's when one is configured,
// and the MCP servers'.
func (w Worker) providerHosts() []string {
	if w.PluginsFetch {
		return PluginHosts(w.Agent)
	}
	hosts := ProviderHostsOf(w.agentsRun())
	if w.Images != nil {
		hosts = append(hosts, config.ImageProviders[w.Images.Provider])
	}
	return append(hosts, MCPHosts(w.MCPServers)...)
}

// needsUserNamespaces is whether an agent the sandbox runs sandboxes its
// commands itself (Codex's bubblewrap).
func (w Worker) needsUserNamespaces() bool {
	for _, agent := range w.agentsRun() {
		if descriptor, _ := agents.Get(agent); descriptor.Container.UserNamespaces {
			return true
		}
	}
	return false
}

// writeImageKey puts the image model's key in the project's run folder, where
// codeflower-sketch reads it; it is not in the environment, which the bridge
// keeps free of credentials.
func writeImageKey(state, instanceID, key string) (string, error) {
	directory := filepath.Join(state, "run")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(directory, "image-key-"+instanceID)
	if err := os.WriteFile(path, []byte(key+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("could not write the image key: %w", err)
	}
	return path, nil
}

// Apptainer runs in the job's own limits, so only Docker and Podman, whose
// containers the daemon or podman start outside them, are limited here.
func (w Worker) runEngine(ctx context.Context, instanceID string, values map[string]string, port int, netDirectory string, resources Resources) (int, error) {
	name := "codeflower-" + instanceID[:8]
	binds := append(EnvBinds(w.Env, w.State), w.Binds...)
	// An agent that sandboxes its commands itself needs user namespaces
	// (Docker's and Podman's default profiles refuse them); Apptainer has none.
	var seccomp []byte
	if w.needsUserNamespaces() && w.Engine.Name != EngineApptainer {
		var err error
		if seccomp, err = UserNamespacesSeccomp(); err != nil {
			return 1, err
		}
	}
	switch w.Engine.Name {
	case EngineDocker:
		return RunDocker(ctx, DockerRun{
			Name: name, Image: w.Image, State: w.State, Binds: binds, Env: values,
			Port: port, InspectPort: w.InspectPort, NetDirectory: netDirectory, CPUs: resources.CPUs, Memory: resources.Memory, Stdin: w.Stdin, Stdout: w.Stdout, Stderr: w.Stderr,
			TTY: w.TUI != "", Seccomp: seccomp, Kill: w.Kill,
		})
	case EnginePodman:
		var seccompFile string
		if seccomp != nil {
			var err error
			if seccompFile, err = writeSeccomp(w.State, instanceID, seccomp); err != nil {
				return 1, err
			}
			defer os.Remove(seccompFile)
		}
		return RunPodman(ctx, w.Engine.Executable, PodmanRun{
			Name: name, Image: w.Image, State: w.State, Binds: binds, Env: values,
			Port: port, NetDirectory: netDirectory, CPUs: resources.CPUs, Memory: resources.Memory, Stdin: w.Stdin, Stdout: w.Stdout, Stderr: w.Stderr,
			TTY: w.TUI != "", SeccompFile: seccompFile, Kill: w.Kill,
		})
	case EngineApptainer:
		scratch, err := newScratch()
		if err != nil {
			return 1, err
		}
		defer os.RemoveAll(scratch)
		args, err := ApptainerCommand(w.Engine.Executable, w.Image, w.State, scratch, binds, netDirectory)
		if err != nil {
			return 1, err
		}
		return RunApptainer(ctx, w.Kill, args, values, w.Stdin, w.Stdout, w.Stderr)
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
