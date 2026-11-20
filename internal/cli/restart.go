package cli

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"codeflower-app/internal/client"
	"codeflower-app/internal/config"
	"codeflower-app/internal/sandbox"
)

// clientStart is what ensureClient did: started a client where none ran, or
// replaced an older background one (replaced is its version). stale is an
// older client it left running: a window, or one running sandboxes it
// started, which a restart would stop.
type clientStart struct {
	started  bool
	replaced string
	pid      int
	stale    *client.Client
}

// note says what happened to the client, for the terminal or a job's log;
// empty when an up-to-date client was running already.
func (c clientStart) note() string {
	switch {
	case c.started:
		return fmt.Sprintf("Started code flower client %s in the background (pid %d): it keeps the tunnel connected; \"codeflower client stop\" stops it.", Version, c.pid)
	case c.replaced != "":
		return fmt.Sprintf("Restarted code flower client in the background, %s → %s (pid %d), so its page is this release's.", c.replaced, Version, c.pid)
	case c.stale != nil:
		return staleClientNote(*c.stale)
	}
	return ""
}

func staleClientNote(running client.Client) string {
	why := "it runs in a window"
	if running.Sandboxes > 0 {
		why = fmt.Sprintf("restarting it would stop the %s it started", sandboxes(running.Sandboxes))
	}
	return fmt.Sprintf("code flower client %s runs here, older than this %s, so its page may lack what is new; it was left running, as %s.\n"+
		`Restart it when it suits you: "codeflower client restart".`, running.Version, Version, why)
}

// sandboxes counts sandboxes: "1 sandbox", "2 sandboxes".
func sandboxes(count int) string {
	if count == 1 {
		return "1 sandbox"
	}
	return fmt.Sprintf("%d sandboxes", count)
}

// olderRelease says whether running is a release older than this one. A
// development build on either side is never older: nothing restarts it.
func olderRelease(running, this string) bool {
	if _, ok := parseRelease(running); !ok {
		return false
	}
	if _, ok := parseRelease(this); !ok {
		return false
	}
	return newer(this, running)
}

// clientWait is how long a client started in the background has to come
// up; it waits for the tunnel (at most 5 s) before it answers.
const clientWait = 20 * time.Second

