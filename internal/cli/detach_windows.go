package cli

import (
	"os/exec"
	"syscall"
)

// detach runs the command without a console, in its own process group, so it
// outlives the terminal and Ctrl+C there.
func detach(command *exec.Cmd) {
	const createNewProcessGroup, createNoWindow = 0x00000200, 0x08000000
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup | createNoWindow, HideWindow: true}
}
