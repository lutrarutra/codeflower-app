package cli

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"codeflower-app/internal/client"
	"codeflower-app/internal/sandbox"
)

func TestAProjectIsNamedOrAFolder(t *testing.T) {
	for argument, want := range map[string][2]string{
		"study42":           {"study42", ""},
		"./study42":         {"", "./study42"},
		"/scratch/study42":  {"", "/scratch/study42"},
		"~/cf/study42":      {"", "~/cf/study42"},
		"..":                {"", ".."},
		`C:\cf\study42`:     {"", `C:\cf\study42`},
		"with space":        {"", "with space"},
		"study-42_v2.final": {"study-42_v2.final", ""},
	} {
		name, folder := projectArgument(argument)
		if name != want[0] || folder != want[1] {
			t.Errorf("%q: name %q folder %q", argument, name, folder)
		}
	}
}

func TestSavedSettingsGiveWhatNoFlagGives(t *testing.T) {
	saved := t.TempDir()
	added := t.TempDir()
	settings := sandbox.Settings{Engine: "apptainer", Image: "/images/cf.sif", Inputs: []string{saved}, Outputs: []string{"/results"},
		Binds: []string{"/ref:/reference:ro"}, IsolateNetwork: true, CPUs: 4, Server: true, Host: "127.0.0.1", Port: 41234, NoTunnel: true}

	flagsGiven := map[string]bool{"cpus": true, "input": true, "bind": true}
	var options sandboxOptions
	options.changed = func(name string) bool { return flagsGiven[name] || options.fromSettings[name] }
	options.engine = "auto"
	options.cpus = 8
	// The saved folder again, by another path, and a new one.
	options.folders.Inputs = []string{saved + string(filepath.Separator), added}
	options.bindValues = []string{"/ref:/reference:ro", "/more"}
	var flags workerFlags
	flags.listenHost = "0.0.0.0"
	options.applySettings(settings, &flags)

	if options.engine != "apptainer" || options.image != "/images/cf.sif" || !options.isolateNetwork {
		t.Errorf("saved settings not taken: %+v", options)
	}
	if options.cpus != 8 {
		t.Errorf("--cpus did not replace the saved CPUs: %d", options.cpus)
	}
	if !slices.Equal(options.folders.Inputs, []string{saved, added}) || !slices.Equal(options.folders.Outputs, []string{"/results"}) {
		t.Errorf("folders %+v", options.folders)
	}
	if !slices.Equal(options.bindValues, []string{"/ref:/reference:ro", "/more"}) {
		t.Errorf("binds %v", options.bindValues)
	}
	if !flags.servePage || flags.listenHost != "127.0.0.1" || flags.port != 41234 || !flags.noTunnel {
		t.Errorf("worker flags %+v", flags)
	}
	// The config does not replace what the settings chose.
	if !options.changed("engine") || !options.changed("isolate-network") || !options.envDirSaved {
		t.Error("saved settings count as given")
	}
}

