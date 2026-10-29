package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"codeflower-launcher/internal/sandbox"
)

// interactive says whether the user can answer questions on this terminal.
func interactive() bool {
	info, err := os.Stdin.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
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

func runCommand() *cobra.Command {
	var options commonOptions
	var agent string
	var folders sandbox.Folders
	var bindValues []string
	var listenHost, advertiseHost string
	var port, inspectPort int

	command := &cobra.Command{
		Use:   "run",
		Short: "Start a sandbox (worker) with an agent",
		Long: `Start a sandbox for a project with one agent. Each folder is mounted in a
subfolder named after it: data folders read-only in /data/input, output
folders read-write in /data/output, and code folders (your repositories and
packages, which the agent may change) read-write in /data/code.

It prints a codeflower://… line: in VS Code, run "code flower: Connect to
worker" and paste it. If the agent is not signed in for this project yet,
VS Code offers its sign-in after connecting. Press Ctrl+C to stop.`,
		Example: `  codeflower run --agent claude --input ~/data/study42 --output ~/results/study42
  codeflower run --agent claude --input ~/data/rna --input ~/data/atac --code ~/src/mypkg
  codeflower run --agent codex --project study42 --input /data/study42
  codeflower run --agent claude --image ./codeflower-sandbox.sif --state ~/cf/study42 --bind /data/ref:/reference`,
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if agent == "" && interactive() {
				chosen, err := askAgent(os.Stdin, os.Stderr)
				if err != nil {
					return err
				}
				agent = chosen
			}
			if !slices.Contains(sandbox.Agents, agent) {
				return fmt.Errorf("--agent must be one of: %s", strings.Join(sandbox.Agents, ", "))
			}
			if err := options.validate(); err != nil {
				return err
			}
			if port < 0 || port > 65535 {
				return fmt.Errorf("--port must be between 0 and 65535")
			}
			if inspectPort < 0 || inspectPort > 65535 {
				return fmt.Errorf("--inspect-port must be between 0 and 65535")
			}
			binds, err := collectBinds(folders, bindValues)
			if err != nil {
				return err
			}

			engine, err := sandbox.ChooseEngine(sandbox.DetectEngines(command.Context()), options.engine, options.image)
			if err != nil {
				return engineMissing(os.Stderr, engine, err)
			}
			if inspectPort > 0 && engine.Name != sandbox.EngineDocker {
				return fmt.Errorf("--inspect-port needs --engine docker")
			}
			image, err := sandbox.ResolveImage(options.image, engine.Name)
			if err != nil {
				return err
			}
			state, err := sandbox.PrepareState(options.stateDirectory())
			if err != nil {
				return err
			}

			fmt.Fprintf(os.Stderr, "Starting a code flower sandbox for %s with %s (%s)\n", sandbox.AgentLabel(agent), engine.Label, image)
			fmt.Fprintf(os.Stderr, "  project  %s\n", state)
			for _, bind := range binds {
				mode := "read-write"
				if bind.ReadOnly {
					mode = "read-only"
				}
				fmt.Fprintf(os.Stderr, "  %-20s %s (%s)\n", bind.Target, bind.Source, mode)
			}
			ctx, stop := signal.NotifyContext(command.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			code, err := sandbox.Worker{
				Agent:         agent,
				Engine:        engine,
				Image:         image,
				State:         state,
				Binds:         binds,
				ListenHost:    listenHost,
				AdvertiseHost: advertiseHost,
				Port:          port,
				InspectPort:   inspectPort,
			}.Run(ctx, nil)
			if err != nil {
				if errors.Is(err, sandbox.ErrNoImage) {
					return fmt.Errorf("%w\nSee `codeflower check` for what this machine has", err)
				}
				return err
			}
			if code != 0 {
				return exitError{code}
			}
			return nil
		},
	}
	addCommonFlags(command, &options)
	command.Flags().StringVar(&agent, "agent", os.Getenv("CODEFLOWER_AGENT"),
		"agent the sandbox runs: claude, codex, or dsh (or set $CODEFLOWER_AGENT; asked if left out)")
	command.Flags().StringArrayVarP(&folders.Inputs, "input", "i", nil,
		"folder with your data, mounted read-only at /data/input/NAME (repeatable)")
	command.Flags().StringArrayVarP(&folders.Outputs, "output", "o", nil,
		"folder for results, created if needed and mounted read-write at /data/output/NAME (repeatable)")
	command.Flags().StringArrayVarP(&folders.Code, "code", "c", nil,
		"repository or package the agent develops, mounted read-write at /data/code/NAME (repeatable)")
	command.Flags().StringArrayVar(&bindValues, "bind", nil,
		"more mounts: SOURCE[:TARGET[:ro|rw]] (repeatable; default target /data/input/NAME, read-only)")
	command.Flags().StringVar(&listenHost, "host", "0.0.0.0", "interface the worker listens on")
	command.Flags().StringVar(&advertiseHost, "advertise-host", "",
		"address printed for VS Code (default: this node's IPv4 address; 127.0.0.1 with Docker or Podman)")
	command.Flags().IntVar(&port, "port", 0, "WebSocket port (default: any free port)")
	// Development only: anyone who can reach the inspector can run code in the sandbox.
	command.Flags().IntVar(&inspectPort, "inspect-port", 0,
		"publish the bridge's Node inspector on 127.0.0.1:PORT (development, Docker only)")
	_ = command.Flags().MarkHidden("inspect-port")
	return command
}

// collectBinds turns --input, --output, --code and --bind into mounts.
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
