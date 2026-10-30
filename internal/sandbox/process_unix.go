//go:build !windows

package sandbox

import (
	"errors"
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