func TestSettingsSaveAbsolutePathsAndNotTheConfigsSkills(t *testing.T) {
	work := t.TempDir()
	t.Chdir(work)
	for _, folder := range []string{"data", "mine", "config-skills"} {
		if err := os.Mkdir(folder, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	options := sandboxOptions{allowlist: "hosts.txt", isolateNetwork: true, cpus: 2}
	options.folders.Inputs = []string{"data"}
	options.folders.Skills = []string{"config-skills", "mine"}
	options.configSkills = map[string]bool{resolvedPath("config-skills"): true}
	options.bindValues = []string{"data:/reference"}
	previous := sandbox.Settings{Key: "kept", Port: 1}
	settings := options.settingsOf(previous, "docker", workerFlags{port: 0, servePage: true})

	if settings.Key != "kept" || settings.Engine != "docker" || settings.Port != 0 || !settings.Server {
		t.Errorf("settings %+v", settings)
	}
	if !slices.Equal(settings.Inputs, []string{filepath.Join(work, "data")}) {
		t.Errorf("inputs %v", settings.Inputs)
	}
	if !slices.Equal(settings.Skills, []string{filepath.Join(work, "mine")}) {
		t.Errorf("skills %v", settings.Skills)
	}
	if settings.Allowlist != filepath.Join(work, "hosts.txt") {
		t.Errorf("allowlist %s", settings.Allowlist)
	}
	resolved, _ := filepath.EvalSymlinks(filepath.Join(work, "data"))
	if len(settings.Binds) != 1 || !strings.HasSuffix(settings.Binds[0], ":/reference:ro") ||
		!strings.HasPrefix(settings.Binds[0], resolved) && !strings.HasPrefix(settings.Binds[0], filepath.Join(work, "data")) {
		t.Errorf("binds %v", settings.Binds)
	}
}

func TestContinuingASandboxChecksItsProject(t *testing.T) {
	projects := t.TempDir()
	if _, err := sandbox.PrepareState(filepath.Join(projects, "fresh")); err != nil {
		t.Fatal(err)
	}
	notProject := filepath.Join(projects, "notes")
	if err := os.MkdirAll(filepath.Join(notProject, "inside"), 0o700); err != nil {
		t.Fatal(err)
	}
	cases := map[string][]string{
		"--create needs --continue":                {"--create", "--project", "fresh"},
		"did you mean fresh? (add --create":        {"--continue", "frseh"},
		"is not a project and is not empty":        {"--continue", notProject, "--create"},
		"--project cannot be used with --continue": {"--continue", "fresh", "--project", "other"},
		"--state cannot be used with --continue":   {"--continue", "fresh", "--state", projects},
		"no project missing in":                    {"--continue", "missing"},
		"has no saved settings":                    {"--continue", "fresh"},
		"no project in":                            {"--continue", filepath.Join(projects, "nowhere")},
	}
	for message, args := range cases {
		command := sandboxCommand()
		command.SetArgs(append(args, "--projects-dir", projects, "--config", t.TempDir()))
		command.SetOut(new(strings.Builder))
		command.SetErr(new(strings.Builder))
		if err := command.Execute(); err == nil || !strings.Contains(err.Error(), message) {
			t.Errorf("%v: got %v, want %q", args, err, message)
		}
	}
}

func TestProjectsAreCopiedMovedAndRemovedByName(t *testing.T) {
	projects := t.TempDir()
	sandbox.RunningDirectory = func() string { return filepath.Join(projects, ".running") }
	t.Cleanup(func() { sandbox.RunningDirectory = func() string { return "" } })
	state, err := sandbox.PrepareState(filepath.Join(projects, "study42"))
	if err != nil {
		t.Fatal(err)
	}
	settings := sandbox.Settings{Engine: "docker", Port: 41234}
	if err := settings.Renew(); err != nil {
		t.Fatal(err)
	}
	if err := sandbox.WriteSettings(sandbox.SettingsFile(state), "study42", settings); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (string, error) {
		command := projectsCommand()
		var out strings.Builder
		command.SetArgs(append(args, "--projects-dir", projects, "--config", t.TempDir()))
		command.SetOut(&out)
		command.SetErr(new(strings.Builder))
		err := command.Execute()
		return out.String(), err
	}

	if out, err := run("copy", "study42", "copy"); err != nil || !strings.Contains(out, "--continue copy --projects-dir "+projects) {
		t.Fatalf("copy: %q %v", out, err)
	}
	if out, err := run("move", "copy", "renamed"); err != nil || !strings.Contains(out, "--continue renamed") {
		t.Fatalf("move: %q %v", out, err)
	}
	out, err := run("list")
	if err != nil || !strings.Contains(out, "renamed") || !strings.Contains(out, "study42") || strings.Contains(out, "copy ") {
		t.Fatalf("list: %q %v", out, err)
	}
	if strings.Contains(out, "SIZE") || !strings.Contains(out, state+"\n") {
		t.Fatalf("list with size or without path: %q", out)
	}
	out, err = run("list", "--size")
	if err != nil || !strings.Contains(out, "SIZE") || !strings.Contains(out, "KiB") || !strings.Contains(out, state+"\n") {
		t.Fatalf("list --size without size and path: %q %v", out, err)
	}
	out, err = run("show", "study42")
	if err != nil || !strings.Contains(out, "port       41234") || !strings.Contains(out, "--continue study42") {
		t.Fatalf("show: %q %v", out, err)
	}
	if _, err := run("show", "missing"); err == nil || !strings.Contains(err.Error(), "no project missing") {
		t.Fatalf("show missing: %v", err)
	}
	// Tests run without a terminal: removing needs --yes there.
	if _, err := run("remove", "renamed"); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("remove without --yes: %v", err)
	}
	if _, err := run("remove", "renamed", "--yes"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(projects, "renamed")); !os.IsNotExist(err) {
		t.Fatalf("not removed: %v", err)
	}
}

func TestRemovingAProjectAsks(t *testing.T) {
	project := sandbox.Project{Name: "p", State: "/nowhere/p"}
	for answer, want := range map[string]bool{"y\n": true, "yes\n": true, "\n": false, "no\n": false, "": false} {
		if got := confirmRemove(strings.NewReader(answer), new(strings.Builder), project); got != want {
			t.Errorf("%q: %v", answer, got)
		}
	}
}

func TestFormatSize(t *testing.T) {
	for bytes, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1536: "1.5 KiB", 5 << 30: "5.0 GiB"} {
		if got := formatSize(bytes); got != want {
			t.Errorf("formatSize(%d) = %q, want %q", bytes, got, want)
		}
	}
}

