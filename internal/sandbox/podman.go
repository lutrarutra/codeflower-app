package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ErrNoImage means the engine does not have the sandbox image.
var ErrNoImage = errors.New("the sandbox image is not available")

// missingImage names the engine and image; it matches ErrNoImage.
type missingImage struct{ engine, image string }

func (e missingImage) Error() string {
	return fmt.Sprintf("%s has no image %q", EngineLabel(e.engine), e.image)
}

func (e missingImage) Is(target error) bool { return target == ErrNoImage }

// PodmanRun describes a sandbox container run with the podman command, which
// needs no daemon. Like Docker, the app owns the container.
type PodmanRun struct {
	Name  string
	Image string
	State string
	Binds []Bind
	Env   map[string]string
	// Port is published on 127.0.0.1 when set.
	Port int
	// NetDirectory, when set, isolates the sandbox: it gets no network, and
	// this folder of sockets shared with the app instead of ports.
	NetDirectory string
	// CPUs and Memory (bytes) limit the container when set.
	CPUs   int
	Memory int64
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// TTY gives the container a terminal: the agent's own terminal UI runs on
	// the user's (`codeflower tui`), with podman relaying it.
	TTY bool
	// SeccompFile, when set, holds the container's seccomp profile instead of
	// Podman's default (UserNamespacesSeccomp).
	SeccompFile string
}

// podmanOwnEnv are variables the podman process reads itself (rootless
// Podman finds its storage and configuration under HOME). They are paths,
// not secrets, so they go by value and podman keeps the host's.
var podmanOwnEnv = map[string]bool{"HOME": true, "TMPDIR": true, "PATH": true}

// PodmanArgs builds the `podman run` arguments. The environment is passed by
// name only (--env NAME), so the token never appears in `ps`.
func PodmanArgs(executable string, run PodmanRun) ([]string, error) {
	if err := checkTargets(run.Binds); err != nil {
		return nil, err
	}
	args := []string{
		executable, "run", "--rm", "--init",
		"--name", run.Name,
		// SELinux would otherwise deny the binds; relabeling the user's data
		// (:z) would change it.
		"--security-opt", "label=disable",
		"--mount", "type=bind,source=" + run.State + ",target=" + ContainerState,
	}
	if run.SeccompFile != "" {
		args = append(args, "--security-opt", "seccomp="+run.SeccompFile)
	}
	if run.Stdin != nil {
		args = append(args, "-i")
	}
	if run.TTY {
		args = append(args, "-t")
	}
	// Rootless Podman maps the user to root inside; keep-id keeps their uid,
	// so the files the sandbox writes stay theirs.
	if os.Getuid() != 0 {
		args = append(args, "--userns=keep-id")
	}
	for _, bind := range run.Binds {
		spec := "type=bind,source=" + bind.Source + ",target=" + bind.Target
		if bind.ReadOnly {
			spec += ",readonly"
		}
		args = append(args, "--mount", spec)
	}
	if run.NetDirectory != "" {
		args = append(args, "--network", "none",
			"--mount", "type=bind,source="+run.NetDirectory+",target="+ContainerNet)
	} else if run.Port > 0 {
		port := strconv.Itoa(run.Port)
		args = append(args, "--publish", "127.0.0.1:"+port+":"+port)
	}
	if run.CPUs > 0 {
		args = append(args, "--cpus", strconv.Itoa(run.CPUs))
	}
	if run.Memory > 0 {
		args = append(args, "--memory", strconv.FormatInt(run.Memory, 10))
	}
	names := make([]string, 0, len(run.Env))
	for name := range run.Env {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if podmanOwnEnv[name] {
			args = append(args, "--env", name+"="+run.Env[name])
		} else {
			args = append(args, "--env", name)
		}
	}
	return append(args, run.Image), nil
}

// RunPodman runs the container and returns its exit code. Cancelling ctx
// stops it gracefully; --rm removes it.
func RunPodman(ctx context.Context, executable string, run PodmanRun) (int, error) {
	check, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if exec.CommandContext(check, executable, "image", "exists", run.Image).Run() != nil {
		return 1, missingImage{EnginePodman, run.Image}
	}
	run = withinControllers(run, podmanControllers(ctx, executable))
	args, err := PodmanArgs(executable, run)
	if err != nil {
		return 1, err
	}
	command := exec.Command(args[0], args[1:]...)
	command.Env = os.Environ()
	command.Stdin = run.Stdin
	for name, value := range run.Env {
		if !podmanOwnEnv[name] {
			command.Env = append(command.Env, name+"="+value)
		}
	}
	command.Stdout, command.Stderr = run.Stdout, run.Stderr
	return runUntilDone(ctx, command, func() {
		go func() {
			stop := exec.Command(executable, "stop", "--time", strconv.Itoa(stopTimeoutSeconds), run.Name)
			_ = stop.Run()
		}()
	})
}

// podmanControllers lists the cgroup controllers podman can use, or nil when
// it cannot tell. Rootless Podman has only those delegated to the user, which
// clusters often limit (or, with cgroups v1, has none).
func podmanControllers(ctx context.Context, executable string) []string {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, executable, "info", "--format",
		"{{range .Host.CgroupControllers}}{{.}} {{end}}").Output()
	if err != nil {
		return nil
	}
	return strings.Fields(string(output))
}

// withinControllers drops the limits whose controller podman does not have,
// which crun would refuse; the batch job's cgroup still holds the container.
// With controllers unknown (nil) it keeps them.
func withinControllers(run PodmanRun, controllers []string) PodmanRun {
	if controllers == nil {
		return run
	}
	var dropped []string
	if run.CPUs > 0 && !slices.Contains(controllers, "cpu") {
		run.CPUs = 0
		dropped = append(dropped, "CPUs")
	}
	if run.Memory > 0 && !slices.Contains(controllers, "memory") {
		run.Memory = 0
		dropped = append(dropped, "memory")
	}
	if len(dropped) > 0 && run.Stderr != nil {
		fmt.Fprintf(run.Stderr, "Podman cannot limit the %s here (the cgroup controller is not delegated to you); the container runs without that limit\n",
			strings.Join(dropped, " or "))
	}
	return run
}
