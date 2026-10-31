package cli

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"codeflower-app/internal/sandbox"
)

func pullCommand() *cobra.Command {
	var engineName, imageTag string
	command := &cobra.Command{
		Use:   "pull",
		Short: "Download or update the sandbox image",
		Long: `Download the sandbox image ` + sandbox.DefaultImage + `, the release this app
is made for; --image-tag downloads another release (latest for the newest, or
0.9 for the newest patch of 0.9) or a commit's build, or updates one.
"codeflower interactive", "codeflower run" and "codeflower task" download it on first use; on a
cluster, run this once on the login node, as compute nodes often have no
internet access. Apptainer keeps it in ` + sandbox.ImageDirectory() + ` as
` + sandbox.SIFName + ` (set CODEFLOWER_IMAGE_DIR to keep it elsewhere).`,
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
