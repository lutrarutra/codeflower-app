package sandbox

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Docker's default seccomp profile, as github.com/moby/profiles/seccomp
// v0.3.0 publishes it (default.json; Copyright The Moby Authors, Apache-2.0).
// Its Go form builds only on Linux, and the app also runs on macOS.
//
//go:embed seccomp/docker-default.json
var dockerDefaultSeccomp []byte

// The system calls bubblewrap needs to set up its namespaces, which the
// default allows only with CAP_SYS_ADMIN (or, for clone, without namespace
// flags). Leaving out any of them fails Codex's sandbox.
var userNamespaceSyscalls = []string{"clone", "unshare", "mount", "umount2", "pivot_root"}

// UserNamespacesSeccomp is the seccomp profile of a container whose agent
// sandboxes its commands itself with user namespaces (the descriptor's
// container.userNamespaces: Codex's bubblewrap): Docker's default, plus the
// calls that need, and nothing else. Inside a user namespace they act only
// on that namespace; outside, the kernel still checks capabilities.
func UserNamespacesSeccomp() ([]byte, error) {
	var profile map[string]any
	if err := json.Unmarshal(dockerDefaultSeccomp, &profile); err != nil {
		return nil, fmt.Errorf("the default seccomp profile: %w", err)
	}
	syscalls, _ := profile["syscalls"].([]any)
	profile["syscalls"] = append(syscalls, map[string]any{
		"names":   userNamespaceSyscalls,
		"action":  "SCMP_ACT_ALLOW",
		"comment": "bubblewrap's namespaces, for the agent's own sandbox",
	})
	return json.Marshal(profile)
}

// writeSeccomp keeps a profile for Podman, which reads it from a file.
func writeSeccomp(state, instanceID string, profile []byte) (string, error) {
	directory := filepath.Join(state, "run")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(directory, "seccomp-"+instanceID+".json")
	if err := os.WriteFile(path, profile, 0o600); err != nil {
		return "", fmt.Errorf("could not write the seccomp profile: %w", err)
	}
	return path, nil
}
