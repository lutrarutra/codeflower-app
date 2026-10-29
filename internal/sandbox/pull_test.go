package sandbox

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDefaultImageIsTheLatestRelease(t *testing.T) {
	if DefaultImage != "ghcr.io/lutrarutra/codeflower-sandbox:latest" {
		t.Fatalf("DefaultImage = %q", DefaultImage)
	}
}

func TestPublishedImageTags(t *testing.T) {
	for tag, want := range map[string]string{
		"":             "latest",
		" latest ":     "latest",
		"0.2.1":        "0.2.1",
		"v0.2.1":       "0.2.1",
		"v0.2":         "0.2",
		"sha-abc1234":  "sha-abc1234",
		"abc1234":      "sha-abc1234",
		"abc1234def56": "sha-abc1234",
		"sha-abc1234def56789012345678901234567890ab": "sha-abc1234",
		"1234567": "1234567",
	} {
		got, err := PublishedImage(tag)
		if err != nil || got != ImageRepository+":"+want {
			t.Errorf("PublishedImage(%q) = %q, %v; want tag %q", tag, got, err, want)
		}
	}
	for _, tag := range []string{"0.2 1", "-rc", "a/b", "x:y"} {
		if _, err := PublishedImage(tag); err == nil {
			t.Errorf("PublishedImage(%q) accepted", tag)
		}
	}
}

func TestPublishedImagesAreDownloadedPerTag(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("CODEFLOWER_IMAGE_DIR", directory)
	image, _ := PublishedImage("0.2")
	if !IsPublished(image) || !IsPublished("") || IsPublished("codeflower-sandbox") || IsPublished(ImageRepository+"@sha256:0") {
		t.Fatal("IsPublished is wrong")
	}
	if got, _ := ResolveImage(image, EngineDocker); got != image {
		t.Fatalf("Docker: %q", got)
	}
	if got, _ := ResolveImage(image, EngineApptainer); got != filepath.Join(directory, "codeflower-sandbox_0.2.sif") {
		t.Fatalf("Apptainer: %q", got)
	}
	if err := pullImage(context.Background(), EngineStatus{Name: EngineDocker}, "codeflower-sandbox", false, io.Discard); err == nil {
		t.Fatal("an image that is not the published one was downloaded")
	}
}

func TestApptainerDefaultsToTheDownloadedFile(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("CODEFLOWER_IMAGE_DIR", directory)
	image, err := ResolveImage("", EngineApptainer)
	if err != nil || image != filepath.Join(directory, "codeflower-sandbox_latest.sif") {
		t.Fatalf("ResolveImage = %q, %v", image, err)
	}
	engine := EngineStatus{Name: EngineApptainer}
	if err := CheckImage(context.Background(), engine, ""); !errors.Is(err, ErrNoImage) {
		t.Fatalf("missing default image: got %v", err)
	}
	if err := os.WriteFile(image, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckImage(context.Background(), engine, ""); err != nil {
		t.Fatalf("downloaded image: got %v", err)
	}
	if _, err := ResolveImage(filepath.Join(directory, "other.sif"), EngineApptainer); err == nil {
		t.Fatal("a named image file that does not exist was accepted")
	}
}

// fakeApptainer writes a script that "pulls" by writing its arguments to the
// target file after a short delay, and counts its runs.
func fakeApptainer(t *testing.T) (string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Apptainer does not run on Windows")
	}
	directory := t.TempDir()
	runs := filepath.Join(directory, "runs")
	script := filepath.Join(directory, "apptainer")
	body := "#!/bin/sh\necho run >> " + runs + "\nsleep 0.2\necho \"$@\" > \"$3\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script, runs
}

func TestApptainerPullReplacesTheFileAtOnce(t *testing.T) {
	script, runs := fakeApptainer(t)
	target := filepath.Join(t.TempDir(), "images", "sandbox.sif")
	var out strings.Builder
	if err := pullApptainer(context.Background(), script, "ghcr.io/x/y:1.2", target, true, &out); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(target)
	if err != nil || !strings.Contains(string(data), "pull --force") || !strings.Contains(string(data), "docker://ghcr.io/x/y:1.2") {
		t.Fatalf("target = %q, %v", data, err)
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(target), ".*.tmp"))
	if len(leftovers) != 0 {
		t.Fatalf("temporary files left: %v", leftovers)
	}
	// A refresh downloads again.
	if err := pullApptainer(context.Background(), script, "ghcr.io/x/y:1.2", target, true, &out); err != nil {
		t.Fatal(err)
	}
	if count, _ := os.ReadFile(runs); strings.Count(string(count), "run") != 2 {
		t.Fatalf("runs = %q, want 2", count)
	}
}

func TestConcurrentApptainerPullsDownloadOnce(t *testing.T) {
	script, runs := fakeApptainer(t)
	target := filepath.Join(t.TempDir(), "sandbox.sif")
	var group sync.WaitGroup
	errs := make(chan error, 3)
	for range 3 {
		group.Add(1)
		go func() {
			defer group.Done()
			errs <- pullApptainer(context.Background(), script, "ghcr.io/x/y:1.2", target, false, new(strings.Builder))
		}()
	}
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if count, _ := os.ReadFile(runs); strings.Count(string(count), "run") != 1 {
		t.Fatalf("runs = %q, want 1", count)
	}
}

func TestReportPullSumsLayers(t *testing.T) {
	stream := strings.Join([]string{
		`{"status":"Pulling from lutrarutra/codeflower-sandbox","id":"0.1"}`,
		`{"status":"Pulling fs layer","id":"a"}`,
		`{"status":"Pulling fs layer","id":"b"}`,
		`{"status":"Downloading","id":"a","progressDetail":{"current":1048576,"total":2097152}}`,
		`{"status":"Downloading","id":"b","progressDetail":{"current":1048576,"total":4194304}}`,
		`{"status":"Pull complete","id":"a"}`,
		`{"status":"Digest: sha256:abc"}`,
		`{"status":"Status: Downloaded newer image for ghcr.io/lutrarutra/codeflower-sandbox:0.1"}`,
	}, "\n")
	clock := time.Unix(0, 0)
	now := func() time.Time { clock = clock.Add(3 * time.Second); return clock }
	var out strings.Builder
	if err := reportPull(strings.NewReader(stream), &out, now); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{"3 of 6 MB, 1 of 2 layers done", "Status: Downloaded newer image"} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	failed := `{"errorDetail":{"message":"denied"},"error":"denied"}`
	if err := reportPull(strings.NewReader(failed), &out, now); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("error not reported: %v", err)
	}
}
