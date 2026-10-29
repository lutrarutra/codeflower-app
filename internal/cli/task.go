package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"codeflower-launcher/internal/sandbox"
)

const defaultRounds = 3

func taskCommand() *cobra.Command {
	var options sandboxOptions
	var skipReport bool
	var timeout time.Duration
	var rounds int
	var model, effort string
	var optionValues []string

	command := &cobra.Command{
		Use:   "task TASK",
		Short: "Let the agent work on a task by itself, then stop",
		Long: `Start a sandbox for a project and let its agent work on TASK by itself,
without VS Code, until it is done: for example as a batch job. TASK is the
request in plain words, or - to read it from standard input.

The agent gets the task as its goal (Claude Code's and Codex's /goal) and
instructions that nobody is there to answer questions. It works until it
judges the task done, or for at most --max-rounds rounds. Its results go to
the first --output folder, with a storyteller report, report.html, that it
builds as it goes (leave it out with --skip-report). Progress is printed as
the agent works.

--model and --effort choose among what the agent offers, e.g. opus or
claude-opus-5 and high for Claude Code, gpt-6.1-sol and xhigh for Codex;
the task stops with the agent's choices when it has no such value. Other
settings of the agent go by their id: --option fast=on.

The agent must already be signed in for the project: start
"codeflower interactive" once, connect VS Code and sign in. The run is a
session of the project, so VS Code lists it afterwards and can continue it.

Exit status: 0 done, 2 not done (the agent judged it impossible, it ran out
of rounds, or the report failed its check), 3 not signed in, 130 interrupted
or timed out, 1 other errors.`,
		Example: `  codeflower task --agent claude --input ~/data/study42 --output ~/results/study42 \
    "Cluster the cells and annotate the cell types"
  codeflower task --agent claude --model opus --effort high --input ~/data/study42 --output ~/results/study42 \
    "Find the genes that respond to the stimulation in each cell type"
  codeflower task --agent codex --project study42 --input /data/study42 --output ~/results/qc --skip-report \
    "Run quality control and write the filtered matrix"
  sbatch --time=8:00:00 --wrap "codeflower task --agent claude --project study42 \
    --input /data/study42 --output ~/results/study42 --timeout 7h30m - < task.md"`,
		Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			text, err := readTask(args[0], os.Stdin)
			if err != nil {
				return err
			}
			if len(options.folders.Outputs) == 0 {
				return fmt.Errorf("--output is required: the folder the results and the report go to")
			}
			if timeout < 0 {
				return fmt.Errorf("--timeout must not be negative")
			}
			if rounds < 1 {
				return fmt.Errorf("--max-rounds must be at least 1")
			}
			settings, err := parseSettings(optionValues)
			if err != nil {
				return err
			}
			if err := options.check(); err != nil {
				return err
			}
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
				Report:    !skipReport,
				MaxRounds: rounds,
				Model:     strings.TrimSpace(model),
				Effort:    strings.TrimSpace(effort),
				Options:   settings,
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
			}
			if err := sandbox.WriteTask(prepared.state, id, task); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "  task     %s\n", summarize(text))
			fmt.Fprintf(os.Stderr, "  results  %s\n", output.Source)

			running := ctx
			if timeout > 0 {
				var cancel context.CancelFunc
				running, cancel = context.WithTimeout(ctx, timeout)
				defer cancel()
			}
			worker := prepared.worker(options.agent)
			worker.Task = id
			started := time.Now()
			code, err := worker.Run(running, nil)
			if err != nil {
				if errors.Is(err, sandbox.ErrNoImage) {
					return fmt.Errorf("%w\nSee `codeflower check` for what this machine has", err)
				}
				return err
			}
			result, err := sandbox.ReadTaskResult(prepared.state, id)
			if err != nil {
				if running.Err() != nil {
					fmt.Fprintln(os.Stderr, "The task was stopped before it could record how far it got.")
					return exitError{sandbox.TaskInterrupted}
				}
				return fmt.Errorf("the task ended without a result (sandbox exit status %d)", code)
			}
			if errors.Is(running.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
				result.Message = strings.TrimSpace(fmt.Sprintf("Stopped after --timeout %s. %s", timeout, result.Message))
			}
			printTaskResult(os.Stderr, result, prepared, time.Since(started))
			if exit := result.ExitCode(); exit != 0 {
				return exitError{exit}
			}
			return nil
		},
	}
	addSandboxFlags(command, &options)
	command.Flags().BoolVar(&skipReport, "skip-report", false, "do not ask for a storyteller report")
	command.Flags().DurationVar(&timeout, "timeout", 0, "stop the task after this long, e.g. 90m or 7h30m (default: no limit)")
	command.Flags().IntVar(&rounds, "max-rounds", defaultRounds,
		"how often the agent is sent back to work when it has not finished")
	command.Flags().StringVar(&model, "model", "", "the agent's model, e.g. opus, sonnet or claude-opus-5 (default: the agent's)")
	command.Flags().StringVar(&effort, "effort", "", "how hard the model thinks, e.g. low, medium, high or max (default: the agent's)")
	command.Flags().StringArrayVar(&optionValues, "option", nil,
		"another setting of the agent, ID=VALUE, e.g. fast=on (repeatable)")
	return command
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
	text := argument
	if argument == "-" {
		data, err := io.ReadAll(in)
		if err != nil {
			return "", fmt.Errorf("could not read the task: %w", err)
		}
		text = string(data)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("the task is empty: say what the agent should do")
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

func plural(count int, word string) string {
	if count == 1 {
		return word
	}
	return word + "s"
}
