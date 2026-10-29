package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/moby/term"
	"github.com/spf13/cobra"

	"codeflower-launcher/internal/sandbox"
)

const defaultRounds = 3

// taskFlags are the flags of `codeflower task` besides the sandbox's.
type taskFlags struct {
	skipReport   bool
	timeout      time.Duration
	wrapUp       time.Duration
	rounds       int
	model        string
	effort       string
	outputMode   string
	optionValues []string
	// promptFile is the Markdown file with the task or follow-up (--prompt).
	promptFile string
	// continueDirectory is the folder of the task to continue.
	continueDirectory string
}

// continueKeeps are the flags a continued task cannot change: it keeps its
// project, agent, folders, network and report.
var continueKeeps = []string{"agent", "project", "state", "input", "output", "code", "skills", "bind",
	"isolate-network", "allowlist", "skip-report"}

func taskCommand() *cobra.Command {
	var options sandboxOptions
	var flags taskFlags

	command := &cobra.Command{
		Use:   "task (TASK | --prompt FILE) | task --continue TASK_DIR [FOLLOW_UP | --prompt FILE]",
		Short: "Let the agent work on a task by itself, then stop",
		Long: `Start a sandbox for a project and let its agent work on TASK by itself,
without VS Code, until it is done: for example as a batch job. TASK is the
request in plain words, or - to read it from standard input; --prompt FILE
reads it from a Markdown file instead.

The agent gets the task as its goal (Claude Code's and Codex's /goal) and
instructions that nobody is there to answer questions. It works until it
judges the task done, or for at most --max-rounds rounds. Its results go to
the first --output folder, with a storyteller report, report.html, that it
builds as it goes (leave it out with --skip-report). Progress is printed as
the agent works.

When the rounds run out, or shortly before --timeout (a tenth of it, between
2 and 15 minutes), the agent is told to stop starting new work and to write up
what it did, what it found and what is left, so a stopped task still leaves a
report.

The task's folder, PROJECT/tasks/ID, is printed when the task starts.
--continue TASK_DIR runs the task again in the same session: after it was
interrupted, failed (e.g. on a usage limit), or ran out of rounds or time, the
agent is told so and picks up where it stopped; after it was done, it checks
its work again. FOLLOW_UP (or - to read it from standard input) asks for more
work in the same task; --prompt FILE reads it from a Markdown file. A continued task keeps its project, agent, folders,
network and report; each run gets its own --max-rounds and --timeout, and
may choose another --model, --effort, --option, --engine or --image.

--model and --effort choose among what the agent offers, e.g. opus or
claude-opus-5 and high for Claude Code, gpt-6.1-sol and xhigh for Codex;
the task stops with the agent's choices when it has no such value. Other
settings of the agent go by their id: --option fast=on.

--output-mode auto uses an attached progress view in a terminal and raw output
when redirected; choose raw or attached to override it.

If the agent offers terminal sign-in and the task does not read standard
input, "codeflower task" signs in when needed in an interactive terminal,
then continues the task. Batch runs and tasks read from standard input still
need sign-in beforehand; use "codeflower interactive" once and sign in there.
The run is a session of the project, so VS Code lists it afterwards and can
continue it.

Exit status: 0 done, 2 not done (the agent judged it impossible, it ran out
of rounds or time, or the report failed its check), 3 not signed in, 130
interrupted or timed out, 1 other errors.`,
		Example: `  codeflower task --agent claude --input ~/data/study42 --output ~/results/study42 \
    "Cluster the cells and annotate the cell types"
  codeflower task --agent claude --model opus --effort high --input ~/data/study42 --output ~/results/study42 \
    "Find the genes that respond to the stimulation in each cell type"
  codeflower task --agent codex --project study42 --input /data/study42 --output ~/results/qc --skip-report \
    "Run quality control and write the filtered matrix"
  sbatch --time=8:00:00 --wrap "codeflower task --agent claude --project study42 \
    --input /data/study42 --output ~/results/study42 --timeout 7h30m --prompt task.md"
  codeflower task --continue ~/codeflower/projects/study42/tasks/1a2b3c --timeout 7h30m
  codeflower task --continue ~/codeflower/projects/study42/tasks/1a2b3c "Also compare the two donors"`,
		Args: func(command *cobra.Command, args []string) error {
			what := "task"
			if flags.continueDirectory != "" {
				what = "follow-up"
			}
			return checkPromptArgs(args, flags.promptFile, flags.continueDirectory == "", what)
		},
		RunE: func(command *cobra.Command, args []string) error {
			if flags.timeout < 0 {
				return fmt.Errorf("--timeout must not be negative")
			}
			if flags.wrapUp < 0 {
				return fmt.Errorf("--wrap-up must not be negative")
			}
			outputMode, err := taskOutputMode(flags.outputMode, isTerminal(os.Stdout))
			if err != nil {
				return err
			}
			if flags.rounds < 1 {
				return fmt.Errorf("--max-rounds must be at least 1")
			}
			settings, err := parseSettings(flags.optionValues)
			if err != nil {
				return err
			}
			if flags.continueDirectory != "" {
				return continueTask(command, &options, flags, outputMode, settings, args)
			}
			return startTask(command, &options, flags, outputMode, settings, args)
		},
	}
	addSandboxFlags(command, &options)
	command.Flags().StringVar(&flags.continueDirectory, "continue", "",
		"continue the task in this folder (PROJECT/tasks/ID, printed when it started) in its session")
	command.Flags().StringVar(&flags.promptFile, "prompt", "",
		"read the task (with --continue: the follow-up) from this Markdown file instead of an argument")
	command.Flags().BoolVar(&flags.skipReport, "skip-report", false, "do not ask for a storyteller report")
	command.Flags().DurationVar(&flags.timeout, "timeout", 0, "stop the task after this long, e.g. 90m or 7h30m (default: no limit)")
	command.Flags().DurationVar(&flags.wrapUp, "wrap-up", 0,
		"how long before --timeout the agent is told to write up what it has (default: a tenth of --timeout, 2-15 minutes)")
	_ = command.Flags().MarkHidden("wrap-up")
	command.Flags().IntVar(&flags.rounds, "max-rounds", defaultRounds,
		"how often the agent is sent back to work when it has not finished (a continued task: the same as before)")
	command.Flags().StringVar(&flags.outputMode, "output-mode", "auto",
		"task output: auto (attached in a terminal, raw when redirected), raw, or attached")
	command.Flags().StringVar(&flags.model, "model", "", "the agent's model, e.g. opus, sonnet or claude-opus-5 (default: the agent's)")
	command.Flags().StringVar(&flags.effort, "effort", "", "how hard the model thinks, e.g. low, medium, high or max (default: the agent's)")
	command.Flags().StringArrayVar(&flags.optionValues, "option", nil,
		"another setting of the agent, ID=VALUE, e.g. fast=on (repeatable)")
	return command
}

