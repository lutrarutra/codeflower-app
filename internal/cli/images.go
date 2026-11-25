package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"codeflower-app/internal/client"
	"codeflower-app/internal/config"
	"codeflower-app/internal/sandbox"
)

func imagesCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "images",
		Short: "List the sandbox images on this computer, or remove those no longer needed",
		Long: `List the builds of the sandbox image (` + sandbox.ImageRepository + `) the engine
keeps here, or remove those no longer needed.

Each update of the app runs a newer release of the image and leaves the older
one behind, a few GB each. An image is kept when:
  - it is the release this app runs (` + sandbox.DefaultImageTag + `)
  - a sandbox or container uses it, running or stopped
  - a project's saved settings, the client window, the config's image or
    $CODEFLOWER_IMAGE / $CODEFLOWER_IMAGE_TAG choose it
  - it is not older than the release this app runs (a newer release or
    latest you downloaded to try)
The others are unused: older releases and commits' builds, and older
downloads a newer one of their tag replaced. With Podman, the copies saved in
the image folder (image_dir) are reviewed the same way.`,
		Example: `  codeflower images list
  codeflower images prune --dry-run
  codeflower images prune --engine podman --yes`,
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return command.Help()
		},
	}
	command.AddCommand(imagesListCommand(), imagesPruneCommand())
	return command
}

// imagesOptions are the flags of `codeflower images`: the engine, and where
// the projects whose saved settings may choose an image are.
type imagesOptions struct {
	commonOptions
}

func (options *imagesOptions) addFlags(command *cobra.Command) {
	defaultEngine := os.Getenv("CODEFLOWER_ENGINE")
	if defaultEngine == "" {
		defaultEngine = "auto"
	}
	command.Flags().StringVar(&options.engine, "engine", defaultEngine, "engine whose images to review: auto, docker, podman, or apptainer")
	addProjectsDirFlag(command, &options.commonOptions)
	command.Flags().StringVar(&options.config, "config", "",
		"config directory with config.yaml (or set $CODEFLOWER_CONFIG; default ~/.codeflower)")
}

// review reviews the images of the engine the flags (or the config) choose.
func (options *imagesOptions) review(command *cobra.Command) (sandbox.EngineStatus, []sandbox.ImageReview, error) {
	loaded, err := options.loadConfig()
	if err != nil {
		return sandbox.EngineStatus{}, nil, err
	}
	sandbox.SetImageDir(loaded.ImageDirectory())
	if !command.Flags().Changed("engine") && os.Getenv("CODEFLOWER_ENGINE") == "" && loaded.Engine != "" {
		options.engine = loaded.Engine
	}
	if options.engine != "auto" && sandbox.EngineLabel(options.engine) == options.engine {
		return sandbox.EngineStatus{}, nil, fmt.Errorf("--engine must be auto, %s", strings.Join(sandbox.EngineNames, ", "))
	}
	engine, err := sandbox.ChooseEngine(sandbox.DetectEngines(command.Context()), options.engine, "")
	if err != nil {
		return engine, nil, engineMissing(os.Stderr, engine, err)
	}
	if options.projectsDir == "" {
		options.projectsDir = projectsDirectory(loaded)
	}
	reviews, err := sandbox.ReviewImages(command.Context(), engine, chosenImages(loaded, options.projectsDir))
	return engine, reviews, err
}

// chosenImages are the images the config, the environment, the client
// window and the projects' saved settings choose, with who chose them.
func chosenImages(loaded *config.Config, projectsDir string) map[string]string {
	chosen := map[string]string{}
	add := func(name, who string) {
		if name != "" {
			if _, known := chosen[name]; !known {
				chosen[name] = who
			}
		}
	}
	if tag := os.Getenv("CODEFLOWER_IMAGE_TAG"); tag != "" {
		if name, err := sandbox.PublishedImage(tag); err == nil {
			add(name, "$CODEFLOWER_IMAGE_TAG chooses it")
		}
	}
	add(os.Getenv("CODEFLOWER_IMAGE"), "$CODEFLOWER_IMAGE chooses it")
	add(loaded.Image, "the config's image")
	if name, err := client.SavedImage(); err == nil {
		add(name, "the client window chooses it")
	}
	byImage := map[string][]string{}
	projects, _ := sandbox.ListProjects(projectsDir, "")
	for _, project := range projects {
		if settings, err := sandbox.ReadSettings(project.Settings()); err == nil && settings.Image != "" {
			byImage[settings.Image] = append(byImage[settings.Image], project.Name)
		}
	}
	for name, names := range byImage {
		who := "project " + names[0]
		if len(names) > 1 {
			who = "projects " + strings.Join(names, ", ")
		}
		add(name, who+" chooses it")
	}
	return chosen
}

