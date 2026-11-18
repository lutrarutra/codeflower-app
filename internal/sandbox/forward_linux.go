package sandbox

import (
	"os/exec"
	"syscall"
)

// dieWithParent has the kernel stop the command when the app dies without
// stopping it (SIGKILL, a crash), so no tunnel outlives its sandbox.
func dieWithParent(command *exec.Cmd) {
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	command.SysProcAttr.Pdeathsig = syscall.SIGTERM
}
