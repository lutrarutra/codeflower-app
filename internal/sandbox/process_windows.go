package sandbox

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// processAlive reports whether pid runs: Windows opens a handle only to a
// process that exists.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	process.Release()
	return true
}

// Stop is not offered on Windows, which has no signal a console program can
// catch from another process.
func (r Running) Stop() error {
	return errors.New("stop this sandbox in the window it was started from")
}

// ownProcessGroup starts the command in a console process group of its own,
// which the console's Ctrl+C does not reach: the app stops the command itself.
func ownProcessGroup(command *exec.Cmd) {
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	command.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP
}
