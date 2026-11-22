package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"codeflower-app/internal/jobs"
)

// Batch jobs (docs/jobs.md): the agent asks for one with codeflower-job, the
// bridge writes the request into the project's spool, the worker's broker
// submits it, and the batch script runs `codeflower job-run`, which starts a
// sandbox of its own with the image, folders and network of the one that
// asked, and the job's command instead of the bridge.
const (
	// JobEntrypoint runs a job's command (sandbox/job.sh).
	JobEntrypoint = "/opt/codeflower/job.sh"
	// JobsLabel marks an image that has JobEntrypoint and codeflower-job.
	JobsLabel = "codeflower.jobs"
	// JobContainerPrefix names the jobs' containers.
	JobContainerPrefix = "codeflower-job-"
)

// startJobs prepares the worker's jobs, or says why it has none; the broker
// runs until the returned stop is called.
func (w Worker) startJobs(ctx context.Context, instanceID string, values map[string]string) (stop func(), err error) {
	base, err := w.jobsBase(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	log := func(message string) { fmt.Fprintln(w.Stderr, message) }
	broker, err := jobs.NewBroker(w.Jobs, base, log)
	if err != nil {
		return nil, err
	}
	values[jobs.InfoVariable] = w.Jobs.InfoJSON()
	values[jobs.SpoolVariable] = jobs.ContainerSpool + "/" + instanceID
	brokerCtx, cancel := context.WithCancel(context.Background())
	var group sync.WaitGroup
	group.Add(1)
	go func() {
		defer group.Done()
		broker.Run(brokerCtx)
	}()
	if base.ImageArchive != "" {
		group.Add(1)
		go func() {
			defer group.Done()
			saveImageArchive(brokerCtx, w.Engine, w.Image, base.ImageArchive, log)
		}()
	}
	return func() {
		cancel()
		group.Wait()
	}, nil
}

// jobsBase is what each of the worker's jobs shares.
func (w Worker) jobsBase(ctx context.Context, instanceID string) (jobs.Base, error) {
	base := jobs.Base{Project: w.State, Instance: instanceID, Engine: w.Engine.Name, Image: w.Image}
	labels, err := imageLabels(ctx, w.Engine, w.Image)
	if err != nil || labels[JobsLabel] != "1" {
		return base, fmt.Errorf("the sandbox image is older than batch jobs; update it with `codeflower pull`")
	}
	if engine := w.Jobs.Engine; engine != "" && engine != w.Engine.Name {
		image, err := imageFor(w.Image, w.Engine.Name, engine)
		if err != nil {
			return base, err
		}
		base.Engine, base.Image = engine, image
	}
	if base.Engine == EnginePodman && !w.Jobs.Profile.Local {
		// A job's node may keep Podman's images apart from this one's (per
		// job, on clusters): it loads this copy when it has no image.
		if identity, err := IdentifyImage(ctx, w.Engine, w.Image); err == nil && identity.ID != "" {
			id := strings.TrimPrefix(identity.ID, "sha256:")
			if len(id) > 12 {
				id = id[:12]
			}
			base.ImageArchive = filepath.Join(ImageDirectory(), "codeflower-sandbox-"+id+".tar")
		}
	}
	return w.jobsMounts(base)
}

// jobsMounts adds the folders the jobs' sandboxes mount, and those their
// commands may be in.
func (w Worker) jobsMounts(base jobs.Base) (jobs.Base, error) {
	ledger := jobs.LedgerDir(w.State)
	for _, bind := range w.Binds {
		base.Roots = append(base.Roots, bind.Target)
		if !bind.ReadOnly && (ledger == bind.Source || strings.HasPrefix(ledger, bind.Source+string(filepath.Separator))) {
			return base, fmt.Errorf("the project's jobs folder %s is inside the read-write folder %s, where the agent could change jobs", ledger, bind.Source)
		}
	}
	base.Roots = append(base.Roots, ContainerState+"/workspace")
	for _, bind := range EnvBinds(w.Env, w.State) {
		if !w.Jobs.EnvWritable && (bind.Target == ContainerState+"/env" || bind.Target == ContainerState+"/env/exports") {
			bind.ReadOnly = true
		}
		base.Mounts = append(base.Mounts, jobs.Mount{Source: bind.Source, Target: bind.Target, ReadOnly: bind.ReadOnly})
	}
	if w.Env == "" && !w.Jobs.EnvWritable {
		base.Mounts = append(base.Mounts, jobs.Mount{Source: filepath.Join(w.State, "env"), Target: ContainerState + "/env", ReadOnly: true})
	}
	for _, bind := range w.Binds {
		base.Mounts = append(base.Mounts, jobs.Mount{Source: bind.Source, Target: bind.Target, ReadOnly: bind.ReadOnly})
	}
	if w.Isolation != nil {
		base.Isolated, base.Allowlist = true, w.Isolation.Allowlist
	}
	return base, nil
}

// imageFor is the image a sandbox runs as another engine needs it: only the
// published image has a form for each.
func imageFor(image, from, to string) (string, error) {
	tag, ok := publishedTag(image)
	if !ok && from == EngineApptainer {
		tag, ok = sifTag(image)
	}
	if !ok {
		return "", fmt.Errorf("jobs.engine %s needs the published image; this sandbox runs %s", to, image)
	}
	resolved, err := ResolveImage(ImageRepository+":"+tag, to)
	if err != nil {
		return "", err
	}
	if to == EngineApptainer {
		if _, err := os.Stat(resolved); err != nil {
			return "", fmt.Errorf("jobs.engine apptainer needs the image file %s: run `codeflower pull --engine apptainer` once", resolved)
		}
	}
	return resolved, nil
}

var sifName = regexp.MustCompile(`codeflower-sandbox_v?([0-9]+\.[0-9]+\.[0-9]+)`)

func sifTag(path string) (string, bool) {
	match := sifName.FindStringSubmatch(filepath.Base(path))
	if match == nil {
		return "", false
	}
	return match[1], true
}

// saveImageArchive saves the Podman image for the jobs' nodes once.
func saveImageArchive(ctx context.Context, engine EngineStatus, image, archive string, log func(string)) {
	if _, err := os.Stat(archive); err == nil {
		return
	}
	log(fmt.Sprintf("jobs: saving the image to %s for the jobs' nodes (once)…", archive))
	if err := saveArchive(ctx, engine, image, archive); err != nil && ctx.Err() == nil {
		log(fmt.Sprintf("jobs: could not save the image: %v", err))
	}
}

// RunJob runs a job's task in a sandbox of its own (`codeflower job-run`),
// writes its result into the job's ledger folder, and returns its exit code.
// What it says goes to stderr, the scheduler's log of the job; the command's
// own output goes to the job's output file in the project.
func RunJob(ctx context.Context, spec jobs.Spec, stderr io.Writer) (int, error) {
	index, err := jobIndex(spec, os.Getenv)
	if err != nil {
		return 2, err
	}
	node, _ := os.Hostname()
	result := jobs.Result{Index: index, Started: time.Now().UTC(), Node: node}
	code, err := runJob(ctx, spec, index, stderr)
	result.Ended = time.Now().UTC()
	result.ExitCode = code
	if err != nil {
		result.Error = err.Error()
		if code == 0 {
			result.ExitCode = 1
		}
	}
	if writeErr := jobs.WriteResult(spec.Ledger, result); writeErr != nil {
		fmt.Fprintf(stderr, "codeflower job-run: could not write the result: %v\n", writeErr)
	}
	return result.ExitCode, err
}

// jobIndex is the array task's index from the scheduler's variable, or nil
// for a job that is no array.
func jobIndex(spec jobs.Spec, getenv func(string) string) (*int, error) {
	if len(spec.Indices) == 0 {
		return nil, nil
	}
	for _, name := range spec.IndexEnv {
		if value := getenv(name); value != "" {
			index, err := strconv.Atoi(value)
			if err != nil || !slices.Contains(spec.Indices, index) {
				return nil, fmt.Errorf("%s=%s is not one of the job's array indices", name, value)
			}
			return &index, nil
		}
	}
	return nil, fmt.Errorf("the job is an array, but none of %s is set", strings.Join(spec.IndexEnv, ", "))
}

func runJob(ctx context.Context, spec jobs.Spec, index *int, stderr io.Writer) (int, error) {
	log := func(message string) { fmt.Fprintln(stderr, "codeflower job-run: "+message) }
	var missing []string
	for _, path := range []string{spec.Project, spec.Ledger, spec.Logs} {
		if _, err := os.Stat(path); err != nil {
			missing = append(missing, path)
		}
	}
	binds := make([]Bind, 0, len(spec.Mounts))
	for _, mount := range spec.Mounts {
		if !filepath.IsAbs(mount.Source) || !strings.HasPrefix(mount.Target, "/") ||
			mount.Target == ContainerNet || strings.HasPrefix(mount.Target, ContainerNet+"/") {
			return 2, fmt.Errorf("the job's spec mounts %s at %s, which is not allowed", mount.Source, mount.Target)
		}
		if _, err := os.Stat(mount.Source); err != nil {
			missing = append(missing, mount.Source)
		}
		binds = append(binds, Bind{Source: mount.Source, Target: mount.Target, ReadOnly: mount.ReadOnly})
	}
	if len(missing) > 0 {
		return 2, fmt.Errorf("this node does not have %s: the project, its folders and env_dir must be on filesystems every node shares",
			strings.Join(missing, ", "))
	}
	engine, err := ChooseEngine(DetectEngines(ctx), spec.Engine, spec.Image)
	if err != nil {
		return 3, fmt.Errorf("%s cannot run the job here: %w (jobs.setup can load it, e.g. module load %s)", spec.Engine, err, spec.Engine)
	}
	if err := jobImage(ctx, engine, spec, log); err != nil {
		return 3, err
	}
	resources := DetectResources(ctx, os.Getenv)

	values := ContainerEnvironment()
	values["CODEFLOWER_ENGINE"] = engine.Name
	resources.environment(values)
	for _, name := range spec.PassEnv {
		if value, ok := os.LookupEnv(name); ok {
			values[name] = value
		}
	}
	for name, value := range spec.Env {
		values[name] = value
	}
	values["CODEFLOWER_JOB_ID"] = spec.ID
	values["CODEFLOWER_JOB_NAME"] = spec.Name
	values["CODEFLOWER_JOB_WORKDIR"] = spec.Workdir
	if index != nil {
		values["CODEFLOWER_JOB_INDEX"] = strconv.Itoa(*index)
	}
	for _, bind := range binds {
		if bind.Target == ContainerState+"/env" {
			// env/ and cache/ are separate mounts then, which hard links cannot cross.
			values["UV_LINK_MODE"] = "copy"
		}
	}

	engineArgs, err := ParseEngineArgs(spec.EngineArgs)
	if err != nil {
		return 2, fmt.Errorf("jobs.engine_args: %w", err)
	}
	if spec.GPUs > 0 && engineArgs.GPUs == "" {
		engineArgs.GPUs = visibleGPUs(os.Getenv)
	}

	var netDirectory string
	if spec.Isolated {
		problem := IsolationProblem(engine.Name)
		if problem == nil && engine.Name == EngineApptainer {
			problem = apptainerAllowsIsolation(ctx, engine.Executable, spec.Image)
		}
		if problem != nil {
			return 3, fmt.Errorf("the job's sandbox must be network-isolated like the one that submitted it, but cannot be here: %w", problem)
		}
		if netDirectory, err = newNetDirectory(randomID()); err != nil {
			return 1, err
		}
		gateway, err := startGateway(netDirectory, "", newAllowlist(spec.Allowlist, nil, log), log)
		if err != nil {
			os.RemoveAll(netDirectory)
			return 1, err
		}
		defer gateway.Close()
		isolatedEnvironment(values)
		delete(values, "CODEFLOWER_LISTEN_SOCKET")
		delete(values, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC")
	}
	engineArgs = engineArgs.forRun(engine.Name, netDirectory == "", stderr)

	output, err := os.OpenFile(filepath.Join(spec.Logs, jobs.OutputFile(index)), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 1, err
	}
	defer output.Close()
	name := JobContainerPrefix + strings.TrimPrefix(spec.ID, "j-")
	if index != nil {
		name += "-" + strconv.Itoa(*index)
	}
	name += "-" + randomID()[:4]
	what := spec.ID
	if index != nil {
		what += fmt.Sprintf(" task %d", *index)
	}
	log(fmt.Sprintf("running %s (%s) in a sandbox with %s (%s)", what, spec.Name, engine.Label, spec.Image))
	code, err := runContainer(ctx, engine, containerSpec{name: name, image: spec.Image, state: spec.Project, binds: binds, env: values,
		command: append([]string{JobEntrypoint}, spec.Command...), netDirectory: netDirectory, noNetwork: netDirectory != "",
		resources: Resources{CPUs: resources.CPUs, Memory: resources.Memory}, engineArgs: engineArgs, stdout: output, stderr: output})
	if err != nil {
		return code, err
	}
	log(fmt.Sprintf("%s ended with exit status %d", what, code))
	return code, nil
}

// jobImage makes sure the engine has the job's image: an Apptainer file
// must be there; Docker and Podman load the saved copy, else download the
// published image.
func jobImage(ctx context.Context, engine EngineStatus, spec jobs.Spec, log func(string)) error {
	if engine.Name == EngineApptainer {
		if _, err := os.Stat(spec.Image); err != nil {
			return fmt.Errorf("the image %s is not on this node: put the image folder (image_dir in the config, or CODEFLOWER_IMAGE_DIR) on a filesystem every node shares", spec.Image)
		}
		return nil
	}
	err := CheckImage(ctx, engine, spec.Image)
	if !errors.Is(err, ErrNoImage) {
		return err
	}
	if spec.ImageArchive != "" {
		if _, statErr := os.Stat(spec.ImageArchive); statErr == nil {
			log("loading the image from " + spec.ImageArchive)
			loadErr := loadArchive(ctx, engine, spec.ImageArchive, spec.Image)
			if loadErr == nil {
				return nil
			}
			log(fmt.Sprintf("could not load it: %v", loadErr))
		}
	}
	if IsPublished(spec.Image) {
		log("downloading the image " + spec.Image)
		return PullImage(ctx, engine, spec.Image, os.Stderr)
	}
	return fmt.Errorf("%s does not have the image %s on this node", engine.Label, spec.Image)
}

var loadedID = regexp.MustCompile(`(?m)^Loaded image(?:\(s\))?: *(?:sha256:)?([0-9a-f]{12,64})\s*$`)

func loadedImage(output string) string {
	match := loadedID.FindStringSubmatch(output)
	if match == nil {
		return ""
	}
	return match[1]
}

// visibleGPUs are the GPUs the batch job was given, else all.
func visibleGPUs(getenv func(string) string) string {
	for _, name := range []string{"CUDA_VISIBLE_DEVICES", "SLURM_JOB_GPUS", "SLURM_STEP_GPUS"} {
		if value := strings.TrimSpace(getenv(name)); value != "" && value != "NoDevFiles" {
			return value
		}
	}
	return "all"
}
