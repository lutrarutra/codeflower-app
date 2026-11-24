package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"codeflower-app/internal/sandbox"
)

// projectsOptions are the flags of `codeflower projects`: where projects
// and their env folders are.
type projectsOptions struct {
	commonOptions
	// usualProjectsDir is the projects directory without --projects-dir.
	usualProjectsDir string
}

// addFlags adds the flags to the command and every subcommand.
func (options *projectsOptions) addFlags(command *cobra.Command) {
	command.PersistentFlags().StringVar(&options.projectsDir, "projects-dir", "",
		"folder of the projects started with --project (or projects_dir in the config; default "+ProjectsDirectory()+")")
	command.PersistentFlags().StringVar(&options.envDir, "env-dir", "",
		"folder of the projects' env folders, for projects whose settings do not name one (or env_dir in the config)")
	command.PersistentFlags().StringVar(&options.config, "config", "",
		"config directory with config.yaml (or set $CODEFLOWER_CONFIG; default ~/.codeflower)")
}

// directories reads the config for what --projects-dir and --env-dir leave out.
func (options *projectsOptions) directories() error {
	loaded, err := options.loadConfig()
	if err != nil {
		return err
	}
	options.usualProjectsDir = projectsDirectory(loaded)
	options.useDirectories(loaded)
	if options.projectsDir == "" {
		options.projectsDir = options.usualProjectsDir
	}
	return nil
}

// envDirOf is the env directory of the project in state: the one its saved
// settings name (none when they keep it in the project), else --env-dir or
// the config's.
func (options projectsOptions) envDirOf(state string) string {
	if settings, err := sandbox.ReadSettings(sandbox.SettingsFile(state)); err == nil {
		return settings.EnvDir
	}
	return options.envDir
}

// location is where a project named by argument (a name, or a folder) is.
func (options projectsOptions) location(argument string) (string, error) {
	name, folder := projectArgument(argument)
	if name == "" {
		return absolutePath(folder), nil
	}
	return filepath.Join(absolutePath(options.projectsDir), name), nil
}

// open finds the project named by argument.
func (options projectsOptions) open(argument string) (sandbox.Project, error) {
	path, err := options.location(argument)
	if err != nil {
		return sandbox.Project{}, err
	}
	project, err := sandbox.OpenProject(path, options.envDirOf(path))
	if err != nil {
		if name, _ := projectArgument(argument); name != "" {
			return project, fmt.Errorf("no project %s in %s; see `codeflower projects list`", name, options.projectsDir)
		}
		return project, err
	}
	return project, nil
}

// continueName is what `codeflower sandbox --continue` takes for a project:
// its name in the projects directory, else its folder.
func (options projectsOptions) continueName(project sandbox.Project) string {
	projects := absolutePath(options.projectsDir)
	if resolved, err := filepath.EvalSymlinks(projects); err == nil {
		projects = resolved
	}
	if filepath.Dir(project.State) != projects {
		return project.State
	}
	if absolutePath(options.projectsDir) != absolutePath(options.usualProjectsDir) {
		return project.Name + " --projects-dir " + options.projectsDir
	}
	return project.Name
}

func projectsCommand() *cobra.Command {
	var options projectsOptions
	command := &cobra.Command{
		Use:   "projects",
		Short: "List, show, copy, move or remove projects",
		Long: `List, show, copy, move or remove projects.

A project holds a sandbox's chats, agent logins, workspace and tasks, in a
folder in the projects directory (--projects-dir, or projects_dir in the
config). Next to the folder are its saved settings (PROJECT.sandbox.yaml,
for "codeflower sandbox --continue") and its allowlist
(PROJECT.allowed-hosts.txt); its software environments may be in an env
folder elsewhere (--env-dir). Copying, moving and removing a project take
all of these along, and refuse while a sandbox uses the project.

Commands take a project's name, or the path of a project folder elsewhere.`,
		Example: `  codeflower projects list
  codeflower projects show study42
  codeflower projects copy study42 study42-v2
  codeflower projects move study42 /scratch/me/study42
  codeflower projects move-workspace study42 /groups/lab/me/workspaces
  codeflower projects remove study42-v2`,
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return command.Help()
		},
	}
	for _, sub := range []*cobra.Command{
		projectsListCommand(&options), projectsShowCommand(&options), projectsCopyCommand(&options),
		projectsMoveCommand(&options), projectsMoveWorkspaceCommand(&options), projectsRemoveCommand(&options),
	} {
		command.AddCommand(sub)
	}
	options.addFlags(command)
	return command
}

