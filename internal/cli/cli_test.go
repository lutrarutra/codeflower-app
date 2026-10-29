package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
		"accepts 1 arg":          {"--agent", "claude", "--output", output},
		"the task is empty":      {"--agent", "claude", "--output", output, "  "},
		"--output is required":   {"--agent", "claude", "Cluster the cells"},
		"--max-rounds must be":   {"--agent", "claude", "--output", output, "--max-rounds", "0", "x"},
		"--timeout must not be":  {"--agent", "claude", "--output", output, "--timeout", "-1m", "x"},
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

func TestTaskResultsAreFoundOnTheHost(t *testing.T) {
	input, first, second := t.TempDir(), t.TempDir(), t.TempDir()
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
