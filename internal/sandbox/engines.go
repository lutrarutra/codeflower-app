package sandbox

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/docker/docker/client"
)

// Engines the launcher can run the sandbox with.
const (
	EngineDocker    = "docker"
	EnginePodman    = "podman"
	EngineApptainer = "apptainer"
)

// EngineNames are the engines in the order "auto" prefers them on a
// workstation; a .sif image always selects Apptainer.
var EngineNames = []string{EngineDocker, EnginePodman, EngineApptainer}

// DefaultImage is the sandbox image name for Docker and Podman.
const DefaultImage = "codeflower-sandbox"

// EngineStatus says whether an engine can run sandboxes on this machine.
type EngineStatus struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	// Installed: the engine's command (or, for Docker, its daemon) exists.
	Installed bool `json:"installed"`
	// Ready: it can start containers now.
	Ready      bool   `json:"ready"`
	Version    string `json:"version,omitempty"`
	Executable string `json:"executable,omitempty"`
	// Problem explains why an installed engine is not ready, and Fix how to
	// solve it.
	Problem string `json:"problem,omitempty"`
	Fix     []Step `json:"fix,omitempty"`
}

var engineLabels = map[string]string{
	EngineDocker:    "Docker",
	EnginePodman:    "Podman",
	EngineApptainer: "Apptainer",
}

// EngineLabel is the engine's product name.
func EngineLabel(name string) string {
	if label, ok := engineLabels[name]; ok {
		return label
	}
	return name
}

func commandOutput(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

// DetectEngines checks every engine; it takes a few seconds at most.
func DetectEngines(ctx context.Context) []EngineStatus {
	results := make([]EngineStatus, len(EngineNames))
	done := make(chan struct{})
	for index, name := range EngineNames {
		go func() {
			defer func() { done <- struct{}{} }()
			switch name {
			case EngineDocker:
				results[index] = detectDocker(ctx)
			case EnginePodman:
				results[index] = detectPodman(ctx)
			case EngineApptainer:
				results[index] = detectApptainer(ctx)
			}
		}()
	}
	for range EngineNames {
		<-done
	}
	return results
}

func detectDocker(ctx context.Context) EngineStatus {
	status := EngineStatus{Name: EngineDocker, Label: "Docker"}
	executable, lookError := exec.LookPath("docker")
	status.Executable = executable
	docker, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		status.Installed = lookError == nil
		status.Problem = err.Error()
		return status
	}
	defer docker.Close()
	ping, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	version, err := docker.ServerVersion(ping)
	if err == nil {
		status.Installed, status.Ready = true, true
		status.Version = version.Version
		// Podman can serve the Docker API; say so rather than call it Docker.
		for _, component := range version.Components {
			if strings.Contains(strings.ToLower(component.Name), "podman") {
				status.Label = "Docker (served by Podman)"
			}
		}
		return status
	}
	socket := "/var/run/docker.sock"
	if host := os.Getenv("DOCKER_HOST"); strings.HasPrefix(host, "unix://") {
		socket = strings.TrimPrefix(host, "unix://")
	}
	_, socketError := os.Stat(socket)
	status.Installed = lookError == nil || socketError == nil
	if !status.Installed {
		return status
	}
	message := err.Error()
	switch {
	case strings.Contains(message, "permission denied"):
		status.Problem = "Docker is installed, but this user may not use it."
		status.Fix = dockerPermissionFix()
	default:
		status.Problem = "Docker is installed, but it is not running."
		status.Fix = dockerStartFix()
	}
	return status
}

func detectPodman(ctx context.Context) EngineStatus {
	status := EngineStatus{Name: EnginePodman, Label: "Podman"}
	executable, err := exec.LookPath("podman")
	if err != nil {
		return status
	}
	status.Installed, status.Executable = true, executable
	if version, err := commandOutput(ctx, executable, "version", "--format", "{{.Client.Version}}"); err == nil {
		status.Version = version
	}
	if _, err := commandOutput(ctx, executable, "info", "--format", "{{.Host.Arch}}"); err != nil {
		status.Problem = "Podman is installed, but it cannot run containers yet."
		if runtime.GOOS == "linux" {
			status.Fix = []Step{{Text: "Check what Podman reports:", Command: "podman info"}}
		} else {
			status.Fix = []Step{
				{Text: "Podman runs in a small virtual machine on this system. Create and start it once:", Command: "podman machine init\npodman machine start"},
			}
		}
		return status
	}
	status.Ready = true
	return status
}