func projectsListCommand(options *projectsOptions) *cobra.Command {
	var withSize bool
	command := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the projects in the projects directory",
		Long: `List the projects in the projects directory.

--size adds the disk space each project itself takes: its folder, settings,
allowlist and batch jobs, not an env or workspace folder kept elsewhere
(` + "`codeflower projects show PROJECT --size`" + ` tells those). It reads every
file, which can take a while for large projects.`,
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := options.directories(); err != nil {
				return err
			}
			projects, err := sandbox.ListProjects(options.projectsDir, "")
			if err != nil {
				return err
			}
			out := command.OutOrStdout()
			if len(projects) == 0 {
				fmt.Fprintf(out, "No projects in %s\n", options.projectsDir)
				return nil
			}
			table := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			header := "NAME\tSTATUS\tLAST STARTED\tSETTINGS\t"
			if withSize {
				header += "SIZE\t"
			}
			fmt.Fprintln(table, header+"PATH")
			for _, project := range projects {
				saved := "saved"
				if settings, err := sandbox.ReadSettings(project.Settings()); errors.Is(err, sandbox.ErrNoSettings) ||
					(err == nil && !settings.Started()) {
					saved = "-"
				} else if err != nil {
					saved = "unreadable"
				}
				fmt.Fprintf(table, "%s\t%s\t%s\t%s\t", project.Name, projectStatus(project),
					formatTime(project.LastStarted()), saved)
				if withSize {
					fmt.Fprintf(table, "%s\t", formatUsage(project.Size()))
				}
				fmt.Fprintln(table, project.State)
			}
			if err := table.Flush(); err != nil {
				return err
			}
			fmt.Fprintf(out, "\nIn %s. Start one again with: codeflower sandbox --continue NAME\n", options.projectsDir)
			return nil
		},
	}
	command.Flags().BoolVar(&withSize, "size", false,
		"also show the disk space each project itself takes, without env and workspace folders elsewhere (slow for large projects)")
	return command
}

// projectStatus says whether sandboxes use the project on this computer.
func projectStatus(project sandbox.Project) string {
	switch running := project.Running(); len(running) {
	case 0:
		return "stopped"
	case 1:
		return "running"
	default:
		return fmt.Sprintf("running (%d)", len(running))
	}
}

// formatUsage writes a disk space, with ">" when some files could not be
// read.
func formatUsage(bytes int64, complete bool) string {
	if complete {
		return formatSize(bytes)
	}
	return ">" + formatSize(bytes)
}

// formatSize writes a size in binary units, with one decimal from KiB up.
func formatSize(bytes int64) string {
	if bytes < 1<<10 {
		return fmt.Sprintf("%d B", bytes)
	}
	value := float64(bytes)
	unit := 0
	for value >= 1<<10 && unit < 4 {
		value /= 1 << 10
		unit++
	}
	return fmt.Sprintf("%.1f %s", value, []string{"B", "KiB", "MiB", "GiB", "TiB"}[unit])
}

func formatTime(moment time.Time) string {
	if moment.IsZero() {
		return "-"
	}
	return moment.Local().Format("2006-01-02 15:04")
}

