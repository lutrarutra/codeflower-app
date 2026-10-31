package sandbox

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDefaultImageIsAPinnedRelease(t *testing.T) {
	if !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(DefaultImageTag) || DefaultImage != ImageRepository+":"+DefaultImageTag {
		t.Fatalf("DefaultImage = %q", DefaultImage)
	}
}

func TestPublishedImageTags(t *testing.T) {
	for tag, want := range map[string]string{
		"":             DefaultImageTag,
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
	for _, name := range []string{"codeflower-sandbox_v0.2.9-sha-1111111.sif", "codeflower-sandbox_v0.2.10-sha-2222222.sif", "codeflower-sandbox_v0.3.0-sha-3333333.sif"} {
		if err := os.WriteFile(filepath.Join(directory, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for tag, want := range map[string]string{
		"0.2":         "codeflower-sandbox_v0.2.10-sha-2222222.sif",
		"0.2.9":       "codeflower-sandbox_v0.2.9-sha-1111111.sif",
		"sha-1111111": "codeflower-sandbox_v0.2.9-sha-1111111.sif",
		"latest":      "codeflower-sandbox_v0.3.0-sha-3333333.sif",
		"0.4":         "codeflower-sandbox_0.4.sif",
	} {
		image, _ := PublishedImage(tag)
		if got, _ := ResolveImage(image, EngineApptainer); got != filepath.Join(directory, want) {
			t.Errorf("Apptainer %s: %q, want %s", tag, got, want)
		}
	}
	if err := pullImage(context.Background(), EngineStatus{Name: EngineDocker}, "codeflower-sandbox", false, io.Discard); err == nil {
		t.Fatal("an image that is not the published one was downloaded")
	}
}

func TestApptainerDefaultsToTheDownloadedFile(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("CODEFLOWER_IMAGE_DIR", directory)
	image, err := ResolveImage("", EngineApptainer)
	if err != nil || image != filepath.Join(directory, "codeflower-sandbox_"+DefaultImageTag+".sif") {
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
// target file after a short delay, and counts its runs; it labels the image
// with labels, a JSON object.
func fakeApptainer(t *testing.T, labels string) (string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Apptainer does not run on Windows")
	}
	directory := t.TempDir()
	runs := filepath.Join(directory, "runs")
	script := filepath.Join(directory, "apptainer")
	body := "#!/bin/sh\nif [ \"$1\" = inspect ]; then echo '{\"data\":{\"attributes\":{\"labels\":" + labels + "}}}'; exit; fi\n" +
		"echo run >> " + runs + "\nsleep 0.2\necho \"$@\" > \"$3\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script, runs
}

func TestApptainerPullReplacesTheFileAtOnce(t *testing.T) {
	script, runs := fakeApptainer(t, `{"org.opencontainers.image.version":"v1.2.3","org.opencontainers.image.revision":"abc1234def"}`)
	directory := filepath.Join(t.TempDir(), "images")
	t.Setenv("CODEFLOWER_IMAGE_DIR", directory)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	older := filepath.Join(directory, "codeflower-sandbox_1.2.sif")
	if err := os.WriteFile(older, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(directory, "codeflower-sandbox_v1.2.3-sha-abc1234.sif")
	var out strings.Builder
	if err := pullApptainer(context.Background(), script, "ghcr.io/x/y:1.2", "1.2", true, &out); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(target)
	if err != nil || !strings.Contains(string(data), "pull --force") || !strings.Contains(string(data), "docker://ghcr.io/x/y:1.2") {
		t.Fatalf("target = %q, %v", data, err)
	}
	if _, err := os.Stat(older); err == nil {
		t.Fatal("the file named after the tag was kept")
	}
	leftovers, _ := filepath.Glob(filepath.Join(directory, ".*.tmp"))
	if len(leftovers) != 0 {
		t.Fatalf("temporary files left: %v", leftovers)
	}
	// A refresh downloads again.
	if err := pullApptainer(context.Background(), script, "ghcr.io/x/y:1.2", "1.2", true, &out); err != nil {
		t.Fatal(err)
	}
	if count, _ := os.ReadFile(runs); strings.Count(string(count), "run") != 2 {
		t.Fatalf("runs = %q, want 2", count)
	}
}

func TestApptainerPullWithoutLabelsIsNamedAfterTheTag(t *testing.T) {
	script, _ := fakeApptainer(t, `{}`)
	directory := t.TempDir()
	t.Setenv("CODEFLOWER_IMAGE_DIR", directory)
	if err := pullApptainer(context.Background(), script, "ghcr.io/x/y:1.2", "1.2", false, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(directory, "codeflower-sandbox_1.2.sif")); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentApptainerPullsDownloadOnce(t *testing.T) {
	script, runs := fakeApptainer(t, `{"org.opencontainers.image.version":"v1.2.3","org.opencontainers.image.revision":"abc1234def"}`)
	t.Setenv("CODEFLOWER_IMAGE_DIR", t.TempDir())
	var group sync.WaitGroup
	errs := make(chan error, 3)
	for range 3 {
		group.Add(1)
		go func() {
			defer group.Done()
			errs <- pullApptainer(context.Background(), script, "ghcr.io/x/y:1.2", "1.2", false, new(strings.Builder))
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
