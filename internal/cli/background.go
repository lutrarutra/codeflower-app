package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"codeflower-app/internal/client"
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

// keepReachable starts the client in the background when this computer is
// signed in to the tunnel with remote access on, so paired browsers reach the
// sandbox that `run` or `interactive` starts. It only reports failures.
func keepReachable(directory string) {
	state, err := tunnel.LoadState(directory)
	if err != nil || state.Off || !state.OnThisHost() {
		return
	}
	started, err := ensureClient(directory)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Paired browsers cannot reach this sandbox: %v\n", err)
		return
	}
	if started {
		fmt.Fprintf(os.Stderr, "  tunnel   paired browsers reach this sandbox at %s\n", tunnel.PageURL(state))
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