func projectsShowCommand(options *projectsOptions) *cobra.Command {
	var withSize bool
	command := &cobra.Command{
		Use:   "show PROJECT",
		Short: "Show where a project's files are and how it starts",
		Long: `Show where a project's files are and how it starts.

--size adds the disk space of the project itself (its folder, settings,
allowlist and batch jobs), and of its env and workspace folders when they
are kept elsewhere. It reads every file, which can take a while.`,
		Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			if err := options.directories(); err != nil {
				return err
			}
			project, err := options.open(args[0])
			if err != nil {
				return err
			}
			out := command.OutOrStdout()
			line := func(label, value string) { fmt.Fprintf(out, "  %-10s %s\n", label, value) }
			fmt.Fprintln(out, project.Name)
			// With its size, when asked: what the folder at path takes.
			sized := func(path string) string {
				if !withSize {
					return path
				}
				return fmt.Sprintf("%s (%s)", path, formatUsage(sandbox.FolderSize(path)))
			}
			if withSize {
				line("folder", fmt.Sprintf("%s (%s with settings and jobs)", project.State, formatUsage(project.Size())))
			} else {
				line("folder", project.State)
			}
			line("status", projectStatus(project))
			for _, running := range project.Running() {
				line("", fmt.Sprintf("%s with %s since %s (process %d)", running.Kind, running.Engine,
					formatTime(running.Started), running.PID))
			}
			if project.Env != "" {
				line("env", sized(project.Env))
			}
			if project.Workspace != "" {
				line("workspace", sized(project.Workspace))
			} else if dir, err := sandbox.SavedWorkspaceDir(project.State); err == nil && dir != "" {
				line("workspace", sandbox.WorkspaceFolder(dir, project.State)+" (missing: made when the project starts)")
			}
			if _, err := os.Stat(project.Allowlist()); err == nil {
				line("allowlist", project.Allowlist())
			}
			if transfer, found, _ := sandbox.ReadTransfer(project.State); found {
				line("unfinished", fmt.Sprintf("%s to %s, started %s (run it again to continue, or with --abort)",
					transfer.Operation, transfer.To, formatTime(transfer.Started)))
			}
			settings, err := sandbox.ReadSettings(project.Settings())
			if errors.Is(err, sandbox.ErrNoSettings) || (err == nil && !settings.Started()) {
				line("settings", "none saved yet: they are saved when `codeflower sandbox` starts the project")
				return nil
			}
			if err != nil {
				line("settings", err.Error())
				return nil
			}
			line("settings", project.Settings())
			engine := settings.Engine
			if settings.Image != "" {
				engine += ", " + settings.Image
			}
			if settings.EngineArgs != "" {
				engine += ", " + settings.EngineArgs
			}
			line("engine", engine)
			for _, folders := range []struct {
				label string
				paths []string
			}{{"input", settings.Inputs}, {"output", settings.Outputs}, {"code", settings.Code},
				{"skills", settings.Skills}, {"bind", settings.Binds}} {
				for _, path := range folders.paths {
					line(folders.label, path)
				}
			}
			for _, value := range settings.Env {
				name, _, _ := strings.Cut(value, "=")
				line("env", name)
			}
			if settings.IsolateNetwork {
				line("network", "isolated")
			}
			port := "any free one"
			if settings.Port > 0 {
				port = fmt.Sprint(settings.Port)
			}
			line("port", port)
			fmt.Fprintf(out, "\nStart it again with: codeflower sandbox --continue %s\n", options.continueName(project))
			return nil
		},
	}
	command.Flags().BoolVar(&withSize, "size", false,
		"also show the disk space of the project and of its env and workspace folders (slow for large projects)")
	return command
}

func projectsCopyCommand(options *projectsOptions) *cobra.Command {
	var abort bool
	command := &cobra.Command{
		Use:   "copy PROJECT NEW",
		Short: "Copy a project, e.g. to try something without changing it",
		Long: `Copy a project, with its chats, agent logins, workspace, tasks, settings,
allowlist, env folder and workspace folder. NEW is a name in the projects directory, or a
folder elsewhere.

The copy gets its own key and certificate, so the original's links do not
open it, and takes any free port when it starts.

` + transferHelp,
		Aliases: []string{"cp"},
		Args:    cobra.RangeArgs(1, 2),
		RunE: func(command *cobra.Command, args []string) error {
			return options.copyOrMove(command, args, abort, "Copied", sandbox.CopyProject)
		},
	}
	command.Flags().BoolVar(&abort, "abort", false, "drop an unfinished copy: delete what it copied so far")
	return command
}

