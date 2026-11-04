package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"codeflower-app/internal/config"
	"codeflower-app/internal/sandbox"
)

// interactive says whether the user can answer questions on this terminal.
func interactive() bool {
	return isTerminal(os.Stdin)
}

// askAgent lets a user who left out --agent pick one.
func askAgent(in io.Reader, out io.Writer) (string, error) {
	fmt.Fprintln(out, "Which agent?")
	for index, agent := range sandbox.Agents {
		fmt.Fprintf(out, "  %d) %s (%s)\n", index+1, sandbox.AgentLabel(agent), agent)
	}
	fmt.Fprintf(out, "Choose 1-%d [1]: ", len(sandbox.Agents))
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("--agent is required: %s", strings.Join(sandbox.Agents, ", "))
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return sandbox.Agents[0], nil
	}
	if number, err := strconv.Atoi(line); err == nil && number >= 1 && number <= len(sandbox.Agents) {
		return sandbox.Agents[number-1], nil
	}
	if slices.Contains(sandbox.Agents, line) {
		return line, nil
	}
	return "", fmt.Errorf("unknown agent %q: choose one of %s", line, strings.Join(sandbox.Agents, ", "))
}

// chooseAgent asks for a missing agent on a terminal and checks it.
func chooseAgent(agent string) (string, error) {
	if agent == "" && interactive() {
		chosen, err := askAgent(os.Stdin, os.Stderr)
		if err != nil {
			return "", err
		}
		agent = chosen
	}
	if !slices.Contains(sandbox.Agents, agent) {
		return "", fmt.Errorf("--agent must be one of: %s", strings.Join(sandbox.Agents, ", "))
	}
	return agent, nil
}

// sandboxOptions are the folder and network flags that sandbox and task
// share, and the agent of the commands that run one (addAgentFlag).
type sandboxOptions struct {
	commonOptions
	// agent is --agent, for a command that names one agent (addAgentFlag);
	// runsOne says the sandbox runs only it: not a run's, which serves
	// windows and so runs every agent.
	agent          string
	oneAgent       bool
	runsOne        bool
	folders        sandbox.Folders
	bindValues     []string
	isolateNetwork bool
	allowlist      string
	cpus           int
	memory         string
	timeLimit      string
	// resources are --cpus, --memory and --time-limit; set by check.
	resources sandbox.Resources

	// changed says whether the user gave a flag; set by addSandboxFlags.
	changed func(name string) bool
	// userConfig is the config.yaml, read by check.
	userConfig *config.Config
	// configSkills are the sources of the skills folders that come from the config.
	configSkills map[string]bool
}

func addSandboxFlags(command *cobra.Command, options *sandboxOptions) {
	addCommonFlags(command, &options.commonOptions)
	options.changed = func(name string) bool { return command.Flags().Changed(name) }
	command.Flags().StringArrayVarP(&options.folders.Inputs, "input", "i", nil,
		"folder with your data, mounted read-only at /data/input/NAME (repeatable)")
	command.Flags().StringArrayVarP(&options.folders.Outputs, "output", "o", nil,
		"folder for results, created if needed and mounted read-write at /data/output/NAME (repeatable)")
	command.Flags().StringArrayVarP(&options.folders.Code, "code", "c", nil,
		"repository or package the agent develops, mounted read-write at /data/code/NAME (repeatable)")
	command.Flags().StringArrayVarP(&options.folders.Skills, "skills", "s", nil,
		"folder with a skill (a SKILL.md) or with skill folders, mounted read-only at /data/skills/NAME (repeatable)")
	command.Flags().BoolVar(&options.isolateNetwork, "isolate-network", false,
		"cut the sandbox off the network except for its agents' services and the hosts in the allowlist (Linux)")
	command.Flags().StringVar(&options.allowlist, "allowlist", "",
		"file of hosts an isolated sandbox may reach, one per line (default: PROJECT.allowed-hosts.txt next to the project)")
	command.Flags().StringArrayVar(&options.bindValues, "bind", nil,
		"more mounts: SOURCE[:TARGET[:ro|rw]] (repeatable; default target /data/input/NAME, read-only)")
	command.Flags().IntVar(&options.cpus, "cpus", 0,
		"CPUs the agent may use (default: the batch job's allocation; Docker and Podman also enforce it)")
	command.Flags().StringVar(&options.memory, "memory", "",
		"memory the agent may use, e.g. 32G (default: the batch job's allocation; Docker and Podman also enforce it)")
	command.Flags().StringVar(&options.timeLimit, "time-limit", "",
		"time the agent has, e.g. 8h or 1-12:00:00 (default: the batch job's time limit); reported, not enforced")
}

