package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"codeflower-launcher/internal/sandbox"
)

func TestInteractiveRequiresAKnownAgent(t *testing.T) {
	command := interactiveCommand()
	command.SetArgs([]string{"--state", t.TempDir(), "--engine", "docker", "--agent", "gemini"})
	err := command.Execute()
	if err == nil || err.Error() != "--agent must be one of: claude, codex, dsh" {
		t.Fatalf("got %v", err)
	}
}

func TestInteractiveRejectsUnknownEnginesAndProjectNames(t *testing.T) {
	cases := map[string][]string{
		"--engine must be":  {"--agent", "claude", "--engine", "lxc"},
		"--project must be": {"--agent", "claude", "--project", "../escape"},
	}
	for message, args := range cases {
		command := interactiveCommand()
		command.SetArgs(args)
		if err := command.Execute(); err == nil || !strings.Contains(err.Error(), message) {
			t.Errorf("%v: got %v, want %q", args, err, message)
		}
	}
}

func TestAskAgentAcceptsNumbersNamesAndTheDefault(t *testing.T) {
	for input, want := range map[string]string{"\n": "claude", "2\n": "codex", "dsh\n": "dsh"} {
		got, err := askAgent(strings.NewReader(input), new(strings.Builder))
		if err != nil || got != want {
			t.Errorf("askAgent(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	if _, err := askAgent(strings.NewReader("7\n"), new(strings.Builder)); err == nil {
		t.Error("askAgent accepted 7")
	}
}

func TestFoldersMountUnderData(t *testing.T) {
	input := t.TempDir()
	output := filepath.Join(t.TempDir(), "results")
	code := t.TempDir()
	binds, err := collectBinds(sandbox.Folders{Inputs: []string{input}, Outputs: []string{output}, Code: []string{code}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(binds) != 3 || !strings.HasPrefix(binds[0].Target, sandbox.InputsRoot+"/") || !binds[0].ReadOnly ||
		binds[1].Target != sandbox.OutputsRoot+"/results" || binds[1].ReadOnly ||
		!strings.HasPrefix(binds[2].Target, sandbox.CodeRoot+"/") || binds[2].ReadOnly {
		t.Fatalf("unexpected binds %+v", binds)
	}
	if info, err := os.Stat(output); err != nil || !info.IsDir() {
		t.Fatalf("--output was not created: %v", err)
	}
	// A bare --bind is another input, and folders keep out of its way.
	binds, err = collectBinds(sandbox.Folders{Inputs: []string{input}}, []string{input + "/../" + filepath.Base(input)})
	if err != nil {
		t.Fatal(err)
	}
	if len(binds) != 2 || binds[0].Target == binds[1].Target {
		t.Fatalf("unexpected binds %+v", binds)
	}
}

func TestProjectsLiveInTheProjectsDirectory(t *testing.T) {
	options := commonOptions{project: "study42"}
	if got := options.stateDirectory(); got != filepath.Join(ProjectsDirectory(), "study42") {
		t.Fatalf("got %s", got)
	}
	options.state = "/somewhere"
	if got := options.stateDirectory(); got != "/somewhere" {
		t.Fatalf("--state did not win: %s", got)
	}
}

func TestTaskChecksItsArguments(t *testing.T) {
	output := t.TempDir()
	cases := map[string][]string{
		"give the task as an":    {"--agent", "claude", "--output", output},
		"not both":               {"--agent", "claude", "--output", output, "--prompt", "task.md", "x"},
		"no such file":           {"--agent", "claude", "--output", output, "--prompt", "missing.md"},
		"the task is empty":      {"--agent", "claude", "--output", output, "  "},
		"--output is required":   {"--agent", "claude", "Cluster the cells"},
		"--max-rounds must be":   {"--agent", "claude", "--output", output, "--max-rounds", "0", "x"},
		"--timeout must not be":  {"--agent", "claude", "--output", output, "--timeout", "-1m", "x"},
		"--output-mode must be":  {"--agent", "claude", "--output", output, "--output-mode", "pretty", "x"},
		"--agent must be one of": {"--agent", "gemini", "--output", output, "x"},
		"--allowlist needs":      {"--agent", "claude", "--output", output, "--allowlist", "hosts.txt", "x"},
		"unknown flag: --port":   {"--agent", "claude", "--output", output, "--port", "7710", "x"},
	}
	for message, args := range cases {
		command := taskCommand()
		command.SetArgs(append([]string{"--state", t.TempDir()}, args...))
		command.SetOut(new(strings.Builder))
		command.SetErr(new(strings.Builder))
		if err := command.Execute(); err == nil || !strings.Contains(err.Error(), message) {
			t.Errorf("%v: got %v, want %q", args, err, message)
		}
	}
}

func TestRunRequiresOneNonEmptyTaskArgument(t *testing.T) {
	cases := map[string][]string{
		"give the task as an": {},
		"the task is empty":   {""},
		"not both":            {"--prompt", "task.md", "x"},
		"no such file":        {"--prompt", "missing.md"},
	}
	for message, args := range cases {
		command := runCommand()
		command.SetArgs(append([]string{"--state", t.TempDir(), "--engine", "docker"}, args...))
		command.SetOut(new(strings.Builder))
		command.SetErr(new(strings.Builder))
		if err := command.Execute(); err == nil || !strings.Contains(err.Error(), message) {
			t.Errorf("%v: got %v, want %q", args, err, message)
		}
	}
}

func TestRunPromptIsStoredPrivately(t *testing.T) {
	state := t.TempDir()
	if err := sandbox.WriteRunPrompt(state, "run-id", "Analyse the input"); err != nil {
		t.Fatal(err)
	}
	got, err := sandbox.ReadRunPrompt(state, "run-id")
	if err != nil || got != "Analyse the input\n" {
		t.Fatalf("got %q, %v", got, err)
	}
	info, err := os.Stat(filepath.Join(state, "runs", "run-id", "prompt.txt"))
	if err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("prompt permissions: %v, %v", info, err)
	}
}

func TestTaskTextComesFromTheArgumentOrStandardInput(t *testing.T) {
	if text, err := readTask("  Cluster the cells\n", nil); err != nil || text != "Cluster the cells" {
		t.Fatalf("got %q, %v", text, err)
	}
	if text, err := readTask("-", strings.NewReader("# Task\n\nCluster the cells\n")); err != nil || text != "# Task\n\nCluster the cells" {
		t.Fatalf("got %q, %v", text, err)
	}
	if _, err := readTask("-", strings.NewReader("\n")); err == nil {
		t.Fatal("accepted an empty task from standard input")
	}
}

func TestTaskTextComesFromTheArgumentOrThePromptFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "task.md")
	if err := os.WriteFile(file, []byte("\n# Task\n\nCluster the cells\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if text, argument, err := readPrompt(nil, file, nil, "task"); err != nil || text != "# Task\n\nCluster the cells" || argument != "" {
		t.Fatalf("got %q, %q, %v", text, argument, err)
	}
	if text, argument, err := readPrompt([]string{"Cluster the cells"}, "", nil, "task"); err != nil || text != "Cluster the cells" || argument != "Cluster the cells" {
		t.Fatalf("got %q, %q, %v", text, argument, err)
	}
	if text, _, err := readPrompt(nil, "", nil, "follow-up"); err != nil || text != "" {
		t.Fatalf("got %q, %v for no follow-up", text, err)
	}
	for _, missing := range []string{filepath.Join(t.TempDir(), "missing.md"), t.TempDir()} {
		if _, _, err := readPrompt(nil, missing, nil, "task"); err == nil {
			t.Errorf("accepted --prompt %s", missing)
		}
	}
	if err := checkPromptArgs([]string{"Cluster the cells"}, file, false, "task"); err == nil {
		t.Error("accepted both an argument and --prompt")
	}
	if err := checkPromptArgs(nil, "", true, "task"); err == nil {
		t.Error("accepted neither an argument nor --prompt")
	}
	if err := checkPromptArgs(nil, "", false, "follow-up"); err != nil {
		t.Errorf("a follow-up was required: %v", err)
	}
	if err := checkPromptArgs(nil, file, true, "task"); err != nil {
		t.Errorf("--prompt alone was refused: %v", err)
	}
}

func TestTaskSignInNeedsAnInteractiveArgument(t *testing.T) {
	if !canSignInFromTask("Cluster the cells", true) {
		t.Fatal("an interactive task argument could not sign in")
	}
	for _, test := range []struct {
		argument string
		terminal bool
	}{
		{"-", true},
		{"Cluster the cells", false},
	} {
		if canSignInFromTask(test.argument, test.terminal) {
			t.Errorf("allowed task sign-in for argument %q with terminal %t", test.argument, test.terminal)
		}
	}
}

func TestTaskResultsAreFoundOnTheHost(t *testing.T) {
	input, first, second := t.TempDir(), t.TempDir(), t.TempDir()
	first, err := filepath.EvalSymlinks(first)
	if err != nil {
		t.Fatal(err)
	}
	binds, err := collectBinds(sandbox.Folders{Inputs: []string{input}, Outputs: []string{first, second}},
		[]string{second + ":" + sandbox.OutputsRoot + "/extra:rw"})
	if err != nil {
		t.Fatal(err)
	}
	output := outputBind(binds)
	if output == nil || output.Source != first {
		t.Fatalf("the results go to %+v, not the first --output", output)
	}
	if got := hostPath(binds, output.Target+"/report.html"); got != filepath.Join(first, "report.html") {
		t.Fatalf("report at %s", got)
	}
	if got := hostPath(binds, "/codeflower-state/workspace/x"); got != "/codeflower-state/workspace/x" {
		t.Fatalf("a path outside the mounts became %s", got)
	}
	if outputBind(binds[:1]) != nil {
		t.Fatal("an input folder was taken for the results")
	}
}

func TestTaskSummaryIsOneShortLine(t *testing.T) {
	if got := summarize("Cluster the cells\nthen annotate them"); got != "Cluster the cells…" {
		t.Fatalf("got %q", got)
	}
	if got := summarize(strings.Repeat("x", 150)); len([]rune(got)) != 100 {
		t.Fatalf("got %d characters", len([]rune(got)))
	}
}

func TestTaskOptionsAreIDEqualsValue(t *testing.T) {
	settings, err := parseSettings([]string{"fast=on", " ultracode = off "})
	if err != nil || len(settings) != 2 || settings["fast"] != "on" || settings["ultracode"] != "off" {
		t.Fatalf("got %v, %v", settings, err)
	}
	if settings, err := parseSettings(nil); err != nil || settings != nil {
		t.Fatalf("no options gave %v, %v", settings, err)
	}
	for _, bad := range []string{"fast", "=on", "fast="} {
		if _, err := parseSettings([]string{bad}); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestImageTagPicksAPublishedBuild(t *testing.T) {
	if image, err := imageName("", ""); err != nil || image != "" {
		t.Fatalf("no flags: %q, %v", image, err)
	}
	if image, err := imageName("", "v0.2.1"); err != nil || image != sandbox.ImageRepository+":0.2.1" {
		t.Fatalf("--image-tag: %q, %v", image, err)
	}
	if _, err := imageName("my-image", "0.2"); err == nil {
		t.Fatal("--image and --image-tag were accepted together")
	}
}

func TestConfigDefaultsAndSkills(t *testing.T) {
	directory := t.TempDir()
	if err := os.Mkdir(filepath.Join(directory, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "config.yaml"), []byte(`
engine: podman
isolate_network: true
agents:
  claude: {model: opus, effort: high, options: {fast: "off", speed: slow}}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEFLOWER_ENGINE", "")
	t.Setenv("CODEFLOWER_IMAGE", "")
	other := t.TempDir()

	options := sandboxOptions{agent: "claude", folders: sandbox.Folders{Skills: []string{other}}}
	options.config = directory
	options.engine = "auto"
	options.changed = func(string) bool { return false }
	if err := options.applyConfig(); err != nil {
		t.Fatal(err)
	}
	if options.engine != "podman" || !options.isolateNetwork {
		t.Fatalf("config not applied: %s %v", options.engine, options.isolateNetwork)
	}
	if len(options.folders.Skills) != 2 || options.folders.Skills[0] != filepath.Join(directory, "skills") || options.folders.Skills[1] != other {
		t.Fatalf("skills %v", options.folders.Skills)
	}
	model, effort, settings := options.taskDefaults("", "low", map[string]string{"speed": "fast"})
	if model != "opus" || effort != "low" || settings["fast"] != "off" || settings["speed"] != "fast" {
		t.Fatalf("got %q %q %v", model, effort, settings)
	}

	// A flag or an environment variable wins over the config, and a skills
	// folder given on the command line is not mounted twice.
	t.Setenv("CODEFLOWER_ENGINE", "docker")
	options = sandboxOptions{agent: "claude", folders: sandbox.Folders{Skills: []string{filepath.Join(directory, "skills")}}}
	options.config = directory
	options.engine = "docker"
	options.changed = func(name string) bool { return name == "isolate-network" }
	if err := options.applyConfig(); err != nil {
		t.Fatal(err)
	}
	if options.engine != "docker" || options.isolateNetwork || len(options.folders.Skills) != 1 {
		t.Fatalf("got %s %v %v", options.engine, options.isolateNetwork, options.folders.Skills)
	}
}

func TestContinuingATaskChecksItsArguments(t *testing.T) {
	state, err := sandbox.PrepareState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(t.TempDir(), "gone")
	task := sandbox.Task{Task: "Cluster the cells", Agent: "claude", Output: sandbox.OutputsRoot + "/gone",
		Mounts: []sandbox.TaskMount{{Source: gone, Target: sandbox.OutputsRoot + "/gone"}}}
	if err := sandbox.WriteTask(state, "abc", task); err != nil {
		t.Fatal(err)
	}
	directory := sandbox.TaskDirectory(state, "abc")
	cases := map[string][]string{
		"--input cannot be used with --continue":       {"--continue", directory, "--input", t.TempDir()},
		"--agent cannot be used with --continue":       {"--continue", directory, "--agent", "codex"},
		"--project cannot be used with --continue":     {"--continue", directory, "--project", "other"},
		"--skip-report cannot be used with --continue": {"--continue", directory, "--skip-report"},
		"accepts at most 1 arg":                        {"--continue", directory, "more", "work"},
		"the follow-up is empty":                       {"--continue", directory, " "},
		"is not a task folder":                         {"--continue", state},
		"missing: " + gone:                             {"--continue", directory},
	}
	for message, args := range cases {
		command := taskCommand()
		command.SetArgs(args)
		command.SetOut(new(strings.Builder))
		command.SetErr(new(strings.Builder))
		if err := command.Execute(); err == nil || !strings.Contains(err.Error(), message) {
			t.Errorf("%v: got %v, want %q", args, err, message)
		}
	}
}

func TestTasksWrapUpShortlyBeforeTheirTimeout(t *testing.T) {
	for _, test := range []struct{ timeout, asked, want time.Duration }{
		{90 * time.Minute, 0, 9 * time.Minute},
		{10 * time.Minute, 0, 2 * time.Minute},
		{3 * time.Minute, 0, 90 * time.Second},
		{7*time.Hour + 30*time.Minute, 0, 15 * time.Minute},
		{7*time.Hour + 30*time.Minute, 30 * time.Minute, 30 * time.Minute},
		{time.Hour, time.Hour, 30 * time.Minute},
	} {
		if got := wrapUpReserve(test.timeout, test.asked); got != test.want {
			t.Errorf("--timeout %s --wrap-up %s wraps up %s before, want %s", test.timeout, test.asked, got, test.want)
		}
	}
}

func TestTheContinueCommandCanBePasted(t *testing.T) {
	if got := continueCommand("/home/u/codeflower/projects/p/tasks/abc", false); got != "codeflower task --continue /home/u/codeflower/projects/p/tasks/abc" {
		t.Errorf("got %s", got)
	}
	if got := continueCommand("/home/u/my projects/it's", true); got != `codeflower task --continue '/home/u/my projects/it'\''s' "FOLLOW-UP"` {
		t.Errorf("got %s", got)
	}
	if got := shellQuote("~/tasks/$x"); got != `'~/tasks/$x'` {
		t.Errorf("got %s", got)
	}
}

func TestTaskSummarySaysHowTheAttemptWentOn(t *testing.T) {
	var out strings.Builder
	result := sandbox.TaskResult{Status: "unmet", Rounds: 2, Attempt: 3, Message: "The time limit was near; wrapped up: more to do",
		Continuation: &sandbox.TaskContinuation{Kind: "interrupted", FollowUp: true},
		WrapUp:       &sandbox.TaskWrapUp{Reason: "time"}}
	printTaskResult(&out, result, preparedSandbox{}, time.Minute)
	for _, want := range []string{"Task not done after 1m0s (2 rounds)", "attempt  3, continued the session after an interruption, with a follow-up",
		"wrap-up  the time was almost up"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("summary %q does not contain %q", out.String(), want)
		}
	}
}
