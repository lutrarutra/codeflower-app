//go:build !linux

package sandbox

import (
	"os"
	"syscall"
)

// Apptainer, whose processes these find, runs only on Linux.

// stopContainer sends SIGTERM to Apptainer.
func stopContainer(starter *os.Process) error {
	return starter.Signal(syscall.SIGTERM)
}

// killTree kills the process.
func killTree(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Kill()
}
