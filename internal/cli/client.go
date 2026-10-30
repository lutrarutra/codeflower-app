package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"codeflower-app/internal/client"
	"codeflower-app/internal/config"
	"codeflower-app/internal/notify"
	"codeflower-app/internal/tray"
)

func clientCommand() *cobra.Command {
	var address string
	var port int
	var noBrowser bool
	var connectOnly bool
	var uiDirectory string
	var configDirectory string
	var noTray bool
	var noNotifications bool
	var desktop bool
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
port with ssh -L and open it on yours. Started again while it runs, it opens
the running client instead.

Where the desktop has a system tray (Windows, macOS's menu bar, Linux desktops
with StatusNotifierItem, e.g. KDE or GNOME with the AppIndicator extension),
the client also shows an icon there that opens the page and quits; --no-tray
(or "client: {tray: false}") turns it off.

When a chat in a connected sandbox asks a question, wants a permission, or
ends its turn, the client tells the desktop (Linux desktops with a
notification service; macOS and Windows with the icon); a click opens the
sandbox in VS Code. --no-notifications (or "client: {notifications: false}")
turns this off.

Quitting (Ctrl+C, the page, or the tray) first asks the chats working in the
sandboxes started here to save their work and wrap up, and stops each sandbox
when they are done, at most after 10 minutes ("client: {wrap_up_minutes: N}").
Ctrl+C again, or Stop now, stops them at once; the page and the tray can also
call the quit off. Sandboxes running elsewhere keep running.`,
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			addressSet, portSet := command.Flags().Changed("address"), command.Flags().Changed("port")
			listenAddress, err := clientAddress(address, port, addressSet, portSet)
			if err != nil {
				return err
			}
			directory := config.Directory(configDirectory)
			loaded, err := config.Load(directory)
			if err != nil {
				loaded = &config.Config{}
			}
			if loaded.Client.ConnectOnly {
				connectOnly = true
			}
			// A second start (e.g. another double-click) opens the running client.
			if !addressSet && !portSet {
				if running, ok := client.Running(directory); ok {
					fmt.Fprintf(os.Stderr, "code flower client is already running at:\n\n  %s\n\n", running)
					if !noBrowser {
						if err := client.OpenBrowser(running); err != nil {
							fmt.Fprintf(os.Stderr, "Could not open a browser (%v); open the address above yourself.\n", err)
						}
					}
					return nil
				}
			}
			showTray := !noTray && (loaded.Client.Tray == nil || *loaded.Client.Tray) && tray.Available() == nil
			// Started from the desktop with a tray, there is no terminal to
			// write to: the log goes to a file in the config directory.
			var log io.Writer = os.Stderr
			if desktop && showTray {
				if file, err := openClientLog(directory); err == nil {
					defer file.Close()
					log = file
				}
			}
			signals := make(chan os.Signal, 2)
			signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
			defer signal.Stop(signals)
			controls := make(chan *client.Control, 1)
			options := client.Options{
				Address:           listenAddress,
				ProjectsDirectory: ProjectsDirectory(),
				ConfigDirectory:   directory,
				Version:           Version,
				OpenBrowser:       !noBrowser,
				Log:               log,
				Image:             os.Getenv("CODEFLOWER_IMAGE"),
				ImageTag:          os.Getenv("CODEFLOWER_IMAGE_TAG"),
				ConnectOnly:       connectOnly,
				UIDirectory:       uiDirectory,
				WrapUp:            time.Duration(loaded.Client.WrapUpMinutes) * time.Minute,
				Ready: func(address string) {
					fmt.Fprintf(log, "code flower client is open at:\n\n  %s\n\n", address)
					if hint := forwardingHint(address); hint != "" {
						fmt.Fprint(log, hint)
					}
					if connectOnly {
						fmt.Fprintln(log, "Keep this running while you use your sandboxes; press Ctrl+C to quit.")
					} else {
						fmt.Fprintln(log, "Keep this running while you use your sandboxes; press Ctrl+C to stop the ones it started and quit.")
					}
					fmt.Fprintln(log, "For the command line instead, run: codeflower --help")
				},
				Started: func(control *client.Control) {
					go client.HandleSignals(control, signals, log)
					controls <- control
				},
			}
			openLink := func(url string) { _ = client.OpenBrowser(url) }
			var notices chan tray.Notice
			if !noNotifications && (loaded.Client.Notifications == nil || *loaded.Client.Notifications) {
				if notifier, err := notify.New(openLink); err == nil {
					defer notifier.Close()
					options.Notify = func(notice client.Notice) {
						if err := notifier.Show(notify.Notice(notice)); err != nil {
							fmt.Fprintf(log, "Could not show a notification (%v)\n", err)
						}
					}
				} else if showTray && tray.Notifies() {
					notices = make(chan tray.Notice, 8)
					options.Notify = func(notice client.Notice) {
						select {
						case notices <- tray.Notice(notice):
						default:
							// The icon is behind (or gone): drop it.
						}
					}
				}
			}
			if !showTray {
				return client.Run(command.Context(), options)
			}
			return runWithTray(command.Context(), options, controls, notices, openLink, desktop)
		},
	}
	command.Flags().StringVar(&address, "address", "127.0.0.1:0", "listener address (mutually exclusive with --port)")
	command.Flags().IntVar(&port, "port", 0, "port to serve the client on (default: auto; 0 selects any free port)")
	command.Flags().BoolVar(&noBrowser, "no-browser", false, "print the client's address instead of opening a browser")
	command.Flags().BoolVar(&connectOnly, "connect-only", false, "only connect to sandboxes running elsewhere; never start one here")
	command.Flags().BoolVar(&noTray, "no-tray", false, "do not show an icon in the system tray or menu bar")
	command.Flags().BoolVar(&noNotifications, "no-notifications", false, "do not notify the desktop when a chat waits for an answer or ends its turn")
	// Set when the binary was double-clicked rather than run in a terminal.
	command.Flags().BoolVar(&desktop, "desktop", false, "")
	_ = command.Flags().MarkHidden("desktop")
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

// runWithTray serves the client while the tray icon holds the main
// goroutine, which macOS's menu bar needs; the icon goes once the client
// stops. Started from the desktop, the icon replaces the console window.
func runWithTray(ctx context.Context, options client.Options, controls <-chan *client.Control, notices <-chan tray.Notice,
	openLink func(string), desktop bool) error {
	finished := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		finished <- client.Run(ctx, options)
		close(done)
	}()
	var control atomic.Pointer[client.Control]
	status := make(chan tray.Status, 1)
	go func() {
		select {
		case current := <-controls:
			control.Store(current)
		case <-done:
			return
		}
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case status <- trayStatus(control.Load().Plan()):
			default:
			}
			select {
			case <-done:
				return
			case <-ticker.C:
			}
		}
	}()
	open := func(query string) {
		if current := control.Load(); current != nil {
			_ = client.OpenBrowser(current.URL + query)
		}
	}
	menu := tray.Menu{
		Tooltip: "code flower",
		Open:    func() { open("") },
		Quit: func() {
			current := control.Load()
			if current == nil {
				return
			}
			// Nothing to lose: quit. Otherwise the page asks, or shows the wrap-up.
			if plan := current.Plan(); plan.Sandboxes == 0 && plan.Quitting == nil {
				current.Quit(true)
				return
			}
			open("&quit=1")
		},
		StopNow: func() {
			if current := control.Load(); current != nil {
				current.StopNow()
			}
		},
		KeepRunning: func() {
			if current := control.Load(); current != nil {
				current.KeepRunning()
			}
		},
		OpenLink: openLink,
	}
	if desktop {
		hideConsole()
	}
	if err := tray.Run(menu, status, notices, done); err != nil {
		fmt.Fprintf(options.Log, "Could not show the tray icon (%v)\n", err)
	}
	return <-finished
}

// trayStatus is the line the tray menu shows.
func trayStatus(plan client.QuitPlan) tray.Status {
	switch {
	case plan.Stopping:
		return tray.Status{Text: "Stopping…"}
	case plan.Quitting != nil:
		waiting := 0
		for _, state := range plan.Quitting.Workers {
			if state == "asked" {
				waiting++
			}
		}
		return tray.Status{Text: fmt.Sprintf("Wrapping up: waiting for %s", counted(waiting, "sandbox", "sandboxes")), WrappingUp: true}
	case plan.Sandboxes == 0:
		return tray.Status{Text: "No sandboxes running here"}
	default:
		return tray.Status{Text: counted(plan.Sandboxes, "sandbox", "sandboxes") + " running"}
	}
}

// counted is "1 sandbox", "2 sandboxes".
func counted(count int, one, many string) string {
	if count == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", count, many)
}

// openClientLog opens the client's log in the config directory, private as
// the log may hold the page's address.
func openClientLog(directory string) (*os.File, error) {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(filepath.Join(directory, "client.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
}
