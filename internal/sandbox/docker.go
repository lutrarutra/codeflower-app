package sandbox

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strconv"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"
)

// DockerRun describes a sandbox container for workstations without
// Apptainer. The launcher owns the container: it has a fixed name and is
// stopped and removed when the launcher exits.
type DockerRun struct {
	Name  string
	Image string
	State string
	Binds []Bind
	Env   map[string]string
	// Port is published on 127.0.0.1 when set.
	Port int
	// InspectPort publishes the bridge's Node inspector on 127.0.0.1 for
	// development; the bridge listens on BridgeInspectPort inside.
	InspectPort int
	// The container's output; standard output and error when nil.
	Stdout io.Writer
	Stderr io.Writer
}

// BridgeInspectPort is the bridge's Node inspector port inside the container.
const BridgeInspectPort = 9229

const stopTimeoutSeconds = 15

// containerUser runs the sandbox as the user, so files it writes to binds are
// theirs. Windows has no uid; Docker Desktop maps file ownership itself, and
// the agents refuse some modes as root, so an ordinary uid is used there.
func containerUser() string {
	if runtime.GOOS == "windows" || os.Getuid() < 0 {
		return "1000:1000"
	}
	return strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())
}

// DockerConfig translates a run into Docker API objects.
func DockerConfig(run DockerRun) (*container.Config, *container.HostConfig, error) {
	if err := checkTargets(run.Binds); err != nil {
		return nil, nil, err
	}
	env := make([]string, 0, len(run.Env))
	for name, value := range run.Env {
		env = append(env, name+"="+value)
	}
	sort.Strings(env)

	init := true
	config := &container.Config{
		Image: run.Image,
		Env:   env,
		User:  containerUser(),
	}
	// Mounts rather than "SOURCE:TARGET" strings, which Windows paths break.
	host := &container.HostConfig{
		Mounts:     []mount.Mount{{Type: mount.TypeBind, Source: run.State, Target: ContainerState}},
		AutoRemove: true,
		Init:       &init,
	}
	for _, bind := range run.Binds {
		host.Mounts = append(host.Mounts, mount.Mount{
			Type: mount.TypeBind, Source: bind.Source, Target: bind.Target, ReadOnly: bind.ReadOnly,
		})
	}
	config.AttachStdout = true
	config.AttachStderr = true
	config.ExposedPorts = nat.PortSet{}
	host.PortBindings = nat.PortMap{}
	publish := func(inside, outside int) {
		port := nat.Port(strconv.Itoa(inside) + "/tcp")
		config.ExposedPorts[port] = struct{}{}
		host.PortBindings[port] = []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: strconv.Itoa(outside)}}
	}
	if run.Port > 0 {
		publish(run.Port, run.Port)
	}
	if run.InspectPort > 0 {
		publish(BridgeInspectPort, run.InspectPort)
	}
	return config, host, nil
}

// RunDocker creates, attaches to, and starts the container, and returns its
// exit code. Cancelling ctx stops it gracefully; it is removed afterwards.
func RunDocker(ctx context.Context, run DockerRun) (int, error) {
	if run.Stdout == nil {
		run.Stdout = os.Stdout
	}
	if run.Stderr == nil {
		run.Stderr = os.Stderr
	}
	stopRequested := ctx.Done()
	// The API calls outlive ctx: a cancelled ctx means "stop", not "abandon".
	ctx = context.WithoutCancel(ctx)
	config, host, err := DockerConfig(run)
	if err != nil {
		return 1, err
	}
	docker, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return 1, fmt.Errorf("could not reach Docker: %w", err)
	}
	defer docker.Close()
	if _, err := docker.ImageInspect(ctx, run.Image); err != nil {
		return 1, missingImage{EngineDocker, run.Image}
	}

	created, err := docker.ContainerCreate(ctx, config, host, nil, nil, run.Name)
	if err != nil {
		return 1, fmt.Errorf("could not create the container: %w", err)
	}
	id := created.ID
	// Removes the container if anything below fails before it runs.
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = docker.ContainerRemove(cleanup, id, container.RemoveOptions{Force: true})
	}()

	attached, err := docker.ContainerAttach(ctx, id, container.AttachOptions{
		Stream: true,
		Stdout: true,
		Stderr: true,
	})
	if err != nil {
		return 1, fmt.Errorf("could not attach to the container: %w", err)
	}
	defer attached.Close()

	// Wait before starting, so the exit is not missed when the container is
	// removed right after it stops.
	waitStatus, waitError := docker.ContainerWait(ctx, id, container.WaitConditionNextExit)

	go func() { _, _ = stdcopy.StdCopy(run.Stdout, run.Stderr, attached.Reader) }()

	if err := docker.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		return 1, fmt.Errorf("could not start the container: %w", err)
	}

	stopping := false
	stop := func() {
		if stopping {
			return
		}
		stopping = true
		go func() {
			stopContext, cancel := context.WithTimeout(context.Background(), (stopTimeoutSeconds+5)*time.Second)
			defer cancel()
			timeout := stopTimeoutSeconds
			_ = docker.ContainerStop(stopContext, id, container.StopOptions{Timeout: &timeout})
		}()
	}

	for {
		select {
		case <-stopRequested:
			stopRequested = nil
			stop()
		case status := <-waitStatus:
			if status.Error != nil {
				return int(status.StatusCode), fmt.Errorf("container failed: %s", status.Error.Message)
			}
			return int(status.StatusCode), nil
		case err := <-waitError:
			return 1, fmt.Errorf("lost the container: %w", err)
		}
	}
}