func projectsMoveCommand(options *projectsOptions) *cobra.Command {
	var abort bool
	command := &cobra.Command{
		Use:   "move PROJECT NEW",
		Short: "Rename a project, or move it to another folder",
		Long: `Rename a project, or move it to another folder, with its settings,
allowlist, env folder and workspace folder. NEW is a name in the projects directory, or a
folder elsewhere. It keeps its key and port, so its links still connect.

` + transferHelp,
		Aliases: []string{"mv", "rename"},
		Args:    cobra.RangeArgs(1, 2),
		RunE: func(command *cobra.Command, args []string) error {
			return options.copyOrMove(command, args, abort, "Moved", sandbox.MoveProject)
		},
	}
	command.Flags().BoolVar(&abort, "abort", false, "drop an unfinished move: delete what it copied so far")
	return command
}

// transferHelp explains how copies between file systems stop and continue.
const transferHelp = `Between file systems the folders are copied, keeping modification times
(which make, Snakemake and the like go by) and hard links, several files at a
time. A copy that is stopped (Ctrl+C) or fails keeps what it copied, hidden;
the same command again continues it, copying only what is missing, and
--abort drops it. Until it is complete, the project stays as it was.`

func projectsMoveWorkspaceCommand(options *projectsOptions) *cobra.Command {
	var intoProject, abort bool
	command := &cobra.Command{
		Use:   "move-workspace PROJECT [DIR]",
		Short: "Move a project's workspace to another folder, e.g. one with more room",
		Long: `Move a project's workspace (the agent's scripts, downloads, intermediate and
processed data) to DIR/PROJECT, e.g. on group storage when the home directory
has a quota, or back into the project with --into-project. The project keeps
its chats, logins and settings where they are, and records where its
workspace is in its settings, so its sandboxes mount it from there; inside
the sandbox the workspace's path stays the same.

The config's workspace_dir only chooses for new projects: this moves an
existing one's. Refused while a sandbox uses the project.

` + transferHelp + ` The copy shows how much is left once its files are
counted, which runs alongside.`,
		Example: `  codeflower projects move-workspace study42 /groups/lab/me/workspaces
  codeflower projects move-workspace study42 --into-project`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(command *cobra.Command, args []string) error {
			if abort && (intoProject || len(args) == 2) {
				return fmt.Errorf("--abort takes only the project")
			}
			if !abort && intoProject == (len(args) == 2) {
				return fmt.Errorf("give a folder to move the workspace to, or --into-project")
			}
			if err := options.directories(); err != nil {
				return err
			}
			project, err := options.open(args[0])
			if err != nil {
				return err
			}
			if abort {
				return abortTransfer(command, project)
			}
			dir := ""
			if len(args) == 2 {
				dir = absolutePath(args[1])
			}
			ctx, stop := interruptible()
			defer stop()
			progress, clear := projectProgress(os.Stderr, project.Name)
			result, err := sandbox.MoveWorkspace(ctx, project, dir, progress)
			clear()
			if err != nil {
				return unfinished(project, err)
			}
			where := filepath.Join(result.State, "workspace")
			if result.Workspace != "" {
				where = result.Workspace
			}
			fmt.Fprintf(command.OutOrStdout(), "Moved the workspace of %s to %s\n", project.Name, where)
			return nil
		},
	}
	command.Flags().BoolVar(&intoProject, "into-project", false, "move the workspace back into the project folder")
	command.Flags().BoolVar(&abort, "abort", false, "drop an unfinished move of the workspace: delete what it copied so far")
	return command
}

