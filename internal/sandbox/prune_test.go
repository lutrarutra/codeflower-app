package sandbox

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReviewImages(t *testing.T) {
	ref := func(tag string) string { return ImageRepository + ":" + tag }
	day := func(n int) time.Time { return time.Date(2026, 11, n, 0, 0, 0, 0, time.UTC) }
	keep := keepers{refs: map[string]string{}, tags: map[string]string{}}
	engine := EngineStatus{Name: EngineDocker}
	keep.add(engine, "", "the release this app runs")
	keep.add(engine, ref("0.1.0"), "project old chooses it")
	images := []LocalImage{
		{Ref: ref(DefaultImageTag), ID: "sha256:default", Tag: DefaultImageTag, Version: DefaultImageTag, Created: day(20)},
		{Ref: ref("99.0.0"), ID: "sha256:newer", Tag: "99.0.0", Version: "99.0.0", Created: day(24)},
		{Ref: ref("0.1.0"), ID: "sha256:pinned", Tag: "0.1.0", Version: "0.1.0", Created: day(1)},
		{Ref: ref("0.2.0"), ID: "sha256:used", Tag: "0.2.0", Version: "0.2.0", Created: day(2)},
		{Ref: ref("0.3.0"), ID: "sha256:old", Tag: "0.3.0", Version: "0.3.0", Created: day(3)},
		{Ref: ref("latest"), ID: "sha256:oldlatest", Tag: "latest", Version: "v0.3.1-rc.1", Created: day(4)},
		{Ref: ref("sha-abc1234"), ID: "sha256:build", Tag: "sha-abc1234", Created: day(5)},
		{Ref: ref("sha-def5678"), ID: "sha256:newbuild", Tag: "sha-def5678", Created: day(22)},
		{Ref: "sha256:replaced", ID: "sha256:replaced", Untagged: true, Created: day(19)},
	}
	archives := []LocalImage{
		{Ref: "/images/codeflower-sandbox_0.1.0.tar", Tag: "0.1.0", Version: "0.1.0", Created: day(1)},
		{Ref: "/images/codeflower-sandbox_0.3.0.tar", Tag: "0.3.0", Version: "0.3.0", Created: day(3)},
		{Ref: "/images/codeflower-sandbox_" + DefaultImageTag + ".tar", Tag: DefaultImageTag, Version: DefaultImageTag, Created: day(20)},
	}
	used := map[string]bool{"used": true}

	unused := map[string]bool{}
	for _, review := range reviewImages(images, archives, keep, used) {
		if review.Reason == "" {
			t.Errorf("%s has no reason", review.Ref)
		}
		if review.Unused {
			unused[review.Ref] = true
		}
	}
	want := map[string]bool{
		ref("0.3.0"): true, ref("latest"): true, ref("sha-abc1234"): true, "sha256:replaced": true,
		"/images/codeflower-sandbox_0.3.0.tar": true,
	}
	for name := range want {
		if !unused[name] {
			t.Errorf("%s should be unused", name)
		}
	}
	for name := range unused {
		if !want[name] {
			t.Errorf("%s should be kept", name)
		}
	}
}

func TestReviewImagesUnknownRelease(t *testing.T) {
	// Without the app's release here, a build without a release is kept.
	images := []LocalImage{{Ref: ImageRepository + ":sha-abc1234", Tag: "sha-abc1234", Created: time.Now()}}
	reviews := reviewImages(images, nil, keepers{refs: map[string]string{}, tags: map[string]string{}}, nil)
	if reviews[0].Unused {
		t.Fatalf("%+v should be kept", reviews[0])
	}
}

func TestListArchives(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("CODEFLOWER_IMAGE_DIR", directory)
	for _, name := range []string{"codeflower-sandbox_0.3.0.tar", "codeflower-sandbox_latest.tar", "codeflower-sandbox_0.3.0.tar.part-abc123", "other.tar"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	archives := listArchives()
	if len(archives) != 2 {
		t.Fatalf("archives = %+v", archives)
	}
	for _, item := range archives {
		if item.Tag == "0.3.0" && item.Version != "0.3.0" || item.Tag == "latest" && item.Version != "" {
			t.Errorf("archive %+v", item)
		}
	}
	review := ImageReview{LocalImage: LocalImage{Ref: filepath.Join(directory, "other.tar")}, Archive: true}
	if err := RemoveReviewed(t.Context(), EngineStatus{Name: EnginePodman}, review); err == nil {
		t.Fatal("removed a file that is no saved copy")
	}
}

func TestReleaseNumbers(t *testing.T) {
	for version, want := range map[string]bool{"0.42.0": true, "v1.2.3": true, "0.3.1-rc.1": true, "0.42": false, "latest": false, "": false} {
		if _, ok := releaseNumbers(version); ok != want {
			t.Errorf("releaseNumbers(%q) = %v", version, ok)
		}
	}
}