// addAgentFlag adds --agent to a command that names one agent: a task, a
// terminal UI or a plugins fetch, which run only it (runsOne), or a run's prompt.
func addAgentFlag(command *cobra.Command, options *sandboxOptions, usage string, runsOne bool) {
	options.oneAgent, options.runsOne = true, runsOne
	command.Flags().StringVar(&options.agent, "agent", os.Getenv("CODEFLOWER_AGENT"),
		usage+": "+strings.Join(sandbox.Agents, ", ")+" (or set $CODEFLOWER_AGENT; asked if left out)")
}

// check validates the flags, asking for a missing agent on a terminal.
func (options *sandboxOptions) check() error {
	if err := options.applyConfig(); err != nil {
		return err
	}
	if options.oneAgent {
		agent, err := chooseAgent(options.agent)
		if err != nil {
			return err
		}
		options.agent = agent
	}
	if err := options.validate(); err != nil {
		return err
	}
	var err error
	if options.image, err = imageName(options.image, options.imageTag); err != nil {
		return err
	}
	if options.allowlist != "" && !options.isolateNetwork {
		return fmt.Errorf("--allowlist needs --isolate-network")
	}
	return options.checkResources()
}

// checkResources reads --cpus, --memory and --time-limit.
func (options *sandboxOptions) checkResources() error {
	var given []string
	if options.cpus < 0 {
		return fmt.Errorf("--cpus must be positive")
	}
	if options.cpus > 0 {
		options.resources.CPUs = options.cpus
		given = append(given, "--cpus")
	}
	if options.memory != "" {
		memory, err := sandbox.ParseMemory(options.memory)
		if err != nil {
			return fmt.Errorf("--memory: %w", err)
		}
		options.resources.Memory = memory
		given = append(given, "--memory")
	}
	if options.timeLimit != "" {
		limit, err := sandbox.ParseTimeLimit(options.timeLimit)
		if err != nil {
			return fmt.Errorf("--time-limit: %w", err)
		}
		options.resources.End = time.Now().Add(limit)
		given = append(given, "--time-limit")
	}
	options.resources.Source = strings.Join(given, ", ")
	return nil
}

// applyConfig reads the config and uses it for what neither a flag nor an
// environment variable set, and mounts the config's skills folders.
func (options *sandboxOptions) applyConfig() error {
	loaded, err := options.loadConfig()
	if err != nil {
		return err
	}
	options.userConfig = loaded
	options.useDirectories(loaded)
	if !options.changed("engine") && os.Getenv("CODEFLOWER_ENGINE") == "" && loaded.Engine != "" {
		options.engine = loaded.Engine
	}
	if !options.changed("image") && os.Getenv("CODEFLOWER_IMAGE") == "" && loaded.Image != "" {
		options.image = loaded.Image
	}
	if !options.changed("isolate-network") && loaded.IsolateNetwork != nil {
		options.isolateNetwork = *loaded.IsolateNetwork
	}

	given := map[string]bool{}
	for _, folder := range options.folders.Skills {
		given[resolvedPath(folder)] = true
	}
	folders, missing := loaded.SkillFolders()
	for _, folder := range missing {
		fmt.Fprintf(os.Stderr, "Skipping the skills folder %s from %s: it does not exist\n", folder, loaded.Path)
	}
	options.configSkills = map[string]bool{}
	var fromConfig []string
	for _, folder := range folders {
		resolved := resolvedPath(folder)
		if given[resolved] {
			continue
		}
		given[resolved] = true
		options.configSkills[resolved] = true
		fromConfig = append(fromConfig, folder)
	}
	options.folders.Skills = append(fromConfig, options.folders.Skills...)
	return nil
}

