package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// ReproduceEntrypoint reruns a pipeline archive with no agent and no bridge
// (sandbox/reproduce.sh); its one argument is the phase.
const ReproduceEntrypoint = "/opt/codeflower/reproduce.sh"

// Where reproduce.sh finds what the app mounts.
const (
	ReproducePipeline = "/repro/pipeline"
	ReproduceData     = "/repro/data"
	ReproduceSnapshot = "/repro/snapshot.tar.gz"
	ReproduceWork     = "/repro/work"
	ReproduceOut      = "/repro/out"
)

// The phases of a reproduction, each a container of its own on one
// throwaway project.
const (
	PhaseCheck   = "check"
	PhasePrepare = "prepare"
	PhaseRun     = "run"
)

// RestoreHosts are what restoring an environment from its lockfiles needs:
// the package indexes, and GitHub for R packages installed from it.
var RestoreHosts = PresetHosts("pypi", "conda", "cran", "bioconductor", "github")

// PresetHosts are the hosts of the named allowlist presets.
func PresetHosts(names ...string) []string {
	var hosts []string
	for _, name := range names {
		for _, preset := range AllowlistPresets {
			if preset.Name == name {
				hosts = append(hosts, preset.Hosts...)
			}
		}
	}
	return hosts
}

// Reproduction reruns an unpacked pipeline archive in a new sandbox. It never
// starts an agent: no sign-in, keys, agent homes or provider hosts.
type Reproduction struct {
	Engine EngineStatus
	Image  string
	// Pipeline is the unpacked archive, mounted read-only.
	Pipeline string
	// Data holds the downloaded inputs at their data/... paths; may be empty.
	Data string
	// Snapshot is the environment snapshot to restore instead of the
	// lockfiles; may be empty.
	Snapshot string
	// Output receives the results, the logs and the reports.
	Output    string
	Cores     int
	Resources Resources
	// Announce, when set, is told each phase before it starts.
	Announce func(phase string)
	// Phases to run, in order; empty: check, prepare and run.
	Phases []string
	// State is the throwaway project folder, which the caller removes; empty:
	// a temporary one, removed afterwards.
	State string
	// Scratch holds the engine's scratch space (Apptainer); empty: the
	// temporary folder.
	Scratch string
	// ID names the containers, ReproduceContainerPrefix + ID + "-" + phase;
	// empty: a random one.
	ID     string
	Stdout io.Writer
	Stderr io.Writer
	Log    func(string)
}

// ReproductionResult says how far a reproduction got.
type ReproductionResult struct {
	// Phase is the last phase that ran; Code its exit status.
	Phase string
	Code  int
	// Warnings, such as a restore whose network was not restricted.
	Warnings []string
}

// Run runs the phases in turn and stops at the first that fails.
func (r Reproduction) Run(ctx context.Context) (ReproductionResult, error) {
	result := ReproductionResult{}
	if r.Stdout == nil {
		r.Stdout = os.Stdout
	}
	if r.Stderr == nil {
		r.Stderr = os.Stderr
	}
	if r.Log == nil {
		r.Log = func(string) {}
	}
	if r.ID == "" {
		r.ID = randomID()
	}
	state := r.State
	if state == "" {
		folder, err := NewReproduceFolder()
		if err != nil {
			return result, err
		}
		defer folder.Close()
		if state, err = folder.Folder("project"); err != nil {
			return result, err
		}
		if r.Scratch == "" {
			r.Scratch = folder.Path
		}
	}
	for _, name := range []string{"env", "cache", "home", "run", "workspace"} {
		if err := os.MkdirAll(filepath.Join(state, name), 0o700); err != nil {
			return result, err
		}
	}
	work := filepath.Join(state, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		return result, err
	}
	if err := os.MkdirAll(r.Output, 0o755); err != nil {
		return result, fmt.Errorf("could not create the output folder: %w", err)
	}
	binds, err := r.binds(work)
	if err != nil {
		return result, err
	}
	phases := r.Phases
	if len(phases) == 0 {
		phases = []string{PhaseCheck, PhasePrepare, PhaseRun}
	}
	for _, phase := range phases {
		result.Phase = phase
		if r.Announce != nil {
			r.Announce(phase)
		}
		code, warnings, err := r.runPhase(ctx, phase, state, binds)
		result.Code = code
		result.Warnings = append(result.Warnings, warnings...)
		if err != nil || code != 0 {
			return result, err
		}
	}
	return result, nil
}