// startClient starts `codeflower client` with args (a background one when
// nil) and waits until it answers; it returns its process ID. A restart
// passes the page address of the client it replaces (else empty): the new
// one takes its port and secret, so open pages carry on.
func startClient(directory string, args []string, replaces ...string) (int, error) {
	executable, err := os.Executable()
	if err != nil {
		return 0, err
	}
	args = backgroundArgs(args, directory)
	command := exec.Command(executable)
	command.Env = os.Environ()
	if len(replaces) > 0 && replaces[0] != "" {
		if previous, err := url.Parse(replaces[0]); err == nil {
			if port := previous.Port(); port != "" && !slices.ContainsFunc(args, func(arg string) bool {
				return arg == "--port" || arg == "--address" || strings.HasPrefix(arg, "--port=") || strings.HasPrefix(arg, "--address=")
			}) {
				args = append(args, "--port", port)
			}
			if key := previous.Query().Get("key"); key != "" {
				command.Env = append(command.Env, client.SecretVariable+"="+key)
			}
		}
	}
	command.Args = append([]string{executable, "client"}, args...)
	detach(command)
	if err := command.Start(); err != nil {
		return 0, err
	}
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	deadline := time.After(clientWait)
	for {
		if _, running := client.Running(directory); running {
			return command.Process.Pid, nil
		}
		select {
		case err := <-exited:
			if err == nil {
				err = errors.New("it exited")
			}
			return 0, fmt.Errorf("the client did not start (%v); see %s", err, clientLogPath(directory))
		case <-deadline:
			return 0, fmt.Errorf("the client did not start within %s; see %s", clientWait, clientLogPath(directory))
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// backgroundArgs are a client's arguments to run it again in the
// background: its own (a restart keeps --ui, --connect-only, --port), never
// as if double-clicked, and for this config directory.
func backgroundArgs(args []string, directory string) []string {
	kept := []string{}
	for _, arg := range args {
		if arg != "--desktop" && arg != "--background" {
			kept = append(kept, arg)
		}
	}
	kept = append([]string{"--background"}, kept...)
	if !slices.ContainsFunc(kept, func(arg string) bool { return arg == "--config" || strings.HasPrefix(arg, "--config=") }) {
		kept = append(kept, "--config", directory)
	}
	return kept
}

// replaceClient quits the running client and starts it again from this
// codeflower, with its arguments, in the background. Without wrapUp the
// sandboxes it started stop at once; callers check there are none, or ask.
// Sandboxes started elsewhere (a terminal, a batch job) keep running.
func replaceClient(directory string, running client.Client, wrapUp bool) (int, error) {
	wait := client.QuitWait
	if wrapUp {
		wait = 11 * time.Minute
	}
	if err := client.QuitWith(directory, wrapUp, wait); err != nil {
		return 0, err
	}
	if err := waitForClientExit(running.PID); err != nil {
		return 0, err
	}
	return startClient(directory, running.Args, running.URL)
}

// waitForClientExit waits for a client whose API is gone to exit: it still
// releases the tunnel and stops what it started before it does.
func waitForClientExit(pid int) error {
	for deadline := time.Now().Add(client.QuitWait); pid > 0 && sandbox.ProcessAlive(pid); time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			return fmt.Errorf("the client (pid %d) did not exit within %s", pid, client.QuitWait)
		}
	}
	return nil
}

// clientArgs are this process's arguments after the "client" command (or
// its alias), which the client records so a restart runs it the same way.
func clientArgs(args []string) []string {
	for i, arg := range args {
		if arg == "client" || arg == "gui" {
			return slices.Clone(args[i+1:])
		}
	}
	return []string{}
}

func clientRestartCommand() *cobra.Command {
	var configDirectory string
	var force bool
	command := &cobra.Command{
		Use:   "restart",
		Short: "Restart the running client from this codeflower, e.g. after an update",
		Long: `Restart the code flower client running for the config directory from this
codeflower, so its page is this release's: it quits and starts again in the
background, with the options it had. Paired browsers lose it for a few
seconds; open pages reconnect and offer to reload.

Sandboxes started in a terminal or a batch job keep running. Those the client
started from its page stop when it quits, so with any running, restart asks
for --force, which first asks their working chats to wrap up (at most 10
minutes) and stops them. With no client running, it starts one.`,
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return restartClient(command.OutOrStdout(), config.Directory(configDirectory), force)
		},
	}
	command.Flags().StringVar(&configDirectory, "config", "",
		"config directory of the client (or set $CODEFLOWER_CONFIG; default ~/.codeflower)")
	command.Flags().BoolVar(&force, "force", false, "also restart a client running sandboxes it started, which stop after their chats wrap up")
	return command
}

func restartClient(out io.Writer, directory string, force bool) error {
	running, ok := client.Describe(directory)
	if !ok {
		pid, err := startClient(directory, nil)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "No client was running: started code flower client %s in the background (pid %d).\n", Version, pid)
		return nil
	}
	if running.Sandboxes > 0 && !force {
		return fmt.Errorf("code flower client %s runs %s it started from its page, which stop when it quits.\n"+
			"Stop them first, or restart with --force: their working chats wrap up first (at most 10 minutes)",
			running.Version, sandboxes(running.Sandboxes))
	}
	if !running.Background {
		fmt.Fprintln(out, "The client runs in a window: it quits there and starts again in the background.")
	}
	if running.Sandboxes > 0 {
		fmt.Fprintf(out, "Asking the chats in the %s it started to wrap up…\n", sandboxes(running.Sandboxes))
	}
	pid, err := replaceClient(directory, running, running.Sandboxes > 0)
	if err != nil {
		return err
	}
	page, _ := client.Running(directory)
	versions := running.Version + " → " + Version
	if running.Version == Version {
		versions = Version
	}
	fmt.Fprintf(out, "Restarted code flower client %s (pid %d), at:\n\n  %s\n\n", versions, pid, page)
	fmt.Fprintln(out, "Open pages reconnect by themselves; reload them for the new page.")
	return nil
}
