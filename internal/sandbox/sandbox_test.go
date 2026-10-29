package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestParseBindDefaultsToReadOnlyInputs(t *testing.T) {
	dir := filepath.Join(tempDir(t), "study 42")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	bind, err := ParseBind(dir)
	if err != nil {
		t.Fatal(err)
	}
	if bind.Source != dir || bind.Target != "/data/input/study_42" || !bind.ReadOnly {
		t.Fatalf("unexpected bind %+v", bind)
	}
	if bind.Spec() != dir+":/data/input/study_42:ro" {
		t.Fatalf("unexpected spec %q", bind.Spec())
	}
}

func TestFoldersMountUnderTheirRootsByName(t *testing.T) {
	base := tempDir(t)
	folder := func(path string) string {
		path = filepath.Join(base, path)
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	first, second, code := folder("a/data"), folder("b/data"), folder("mypkg")
	output := filepath.Join(base, "results")
	taken := []Bind{{Source: base, Target: "/data/input/data"}}
	binds, err := Folders{Inputs: []string{first, second}, Outputs: []string{output}, Code: []string{code}}.Binds(taken)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, bind := range binds {
		got = append(got, bind.Spec())
	}
	want := []string{
		first + ":/data/input/data-2:ro", second + ":/data/input/data-3:ro",
		output + ":/data/output/results", code + ":/data/code/mypkg",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("binds %v, want %v", got, want)
	}
	if info, err := os.Stat(output); err != nil || !info.IsDir() {
		t.Fatalf("the output folder was not created: %v", err)
	}

	if _, err := (Folders{Inputs: []string{code}, Code: []string{code}}).Binds(nil); err == nil ||
		!strings.Contains(err.Error(), "an input folder and as a code folder") {
		t.Fatalf("a folder chosen twice was accepted: %v", err)
	}
	if err := checkTargets([]Bind{{Source: base, Target: CodeRoot}}); err == nil {
		t.Fatal("a bind over /data/code was accepted")
	}
	for source, want := range map[string]string{"/": "folder", "/x/.config": "config", "/x/R&D data": "R_D_data"} {
		if got := folderName(source); got != want {
			t.Errorf("folderName(%q) = %q, want %q", source, got, want)
		}
	}
}

func TestParseBindAcceptsExplicitTargetAndMode(t *testing.T) {
	dir := tempDir(t)
	bind, err := ParseBind(dir + ":/workspace/data:rw")
	if err != nil {
		t.Fatal(err)
	}
	if bind.Target != "/workspace/data" || bind.ReadOnly {
		t.Fatalf("unexpected bind %+v", bind)
	}
}

func TestParseBindRejectsUnsafeTargets(t *testing.T) {
	dir := tempDir(t)
	cases := map[string]string{
		dir + ":relative":                     "absolute",
		dir + ":/codeflower-state/dsh-home":   "reserved",
		dir + ":/a/../codeflower-state:rw":    "'..'",
		dir + ":/data:rx":                     "'ro' or 'rw'",
		filepath.Join(dir, "missing") + ":/x": "does not exist",
	}
	for value, message := range cases {
		if _, err := ParseBind(value); err == nil || !strings.Contains(err.Error(), message) {
			t.Errorf("ParseBind(%q) = %v, want error containing %q", value, err, message)
		}
	}
}

func TestPrepareStateIsPrivateAndKeepsUserEdits(t *testing.T) {
	state, err := PrepareState(filepath.Join(tempDir(t), "project"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range StateDirectories {
		info, err := os.Stat(filepath.Join(state, name))
		if err != nil {
			t.Fatal(err)
		}
		// Windows has no Unix modes; the user's profile ACLs keep it private.
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
			t.Errorf("%s has mode %o", name, info.Mode().Perm())
		}
	}
	config := filepath.Join(state, "harness", "codex", "config.toml")
	content, err := os.ReadFile(config)
	if err != nil || !strings.Contains(string(content), `sandbox_mode = "danger-full-access"`) {
		t.Fatalf("codex config: %q %v", content, err)
	}
	manifest, err := os.ReadFile(filepath.Join(state, "dsh-home", "profiles", "codeflower", "package.json"))
	if err != nil || !strings.Contains(string(manifest), "@deepseek-ai/dsh-acp-app") {
		t.Fatalf("dsh profile: %q %v", manifest, err)
	}

	if err := os.WriteFile(config, []byte("# mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareState(state); err != nil {
		t.Fatal(err)
	}
	if content, _ := os.ReadFile(config); string(content) != "# mine\n" {
		t.Fatalf("user edit was overwritten: %q", content)
	}
}

func TestApptainerCommandConfinesTheSandbox(t *testing.T) {
	dir := tempDir(t)
	args, err := ApptainerCommand("apptainer", "/images/sandbox.sif", dir,
		[]Bind{{Source: dir, Target: "/data/input/study", ReadOnly: true}, {Source: dir, Target: "/scratch"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(args[1:5], []string{"exec", "--containall", "--no-home", "--cleanenv"}) {
		t.Fatalf("missing confinement flags: %v", args)
	}
	for _, want := range []string{dir + ":" + ContainerState, dir + ":/data/input/study:ro", dir + ":/scratch"} {
		if !slices.Contains(args, want) {
			t.Errorf("missing bind %q in %v", want, args)
		}
	}
	if !slices.Equal(args[len(args)-2:], []string{"/images/sandbox.sif", Entrypoint}) {
		t.Fatalf("unexpected command tail: %v", args)
	}

	if _, err := ApptainerCommand("apptainer", "x.sif", dir, []Bind{{Source: dir, Target: "/a"}, {Source: dir, Target: "/a"}}, ""); err == nil {
		t.Fatal("duplicate targets were accepted")
	}
}

func TestDockerConfigMirrorsTheSandbox(t *testing.T) {
	dir := tempDir(t)
	config, host, err := DockerConfig(DockerRun{
		Image: "codeflower-sandbox",
		State: dir,
		Binds: []Bind{{Source: dir, Target: "/data/input/study", ReadOnly: true}},
		Env:   map[string]string{"CODEFLOWER_TOKEN": "secret", "HOME": "/codeflower-state/home"},
		Port:  7781,
	})
	if err != nil {
		t.Fatal(err)
	}
	mounts := []string{}
	for _, m := range host.Mounts {
		mounts = append(mounts, fmt.Sprintf("%s:%s:%t", m.Source, m.Target, m.ReadOnly))
	}
	if !slices.Equal(mounts, []string{dir + ":" + ContainerState + ":false", dir + ":/data/input/study:true"}) || len(host.Binds) != 0 {
		t.Fatalf("mounts: %v", mounts)
	}
	if !host.AutoRemove || host.Init == nil || !*host.Init {
		t.Fatal("container must be removed and run with an init process")
	}
	binding := host.PortBindings["7781/tcp"]
	if len(binding) != 1 || binding[0].HostIP != "127.0.0.1" || binding[0].HostPort != "7781" {
		t.Fatalf("port binding: %v", host.PortBindings)
	}
	if !slices.Contains(config.Env, "CODEFLOWER_TOKEN=secret") || config.Tty || config.Entrypoint != nil {
		t.Fatalf("worker config: %+v", config)
	}
	if len(host.PortBindings) != 1 {
		t.Fatalf("only the WebSocket port is published by default: %v", host.PortBindings)
	}
}

func TestDockerConfigAttachesStdinOnlyWhenProvided(t *testing.T) {
	dir := tempDir(t)
	config, _, err := DockerConfig(DockerRun{
		Image: "codeflower-sandbox",
		State: dir,
		Stdin: strings.NewReader(""),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !config.AttachStdin || !config.OpenStdin {
		t.Fatalf("stdin was not attached: %+v", config)
	}
}

func TestDockerConfigPublishesTheInspectorOnLoopback(t *testing.T) {
	dir := tempDir(t)
	_, host, err := DockerConfig(DockerRun{Image: "codeflower-sandbox", State: dir, Port: 7781, InspectPort: 9339})
	if err != nil {
		t.Fatal(err)
	}
	binding := host.PortBindings["9229/tcp"]
	if len(binding) != 1 || binding[0].HostIP != "127.0.0.1" || binding[0].HostPort != "9339" {
		t.Fatalf("inspector binding: %v", host.PortBindings)
	}
}

func TestEnvironmentKeepsToolStateInTheProject(t *testing.T) {
	env := WorkerEnvironment(WorkerSettings{Agent: "claude", InstanceID: "id", Token: "t", Port: 1, HostState: "/home/me/p", Engine: "apptainer"})
	for _, name := range []string{"HOME", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "VIRTUAL_ENV", "CONDA_ENVS_DIRS", "CODEFLOWER_SESSIONS_DIR"} {
		if !strings.HasPrefix(env[name], ContainerState) {
			t.Errorf("%s = %q is outside the project", name, env[name])
		}
	}
	if env["CODEFLOWER_CONNECTION_FILE"] != ContainerState+"/instances/id.json" || env["CODEFLOWER_HOST_STATE"] != "/home/me/p" {
		t.Fatalf("worker settings: %v", env)
	}
	if env["CODEFLOWER_AGENT"] != "claude" {
		t.Fatalf("agent: %q", env["CODEFLOWER_AGENT"])
	}
	for _, name := range []string{"ANTHROPIC_API_KEY", "NO_BROWSER"} {
		if _, ok := env[name]; ok {
			t.Fatalf("%s must not be part of the worker environment", name)
		}
	}
	if _, ok := env["CODEFLOWER_SLURM_JOB_ID"]; ok {
		t.Fatal("job id set without Slurm")
	}
}

func TestDefaultAdvertiseHostIsNotLoopback(t *testing.T) {
	if host := DefaultAdvertiseHost(); strings.HasPrefix(host, "127.") || host == "" {
		t.Fatalf("got %q", host)
	}
}
