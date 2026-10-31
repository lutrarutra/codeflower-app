package cli

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"codeflower-app/internal/sandbox"
)

func interactiveCommand() *cobra.Command {
	var options sandboxOptions
	var listenHost, advertiseHost string
	var port, inspectPort int
	var servePage bool

	command := &cobra.Command{
		Use:   "interactive",
		Short: "Start an interactive sandbox (worker) to use from VS Code",
		Long: `Start a sandbox for a project with one agent, to work with from VS Code.
Each folder is mounted in a subfolder named after it: data folders read-only
in /data/input, output folders read-write in /data/output, code folders (your
repositories and packages, which the agent may change) read-write in
/data/code, and skills folders read-only in /data/skills, where the agent
loads them from.

With --isolate-network the sandbox has no network of its own: the app
is its only way out, and lets it reach only its agent's service and the hosts
in the allowlist file. Edit the file while the sandbox runs to allow more.

It prints a link to open in the browser while "codeflower client" runs on
your computer, which adds the sandbox there; or, in VS Code, run "code
flower: Connect to worker" and paste the codeflower://… line. If the agent
is not signed in for this project yet, the page or VS Code offers its sign-in
after connecting. Press Ctrl+C to stop.

With --server the sandbox also serves its chat page on the same port and
prints a link to it (http://HOST:PORT/?key=…), like Jupyter's: open it in any
browser that reaches this machine, e.g. through ssh -L. Anyone with the link
controls the agent, and plain HTTP carries the key and the chats unencrypted,
so keep job logs private and prefer --host 127.0.0.1 with a tunnel on shared
networks.

To start a session with an initial task and continue it in VS Code, see
"codeflower run". To let the agent work on a task by itself instead, see
"codeflower task".`,
		Example: `  codeflower interactive --agent claude --input ~/data/study42 --output ~/results/study42
  codeflower interactive --agent claude --input ~/data/rna --input ~/data/atac --code ~/src/mypkg
  codeflower interactive --agent codex --project study42 --input /data/study42
  codeflower interactive --agent claude --input ~/data/study42 --skills ~/my-skills --isolate-network
  codeflower interactive --agent claude --image ./codeflower-sandbox.sif --state ~/cf/study42 --bind /data/ref:/reference`,
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
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
			prepared, err := options.prepare(ctx, engine, "a code flower sandbox")
			if err != nil {
				return err
			}
			worker := prepared.worker(options.agent)
			worker.ListenHost = listenHost
			worker.AdvertiseHost = advertiseHost
			worker.Port = port
			worker.InspectPort = inspectPort
			worker.ServePage = servePage
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
	command.Flags().StringVar(&listenHost, "host", "0.0.0.0", "interface the worker listens on")
	command.Flags().BoolVar(&servePage, "server", false,
		"also serve the chat page on the worker's port, and print a link to open it in a browser")
	command.Flags().StringVar(&advertiseHost, "advertise-host", "",
		"address printed for VS Code and the page link (default: this node's IPv4 address; 127.0.0.1 with Docker or Podman)")
	command.Flags().IntVar(&port, "port", 0, "port of the worker (and its page with --server) (default: any free port)")
	// Development only: anyone who can reach the inspector can run code in the sandbox.
	command.Flags().IntVar(&inspectPort, "inspect-port", 0,
		"publish the bridge's Node inspector on 127.0.0.1:PORT (development, Docker only)")
	_ = command.Flags().MarkHidden("inspect-port")
	return command
}