func TestContinuingWithCreateStartsAMissingProject(t *testing.T) {
	projects := t.TempDir()
	if _, err := sandbox.PrepareState(filepath.Join(projects, "fresh")); err != nil {
		t.Fatal(err)
	}
	for _, argument := range []string{"study42", "fresh", filepath.Join(projects, "elsewhere")} {
		options := sandboxOptions{}
		options.projectsDir, options.config = projects, t.TempDir()
		settings, found, err := options.continueProject(argument, true)
		if err != nil || found || settings.Key != "" {
			t.Errorf("%s: found %v, err %v", argument, found, err)
		}
		if want := filepath.Join(projects, filepath.Base(argument)); options.stateDirectory() != want {
			t.Errorf("%s: state %s, want %s", argument, options.stateDirectory(), want)
		}
	}
}

func TestAStartWithoutContinueKeepsTheSavedPortIfItIsFree(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	taken := listener.Addr().(*net.TCPAddr).Port
	if port := savedPort(taken, "docker", "0.0.0.0"); port != 0 {
		t.Errorf("taken port %d: got %d, want any free port (0)", taken, port)
	}
	listener.Close()
	if port := savedPort(taken, "docker", "0.0.0.0"); port != taken {
		t.Errorf("free port %d: got %d", taken, port)
	}
	if port := savedPort(0, "docker", "0.0.0.0"); port != 0 {
		t.Errorf("no saved port: got %d", port)
	}
}

func TestTheVSCodeLinkIsTheBridgesLocalLine(t *testing.T) {
	connection := sandbox.Connection{URL: "codeflower://10.0.0.5:4100?token=a%2Bb&fp=xyz", Host: "10.0.0.5", Port: 4100, Token: "a+b", Fingerprint: "xyz"}
	if line := localLine(connection, reachableOnLoopback(sandbox.EngineApptainer, "0.0.0.0", false)); line != "codeflower://127.0.0.1:4100?token=a%2Bb&fp=xyz" {
		t.Errorf("all interfaces: %s", line)
	}
	for _, local := range []bool{
		reachableOnLoopback(sandbox.EngineApptainer, "10.0.0.5", false),
		reachableOnLoopback(sandbox.EngineApptainer, "0.0.0.0", true),
	} {
		if line := localLine(connection, local); line != connection.URL {
			t.Errorf("not on loopback: %s", line)
		}
	}
}

func TestTheOpenLinksAreOneTable(t *testing.T) {
	connection := sandbox.Connection{URL: "codeflower://10.0.0.5:4100?token=k", Host: "10.0.0.5", Port: 4100, Token: "k"}
	vscode := client.VSCodeLink("codeflower://127.0.0.1:4100?token=k")
	warning := "In VS Code, Ctrl+click a link, or run \"code flower: Connect to worker\" and paste the codeflower:// line.\n" +
		"Anyone with these links can control the agents. Keep job logs private,\n" +
		"e.g. sbatch --output=$HOME/.codeflower-logs/%j.out with that directory chmod 700.\n"
	cases := map[string]struct {
		links sandboxLinks
		want  string
	}{
		"tunnel": {
			sandboxLinks{connection: connection, options: linkOptions{tunnel: true, local: true},
				remote: "https://tunnel.example/#/chat/x", localPage: "http://127.0.0.1:7720/#/chat/x"},
			"\nScan the code with a phone, or open a link:\n\n" +
				"  Paired browsers     https://tunnel.example/#/chat/x\n" +
				"  This computer       http://127.0.0.1:7720/#/chat/x\n" +
				"  VS Code here        " + vscode + "\n" +
				"  VS Code elsewhere   codeflower://10.0.0.5:4100?token=k\n\n" + warning +
				"No browser is paired yet: run \"codeflower tunnel pair\".\n\n",
		},
		"no client": {
			sandboxLinks{connection: connection, options: linkOptions{local: true}},
			"\nOpen the sandbox:\n\n" +
				"  Browser (app running)   http://127.0.0.1:7720/#add=codeflower%3A%2F%2F10.0.0.5%3A4100%3Ftoken%3Dk\n" +
				"  VS Code here            " + vscode + "\n" +
				"  VS Code elsewhere       codeflower://10.0.0.5:4100?token=k\n\n" + warning + "\n",
		},
	}
	for name, test := range cases {
		var out strings.Builder
		printOpenLinks(&out, test.links)
		if out.String() != test.want {
			t.Errorf("%s: got\n%s\nwant\n%s", name, out.String(), test.want)
		}
	}
	var out strings.Builder
	printOpenLinks(&out, sandboxLinks{connection: connection, options: linkOptions{page: true}})
	if !strings.Contains(out.String(), "  Browser           http://10.0.0.5:4100/?key=k\n") ||
		!strings.Contains(out.String(), "  VS Code (paste)   codeflower://10.0.0.5:4100?token=k\n") ||
		!strings.Contains(out.String(), "plain HTTP") {
		t.Errorf("with --server:\n%s", out.String())
	}
}
