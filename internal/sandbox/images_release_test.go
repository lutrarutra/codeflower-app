package sandbox

import (
	"context"
	"testing"
)

func TestImageReleaseOfAPublishedRelease(t *testing.T) {
	// A full release tag says it without asking the engine (none here).
	if got := ImageRelease(context.Background(), EngineStatus{Name: "none"}, ImageRepository+":0.32.0"); got != "0.32.0" {
		t.Fatalf("ImageRelease = %q", got)
	}
	// latest, a minor and a local build ask the image's labels, which an
	// unknown engine cannot read.
	for _, image := range []string{ImageRepository + ":latest", ImageRepository + ":0.32", "codeflower-sandbox"} {
		if got := ImageRelease(context.Background(), EngineStatus{Name: "none"}, image); got != "" {
			t.Fatalf("ImageRelease(%s) = %q", image, got)
		}
	}
}
