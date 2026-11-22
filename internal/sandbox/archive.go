package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// configImageDir is the config's image_dir (SetImageDir).
var configImageDir atomic.Pointer[string]

// SetImageDir sets the config's image_dir, the image folder when
// $CODEFLOWER_IMAGE_DIR is not set; empty for neither.
func SetImageDir(directory string) {
	configImageDir.Store(&directory)
}

// chosenImageDirectory is the image folder the user chose:
// $CODEFLOWER_IMAGE_DIR, else the config's image_dir; "" for the default.
func chosenImageDirectory() string {
	if directory := os.Getenv("CODEFLOWER_IMAGE_DIR"); directory != "" {
		return expandHome(directory)
	}
	if directory := configImageDir.Load(); directory != nil {
		return *directory
	}
	return ""
}

// publishedArchive is where Podman's saved copy of a build of the published
// image is kept, or "" when the user chose no image folder: in the default
// one, a copy would only double the disk Podman's own storage already takes.
func publishedArchive(tag string) string {
	directory := chosenImageDirectory()
	if directory == "" {
		return ""
	}
	return filepath.Join(directory, "codeflower-sandbox_"+tag+".tar")
}

// savedArchive is the saved copy of image that engine can load instead of
// downloading it, or "" when there is none.
func savedArchive(engine EngineStatus, image string) string {
	if engine.Name != EnginePodman {
		return ""
	}
	tag, ok := publishedTag(image)
	if !ok {
		return ""
	}
	archive := publishedArchive(tag)
	if archive == "" {
		return ""
	}
	if _, err := os.Stat(archive); err != nil {
		return ""
	}
	return archive
}

// saveArchive saves the engine's image to archive, replacing it at once, so
// that another process never loads half of it.
func saveArchive(ctx context.Context, engine EngineStatus, image, archive string) error {
	if err := os.MkdirAll(filepath.Dir(archive), 0o755); err != nil {
		return err
	}
	temporary := archive + ".part-" + randomID()[:6]
	defer os.Remove(temporary)
	output, err := exec.CommandContext(ctx, engine.Executable, "save", "--format", "oci-archive", "-o", temporary, image).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return os.Rename(temporary, archive)
}

// loadArchive loads a saved copy of image into the engine, under its name.
func loadArchive(ctx context.Context, engine EngineStatus, archive, image string) error {
	output, err := exec.CommandContext(ctx, engine.Executable, "load", "-i", archive).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	// An OCI archive loads without its name: give it back.
	if CheckImage(ctx, engine, image) == nil {
		return nil
	}
	if id := loadedImage(string(output)); id != "" {
		_ = exec.CommandContext(ctx, engine.Executable, "tag", id, image).Run()
	}
	return CheckImage(ctx, engine, image)
}
