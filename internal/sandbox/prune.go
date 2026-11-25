package sandbox

import (
	"context"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
)

// The images a computer gathers: each update of the app runs a newer release
// of the published image, and leaves the older one behind. ReviewImages says
// which ones nothing needs any more, for `codeflower images prune`.

// ImageReview is a build of the published image with whether it is still
// needed.
type ImageReview struct {
	LocalImage
	// Archive: Podman's saved copy (.tar) in the image folder, not an image
	// in Podman's storage.
	Archive bool `json:"archive,omitempty"`
	// Unused: nothing needs it, and a newer release replaced it.
	Unused bool `json:"unused"`
	// Reason says why it is kept, or why it is not needed.
	Reason string `json:"reason"`
}

// ReviewImages lists the builds of the published image the engine has, and
// Podman's saved copies, newest first, saying which ones nothing needs. One
// is kept when it is the release this app runs, a sandbox or container uses
// it, chosen names it (an image name, as --image and saved settings give it,
// with who chose it), or it is not older than the app's release. The others
// are unused: those older than the app's release, and older downloads a newer
// one of their tag replaced.
func ReviewImages(ctx context.Context, engine EngineStatus, chosen map[string]string) ([]ImageReview, error) {
	images, err := ListImages(ctx, engine)
	if err != nil {
		return nil, err
	}
	var archives []LocalImage
	if engine.Name == EnginePodman {
		archives = listArchives()
	}
	used, err := imagesUsed(ctx, engine)
	if err != nil {
		return nil, err
	}
	keep := keepers{refs: map[string]string{}, tags: map[string]string{}}
	keep.add(engine, "", "the release this app runs")
	for _, name := range slices.Sorted(maps.Keys(chosen)) {
		keep.add(engine, name, chosen[name])
	}
	return reviewImages(images, archives, keep, used), nil
}

// keepers are the images something chose, by ref (as ListImages names them)
// and by tag (for Podman's saved copies), with who chose them.
type keepers struct {
	refs, tags map[string]string
	// defaultRef is the release this app runs, as ListImages names it.
	defaultRef string
}

func (k *keepers) add(engine EngineStatus, name, who string) {
	tag, published := publishedTag(name)
	if !published {
		return
	}
	ref, err := ResolveImage(name, engine.Name)
	if err != nil {
		return
	}
	if name == "" {
		k.defaultRef = ref
	}
	if _, known := k.refs[ref]; !known {
		k.refs[ref] = who
	}
	if _, known := k.tags[tag]; !known {
		k.tags[tag] = who
	}
}

// reviewImages decides for each image; used holds the refs and ids of the
// images sandboxes and containers use.
func reviewImages(images, archives []LocalImage, keep keepers, used map[string]bool) []ImageReview {
	var defaultCreated time.Time
	for _, item := range images {
		if item.Ref == keep.defaultRef {
			defaultCreated = item.Created
		}
	}
	older := func(item LocalImage) (bool, string) {
		version := item.Version
		if version == "" && releaseTag.MatchString(item.Tag) {
			version = item.Tag
		}
		if numbers, ok := releaseNumbers(version); ok {
			defaults, _ := releaseNumbers(DefaultImageTag)
			if slices.Compare(numbers, defaults) < 0 {
				return true, "older than " + DefaultImageTag + ", the release this app runs"
			}
			return false, "not older than " + DefaultImageTag + ", the release this app runs"
		}
		if !defaultCreated.IsZero() && !item.Created.IsZero() {
			if item.Created.Before(defaultCreated) {
				return true, "built before " + DefaultImageTag + ", the release this app runs"
			}
			return false, "built after " + DefaultImageTag + ", the release this app runs"
		}
		return false, "its release is not known"
	}
	reviews := []ImageReview{}
	for _, item := range images {
		review := ImageReview{LocalImage: item}
		switch {
		case item.Ref == keep.defaultRef:
			review.Reason = "the release this app runs"
		case used[item.Ref] || (item.ID != "" && used[imageID(item.ID)]):
			review.Reason = "a sandbox or container uses it"
		case keep.refs[item.Ref] != "":
			review.Reason = keep.refs[item.Ref]
		case item.Untagged:
			review.Unused, review.Reason = true, "a newer download of its tag replaced it"
		default:
			review.Unused, review.Reason = older(item)
		}
		reviews = append(reviews, review)
	}
	for _, item := range archives {
		review := ImageReview{LocalImage: item, Archive: true}
		if who := keep.tags[item.Tag]; who != "" {
			review.Reason = who
		} else {
			review.Unused, review.Reason = older(item)
		}
		reviews = append(reviews, review)
	}
	slices.SortStableFunc(reviews, func(a, b ImageReview) int { return b.Created.Compare(a.Created) })
	return reviews
}

