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

	"github.com/spf13/cobra"

	"codeflower-launcher/internal/config"
	"codeflower-launcher/internal/sandbox"
)

// interactive says whether the user can answer questions on this terminal.
func interactive() bool {
	return isTerminal(os.Stdin)
}

// askAgent lets a user who left out --agent pick one.
func askAgent(in io.Reader, out io.Writer) (string, error) {
	fmt.Fprintln(out, "Which agent should the sandbox run?")
	for index, agent := range sandbox.Agents {
		fmt.Fprintf(out, "  %d) %s (%s)\n", index+1, sandbox.AgentLabel(agent), agent)
	}
	fmt.Fprint(out, "Choose 1-3 [1]: ")
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

// sandboxOptions are the agent, folder and network flags that interactive
// and task share.
type sandboxOptions struct {
	commonOptions
	agent          string
	folders        sandbox.Folders
	bindValues     []string
	isolateNetwork bool
	allowlist      string

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
	command.Flags().StringVar(&options.agent, "agent", os.Getenv("CODEFLOWER_AGENT"),
		"agent the sandbox runs: claude, codex, or dsh (or set $CODEFLOWER_AGENT; asked if left out)")
	command.Flags().StringArrayVarP(&options.folders.Inputs, "input", "i", nil,
		"folder with your data, mounted read-only at /data/input/NAME (repeatable)")
	command.Flags().StringArrayVarP(&options.folders.Outputs, "output", "o", nil,
		"folder for results, created if needed and mounted read-write at /data/output/NAME (repeatable)")
	command.Flags().StringArrayVarP(&options.folders.Code, "code", "c", nil,
		"repository or package the agent develops, mounted read-write at /data/code/NAME (repeatable)")
	command.Flags().StringArrayVarP(&options.folders.Skills, "skills", "s", nil,
		"folder with a skill (a SKILL.md) or with skill folders, mounted read-only at /data/skills/NAME (repeatable)")
	command.Flags().BoolVar(&options.isolateNetwork, "isolate-network", false,
		"cut the sandbox off the network except for its agent's service and the hosts in the allowlist (Linux)")
	command.Flags().StringVar(&options.allowlist, "allowlist", "",
		"file of hosts an isolated sandbox may reach, one per line (default: PROJECT.allowed-hosts.txt next to the project)")
	command.Flags().StringArrayVar(&options.bindValues, "bind", nil,
		"more mounts: SOURCE[:TARGET[:ro|rw]] (repeatable; default target /data/input/NAME, read-only)")
}

// check validates the flags, asking for a missing agent on a terminal.
func (options *sandboxOptions) check() error {
	if err := options.applyConfig(); err != nil {
		return err
	}
	agent, err := chooseAgent(options.agent)
	if err != nil {
		return err
	}
	options.agent = agent
	if err := options.validate(); err != nil {
		return err
	}
	if options.image, err = imageName(options.image, options.imageTag); err != nil {
		return err
	}
	if options.allowlist != "" && !options.isolateNetwork {
		return fmt.Errorf("--allowlist needs --isolate-network")
	}
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

// resolvedPath is a folder as the launcher mounts it, to tell the same folder apart.
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
	engine    sandbox.EngineStatus
	image     string
	state     string
	binds     []sandbox.Bind
	isolation *sandbox.Isolation
	// apiKey is the agent's key from the config, if any.
	apiKey string
}

// prepare mounts the folders, makes sure the image is there and prepares the
// project, then prints what the sandbox will see.
func (options sandboxOptions) prepare(ctx context.Context, engine sandbox.EngineStatus, what string) (preparedSandbox, error) {
	binds, err := collectBinds(options.folders, options.bindValues)
	if err != nil {
		return preparedSandbox{}, err
	}
	apiKey, err := options.userConfig.APIKey(options.agent)
	if err != nil {
		return preparedSandbox{}, err
	}
	image, err := sandbox.ResolveImage(options.image, engine.Name)
	if err != nil {
		return preparedSandbox{}, err
	}
	// Only the published image is downloaded: another one is the user's to provide.
	if sandbox.IsPublished(options.image) {
		if err := sandbox.EnsureImage(ctx, engine, options.image, os.Stderr); err != nil {
			return preparedSandbox{}, err
		}
	}
	state, err := sandbox.PrepareState(options.stateDirectory())
	if err != nil {
		return preparedSandbox{}, err
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

	fmt.Fprintf(os.Stderr, "Starting %s for %s with %s (%s)\n", what, sandbox.AgentLabel(options.agent), engine.Label, image)
	fmt.Fprintf(os.Stderr, "  project  %s\n", state)
	if isolation != nil {
		fmt.Fprintf(os.Stderr, "  network  isolated: only %s and the hosts in %s\n",
			strings.Join(sandbox.ProviderHosts[options.agent], ", "), isolation.Allowlist)
	}
	for _, bind := range binds {
		mode := "read-write"
		if bind.ReadOnly {
			mode = "read-only"
		}
		origin := ""
		if options.configSkills[bind.Source] {
			origin = ", from the config"
		}
		fmt.Fprintf(os.Stderr, "  %-20s %s (%s%s)\n", bind.Target, bind.Source, mode, origin)
	}
	if apiKey != "" {
		fmt.Fprintf(os.Stderr, "  api key  from the config, for %s\n", sandbox.AgentLabel(options.agent))
	}
	return preparedSandbox{engine: engine, image: image, state: state, binds: binds, isolation: isolation, apiKey: apiKey}, nil
}

// worker is the prepared sandbox as a worker for agent.
func (p preparedSandbox) worker(agent string) sandbox.Worker {
	return sandbox.Worker{
		Agent:     agent,
		Engine:    p.engine,
		Image:     p.image,
		State:     p.state,
		Binds:     p.binds,
		Isolation: p.isolation,
		APIKey:    p.apiKey,
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
