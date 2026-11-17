//go:build !windows

package sandbox

import (
	"errors"
	"os/exec"
	"syscall"
)

// processAlive reports whether pid runs; a process of another user counts.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Stop asks the codeflower process of a run to stop its sandbox, as
// Ctrl+C in its terminal would.
func (r Running) Stop() error {
	return syscall.Kill(r.PID, syscall.SIGTERM)
}

// ownProcessGroup starts the command in a process group of its own: the
// terminal's Ctrl+C (and Ctrl+\) go to its foreground group only, so they
// reach the app, which stops the command itself.
func ownProcessGroup(command *exec.Cmd) {
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	command.SysProcAttr.Setpgid = true
}
