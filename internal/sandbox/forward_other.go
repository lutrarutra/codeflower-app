//go:build !linux

package sandbox

import "os/exec"

// dieWithParent does nothing where the kernel cannot stop a child with its
// parent; the app stops the command itself when the sandbox ends.
func dieWithParent(*exec.Cmd) {}
