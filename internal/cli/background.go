package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"codeflower-app/internal/client"
	"codeflower-app/internal/sandbox"
	"codeflower-app/internal/tunnel"
)

// backgroundWait is how long a client started in the background has to come
// up; it waits for the tunnel (at most 5 s) before it answers.
const backgroundWait = 20 * time.Second

// ensureClient makes sure a client runs for the config directory, starting
// `codeflower client --background` when none does. That client keeps the
// tunnel connected and lists every sandbox on this computer, whoever started
// it. It says whether it started one.
func ensureClient(directory string) (bool, error) {
	if _, running := client.Running(directory); running {
		return false, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return false, err
	}
	command := exec.Command(executable, "client", "--background", "--config", directory)
	detach(command)
	if err := command.Start(); err != nil {
		return false, err
	}
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	deadline := time.After(backgroundWait)
	for {
		if _, running := client.Running(directory); running {
			return true, nil
		}
		select {
		case err := <-exited:
			if err == nil {
				err = errors.New("it exited")
			}
			return false, fmt.Errorf("the client did not start (%v); see %s", err, clientLogPath(directory))
		case <-deadline:
			return false, fmt.Errorf("the client did not start within %s; see %s", backgroundWait, clientLogPath(directory))
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// tunnelLinks starts the client in the background when this computer is
// signed in to the tunnel with remote access on, so paired browsers reach the
// sandbox that `run` or `sandbox` starts. It returns what to call once the
// sandbox is ready, which prints where to open it: through the tunnel (with a
// QR code for phones), on this computer, and in VS Code. It returns nil when
// the tunnel is not used. With the config directory shared between computers
// (a cluster's home), it takes the tunnel over from a computer whose hold was
// released or lapsed, and says so when another still holds it.
func tunnelLinks(directory string, out io.Writer) func(sandbox.Connection) {
	state, err := tunnel.LoadState(directory)
	if err != nil || state.Off {
		return nil
	}
	if !state.MayConnect(time.Now()) {
		return func(sandbox.Connection) {
			time.Sleep(time.Second)
			fmt.Fprintf(out, "\nPaired browsers cannot reach this sandbox: %v.\n\n", &tunnel.ErrHeld{Host: state.Host})
		}
	}
	started := make(chan error, 1)
	go func() {
		_, err := ensureClient(directory)
		started <- err
	}()
	return func(connection sandbox.Connection) {
		if err := <-started; err != nil {
			fmt.Fprintf(out, "Paired browsers cannot reach this sandbox: %v\n", err)
			return
		}
		// After the bridge's own lines, which reach the terminal through the engine.
		time.Sleep(time.Second)
		route := "#/chat/" + client.LocalID(connection.ID)
		remote := tunnel.PageURL(state) + route
		fmt.Fprintln(out)
		printQR(out, remote)
		fmt.Fprintf(out, "Through the tunnel, in paired browsers (scan the code with a phone):\n\n  %s\n\n", remote)
		if len(state.Devices) == 0 {
			fmt.Fprintf(out, "No browser is paired yet: run \"codeflower tunnel pair\".\n\n")
		}
		if local, running := client.Running(directory); running {
			fmt.Fprintf(out, "On this computer:\n\n  %s\n\n", local+route)
		}
		fmt.Fprintf(out, "In VS Code:\n\n  %s\n\n", client.VSCodeLink(connection.URL))
	}
}

// quitWhenSignedOut quits a background client once this computer is signed
// out of the tunnel: it ran for the tunnel only.
func quitWhenSignedOut(control *client.Control, directory string) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if _, err := tunnel.LoadState(directory); errors.Is(err, tunnel.ErrNotSignedIn) {
			control.Quit(true)
			return
		}
	}
}
