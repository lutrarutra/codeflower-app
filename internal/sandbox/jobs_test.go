package sandbox

import (
	"path/filepath"
	"strings"
	"testing"

	"codeflower-app/internal/jobs"
)

func TestJobIndex(t *testing.T) {
	spec := jobs.Spec{Indices: []int{1, 3}, IndexEnv: []string{"SLURM_ARRAY_TASK_ID", "CODEFLOWER_JOB_INDEX"}}
	env := map[string]string{"CODEFLOWER_JOB_INDEX": "3"}
	index, err := jobIndex(spec, func(name string) string { return env[name] })
	if err != nil || index == nil || *index != 3 {
		t.Fatalf("%v %v", index, err)
	}
	env["SLURM_ARRAY_TASK_ID"] = "2"
	if _, err := jobIndex(spec, func(name string) string { return env[name] }); err == nil {
		t.Fatal("an index outside the array must fail")
	}
	if _, err := jobIndex(spec, func(string) string { return "" }); err == nil {
		t.Fatal("an array without an index must fail")
	}
	if index, err := jobIndex(jobs.Spec{}, func(string) string { return "5" }); index != nil || err != nil {
		t.Fatalf("no array: %v %v", index, err)
	}
}

func TestVisibleGPUs(t *testing.T) {
	if got := visibleGPUs(func(name string) string { return map[string]string{"SLURM_JOB_GPUS": "2,3"}[name] }); got != "2,3" {
		t.Fatal(got)
	}
	if got := visibleGPUs(func(string) string { return "" }); got != "all" {
		t.Fatal(got)
	}
}

func TestImageFor(t *testing.T) {
	if tag, ok := sifTag("/images/codeflower-sandbox_v0.35.1-sha-abc.sif"); !ok || tag != "0.35.1" {
		t.Fatalf("%s %v", tag, ok)
	}
	image, err := imageFor("/images/codeflower-sandbox_v0.35.1-sha-abc.sif", EngineApptainer, EnginePodman)
	if err != nil || image != ImageRepository+":0.35.1" {
		t.Fatalf("%s %v", image, err)
	}
	if _, err := imageFor("my/own:image", EnginePodman, EngineApptainer); err == nil {
		t.Fatal("a local image has no Apptainer form")
	}
	t.Setenv("CODEFLOWER_IMAGE_DIR", t.TempDir())
	if _, err := imageFor(ImageRepository+":0.35.1", EnginePodman, EngineApptainer); err == nil || !strings.Contains(err.Error(), "codeflower pull --engine apptainer") {
		t.Fatalf("a missing image file: %v", err)
	}
}

func TestLoadedImage(t *testing.T) {
	if id := loadedImage("Getting image source signatures\nLoaded image: sha256:0123456789abcdef0123\n"); id != "0123456789abcdef0123" {
		t.Fatal(id)
	}
	if id := loadedImage("Loaded image: ghcr.io/x:1\n"); id != "" {
		t.Fatal(id)
	}
}

func TestJobsBaseMounts(t *testing.T) {
	directory := t.TempDir()
	state := filepath.Join(directory, "project")
	output := filepath.Join(directory, "out")
	w := Worker{State: state, Engine: EngineStatus{Name: EngineDocker},
		Binds: []Bind{{Source: output, Target: OutputsRoot + "/out"}, {Source: "/data", Target: InputsRoot + "/data", ReadOnly: true}},
		Jobs:  &jobs.Settings{Profile: &jobs.Profile{Local: true}}}
	base, err := jobsBaseOf(w)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(base.Roots, " ") != OutputsRoot+"/out "+InputsRoot+"/data "+ContainerState+"/workspace" {
		t.Fatalf("roots: %v", base.Roots)
	}
	if base.Mounts[0] != (jobs.Mount{Source: filepath.Join(state, "env"), Target: ContainerState + "/env", ReadOnly: true}) || len(base.Mounts) != 3 {
		t.Fatalf("mounts: %+v", base.Mounts)
	}
	w.Env = filepath.Join(directory, "env")
	w.Jobs.EnvWritable = false
	base, _ = jobsBaseOf(w)
	readOnly := map[string]bool{}
	for _, mount := range base.Mounts {
		readOnly[mount.Target] = mount.ReadOnly
	}
	if !readOnly[ContainerState+"/env"] || !readOnly[ContainerState+"/env/exports"] || readOnly[ContainerState+"/cache"] {
		t.Fatalf("env folder mounts: %+v", base.Mounts)
	}
	// A read-write folder holding the ledger would let the agent change jobs.
	w.Binds = append(w.Binds, Bind{Source: directory, Target: "/data/output/all"})
	if _, err := jobsBaseOf(w); err == nil || !strings.Contains(err.Error(), "could change jobs") {
		t.Fatalf("ledger in a mount: %v", err)
	}
}

// jobsBaseOf is jobsBase without the image's label, which needs an engine.
func jobsBaseOf(w Worker) (jobs.Base, error) {
	return w.jobsMounts(jobs.Base{Project: w.State})
}
