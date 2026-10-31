package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"codeflower-app/internal/config"
	"codeflower-app/internal/sandbox"
)

// Version is set at build time with -ldflags.
var Version = "dev"

type commonOptions struct {
	project string
	state   string
	image   string
	// imageTag picks a build of the published image instead of --image.
	imageTag string
	engine   string
	config   string
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
	addImageTagFlag(command, &options.imageTag)
	command.Flags().StringVar(&options.engine, "engine", defaultEngine,
		"container engine: auto, docker, podman, or apptainer (or set $CODEFLOWER_ENGINE)")
	command.Flags().StringVar(&options.config, "config", "",
		"config directory with config.yaml and skills (or set $CODEFLOWER_CONFIG; default ~/.codeflower)")
}

func addImageTagFlag(command *cobra.Command, tag *string) {
	command.Flags().StringVar(tag, "image-tag", os.Getenv("CODEFLOWER_IMAGE_TAG"),
		"build of the published image to use instead of "+sandbox.DefaultImageTag+", the release this app is made for: latest, another release (0.2.1, or 0.2 for its newest patch) or a commit (sha-abc1234) (or set $CODEFLOWER_IMAGE_TAG)")
}

// imageName combines --image and --image-tag into the image to use; empty
// means the default published image.
func imageName(image, tag string) (string, error) {
	if tag == "" {
		return image, nil
	}
	if image != "" {
		return "", fmt.Errorf("--image and --image-tag (or $CODEFLOWER_IMAGE and $CODEFLOWER_IMAGE_TAG) cannot be used together")
	}
	return sandbox.PublishedImage(tag)
}

// loadConfig reads the config directory (--config, $CODEFLOWER_CONFIG or ~/.codeflower).
func (options commonOptions) loadConfig() (*config.Config, error) {
	return config.Load(config.Directory(options.config))
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

Run "codeflower interactive" for a sandbox to work with from VS Code,
"codeflower run TASK" to start an initial task in a session you can continue
there, or "codeflower task" to let the agent work on a task by itself.

New to the command line? Run "codeflower client" for a window that starts a
sandbox with a few clicks, or connects to one running elsewhere. Run
"codeflower check" to see whether this machine can run sandboxes.`,
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	update := root.Flags().Bool("update", false, "download the latest release and replace this program with it")
	root.RunE = func(command *cobra.Command, _ []string) error {
		if !*update {
			return command.Help()
		}
		u, err := newUpdater(os.Stdout)
		if err != nil {
			return err
		}
		return u.run(command.Context())
	}
	root.AddCommand(interactiveCommand(), runCommand(), taskCommand(), pullCommand(), checkCommand(), clientCommand(), tunnelCommand())
	// Cobra would otherwise exit with "This is a command line tool" when
	// Explorer starts the program, before the window below can open.
	cobra.MousetrapHelpText = ""
	if startedFromDesktop(os.Args[1:]) {
		root.SetArgs([]string{"client", "--desktop"})
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
