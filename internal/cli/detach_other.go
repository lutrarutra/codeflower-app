//go:build !windows

package cli

import (
	"os/exec"
	"syscall"
)

// detach runs the command in its own session, so it outlives the terminal.
func detach(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
