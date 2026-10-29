package cli

import (
	"strings"
	"testing"
)

func TestInteractiveChecksTheNetworkFlags(t *testing.T) {
	cases := map[string][]string{
		"--allowlist needs --isolate-network": {"--agent", "claude", "--allowlist", "hosts.txt"},
		"--inspect-port cannot be used with --isolate-network": {
			"--agent", "claude", "--isolate-network", "--inspect-port", "9339",
		},
	}
	for message, args := range cases {
		command := interactiveCommand()
		command.SetArgs(append([]string{"--state", t.TempDir()}, args...))
		if err := command.Execute(); err == nil || !strings.Contains(err.Error(), message) {
			t.Errorf("%v: got %v, want %q", args, err, message)
		}
	}
}