func detectApptainer(ctx context.Context) EngineStatus {
	status := EngineStatus{Name: EngineApptainer, Label: "Apptainer"}
	executable := ApptainerExecutable()
	if executable == "" {
		return status
	}
	status.Installed, status.Ready, status.Executable = true, true, executable
	if filepath.Base(executable) == "singularity" {
		status.Label = "Singularity"
	}
	if version, err := commandOutput(ctx, executable, "--version"); err == nil {
		status.Version = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(version, "apptainer version"), "singularity-ce version"))
	}
	return status
}

// ApptainerExecutable finds apptainer, or its predecessor singularity.
func ApptainerExecutable() string {
	for _, name := range []string{"apptainer", "singularity"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	return ""
}

// IsSIF says whether an image refers to an Apptainer image file.
func IsSIF(image string) bool {
	if strings.HasSuffix(strings.ToLower(image), ".sif") {
		return true
	}
	info, err := os.Stat(expandHome(image))
	return err == nil && !info.IsDir()
}

// ErrNoEngine means no engine can run the sandbox; the caller shows the
// install guide.
var ErrNoEngine = errors.New("no container engine can run the sandbox on this machine")

// ChooseEngine resolves "auto" (or "") to a ready engine, and checks that a
// named engine is ready.
func ChooseEngine(statuses []EngineStatus, requested, image string) (EngineStatus, error) {
	byName := map[string]EngineStatus{}
	for _, status := range statuses {
		byName[status.Name] = status
	}
	if requested != "" && requested != "auto" {
		status, ok := byName[requested]
		if !ok {
			return EngineStatus{}, fmt.Errorf("unknown engine %q: use auto, %s", requested, strings.Join(EngineNames, ", "))
		}
		if !status.Ready {
			if status.Problem != "" {
				return status, errors.New(status.Problem)
			}
			return status, fmt.Errorf("%s is not installed", status.Label)
		}
		return status, nil
	}
	if image != "" && IsSIF(image) {
		if status := byName[EngineApptainer]; status.Ready {
			return status, nil
		}
		return byName[EngineApptainer], fmt.Errorf("%s is an Apptainer image, but Apptainer is not installed", image)
	}
	for _, name := range EngineNames {
		if status := byName[name]; status.Ready {
			return status, nil
		}
	}
	for _, name := range EngineNames {
		if status := byName[name]; status.Installed && status.Problem != "" {
			return status, fmt.Errorf("%w: %s", ErrNoEngine, status.Problem)
		}
	}
	return EngineStatus{}, ErrNoEngine
}

// Step is one instruction: text, optionally a command to run or a link.
type Step struct {
	Text    string `json:"text"`
	Command string `json:"command,omitempty"`
	Link    string `json:"link,omitempty"`
}

// InstallGuide tells the user how to install a container engine on this
// system.
type InstallGuide struct {
	System  string          `json:"system"`
	Options []InstallOption `json:"options"`
}

// InstallOption is one way to get an engine.
type InstallOption struct {
	Title string `json:"title"`
	Note  string `json:"note,omitempty"`
	Steps []Step `json:"steps"`
}

// LinuxRelease is the distribution as /etc/os-release names it.
type LinuxRelease struct {
	ID     string
	IDLike string
	Name   string
}

func readLinuxRelease() LinuxRelease {
	file, err := os.Open("/etc/os-release")
	if err != nil {
		return LinuxRelease{}
	}
	defer file.Close()
	release := LinuxRelease{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		name, value, ok := strings.Cut(scanner.Text(), "=")
		if !ok {
			continue
		}
		value = strings.Trim(value, `"'`)
		switch name {
		case "ID":
			release.ID = value
		case "ID_LIKE":
			release.IDLike = value
		case "PRETTY_NAME":
			release.Name = value
		}
	}
	return release
}

// IsWSL says whether this Linux runs inside Windows (WSL).
func IsWSL() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	if os.Getenv("WSL_DISTRO_NAME") != "" {
		return true
	}
	data, err := os.ReadFile("/proc/sys/kernel/osrelease")
	return err == nil && strings.Contains(strings.ToLower(string(data)), "microsoft")
}

