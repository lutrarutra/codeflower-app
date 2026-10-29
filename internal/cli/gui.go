package cli

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/spf13/cobra"

	"codeflower-launcher/internal/gui"
)

func guiCommand() *cobra.Command {
	var address string
	var port int
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
			listenAddress, err := guiAddress(address, port, command.Flags().Changed("address"), command.Flags().Changed("port"))
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(command.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return gui.Run(ctx, gui.Options{
				Address:           listenAddress,
				ProjectsDirectory: ProjectsDirectory(),
				Version:           Version,
				OpenBrowser:       !noBrowser,
				Log:               os.Stderr,
				Image:             os.Getenv("CODEFLOWER_IMAGE"),
				ImageTag:          os.Getenv("CODEFLOWER_IMAGE_TAG"),
				Ready: func(url string) {
					fmt.Fprintf(os.Stderr, "code flower launcher is open at:\n\n  %s\n\n", url)
					fmt.Fprintln(os.Stderr, "Keep this running while you use your sandboxes; press Ctrl+C to stop them and quit.")
					fmt.Fprintln(os.Stderr, "For the command line instead, run: codeflower --help")
				},
			})
		},
	}
	command.Flags().StringVar(&address, "address", "127.0.0.1:0", "listener address (mutually exclusive with --port)")
	command.Flags().IntVar(&port, "port", 0, "port to serve the window on (default: auto; 0 selects any free port)")
	command.Flags().BoolVar(&noBrowser, "no-browser", false, "print the window's address instead of opening a browser")
	return command
}

func guiAddress(address string, port int, addressSet, portSet bool) (string, error) {
	if port < 0 || port > 65535 {
		return "", fmt.Errorf("--port must be between 0 and 65535")
	}
	if addressSet && portSet {
		return "", fmt.Errorf("--address cannot be used with --port")
	}
	if portSet {
		return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), nil
	}
	return address, nil
}
