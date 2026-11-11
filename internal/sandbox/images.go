package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
)

// PullProgress is how far a download of the image has come, in bytes and
// layers. Only Docker reports it; Podman and Apptainer print their own.
type PullProgress struct {
	Current int64 `json:"current"`
	Total   int64 `json:"total"`
	Layers  int   `json:"layers"`
	Done    int   `json:"done"`
}

// ProgressWriter is a download's output that also takes its progress in
// numbers, as a page shows it in a bar.
type ProgressWriter interface {
	io.Writer
	PullProgress(PullProgress)
}

// LocalImage is a build of the published image this computer has.
type LocalImage struct {
	// Ref names it for RemoveImage: name:tag (Docker and Podman), the image's
	// id when it has no tag left, or the file (Apptainer).
	Ref string `json:"ref"`
	// Tag is what it was downloaded as: a release, latest or a commit's build.
	Tag string `json:"tag"`
	// Version and Revision are the release and commit it was built from.
	Version  string    `json:"version,omitempty"`
	Revision string    `json:"revision,omitempty"`
	Size     int64     `json:"size"`
	Created  time.Time `json:"created,omitzero"`
	// Untagged: an older download a newer one of its tag replaced.
	Untagged bool `json:"untagged,omitempty"`
}

const (
	versionLabel  = "org.opencontainers.image.version"
	revisionLabel = "org.opencontainers.image.revision"
)

func shortRevision(revision string) string {
	if len(revision) > 7 {
		return revision[:7]
	}
	return revision
}

// ListImages lists the builds of the published image the engine has, newest
// first.
func ListImages(ctx context.Context, engine EngineStatus) ([]LocalImage, error) {
	var images []LocalImage
	var err error
	switch engine.Name {
	case EngineDocker:
		images, err = listDocker(ctx)
	case EnginePodman:
		images, err = listPodman(ctx, engine.Executable)
	case EngineApptainer:
		images, err = listApptainer()
	default:
		return nil, fmt.Errorf("unknown engine %q", engine.Name)
	}
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(images, func(a, b LocalImage) int { return b.Created.Compare(a.Created) })
	return images, nil
}

func listDocker(ctx context.Context) ([]LocalImage, error) {
	docker, err := newDockerClient()
	if err != nil {
		return nil, fmt.Errorf("could not reach Docker: %w", err)
	}
	defer docker.Close()
	summaries, err := docker.ImageList(ctx, image.ListOptions{Filters: filters.NewArgs(filters.Arg("reference", ImageRepository))})
	if err != nil {
		return nil, fmt.Errorf("Docker could not list its images: %w", err)
	}
	// A download that replaced its tag leaves the older image without one,
	// which the reference filter no longer finds: look among the dangling.
	dangling, _ := docker.ImageList(ctx, image.ListOptions{Filters: filters.NewArgs(filters.Arg("dangling", "true"))})
	images := []LocalImage{}
	add := func(summary image.Summary, ref, tag string, untagged bool) {
		images = append(images, LocalImage{
			Ref: ref, Tag: tag, Untagged: untagged, Size: summary.Size, Created: time.Unix(summary.Created, 0),
			Version: strings.TrimPrefix(summary.Labels[versionLabel], "v"), Revision: shortRevision(summary.Labels[revisionLabel]),
		})
	}
	for _, summary := range summaries {
		for _, name := range summary.RepoTags {
			if tag, ok := strings.CutPrefix(name, ImageRepository+":"); ok {
				add(summary, name, tag, false)
			}
		}
	}
	for _, summary := range dangling {
		if slices.ContainsFunc(summary.RepoDigests, func(digest string) bool { return strings.HasPrefix(digest, ImageRepository+"@") }) {
			add(summary, summary.ID, "", true)
		}
	}
	return images, nil
}

func listPodman(ctx context.Context, executable string) ([]LocalImage, error) {
	output, err := exec.CommandContext(ctx, executable, "images", "--all", "--format", "json").Output()
	if err != nil {
		return nil, fmt.Errorf("podman could not list its images: %w", err)
	}
	var listed []struct {
		ID      string            `json:"Id"`
		Names   []string          `json:"Names"`
		Digests []string          `json:"Digests"`
		History []string          `json:"History"`
		Size    int64             `json:"Size"`
		Created int64             `json:"Created"`
		Labels  map[string]string `json:"Labels"`
	}
	if err := json.Unmarshal(output, &listed); err != nil {
		return nil, fmt.Errorf("reading podman's images: %w", err)
	}
	images := []LocalImage{}
	for _, item := range listed {
		entry := LocalImage{
			Size: item.Size, Created: time.Unix(item.Created, 0),
			Version: strings.TrimPrefix(item.Labels[versionLabel], "v"), Revision: shortRevision(item.Labels[revisionLabel]),
		}
		tagged := false
		for _, name := range item.Names {
			if tag, ok := strings.CutPrefix(name, ImageRepository+":"); ok {
				entry.Ref, entry.Tag = name, tag
				images = append(images, entry)
				tagged = true
			}
		}
		// An older download a newer one replaced keeps only its history.
		if !tagged && len(item.Names) == 0 && slices.ContainsFunc(item.History, func(name string) bool { return strings.HasPrefix(name, ImageRepository+":") }) {
			entry.Ref, entry.Untagged = item.ID, true
			images = append(images, entry)
		}
	}
	return images, nil
}

var downloadedFile = regexp.MustCompile(`^codeflower-sandbox_(.+)\.sif$`)

func listApptainer() ([]LocalImage, error) {
	entries, err := os.ReadDir(ImageDirectory())
	if os.IsNotExist(err) {
		return []LocalImage{}, nil
	}
	if err != nil {
		return nil, err
	}
	images := []LocalImage{}
	for _, entry := range entries {
		match := downloadedFile.FindStringSubmatch(entry.Name())
		if match == nil || entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		item := LocalImage{Ref: filepath.Join(ImageDirectory(), entry.Name()), Tag: match[1], Size: info.Size(), Created: info.ModTime()}
		if released := releasedFile.FindStringSubmatch(entry.Name()); released != nil {
			item.Version = strings.Join(released[1:4], ".")
			item.Revision = released[4]
			item.Tag = item.Version
		}
		images = append(images, item)
	}
	return images, nil
}

// RemoveImage deletes a build of the published image that ListImages listed.
// Docker and Podman only remove the tag while another one names the image,
// and refuse an image a container uses.
func RemoveImage(ctx context.Context, engine EngineStatus, ref string) error {
	images, err := ListImages(ctx, engine)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(images, func(item LocalImage) bool { return item.Ref == ref }) {
		return fmt.Errorf("%s is not a sandbox image on this computer", ref)
	}
	switch engine.Name {
	case EngineDocker:
		docker, err := newDockerClient()
		if err != nil {
			return fmt.Errorf("could not reach Docker: %w", err)
		}
		defer docker.Close()
		if _, err := docker.ImageRemove(ctx, ref, image.RemoveOptions{PruneChildren: true}); err != nil {
			return fmt.Errorf("Docker could not remove %s: %w", ref, err)
		}
	case EnginePodman:
		if output, err := exec.CommandContext(ctx, engine.Executable, "rmi", ref).CombinedOutput(); err != nil {
			return fmt.Errorf("podman could not remove %s: %s", ref, strings.TrimSpace(string(output)))
		}
	case EngineApptainer:
		if err := os.Remove(ref); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown engine %q", engine.Name)
	}
	return nil
}