func (r Reproduction) binds(work string) ([]Bind, error) {
	specs := []struct {
		source, target string
		readOnly       bool
	}{
		{r.Pipeline, ReproducePipeline, true},
		{r.Data, ReproduceData, true},
		{r.Snapshot, ReproduceSnapshot, true},
		{work, ReproduceWork, false},
		{r.Output, ReproduceOut, false},
	}
	var binds []Bind
	for _, spec := range specs {
		if spec.source == "" {
			continue
		}
		bind, err := NewBind(spec.source, spec.target, spec.readOnly)
		if err != nil {
			return nil, err
		}
		binds = append(binds, bind)
	}
	return binds, nil
}

// runPhase runs one phase. Check and run get no network; prepare reaches only
// the package indexes where the engine can restrict it, and the network
// otherwise (macOS and Windows), which it reports as a warning. A snapshot
// needs no network at all.
func (r Reproduction) runPhase(ctx context.Context, phase, state string, binds []Bind) (int, []string, error) {
	values := ContainerEnvironment()
	values["CODEFLOWER_ENGINE"] = r.Engine.Name
	if r.Cores > 0 {
		values["CODEFLOWER_REPRO_CORES"] = strconv.Itoa(r.Cores)
	}
	r.Resources.environment(values)
	spec := containerSpec{name: ReproduceContainerPrefix + r.ID + "-" + phase, scratch: r.Scratch, image: r.Image, state: state, binds: binds, env: values,
		command: []string{ReproduceEntrypoint, phase}, resources: r.Resources, stdout: r.Stdout, stderr: r.Stderr}
	var warnings []string
	switch {
	case phase != PhasePrepare || r.Snapshot != "":
		spec.noNetwork = true
		values["CODEFLOWER_NETWORK"] = "isolated"
	default:
		problem := IsolationProblem(r.Engine.Name)
		if problem == nil && r.Engine.Name == EngineApptainer {
			problem = apptainerAllowsIsolation(ctx, r.Engine.Executable, r.Image)
		}
		if problem != nil {
			warnings = append(warnings, "The environment was restored with an unrestricted network ("+problem.Error()+").")
			break
		}
		directory, err := newNetDirectory(randomID())
		if err != nil {
			return 1, warnings, err
		}
		defer os.RemoveAll(directory)
		list := newAllowlist("", RestoreHosts, r.Log)
		list.purpose = "restore the environment"
		gateway, err := startGateway(directory, "", list, r.Log)
		if err != nil {
			return 1, warnings, err
		}
		defer gateway.Close()
		isolatedEnvironment(values)
		delete(values, "CODEFLOWER_LISTEN_SOCKET")
		delete(values, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC")
		spec.netDirectory = directory
	}
	code, err := runContainer(ctx, r.Engine, spec)
	return code, warnings, err
}

// containerSpec is a container that runs a command of the image instead of
// the bridge.
type containerSpec struct {
	name, image, state string
	// scratch is where Apptainer's scratch folder goes; empty: the temporary folder.
	scratch      string
	binds        []Bind
	env          map[string]string
	command      []string
	netDirectory string
	noNetwork    bool
	resources    Resources
	// engineArgs are the user's engine flags (jobs); reproductions have none.
	engineArgs     EngineArgs
	stdout, stderr io.Writer
}

func runContainer(ctx context.Context, engine EngineStatus, spec containerSpec) (int, error) {
	switch engine.Name {
	case EngineDocker:
		return RunDocker(ctx, DockerRun{
			Name: spec.name, Image: spec.image, State: spec.state, Binds: spec.binds, Env: spec.env,
			NetDirectory: spec.netDirectory, NoNetwork: spec.noNetwork, Entrypoint: spec.command,
			CPUs: spec.resources.CPUs, Memory: spec.resources.Memory, Stdout: spec.stdout, Stderr: spec.stderr,
			EngineArgs: spec.engineArgs,
		})
	case EnginePodman:
		return RunPodman(ctx, engine.Executable, PodmanRun{
			Name: spec.name, Image: spec.image, State: spec.state, Binds: spec.binds, Env: spec.env,
			NetDirectory: spec.netDirectory, NoNetwork: spec.noNetwork, Entrypoint: spec.command,
			CPUs: spec.resources.CPUs, Memory: spec.resources.Memory, Stdout: spec.stdout, Stderr: spec.stderr,
			EngineArgs: spec.engineArgs,
		})
	case EngineApptainer:
		scratch, err := os.MkdirTemp(spec.scratch, scratchPrefix)
		if err != nil {
			return 1, fmt.Errorf("could not create the sandbox's temporary folder: %w", err)
		}
		defer removeTree(scratch)
		args, err := ApptainerCommandWith(ApptainerOptions{Executable: engine.Executable, Image: spec.image,
			State: spec.state, Scratch: scratch, Binds: spec.binds, NetDirectory: spec.netDirectory,
			NoNetwork: spec.noNetwork, Command: spec.command, GPUs: spec.engineArgs.GPUs != ""})
		if err != nil {
			return 1, err
		}
		spec.engineArgs.apptainerGPUs(spec.env)
		return RunApptainer(ctx, nil, false, args, spec.env, nil, spec.stdout, spec.stderr)
	}
	return 1, fmt.Errorf("unknown engine %q", engine.Name)
}

func privateTemp(prefix string) (string, error) {
	directory, err := os.MkdirTemp("", prefix)
	if err != nil {
		return "", err
	}
	// Docker Desktop shares /private/var/folders, not the /var/folders link.
	if resolved, err := filepath.EvalSymlinks(directory); err == nil {
		directory = resolved
	}
	return directory, os.Chmod(directory, 0o700)
}

func randomID() string {
	buffer := make([]byte, 8)
	_, _ = rand.Read(buffer)
	return hex.EncodeToString(buffer)
}

// ImageIdentity is what pins an image: its repository digest when it came
// from a registry, and its local ID. Values the app gives every sandbox
// (CODEFLOWER_IMAGE_REF, _DIGEST, _ID), so a pipeline records them.
type ImageIdentity struct {
	Ref    string `json:"ref,omitempty"`
	Digest string `json:"digest,omitempty"`
	ID     string `json:"id,omitempty"`
}

// Environment adds the identity to a sandbox's variables.
func (i ImageIdentity) Environment(values map[string]string) {
	for name, value := range map[string]string{"CODEFLOWER_IMAGE_REF": i.Ref, "CODEFLOWER_IMAGE_DIGEST": i.Digest,
		"CODEFLOWER_IMAGE_ID": i.ID} {
		if value != "" {
			values[name] = value
		}
	}
}

// IdentifyImage reads an image's digest and ID. For Apptainer the ID is the
// SIF file's name, and the digest comes from its version label when the image
// is a published one.
func IdentifyImage(ctx context.Context, engine EngineStatus, image string) (ImageIdentity, error) {
	identity := ImageIdentity{Ref: image}
	switch engine.Name {
	case EngineDocker:
		docker, err := newDockerClient()
		if err != nil {
			return identity, err
		}
		defer docker.Close()
		inspected, err := docker.ImageInspect(ctx, image)
		if err != nil {
			return identity, err
		}
		identity.ID = inspected.ID
		identity.Digest = repositoryDigest(inspected.RepoDigests)
	case EnginePodman:
		output, err := exec.CommandContext(ctx, engine.Executable, "image", "inspect", "--format",
			"{{.Id}} {{range .RepoDigests}}{{.}} {{end}}", image).Output()
		if err != nil {
			return identity, err
		}
		fields := strings.Fields(string(output))
		if len(fields) > 0 {
			identity.ID = "sha256:" + strings.TrimPrefix(fields[0], "sha256:")
			identity.Digest = repositoryDigest(fields[1:])
		}
	case EngineApptainer:
		identity.ID = filepath.Base(image)
	default:
		return identity, fmt.Errorf("unknown engine %q", engine.Name)
	}
	return identity, nil
}

// repositoryDigest is the digest of the published repository's entry, else
// the first one of a registry. A local build's entries (no registry in the
// name) cannot be pulled, so they do not count.
func repositoryDigest(digests []string) string {
	chosen := ""
	for _, entry := range digests {
		repository, _, ok := strings.Cut(entry, "@")
		host, _, hasHost := strings.Cut(repository, "/")
		if !ok || !hasHost || !strings.ContainsAny(host, ".:") {
			continue
		}
		if strings.HasPrefix(entry, ImageRepository+"@") {
			return entry
		}
		if chosen == "" {
			chosen = entry
		}
	}
	return chosen
}

// ArchiveImage finds or downloads the image a pipeline archive was made with,
// and returns what to run. A digest (repository@sha256:…) is pulled when the
// engine does not have it; an ID alone names a local build, which only this
// machine's engine can have.
func ArchiveImage(ctx context.Context, engine EngineStatus, identity ImageIdentity, out io.Writer) (string, error) {
	switch {
	case identity.ID != "" && engine.Name != EngineApptainer && CheckImage(ctx, engine, identity.ID) == nil:
		// The very image, already here: a local build, or a published one pulled before.
		return identity.ID, nil
	case identity.Digest != "" && engine.Name == EngineApptainer:
		return apptainerByDigest(ctx, engine.Executable, identity.Digest, out)
	case identity.Digest != "":
		if CheckImage(ctx, engine, identity.Digest) == nil {
			return identity.Digest, nil
		}
		fmt.Fprintf(out, "Downloading the archive's sandbox image %s (a few GB, once)…\n", identity.Digest)
		if engine.Name == EngineDocker {
			if err := pullDocker(ctx, identity.Digest, out); err != nil {
				return "", err
			}
			return identity.Digest, nil
		}
		command := exec.CommandContext(ctx, engine.Executable, "pull", identity.Digest)
		command.Stdout, command.Stderr = out, out
		if err := command.Run(); err != nil {
			return "", fmt.Errorf("could not download %s: %w", identity.Digest, err)
		}
		return identity.Digest, nil
	case identity.ID != "" && engine.Name != EngineApptainer:
		if CheckImage(ctx, engine, identity.ID) == nil {
			return identity.ID, nil
		}
		return "", fmt.Errorf("the archive was made with a local build of the image (%s, ID %s), which %s does not have: "+
			"pass that image with --image", identity.Ref, identity.ID, EngineLabel(engine.Name))
	}
	return "", fmt.Errorf("the archive does not say which image it was made with: pass one with --image")
}

// apptainerByDigest converts the image of a digest to a SIF file once, named
// after the digest in the image folder.
func apptainerByDigest(ctx context.Context, executable, digest string, out io.Writer) (string, error) {
	_, sum, ok := strings.Cut(digest, "@sha256:")
	if !ok || len(sum) < 12 {
		return "", fmt.Errorf("not an image digest: %s", digest)
	}
	directory := ImageDirectory()
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return "", err
	}
	file := filepath.Join(directory, "codeflower-sandbox_sha256-"+sum[:12]+".sif")
	if _, err := os.Stat(file); err == nil {
		return file, nil
	}
	unlock, err := lockFile(file + ".lock")
	if err != nil {
		return "", err
	}
	defer unlock()
	if _, err := os.Stat(file); err == nil {
		return file, nil
	}
	fmt.Fprintf(out, "Downloading the archive's sandbox image %s (a few GB, once)…\n", digest)
	temporary := file + "." + strconv.Itoa(os.Getpid()) + ".tmp"
	defer os.Remove(temporary)
	// --disable-cache: the .sif file is kept, so a second copy of its layers in
	// ~/.apptainer/cache would only take space.
	command := exec.CommandContext(ctx, executable, "pull", "--force", "--disable-cache", temporary, "docker://"+digest)
	command.Stdout, command.Stderr = out, out
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("could not download %s: %w", digest, err)
	}
	return file, os.Rename(temporary, file)
}