// startTask writes a new task to the project and runs its first attempt.
func startTask(command *cobra.Command, options *sandboxOptions, flags taskFlags, outputMode string,
	settings map[string]string, args []string) error {
	text, argument, err := readPrompt(args, flags.promptFile, os.Stdin, "task")
	if err != nil {
		return err
	}
	if len(options.folders.Outputs) == 0 {
		return fmt.Errorf("--output is required: the folder the results and the report go to")
	}
	if err := options.check(); err != nil {
		return err
	}
	model, effort, settings := options.taskDefaults(flags.model, flags.effort, settings)
	engine, err := options.chooseEngine(command.Context())
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(command.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	prepared, err := options.prepare(ctx, engine, "a code flower task")
	if err != nil {
		return err
	}
	output := outputBind(prepared.binds)
	if output == nil {
		return fmt.Errorf("--output is required: the folder the results and the report go to")
	}

	id := sandbox.NewInstanceID()
	task := sandbox.Task{
		Task:      text,
		Agent:     options.agent,
		Output:    output.Target,
		Report:    !flags.skipReport,
		MaxRounds: flags.rounds,
		Model:     strings.TrimSpace(model),
		Effort:    strings.TrimSpace(effort),
		Options:   settings,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Mounts:    sandbox.TaskMounts(prepared.binds),
		Engine:    options.engine,
		Image:     options.image,
	}
	if prepared.isolation != nil {
		task.Allowlist = prepared.isolation.Allowlist
	}
	if err := sandbox.WriteTask(prepared.state, id, task); err != nil {
		return err
	}
	directory := sandbox.TaskDirectory(prepared.state, id)
	unlock, err := sandbox.LockTask(directory)
	if err != nil {
		return err
	}
	defer unlock()
	fmt.Fprintf(os.Stderr, "  task     %s\n", summarize(text))
	return runTaskAttempt(ctx, taskAttemptRun{
		prepared:    prepared,
		agent:       options.agent,
		id:          id,
		directory:   directory,
		attempt:     sandbox.TaskAttempt{Attempt: 1},
		interactive: canSignInFromTask(argument, isTerminal(os.Stdin)),
		timeout:     flags.timeout,
		wrapUp:      flags.wrapUp,
		outputMode:  outputMode,
		model:       task.Model,
		effort:      task.Effort,
	})
}

// continueTask runs the next attempt of the task in a folder, with the
// project, agent, mounts and network it started with.
func continueTask(command *cobra.Command, options *sandboxOptions, flags taskFlags, outputMode string,
	settings map[string]string, args []string) error {
	for _, name := range continueKeeps {
		if command.Flags().Changed(name) {
			return fmt.Errorf("--%s cannot be used with --continue: a continued task keeps its project, agent, folders, network and report", name)
		}
	}
	followUp, argument, err := readPrompt(args, flags.promptFile, os.Stdin, "follow-up")
	if err != nil {
		return err
	}

	directory, state, id, err := sandbox.ResolveTaskDirectory(flags.continueDirectory)
	if err != nil {
		return err
	}
	unlock, err := sandbox.LockTask(directory)
	if err != nil {
		return err
	}
	defer unlock()
	task, err := sandbox.ReadTask(directory)
	if err != nil {
		return err
	}
	binds := task.Binds()
	if output := outputBind(binds); output == nil || output.Target != task.Output {
		return fmt.Errorf("%s does not say where the task's output folder is", filepath.Join(directory, "task.json"))
	}
	if missing := missingSources(binds); len(missing) > 0 {
		return fmt.Errorf("the task's folders must be where they were when it started; missing: %s", strings.Join(missing, ", "))
	}

	options.agent = task.Agent
	options.state = state
	if err := options.check(); err != nil {
		return err
	}
	// The task's own mounts and network, not the config's current ones.
	options.configSkills = nil
	options.isolateNetwork = task.Allowlist != ""
	options.allowlist = task.Allowlist
	if !command.Flags().Changed("engine") && task.Engine != "" {
		options.engine = task.Engine
	}
	if !command.Flags().Changed("image") && !command.Flags().Changed("image-tag") && task.Image != "" {
		options.image = task.Image
	}
	engine, err := options.chooseEngine(command.Context())
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(command.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	prepared, err := options.prepareWith(ctx, engine, "a code flower task", binds)
	if err != nil {
		return err
	}

	number, err := sandbox.NextAttempt(directory)
	if err != nil {
		return err
	}
	attempt := sandbox.TaskAttempt{
		Attempt:   number,
		FollowUp:  followUp,
		MaxRounds: task.MaxRounds,
		Model:     strings.TrimSpace(flags.model),
		Effort:    strings.TrimSpace(flags.effort),
	}
	if command.Flags().Changed("max-rounds") {
		attempt.MaxRounds = flags.rounds
	}
	if len(settings) > 0 {
		attempt.Options = maps.Clone(task.Options)
		if attempt.Options == nil {
			attempt.Options = map[string]string{}
		}
		maps.Copy(attempt.Options, settings)
	}
	fmt.Fprintf(os.Stderr, "  task     %s\n", summarize(task.Task))
	if followUp != "" {
		fmt.Fprintf(os.Stderr, "  follow-up %s\n", summarize(followUp))
	}
	return runTaskAttempt(ctx, taskAttemptRun{
		prepared:    prepared,
		agent:       task.Agent,
		id:          id,
		directory:   directory,
		attempt:     attempt,
		interactive: canSignInFromTask(argument, isTerminal(os.Stdin)),
		timeout:     flags.timeout,
		wrapUp:      flags.wrapUp,
		outputMode:  outputMode,
		model:       cmp.Or(attempt.Model, task.Model),
		effort:      cmp.Or(attempt.Effort, task.Effort),
	})
}

// missingSources are the mounts' host folders that are not there.
func missingSources(binds []sandbox.Bind) []string {
	var missing []string
	for _, bind := range binds {
		if _, err := os.Stat(bind.Source); err != nil {
			missing = append(missing, bind.Source)
		}
	}
	return missing
}

// taskAttemptRun is one attempt at a task, ready to run.
type taskAttemptRun struct {
	prepared    preparedSandbox
	agent       string
	id          string
	directory   string
	attempt     sandbox.TaskAttempt
	interactive bool
	timeout     time.Duration
	wrapUp      time.Duration
	outputMode  string
	// model and effort are what the progress view shows until the agent says.
	model  string
	effort string
}

// runTaskAttempt writes the attempt, runs it, and prints how it ended and how
// to continue the task.
func runTaskAttempt(ctx context.Context, run taskAttemptRun) error {
	attempt := run.attempt
	attempt.Progress = progressProtocol(run.outputMode)
	attempt.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	if run.timeout > 0 {
		attempt.WrapUpAt = time.Now().Add(run.timeout - wrapUpReserve(run.timeout, run.wrapUp)).UTC().Format(time.RFC3339)
	}
	if err := sandbox.WriteAttempt(run.directory, attempt); err != nil {
		return err
	}
	if output := outputBind(run.prepared.binds); output != nil {
		fmt.Fprintf(os.Stderr, "  results  %s\n", output.Source)
	}
	fmt.Fprintf(os.Stderr, "  task dir %s\n", run.directory)

	result, err := run.run(ctx, attempt)
	fmt.Fprintf(os.Stderr, "  continue %s\n", continueCommand(run.directory, result != nil && result.Status == "completed"))
	return err
}

func (run taskAttemptRun) run(ctx context.Context, attempt sandbox.TaskAttempt) (*sandbox.TaskResult, error) {
	running := ctx
	if run.timeout > 0 {
		var cancel context.CancelFunc
		running, cancel = context.WithTimeout(ctx, run.timeout)
		defer cancel()
	}
	worker := run.prepared.worker(run.agent)
	worker.Task = run.id
	worker.TaskAttempt = attempt.Attempt
	worker.TaskInteractive = run.interactive
	if worker.TaskInteractive {
		worker.Stdin = os.Stdin
	}
	started := time.Now()
	var progress *taskProgress
	if run.outputMode == "attached" {
		progress = newTaskProgress(os.Stdout, isTerminal(os.Stdout), sandbox.AgentLabel(run.agent), run.model, run.effort, started)
		progress.Start()
		worker.Stdout = progress
		defer progress.Close()
	}
	code, err := worker.Run(running, nil)
	if progress != nil {
		if closeErr := progress.Close(); closeErr != nil {
			return nil, fmt.Errorf("could not finish the attached progress view: %w", closeErr)
		}
	}
	if err != nil {
		if errors.Is(err, sandbox.ErrNoImage) {
			return nil, fmt.Errorf("%w\nSee `codeflower check` for what this machine has", err)
		}
		return nil, err
	}
	result, err := sandbox.ReadAttemptResult(run.directory, attempt.Attempt)
	if err != nil {
		if running.Err() != nil {
			fmt.Fprintln(os.Stderr, "The task was stopped before it could record how far it got.")
			return nil, exitError{sandbox.TaskInterrupted}
		}
		return nil, fmt.Errorf("the task ended without a result (sandbox exit status %d)", code)
	}
	if errors.Is(running.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		result.Message = strings.TrimSpace(fmt.Sprintf("Stopped after --timeout %s. %s", run.timeout, result.Message))
	}
	printTaskResult(os.Stderr, result, run.prepared, time.Since(started))
	if exit := result.ExitCode(); exit != 0 {
		return &result, exitError{exit}
	}
	return &result, nil
}

// wrapUpReserve is how long before the timeout the agent is told to wrap up:
// the --wrap-up asked for, or a tenth of the timeout between 2 and 15
// minutes; at most half the timeout.
func wrapUpReserve(timeout, asked time.Duration) time.Duration {
	reserve := asked
	if reserve == 0 {
		reserve = min(max(timeout/10, 2*time.Minute), 15*time.Minute)
	}
	return min(reserve, timeout/2)
}

// continueCommand is the command that continues the task in directory.
func continueCommand(directory string, followUp bool) string {
	command := "codeflower task --continue " + shellQuote(directory)
	if followUp {
		command += ` "FOLLOW-UP"`
	}
	return command
}

// shellQuote quotes a word for a POSIX shell when it needs it.
func shellQuote(word string) string {
	if word != "" && strings.Trim(word, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-./:@%+=,") == "" {
		return word
	}
	return "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
}

func taskOutputMode(requested string, terminal bool) (string, error) {
	switch requested {
	case "auto":
		if terminal {
			return "attached", nil
		}
		return "raw", nil
	case "raw", "attached":
		return requested, nil
	default:
		return "", fmt.Errorf("--output-mode must be auto, raw or attached: %q", requested)
	}
}

func progressProtocol(outputMode string) string {
	if outputMode == "attached" {
		return "json"
	}
	return ""
}

func isTerminal(file *os.File) bool {
	_, isTerminal := term.GetFdInfo(file)
	return isTerminal
}

func canSignInFromTask(argument string, terminal bool) bool {
	return argument != "-" && terminal
}

// taskDefaults fills the model, effort and settings the user left out from the
// agent's entry in the config; --option values win over the config's.
func (options sandboxOptions) taskDefaults(model, effort string, settings map[string]string) (string, string, map[string]string) {
	defaults := options.userConfig.Agent(options.agent)
	if strings.TrimSpace(model) == "" {
		model = defaults.Model
	}
	if strings.TrimSpace(effort) == "" {
		effort = defaults.Effort
	}
	if len(defaults.Options) > 0 {
		merged := map[string]string{}
		for id, value := range defaults.Options {
			merged[id] = value
		}
		for id, value := range settings {
			merged[id] = value
		}
		settings = merged
	}
	return model, effort, settings
}

// parseSettings reads --option ID=VALUE values.
func parseSettings(values []string) (map[string]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	settings := map[string]string{}
	for _, value := range values {
		id, setting, ok := strings.Cut(value, "=")
		id, setting = strings.TrimSpace(id), strings.TrimSpace(setting)
		if !ok || id == "" || setting == "" {
			return nil, fmt.Errorf("--option must be ID=VALUE, e.g. fast=on: %q", value)
		}
		settings[id] = setting
	}
	return settings, nil
}

// readTask returns the task text; - reads it from in.
func readTask(argument string, in io.Reader) (string, error) {
	return readArgument(argument, in, "task")
}

// checkPromptArgs accepts either the positional argument or --prompt, not
// both; required says one of them must be given.
func checkPromptArgs(args []string, promptFile string, required bool, what string) error {
	if err := cobra.MaximumNArgs(1)(nil, args); err != nil {
		return err
	}
	switch {
	case len(args) == 1 && promptFile != "":
		return fmt.Errorf("give the %s as an argument or with --prompt, not both", what)
	case required && len(args) == 0 && promptFile == "":
		return fmt.Errorf("give the %s as an argument, - to read it from standard input, or --prompt FILE", what)
	}
	return nil
}

// readPrompt returns the text of the positional argument or of the --prompt
// file, and the argument ("" for a file); it returns "" when neither was given.
func readPrompt(args []string, promptFile string, in io.Reader, what string) (string, string, error) {
	if promptFile != "" {
		text, err := readPromptFile(promptFile, what)
		return text, "", err
	}
	if len(args) == 0 {
		return "", "", nil
	}
	text, err := readArgument(args[0], in, what)
	return text, args[0], err
}

// readPromptFile reads the Markdown file --prompt names.
func readPromptFile(file string, what string) (string, error) {
	info, err := os.Stat(file)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("--prompt %s: no such file", file)
	}
	if err != nil {
		return "", fmt.Errorf("--prompt %s: %w", file, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("--prompt %s is a folder: give the Markdown file with the %s", file, what)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("could not read the %s: %w", what, err)
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return "", fmt.Errorf("the %s in %s is empty: say what the agent should do", what, file)
	}
	return text, nil
}

// readArgument returns the text of an argument; - reads it from in.
func readArgument(argument string, in io.Reader, what string) (string, error) {
	text := argument
	if argument == "-" {
		data, err := io.ReadAll(in)
		if err != nil {
			return "", fmt.Errorf("could not read the %s: %w", what, err)
		}
		text = string(data)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("the %s is empty: say what the agent should do", what)
	}
	return text, nil
}

// outputBind is the first --output folder's mount: --output folders are
// mounted before --bind ones.
func outputBind(binds []sandbox.Bind) *sandbox.Bind {
	for index := range binds {
		if strings.HasPrefix(binds[index].Target, sandbox.OutputsRoot+"/") && !binds[index].ReadOnly {
			return &binds[index]
		}
	}
	return nil
}

// hostPath is where a sandbox path is on this computer, if it is in a mount.
func hostPath(binds []sandbox.Bind, inside string) string {
	for _, bind := range binds {
		if inside == bind.Target || strings.HasPrefix(inside, bind.Target+"/") {
			relative := strings.TrimPrefix(strings.TrimPrefix(inside, bind.Target), "/")
			return filepath.Join(bind.Source, filepath.FromSlash(relative))
		}
	}
	return inside
}

// summarize is the task's first line, shortened for the summary.
func summarize(text string) string {
	line, _, more := strings.Cut(text, "\n")
	if len(line) > 100 {
		line, more = line[:99], true
	}
	if more {
		line += "…"
	}
	return line
}

func printTaskResult(out io.Writer, result sandbox.TaskResult, prepared preparedSandbox, took time.Duration) {
	headline := map[string]string{
		"completed":     "Task done",
		"unmet":         "Task not done",
		"not_signed_in": "Task not started",
		"cancelled":     "Task stopped",
	}[result.Status]
	if headline == "" {
		headline = "Task failed"
	}
	fmt.Fprintf(out, "\n%s after %s", headline, took.Round(time.Second))
	if result.Rounds > 0 {
		fmt.Fprintf(out, " (%d %s)", result.Rounds, plural(result.Rounds, "round"))
	}
	fmt.Fprintln(out)
	if result.Message != "" {
		fmt.Fprintf(out, "  %s\n", result.Message)
	}
	if continuation := result.Continuation; continuation != nil {
		fmt.Fprintf(out, "  attempt  %d, %s\n", result.Attempt, describeContinuation(*continuation))
	}
	if result.WrapUp != nil {
		fmt.Fprintf(out, "  wrap-up  %s: the agent wrote up what it had\n", wrapUpReason(result.WrapUp.Reason))
	}
	if usage := taskUsageSummary(result.Usage); usage != "" {
		fmt.Fprintf(out, "  usage    %s\n", usage)
	}
	if len(result.Settings) > 0 {
		keys := slices.Sorted(maps.Keys(result.Settings))
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, key+" "+result.Settings[key])
		}
		fmt.Fprintf(out, "  ran with %s\n", strings.Join(parts, " · "))
	}
	if report := result.Report; report != nil {
		where := hostPath(prepared.binds, path.Clean(report.Path))
		switch {
		case !report.Exists:
			fmt.Fprintf(out, "  report   missing (expected at %s)\n", where)
		case report.Checked:
			fmt.Fprintf(out, "  report   %s\n", where)
		default:
			fmt.Fprintf(out, "  report   %s (storyteller check failed: %s)\n", where, report.Check)
		}
	}
	if result.SessionKey != "" {
		fmt.Fprintf(out, "  session  %s: open the project's sessions in VS Code to read or continue it\n", result.SessionKey)
	}
}

// describeContinuation says how an attempt went on from the one before.
func describeContinuation(continuation sandbox.TaskContinuation) string {
	text, ok := map[string]string{
		"restart":     "started over: the attempt before had not started a session",
		"interrupted": "continued the session after an interruption",
		"error":       "continued the session after an error",
		"unmet":       "continued the session: the task was not done",
		"completed":   "continued the finished task's session",
	}[continuation.Kind]
	if !ok {
		text = "continued the session"
	}
	if continuation.FollowUp {
		text += ", with a follow-up"
	}
	return text
}

// wrapUpReason says why a task wrapped up.
func wrapUpReason(reason string) string {
	if reason == "time" {
		return "the time was almost up"
	}
	return "out of rounds"
}

func plural(count int, word string) string {
	if count == 1 {
		return word
	}
	return word + "s"
}
