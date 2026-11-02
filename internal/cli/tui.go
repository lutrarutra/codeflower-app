package cli

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/moby/term"
	"github.com/spf13/cobra"

	"codeflower-app/internal/sandbox"
)

func tuiCommand() *cobra.Command {
	var options sandboxOptions
	var continueLast bool
	var resume string

	command := &cobra.Command{
		Use:   "tui",
		Short: "Work with the agent in its own terminal UI, in a sandbox that ends with it",
		Long: `Start a sandbox for a project and open the agent's own terminal UI in it,
in this terminal: Claude Code's, Codex's or OpenCode's. The sandbox ends when
the terminal UI does (quit it, e.g. /exit or Ctrl+C there), and nothing else
runs: no worker for VS Code or the app's page, no port, nothing to connect to.

Folders, network isolation, skills, the config's MCP servers and plugins, and
the project work as for "codeflower sandbox", and the agent signs in in its
own terminal UI, into the project (so a sandbox of the same project is
signed in too). The chats are the agent's own, kept in the project: open the
last one again with --continue, or choose one with --resume. They are not in
the chat list of the app's page or VS Code.

Files the agent shows (codeflower-show) have no page to open in here; to see
them in the app, use "codeflower sandbox" and "codeflower attach" instead.`,
		Example: `  codeflower tui --agent claude --input ~/data/study42
  codeflower tui --agent codex --project study42 --continue
  codeflower tui --agent opencode --project study42 --resume
  codeflower tui --agent claude --project study42 --resume=3f2a…`,
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := options.check(); err != nil {
				return err
			}
			if options.agent == "dsh" {
				return errors.New("DeepSeek Harness has no terminal UI: use `codeflower sandbox` and its chat")
			}
			if continueLast && resume != "" {
				return errors.New("--continue opens the last chat, --resume another: use one")
			}
			if !term.IsTerminal(os.Stdin.Fd()) || !term.IsTerminal(os.Stdout.Fd()) {
				return errors.New("tui needs a terminal: run it in one")
			}
			chat := "new"
			switch {
			case continueLast:
				chat = "last"
			case resume != "":
				chat = resume
			}
			engine, err := options.chooseEngine(command.Context())
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(command.Context(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
			defer stop()
			prepared, err := options.prepare(ctx, engine, "a code flower sandbox")
			if err != nil {
				return err
			}
			worker := prepared.worker(options.agent)
			worker.TUI = chat
			worker.TUIAgain = tuiAgain(os.Args[1:])
			worker.Stdin, worker.Stdout, worker.Stderr = os.Stdin, os.Stdout, os.Stderr
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
	command.Flags().BoolVar(&continueLast, "continue", false, "open the agent's last chat in the project")
	command.Flags().StringVar(&resume, "resume", "", "open a chat of the project: its id, or without one the agent's own list")
	command.Flags().Lookup("resume").NoOptDefVal = "pick"
	return command
}

// tuiAgain is the command line that ran, without the chat it opened, and
// with --resume= to be followed by the id of the chat to open again.
func tuiAgain(args []string) string {
	words := []string{"codeflower"}
	for _, arg := range args {
		if arg == "--continue" || strings.HasPrefix(arg, "--continue=") || arg == "--resume" || strings.HasPrefix(arg, "--resume=") {
			continue
		}
		words = append(words, shellQuote(arg))
	}
	return strings.Join(words, " ") + " --resume="
}
