package sandbox

import (
	"encoding/json"
	"strings"

	"codeflower-app/internal/agents"
)

// ContainerEnvironment keeps every tool's state, logins, and installs in the
// project, so they survive restarts and every worker of the project sees them.
func ContainerEnvironment() map[string]string {
	state := ContainerState
	env := state + "/env"
	cache := state + "/cache"
	values := map[string]string{
		"HOME":                    state + "/home",
		"CODEFLOWER_SESSIONS_DIR": state + "/sessions",
		"CODEFLOWER_RUN_DIR":      state + "/run",
		"CODEFLOWER_WORKSPACE":    state + "/workspace",
		"VIRTUAL_ENV":             env + "/venv",
		"CONDA_ENVS_DIRS":         env + "/conda",
		"CONDA_PKGS_DIRS":         cache + "/conda-pkgs",
		"UV_CACHE_DIR":            cache + "/uv",
		"PIP_CACHE_DIR":           cache + "/pip",
		"R_LIBS_USER":             env + "/R",
		"NPM_CONFIG_PREFIX":       env + "/npm",
		"NPM_CONFIG_CACHE":        cache + "/npm",
		"CARGO_HOME":              env + "/cargo",
		// No display in the sandbox: figures are written to files.
		"MPLBACKEND": "Agg",
		"CODEFLOWER_PREPEND_PATH": strings.Join(
			[]string{env + "/venv/bin", env + "/npm/bin", env + "/cargo/bin"}, ":"),
	}
	// Each agent's home (CLAUDE_CONFIG_DIR, CODEX_HOME, DSH_HOME, …), from its descriptor.
	for _, descriptor := range agents.All() {
		values[descriptor.Home.Variable] = state + "/" + descriptor.Home.Path
	}
	return values
}

// WorkerSettings describe one worker for the bridge inside the sandbox.
type WorkerSettings struct {
	Agent         string
	InstanceID    string
	Token         string
	ListenHost    string
	AdvertiseHost string
	Port          int
	HostState     string
	Engine        string
	SlurmJobID    string
	AppPID        int
	// Binds are listed for VS Code, which shows them next to the workspace.
	Binds []Bind
}

type mountInfo struct {
	Target   string `json:"target"`
	Source   string `json:"source"`
	ReadOnly bool   `json:"readOnly"`
}

// WorkerEnvironment is the full environment the bridge starts with.
func WorkerEnvironment(settings WorkerSettings) map[string]string {
	values := ContainerEnvironment()
	values["CODEFLOWER_AGENT"] = settings.Agent
	values["CODEFLOWER_INSTANCE_ID"] = settings.InstanceID
	values["CODEFLOWER_TOKEN"] = settings.Token
	values["CODEFLOWER_CONNECTION_FILE"] = ContainerState + "/instances/" + settings.InstanceID + ".json"
	values["CODEFLOWER_LISTEN_HOST"] = settings.ListenHost
	values["CODEFLOWER_ADVERTISE_HOST"] = settings.AdvertiseHost
	values["CODEFLOWER_PORT"] = itoa(settings.Port)
	values["CODEFLOWER_APP_PID"] = itoa(settings.AppPID)
	// Shown in VS Code next to the worker.
	values["CODEFLOWER_HOST_STATE"] = settings.HostState
	values["CODEFLOWER_ENGINE"] = settings.Engine
	mounts := make([]mountInfo, 0, len(settings.Binds))
	for _, bind := range settings.Binds {
		mounts = append(mounts, mountInfo{Target: bind.Target, Source: bind.Source, ReadOnly: bind.ReadOnly})
	}
	if encoded, err := json.Marshal(mounts); err == nil {
		values["CODEFLOWER_MOUNTS"] = string(encoded)
	}
	if settings.SlurmJobID != "" {
		values["CODEFLOWER_SLURM_JOB_ID"] = settings.SlurmJobID
	}
	return values
}
