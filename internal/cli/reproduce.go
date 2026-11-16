package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"codeflower-app/internal/reproduce"
	"codeflower-app/internal/sandbox"
)

// Exit statuses of codeflower reproduce besides 0 (reproduced) and 3 (no engine).
const (
	reproduceDiffers      = 1
	reproduceInvalid      = 2
	reproduceRestore      = 4
	reproducePipelineFail = 5
)

type reproduceOptions struct {
	output    string
	engine    string
	image     string
	snapshot  string
	cache     string
	cores     int
	cpus      int
	memory    string
	validate  bool
	keepState bool
	verbose   bool
}

func reproduceCommand() *cobra.Command {
	var options reproduceOptions
	defaultEngine := os.Getenv("CODEFLOWER_ENGINE")
	if defaultEngine == "" {
		defaultEngine = "auto"
	}
	command := &cobra.Command{
		Use:   "reproduce ARCHIVE --output DIR",
		Short: "Rerun a pipeline archive in a new sandbox and compare its results",
		Long: `Rerun a reproducible pipeline archive, which an agent made with codeflower-repro
in a sandbox, and compare its results with those it recorded. ARCHIVE is the
NAME.tar.gz file, its URL, or an unpacked pipeline folder.

No agent is involved and no sign-in is needed. In a new sandbox with a new,
throwaway project, it:

  1. downloads the archive's input data and checks each file's sha256 (on this
     computer, into a temporary folder, or kept in --cache), and checks the
     archive (no network);
  2. restores the software environment from the archive's lockfiles: only the
     package indexes (PyPI, conda, CRAN, Bioconductor, GitHub) are reachable
     where the engine can restrict the network (Linux, Apptainer); on macOS and
     Windows the network is not restricted, which the summary says. With
     --snapshot, the environment packed beside the archive is unpacked instead,
     without network;
  3. runs the pipeline without network, and compares every result with the
     recorded one, exactly or within the tolerance its two runs showed.

The output folder gets results/, logs/ (one per rule), the run's time and
memory (run.json), the comparison (reproduce-report.md and .json) and
everything the sandbox printed (reproduce.log; --verbose also shows it here).

The image is the archive's: pulled by its digest when it was published, or the
local build it names. --image runs another one, which can change results.

Exit status: 0 reproduced, 1 results differ, 2 the archive is not valid,
3 no container engine, 4 the environment could not be restored, 5 the
pipeline failed.`,
		Example: `  codeflower reproduce smith2026.tar.gz --output ~/repro/smith2026
  codeflower reproduce https://zenodo.org/records/123/files/smith2026.tar.gz -o repro
  codeflower reproduce smith2026.tar.gz --snapshot smith2026-env-amd64.tar.gz -o repro
  codeflower reproduce smith2026.tar.gz --validate -o check`,
		Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			if options.output == "" {
				return fmt.Errorf("--output is required: the folder for the results and the report")
			}
			if options.engine != "auto" && sandbox.EngineLabel(options.engine) == options.engine {
				return fmt.Errorf("--engine must be auto, %s", strings.Join(sandbox.EngineNames, ", "))
			}
			var resources sandbox.Resources
			if options.cpus < 0 {
				return fmt.Errorf("--cpus must be positive")
			}
			resources.CPUs = options.cpus
			if options.memory != "" {
				memory, err := sandbox.ParseMemory(options.memory)
				if err != nil {
					return fmt.Errorf("--memory: %w", err)
				}
				resources.Memory = memory
			}
			ctx, stop := signal.NotifyContext(command.Context(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
			defer stop()
			code, err := runReproduce(ctx, args[0], options, resources, os.Stdout, os.Stderr)
			if err != nil {
				return err
			}
			if code != 0 {
				return exitError{code}
			}
			return nil
		},
	}
	command.Flags().StringVarP(&options.output, "output", "o", "", "folder for the results, logs and report (created)")
	command.Flags().StringVar(&options.engine, "engine", defaultEngine,
		"container engine: auto, docker, podman, or apptainer (or set $CODEFLOWER_ENGINE)")
	command.Flags().StringVar(&options.image, "image", "",
		"run this image instead of the archive's (a Docker/Podman image name, or an Apptainer .sif file)")
	command.Flags().StringVar(&options.snapshot, "snapshot", "",
		"restore the environment from this snapshot (NAME-env-ARCH.tar.gz, packed with the archive) instead of the lockfiles")
	command.Flags().StringVar(&options.cache, "cache", "",
		"folder to keep downloaded archives and data in for later runs (default: a temporary folder, removed afterwards)")
	command.Flags().IntVar(&options.cores, "cores", 0, "cores the pipeline may use (default: the archive's)")
	command.Flags().IntVar(&options.cpus, "cpus", 0, "CPUs the sandbox may use (Docker and Podman)")
	command.Flags().StringVar(&options.memory, "memory", "", "memory the sandbox may use, e.g. 16g (Docker and Podman)")
	command.Flags().BoolVar(&options.validate, "validate", false, "only check the archive and its data; do not run it")
	command.Flags().BoolVar(&options.keepState, "keep-state", false, "keep the throwaway project folder, to look into a failure")
	command.Flags().BoolVarP(&options.verbose, "verbose", "v", false,
		"show the sandbox's output (Snakemake, the package installs) here, not only in reproduce.log")
	return command
}

func runReproduce(ctx context.Context, source string, options reproduceOptions, resources sandbox.Resources, stdout, stderr io.Writer) (int, error) {
	// Downloads go to a temporary folder removed afterwards, unless --cache
	// names a folder to keep them in for later runs.
	cache := options.cache
	if cache == "" {
		temporary, err := os.MkdirTemp("", "codeflower-reproduce-downloads-")
		if err != nil {
			return 0, err
		}
		defer os.RemoveAll(temporary)
		// Docker Desktop shares /private/var/folders, not the /var/folders link.
		if resolved, err := filepath.EvalSymlinks(temporary); err == nil {
			temporary = resolved
		}
		cache = temporary
	} else {
		absolute, err := filepath.Abs(cache)
		if err != nil {
			return 0, err
		}
		cache = absolute
	}
	output, err := filepath.Abs(options.output)
	if err != nil {
		return 0, err
	}
	if entries, err := os.ReadDir(output); err == nil && len(entries) > 0 {
		return 0, fmt.Errorf("the output folder %s is not empty: give a new one", output)
	}

	pipeline, cleanup, err := openArchive(ctx, source, cache, stderr)
	if err != nil {
		return reproduceInvalid, err
	}
	defer cleanup()
	manifest, err := reproduce.ReadManifest(pipeline)
	if err != nil {
		return reproduceInvalid, err
	}
	data, err := reproduce.ReadData(pipeline)
	if err != nil {
		return reproduceInvalid, err
	}
	title := manifest.Title
	if title == "" {
		title = filepath.Base(pipeline)
	}
	fmt.Fprintf(stderr, "Reproducing %s\n", title)
	if manifest.Resources.WallSeconds > 0 {
		fmt.Fprintf(stderr, "  The author's run took %s on %d cores, with up to %.0f MB of memory.\n",
			humanSeconds(manifest.Resources.WallSeconds), manifest.Resources.Cores, manifest.Resources.PeakRSSMB)
	}

	var warnings []string
	if host := hostPlatform(); manifest.Platform != "" && manifest.Platform != host {
		warnings = append(warnings, fmt.Sprintf("The archive was made on %s and runs here on %s: numbers can differ slightly.",
			manifest.Platform, host))
	}
	snapshot := ""
	if options.snapshot != "" {
		if snapshot, err = filepath.Abs(options.snapshot); err != nil {
			return 0, err
		}
		if manifest.Environment.Snapshot == nil {
			return reproduceInvalid, fmt.Errorf("the archive lists no environment snapshot")
		}
		sum, err := reproduce.FileSHA256(snapshot)
		if err != nil {
			return 0, err
		}
		if sum != manifest.Environment.Snapshot.SHA256 {
			return reproduceInvalid, fmt.Errorf("%s is not the archive's environment snapshot (sha256 differs)", options.snapshot)
		}
	}

	engine, err := sandbox.ChooseEngine(sandbox.DetectEngines(ctx), options.engine, options.image)
	if err != nil {
		return 0, engineMissing(stderr, engine, err)
	}
	image := options.image
	identity := sandbox.ImageIdentity{Ref: manifest.Image.Ref, Digest: manifest.Image.Digest, ID: manifest.Image.ID}
	if image == "" {
		if image, err = sandbox.ArchiveImage(ctx, engine, identity, stderr); err != nil {
			return reproduceInvalid, err
		}
	} else {
		if image, err = sandbox.ResolveImage(image, engine.Name); err != nil {
			return 0, err
		}
		if got, err := sandbox.IdentifyImage(ctx, engine, image); err == nil && !sameImage(identity, got) {
			warnings = append(warnings, fmt.Sprintf("This runs %s, not the archive's image (%s): results can differ.",
				options.image, firstNonEmpty(identity.Digest, identity.ID, identity.Ref, "not recorded")))
		}
	}

	dataDir := ""
	if bundled := len(data); bundled > 0 {
		if data, err = reproduce.ToDownload(pipeline, data); err != nil {
			return reproduceInvalid, err
		}
		if bundled -= len(data); bundled > 0 {
			if bundled == 1 {
				fmt.Fprintln(stderr, "  1 input is in the archive.")
			} else {
				fmt.Fprintf(stderr, "  %d inputs are in the archive.\n", bundled)
			}
		}
	}
	if len(data) > 0 {
		if dataDir, err = reproduce.FetchData(ctx, data, cache, stderr); err != nil {
			return reproduceInvalid, err
		}
		defer os.RemoveAll(dataDir)
	}

	// The sandbox's output goes to reproduce.log in the output folder; the terminal gets a line per phase.
	if err := os.MkdirAll(output, 0o755); err != nil {
		return 0, err
	}
	logPath := filepath.Join(output, "reproduce.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return 0, err
	}
	defer logFile.Close()
	var sandboxOutput io.Writer = logFile
	if options.verbose {
		sandboxOutput = io.MultiWriter(logFile, stderr)
	}
	run := sandbox.Reproduction{
		Engine: engine, Image: image, Pipeline: pipeline, Data: dataDir, Snapshot: snapshot, Output: output,
		Cores: options.cores, Resources: resources, KeepState: options.keepState, Stdout: sandboxOutput, Stderr: sandboxOutput,
		Log:      func(line string) { fmt.Fprintln(sandboxOutput, line) },
		Announce: func(phase string) { fmt.Fprintf(stderr, "  %s\n", phaseLabel(phase, snapshot != "")) },
	}
	if options.validate {
		run.Phases = []string{sandbox.PhaseCheck}
	}
	result, err := run.Run(ctx)
	warnings = append(warnings, result.Warnings...)
	if result.State != "" {
		fmt.Fprintf(stderr, "The throwaway project is kept in %s\n", result.State)
	}
	if err != nil {
		return 0, err
	}
	code := summarizeReproduction(stdout, output, result, warnings, options.validate)
	if code == reproduceInvalid || code == reproduceRestore || code == reproducePipelineFail {
		printTail(stdout, logPath, 40)
	}
	return code, nil
}

func phaseLabel(phase string, snapshot bool) string {
	switch phase {
	case sandbox.PhaseCheck:
		return "Checking the archive…"
	case sandbox.PhasePrepare:
		if snapshot {
			return "Restoring the environment from the snapshot…"
		}
		return "Restoring the environment from the lockfiles (a few minutes for large ones)…"
	}
	return "Running the pipeline without network…"
}

// printTail shows the end of the log, where a failure says why.
func printTail(out io.Writer, path string, lines int) {
	content, err := os.ReadFile(path)
	if err != nil || len(content) == 0 {
		return
	}
	all := strings.Split(strings.TrimRight(string(content), "\n"), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	fmt.Fprintf(out, "The end of %s:\n", path)
	for _, line := range all {
		fmt.Fprintf(out, "  | %s\n", line)
	}
}

// openArchive returns the unpacked pipeline: a folder as it is, a .tar.gz
// unpacked into a temporary folder, a URL downloaded into the cache first.
func openArchive(ctx context.Context, source, cache string, out io.Writer) (string, func(), error) {
	nothing := func() {}
	if strings.HasPrefix(source, "https://") || strings.HasPrefix(source, "http://") {
		name := filepath.Base(strings.SplitN(source, "?", 2)[0])
		if !strings.HasSuffix(name, ".tar.gz") && !strings.HasSuffix(name, ".tgz") {
			name += ".tar.gz"
		}
		file := filepath.Join(cache, "archives", name)
		fmt.Fprintf(out, "Downloading %s…\n", source)
		if _, err := reproduce.Download(ctx, source, file, out); err != nil {
			return "", nothing, err
		}
		source = file
	}
	info, err := os.Stat(source)
	if err != nil {
		return "", nothing, err
	}
	if info.IsDir() {
		absolute, err := filepath.Abs(source)
		return absolute, nothing, err
	}
	dir, err := os.MkdirTemp("", "codeflower-archive-")
	if err != nil {
		return "", nothing, err
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	cleanup := func() { os.RemoveAll(dir) }
	pipeline, err := reproduce.Unpack(source, dir)
	if err != nil {
		cleanup()
		return "", nothing, err
	}
	return pipeline, cleanup, nil
}

// summarizeReproduction prints the outcome and returns the exit status.
func summarizeReproduction(out io.Writer, output string, result sandbox.ReproductionResult, warnings []string, validate bool) int {
	// The archive check's warnings, whichever phase the run ended in.
	var check reproduce.CheckResult
	if content, err := os.ReadFile(filepath.Join(output, "check.json")); err == nil {
		_ = json.Unmarshal(content, &check)
	}
	warnings = append(check.Warnings, warnings...)
	defer func() {
		for _, warning := range warnings {
			fmt.Fprintf(out, "Warning: %s\n", warning)
		}
	}()
	switch result.Phase {
	case sandbox.PhaseCheck:
		if result.Code != 0 {
			fmt.Fprintln(out, "The archive is not valid:")
			for _, problem := range check.Errors {
				fmt.Fprintf(out, "  %s\n", problem)
			}
			return reproduceInvalid
		}
		if validate {
			fmt.Fprintln(out, "The archive and its data are valid.")
			return 0
		}
	case sandbox.PhasePrepare:
		if result.Code != 0 {
			fmt.Fprintln(out, "The environment could not be restored (see the output above).")
			return reproduceRestore
		}
	}
	var report reproduce.Report
	content, err := os.ReadFile(filepath.Join(output, "reproduce-report.json"))
	if err != nil || json.Unmarshal(content, &report) != nil {
		fmt.Fprintf(out, "The pipeline failed (exit status %d); the rules' logs are in %s.\n", result.Code, filepath.Join(output, "logs"))
		return reproducePipelineFail
	}
	if report.OK {
		fmt.Fprintf(out, "Reproduced: %d of %d results match. Results and report in %s\n", report.Matched, report.Checked, output)
		return 0
	}
	fmt.Fprintf(out, "Not reproduced: %d of %d results differ.\n", report.Checked-report.Matched, report.Checked)
	for _, entry := range report.Results {
		if !entry.OK {
			fmt.Fprintf(out, "  %s (%s): %s\n", entry.Path, entry.Compare, entry.Detail)
		}
	}
	fmt.Fprintf(out, "Results and report in %s\n", output)
	return reproduceDiffers
}

func sameImage(want, got sandbox.ImageIdentity) bool {
	if want.Digest != "" && got.Digest != "" {
		return want.Digest == got.Digest
	}
	return want.ID != "" && want.ID == got.ID
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// hostPlatform is the platform Linux containers run as here.
func hostPlatform() string {
	arch := runtime.GOARCH
	return "linux/" + arch
}

func humanSeconds(seconds float64) string {
	switch {
	case seconds < 90:
		return fmt.Sprintf("%.0f s", seconds)
	case seconds < 5400:
		return fmt.Sprintf("%.0f min", seconds/60)
	}
	return fmt.Sprintf("%.1f h", seconds/3600)
}