func imagesListCommand() *cobra.Command {
	var options imagesOptions
	command := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the sandbox images and whether they are still needed",
		Args:    cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			engine, reviews, err := options.review(command)
			if err != nil {
				return err
			}
			out := command.OutOrStdout()
			if len(reviews) == 0 {
				fmt.Fprintf(out, "%s has no sandbox images.\n", engine.Label)
				return nil
			}
			if err := writeImages(out, reviews, true); err != nil {
				return err
			}
			unused := slices.DeleteFunc(slices.Clone(reviews), func(review sandbox.ImageReview) bool { return !review.Unused })
			if len(unused) > 0 {
				fmt.Fprintf(out, "\n%d unused (%s): remove them with: codeflower images prune\n", len(unused), formatSize(imagesSize(unused)))
			}
			return nil
		},
	}
	options.addFlags(command)
	return command
}

func imagesPruneCommand() *cobra.Command {
	var options imagesOptions
	var yes, dryRun bool
	command := &cobra.Command{
		Use:   "prune",
		Short: "Remove the sandbox images no longer needed",
		Long: `Remove the sandbox images no longer needed: the releases and commits' builds
older than the one this app runs, which nothing chooses or uses (see
"codeflower images"). It shows them and asks first, unless --yes is given.`,
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			engine, reviews, err := options.review(command)
			if err != nil {
				return err
			}
			out := command.OutOrStdout()
			unused := slices.DeleteFunc(reviews, func(review sandbox.ImageReview) bool { return !review.Unused })
			if len(unused) == 0 {
				fmt.Fprintf(out, "%s has no unused sandbox images.\n", engine.Label)
				return nil
			}
			fmt.Fprintf(out, "Unused sandbox images (%s):\n", engine.Label)
			if err := writeImages(out, unused, false); err != nil {
				return err
			}
			size := formatSize(imagesSize(unused))
			if dryRun {
				fmt.Fprintf(out, "\nRemoving them would free up to %s: run without --dry-run to remove them.\n", size)
				return nil
			}
			if !yes {
				if !interactive() {
					return errors.New("give --yes to remove them without asking")
				}
				if !confirmPrune(os.Stdin, os.Stderr, len(unused), size) {
					return errors.New("no images were removed")
				}
			}
			ctx, stop := interruptible()
			defer stop()
			failed := 0
			for _, review := range unused {
				if err := sandbox.RemoveReviewed(ctx, engine, review); err != nil {
					if ctx.Err() != nil {
						return errors.New("stopped removing the images")
					}
					fmt.Fprintf(os.Stderr, "Could not remove %s: %v\n", imageLabel(review), err)
					failed++
					continue
				}
				fmt.Fprintf(out, "Removed %s\n", imageLabel(review))
			}
			if failed > 0 {
				return fmt.Errorf("%d of %d images were not removed", failed, len(unused))
			}
			return nil
		},
	}
	options.addFlags(command)
	command.Flags().BoolVarP(&yes, "yes", "y", false, "remove without asking")
	command.Flags().BoolVar(&dryRun, "dry-run", false, "only show what would be removed")
	return command
}

// writeImages writes the reviewed images as a table; withState adds whether
// each one is kept.
func writeImages(out io.Writer, reviews []sandbox.ImageReview, withState bool) error {
	table := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	header := "TAG\tRELEASE\tCREATED\tSIZE\t"
	if withState {
		header += "STATE\t"
	}
	fmt.Fprintln(table, header+"WHY")
	for _, review := range reviews {
		release := review.Version
		if release == "" {
			release = "-"
		}
		if review.Revision != "" {
			release += " (" + review.Revision + ")"
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t", imageLabel(review), release, formatTime(review.Created), formatSize(review.Size))
		if withState {
			state := "kept"
			if review.Unused {
				state = "unused"
			}
			fmt.Fprintf(table, "%s\t", state)
		}
		fmt.Fprintln(table, review.Reason)
	}
	return table.Flush()
}

// imageLabel names a reviewed image for the user.
func imageLabel(review sandbox.ImageReview) string {
	switch {
	case review.Archive:
		return review.Tag + " (saved copy " + review.Ref + ")"
	case review.Untagged:
		id := strings.TrimPrefix(review.Ref, "sha256:")
		return "untagged " + id[:min(12, len(id))]
	case strings.HasPrefix(review.Ref, sandbox.ImageRepository+":"):
		return review.Tag
	}
	return review.Ref // an Apptainer file
}

// imagesSize is the space the images take; two tags of one image count once.
func imagesSize(reviews []sandbox.ImageReview) int64 {
	var total int64
	seen := map[string]bool{}
	for _, review := range reviews {
		key := review.Ref
		if review.ID != "" {
			key = review.ID
		}
		if !seen[key] {
			seen[key] = true
			total += review.Size
		}
	}
	return total
}

// confirmPrune asks before the images are removed.
func confirmPrune(in io.Reader, out io.Writer, count int, size string) bool {
	fmt.Fprintf(out, "Remove these %d images (up to %s)? [y/N] ", count, size)
	line, _ := bufio.NewReader(in).ReadString('\n')
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}
