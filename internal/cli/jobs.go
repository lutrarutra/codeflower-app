package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"codeflower-app/internal/config"
	"codeflower-app/internal/jobs"
	"codeflower-app/internal/sandbox"
)

// jobsCommand lists and cancels the batch jobs a project's agents asked for,
// also when no sandbox of the project runs.
func jobsCommand() *cobra.Command {
	var options projectsOptions
	command := &cobra.Command{
		Use:   "jobs",
		Short: "List, show, read and cancel the batch jobs of a project",
		Long: `List, show, read and cancel the batch jobs a project's agents asked for.

With jobs.scheduler in the config (or --jobs), the agent in a sandbox can ask
for batch jobs with codeflower-job; the app submits each, and it runs in a
sandbox of its own with the same image, folders and network. Jobs keep
running when the sandbox stops (unless jobs.on_stop is cancel), and the next
sandbox of the project follows them again. These commands work without one.

PROJECT is a project's name in the projects directory, or its folder.`,
		Args: cobra.NoArgs,
	}
	options.addFlags(command)

	var all bool
	list := &cobra.Command{
		Use:   "list PROJECT",
		Short: "List a project's jobs, asking the scheduler about those still active",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			ledger, cfg, err := options.jobsOf(args[0])
			if err != nil {
				return err
			}
			refreshJobs(command.Context(), cfg, ledger, os.Stderr)
			records, err := ledger.Records()
			if err != nil {
				return err
			}
			if !all {
				records = recentJobs(records)
			}
			printJobs(os.Stdout, records)
			return nil
		},
	}
	list.Flags().BoolVar(&all, "all", false, "list every job, not only the active ones and those of the last week")

	show := &cobra.Command{
		Use:   "show PROJECT JOB",
		Short: "Show a job: what it runs, with what, and how it ended",
		Args:  cobra.ExactArgs(2),
		RunE: func(command *cobra.Command, args []string) error {
			ledger, cfg, err := options.jobsOf(args[0])
			if err != nil {
				return err
			}
			refreshJobs(command.Context(), cfg, ledger, os.Stderr)
			record, err := ledger.Find(args[1])
			if err != nil {
				return err
			}
			showJob(os.Stdout, ledger, record)
			return nil
		},
	}

	var index int
	var schedulerLog bool
	logCommand := &cobra.Command{
		Use:   "log PROJECT JOB",
		Short: "Print a job's output",
		Args:  cobra.ExactArgs(2),
		RunE: func(command *cobra.Command, args []string) error {
			ledger, _, err := options.jobsOf(args[0])
			if err != nil {
				return err
			}
			record, err := ledger.Find(args[1])
			if err != nil {
				return err
			}
			name := jobs.OutputFile(nil)
			if command.Flags().Changed("index") {
				name = jobs.OutputFile(&index)
			} else if record.Array != "" && !schedulerLog {
				return fmt.Errorf("%s is an array: choose a task with --index", record.ID)
			}
			if schedulerLog {
				name = "scheduler.log"
				if command.Flags().Changed("index") {
					name = fmt.Sprintf("scheduler-%d.log", index)
				}
			}
			file, err := os.Open(filepath.Join(ledger.LogDir(record.ID), name))
			if err != nil {
				return fmt.Errorf("%s has no %s yet", record.ID, name)
			}
			defer file.Close()
			_, err = io.Copy(os.Stdout, file)
			return err
		},
	}
	logCommand.Flags().IntVar(&index, "index", 0, "the array task whose output to print")
	logCommand.Flags().BoolVar(&schedulerLog, "scheduler", false, "print the scheduler's log of the job (its sandbox starting, errors) instead")

	cancel := &cobra.Command{
		Use:   "cancel PROJECT JOB...",
		Short: "Cancel jobs of a project",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(command *cobra.Command, args []string) error {
			ledger, cfg, err := options.jobsOf(args[0])
			if err != nil {
				return err
			}
			failed := false
			for _, id := range args[1:] {
				record, err := ledger.Find(id)
				if err == nil {
					var settings *jobs.Settings
					if settings, err = jobs.Setup(cfg, record.Scheduler); err == nil && settings != nil {
						owner, _ := jobs.NewTracker(settings, ledger.Project, func(message string) { fmt.Fprintln(os.Stderr, message) })
						_, err = owner.Cancel(command.Context(), record.ID)
					}
				}
				if err != nil {
					fmt.Fprintf(os.Stderr, "%s: %v\n", id, err)
					failed = true
				}
			}
			if failed {
				return exitError{1}
			}
			return nil
		},
	}
	command.AddCommand(list, show, logCommand, cancel)
	return command
}

// jobsOf opens a project's ledger, with the config.
func (options *projectsOptions) jobsOf(argument string) (jobs.Ledger, *config.Config, error) {
	var ledger jobs.Ledger
	if err := options.directories(); err != nil {
		return ledger, nil, err
	}
	project, err := options.open(argument)
	if err != nil {
		return ledger, nil, err
	}
	cfg, err := options.loadConfig()
	if err != nil {
		return ledger, nil, err
	}
	if _, err := os.Stat(jobs.LedgerDir(project.State)); err != nil {
		return ledger, nil, fmt.Errorf("the project %s has no jobs", project.Name)
	}
	return jobs.Ledger{Dir: jobs.LedgerDir(project.State), Project: project.State}, cfg, nil
}

