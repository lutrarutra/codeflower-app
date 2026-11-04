package cli

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"syscall"

	"github.com/spf13/cobra"

	"codeflower-app/internal/agents"
	"codeflower-app/internal/config"
	"codeflower-app/internal/sandbox"
)

func pluginsCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "plugins",
		Short: "List, add, remove and fetch the agents' own plugins in a project",
		Long: `Each agent has plugins of its own, such as Claude Code's marketplaces. The
config's plugins: are in every project; a project adds its own, here or in
the chat's Plugins view, and may switch the config's off.

Sources: marketplace:NAME@CATALOG, github:OWNER/REPO[#REF], git:URL[#REF],
npm:PACKAGE@VERSION, url:https://….zip[#sha256=…] or path:/absolute; each
agent takes some of them.

Plugins are fetched with the agent's own installer, which needs the
network: in the sandbox when it has it, else by the app before the sandbox
starts. "codeflower plugins fetch" fetches what is missing, also for an
isolated sandbox, which reaches only the hosts the installer needs while
fetching. A running agent picks up changes when it restarts.`,
		Example: `  codeflower plugins add --agent claude --project study42 marketplace:commit-commands@claude-plugins-official
  codeflower plugins fetch --agent claude --project study42
  codeflower plugins list --agent claude --project study42`,
	}
	command.AddCommand(pluginsListCommand(), pluginsAddCommand(), pluginsRemoveCommand(), pluginsEnableCommand(true),
		pluginsEnableCommand(false), pluginsFetchCommand())
	return command
}

// pluginsTarget is the agent and project a plugins subcommand works on.
type pluginsTarget struct {
	commonOptions
	agent string
}

func addPluginsFlags(command *cobra.Command, target *pluginsTarget) {
	command.Flags().StringVarP(&target.project, "project", "p", "default",
		"project name: its plugins live in "+filepath.Join(ProjectsDirectory(), "NAME", "plugins"))
	command.Flags().StringVar(&target.state, "state", "", "project directory, instead of --project")
	addProjectsDirFlag(command, &target.commonOptions)
	command.Flags().StringVar(&target.config, "config", "",
		"config directory with config.yaml (or set $CODEFLOWER_CONFIG; default ~/.codeflower)")
	command.Flags().StringVar(&target.agent, "agent", os.Getenv("CODEFLOWER_AGENT"), "the agent whose plugins these are")
}

// resolve checks the agent and reads the config's entries for it.
func (target *pluginsTarget) resolve() (string, []config.PluginEntry, error) {
	agent, err := chooseAgent(target.agent)
	if err != nil {
		return "", nil, err
	}
	if !sandbox.HasPlugins(agent) {
		return "", nil, fmt.Errorf("%s has no plugins in code flower yet", sandbox.AgentLabel(agent))
	}
	target.agent = agent
	loaded, err := config.Load(config.Directory(target.config))
	if err != nil {
		return "", nil, err
	}
	target.useDirectories(loaded)
	state, err := filepath.Abs(target.stateDirectory())
	if err != nil {
		return "", nil, err
	}
	return state, loaded.AgentPlugins(agent), nil
}

// projectIdle refuses to write plugins.json while a worker of the project
// runs: its bridge owns the file then.
func projectIdle(state string) error {
	for _, running := range sandbox.ListRunning() {
		if filepath.Clean(running.State) == filepath.Clean(state) {
			return fmt.Errorf("a sandbox of this project is running (%s): change its plugins in its chat's Plugins view", running.ID)
		}
	}
	return nil
}

func pluginsListCommand() *cobra.Command {
	var target pluginsTarget
	command := &cobra.Command{
		Use:   "list",
		Short: "List an agent's plugins in a project and whether they are fetched",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			state, configured, err := target.resolve()
			if err != nil {
				return err
			}
			list, err := sandbox.ListPlugins(state, target.agent, configured)
			if err != nil {
				return err
			}
			out := command.OutOrStdout()
			if len(list) == 0 {
				fmt.Fprintf(out, "No plugins for %s in %s.\n", sandbox.AgentLabel(target.agent), state)
				return nil
			}
			for _, entry := range list {
				status := entry.State
				switch {
				case !entry.Enabled:
					status = "off"
				case entry.Version != "":
					status += " " + entry.Version
				case entry.Error != "":
					status += ": " + entry.Error
				}
				fmt.Fprintf(out, "%-60s %-8s %s\n", entry.Source, entry.Origin, status)
			}
			return nil
		},
	}
	addPluginsFlags(command, &target)
	return command
}

// checkSource checks a source against the agent's kinds.
func checkSource(agent, text string) (string, error) {
	kind, source, err := config.ParsePluginSource(text)
	if err != nil {
		return "", err
	}
	descriptor, _ := agents.Get(agent)
	if !slices.Contains(descriptor.Plugins.Kinds, kind) {
		return "", fmt.Errorf("%s takes plugins from %v, not %s:", descriptor.Name, descriptor.Plugins.Kinds, kind)
	}
	return source, nil
}

