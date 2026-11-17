package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"

	"codeflower-app/internal/config"
	"codeflower-app/internal/sandbox"
)

func sandboxCommand() *cobra.Command {
	var options sandboxOptions
	var flags workerFlags

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

Each start saves how the project started in PROJECT.sandbox.yaml next to the
project folder: its folders, engine, image, network, port and the key and TLS
certificate that its links carry. "codeflower sandbox --continue PROJECT"
starts it again with them, on the same port and with the same key, so VS Code
and saved page links reconnect; it fails if that port is taken. Flags given
with --continue replace the saved settings, or add folders to the saved ones,
and are saved for next time. The file is YAML, readable only by you: edit it
to change or remove a setting while the sandbox is stopped. --new-key makes
the links saved before stop working. Started without --continue, a project
still keeps its key and certificate, and its saved port unless --port is
given or the port is taken, so VS Code's saved connection still works. See
"codeflower projects" to list, copy, move or remove projects.

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
  codeflower sandbox --continue study42
  codeflower sandbox --continue study42 --input ~/data/study42-batch2
  codeflower sandbox --continue study42 --create --input ~/data/study42 --output ~/results/study42
  codeflower sandbox --input ~/data/study42 --skills ~/my-skills --isolate-network
  codeflower sandbox --image ./codeflower-sandbox.sif --state ~/cf/study42 --bind /data/ref:/reference`,
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			given := command.Flags().Changed
			if flags.create && flags.continueFrom == "" {
				return fmt.Errorf("--create needs --continue")
			}
			if flags.continueFrom != "" {
				for _, name := range []string{"project", "state"} {
					if given(name) {
						return fmt.Errorf("--%s cannot be used with --continue, which names the project", name)
					}
				}
				saved, found, err := options.continueProject(flags.continueFrom, flags.create)
				if err != nil {
					return err
				}
				if found {
					options.applySettings(saved, &flags)
				}
			}
			if err := options.check(); err != nil {
				return err
			}
			if flags.port < 0 || flags.port > 65535 {
				return fmt.Errorf("--port must be between 0 and 65535")
			}
			if flags.inspectPort < 0 || flags.inspectPort > 65535 {
				return fmt.Errorf("--inspect-port must be between 0 and 65535")
			}
			if options.isolateNetwork && flags.inspectPort > 0 {
				return fmt.Errorf("--inspect-port cannot be used with --isolate-network")
			}
			engine, err := options.chooseEngine(command.Context())
			if err != nil {
				return err
			}
			if flags.inspectPort > 0 && engine.Name != sandbox.EngineDocker {
				return fmt.Errorf("--inspect-port needs --engine docker")
			}
			if flags.port > 0 {
				if err := sandbox.CheckPort(portHost(engine.Name, flags.listenHost), flags.port); err != nil {
					hint := "choose another --port, or --port 0 for any free one"
					if options.fromSettings["port"] {
						hint = "the project's saved settings ask for it; stop what uses it, or give --port 0 for any free port (saved for next time)"
					}
					return fmt.Errorf("%w: %s", err, hint)
				}
			}
			ctx, kill, stop := stopOnSignals(command.Context())
			defer stop()
			prepared, err := options.prepare(ctx, engine, "a code flower sandbox")
			if err != nil {
				return err
			}

			// The project's settings: how it started, for --continue, and its
			// key and certificate, kept so saved links connect after a restart.
			settingsFile := sandbox.SettingsFile(prepared.state)
			settings, err := sandbox.ReadSettings(settingsFile)
			if err != nil && !errors.Is(err, sandbox.ErrNoSettings) {
				return fmt.Errorf("%w\nFix the file, or remove it to start the project afresh", err)
			}
			certificate, err := projectKey(&settings, flags.newKey)
			if err != nil {
				return fmt.Errorf("%s: %w; give --new-key for a new key and certificate", settingsFile, err)
			}
			if flags.continueFrom == "" && !given("port") {
				flags.port = savedPort(settings.Port, engine.Name, flags.listenHost)
			}
			settings = options.settingsOf(settings, engine.Name, flags)
			projectName := filepath.Base(prepared.state)
			continueWith := projectName
			if options.state != "" {
				continueWith = prepared.state
			} else if given("projects-dir") {
				continueWith += " --projects-dir " + options.projectsDir
			}
			if err := sandbox.WriteSettings(settingsFile, projectName, settings); err != nil {
				return fmt.Errorf("could not save the project's settings: %w", err)
			}
			fmt.Fprintf(os.Stderr, "  settings %s (start it again with: codeflower sandbox --continue %s)\n", settingsFile, continueWith)

			worker := prepared.worker(options.agent)
			worker.ListenHost = flags.listenHost
			worker.AdvertiseHost = flags.advertiseHost
			worker.Port = flags.port
			worker.Token = settings.Key
			worker.Certificate = &certificate
			worker.InspectPort = flags.inspectPort
			worker.ServePage = flags.servePage
			worker.NoTunnel = flags.noTunnel
			worker.Kill = kill
			var tunnelReady func(sandbox.Connection)
			if !flags.noTunnel {
				tunnelReady = tunnelLinks(config.Directory(options.config), os.Stderr)
			}
			ready := func(connection sandbox.Connection) {
				// The port the worker took, for its links to connect next time.
				if connection.Port > 0 && connection.Port != settings.Port {
					settings.Port = connection.Port
					if err := sandbox.WriteSettings(settingsFile, projectName, settings); err != nil {
						fmt.Fprintf(os.Stderr, "Could not save the port in %s: %v\n", settingsFile, err)
					}
				}
				if tunnelReady != nil {
					tunnelReady(connection)
				}
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
	command.Flags().StringVar(&flags.listenHost, "host", "0.0.0.0", "interface the worker listens on (Apptainer; Docker and Podman always publish on 127.0.0.1)")
	command.Flags().BoolVar(&flags.servePage, "server", false,
		"also serve the chat page on the worker's port, and print a link to open it in a browser")
	command.Flags().StringVar(&flags.advertiseHost, "advertise-host", "",
		"address printed for VS Code and the page link (default: this node's IPv4 address; 127.0.0.1 with Docker or Podman)")
	command.Flags().BoolVar(&flags.noTunnel, "no-tunnel", false,
		"keep this sandbox from browsers paired through the tunnel (it does not start the client for the tunnel either)")
	command.Flags().IntVar(&flags.port, "port", 0, "port of the worker (and its page with --server) (default: any free port); fails if it is taken")
	command.Flags().StringVar(&flags.continueFrom, "continue", "",
		"start a project again with its saved settings: a project name, or a project folder (flags given replace settings, or add folders)")
	command.Flags().BoolVar(&flags.create, "create", false,
		"with --continue: start the project new from the flags given if it does not exist yet (a script can then always use --continue)")
	command.Flags().BoolVar(&flags.newKey, "new-key", false,
		"give the project a new key and TLS certificate: links saved before no longer connect")
	// Development only: anyone who can reach the inspector can run code in the sandbox.
	command.Flags().IntVar(&flags.inspectPort, "inspect-port", 0,
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

// portHost is the interface the worker's port is opened on: Docker and
// Podman publish it on 127.0.0.1, Apptainer listens on --host.
func portHost(engine, listenHost string) string {
	if engine == sandbox.EngineApptainer {
		return listenHost
	}
	return "127.0.0.1"
}

// savedPort is the port a project started on last time, for a start
// without --continue or --port, so VS Code's saved connection reconnects:
// if it is taken, any free port (0) instead, since no one asked for it.
func savedPort(port int, engine, listenHost string) int {
	if port <= 0 {
		return 0
	}
	if err := sandbox.CheckPort(portHost(engine, listenHost), port); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: the project's saved %v, so the sandbox takes another port.\n"+
			"VS Code's saved connection will not reach it: connect with the new link.\n", err)
		return 0
	}
	return port
}
