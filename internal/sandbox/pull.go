package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
)

// The published sandbox image. The launcher pins the major.minor release it
// was tested with; patch releases of the sandbox keep that tag.
const (
	ImageRepository = "ghcr.io/lutrarutra/codeflower-sandbox"
	ImageVersion    = "0.1"
)

// ImageDirectory holds the Apptainer image files the launcher downloads:
// $CODEFLOWER_IMAGE_DIR, or ~/codeflower/images next to the projects.
func ImageDirectory() string {
	if directory := os.Getenv("CODEFLOWER_IMAGE_DIR"); directory != "" {
		return expandHome(directory)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join("codeflower", "images")
	}
	return filepath.Join(home, "codeflower", "images")
}

// DefaultSIF is where the default image is kept for Apptainer.
func DefaultSIF() string {
	return filepath.Join(ImageDirectory(), "codeflower-sandbox_"+ImageVersion+".sif")
}

// EnsureImage pulls the default image unless the engine already has it.
// Callers use it only for the default image: one the user named is theirs to
// provide.
func EnsureImage(ctx context.Context, engine EngineStatus, out io.Writer) error {
	err := CheckImage(ctx, engine, "")
	if !errors.Is(err, ErrNoImage) {
		return err
	}
	fmt.Fprintf(out, "Downloading the sandbox image %s (a few GB, once)…\n", DefaultImage)
	return pullImage(ctx, engine, false, out)
}

// PullImage downloads the default image, replacing an older copy of the same
// tag (a newer patch release). Progress goes to out, a line every few seconds.
func PullImage(ctx context.Context, engine EngineStatus, out io.Writer) error {
	return pullImage(ctx, engine, true, out)
}

// pullImage downloads the default image; without refresh, an Apptainer image
// another launcher downloaded meanwhile is kept.
func pullImage(ctx context.Context, engine EngineStatus, refresh bool, out io.Writer) error {
	switch engine.Name {
	case EngineDocker:
		return pullDocker(ctx, DefaultImage, out)
	case EnginePodman:
		command := exec.CommandContext(ctx, engine.Executable, "pull", DefaultImage)
		command.Stdout, command.Stderr = out, out
		if err := command.Run(); err != nil {
			return fmt.Errorf("podman could not download %s: %w", DefaultImage, err)
		}
		return nil
	case EngineApptainer:
		return pullApptainer(ctx, engine.Executable, DefaultImage, DefaultSIF(), refresh, out)
	}
	return fmt.Errorf("unknown engine %q", engine.Name)
}

func pullDocker(ctx context.Context, name string, out io.Writer) error {
	docker, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("could not reach Docker: %w", err)
	}
	defer docker.Close()
	stream, err := docker.ImagePull(ctx, name, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("Docker could not download %s: %w", name, err)
	}
	defer stream.Close()
	return reportPull(stream, out, time.Now)
}

// pullMessage is one line of Docker's pull progress stream.
type pullMessage struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Progress *struct {
		Current int64 `json:"current"`
		Total   int64 `json:"total"`
	} `json:"progressDetail"`
	Error *struct {
		Message string `json:"message"`
	} `json:"errorDetail"`
}

// reportPull condenses Docker's per-layer progress stream into a total.
func reportPull(stream io.Reader, out io.Writer, now func() time.Time) error {
	type layer struct {
		current, total int64
		done           bool
	}
	layers := map[string]*layer{}
	var order []string
	var last time.Time
	report := func(force bool) {
		if !force && now().Sub(last) < 2*time.Second {
			return
		}
		last = now()
		var current, total int64
		done := 0
		for _, id := range order {
			item := layers[id]
			current += item.current
			total += item.total
			if item.done {
				done++
			}
		}
		if total == 0 {
			return
		}
		fmt.Fprintf(out, "  %d of %d MB, %d of %d layers done\n", current>>20, total>>20, done, len(order))
	}
	decoder := json.NewDecoder(stream)
	for {
		var message pullMessage
		if err := decoder.Decode(&message); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("reading Docker's progress: %w", err)
		}
		if message.Error != nil {
			return fmt.Errorf("Docker could not download the image: %s", message.Error.Message)
		}
		if message.ID == "" {
			// "Digest: …" and "Status: Downloaded newer image for …".
			if message.Status != "" {
				report(true)
				fmt.Fprintln(out, "  "+message.Status)
			}
			continue
		}
		if strings.HasPrefix(message.Status, "Pulling from") {
			continue // its id is the tag, not a layer
		}
		item, known := layers[message.ID]
		if !known {
			item = &layer{}
			layers[message.ID] = item
			order = append(order, message.ID)
		}
		switch message.Status {
		case "Downloading":
			if message.Progress != nil {
				item.current, item.total = message.Progress.Current, message.Progress.Total
			}
		case "Download complete", "Pull complete", "Already exists":
			item.done = true
			item.current = item.total
		}
		report(false)
	}
}

// pullApptainer converts the image into a SIF file. Several jobs may start at
// once on a cluster: they wait for one download, and nobody sees a partial file.
func pullApptainer(ctx context.Context, executable, name, target string, refresh bool, out io.Writer) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	unlock, err := lockFile(target + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	// Another launcher may have finished the download while this one waited.
	if _, err := os.Stat(target); err == nil && !refresh {
		return nil
	}
	temporary := filepath.Join(filepath.Dir(target), "."+filepath.Base(target)+"."+strconv.Itoa(os.Getpid())+".tmp")
	defer os.Remove(temporary)
	command := exec.CommandContext(ctx, executable, "pull", "--force", temporary, "docker://"+name)
	command.Stdout, command.Stderr = out, out
	if err := command.Run(); err != nil {
		return fmt.Errorf("%s could not download %s: %w", filepath.Base(executable), name, err)
	}
	if err := os.Rename(temporary, target); err != nil {
		return err
	}
	fmt.Fprintf(out, "  Saved %s\n", target)
	return nil
}