// releaseNumbers parses a release (0.39.0, or 0.39.0-rc.1 by its numbers).
func releaseNumbers(version string) ([]int, bool) {
	version, _, _ = strings.Cut(strings.TrimPrefix(version, "v"), "-")
	if !releaseTag.MatchString(version) {
		return nil, false
	}
	numbers := []int{}
	for part := range strings.SplitSeq(version, ".") {
		number, err := strconv.Atoi(part)
		if err != nil {
			return nil, false
		}
		numbers = append(numbers, number)
	}
	return numbers, true
}

// imageID is an image's id without its sha256: prefix, which Podman leaves out.
func imageID(id string) string {
	return strings.TrimPrefix(id, "sha256:")
}

// imagesUsed are the images the sandboxes running here and the engine's
// containers, running or stopped, use: by ref and by id.
func imagesUsed(ctx context.Context, engine EngineStatus) (map[string]bool, error) {
	used := map[string]bool{}
	for _, running := range ListRunning() {
		if running.Engine == engine.Name && running.Image != "" {
			used[running.Image] = true
		}
	}
	switch engine.Name {
	case EngineDocker:
		docker, err := newDockerClient()
		if err != nil {
			return nil, fmt.Errorf("could not reach Docker: %w", err)
		}
		defer docker.Close()
		containers, err := docker.ContainerList(ctx, container.ListOptions{All: true})
		if err != nil {
			return nil, fmt.Errorf("Docker could not list its containers: %w", err)
		}
		for _, item := range containers {
			used[imageID(item.ImageID)] = true
		}
	case EnginePodman:
		output, err := exec.CommandContext(ctx, engine.Executable, "ps", "--all", "--format", "{{.ImageID}}").Output()
		if err != nil {
			return nil, fmt.Errorf("podman could not list its containers: %w", err)
		}
		for id := range strings.FieldsSeq(string(output)) {
			used[imageID(id)] = true
		}
	}
	return used, nil
}

var archiveFile = regexp.MustCompile(`^codeflower-sandbox_(.+)\.tar$`)

// listArchives lists Podman's saved copies of the published image, in the
// image folder the user chose (there are none in the default one).
func listArchives() []LocalImage {
	directory := chosenImageDirectory()
	if directory == "" {
		return nil
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil
	}
	var archives []LocalImage
	for _, entry := range entries {
		match := archiveFile.FindStringSubmatch(entry.Name())
		if match == nil || entry.IsDir() || !imageTag.MatchString(match[1]) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		item := LocalImage{Ref: filepath.Join(directory, entry.Name()), Tag: match[1], Size: info.Size(), Created: info.ModTime()}
		if releaseTag.MatchString(item.Tag) {
			item.Version = item.Tag
		}
		archives = append(archives, item)
	}
	return archives
}

// RemoveReviewed removes an image ReviewImages listed: the engine's image, or
// Podman's saved copy.
func RemoveReviewed(ctx context.Context, engine EngineStatus, review ImageReview) error {
	if !review.Archive {
		return RemoveImage(ctx, engine, review.Ref)
	}
	if !slices.ContainsFunc(listArchives(), func(item LocalImage) bool { return item.Ref == review.Ref }) {
		return fmt.Errorf("%s is not a saved copy of the sandbox image", review.Ref)
	}
	return os.Remove(review.Ref)
}
