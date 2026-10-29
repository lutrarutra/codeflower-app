package cli

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"codeflower-launcher/internal/sandbox"
)

func runCommand() *cobra.Command {
	var options sandboxOptions
	var listenHost, advertiseHost string
	var port, inspectPort int
	var promptFile string

	command := &cobra.Command{
		Use:   "run (TASK | --prompt FILE)",
		Short: "Start a task in a session you can continue from VS Code",
		Long: `Start a sandbox and create a new chat session with TASK as its first
prompt: the request in plain words, - to read it from standard input, or
--prompt FILE to read it from a Markdown file. The agent starts working
immediately; connect to the worker in VS Code to follow along, answer
questions, or continue the session. Unlike "task", this command keeps the
worker running for interactive follow-up.

The session is saved in the project and appears in the extension's sessions
list. If the agent is not signed in yet, sign in with "codeflower interactive"
first.

It prints a codeflower://… line: in VS Code, run "code flower: Connect to
worker" and paste it. Press Ctrl+C to stop.`,
		Example: `  codeflower run --agent claude --input ~/data/study42 --output ~/results/study42 \
    "Cluster the cells and annotate the cell types"
  codeflower run --agent codex --project study42 --input /data/study42 \
    "Run quality control and write the filtered matrix"
  codeflower run --agent claude --project study42 --input /data/study42 --prompt task.md`,
		Args: func(command *cobra.Command, args []string) error {
			return checkPromptArgs(args, promptFile, true, "task")
		},
		RunE: func(command *cobra.Command, args []string) error {
			text, _, err := readPrompt(args, promptFile, os.Stdin, "task")
			if err != nil {
				return err
			}
			if err := options.check(); err != nil {
				return err
			}
			if port < 0 || port > 65535 {
				return fmt.Errorf("--port must be between 0 and 65535")
			}
			if inspectPort < 0 || inspectPort > 65535 {
				return fmt.Errorf("--inspect-port must be between 0 and 65535")
			}
			if options.isolateNetwork && inspectPort > 0 {
				return fmt.Errorf("--inspect-port cannot be used with --isolate-network")
			}
			engine, err := options.chooseEngine(command.Context())
			if err != nil {
				return err
			}
			if inspectPort > 0 && engine.Name != sandbox.EngineDocker {
				return fmt.Errorf("--inspect-port needs --engine docker")
			}
			ctx, stop := signal.NotifyContext(command.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			prepared, err := options.prepare(ctx, engine, "a code flower run")
			if err != nil {
				return err
			}

			id := sandbox.NewInstanceID()
			if err := sandbox.WriteRunPrompt(prepared.state, id, text); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "  task     %s\n", summarize(text))

			worker := prepared.worker(options.agent)
			worker.InitialPrompt = id
			worker.ListenHost = listenHost
			worker.AdvertiseHost = advertiseHost
			worker.Port = port
			worker.InspectPort = inspectPort
			code, err := worker.Run(ctx, nil)
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
	addSandboxFlags(command, &options)
	command.Flags().StringVar(&promptFile, "prompt", "", "read TASK from this Markdown file instead of an argument")
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
