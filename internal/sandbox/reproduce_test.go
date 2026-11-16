package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/network"
)

func TestReproduceContainersRunTheCommandWithoutNetwork(t *testing.T) {
	dir := tempDir(t)
	command := []string{ReproduceEntrypoint, PhaseRun}
	binds := []Bind{{Source: dir, Target: ReproducePipeline, ReadOnly: true}, {Source: dir, Target: ReproduceOut}}

	args, err := ApptainerCommandWith(ApptainerOptions{Executable: "apptainer", Image: "/images/sandbox.sif", State: dir,
		Scratch: dir, Binds: binds, NoNetwork: true, Command: command})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(args[len(args)-3:], []string{"/images/sandbox.sif", ReproduceEntrypoint, PhaseRun}) {
		t.Fatalf("the command does not replace the entrypoint: %v", args)
	}
	line := strings.Join(args, " ")
	if !strings.Contains(line, "--net --network none") || strings.Contains(line, ContainerNet) {
		t.Fatalf("no network, and no socket folder, expected: %v", args)
	}

	config, host, err := DockerConfig(DockerRun{Image: "img", State: dir, Binds: binds, NoNetwork: true, Entrypoint: command, Port: 7000})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(config.Entrypoint, command[:1]) || !slices.Equal(config.Cmd, command[1:]) {
		t.Fatalf("entrypoint %v, cmd %v", config.Entrypoint, config.Cmd)
	}
	if host.NetworkMode != network.NetworkNone || len(host.PortBindings) != 0 {
		t.Fatalf("network %q, ports %v", host.NetworkMode, host.PortBindings)
	}

	podman, err := PodmanArgs("podman", PodmanRun{Name: "x", Image: "img", State: dir, Binds: binds, NoNetwork: true, Entrypoint: command, Port: 7000})
	if err != nil {
		t.Fatal(err)
	}
	line = strings.Join(podman, " ")
	if !strings.Contains(line, "--network none") || strings.Contains(line, "--publish") {
		t.Fatalf("podman network: %v", podman)
	}
	if !strings.HasSuffix(line, "--entrypoint "+ReproduceEntrypoint+" img "+PhaseRun) {
		t.Fatalf("podman entrypoint: %v", podman)
	}
}

func TestRestoreHostsAreThePackageIndexes(t *testing.T) {
	for _, want := range []string{"pypi.org", "files.pythonhosted.org", "conda.anaconda.org", "packagemanager.posit.co", "bioconductor.org", "github.com"} {
		if !slices.Contains(RestoreHosts, want) {
			t.Errorf("RestoreHosts lacks %s: %v", want, RestoreHosts)
		}
	}
	for _, provider := range ProviderHostsOf([]string{"claude", "codex", "dsh", "opencode"}) {
		if slices.Contains(RestoreHosts, provider) {
			t.Errorf("RestoreHosts allows the agent service %s", provider)
		}
	}
}

func TestRepositoryDigestNeedsARegistry(t *testing.T) {
	published := ImageRepository + "@sha256:" + strings.Repeat("a", 64)
	if got := repositoryDigest([]string{"codeflower-sandbox@sha256:" + strings.Repeat("b", 64), published}); got != published {
		t.Fatalf("got %q", got)
	}
	if got := repositoryDigest([]string{"codeflower-sandbox@sha256:" + strings.Repeat("b", 64)}); got != "" {
		t.Fatalf("a local build's digest counts: %q", got)
	}
	other := "registry.example.org:5000/lab/sandbox@sha256:" + strings.Repeat("c", 64)
	if got := repositoryDigest([]string{other}); got != other {
		t.Fatalf("got %q", got)
	}
}

func TestImageIdentityEnvironment(t *testing.T) {
	values := map[string]string{}
	ImageIdentity{Ref: "codeflower-sandbox", ID: "sha256:abc"}.Environment(values)
	if values["CODEFLOWER_IMAGE_REF"] != "codeflower-sandbox" || values["CODEFLOWER_IMAGE_ID"] != "sha256:abc" {
		t.Fatalf("values %v", values)
	}
	if _, ok := values["CODEFLOWER_IMAGE_DIGEST"]; ok {
		t.Fatal("an empty digest was set")
	}
}

// The three phases run the reproduce entrypoint in turn, each with a new
// container on one throwaway project, and stop at the first that fails.
func TestReproductionRunsThePhasesInTurn(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake engine is a shell script")
	}
	dir := tempDir(t)
	record := filepath.Join(dir, "calls")
	script := filepath.Join(dir, "apptainer")
	body := "#!/bin/sh\n" +
		"[ \"$1\" = exec ] || exit 0\n" +
		"echo \"$*\" >> " + record + "\n" +
		"case \"$*\" in *' prepare') exit 4 ;; esac\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	pipeline := filepath.Join(dir, "pipeline")
	output := filepath.Join(dir, "out")
	if err := os.MkdirAll(pipeline, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", dir)
	result, err := Reproduction{Engine: EngineStatus{Name: EngineApptainer, Executable: script}, Image: "sandbox.sif",
		Pipeline: pipeline, Snapshot: "", Output: output, Stdout: os.Stderr, Stderr: os.Stderr}.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Phase != PhasePrepare || result.Code != 4 {
		t.Fatalf("result %+v", result)
	}
	calls, _ := os.ReadFile(record)
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
		if strings.Contains(line, ReproduceEntrypoint) {
			lines = append(lines, line)
		}
	}
	if len(lines) != 2 || !strings.HasSuffix(lines[0], ReproduceEntrypoint+" check") || !strings.HasSuffix(lines[1], ReproduceEntrypoint+" prepare") {
		t.Fatalf("calls:\n%s", calls)
	}
	if !strings.Contains(lines[0], "--network none") || !strings.Contains(lines[0], pipeline+":"+ReproducePipeline+":ro") {
		t.Fatalf("check phase: %s", lines[0])
	}
	if !strings.Contains(lines[1], ":"+ContainerNet) {
		t.Fatalf("prepare reaches the network only through the gateway: %s", lines[1])
	}
	if strings.Contains(string(calls), "CLAUDE") || strings.Contains(string(calls), "harness") {
		t.Fatalf("agent state reached the sandbox: %s", calls)
	}
}
