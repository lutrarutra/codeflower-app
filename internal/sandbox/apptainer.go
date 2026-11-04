package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// ApptainerCommand confines the sandbox: no home, a clean environment, its
// own PID and IPC namespaces, and only the project plus explicit binds. With
// netDirectory it also has no network, only that folder of sockets shared
// with the app. The sandbox's /tmp and /var/tmp live in scratch, as
// --containall otherwise makes them a tmpfs of `sessiondir max size` (16 MB
// by default).
func ApptainerCommand(executable, image, state, scratch string, binds []Bind, netDirectory string) ([]string, error) {
	if err := checkTargets(binds); err != nil {
		return nil, err
	}
	args := []string{
		executable,
		"exec",
		"--containall",
		"--no-home",
		"--cleanenv",
		"--workdir", scratch,
		"--bind", state + ":" + ContainerState,
	}
	if netDirectory != "" {
		args = append(args, "--net", "--network", "none", "--bind", netDirectory+":"+ContainerNet)
	}
	for _, bind := range binds {
		args = append(args, "--bind", bind.Spec())
	}
	return append(args, image, Entrypoint), nil
}

const scratchPrefix = "codeflower-tmp-"

// newScratch makes the private folder behind the sandbox's /tmp, in the
// app's temporary folder ($TMPDIR, which on a cluster is usually the
// job's local disk), rather than sharing the node's /tmp with the sandbox.
func newScratch() (string, error) {
	scratch, err := os.MkdirTemp("", scratchPrefix)
	if err != nil {
		return "", fmt.Errorf("could not create the sandbox's temporary folder: %w", err)
	}
	return scratch, nil
}

// apptainerAllowsIsolation checks that Apptainer may give the user's sandbox
// a network of its own, which a site's apptainer.conf can forbid.
func apptainerAllowsIsolation(ctx context.Context, executable, image string) error {
	check, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	output, err := exec.CommandContext(check, executable, "exec", "--containall", "--no-home", "--cleanenv",
		"--net", "--network", "none", image, "true").CombinedOutput()
	if err == nil {
		return nil
	}
	detail := strings.TrimSpace(string(output))
	if detail == "" {
		detail = err.Error()
	}
	return fmt.Errorf("this machine's Apptainer does not let you isolate the sandbox's network "+
		"(apptainer exec --net --network none): %s\n"+
		"Ask the cluster's administrators to allow it, or run without --isolate-network", detail)
}

// RunApptainer runs the command with values passed as APPTAINERENV_* so they
// reach the clean container environment without appearing in `ps`.
// Cancelling ctx sends SIGTERM to the container's process so the worker can
// shut down cleanly (see stopContainer), and SIGKILL to Apptainer and all it
// started when it has not (see runUntilDone).
func RunApptainer(ctx context.Context, kill <-chan struct{}, args []string, values map[string]string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	command := exec.Command(args[0], args[1:]...)
	command.Env = os.Environ()
	for name, value := range values {
		command.Env = append(command.Env, "APPTAINERENV_"+name+"="+value)
	}
	command.Stdin = stdin
	command.Stdout, command.Stderr = stdout, stderr
	return runUntilDone(ctx, kill, command,
		func() error { return stopContainer(command.Process) },
		func() error { return killTree(command.Process.Pid) })
}

// killTimeout is how long a stopping sandbox has before runUntilDone kills
// it: longer than the engines' own stop timeout, which Docker and Podman keep
// themselves. A variable for tests.
var killTimeout = (stopTimeoutSeconds + 5) * time.Second

// runUntilDone runs a command until it exits. Cancelling ctx calls stop once;
// if the command has not exited killTimeout later, or once kill closes, it
// calls end. One lost signal on the way into the sandbox would otherwise keep
// it running, and its run waiting, for good.
func runUntilDone(ctx context.Context, kill <-chan struct{}, command *exec.Cmd, stop, end func() error) (int, error) {
	if err := command.Start(); err != nil {
		return 1, fmt.Errorf("could not start %s: %w", command.Args[0], err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	report := func(format string, arguments ...any) {
		if command.Stderr != nil {
			fmt.Fprintf(command.Stderr, format, arguments...)
		}
	}
	ending := false
	endNow := func() {
		if ending {
			return
		}
		ending = true
		if err := end(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			report("Could not kill the sandbox: %v\n", err)
		}
	}
	stopRequested := ctx.Done()
	var timedOut <-chan time.Time
	for {
		select {
		case <-stopRequested:
			stopRequested = nil
			if err := stop(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				report("Could not stop the sandbox: %v\n", err)
			}
			timeout := time.NewTimer(killTimeout)
			defer timeout.Stop()
			timedOut = timeout.C
		case <-timedOut:
			timedOut = nil
			report("The sandbox did not stop within %s; killing it\n", killTimeout)
			endNow()
		case <-kill:
			kill = nil
			endNow()
		case err := <-done:
			return exitCode(err), nil
		}
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		if status, ok := exitError.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal())
		}
		return exitError.ExitCode()
	}
	return 1
}
