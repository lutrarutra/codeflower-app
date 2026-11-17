package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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
  codeflower projects remove study42-v2`,
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return command.Help()
		},
	}
	for _, sub := range []*cobra.Command{
		projectsListCommand(&options), projectsShowCommand(&options), projectsCopyCommand(&options),
		projectsMoveCommand(&options), projectsRemoveCommand(&options),
	} {
		command.AddCommand(sub)
	}
	options.addFlags(command)
	return command
}

func projectsListCommand(options *projectsOptions) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the projects in the projects directory",
		Args:    cobra.NoArgs,
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
			fmt.Fprintln(table, "NAME\tSTATUS\tLAST STARTED\tSETTINGS\tSIZE\tPATH")
			for _, project := range projects {
				// Open it again with its env folder, which its size includes.
				if withEnv, err := sandbox.OpenProject(project.State, options.envDirOf(project.State)); err == nil {
					project = withEnv
				}
				saved := "saved"
				if _, err := sandbox.ReadSettings(project.Settings()); errors.Is(err, sandbox.ErrNoSettings) {
					saved = "-"
				} else if err != nil {
					saved = "unreadable"
				}
				fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\n", project.Name, projectStatus(project),
					formatTime(project.LastStarted()), saved, projectSize(project), project.State)
			}
			if err := table.Flush(); err != nil {
				return err
			}
			fmt.Fprintf(out, "\nIn %s. Start one again with: codeflower sandbox --continue NAME\n", options.projectsDir)
			return nil
		},
	}
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

// projectSize is the disk space a project takes, with ">" when some of its
// files could not be read.
func projectSize(project sandbox.Project) string {
	bytes, complete := project.Size()
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
	return &cobra.Command{
		Use:   "show PROJECT",
		Short: "Show where a project's files are and how it starts",
		Args:  cobra.ExactArgs(1),
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
			line("folder", project.State)
			line("status", projectStatus(project))
			for _, running := range project.Running() {
				line("", fmt.Sprintf("%s with %s since %s (process %d)", running.Kind, running.Engine,
					formatTime(running.Started), running.PID))
			}
			if project.Env != "" {
				line("env", project.Env)
			}
			if _, err := os.Stat(project.Allowlist()); err == nil {
				line("allowlist", project.Allowlist())
			}
			settings, err := sandbox.ReadSettings(project.Settings())
			if errors.Is(err, sandbox.ErrNoSettings) {
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
}

func projectsCopyCommand(options *projectsOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "copy PROJECT NEW",
		Short: "Copy a project, e.g. to try something without changing it",
		Long: `Copy a project, with its chats, agent logins, workspace, tasks, settings,
allowlist and env folder. NEW is a name in the projects directory, or a
folder elsewhere.

The copy gets its own key and certificate, so the original's links do not
open it, and takes any free port when it starts.`,
		Aliases: []string{"cp"},
		Args:    cobra.ExactArgs(2),
		RunE: func(command *cobra.Command, args []string) error {
			return options.copyOrMove(command, args, "Copied", sandbox.CopyProject)
		},
	}
}

func projectsMoveCommand(options *projectsOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "move PROJECT NEW",
		Short: "Rename a project, or move it to another folder",
		Long: `Rename a project, or move it to another folder, with its settings,
allowlist and env folder. NEW is a name in the projects directory, or a
folder elsewhere. It keeps its key and port, so its links still connect.`,
		Aliases: []string{"mv", "rename"},
		Args:    cobra.ExactArgs(2),
		RunE: func(command *cobra.Command, args []string) error {
			return options.copyOrMove(command, args, "Moved", sandbox.MoveProject)
		},
	}
}

func (options *projectsOptions) copyOrMove(command *cobra.Command, args []string, done string,
	action func(sandbox.Project, string, string) (sandbox.Project, error)) error {
	if err := options.directories(); err != nil {
		return err
	}
	project, err := options.open(args[0])
	if err != nil {
		return err
	}
	target, err := options.location(args[1])
	if err != nil {
		return err
	}
	result, err := action(project, target, "")
	if err != nil {
		return err
	}
	fmt.Fprintf(command.OutOrStdout(), "%s %s to %s\nStart it with: codeflower sandbox --continue %s\n",
		done, project.State, result.State, options.continueName(result))
	return nil
}

func projectsRemoveCommand(options *projectsOptions) *cobra.Command {
	var yes bool
	command := &cobra.Command{
		Use:   "remove PROJECT",
		Short: "Delete a project and everything in it",
		Long: `Delete a project: its chats, agent logins, workspace and tasks, its settings,
allowlist and env folder. Your input, output and code folders are not touched.
It asks first, unless --yes is given.`,
		Aliases: []string{"rm"},
		Args:    cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			if err := options.directories(); err != nil {
				return err
			}
			project, err := options.open(args[0])
			if err != nil {
				return err
			}
			if !yes {
				if !interactive() {
					return fmt.Errorf("give --yes to remove the project %s without asking", project.Name)
				}
				if !confirmRemove(os.Stdin, os.Stderr, project) {
					return fmt.Errorf("the project %s was not removed", project.Name)
				}
			}
			if err := sandbox.RemoveProject(project); err != nil {
				return err
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
	for _, path := range []string{project.Settings(), project.Allowlist(), project.Env} {
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