// resolvedPath is a folder as the app mounts it, to tell the same folder apart.
func resolvedPath(folder string) string {
	absolute, err := filepath.Abs(folder)
	if err != nil {
		return folder
	}
	if real, err := filepath.EvalSymlinks(absolute); err == nil {
		return real
	}
	return absolute
}

// chooseEngine finds the container engine and checks that it can isolate the
// network when asked to.
func (options sandboxOptions) chooseEngine(ctx context.Context) (sandbox.EngineStatus, error) {
	engine, err := sandbox.ChooseEngine(sandbox.DetectEngines(ctx), options.engine, options.image)
	if err != nil {
		return engine, engineMissing(os.Stderr, engine, err)
	}
	if options.isolateNetwork {
		if err := sandbox.IsolationProblem(engine.Name); err != nil {
			return engine, err
		}
	}
	return engine, nil
}

// preparedSandbox is what a worker needs besides how it is reached.
type preparedSandbox struct {
	engine sandbox.EngineStatus
	image  string
	state  string
	// env is the project's env folder (--env-dir), or "".
	env       string
	binds     []sandbox.Bind
	isolation *sandbox.Isolation
	// apiKeys are the agents' keys from the config, by agent.
	apiKeys map[string]string
	// images is the image model from the config, if any.
	images *config.ImageGeneration
	// mcpServers are the config's MCP servers.
	mcpServers []config.MCPServer
	// plugins are the config's plugins, by agent.
	plugins   map[string][]config.PluginEntry
	resources sandbox.Resources
}

// prepare mounts the folders, makes sure the image is there and prepares the
// project, then prints what the sandbox will see.
func (options sandboxOptions) prepare(ctx context.Context, engine sandbox.EngineStatus, what string) (preparedSandbox, error) {
	binds, err := collectBinds(options.folders, options.bindValues)
	if err != nil {
		return preparedSandbox{}, err
	}
	return options.prepareWith(ctx, engine, what, binds)
}

