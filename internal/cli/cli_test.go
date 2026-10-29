package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codeflower-launcher/internal/sandbox"
)

func TestRunRequiresAKnownAgent(t *testing.T) {
	command := runCommand()
	command.SetArgs([]string{"--state", t.TempDir(), "--engine", "docker", "--agent", "gemini"})
	err := command.Execute()
	if err == nil || err.Error() != "--agent must be one of: claude, codex, dsh" {
		t.Fatalf("got %v", err)
	}
}

func TestRunRejectsUnknownEnginesAndProjectNames(t *testing.T) {
	cases := map[string][]string{
		"--engine must be":  {"--agent", "claude", "--engine", "lxc"},
		"--project must be": {"--agent", "claude", "--project", "../escape"},
	}
	for message, args := range cases {
		command := runCommand()
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
