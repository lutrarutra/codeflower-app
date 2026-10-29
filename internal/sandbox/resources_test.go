package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func environment(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

// fakeQuery answers the scheduler commands the tests expect.
func fakeQuery(t *testing.T, answers map[string]string) {
	original := schedulerQuery
	t.Cleanup(func() { schedulerQuery = original })
	schedulerQuery = func(_ context.Context, name string, _ ...string) (string, error) {
		if answer, ok := answers[name]; ok {
			return answer, nil
		}
		return "", errors.New(name + " not found")
	}
}

func TestSlurmAllocation(t *testing.T) {
	fakeQuery(t, map[string]string{"squeue": "2026-10-29T18:40:00\n"})
	r := DetectResources(context.Background(), environment(map[string]string{
		"SLURM_JOB_ID": "123", "SLURM_CPUS_ON_NODE": "8", "SLURM_MEM_PER_CPU": "4000", "SLURM_JOB_GPUS": "0,1",
	}))
	want := time.Date(2026, 10, 29, 18, 40, 0, 0, time.Local)
	if r.CPUs != 8 || r.Memory != 32000<<20 || r.GPUs != "2" || !r.End.Equal(want) || r.Source != "Slurm job 123" {
		t.Fatalf("unexpected allocation: %+v", r)
	}

	fakeQuery(t, map[string]string{"squeue": "N/A\n"})
	r = DetectResources(context.Background(), environment(map[string]string{
		"SLURM_JOB_ID": "7", "SLURM_CPUS_ON_NODE": "4", "SLURM_MEM_PER_NODE": "16384", "SLURM_JOB_END_TIME": "1800000000",
	}))
	if r.Memory != 16<<30 || !r.End.Equal(time.Unix(1800000000, 0)) {
		t.Fatalf("unexpected allocation: %+v", r)
	}
}

func TestPBSAllocation(t *testing.T) {
	fakeQuery(t, map[string]string{"qstat": "Job Id: 42.server\n" +
		"    Resource_List.mem = 32gb\n    Resource_List.ncpus = 16\n    Resource_List.ngpus = 1\n" +
		"    Resource_List.walltime = 08:00:00\n    stime = Thu Oct 29 10:00:00 2026\n"})
	r := DetectResources(context.Background(), environment(map[string]string{"PBS_JOBID": "42.server"}))
	want := time.Date(2026, 10, 29, 18, 0, 0, 0, time.Local)
	if r.CPUs != 16 || r.Memory != 32<<30 || r.GPUs != "1" || !r.End.Equal(want) {
		t.Fatalf("unexpected allocation: %+v", r)
	}

	fakeQuery(t, nil)
	r = DetectResources(context.Background(), environment(map[string]string{"PBS_JOBID": "1", "PBS_NUM_PPN": "4"}))
	if r.CPUs != 4 || r.Memory != 0 || !r.End.IsZero() {
		t.Fatalf("a failed qstat should leave the environment's figures: %+v", r)
	}
}

func TestOtherSchedulers(t *testing.T) {
	r := DetectResources(context.Background(), environment(map[string]string{"LSB_JOBID": "5", "LSB_DJOB_NUMPROC": "6"}))
	if r.CPUs != 6 || r.Source != "LSF job 5" {
		t.Fatalf("unexpected LSF allocation: %+v", r)
	}
	r = DetectResources(context.Background(), environment(map[string]string{"SGE_ROOT": "/opt/sge", "JOB_ID": "9", "NSLOTS": "12"}))
	if r.CPUs != 12 || r.Source != "Grid Engine job 9" {
		t.Fatalf("unexpected Grid Engine allocation: %+v", r)
	}
	ad := filepath.Join(t.TempDir(), "machine.ad")
	if err := os.WriteFile(ad, []byte("Cpus = 2\nMemory = 4096\nGpus = 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r = DetectResources(context.Background(), environment(map[string]string{"_CONDOR_MACHINE_AD": ad}))
	if r.CPUs != 2 || r.Memory != 4<<30 || r.GPUs != "" {
		t.Fatalf("unexpected HTCondor allocation: %+v", r)
	}
	if r := DetectResources(context.Background(), environment(nil)); !r.Empty() {
		t.Fatalf("no job should mean no allocation: %+v", r)
	}
}

func TestFlagsReplaceTheAllocation(t *testing.T) {
	job := Resources{CPUs: 8, Memory: 32 << 30, Source: "Slurm job 1"}
	r := job.With(Resources{Memory: 16 << 30, Source: "--memory"})
	if r.CPUs != 8 || r.Memory != 16<<30 || r.Source != "Slurm job 1, --memory" {
		t.Fatalf("unexpected merge: %+v", r)
	}
	values := map[string]string{}
	r.environment(values)
	if values["CODEFLOWER_ALLOCATION_CPUS"] != "8" || values["CODEFLOWER_ALLOCATION_MEMORY"] != "17179869184" ||
		values["CODEFLOWER_ALLOCATION_SOURCE"] != "Slurm job 1, --memory" {
		t.Fatalf("unexpected environment: %v", values)
	}
	if _, ok := values["CODEFLOWER_ALLOCATION_END"]; ok {
		t.Fatal("an unknown end time should not be passed")
	}
}

func TestParseMemoryAndTimeLimit(t *testing.T) {
	for text, want := range map[string]int64{"32G": 32 << 30, "512m": 512 << 20, "1.5TiB": 3 << 39, "64GB": 64 << 30} {
		if got, err := ParseMemory(text); err != nil || got != want {
			t.Errorf("ParseMemory(%q) = %d, %v", text, got, err)
		}
	}
	if _, err := ParseMemory("lots"); err == nil {
		t.Error("ParseMemory accepted lots")
	}
	for text, want := range map[string]time.Duration{"8h": 8 * time.Hour, "90m": 90 * time.Minute,
		"1-12:00:00": 36 * time.Hour, "02:30:00": 150 * time.Minute, "45:00": 45 * time.Minute} {
		if got, err := ParseTimeLimit(text); err != nil || got != want {
			t.Errorf("ParseTimeLimit(%q) = %v, %v", text, got, err)
		}
	}
	if _, err := ParseTimeLimit("soon"); err == nil {
		t.Error("ParseTimeLimit accepted soon")
	}
}

func TestEnginesEnforceTheLimits(t *testing.T) {
	dir := tempDir(t)
	args, err := PodmanArgs("podman", PodmanRun{Name: "x", Image: "i", State: dir, CPUs: 4, Memory: 8 << 30})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(args, "--cpus") || !slices.Contains(args, "8589934592") {
		t.Fatalf("podman is not limited: %v", args)
	}
	_, host, err := DockerConfig(DockerRun{Image: "i", State: dir, CPUs: 4, Memory: 8 << 30})
	if err != nil {
		t.Fatal(err)
	}
	if host.NanoCPUs != 4e9 || host.Memory != 8<<30 {
		t.Fatalf("docker is not limited: %+v", host.Resources)
	}
}
