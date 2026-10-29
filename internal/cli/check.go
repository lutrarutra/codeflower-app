package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"codeflower-launcher/internal/sandbox"
)

func printSteps(out io.Writer, steps []sandbox.Step, indent string) {
	for _, step := range steps {
		fmt.Fprintf(out, "%s- %s\n", indent, step.Text)
		if step.Command != "" {
			for _, line := range strings.Split(step.Command, "\n") {
				fmt.Fprintf(out, "%s    %s\n", indent, line)
			}
		}
		if step.Link != "" {
			fmt.Fprintf(out, "%s    %s\n", indent, step.Link)
		}
	}
}

// printGuide explains how to install a container engine on this system.
func printGuide(out io.Writer) {
	guide := sandbox.Guide()
	fmt.Fprintf(out, "\nTo run sandboxes on %s, install one of these:\n", guide.System)
	for index, option := range guide.Options {
		fmt.Fprintf(out, "\n%d. %s\n", index+1, option.Title)
		if option.Note != "" {
			fmt.Fprintf(out, "   %s\n", option.Note)
		}
		printSteps(out, option.Steps, "   ")
	}
	fmt.Fprintln(out, "\nThen run `codeflower check` again.")
}

// engineMissing explains why no engine can run the sandbox, and how to fix it.
func engineMissing(out io.Writer, engine sandbox.EngineStatus, err error) error {
	if engine.Installed && len(engine.Fix) > 0 {
		fmt.Fprintf(out, "%s\n", engine.Problem)
		printSteps(out, engine.Fix, "")
		return exitError{3}
	}
	if errors.Is(err, sandbox.ErrNoEngine) || (engine.Name != "" && !engine.Installed) {
		if !errors.Is(err, sandbox.ErrNoEngine) {
			fmt.Fprintf(out, "%v.\n", err)
			printGuide(out)
			return exitError{3}
		}
		fmt.Fprintln(out, "No container engine was found: code flower needs Docker, Podman or Apptainer to run its sandbox.")
		printGuide(out)
		return exitError{3}
	}
	return err
}

func checkCommand() *cobra.Command {
	var image, engineName string
	command := &cobra.Command{
		Use:   "check",
		Short: "Check that this machine can run sandboxes, and how to set it up",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			out := os.Stdout
			statuses := sandbox.DetectEngines(command.Context())
			fmt.Fprintln(out, "Container engines:")
			for _, status := range statuses {
				state := "not installed"
				switch {
				case status.Ready:
					state = "ready"
				case status.Installed:
					state = "installed, not usable: " + status.Problem
				}
				version := ""
				if status.Version != "" {
					version = " " + status.Version
				}
				fmt.Fprintf(out, "  %-10s %s%s\n", status.Name, state, version)
			}
			engine, err := sandbox.ChooseEngine(statuses, engineName, image)
			if err != nil {
				fmt.Fprintln(out)
				return engineMissing(out, engine, err)
			}
			fmt.Fprintf(out, "\nSandboxes will run with %s.\n", engine.Label)
			if err := sandbox.CheckImage(command.Context(), engine, image); err != nil {
				fmt.Fprintf(out, "\nThe sandbox image is missing. %v.\n", err)
				printSteps(out, sandbox.ImageHelp(engine.Name), "")
				return exitError{4}
			}
			resolved, _ := sandbox.ResolveImage(image, engine.Name)
			fmt.Fprintf(out, "The sandbox image %s is ready.\n", resolved)
			fmt.Fprintln(out, "\nStart one with:  codeflower run --agent claude --input DATA_FOLDER  (or: codeflower gui)")
			return nil
		},
	}
	defaultEngine := os.Getenv("CODEFLOWER_ENGINE")
	if defaultEngine == "" {
		defaultEngine = "auto"
	}
	command.Flags().StringVar(&image, "image", os.Getenv("CODEFLOWER_IMAGE"), "sandbox image to look for")
	command.Flags().StringVar(&engineName, "engine", defaultEngine, "engine to check: auto, docker, podman, or apptainer")
	return command
}
