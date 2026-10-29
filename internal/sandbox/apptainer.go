package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
)

// ApptainerCommand confines the sandbox: no home, a clean environment, its
// own PID and IPC namespaces, and only the project plus explicit binds.
func ApptainerCommand(executable, image, state string, binds []Bind) ([]string, error) {
	if err := checkTargets(binds); err != nil {
		return nil, err
	}
	args := []string{
		executable,
		"exec",
		"--containall",
		"--no-home",
		"--cleanenv",
		"--bind", state + ":" + ContainerState,
	}
	for _, bind := range binds {
		args = append(args, "--bind", bind.Spec())
	}
	return append(args, image, Entrypoint), nil
}

// RunApptainer runs the command with values passed as APPTAINERENV_* so they
// reach the clean container environment without appearing in `ps`.
// Cancelling ctx sends SIGTERM so the worker can shut down cleanly.
func RunApptainer(ctx context.Context, args []string, values map[string]string, stdout, stderr io.Writer) (int, error) {
	command := exec.Command(args[0], args[1:]...)
	command.Env = os.Environ()
	for name, value := range values {
		command.Env = append(command.Env, "APPTAINERENV_"+name+"="+value)
	}
	command.Stdout, command.Stderr = stdout, stderr
	return runUntilDone(ctx, command, func() { _ = command.Process.Signal(syscall.SIGTERM) })
}

// runUntilDone runs a command; cancelling ctx calls stop once and waits for
// the command to exit.
func runUntilDone(ctx context.Context, command *exec.Cmd, stop func()) (int, error) {
	if err := command.Start(); err != nil {
		return 1, fmt.Errorf("could not start %s: %w", command.Args[0], err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	stopRequested := ctx.Done()
	for {
		select {
		case <-stopRequested:
			stopRequested = nil
			stop()
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
