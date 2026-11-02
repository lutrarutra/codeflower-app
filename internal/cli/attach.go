package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"codeflower-app/internal/attach"
	"codeflower-app/internal/config"
)

func attachCommand() *cobra.Command {
	var options attach.Options
	command := &cobra.Command{
		Use:   "attach [SANDBOX] [CHAT]",
		Short: "Open a sandbox's chat in the agent's own terminal UI",
		Long: `Open a chat of a running sandbox in the agent's own terminal UI, in this
terminal. The terminal UI runs in the sandbox and keeps running when you
detach, so you can come back to it, also from another terminal, and the
chat in VS Code or the app's page follows what you do there.

SANDBOX is a sandbox running on this computer (its project name, its id
as the app's page shows it, e.g. local-1a2b3c4d, or a number from
"codeflower chats"), one added to "codeflower client", or the
codeflower://… line a sandbox printed. With one sandbox it can be left out.
CHAT is a chat's number from "codeflower chats", the start of its title, or
its key; without it (and without --new or --last) you choose from a list.

After Enter, type ~. to detach, ~s to switch to another chat, ~q to quit the
terminal UI, ~l for links, ~o to open the file the agent showed last, or ~?
for help.

Files the agent shows (codeflower-show) appear as links that open them in
your browser, in the page of "codeflower client": on this computer's page,
or over SSH through remote access (codeflower tunnel pair) when it is on.

OpenCode's and Codex's terminal UIs share the chat with VS Code and the
app's page: each shows and can continue it. Claude Code's takes the chat
over while it runs (after the chat's current turn): the page and VS Code
follow it, and what they send is typed into it, until you quit it (~q), when
they have the chat again. DeepSeek Harness has no terminal UI.`,
		Example: `  codeflower attach
  codeflower attach study42 --last
  codeflower attach study42 "PCA of batch 3"
  codeflower attach study42 --new
  ssh -t login-node codeflower attach study42 --last`,
		Args: cobra.MaximumNArgs(2),
		RunE: func(command *cobra.Command, args []string) error {
			if len(args) > 0 {
				options.Sandbox = args[0]
			}
			if len(args) > 1 {
				options.Chat = args[1]
			}
			if options.New && (options.Last || options.Chat != "") {
				return fmt.Errorf("--new starts a chat of its own: leave out CHAT and --last")
			}
			switch options.Links {
			case attach.LinksAuto, attach.LinksLocal, attach.LinksTunnel, attach.LinksPath:
			default:
				return fmt.Errorf("--links is auto, local, tunnel or path, not %q", options.Links)
			}
			options.Config = config.Directory(options.Config)
			options.In, options.Out, options.Err = os.Stdin, os.Stdout, command.ErrOrStderr()
			ctx, stop := signal.NotifyContext(command.Context(), syscall.SIGTERM, syscall.SIGHUP)
			defer stop()
			return attach.Run(ctx, options)
		},
	}
	command.Flags().BoolVar(&options.New, "new", false, "start a new chat")
	command.Flags().BoolVar(&options.Last, "last", false, "open the chat that changed last")
	command.Flags().BoolVar(&options.Watch, "watch", false, "only watch: what you type is not sent")
	command.Flags().StringVar(&options.Links, "links", attach.LinksAuto,
		"where the files the agent shows open: auto (this computer's page, through remote access over SSH), local, tunnel, or path (no links)")
	command.Flags().StringVar(&options.Config, "config", "",
		"config directory, whose running client's page the links open (or set $CODEFLOWER_CONFIG; default ~/.codeflower)")
	return command
}

func chatsCommand() *cobra.Command {
	var asJSON bool
	command := &cobra.Command{
		Use:   "chats [SANDBOX]",
		Short: "List a sandbox's chats, for codeflower attach",
		Long: `List the chats of a running sandbox, newest first: their number and key
for "codeflower attach", their title, when they last changed, whether they
work or wait for you, and whether their terminal UI runs.

Without SANDBOX, and with more than one running, it lists the sandboxes.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(command.Context(), 30*time.Second)
			defer cancel()
			listing, err := attach.ListChats(ctx, firstArgument(args))
			if err != nil {
				return err
			}
			out := command.OutOrStdout()
			if asJSON {
				return json.NewEncoder(out).Encode(map[string]any{"sandbox": listing.Target.ID, "agent": listing.Agent, "chats": listing.Chats})
			}
			fmt.Fprintf(out, "%s · %s\n", listing.Target.Label(), listing.Agent)
			attach.PrintChats(out, listing.Chats, "")
			return nil
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "print JSON, with the chats' keys")
	return command
}

func terminalsCommand() *cobra.Command {
	var asJSON bool
	var closing string
	command := &cobra.Command{
		Use:   "terminals [SANDBOX]",
		Short: "List the agent's terminal UIs running in a sandbox, or quit one",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(command.Context(), 30*time.Second)
			defer cancel()
			if closing != "" {
				return attach.CloseTerminal(ctx, firstArgument(args), closing)
			}
			listing, err := attach.ListTerminals(ctx, firstArgument(args))
			if err != nil {
				return err
			}
			out := command.OutOrStdout()
			if asJSON {
				return json.NewEncoder(out).Encode(map[string]any{"sandbox": listing.Target.ID, "agent": listing.Agent, "terminals": listing.Terminals})
			}
			if len(listing.Terminals) == 0 {
				fmt.Fprintf(out, "No terminal UI runs in %s.\n", listing.Target.Label())
				return nil
			}
			for _, terminal := range listing.Terminals {
				fmt.Fprintf(out, "%s  %-48s %d watching  since %s\n", terminal.TerminalID, terminal.SessionKey, terminal.Viewers, terminal.StartedAt)
			}
			return nil
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	command.Flags().StringVar(&closing, "close", "", "quit the terminal UI with this id or chat key")
	return command
}

func firstArgument(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	return ""
}