func (options *projectsOptions) copyOrMove(command *cobra.Command, args []string, abort bool, done string,
	action func(context.Context, sandbox.Project, string, string, sandbox.Progress) (sandbox.Project, error)) error {
	if abort != (len(args) == 1) {
		if abort {
			return fmt.Errorf("--abort takes only the project")
		}
		return fmt.Errorf("give the project and where it goes")
	}
	if err := options.directories(); err != nil {
		return err
	}
	project, err := options.open(args[0])
	if err != nil {
		return err
	}
	if abort {
		return abortTransfer(command, project)
	}
	target, err := options.location(args[1])
	if err != nil {
		return err
	}
	ctx, stop := interruptible()
	defer stop()
	progress, clear := projectProgress(os.Stderr, project.Name)
	result, err := action(ctx, project, target, "", progress)
	clear()
	if err != nil {
		if result.State != "" {
			// Moved, but the original is not all removed.
			if errors.Is(err, context.Canceled) {
				return fmt.Errorf("stopped: the project is in %s now, but its original is not all removed; "+
					"run `codeflower projects remove %s` to finish", result.State, project.State)
			}
			return fmt.Errorf("%w\nRun `codeflower projects remove %s` to finish", err, project.State)
		}
		return unfinished(project, err)
	}
	fmt.Fprintf(command.OutOrStdout(), "%s %s to %s\nStart it with: codeflower sandbox --continue %s\n",
		done, project.State, result.State, options.continueName(result))
	return nil
}

// unfinished explains a copy or move that stopped or failed: what it kept,
// and how to go on.
func unfinished(project sandbox.Project, err error) error {
	var pending sandbox.PendingTransferError
	if errors.As(err, &pending) {
		return err
	}
	stopped := errors.Is(err, context.Canceled)
	transfer, found, _ := sandbox.ReadTransfer(project.State)
	switch {
	case found && len(transfer.Partial) > 0:
		what := "stopped"
		if !stopped {
			what = err.Error()
		}
		return fmt.Errorf("%s; the project %s is as it was. What was copied to %s is kept: run the same command "+
			"again to continue, or `codeflower projects %s %s --abort` to delete it",
			what, project.Name, transfer.To, transfer.Operation, project.State)
	case found && len(transfer.Removing) > 0:
		if stopped {
			return fmt.Errorf("stopped: the workspace is in %s now, but its original is not all removed; "+
				"run the same command again to finish", transfer.To)
		}
		return err
	case stopped:
		return fmt.Errorf("stopped; the project %s is as it was", project.Name)
	}
	return err
}

// abortTransfer drops the project's unfinished copy or move.
func abortTransfer(command *cobra.Command, project sandbox.Project) error {
	ctx, stop := interruptible()
	defer stop()
	progress, clear := projectProgress(os.Stderr, project.Name)
	transfer, err := sandbox.AbortTransfer(ctx, project, progress)
	clear()
	if err != nil {
		return err
	}
	fmt.Fprintf(command.OutOrStdout(), "Dropped the unfinished %s of %s to %s; the project is as it was\n",
		transfer.Operation, project.Name, transfer.To)
	return nil
}

func projectsRemoveCommand(options *projectsOptions) *cobra.Command {
	var yes bool
	command := &cobra.Command{
		Use:   "remove PROJECT",
		Short: "Delete a project and everything in it",
		Long: `Delete a project: its chats, agent logins, workspace and tasks, its settings,
allowlist, env folder and workspace folder. Your input, output and code folders are not touched.
It asks first, unless --yes is given.

A removal that is stopped (Ctrl+C) or fails leaves the rest hidden next to
the project (.PROJECT.removing); running the same command again finishes it.`,
		Aliases: []string{"rm"},
		Args:    cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			if err := options.directories(); err != nil {
				return err
			}
			project, err := options.open(args[0])
			leftover := false
			if err != nil {
				// What a stopped removal (or move) left: removed without asking again.
				var found bool
				if path, pathErr := options.location(args[0]); pathErr == nil {
					project, found = sandbox.LeftoverRemoval(path, options.envDirOf(sandbox.RemovingPath(path)))
				}
				if !found {
					return err
				}
				leftover = true
			}
			if !yes && !leftover {
				if !interactive() {
					return fmt.Errorf("give --yes to remove the project %s without asking", project.Name)
				}
				if !confirmRemove(os.Stdin, os.Stderr, project) {
					return fmt.Errorf("the project %s was not removed", project.Name)
				}
			}
			ctx, stop := interruptible()
			defer stop()
			progress, clear := projectProgress(os.Stderr, project.Name)
			err = sandbox.RemoveProject(ctx, project, progress)
			clear()
			if errors.Is(err, context.Canceled) {
				return fmt.Errorf("stopped removing the project %s; run `codeflower projects remove %s` again to finish",
					project.Name, args[0])
			}
			if err != nil {
				return fmt.Errorf("%w\nRun `codeflower projects remove %s` again to finish", err, args[0])
			}
			fmt.Fprintf(command.OutOrStdout(), "Removed the project %s\n", project.Name)
			return nil
		},
	}
	command.Flags().BoolVarP(&yes, "yes", "y", false, "remove without asking")
	return command
}

