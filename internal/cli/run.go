package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"codeflower-app/internal/config"
	"codeflower-app/internal/sandbox"
)

func runCommand() *cobra.Command {
	var options sandboxOptions
	var listenHost, advertiseHost string
	var port, inspectPort int
	var servePage bool
	var noTunnel bool
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
list. If the agent is not signed in yet, the task waits until you connect
and sign in.

It prints a link to open in the browser while "codeflower client" runs on
your computer, which adds the sandbox there; or, in VS Code, run "code
flower: Connect to worker" and paste the codeflower://… line. Press Ctrl+C to stop.

With --server the sandbox also serves its chat page on the same port and
prints a link to it (http://HOST:PORT/?key=…), like Jupyter's: open it in any
browser that reaches this machine, e.g. through ssh -L. Anyone with the link
controls the agent, and plain HTTP carries the key and the chats unencrypted,
so keep job logs private and prefer --host 127.0.0.1 with a tunnel on shared
networks.`,
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
			ctx, kill, stop := stopOnSignals(command.Context())
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
			worker.ServePage = servePage
			worker.NoTunnel = noTunnel
			worker.Kill = kill
			var ready func(sandbox.Connection)
			if !noTunnel {
				ready = tunnelLinks(config.Directory(options.config), os.Stderr)
			}
			code, err := worker.Run(ctx, ready)
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
	addAgentFlag(command, &options, "agent that starts on the task; the sandbox runs every agent", false)
	command.Flags().StringVar(&promptFile, "prompt", "", "read TASK from this Markdown file instead of an argument")
	command.Flags().StringVar(&listenHost, "host", "0.0.0.0", "interface the worker listens on (Apptainer; Docker and Podman always publish on 127.0.0.1)")
	command.Flags().BoolVar(&servePage, "server", false,
		"also serve the chat page on the worker's port, and print a link to open it in a browser")
	command.Flags().StringVar(&advertiseHost, "advertise-host", "",
		"address printed for VS Code and the page link (default: this node's IPv4 address; 127.0.0.1 with Docker or Podman)")
	command.Flags().BoolVar(&noTunnel, "no-tunnel", false,
		"keep this sandbox from browsers paired through the tunnel (it does not start the client for the tunnel either)")
	command.Flags().IntVar(&port, "port", 0, "port of the worker (and its page with --server) (default: any free port)")
	// Development only: anyone who can reach the inspector can run code in the sandbox.
	command.Flags().IntVar(&inspectPort, "inspect-port", 0,
		"publish the bridge's Node inspector on 127.0.0.1:PORT (development, Docker only)")
	_ = command.Flags().MarkHidden("inspect-port")
	return command
}