// OnCluster guesses whether this is a shared HPC system, where users cannot
// install Docker.
func OnCluster() bool {
	if os.Getenv("SLURM_JOB_ID") != "" || os.Getenv("SLURM_CLUSTER_NAME") != "" || os.Getenv("PBS_JOBID") != "" {
		return true
	}
	for _, name := range []string{"sbatch", "qsub", "bsub"} {
		if _, err := exec.LookPath(name); err == nil {
			return true
		}
	}
	return os.Getenv("MODULESHOME") != "" || os.Getenv("LMOD_CMD") != ""
}

// Install guides for this machine.
func Guide() InstallGuide {
	return GuideFor(runtime.GOOS, readLinuxRelease(), IsWSL(), OnCluster())
}

var (
	dockerDesktop = InstallOption{
		Title: "Docker Desktop",
		Note:  "The easiest choice on a personal computer. Free for personal use, education and small companies.",
	}
	clusterOption = InstallOption{
		Title: "On a shared cluster: Apptainer",
		Note:  "Clusters do not allow Docker; most provide Apptainer (formerly Singularity).",
		Steps: []Step{
			{Text: "See whether it is available as a module:", Command: "module avail apptainer\nmodule load apptainer"},
			{Text: "If it is not, ask your cluster's support team to install Apptainer.", Link: "https://apptainer.org/docs/admin/main/installation.html"},
		},
	}
)

// GuideFor builds the install guide for a system; split out for tests.
func GuideFor(goos string, release LinuxRelease, wsl, cluster bool) InstallGuide {
	switch goos {
	case "darwin":
		desktop := dockerDesktop
		desktop.Steps = []Step{
			{Text: "Download Docker Desktop for Mac (choose Apple silicon or Intel), open the .dmg and drag Docker to Applications.", Link: "https://docs.docker.com/desktop/setup/install/mac-install/"},
			{Text: "Or install it with Homebrew:", Command: "brew install --cask docker"},
			{Text: "Open Docker from Applications and wait until the whale icon in the menu bar stops animating."},
		}
		return InstallGuide{System: "macOS", Options: []InstallOption{
			desktop,
			{Title: "Podman", Note: "Free and open source.", Steps: []Step{
				{Text: "Install Podman Desktop, or the command-line tool with Homebrew:", Command: "brew install podman", Link: "https://podman-desktop.io/downloads"},
				{Text: "Create and start its virtual machine once:", Command: "podman machine init\npodman machine start"},
			}},
		}}
	case "windows":
		desktop := dockerDesktop
		desktop.Steps = []Step{
			{Text: "Docker needs WSL 2. In PowerShell as administrator, run this and restart the computer:", Command: "wsl --install"},
			{Text: "Install Docker Desktop from its website, or with winget:", Command: "winget install -e --id Docker.DockerDesktop", Link: "https://docs.docker.com/desktop/setup/install/windows-install/"},
			{Text: "Start Docker Desktop and wait until it says \"Engine running\"."},
		}
		return InstallGuide{System: "Windows", Options: []InstallOption{
			desktop,
			{Title: "Podman Desktop", Note: "Free and open source.", Steps: []Step{
				{Text: "Install it from its website, or with winget, then follow its setup to create a Podman machine:", Command: "winget install -e --id RedHat.Podman-Desktop", Link: "https://podman-desktop.io/downloads"},
			}},
		}}
	}

	// Linux.
	system := "Linux"
	if release.Name != "" {
		system = release.Name
	}
	if wsl {
		desktop := dockerDesktop
		desktop.Steps = []Step{
			{Text: "Install Docker Desktop on Windows (not inside Linux).", Link: "https://docs.docker.com/desktop/setup/install/windows-install/"},
			{Text: "In Docker Desktop, open Settings → Resources → WSL integration and turn it on for this distribution."},
			{Text: "Open a new terminal here and check that it works:", Command: "docker info"},
		}
		return InstallGuide{System: system + " on Windows (WSL)", Options: []InstallOption{desktop, linuxDocker(release)}}
	}
	options := []InstallOption{linuxDocker(release), linuxPodman(release)}
	if cluster {
		options = append([]InstallOption{clusterOption}, options...)
	} else {
		options = append(options, clusterOption)
	}
	return InstallGuide{System: system, Options: options}
}

func family(release LinuxRelease) string {
	ids := strings.Fields(release.ID + " " + release.IDLike)
	for _, id := range ids {
		switch id {
		case "debian", "ubuntu":
			return "debian"
		case "fedora", "rhel", "centos", "rocky", "almalinux":
			return "fedora"
		case "arch":
			return "arch"
		case "suse", "opensuse", "opensuse-leap", "opensuse-tumbleweed", "sles":
			return "suse"
		}
	}
	return ""
}

