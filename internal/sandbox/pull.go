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
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
)

// The published sandbox image. The launcher follows its newest release;
// --image-tag picks another release or a commit's build.
const (
	ImageRepository = "ghcr.io/lutrarutra/codeflower-sandbox"
	DefaultImageTag = "latest"
)

var (
	imageTag   = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	versionTag = regexp.MustCompile(`^v[0-9]+(\.[0-9]+){0,2}$`)
	commitHash = regexp.MustCompile(`^(sha-)?[0-9a-f]{7,40}$`)
)

// PublishedImage names a build of the published image by its tag: "latest"
// (or empty), a release (0.2.1, v0.2.1, or 0.2 for its newest patch), or a
// commit (sha-abc1234, or the commit's hash).
func PublishedImage(tag string) (string, error) {
	tag = strings.TrimSpace(tag)
	switch {
	case tag == "":
		tag = DefaultImageTag
	case versionTag.MatchString(tag):
		tag = tag[1:]
	case commitHash.MatchString(tag) && (strings.HasPrefix(tag, "sha-") || strings.ContainsAny(tag, "abcdef")):
		// The release workflow tags each build with its short commit hash.
		tag = "sha-" + strings.TrimPrefix(tag, "sha-")[:7]
	}
	if !imageTag.MatchString(tag) {
		return "", fmt.Errorf("%q is not an image tag: use latest, a release such as 0.2.1, or a commit such as sha-abc1234", tag)
	}
	return ImageRepository + ":" + tag, nil
}

// publishedTag returns the tag of a published image name; an empty name is
// the default one. It is false for any other image.
func publishedTag(image string) (string, bool) {
	if image == "" {
		return DefaultImageTag, true
	}
	tag, ok := strings.CutPrefix(image, ImageRepository+":")
	return tag, ok && imageTag.MatchString(tag)
}

// IsPublished says whether image names a build of the published image, which
// the launcher downloads itself; another image is the user's to provide.
func IsPublished(image string) bool {
	_, ok := publishedTag(image)
	return ok
}

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
	return publishedSIF(DefaultImageTag)
}

// publishedSIF is where a build of the published image is kept for Apptainer.
func publishedSIF(tag string) string {
	return filepath.Join(ImageDirectory(), "codeflower-sandbox_"+tag+".sif")
}

// EnsureImage pulls a build of the published image (empty: the default)
// unless the engine already has it. Callers use it only for those: another
// image the user named is theirs to provide.
func EnsureImage(ctx context.Context, engine EngineStatus, image string, out io.Writer) error {
	err := CheckImage(ctx, engine, image)
	if !errors.Is(err, ErrNoImage) {
		return err
	}
	fmt.Fprintf(out, "Downloading the sandbox image %s (a few GB, once)…\n", PublishedName(image))
	return pullImage(ctx, engine, image, false, out)
}

// PullImage downloads a build of the published image (empty: the default),
// replacing an older copy of the same tag (latest, or a release's newest
// patch). Progress goes to out, a line every few seconds.
func PullImage(ctx context.Context, engine EngineStatus, image string, out io.Writer) error {
	return pullImage(ctx, engine, image, true, out)
}

// PublishedName is the full name of a published image; empty is the default.
func PublishedName(image string) string {
	if image == "" {
		return DefaultImage
	}
	return image
}

// pullImage downloads a build of the published image; without refresh, an
// Apptainer image another launcher downloaded meanwhile is kept.
func pullImage(ctx context.Context, engine EngineStatus, image string, refresh bool, out io.Writer) error {
	tag, ok := publishedTag(image)
	if !ok {
		return fmt.Errorf("only the published image %s can be downloaded, not %s", ImageRepository, image)
	}
	name := ImageRepository + ":" + tag
	switch engine.Name {
	case EngineDocker:
		return pullDocker(ctx, name, out)
	case EnginePodman:
		command := exec.CommandContext(ctx, engine.Executable, "pull", name)
		command.Stdout, command.Stderr = out, out
		if err := command.Run(); err != nil {
			return fmt.Errorf("podman could not download %s: %w", name, err)
		}
		return nil
	case EngineApptainer:
		return pullApptainer(ctx, engine.Executable, name, publishedSIF(tag), refresh, out)
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
