package cli

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"codeflower-launcher/internal/gui"
)

func guiCommand() *cobra.Command {
	var address string
	var noBrowser bool
	command := &cobra.Command{
		Use:   "gui",
		Short: "Open a window to start sandboxes without the command line",
		Long: `Open the launcher window in your browser. It checks that this computer can
run sandboxes (and explains how to install Docker or Podman if not), lets you
pick the agent and your input and output folders, and starts and stops
sandboxes. The window is served only to this computer.`,
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(command.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return gui.Run(ctx, gui.Options{
				Address:           address,
				ProjectsDirectory: ProjectsDirectory(),
				Version:           Version,
				OpenBrowser:       !noBrowser,
				Log:               os.Stderr,
				Image:             os.Getenv("CODEFLOWER_IMAGE"),
				Ready: func(url string) {
					fmt.Fprintf(os.Stderr, "code flower launcher is open at:\n\n  %s\n\n", url)
					fmt.Fprintln(os.Stderr, "Keep this running while you use your sandboxes; press Ctrl+C to stop them and quit.")
					fmt.Fprintln(os.Stderr, "For the command line instead, run: codeflower --help")
				},
			})
		},
	}
	command.Flags().StringVar(&address, "address", "127.0.0.1:0", "address to serve the window on")
	command.Flags().BoolVar(&noBrowser, "no-browser", false, "print the window's address instead of opening a browser")
	return command
}
