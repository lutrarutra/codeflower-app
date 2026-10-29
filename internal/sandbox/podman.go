package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
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
// needs no daemon. Like Docker, the launcher owns the container.
type PodmanRun struct {
	Name  string
	Image string
	State string
	Binds []Bind
	Env   map[string]string
	// Port is published on 127.0.0.1 when set.
	Port int
	// NetDirectory, when set, isolates the sandbox: it gets no network, and
	// this folder of sockets shared with the launcher instead of ports.
	NetDirectory string
	Stdout       io.Writer
	Stderr       io.Writer
}

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
	names := make([]string, 0, len(run.Env))
	for name := range run.Env {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		args = append(args, "--env", name)
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
	args, err := PodmanArgs(executable, run)
	if err != nil {
		return 1, err
	}
	command := exec.Command(args[0], args[1:]...)
	command.Env = os.Environ()
	for name, value := range run.Env {
		command.Env = append(command.Env, name+"="+value)
	}
	command.Stdout, command.Stderr = run.Stdout, run.Stderr
	return runUntilDone(ctx, command, func() {
		go func() {
			stop := exec.Command(executable, "stop", "--time", strconv.Itoa(stopTimeoutSeconds), run.Name)
			_ = stop.Run()
		}()
	})
}