func linuxDocker(release LinuxRelease) InstallOption {
	option := InstallOption{Title: "Docker Engine", Note: "Needs administrator (sudo) rights."}
	switch family(release) {
	case "arch":
		option.Steps = []Step{{Text: "Install and start Docker:", Command: "sudo pacman -S docker\nsudo systemctl enable --now docker"}}
	case "suse":
		option.Steps = []Step{{Text: "Install and start Docker:", Command: "sudo zypper install docker\nsudo systemctl enable --now docker"}}
	default:
		link := "https://docs.docker.com/engine/install/"
		switch release.ID {
		case "ubuntu", "debian", "fedora", "rhel", "centos":
			link += release.ID + "/"
		}
		option.Steps = []Step{
			{Text: "Install Docker with its official script (or follow the guide for your distribution):", Command: "curl -fsSL https://get.docker.com | sudo sh", Link: link},
		}
	}
	option.Steps = append(option.Steps, Step{
		Text:    "Let your user run containers, then log out and back in:",
		Command: "sudo usermod -aG docker $USER",
	})
	return option
}

func linuxPodman(release LinuxRelease) InstallOption {
	option := InstallOption{Title: "Podman", Note: "Runs without a background service; often already installed on Fedora and RHEL."}
	command := ""
	switch family(release) {
	case "debian":
		command = "sudo apt-get install -y podman"
	case "fedora":
		command = "sudo dnf install -y podman"
	case "arch":
		command = "sudo pacman -S podman"
	case "suse":
		command = "sudo zypper install podman"
	}
	if command == "" {
		option.Steps = []Step{{Text: "Install podman with your distribution's package manager.", Link: "https://podman.io/docs/installation"}}
	} else {
		option.Steps = []Step{{Text: "Install it:", Command: command, Link: "https://podman.io/docs/installation"}}
	}
	return option
}

func dockerStartFix() []Step {
	switch runtime.GOOS {
	case "darwin", "windows":
		return []Step{{Text: "Start Docker Desktop and wait until it says the engine is running."}}
	}
	if IsWSL() {
		return []Step{
			{Text: "Start Docker Desktop on Windows, and make sure WSL integration is on for this distribution (Settings → Resources → WSL integration)."},
			{Text: "If Docker is installed inside this Linux instead, start it:", Command: "sudo service docker start"},
		}
	}
	return []Step{{Text: "Start the Docker service:", Command: "sudo systemctl enable --now docker"}}
}

func dockerPermissionFix() []Step {
	return []Step{
		{Text: "Add your user to the docker group, then log out and back in:", Command: "sudo usermod -aG docker $USER"},
	}
}

// CheckImage says whether the engine has the sandbox image; nil means it
// does.
func CheckImage(ctx context.Context, engine EngineStatus, image string) error {
	image, err := ResolveImage(image, engine.Name)
	if err != nil {
		return err
	}
	switch engine.Name {
	case EngineDocker:
		docker, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
		if err != nil {
			return err
		}
		defer docker.Close()
		if _, err := docker.ImageInspect(ctx, image); err != nil {
			return missingImage{EngineDocker, image}
		}
	case EnginePodman:
		if _, err := commandOutput(ctx, engine.Executable, "image", "exists", image); err != nil {
			return missingImage{EnginePodman, image}
		}
	}
	return nil
}

// ImageHelp says how to get the sandbox image for an engine.
func ImageHelp(engine string) []Step {
	switch engine {
	case EngineApptainer:
		return []Step{
			{Text: "On a computer with Docker, build the image for the cluster from the code flower repository:", Command: "make sandbox-build-linux"},
			{Text: "Copy build/codeflower-sandbox-amd64.tar to the cluster and convert it:", Command: "apptainer build codeflower-sandbox.sif docker-archive://codeflower-sandbox-amd64.tar"},
			{Text: "Then pass --image codeflower-sandbox.sif, or set CODEFLOWER_IMAGE to its path."},
		}
	case EnginePodman:
		return []Step{{Text: "Build it from the code flower repository:", Command: "podman build -f sandbox/Dockerfile -t " + DefaultImage + " ."}}
	}
	return []Step{{Text: "Build it from the code flower repository:", Command: "make sandbox-build"}}
}
