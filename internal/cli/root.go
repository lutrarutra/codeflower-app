package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"codeflower-launcher/internal/sandbox"
)

// Version is set at build time with -ldflags.
var Version = "dev"

type commonOptions struct {
	project string
	state   string
	image   string
	engine  string
}

// ProjectsDirectory holds projects started by name (--project, or the GUI).
func ProjectsDirectory() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "codeflower-projects"
	}
	return filepath.Join(home, "codeflower", "projects")
}

func addCommonFlags(command *cobra.Command, options *commonOptions) {
	defaultEngine := os.Getenv("CODEFLOWER_ENGINE")
	if defaultEngine == "" {
		defaultEngine = "auto"
	}
	command.Flags().StringVarP(&options.project, "project", "p", "default",
		"project name: its sessions, logins and workspace live in "+filepath.Join(ProjectsDirectory(), "NAME"))
	command.Flags().StringVar(&options.state, "state", "", "project directory, instead of --project (created with owner-only permissions)")
	command.Flags().StringVar(&options.image, "image", os.Getenv("CODEFLOWER_IMAGE"),
		"sandbox image: a Docker/Podman image name, or an Apptainer .sif file (or set $CODEFLOWER_IMAGE; default "+sandbox.DefaultImage+", downloaded on first use)")
	command.Flags().StringVar(&options.engine, "engine", defaultEngine,
		"container engine: auto, docker, podman, or apptainer (or set $CODEFLOWER_ENGINE)")
}

func (options commonOptions) validate() error {
	if options.engine != "auto" && sandbox.EngineLabel(options.engine) == options.engine {
		return fmt.Errorf("--engine must be auto, %s", strings.Join(sandbox.EngineNames, ", "))
	}
	if options.state == "" && !validProjectName(options.project) {
		return fmt.Errorf("--project must be a simple name (letters, digits, '-', '_' and '.'), or use --state DIR")
	}
	return nil
}

func validProjectName(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > 100 {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

func (options commonOptions) stateDirectory() string {
	if options.state != "" {
		return options.state
	}
	return filepath.Join(ProjectsDirectory(), options.project)
}

// exitError carries the sandbox's exit code through cobra.
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// startedFromDesktop guesses that the binary was double-clicked rather than
// typed in a terminal, so it opens the window instead of printing help: no
// arguments, and no terminal, a console of its own (Windows), or a local Mac
// (Finder runs programs in a new Terminal window).
func startedFromDesktop(args []string) bool {
	if len(args) > 0 {
		return false
	}
	if ownConsole() {
		return true
	}
	if runtime.GOOS == "darwin" && os.Getenv("SSH_CONNECTION") == "" {
		return true
	}
	info, err := os.Stdout.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice == 0 && !interactive()
}

// Execute runs the command line and returns the process exit code.
func Execute() int {
	root := &cobra.Command{
		Use:   "codeflower",
		Short: "Start code flower sandboxes for Claude Code, Codex, or DeepSeek Harness",
		Long: `Start code flower sandboxes for Claude Code, Codex, or DeepSeek Harness.

New to the command line? Run "codeflower gui" for a window that starts a
sandbox with a few clicks. Run "codeflower check" to see whether this
machine can run sandboxes.`,
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(runCommand(), pullCommand(), checkCommand(), guiCommand())
	if startedFromDesktop(os.Args[1:]) {
		root.SetArgs([]string{"gui"})
	}
	if err := root.Execute(); err != nil {
		if exit, ok := err.(exitError); ok {
			return exit.code
		}
		fmt.Fprintf(os.Stderr, "codeflower: %v\n", err)
		return 1
	}
	return 0
}