// prepareWith is prepare with the mounts already chosen, e.g. a continued task's.
func (options sandboxOptions) prepareWith(ctx context.Context, engine sandbox.EngineStatus, what string, binds []sandbox.Bind) (preparedSandbox, error) {
	apiKeys, err := options.userConfig.APIKeys()
	if err != nil {
		return preparedSandbox{}, err
	}
	// The agents the sandbox runs: one, or every one for windows.
	agentsRun := sandbox.Agents
	if options.runsOne {
		agentsRun = []string{options.agent}
	}
	labels := make([]string, len(agentsRun))
	for index, agent := range agentsRun {
		labels[index] = sandbox.AgentLabel(agent)
	}
	images, err := options.userConfig.ImageGeneration()
	if err != nil {
		return preparedSandbox{}, err
	}
	binds, err = withIcons(binds, options.userConfig.IconsFolder())
	if err != nil {
		return preparedSandbox{}, err
	}
	// Only the published image is downloaded: another one is the user's to provide.
	if sandbox.IsPublished(options.image) {
		if err := sandbox.EnsureImage(ctx, engine, options.image, os.Stderr); err != nil {
			return preparedSandbox{}, err
		}
	}
	// After the download, which names an Apptainer file after its release.
	image, err := sandbox.ResolveImage(options.image, engine.Name)
	if err != nil {
		return preparedSandbox{}, err
	}
	state, err := sandbox.PrepareState(options.stateDirectory())
	if err != nil {
		return preparedSandbox{}, err
	}
	var env string
	if dir := options.envDirectory(state); dir != "" {
		if env, err = sandbox.PrepareEnv(dir, state); err != nil {
			return preparedSandbox{}, err
		}
	}

	var isolation *sandbox.Isolation
	if options.isolateNetwork {
		allowlist := options.allowlist
		if allowlist == "" {
			allowlist = sandbox.DefaultAllowlist(state)
		}
		path, err := sandbox.PrepareAllowlist(allowlist)
		if err != nil {
			return preparedSandbox{}, err
		}
		isolation = &sandbox.Isolation{Allowlist: path}
	}

	mcpServers := options.userConfig.MCPServers()
	fmt.Fprintf(os.Stderr, "Starting %s for %s with %s (%s)\n", what, strings.Join(labels, ", "), engine.Label, image)
	fmt.Fprintf(os.Stderr, "  project  %s\n", state)
	if env != "" {
		fmt.Fprintf(os.Stderr, "  env      %s (software environments and caches)\n", env)
	}
	if isolation != nil {
		hosts := sandbox.ProviderHostsOf(agentsRun)
		if images != nil {
			hosts = append(hosts, config.ImageProviders[images.Provider])
		}
		hosts = append(hosts, sandbox.MCPHosts(mcpServers)...)
		fmt.Fprintf(os.Stderr, "  network  isolated: only %s and the hosts in %s\n",
			strings.Join(hosts, ", "), isolation.Allowlist)
	}
	for _, bind := range binds {
		mode := "read-write"
		if bind.ReadOnly {
			mode = "read-only"
		}
		origin := ""
		if options.configSkills[bind.Source] {
			origin = ", from the config"
		} else if bind.Target == sandbox.IconsRoot {
			origin = ", the icon gallery"
		}
		fmt.Fprintf(os.Stderr, "  %-20s %s (%s%s)\n", bind.Target, bind.Source, mode, origin)
	}
	var keyed []string
	for _, agent := range agentsRun {
		if apiKeys[agent] != "" {
			keyed = append(keyed, sandbox.AgentLabel(agent))
		}
	}
	if len(keyed) > 0 {
		fmt.Fprintf(os.Stderr, "  api key  from the config, for %s\n", strings.Join(keyed, ", "))
	}
	if images != nil {
		fmt.Fprintf(os.Stderr, "  images   %s, from the config, for generating icons\n", images.Provider)
	}
	if len(mcpServers) > 0 {
		names := make([]string, len(mcpServers))
		for index, server := range mcpServers {
			names[index] = server.Name
		}
		fmt.Fprintf(os.Stderr, "  mcp      %s, from the config\n", strings.Join(names, ", "))
	}
	plugins := options.userConfig.AllPlugins()
	var sources []string
	for _, agent := range agentsRun {
		for _, entry := range plugins[agent] {
			sources = append(sources, entry.Source)
		}
	}
	if len(sources) > 0 {
		fmt.Fprintf(os.Stderr, "  plugins  %s, from the config\n", strings.Join(sources, ", "))
	}
	return preparedSandbox{engine: engine, image: image, state: state, env: env, binds: binds, isolation: isolation, apiKeys: apiKeys,
		images: images, mcpServers: mcpServers, plugins: plugins, resources: options.resources}, nil
}

// withIcons adds the icon gallery's mount, unless it is off or already there
// (a continued task's saved mounts).
func withIcons(binds []sandbox.Bind, folder string) ([]sandbox.Bind, error) {
	if folder == "" {
		return binds, nil
	}
	for _, bind := range binds {
		if bind.Target == sandbox.IconsRoot {
			return binds, nil
		}
	}
	bind, err := sandbox.IconsBind(folder)
	if err != nil {
		return nil, err
	}
	return append(binds, bind), nil
}

// worker is the prepared sandbox as a worker; agent is the one agent of a
// task, a terminal UI or a fetch run, or the agent of a run's prompt.
func (p preparedSandbox) worker(agent string) sandbox.Worker {
	return sandbox.Worker{
		Agent:      agent,
		Engine:     p.engine,
		Image:      p.image,
		State:      p.state,
		Env:        p.env,
		Binds:      p.binds,
		Isolation:  p.isolation,
		APIKeys:    p.apiKeys,
		Images:     p.images,
		MCPServers: p.mcpServers,
		Plugins:    p.plugins,
		Resources:  p.resources,
	}
}

// collectBinds turns --input, --output, --code, --skills and --bind into mounts.
func collectBinds(folders sandbox.Folders, values []string) ([]sandbox.Bind, error) {
	var extra []sandbox.Bind
	for _, value := range values {
		bind, err := sandbox.ParseBind(value)
		if err != nil {
			return nil, err
		}
		extra = append(extra, bind)
	}
	binds, err := folders.Binds(extra)
	if err != nil {
		return nil, err
	}
	return append(binds, extra...), nil
}
