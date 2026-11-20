package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"codeflower-app/internal/config"
)

func TestBuiltinProfilesCompile(t *testing.T) {
	for _, name := range Builtin {
		profile, err := LoadProfile(nil, name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if profile.Local != (name == "local") || profile.Example != slices.Contains(Examples, name) {
			t.Fatalf("%s: local %v, example %v", name, profile.Local, profile.Example)
		}
	}
	if _, err := LoadProfile(nil, "nope"); err == nil || !strings.Contains(err.Error(), "unknown scheduler") {
		t.Fatalf("unknown scheduler: %v", err)
	}
}

func sampleData() ScriptData {
	array, _ := ParseArray("1-10%2", 0)
	return ScriptData{JobID: "j-abc123", Name: "align", CPUs: 4, Memory: 16 << 30, Time: 26*time.Hour + 30*time.Minute,
		GPUs: 1, GPUType: "l4", Queue: "shortq", Account: "lab", Array: array, After: []string{"11", "12"},
		Extra: []string{"--export=NONE"}, Setup: "module load apptainer", Run: "/home/u/codeflower job-run /p.jobs/j-abc123/spec.json",
		Directory: "/p.jobs/j-abc123", Script: "/p.jobs/j-abc123/job.sh", SubmitFile: "/p.jobs/j-abc123/job.sub",
		LogDir: "/p/jobs/j-abc123", Output: "/p/jobs/j-abc123/scheduler.log", User: "me"}
}

func TestSlurmScript(t *testing.T) {
	profile, err := LoadProfile(nil, "slurm")
	if err != nil {
		t.Fatal(err)
	}
	script, err := render(profile.script, sampleData())
	if err != nil {
		t.Fatal(err)
	}
	want := `#!/bin/bash
#SBATCH --job-name=align
#SBATCH --output=/p/jobs/j-abc123/scheduler-%a.log
#SBATCH --nodes=1
#SBATCH --ntasks=1
#SBATCH --cpus-per-task=4
#SBATCH --mem=16384M
#SBATCH --time=1-02:30:00
#SBATCH --partition=shortq
#SBATCH --account=lab
#SBATCH --gres=gpu:l4:1
#SBATCH --array=1-10%2
#SBATCH --dependency=afterok:11:12
#SBATCH --export=NONE
module load apptainer
exec /home/u/codeflower job-run /p.jobs/j-abc123/spec.json
`
	if script != want {
		t.Fatalf("script:\n%s\nwant:\n%s", script, want)
	}
	plain := ScriptData{Name: "x", CPUs: 1, Memory: 1 << 30, Time: time.Hour, Run: "run", LogDir: "/l"}
	script, err = render(profile.script, plain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(script, "partition") || strings.Contains(script, "array") || !strings.HasSuffix(script, "\nexec run\n") ||
		!strings.Contains(script, "--output=/l/scheduler.log\n") {
		t.Fatalf("plain script:\n%s", script)
	}
}

func TestExampleProfilesRender(t *testing.T) {
	for _, name := range append([]string{"local"}, Examples...) {
		profile, err := LoadProfile(nil, name)
		if err != nil {
			t.Fatal(err)
		}
		data := sampleData()
		data.Array, _ = ParseArray("1-9:2%3", 0)
		script, err := render(profile.script, data)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(script, data.Run) {
			t.Fatalf("%s does not run the job:\n%s", name, script)
		}
		if profile.submitFile != nil {
			description, err := render(profile.submitFile, data)
			if err != nil {
				t.Fatalf("%s submit file: %v", name, err)
			}
			if !strings.Contains(description, "queue Item in (1 3 5 7 9)") || !strings.Contains(description, "executable = "+data.Script) {
				t.Fatalf("%s submit file:\n%s", name, description)
			}
		}
	}
	pbs, _ := LoadProfile(nil, "pbs")
	data := sampleData()
	data.Array, _ = ParseArray("1,5", 0)
	if _, err := render(pbs.script, data); err == nil || !strings.Contains(templateError(err).Error(), "PBS arrays must be one range") {
		t.Fatalf("PBS with a list: %v", err)
	}
	local, _ := LoadProfile(nil, "local")
	script, _ := render(local.script, sampleData())
	if !strings.Contains(script, "printf '%s\\n' 1 2 3 4 5 6 7 8 9 10 | xargs -P 2 -I {} env CODEFLOWER_JOB_INDEX={} ") {
		t.Fatalf("local array:\n%s", script)
	}
}

func TestCategory(t *testing.T) {
	profile, _ := LoadProfile(nil, "slurm")
	for state, want := range map[string]string{
		"PENDING": StateQueued, "RUNNING": StateRunning, "COMPLETED": StateDone, "CANCELLED": StateCancelled,
		"CANCELLED+": StateCancelled, "REQUEUE_HOLD": StateQueued, "TIMEOUT": StateFailed, "WHATEVER": StateFailed,
	} {
		if got := profile.Category(state); got != want {
			t.Errorf("%s: %s, want %s", state, got, want)
		}
	}
}

func TestExtends(t *testing.T) {
	cfg := &config.Config{Jobs: config.Jobs{Schedulers: map[string]config.SchedulerProfile{
		"cluster": {Extends: "slurm", Script: "#!/bin/sh\n#SBATCH --qos={{.Queue}}\nexec {{.Run}}\n", Supports: []string{"queue"}},
		"loop":    {Extends: "loop2"},
		"loop2":   {Extends: "loop"},
		"slurm":   {Extends: "slurm", PassEnv: []string{"SLURM_JOB_ID"}},
	}}}
	profile, err := LoadProfile(cfg, "cluster")
	if err != nil {
		t.Fatal(err)
	}
	if !profile.status.set() || profile.Supported("array") || profile.Category("PENDING") != StateQueued {
		t.Fatalf("extended profile: %+v", profile)
	}
	if _, err := LoadProfile(cfg, "loop"); err == nil {
		t.Fatal("a circle of extends must fail")
	}
	slurm, err := LoadProfile(cfg, "slurm")
	if err != nil || !slices.Equal(slurm.PassEnv, []string{"SLURM_JOB_ID"}) || !slurm.Supported("array") {
		t.Fatalf("slurm extending itself: %v %+v", err, slurm)
	}
	bad := &config.Config{Jobs: config.Jobs{Schedulers: map[string]config.SchedulerProfile{
		"bad": {Script: "{{.Nope}}", Submit: config.ProfileCommand{Command: []string{"x"}},
			Status: config.ProfileCommand{Command: []string{"x"}, Line: "(?P<id>.)"}, Cancel: config.ProfileCommand{Command: []string{"x"}},
			States: map[string][]string{"done": {"x"}}},
	}}}
	if _, err := LoadProfile(bad, "bad"); err == nil || !strings.Contains(err.Error(), "state") {
		t.Fatalf("a line without a state group: %v", err)
	}
}

func TestParseArray(t *testing.T) {
	array, err := ParseArray("1-5,9,3:2", 0)
	if err == nil {
		t.Fatalf("a step without a range: %+v", array)
	}
	array, err = ParseArray("0-8:4,10%3", 0)
	if err != nil || !slices.Equal(array.Indices, []int{0, 4, 8, 10}) || array.Limit != 3 || array.Simple || array.Spec != "0-8:4,10" {
		t.Fatalf("%+v %v", array, err)
	}
	array, err = ParseArray("2-6:2", 0)
	if err != nil || !array.Simple || array.First != 2 || array.Last != 6 || array.Step != 2 || array.Count != 3 {
		t.Fatalf("%+v %v", array, err)
	}
	var requestError *RequestError
	if _, err := ParseArray("1-1000", 100); !errors.As(err, &requestError) || requestError.Code != CodeLimit {
		t.Fatalf("over the limit: %v", err)
	}
	for _, bad := range []string{"", "a", "5-1", "1-2%0", "-1", "1-"} {
		if _, err := ParseArray(bad, 0); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}

func TestParseDurationAndMemory(t *testing.T) {
	for text, want := range map[string]time.Duration{
		"90m": 90 * time.Minute, "2h": 2 * time.Hour, "3d": 72 * time.Hour, "1-12:00:00": 36 * time.Hour,
		"12:30:00": 12*time.Hour + 30*time.Minute, "45": 45 * time.Minute, "1-2": 26 * time.Hour, "10:30": 10*time.Minute + 30*time.Second,
	} {
		if got, err := ParseDuration(text); err != nil || got != want {
			t.Errorf("%s: %v %v", text, got, err)
		}
	}
	for _, bad := range []string{"", "0", "x", "-1h", "400d"} {
		if _, err := ParseDuration(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
	for text, want := range map[string]int64{"16G": 16 << 30, "512M": 512 << 20, "1.5T": 3 << 39, "2048": 2048 << 20, "4GiB": 4 << 30} {
		if got, err := ParseMemory(text); err != nil || got != want {
			t.Errorf("%s: %v %v", text, got, err)
		}
	}
	for d, want := range map[time.Duration]string{time.Hour: "1h", 90 * time.Minute: "1h30m", 30 * time.Minute: "30m", 72 * time.Hour: "3d"} {
		if got := FormatDuration(d); got != want {
			t.Errorf("%v: %s, want %s", d, got, want)
		}
	}
}

func testPolicy(t *testing.T) Policy {
	policy, err := policy(config.Jobs{
		Defaults: config.JobDefaults{Queue: "shortq"},
		Limits:   config.JobLimits{CPUs: 32, Memory: "128G", Time: "3d", GPUs: 2, Active: 3, Array: 50},
		Queues: []config.JobQueue{{Name: "shortq", Time: "12h"}, {Name: "gpu", GPUs: 2, GPUTypes: []string{"l4"}},
			{Name: "tinyq", Time: "2h", CPUs: 8}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func TestCheck(t *testing.T) {
	policy := testPolicy(t)
	slurm, _ := LoadProfile(nil, "slurm")
	roots := []string{"/data/input/a", "/data/output/b", "/codeflower-state/workspace"}
	good := Request{Command: []string{"/data/output/b/run.sh", "x"}, Workdir: "/data/output/b"}
	job, err := policy.Check(good, slurm, roots, 0)
	if err != nil {
		t.Fatal(err)
	}
	if job.Name != "run.sh" || job.Resources.CPUs != 1 || job.Resources.Queue != "shortq" || job.Resources.Time != time.Hour {
		t.Fatalf("defaults: %+v", job)
	}
	with := func(change func(*Request)) Request {
		request := good
		request.Command = slices.Clone(good.Command)
		change(&request)
		return request
	}
	cases := []struct {
		name    string
		request Request
		active  int
		code    string
		message string
	}{
		{"too many active", good, 3, CodeLimit, "jobs.limits.active"},
		{"no command", with(func(r *Request) { r.Command = nil }), 0, CodeInvalid, "needs a command"},
		{"command outside", with(func(r *Request) { r.Command = []string{"/etc/passwd"} }), 0, CodeInvalid, "not in a folder"},
		{"command escapes", with(func(r *Request) { r.Command = []string{"/data/output/b/../../../etc/x"} }), 0, CodeInvalid, "not in a folder"},
		{"odd program", with(func(r *Request) { r.Command = []string{"rm -rf"} }), 0, CodeInvalid, "program on PATH"},
		{"workdir outside", with(func(r *Request) { r.Workdir = "/tmp" }), 0, CodeInvalid, "--workdir"},
		{"bad name", with(func(r *Request) { r.Name = "a\n#SBATCH --uid=0" }), 0, CodeInvalid, "name"},
		{"unknown queue", with(func(r *Request) { r.Queue = "longq" }), 0, CodeInvalid, "not one jobs may use"},
		{"queue injection", with(func(r *Request) { r.Queue = "x --wrap" }), 0, CodeInvalid, "queue name"},
		{"too many cpus", with(func(r *Request) { r.CPUs = 64 }), 0, CodeLimit, "jobs.limits.cpus"},
		{"queue cpus", with(func(r *Request) { r.Queue, r.CPUs = "tinyq", 16 }), 0, CodeLimit, "tinyq"},
		{"queue time", with(func(r *Request) { r.Time = "1d" }), 0, CodeLimit, "shortq"},
		{"bad memory", with(func(r *Request) { r.Memory = "lots" }), 0, CodeInvalid, "memory"},
		{"no gpus on queue", with(func(r *Request) { r.GPUs = 1 }), 0, CodeLimit, "no GPUs"},
		{"gpu type", with(func(r *Request) { r.Queue, r.GPUs, r.GPUType = "gpu", 1, "h100" }), 0, CodeInvalid, "type l4"},
		{"array too big", with(func(r *Request) { r.Array = "1-100" }), 0, CodeLimit, "limit"},
		{"bad after", with(func(r *Request) { r.After = []string{"1;rm"} }), 0, CodeInvalid, "job id"},
		{"reserved env", with(func(r *Request) { r.Env = map[string]string{"CODEFLOWER_JOB_ID": "x"} }), 0, CodeInvalid, "set by codeflower"},
		{"scheduler env", with(func(r *Request) { r.Env = map[string]string{"SLURM_JOB_ID": "x"} }), 0, CodeInvalid, "scheduler"},
		{"env name", with(func(r *Request) { r.Env = map[string]string{"A-B": "x"} }), 0, CodeInvalid, "variable name"},
	}
	for _, c := range cases {
		_, err := policy.Check(c.request, slurm, roots, c.active)
		var requestError *RequestError
		if !errors.As(err, &requestError) || requestError.Code != c.code || !strings.Contains(requestError.Message, c.message) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	job, err = policy.Check(with(func(r *Request) {
		r.Queue, r.GPUs, r.GPUType, r.Array, r.Command = "gpu", 2, "l4", "1-50%5", []string{"python", "-c", "print(1)"}
	}), slurm, roots, 0)
	if err != nil || job.Array.Count != 50 || job.Name != "python" {
		t.Fatalf("gpu array: %+v %v", job, err)
	}
	local, _ := LoadProfile(nil, "local")
	if _, err := policy.Check(with(func(r *Request) { r.After = []string{"j-123456"} }), local, roots, 0); err == nil {
		t.Fatal("local jobs cannot wait for others")
	}
}

// fakeScheduler stands in for Slurm's commands.
type fakeScheduler struct {
	next     int
	queue    map[string]string
	history  string
	calls    []string
	scripts  []string
	failNext error
}

func (f *fakeScheduler) run(_ context.Context, args []string, _ []byte) ([]byte, []byte, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	if f.failNext != nil {
		err := f.failNext
		f.failNext = nil
		return nil, []byte("sbatch: error: Batch job submission failed: Invalid partition"), err
	}
	switch args[0] {
	case "sbatch":
		f.next++
		id := fmt.Sprint(100 + f.next)
		f.queue[id] = "PENDING"
		script, _ := os.ReadFile(args[len(args)-1])
		f.scripts = append(f.scripts, string(script))
		return []byte(id + "\n"), nil, nil
	case "squeue":
		var lines []string
		for id, state := range f.queue {
			lines = append(lines, id+"|"+state)
		}
		return []byte(strings.Join(lines, "\n") + "\n"), nil, nil
	case "sacct":
		return []byte(f.history), nil, nil
	case "scancel":
		delete(f.queue, args[1])
		return nil, nil, nil
	}
	return nil, nil, fmt.Errorf("unknown command %s", args[0])
}

func newTestBroker(t *testing.T, cfg config.Jobs) (*Broker, *fakeScheduler, string) {
	t.Helper()
	fake := &fakeScheduler{queue: map[string]string{}}
	previous := runCommand
	runCommand = fake.run
	t.Cleanup(func() { runCommand = previous })
	state := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if cfg.Scheduler == "" {
		cfg.Scheduler = "slurm"
	}
	cfg.Codeflower = "/opt/codeflower"
	settings, err := Setup(&config.Config{Jobs: cfg}, "")
	if err != nil {
		t.Fatal(err)
	}
	base := Base{Project: state, Instance: "instance-1", Engine: "apptainer", Image: "/images/sandbox.sif",
		Mounts: []Mount{{Source: "/host/out", Target: "/data/output/out"}}, Roots: []string{"/data/output/out"}}
	broker, err := NewBroker(settings, base, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	return broker, fake, state
}

func TestBrokerSubmitPollFinish(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("batch jobs need Linux or macOS")
	}
	broker, fake, state := newTestBroker(t, config.Jobs{Extra: []string{"--export=NONE"}, Setup: "module load apptainer"})
	ctx := context.Background()
	record, err := broker.Submit(ctx, Request{Command: []string{"/data/output/out/run.sh"}, Workdir: "/data/output/out", Agent: "claude", SessionID: "s1", Memory: "8G"})
	if err != nil {
		t.Fatal(err)
	}
	if record.SchedulerID != "101" || record.State != StateQueued || !record.Notify {
		t.Fatalf("record: %+v", record)
	}
	if !strings.Contains(fake.scripts[0], "#SBATCH --export=NONE") || !strings.Contains(fake.scripts[0], "module load apptainer\nexec /opt/codeflower job-run "+broker.Ledger.JobDir(record.ID)+"/spec.json") {
		t.Fatalf("script:\n%s", fake.scripts[0])
	}
	spec, err := ReadSpec(filepath.Join(broker.Ledger.JobDir(record.ID), SpecFile))
	if err != nil || spec.Image != "/images/sandbox.sif" || spec.Logs != filepath.Join(state, "jobs", record.ID) || len(spec.Mounts) != 1 {
		t.Fatalf("spec: %+v %v", spec, err)
	}
	public, err := os.ReadFile(filepath.Join(state, "jobs", record.ID, RecordFile))
	if err != nil || !strings.Contains(string(public), `"schedulerId": "101"`) {
		t.Fatalf("the project's copy: %s %v", public, err)
	}

	fake.queue["101"] = "RUNNING"
	broker.Poll(ctx)
	if loaded, _ := broker.Ledger.Load(record.ID); loaded.State != StateRunning || loaded.Started == nil {
		t.Fatalf("running: %+v", loaded)
	}
	// It ends; its sandbox wrote a result.
	delete(fake.queue, "101")
	if err := WriteResult(broker.Ledger.JobDir(record.ID), Result{ExitCode: 3, Started: time.Now(), Ended: time.Now()}); err != nil {
		t.Fatal(err)
	}
	broker.Poll(ctx)
	loaded, _ := broker.Ledger.Load(record.ID)
	if loaded.State != StateFailed || loaded.ExitCode == nil || *loaded.ExitCode != 3 || loaded.Ended == nil {
		t.Fatalf("failed: %+v", loaded)
	}
	for _, call := range fake.calls {
		if strings.HasPrefix(call, "sacct") {
			t.Fatalf("a job with its result needs no history: %v", fake.calls)
		}
	}
}

func TestBrokerArrayHistoryAndLost(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("batch jobs need Linux or macOS")
	}
	broker, fake, _ := newTestBroker(t, config.Jobs{})
	ctx := context.Background()
	array, err := broker.Submit(ctx, Request{Command: []string{"python"}, Workdir: "/data/output/out", Array: "1-3"})
	if err != nil {
		t.Fatal(err)
	}
	single, err := broker.Submit(ctx, Request{Command: []string{"python"}, Workdir: "/data/output/out", After: []string{array.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fake.scripts[1], "--dependency=afterok:101") {
		t.Fatalf("after:\n%s", fake.scripts[1])
	}
	fake.queue = map[string]string{}
	one, two := 1, 2
	WriteResult(broker.Ledger.JobDir(array.ID), Result{Index: &one, ExitCode: 0})
	WriteResult(broker.Ledger.JobDir(array.ID), Result{Index: &two, ExitCode: 0})
	fake.history = "101_3|TIMEOUT|0\n"
	broker.Poll(ctx)
	loaded, _ := broker.Ledger.Load(array.ID)
	if loaded.State != StateFailed || loaded.Outcome == nil || loaded.Outcome.Done != 2 || loaded.Outcome.Failed != 1 {
		t.Fatalf("array: %+v %+v", loaded, loaded.Outcome)
	}
	// The single job left no result and the history does not know it: lost,
	// after a few polls.
	for range missingPolls - 1 {
		broker.Poll(ctx)
		if loaded, _ := broker.Ledger.Load(single.ID); loaded.State != StateQueued {
			t.Fatalf("lost too early: %+v", loaded)
		}
	}
	broker.Poll(ctx)
	if loaded, _ := broker.Ledger.Load(single.ID); loaded.State != StateLost {
		t.Fatalf("lost: %+v", loaded)
	}
	if _, err := broker.Submit(ctx, Request{Command: []string{"python"}, Workdir: "/data/output/out", After: []string{single.ID}}); err == nil {
		t.Fatal("waiting for a lost job must be refused")
	}
}

func TestBrokerSpoolAndCancel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("batch jobs need Linux or macOS")
	}
	broker, fake, _ := newTestBroker(t, config.Jobs{Limits: config.JobLimits{Active: 1}})
	ctx := context.Background()
	requests := filepath.Join(broker.spool, spoolRequests)
	write := func(name string, request Request) Response {
		data, _ := json.Marshal(request)
		if err := os.WriteFile(filepath.Join(requests, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
		broker.handleRequests(ctx)
		answer, err := os.ReadFile(filepath.Join(broker.spool, spoolResponses, name))
		if err != nil {
			t.Fatal(err)
		}
		var response Response
		json.Unmarshal(answer, &response)
		return response
	}
	response := write("r-1.json", Request{ID: "r-1", Action: ActionSubmit, Command: []string{"python"}, Workdir: "/data/output/out"})
	if !response.OK || response.Job == nil || response.Job.SchedulerID != "101" {
		t.Fatalf("submit: %+v", response)
	}
	if response := write("r-2.json", Request{Action: ActionSubmit, Command: []string{"python"}, Workdir: "/data/output/out"}); response.OK || response.Code != CodeLimit {
		t.Fatalf("over the active limit: %+v", response)
	}
	if response := write("r-3.json", Request{Action: ActionCancel, Job: "101"}); !response.OK || response.Job.State != StateCancelled {
		t.Fatalf("cancel by scheduler id: %+v", response)
	}
	if _, queued := fake.queue["101"]; queued {
		t.Fatal("scancel was not run")
	}
	if response := write("r-4.json", Request{Action: ActionCancel, Job: "j-ffffff"}); response.OK || response.Code != CodeUnknown {
		t.Fatalf("cancel an unknown job: %+v", response)
	}
	fake.failNext = errors.New("exit status 1")
	response = write("r-5.json", Request{Action: ActionSubmit, Command: []string{"python"}, Workdir: "/data/output/out"})
	if response.OK || response.Code != CodeScheduler || !strings.Contains(response.Error, "Invalid partition") || response.Job == nil || response.Job.State != StateFailed {
		t.Fatalf("refused by the scheduler: %+v", response)
	}
	os.WriteFile(filepath.Join(requests, "bad name.json"), []byte("{}"), 0o600)
	os.WriteFile(filepath.Join(requests, "r-6.json"), []byte("not json"), 0o600)
	broker.handleRequests(ctx)
	if answer, _ := os.ReadFile(filepath.Join(broker.spool, spoolResponses, "r-6.json")); !strings.Contains(string(answer), CodeInvalid) {
		t.Fatalf("not JSON: %s", answer)
	}
	if _, err := os.Stat(filepath.Join(broker.spool, spoolResponses, "bad name.json")); err == nil {
		t.Fatal("a request with an odd name must be ignored")
	}
}

func TestBrokerRefusesUnsafeProject(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("batch jobs need Linux or macOS")
	}
	settings, err := Setup(&config.Config{Jobs: config.Jobs{Scheduler: "local", Codeflower: "/x"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewBroker(settings, Base{Project: filepath.Join(t.TempDir(), "my project")}, nil); err == nil {
		t.Fatal("a project path with a space must be refused")
	}
}

func TestSetupDetect(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("batch jobs need Linux or macOS")
	}
	previous := lookPath
	t.Cleanup(func() { lookPath = previous })
	lookPath = func(name string) (string, error) {
		if name == "bsub" || name == "bjobs" {
			return "/usr/bin/" + name, nil
		}
		return "", errors.New("not found")
	}
	settings, err := Setup(&config.Config{Jobs: config.Jobs{Scheduler: "auto", Codeflower: "/x"}}, "")
	if err != nil || settings == nil || settings.Scheduler != "lsf" {
		t.Fatalf("auto: %+v %v", settings, err)
	}
	if settings, err := Setup(&config.Config{Jobs: config.Jobs{Scheduler: "slurm"}}, "off"); settings != nil || err != nil {
		t.Fatalf("--jobs off: %v %v", settings, err)
	}
	if _, err := Setup(&config.Config{Jobs: config.Jobs{Scheduler: "local", Queues: []config.JobQueue{{Name: "q"}}}}, ""); err == nil {
		t.Fatal("local has no queues")
	}
	info := settingsInfo(t)
	if info.Scheduler != "slurm" || info.Active != DefaultActive || info.Defaults.Memory != "4G" || info.Defaults.Time != "1h" {
		t.Fatalf("info: %+v", info)
	}
}

func settingsInfo(t *testing.T) Info {
	settings, err := Setup(&config.Config{Jobs: config.Jobs{Scheduler: "slurm", Codeflower: "/x"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	var info Info
	if err := json.Unmarshal([]byte(settings.InfoJSON()), &info); err != nil {
		t.Fatal(err)
	}
	return info
}

// The local backend, end to end: a fake app writes the job's result, as
// job-run does.
func TestLocalJobs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("local jobs need Linux or macOS")
	}
	directory := t.TempDir()
	app := filepath.Join(directory, "codeflower")
	script := `#!/bin/sh
spec="$2"
dir=$(dirname "$spec")
index="${CODEFLOWER_JOB_INDEX:-}"
if [ -n "$index" ]; then
  printf '{"index": %s, "exitCode": %s}' "$index" "$((index % 2))" > "$dir/result-$index.json"
else
  sleep "${SLEEP:-0}"
  printf '{"exitCode": 0}' > "$dir/result.json"
fi
`
	if err := os.WriteFile(app, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(directory, "project")
	os.MkdirAll(state, 0o700)
	settings, err := Setup(&config.Config{Jobs: config.Jobs{Scheduler: "local", Codeflower: app, Setup: "export SLEEP=30"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	broker, err := NewBroker(settings, Base{Project: state, Instance: "i", Roots: []string{"/data/output/out"}}, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	array, err := broker.Submit(ctx, Request{Command: []string{"python"}, Workdir: "/data/output/out", Array: "1-4%2"})
	if err != nil {
		t.Fatal(err)
	}
	slow, err := broker.Submit(ctx, Request{Command: []string{"python"}, Workdir: "/data/output/out"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		broker.Poll(ctx)
		loaded, _ := broker.Ledger.Load(array.ID)
		if !Active(loaded.State) {
			if loaded.State != StateFailed || loaded.Outcome.Done != 2 || loaded.Outcome.Failed != 2 {
				t.Fatalf("array: %+v %+v", loaded, loaded.Outcome)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the array did not end: %+v", loaded)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if loaded, _ := broker.Ledger.Load(slow.ID); loaded.State != StateRunning {
		t.Fatalf("the slow job: %+v", loaded)
	}
	if record, err := broker.Cancel(ctx, slow.ID); err != nil || record.State != StateCancelled {
		t.Fatalf("cancel: %+v %v", record, err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for (localBackend{}).running(&Record{ID: slow.ID, PID: slow.PID}) {
		if time.Now().After(deadline) {
			t.Fatal("the cancelled job still runs")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