// confirmRemove asks before a project is deleted.
func confirmRemove(in io.Reader, out io.Writer, project sandbox.Project) bool {
	fmt.Fprintf(out, "This deletes the project %s: its chats, agent logins, workspace and tasks.\n", project.Name)
	fmt.Fprintf(out, "  %s\n", project.State)
	for _, path := range []string{project.Settings(), project.Allowlist(), project.Env, project.Workspace} {
		if path != "" {
			if _, err := os.Lstat(path); err == nil {
				fmt.Fprintf(out, "  %s\n", path)
			}
		}
	}
	fmt.Fprintf(out, "Remove it? [y/N] ")
	line, _ := bufio.NewReader(in).ReadString('\n')
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}

// interruptible is a context that the first Ctrl+C (or SIGTERM) ends;
// another one then stops the program at once.
func interruptible() (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop()
	}()
	return ctx, stop
}

// projectProgress shows on a terminal how a long copy, move or removal of
// the project name goes; clear erases it again.
func projectProgress(out *os.File, name string) (progress sandbox.Progress, clear func()) {
	if !isTerminal(out) {
		return nil, func() {}
	}
	// Only the meter's goroutine writes, and clear runs after it stopped.
	shown := 0
	progress = func(report sandbox.ProgressReport) {
		line := fmt.Sprintf("%s %s: %s", report.Doing, name, progressLine(report))
		fmt.Fprintf(out, "\r%-*s", shown, line)
		shown = max(shown, len(line))
	}
	return progress, func() {
		if shown > 0 {
			fmt.Fprintf(out, "\r%*s\r", shown, "")
		}
	}
}

// progressLine says how far a copy, move or removal has come: files and
// bytes done, of the total as far as counted ("≥" while counting), and the
// speed of the bytes written.
func progressLine(report sandbox.ProgressReport) string {
	of := ""
	if report.Counted {
		of = " of "
	} else if report.TotalFiles > 0 {
		of = " of ≥"
	}
	line := thousands(report.Files)
	if of != "" {
		line += of + thousands(max(report.TotalFiles, report.Files))
	}
	line += " files"
	if report.Bytes > 0 || report.TotalBytes > 0 {
		line += ", " + humanBytes(report.Bytes)
		if of != "" && report.TotalBytes > 0 {
			line += of + humanBytes(max(report.TotalBytes, report.Bytes))
		}
	}
	if report.Counted && report.TotalBytes > 0 {
		line += fmt.Sprintf(" (%d%%)", min(100, report.Bytes*100/report.TotalBytes))
	} else if report.Counted && report.TotalFiles > 0 {
		line += fmt.Sprintf(" (%d%%)", min(100, report.Files*100/report.TotalFiles))
	}
	if seconds := report.Elapsed.Seconds(); seconds >= 1 && report.Written > 0 {
		line += ", " + humanBytes(int64(float64(report.Written)/seconds)) + "/s"
	}
	return line
}

// humanBytes writes a size in decimal units: 950 B, 6.3 GB.
func humanBytes(size int64) string {
	const unit = 1000
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	value, suffix := float64(size), ""
	for _, next := range []string{"kB", "MB", "GB", "TB", "PB"} {
		value /= unit
		suffix = next
		if value < unit {
			break
		}
	}
	return fmt.Sprintf("%.1f %s", value, suffix)
}

// thousands writes a count with commas: 1,234,567.
func thousands(count int64) string {
	digits := fmt.Sprint(count)
	var result strings.Builder
	for index, digit := range digits {
		if index > 0 && (len(digits)-index)%3 == 0 {
			result.WriteByte(',')
		}
		result.WriteRune(digit)
	}
	return result.String()
}
