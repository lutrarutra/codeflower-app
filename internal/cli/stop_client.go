package cli

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"codeflower-app/internal/client"
	"codeflower-app/internal/config"
)

func clientStopCommand() *cobra.Command {
	var configDirectory string
	var now bool
	command := &cobra.Command{
		Use:   "stop",
		Short: "Stop the running client, e.g. one started in the background",
		Long: `Stop the code flower client running for the config directory, whether it
runs in a window or in the background (as "codeflower tunnel", "sandbox" and
"run" start it). Paired browsers lose remote access until a client runs again.

The sandboxes the client started from its page stop with it: as when quitting
from the page, their working chats are first asked to save their work and wrap
up, and each sandbox stops when they are done, at most after 10 minutes
("client: {wrap_up_minutes: N}"). Ctrl+C, or --now, stops them at once.

Sandboxes merely connected to it keep running: those started in a terminal
("codeflower sandbox", "codeflower run"), by a batch job, or added by their
address. With no client running, it does nothing.`,
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			interrupts := make(chan os.Signal, 1)
			signal.Notify(interrupts, os.Interrupt, syscall.SIGTERM)
			defer signal.Stop(interrupts)
			return stopClient(command.OutOrStdout(), config.Directory(configDirectory), now, interrupts)
		},
	}
	command.Flags().StringVar(&configDirectory, "config", "",
		"config directory of the client (or set $CODEFLOWER_CONFIG; default ~/.codeflower)")
	command.Flags().BoolVar(&now, "now", false, "stop the sandboxes it started at once, without asking their chats to wrap up")
	return command
}

// stopClient quits the client running for directory and waits until it has
// exited. The sandboxes it started stop with it: unless now, their working
// chats wrap up first, and an interrupt meanwhile stops them at once. Those
// only connected to it keep running.
func stopClient(out io.Writer, directory string, now bool, interrupts <-chan os.Signal) error {
	running, ok := client.Describe(directory)
	if !ok {
		fmt.Fprintln(out, "No code flower client is running.")
		return nil
	}
	wrapUp := clientWrapUp(directory)
	if running.Sandboxes > 0 {
		if now {
			fmt.Fprintf(out, "Stopping the %s it started…\n", sandboxes(running.Sandboxes))
		} else {
			fmt.Fprintf(out, "Asking the working chats in the %s it started to wrap up before they stop (at most %s); press Ctrl+C to stop them now…\n",
				sandboxes(running.Sandboxes), wrapUp.Round(time.Second))
		}
	}
	done := make(chan error, 1)
	go func() { done <- client.QuitWith(directory, !now, wrapUp+client.QuitWait) }()
	var err error
	select {
	case err = <-done:
	case <-interrupts:
		// A second quit without wrapping up stops the sandboxes at once,
		// as Stop now does on the page.
		fmt.Fprintln(out, "Stopping them now…")
		if stopErr := client.QuitWith(directory, false, client.QuitWait); stopErr != nil {
			// It may have gone meanwhile, which refuses the request.
			if _, still := client.Running(directory); still {
				err = stopErr
			}
		} else {
			err = <-done
		}
	}
	if err != nil {
		return fmt.Errorf("could not stop code flower client %s (pid %d): %w", running.Version, running.PID, err)
	}
	if err := waitForClientExit(running.PID); err != nil {
		return err
	}
	fmt.Fprintf(out, "Stopped code flower client %s (pid %d).\n", running.Version, running.PID)
	return nil
}
