package sandbox

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakePodman has the image once pulled or loaded, and logs its commands.
func fakePodman(t *testing.T) (EngineStatus, string, string) {
	t.Helper()
	folder := t.TempDir()
	have, log := filepath.Join(folder, "have"), filepath.Join(folder, "log")
	script := "#!/bin/sh\necho \"$1\" >> " + log + "\n" +
		"case \"$1\" in\n" +
		"image) [ -f " + have + " ] ;;\n" +
		"pull) touch " + have + " ;;\n" +
		"save) echo archive > \"$5\" ;;\n" +
		"load) touch " + have + "; echo 'Loaded image: sha256:0123456789abcdef' ;;\n" +
		"esac\n"
	executable := filepath.Join(folder, "podman")
	if err := os.WriteFile(executable, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return EngineStatus{Name: EnginePodman, Label: "Podman", Executable: executable}, have, log
}

func commands(t *testing.T, log string) string {
	t.Helper()
	data, _ := os.ReadFile(log)
	_ = os.Remove(log)
	return strings.Join(strings.Fields(string(data)), " ")
}

func TestPodmanImageArchive(t *testing.T) {
	engine, have, log := fakePodman(t)
	archives := t.TempDir()
	t.Setenv("CODEFLOWER_IMAGE_DIR", "")
	SetImageDir(archives)
	t.Cleanup(func() { SetImageDir("") })
	ctx, out := context.Background(), &bytes.Buffer{}
	archive := filepath.Join(archives, "codeflower-sandbox_"+DefaultImageTag+".tar")

	// The first time downloads it and saves a copy.
	if err := EnsureImage(ctx, engine, "", out); err != nil {
		t.Fatal(err)
	}
	if got := commands(t, log); got != "image pull save" {
		t.Fatal(got)
	}
	if _, err := os.Stat(archive); err != nil {
		t.Fatal(err)
	}

	// Podman lost it (storage per job): the copy is loaded, not downloaded.
	_ = os.Remove(have)
	if err := EnsureImage(ctx, engine, "", out); err != nil {
		t.Fatal(err)
	}
	if got := commands(t, log); got != "image load image" {
		t.Fatal(got)
	}

	// `codeflower pull` downloads it again and replaces the copy.
	if err := PullImage(ctx, engine, "", out); err != nil {
		t.Fatal(err)
	}
	if got := commands(t, log); got != "pull save" {
		t.Fatal(got)
	}

	// The variable comes before the config.
	variable := t.TempDir()
	t.Setenv("CODEFLOWER_IMAGE_DIR", variable)
	_ = os.Remove(have)
	if err := EnsureImage(ctx, engine, "", out); err != nil {
		t.Fatal(err)
	}
	if got := commands(t, log); got != "image pull save" {
		t.Fatal(got)
	}
	if _, err := os.Stat(filepath.Join(variable, filepath.Base(archive))); err != nil {
		t.Fatal(err)
	}

	// With neither, the default folder gets no copy.
	t.Setenv("CODEFLOWER_IMAGE_DIR", "")
	SetImageDir("")
	_ = os.Remove(have)
	if err := EnsureImage(ctx, engine, "", out); err != nil {
		t.Fatal(err)
	}
	if got := commands(t, log); got != "image pull" {
		t.Fatal(got)
	}
}
