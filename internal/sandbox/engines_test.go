package sandbox

import (
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestInstanceIDsAndTokens(t *testing.T) {
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(NewInstanceID()) {
		t.Fatal("instance id is not a UUIDv4")
	}
	first, second := NewToken(), NewToken()
	if len(first) < 43 || first == second {
		t.Fatalf("weak or repeated token: %q %q", first, second)
	}
}

func statuses(ready ...string) []EngineStatus {
	var result []EngineStatus
	for _, name := range EngineNames {
		status := EngineStatus{Name: name, Label: EngineLabel(name)}
		for _, other := range ready {
			if other == name {
				status.Installed, status.Ready = true, true
			}
		}
		result = append(result, status)
	}
	return result
}

func TestAutoPrefersDockerThenPodmanThenApptainer(t *testing.T) {
	cases := []struct {
		ready []string
		image string
		want  string
	}{
		{[]string{EngineDocker, EnginePodman, EngineApptainer}, "", EngineDocker},
		{[]string{EnginePodman, EngineApptainer}, "", EnginePodman},
		{[]string{EngineApptainer}, "", EngineApptainer},
		{[]string{EngineDocker, EngineApptainer}, "/images/codeflower-sandbox.sif", EngineApptainer},
	}
	for _, c := range cases {
		got, err := ChooseEngine(statuses(c.ready...), "auto", c.image)
		if err != nil || got.Name != c.want {
			t.Errorf("ready %v image %q: got %q %v, want %q", c.ready, c.image, got.Name, err, c.want)
		}
	}
}

func TestNoEngineIsReportedForTheInstallGuide(t *testing.T) {
	if _, err := ChooseEngine(statuses(), "auto", ""); !errors.Is(err, ErrNoEngine) {
		t.Fatalf("got %v", err)
	}
	if _, err := ChooseEngine(statuses(EngineDocker), "podman", ""); err == nil || !strings.Contains(err.Error(), "Podman is not installed") {
		t.Fatalf("got %v", err)
	}
	stopped := statuses()
	stopped[0].Installed, stopped[0].Problem = true, "Docker is installed, but it is not running."
	if _, err := ChooseEngine(stopped, "", ""); !errors.Is(err, ErrNoEngine) || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("got %v", err)
	}
}

func guideText(guide InstallGuide) string {
	var text strings.Builder
	for _, option := range guide.Options {
		text.WriteString(option.Title + "\n")
		for _, step := range option.Steps {
			text.WriteString(step.Text + "\n" + step.Command + "\n" + step.Link + "\n")
		}
	}
	return text.String()
}

func TestInstallGuidesFitTheSystem(t *testing.T) {
	cases := []struct {
		name    string
		guide   InstallGuide
		want    []string
		first   string
		without string
	}{
		{"mac", GuideFor("darwin", LinuxRelease{}, false, false), []string{"brew install --cask docker", "podman machine init"}, "Docker Desktop", "apt-get"},
		{"windows", GuideFor("windows", LinuxRelease{}, false, false), []string{"wsl --install", "winget install -e --id Docker.DockerDesktop"}, "Docker Desktop", "sudo"},
		{"ubuntu", GuideFor("linux", LinuxRelease{ID: "ubuntu", IDLike: "debian", Name: "Ubuntu 24.04"}, false, false), []string{"get.docker.com", "apt-get install -y podman", "usermod -aG docker", "docs.docker.com/engine/install/ubuntu/"}, "Docker Engine", "brew"},
		{"rocky", GuideFor("linux", LinuxRelease{ID: "rocky", IDLike: "rhel centos fedora"}, false, false), []string{"dnf install -y podman"}, "Docker Engine", "apt-get"},
		{"cluster", GuideFor("linux", LinuxRelease{ID: "rhel"}, false, true), []string{"module load apptainer"}, "On a shared cluster: Apptainer", "brew"},
		{"wsl", GuideFor("linux", LinuxRelease{ID: "ubuntu", Name: "Ubuntu"}, true, false), []string{"WSL integration"}, "Docker Desktop", "brew"},
	}
	for _, c := range cases {
		text := guideText(c.guide)
		for _, want := range c.want {
			if !strings.Contains(text, want) {
				t.Errorf("%s: guide lacks %q", c.name, want)
			}
		}
		if c.guide.Options[0].Title != c.first {
			t.Errorf("%s: first option %q, want %q", c.name, c.guide.Options[0].Title, c.first)
		}
		if strings.Contains(text, c.without) {
			t.Errorf("%s: guide mentions %q", c.name, c.without)
		}
	}
}

func TestPodmanArgsKeepTheTokenOutOfTheCommandLine(t *testing.T) {
	dir := tempDir(t)
	args, err := PodmanArgs("/usr/bin/podman", PodmanRun{
		Name:  "codeflower-x",
		Image: "codeflower-sandbox",
		State: dir,
		Binds: []Bind{{Source: dir, Target: "/data/input/study", ReadOnly: true}},
		Env:   map[string]string{"CODEFLOWER_TOKEN": "secret-value"},
		Port:  7000,
	})
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Join(args, " ")
	for _, want := range []string{
		"run --rm --init", "--env CODEFLOWER_TOKEN", "--publish 127.0.0.1:7000:7000",
		"type=bind,source=" + dir + ",target=/data/input/study,readonly", "type=bind,source=" + dir + ",target=" + ContainerState,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("podman args lack %q: %s", want, line)
		}
	}
	if strings.Contains(line, "secret-value") {
		t.Fatal("the token is on the command line")
	}
	if args[len(args)-1] != "codeflower-sandbox" {
		t.Fatalf("image is not last: %s", line)
	}

	args, err = PodmanArgs("/usr/bin/podman", PodmanRun{
		Name:  "codeflower-x",
		Image: "codeflower-sandbox",
		State: dir,
		Stdin: strings.NewReader(""),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(args, "-i") {
		t.Fatalf("Podman did not attach stdin: %v", args)
	}
}

func TestBindsAcceptWindowsDrivesAndCheckFolders(t *testing.T) {
	dir := tempDir(t)
	if _, err := ParseBind(`C:\missing\data:/data/input/x`); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("drive letter was not parsed as a path: %v", err)
	}
	file := dir + "/file.txt"
	if err := writeOnce(file, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := NewBind(file, OutputsRoot+"/file", false); err == nil || !strings.Contains(err.Error(), "must be a folder") {
		t.Fatalf("a file was accepted as an output folder: %v", err)
	}
}
