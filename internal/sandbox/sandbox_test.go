package sandbox

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"codeflower-app/internal/config"
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
	// The agents' homes and files come from their descriptors, as they were written before those.
	manifest, err := os.ReadFile(filepath.Join(state, "dsh-home", "profiles", "codeflower", "package.json"))
	want := "{\n  \"dsh\": {\n    \"profile\": {\n      \"bundles\": [\n        \"@deepseek-ai/dsh-base\",\n" +
		"        \"@deepseek-ai/dsh-acp-app\"\n      ]\n    }\n  },\n  \"name\": \"codeflower\",\n  \"private\": true\n}\n"
	if err != nil || string(manifest) != want {
		t.Fatalf("dsh profile: %q %v", manifest, err)
	}
	if patch, _ := os.ReadFile(filepath.Join(state, "dsh-home", "profiles", "codeflower", "cordis.patch.yml")); string(patch) != "[]\n" {
		t.Fatalf("dsh patch: %q", patch)
	}
	for _, name := range []string{"harness", "harness/claude", "harness/codex", "dsh-home", "dsh-home/profiles/codeflower"} {
		if info, err := os.Stat(filepath.Join(state, name)); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o700) {
			t.Errorf("%s: %v %v", name, info, err)
		}
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
	args, err := ApptainerCommand("apptainer", "/images/sandbox.sif", dir, "/local/codeflower-tmp-1",
		[]Bind{{Source: dir, Target: "/data/input/study", ReadOnly: true}, {Source: dir, Target: "/scratch"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(args[1:5], []string{"exec", "--containall", "--no-home", "--cleanenv"}) {
		t.Fatalf("missing confinement flags: %v", args)
	}
	if !strings.Contains(strings.Join(args, " "), "--workdir /local/codeflower-tmp-1") {
		t.Errorf("/tmp is not backed by the scratch folder: %v", args)
	}
	for _, want := range []string{dir + ":" + ContainerState, dir + ":/data/input/study:ro", dir + ":/scratch"} {
		if !slices.Contains(args, want) {
			t.Errorf("missing bind %q in %v", want, args)
		}
	}
	if !slices.Equal(args[len(args)-2:], []string{"/images/sandbox.sif", Entrypoint}) {
		t.Fatalf("unexpected command tail: %v", args)
	}

	if _, err := ApptainerCommand("apptainer", "x.sif", dir, dir, []Bind{{Source: dir, Target: "/a"}, {Source: dir, Target: "/a"}}, ""); err == nil {
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
	for _, name := range []string{"HOME", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "DSH_HOME", "VIRTUAL_ENV", "CONDA_ENVS_DIRS", "CODEFLOWER_SESSIONS_DIR"} {
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

func TestWorkerMountsTheGalleryAndGivesTheImageKeyInAFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a shell script as the engine")
	}
	directory := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(directory, "config"))
	state := filepath.Join(directory, "project")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(directory, "out")
	// The engine notes what the sandbox would get, including the key file while it runs.
	engine := filepath.Join(directory, "apptainer")
	script := "#!/bin/sh\nenv | grep '^APPTAINERENV_' | sort > " + out + "\n" +
		"ls -l " + state + "/run/image-key-* | cut -c1-10 >> " + out + "\ncat " + state + "/run/image-key-* >> " + out + "\necho \"$@\" >> " + out + "\n"
	if err := os.WriteFile(engine, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	gallery, err := IconsBind(filepath.Join(directory, "icons"))
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(directory, "icons")); err != nil || !info.IsDir() || gallery.ReadOnly || gallery.Target != IconsRoot {
		t.Fatalf("gallery bind %+v: %v", gallery, err)
	}
	worker := Worker{
		Agent:  "claude",
		Engine: EngineStatus{Name: EngineApptainer, Executable: engine},
		Image:  filepath.Join(directory, "image.sif"),
		State:  state,
		Binds:  []Bind{gallery},
		Images: &config.ImageGeneration{Provider: "google", Model: "gemini-test", Key: "image-secret"},
		Stdout: io.Discard, Stderr: io.Discard,
	}
	if code, err := worker.Run(context.Background(), nil); code != 0 || err != nil {
		t.Fatalf("run: %d, %v", code, err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	seen := string(data)
	for _, want := range []string{
		"APPTAINERENV_CODEFLOWER_ICONS=/data/icons\n",
		"APPTAINERENV_CODEFLOWER_IMAGE_PROVIDER=google\n",
		"APPTAINERENV_CODEFLOWER_IMAGE_MODEL=gemini-test\n",
		"APPTAINERENV_CODEFLOWER_IMAGE_KEY_FILE=/codeflower-state/run/image-key-",
		"\n-rw-------\nimage-secret\n",
		gallery.Source + ":/data/icons",
	} {
		if !strings.Contains(seen, want) {
			t.Errorf("the sandbox did not get %q:\n%s", want, seen)
		}
	}
	if strings.Contains(seen, "APPTAINERENV_CODEFLOWER_IMAGE_KEY=") || strings.Count(seen, "image-secret") != 1 {
		t.Errorf("the key must only be in the file:\n%s", seen)
	}
	if left, _ := filepath.Glob(filepath.Join(state, "run", "image-key-*")); len(left) != 0 {
		t.Fatalf("the key file outlived the worker: %v", left)
	}
	if hosts := worker.providerHosts(); !slices.Contains(hosts, "generativelanguage.googleapis.com") || !slices.Contains(hosts, ProviderHosts["claude"][0]) {
		t.Fatalf("an isolated sandbox reaches %v", hosts)
	}
}
