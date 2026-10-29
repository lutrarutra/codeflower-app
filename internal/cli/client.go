package cli

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/spf13/cobra"

	"codeflower-launcher/internal/client"
	"codeflower-launcher/internal/config"
)

func clientCommand() *cobra.Command {
	var address string
	var port int
	var noBrowser bool
	var connectOnly bool
	var uiDirectory string
	var configDirectory string
	command := &cobra.Command{
		Use:     "client",
		Aliases: []string{"gui"},
		Short:   "Open the code flower client to start sandboxes and connect to them",
		Long: `Open the code flower client in your browser. From it you start sandboxes on
this computer and connect to sandboxes running elsewhere (a batch job, another
computer) by the codeflower:// line they printed.

Starting sandboxes needs Docker, Podman or Apptainer; without one the client
explains how to install it and still connects to sandboxes elsewhere. With
--connect-only (or "client: {connect_only: true}" in config.yaml) it never
starts sandboxes, e.g. on a cluster's login node.

The client is served only to this computer. On a remote machine, forward its
port with ssh -L and open it on yours.`,
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			listenAddress, err := clientAddress(address, port, command.Flags().Changed("address"), command.Flags().Changed("port"))
			if err != nil {
				return err
			}
			directory := config.Directory(configDirectory)
			if loaded, err := config.Load(directory); err == nil && loaded.Client.ConnectOnly {
				connectOnly = true
			}
			ctx, stop := signal.NotifyContext(command.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return client.Run(ctx, client.Options{
				Address:           listenAddress,
				ProjectsDirectory: ProjectsDirectory(),
				ConfigDirectory:   directory,
				Version:           Version,
				OpenBrowser:       !noBrowser,
				Log:               os.Stderr,
				Image:             os.Getenv("CODEFLOWER_IMAGE"),
				ImageTag:          os.Getenv("CODEFLOWER_IMAGE_TAG"),
				ConnectOnly:       connectOnly,
				UIDirectory:       uiDirectory,
				Ready: func(address string) {
					fmt.Fprintf(os.Stderr, "code flower client is open at:\n\n  %s\n\n", address)
					if hint := forwardingHint(address); hint != "" {
						fmt.Fprint(os.Stderr, hint)
					}
					if connectOnly {
						fmt.Fprintln(os.Stderr, "Keep this running while you use your sandboxes; press Ctrl+C to quit.")
					} else {
						fmt.Fprintln(os.Stderr, "Keep this running while you use your sandboxes; press Ctrl+C to stop the ones it started and quit.")
					}
					fmt.Fprintln(os.Stderr, "For the command line instead, run: codeflower --help")
				},
			})
		},
	}
	command.Flags().StringVar(&address, "address", "127.0.0.1:0", "listener address (mutually exclusive with --port)")
	command.Flags().IntVar(&port, "port", 0, "port to serve the client on (default: auto; 0 selects any free port)")
	command.Flags().BoolVar(&noBrowser, "no-browser", false, "print the client's address instead of opening a browser")
	command.Flags().BoolVar(&connectOnly, "connect-only", false, "only connect to sandboxes running elsewhere; never start one here")
	command.Flags().StringVar(&uiDirectory, "ui", "", "serve the page from this folder instead of the built-in one (development)")
	_ = command.Flags().MarkHidden("ui")
	command.Flags().StringVar(&configDirectory, "config", "",
		"config directory with config.yaml and skills (or set $CODEFLOWER_CONFIG; default ~/.codeflower)")
	return command
}

func clientAddress(address string, port int, addressSet, portSet bool) (string, error) {
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

// forwardingHint tells a user logged in over SSH how to open the client on
// their own computer.
func forwardingHint(address string) string {
	if os.Getenv("SSH_CONNECTION") == "" {
		return ""
	}
	parsed, err := url.Parse(address)
	if err != nil || parsed.Port() == "" {
		return ""
	}
	host, err := os.Hostname()
	if err != nil {
		host = "this-host"
	}
	port := parsed.Port()
	local := *parsed
	local.Host = "localhost:" + port
	return fmt.Sprintf("You are logged in over SSH. On your own computer, run\n\n  ssh -N -L %s:localhost:%s %s\n\nand open %s\n\n",
		port, port, host, local.String())
}
