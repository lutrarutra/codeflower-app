package cli

import (
	"slices"
	"testing"

	"codeflower-app/internal/sandbox"
)

func TestSavedEnvAndEngineArgs(t *testing.T) {
	settings := sandbox.Settings{Env: []string{"HF_HOME=/data/hf", "WANDB_API_KEY"}, EngineArgs: "--gpus all"}

	// --env adds to the saved variables, replacing one of the same name.
	var options sandboxOptions
	options.changed = func(name string) bool { return name == "env" || options.fromSettings[name] }
	options.envValues = []string{"HF_HOME=/scratch/hf", "SEED=1"}
	options.applySettings(settings, &workerFlags{})
	if !slices.Equal(options.envValues, []string{"HF_HOME=/scratch/hf", "WANDB_API_KEY", "SEED=1"}) {
		t.Errorf("env %v", options.envValues)
	}
	if options.engineArgsLine() != "--gpus all" {
		t.Errorf("saved engine args not taken: %q", options.engineArgsLine())
	}

	// --engine-args replaces the saved ones, and an empty one clears them.
	options = sandboxOptions{}
	options.changed = func(name string) bool { return name == "engine-args" || options.fromSettings[name] }
	options.engineArgsValues = []string{""}
	options.applySettings(settings, &workerFlags{})
	if options.engineArgsLine() != "" {
		t.Errorf("--engine-args '' kept %q", options.engineArgsLine())
	}

	options = sandboxOptions{envValues: []string{"A=1"}, engineArgsValues: []string{"--gpus all", "--shm-size 8g"}}
	saved := options.settingsOf(sandbox.Settings{}, "podman", workerFlags{})
	if !slices.Equal(saved.Env, []string{"A=1"}) || saved.EngineArgs != "--gpus all --shm-size 8g" {
		t.Errorf("saved %+v", saved)
	}
}
