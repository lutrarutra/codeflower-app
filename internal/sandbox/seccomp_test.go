package sandbox

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"codeflower-app/internal/agents"
)

// Codex sandboxes its commands with bubblewrap, which Docker's default
// seccomp profile keeps from creating user namespaces.
func TestCodexNeedsUserNamespacesAndNoOtherAgentDoes(t *testing.T) {
	for _, descriptor := range agents.All() {
		if want := descriptor.ID == "codex"; descriptor.Container.UserNamespaces != want {
			t.Errorf("%s: userNamespaces = %v", descriptor.ID, descriptor.Container.UserNamespaces)
		}
	}
}

func TestUserNamespacesSeccompIsDockersDefaultPlusBubblewrapsCalls(t *testing.T) {
	profile, err := UserNamespacesSeccomp()
	if err != nil {
		t.Fatal(err)
	}
	var base, got struct {
		DefaultAction string `json:"defaultAction"`
		Syscalls      []struct {
			Names  []string `json:"names"`
			Action string   `json:"action"`
		} `json:"syscalls"`
	}
	if err := json.Unmarshal(dockerDefaultSeccomp, &base); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(profile, &got); err != nil {
		t.Fatal(err)
	}
	if got.DefaultAction != "SCMP_ACT_ERRNO" || len(got.Syscalls) != len(base.Syscalls)+1 {
		t.Fatalf("not the default with one rule more: %s, %d rules of %d", got.DefaultAction, len(got.Syscalls), len(base.Syscalls))
	}
	added := got.Syscalls[len(got.Syscalls)-1]
	if added.Action != "SCMP_ACT_ALLOW" || !slices.Equal(added.Names, []string{"clone", "unshare", "mount", "umount2", "pivot_root"}) {
		t.Fatalf("added %+v", added)
	}
}

func TestEnginesTakeTheSeccompProfile(t *testing.T) {
	dir := tempDir(t)
	profile, err := UserNamespacesSeccomp()
	if err != nil {
		t.Fatal(err)
	}
	_, host, err := DockerConfig(DockerRun{Image: "i", State: dir, Seccomp: profile})
	if err != nil {
		t.Fatal(err)
	}
	if len(host.SecurityOpt) != 1 || host.SecurityOpt[0] != "seccomp="+string(profile) {
		t.Fatalf("docker security options: %.80q", host.SecurityOpt)
	}
	if _, host, _ = DockerConfig(DockerRun{Image: "i", State: dir}); len(host.SecurityOpt) != 0 {
		t.Fatalf("docker without a profile: %q", host.SecurityOpt)
	}

	file, err := writeSeccomp(dir, "0123456789abcdef", profile)
	if err != nil {
		t.Fatal(err)
	}
	args, err := PodmanArgs("podman", PodmanRun{Name: "x", Image: "i", State: dir, SeccompFile: file})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(args, " "), "--security-opt seccomp="+file) {
		t.Fatalf("podman args: %v", args)
	}
	if args, _ = PodmanArgs("podman", PodmanRun{Name: "x", Image: "i", State: dir}); strings.Contains(strings.Join(args, " "), "seccomp=") {
		t.Fatalf("podman without a profile: %v", args)
	}
}
