package cli

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"codeflower-launcher/internal/sandbox"
)

func pullCommand() *cobra.Command {
	var engineName, imageTag string
	command := &cobra.Command{
		Use:   "pull",
		Short: "Download or update the sandbox image",
		Long: `Download the sandbox image ` + sandbox.DefaultImage + `, or update it to the
newest release; --image-tag downloads another release or a commit's build.
"codeflower interactive" and "codeflower task" download it on first use; on a
cluster, run this once on the login node, as compute nodes often have no
internet access. Apptainer keeps it as ` + sandbox.DefaultSIF() + `
(named after the tag; set CODEFLOWER_IMAGE_DIR to keep it elsewhere).`,
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			image, err := imageName("", imageTag)
			if err != nil {
				return err
			}
			engine, err := sandbox.ChooseEngine(sandbox.DetectEngines(command.Context()), engineName, "")
			if err != nil {
				return engineMissing(os.Stderr, engine, err)
			}
			ctx, stop := signal.NotifyContext(command.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			fmt.Fprintf(os.Stderr, "Downloading %s with %s…\n", sandbox.PublishedName(image), engine.Label)
			if err := sandbox.PullImage(ctx, engine, image, os.Stderr); err != nil {
				return err
			}
			resolved, _ := sandbox.ResolveImage(image, engine.Name)
			fmt.Fprintf(os.Stderr, "The sandbox image %s is ready.\n", resolved)
			return nil
		},
	}
	defaultEngine := os.Getenv("CODEFLOWER_ENGINE")
	if defaultEngine == "" {
		defaultEngine = "auto"
	}
	command.Flags().StringVar(&engineName, "engine", defaultEngine, "engine to download it for: auto, docker, podman, or apptainer")
	addImageTagFlag(command, &imageTag)
	return command
}
