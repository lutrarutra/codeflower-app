//go:build !windows

package sandbox

import (
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestOwnProcessGroupLeavesTheTerminalsGroup(t *testing.T) {
	command := exec.Command("sh", "-c", "ps -o pgid= -p $$")
	ownProcessGroup(command)
	out, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	group, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("pgid %q: %v", out, err)
	}
	if group == syscall.Getpgrp() {
		t.Fatalf("the command runs in the app's process group %d", group)
	}
}