// refreshJobs asks each scheduler with active jobs about them.
func refreshJobs(ctx context.Context, cfg *config.Config, ledger jobs.Ledger, out io.Writer) {
	records, err := ledger.Records()
	if err != nil {
		return
	}
	var schedulers []string
	for _, record := range records {
		if jobs.Active(record.State) && !slices.Contains(schedulers, record.Scheduler) {
			schedulers = append(schedulers, record.Scheduler)
		}
	}
	for _, name := range schedulers {
		settings, err := jobs.Setup(cfg, name)
		if err != nil || settings == nil {
			fmt.Fprintf(out, "Cannot ask %s about its jobs: %v\n", name, err)
			continue
		}
		tracker, err := jobs.NewTracker(settings, ledger.Project, func(message string) { fmt.Fprintln(out, message) })
		if err != nil {
			continue
		}
		tracker.Poll(ctx)
	}
}

// recentJobs are the active jobs and those that ended in the last week.
func recentJobs(records []*jobs.Record) []*jobs.Record {
	var recent []*jobs.Record
	for _, record := range records {
		if jobs.Active(record.State) || record.Ended == nil || time.Since(*record.Ended) < 7*24*time.Hour {
			recent = append(recent, record)
		}
	}
	return recent
}

func printJobs(out io.Writer, records []*jobs.Record) {
	if len(records) == 0 {
		fmt.Fprintln(out, "No jobs.")
		return
	}
	table := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "JOB\tNAME\tSCHEDULER\tSTATE\tTASKS\tSUBMITTED\tTIME")
	for _, record := range records {
		tasks := "-"
		if record.Tasks > 0 {
			tasks = fmt.Sprint(record.Tasks)
		}
		scheduler := record.Scheduler
		if record.SchedulerID != "" {
			scheduler += " " + record.SchedulerID
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", record.ID, record.Name, scheduler, jobState(record), tasks,
			record.Submitted.Local().Format("2006-01-02 15:04"), jobElapsed(record))
	}
	table.Flush()
}

func jobState(record *jobs.Record) string {
	state := record.State
	if record.ExitCode != nil && *record.ExitCode != 0 {
		state += fmt.Sprintf(" (exit %d)", *record.ExitCode)
	}
	if outcome := record.Outcome; outcome != nil && state != jobs.StateDone {
		state += fmt.Sprintf(" (%d done, %d failed)", outcome.Done, outcome.Failed+outcome.Lost)
	}
	return state
}

func jobElapsed(record *jobs.Record) string {
	if record.Started == nil {
		return "-"
	}
	end := time.Now()
	if record.Ended != nil {
		end = *record.Ended
	}
	return end.Sub(*record.Started).Round(time.Second).String()
}

func showJob(out io.Writer, ledger jobs.Ledger, record *jobs.Record) {
	line := func(label, value string) {
		if value != "" {
			fmt.Fprintf(out, "%-11s %s\n", label, value)
		}
	}
	line("job", record.ID+" ("+record.Name+")")
	scheduler := record.Scheduler
	if record.SchedulerID != "" {
		scheduler += " job " + record.SchedulerID
	}
	line("scheduler", scheduler)
	line("state", jobState(record))
	if record.Error != "" {
		line("error", record.Error)
	}
	quoted := make([]string, len(record.Command))
	for index, arg := range record.Command {
		quoted[index] = jobs.ShellQuote(arg)
	}
	line("command", strings.Join(quoted, " "))
	line("folder", record.Workdir)
	r := record.Resources
	resources := fmt.Sprintf("%d CPUs, %s memory, %s", r.CPUs, jobs.FormatMemory(r.Memory), jobs.FormatDuration(r.Time))
	if r.GPUs > 0 {
		resources += fmt.Sprintf(", %d GPUs", r.GPUs)
		if record.GPUType != "" {
			resources += " (" + record.GPUType + ")"
		}
	}
	if r.Queue != "" {
		resources += ", queue " + r.Queue
	}
	line("resources", resources)
	line("array", record.Array)
	line("after", strings.Join(record.After, ", "))
	line("submitted", record.Submitted.Local().Format(time.DateTime))
	if record.Started != nil {
		line("started", record.Started.Local().Format(time.DateTime))
	}
	if record.Ended != nil {
		line("ended", record.Ended.Local().Format(time.DateTime))
	}
	if record.Agent != "" {
		line("asked by", sandbox.AgentLabel(record.Agent)+", chat "+record.SessionID)
	}
	line("logs", ledger.LogDir(record.ID))
	line("script", filepath.Join(ledger.JobDir(record.ID), jobs.ScriptFile))
}

// jobRunCommand is what a batch script runs: the job's task in a sandbox of
// its own. Not for people: the app writes the spec.
func jobRunCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "job-run SPEC",
		Short:  "Run a batch job's task in a sandbox (what the app's batch scripts run)",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			spec, err := jobs.ReadSpec(args[0])
			if err != nil {
				return err
			}
			// The scheduler stops a job with SIGTERM (cancelled, out of time):
			// stop its sandbox cleanly before it sends SIGKILL.
			ctx, stop := signal.NotifyContext(command.Context(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
			defer stop()
			code, err := sandbox.RunJob(ctx, spec, os.Stderr)
			if err != nil {
				fmt.Fprintf(os.Stderr, "codeflower job-run: %v\n", err)
			}
			if code != 0 {
				return exitError{code}
			}
			return nil
		},
	}
}