func pluginsAddCommand() *cobra.Command {
	var target pluginsTarget
	command := &cobra.Command{
		Use:   "add SOURCE",
		Short: "Add a plugin to a project; it is fetched before the next start",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			state, _, err := target.resolve()
			if err != nil {
				return err
			}
			source, err := checkSource(target.agent, args[0])
			if err != nil {
				return err
			}
			if err := projectIdle(state); err != nil {
				return err
			}
			if err := sandbox.EditProjectPlugins(state, target.agent, func(entries []sandbox.PluginEntryJSON) []sandbox.PluginEntryJSON {
				entries = slices.DeleteFunc(entries, func(entry sandbox.PluginEntryJSON) bool { return sameSource(entry.Source, source) })
				return append(entries, sandbox.PluginEntryJSON{Source: source, Enabled: true})
			}); err != nil {
				return err
			}
			fmt.Fprintf(command.OutOrStdout(), "Added %s for %s; it is fetched when the sandbox starts, or now with `codeflower plugins fetch`.\n",
				source, sandbox.AgentLabel(target.agent))
			return nil
		},
	}
	addPluginsFlags(command, &target)
	return command
}

func sameSource(text, source string) bool {
	_, normalized, err := config.ParsePluginSource(text)
	return err == nil && normalized == source
}

func pluginsRemoveCommand() *cobra.Command {
	var target pluginsTarget
	command := &cobra.Command{
		Use:   "remove SOURCE",
		Short: "Remove a project's plugin (the config's are switched off with disable)",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			state, configured, err := target.resolve()
			if err != nil {
				return err
			}
			_, source, err := config.ParsePluginSource(args[0])
			if err != nil {
				return err
			}
			if slices.ContainsFunc(configured, func(entry config.PluginEntry) bool { return entry.Source == source }) {
				return fmt.Errorf("%s comes from the config; switch it off for this project with `codeflower plugins disable`", source)
			}
			if err := projectIdle(state); err != nil {
				return err
			}
			found := false
			if err := sandbox.EditProjectPlugins(state, target.agent, func(entries []sandbox.PluginEntryJSON) []sandbox.PluginEntryJSON {
				return slices.DeleteFunc(entries, func(entry sandbox.PluginEntryJSON) bool {
					match := sameSource(entry.Source, source)
					found = found || match
					return match
				})
			}); err != nil {
				return err
			}
			if !found {
				return fmt.Errorf("no plugin %s in this project", source)
			}
			fmt.Fprintf(command.OutOrStdout(), "Removed %s.\n", source)
			return nil
		},
	}
	addPluginsFlags(command, &target)
	return command
}

func pluginsEnableCommand(enabled bool) *cobra.Command {
	var target pluginsTarget
	use, short := "enable SOURCE", "Switch a plugin on for a project"
	if !enabled {
		use, short = "disable SOURCE", "Switch a plugin off for a project, also one from the config"
	}
	command := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			state, configured, err := target.resolve()
			if err != nil {
				return err
			}
			_, source, err := config.ParsePluginSource(args[0])
			if err != nil {
				return err
			}
			list, err := sandbox.ListPlugins(state, target.agent, configured)
			if err != nil {
				return err
			}
			if !slices.ContainsFunc(list, func(entry sandbox.PluginStatus) bool { return entry.Source == source }) {
				return fmt.Errorf("no plugin %s in this project; add it first", source)
			}
			if err := projectIdle(state); err != nil {
				return err
			}
			configEntry := slices.IndexFunc(configured, func(entry config.PluginEntry) bool { return entry.Source == source })
			if err := sandbox.EditProjectPlugins(state, target.agent, func(entries []sandbox.PluginEntryJSON) []sandbox.PluginEntryJSON {
				entries = slices.DeleteFunc(entries, func(entry sandbox.PluginEntryJSON) bool { return sameSource(entry.Source, source) })
				// A config entry in its configured state needs no project entry.
				if configEntry >= 0 && configured[configEntry].IsEnabled() == enabled {
					return entries
				}
				return append(entries, sandbox.PluginEntryJSON{Source: source, Enabled: enabled})
			}); err != nil {
				return err
			}
			word := "off"
			if enabled {
				word = "on"
			}
			fmt.Fprintf(command.OutOrStdout(), "Switched %s %s for this project.\n", source, word)
			return nil
		},
	}
	addPluginsFlags(command, &target)
	return command
}

func pluginsFetchCommand() *cobra.Command {
	var options sandboxOptions
	command := &cobra.Command{
		Use:   "fetch",
		Short: "Fetch a project's missing plugins now, with the agent's own installer",
		Long: `Starts the project's sandbox briefly with the network to fetch the plugins
that are not fetched yet, or whose fetch failed. Run it where there is
internet access, e.g. on a cluster's login node; a sandbox started later on a
compute node, or with --isolate-network, then needs no network for them.
With --isolate-network the fetch reaches only the hosts the agent's
installer needs. A running agent picks the plugins up when it restarts.`,
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := options.check(); err != nil {
				return err
			}
			if !sandbox.HasPlugins(options.agent) {
				return fmt.Errorf("%s has no plugins in code flower yet", sandbox.AgentLabel(options.agent))
			}
			engine, err := options.chooseEngine(command.Context())
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(command.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			prepared, err := options.prepare(ctx, engine, "a plugin fetch")
			if err != nil {
				return err
			}
			worker := prepared.worker(options.agent)
			worker.Stdout = nil
			results, err := worker.FetchPlugins(ctx)
			sandbox.ReportFetch(os.Stderr, results)
			if err != nil {
				return err
			}
			if len(results) == 0 {
				fmt.Fprintln(os.Stderr, "Nothing to fetch.")
			}
			for _, result := range results {
				if result.Error != "" {
					return exitError{1}
				}
			}
			return nil
		},
	}
	addSandboxFlags(command, &options)
	addAgentFlag(command, &options, "agent whose plugins are fetched", true)
	return command
}
