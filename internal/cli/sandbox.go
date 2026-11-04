package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"codeflower-app/internal/config"
	"codeflower-app/internal/sandbox"
)

func sandboxCommand() *cobra.Command {
	var options sandboxOptions
	var listenHost, advertiseHost string
	var port, inspectPort int
	var servePage bool
	var noTunnel bool

	command := &cobra.Command{
		Use:   "sandbox",
		Short: "Start a sandbox (worker) to use from VS Code or the browser",
		Long: `Start a sandbox for a project, to work with from VS Code or the browser.
Every agent runs in it: Claude Code, Codex, DeepSeek Harness and OpenCode;
each chat is one agent's, chosen when it starts.

Each folder is mounted in a subfolder named after it: data folders read-only
in /data/input, output folders read-write in /data/output, code folders (your
repositories and packages, which the agents may change) read-write in
/data/code, and skills folders read-only in /data/skills, where the agents
load them from.

With --isolate-network the sandbox has no network of its own: the app
is its only way out, and lets it reach only the agents' services and the
hosts in the allowlist file. Edit the file while the sandbox runs to allow more.

It prints a link to open in the browser while "codeflower client" runs on
your computer, which adds the sandbox there; or, in VS Code, run "code
flower: Connect to worker" and paste the codeflower://… line. Sign in to the
agents from the page's or VS Code's view of the sandbox; logins are kept in
the project. Press Ctrl+C to stop.

With --server the sandbox also serves its chat page on the same port and
prints a link to it (http://HOST:PORT/?key=…), like Jupyter's: open it in any
browser that reaches this machine, e.g. through ssh -L. Anyone with the link
controls the agent, and plain HTTP carries the key and the chats unencrypted,
so keep job logs private and prefer --host 127.0.0.1 with a tunnel on shared
networks.

To start a session with an initial task and continue it in VS Code, see
"codeflower run". To let the agent work on a task by itself instead, see
"codeflower task".`,
		Example: `  codeflower sandbox --input ~/data/study42 --output ~/results/study42
  codeflower sandbox --input ~/data/rna --input ~/data/atac --code ~/src/mypkg
  codeflower sandbox --project study42 --input /data/study42
  codeflower sandbox --input ~/data/study42 --skills ~/my-skills --isolate-network
  codeflower sandbox --image ./codeflower-sandbox.sif --state ~/cf/study42 --bind /data/ref:/reference`,
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
			ctx, kill, stop := stopOnSignals(command.Context())
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

// stopOnSignals ends ctx on the first Ctrl+C or SIGTERM, which stops the
// sandbox, and closes kill on the second, which kills it at once. stop ends
// the watch.
func stopOnSignals(parent context.Context) (ctx context.Context, kill <-chan struct{}, stop func()) {
	ctx, cancel := context.WithCancel(parent)
	killing := make(chan struct{})
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		for first := true; ; first = false {
			select {
			case <-done:
				return
			case <-signals:
			}
			if !first {
				close(killing)
				return
			}
			fmt.Fprintln(os.Stderr, "\nStopping the sandbox; press Ctrl+C again to kill it now.")
			cancel()
		}
	}()
	return ctx, killing, func() {
		signal.Stop(signals)
		close(done)
		cancel()
	}
}
