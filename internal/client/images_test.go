package client

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codeflower-app/internal/sandbox"
)

func TestTheLogRedrawsALineAtACarriageReturn(t *testing.T) {
	item := &worker{}
	_, _ = item.Write([]byte("Getting image\n 10%\r 50"))
	if view := item.view(); strings.Join(view.Log, "|") != "Getting image| 50" || view.LastOutput.IsZero() {
		t.Fatalf("while drawing: %q", view.Log)
	}
	_, _ = item.Write([]byte("%\r"))
	_, _ = item.Write([]byte("\n100%\r\ndone\n"))
	if view := item.view(); strings.Join(view.Log, "|") != "Getting image| 50%|100%|done" {
		t.Fatalf("drawn: %q", view.Log)
	}
	item.PullProgress(sandbox.PullProgress{Current: 1, Total: 4, Layers: 2, Done: 1})
	if view := item.view(); view.Progress == nil || view.Progress.Total != 4 {
		t.Fatalf("progress: %+v", view.Progress)
	}
}

func TestThePageListsAndRemovesApptainerImages(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("CODEFLOWER_IMAGE_DIR", directory)
	released := filepath.Join(directory, "codeflower-sandbox_v"+sandbox.DefaultImageTag+"-sha-abc1234.sif")
	older := filepath.Join(directory, "codeflower-sandbox_latest.sif")
	for _, file := range []string{released, older, filepath.Join(directory, ".codeflower-sandbox_latest.sif.1.tmp"), filepath.Join(directory, "other.sif")} {
		if err := os.WriteFile(file, []byte("sif"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	server, web := testServer(t)
	server.detectEngines = func(context.Context) []sandbox.EngineStatus {
		return []sandbox.EngineStatus{{Name: "apptainer", Label: "Apptainer", Installed: true, Ready: true}}
	}
	response, data := authorized(t, web, "GET", "/api/images?engine=apptainer", "")
	images, _ := data["images"].([]any)
	if response.StatusCode != 200 || len(images) != 2 || data["directory"] != directory || data["defaultTag"] != sandbox.DefaultImageTag {
		t.Fatalf("list %d %v", response.StatusCode, data)
	}
	defaults := 0
	for _, item := range images {
		image := item.(map[string]any)
		if image["default"] == true {
			defaults++
			if image["ref"] != released || image["version"] != sandbox.DefaultImageTag || image["revision"] != "abc1234" {
				t.Fatalf("default image: %v", image)
			}
		}
	}
	if defaults != 1 {
		t.Fatalf("defaults: %v", images)
	}
	if response, _ := authorized(t, web, "DELETE", "/api/images", `{"engine":"apptainer","ref":"`+filepath.Join(directory, "other.sif")+`"}`); response.StatusCode != 400 {
		t.Fatalf("removing a file that is no sandbox image: %d", response.StatusCode)
	}
	if response, data := authorized(t, web, "DELETE", "/api/images", `{"engine":"apptainer","ref":"`+older+`"}`); response.StatusCode != 200 {
		t.Fatalf("remove %d %v", response.StatusCode, data)
	}
	if _, err := os.Stat(older); !os.IsNotExist(err) {
		t.Fatalf("the image file is still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, "other.sif")); err != nil {
		t.Fatal("another file went too")
	}
}

func TestAPullOfAnUnknownTagIsRefused(t *testing.T) {
	server, web := testServer(t)
	server.detectEngines = func(context.Context) []sandbox.EngineStatus {
		return []sandbox.EngineStatus{{Name: "apptainer", Label: "Apptainer", Installed: true, Ready: true}}
	}
	if response, data := authorized(t, web, "POST", "/api/images/pull", `{"imageTag":"no such tag!"}`); response.StatusCode != 400 {
		t.Fatalf("pull %d %v", response.StatusCode, data)
	}
	if response, _ := authorized(t, web, "DELETE", "/api/images/pull/nope", `{}`); response.StatusCode != 404 {
		t.Fatalf("dismiss %d", response.StatusCode)
	}
}
