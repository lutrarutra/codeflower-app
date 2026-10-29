package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ContainerState is where the project directory appears inside the sandbox.
const ContainerState = "/codeflower-state"

// Entrypoint prepares the project environment and starts the bridge.
const Entrypoint = "/opt/codeflower/entrypoint.sh"

const dshProfile = "codeflower"

var dshBundles = []string{"@deepseek-ai/dsh-base", "@deepseek-ai/dsh-acp-app"}

// StateDirectories are created privately in every project.
var StateDirectories = []string{
	"instances",
	"sessions",
	"run",
	"dsh-home",
	"workspace",
	"env",
	"cache",
	"home",
	"harness",
	"harness/claude",
	"harness/codex",
	"tasks",
}

// Apptainer is the isolation boundary, so Codex's own bubblewrap sandbox
// stays off; approval prompts still reach the chat.
const codexConfig = `# Written once by codeflower; edit freely.
sandbox_mode = "danger-full-access"
approval_policy = "on-request"
`

func privateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("state path must be a real directory: %s", path)
	}
	return os.Chmod(path, 0o700)
}

// writeOnce creates a file with private permissions unless it already exists,
// so the user's later edits are kept.
func writeOnce(path string, content []byte) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, content, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

// PrepareState creates the project layout and its default files, and returns
// the absolute project path.
func PrepareState(dir string) (string, error) {
	state, err := filepath.Abs(expandHome(dir))
	if err != nil {
		return "", err
	}
	// Apptainer binds are SOURCE:TARGET; a Windows drive letter is not part of that.
	if strings.Contains(strings.TrimPrefix(state, filepath.VolumeName(state)), ":") {
		return "", fmt.Errorf("state path must not contain ':'")
	}
	if err := privateDirectory(state); err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(state); err == nil {
		state = resolved
	}
	for _, name := range StateDirectories {
		if err := privateDirectory(filepath.Join(state, name)); err != nil {
			return "", err
		}
	}

	profile := filepath.Join(state, "dsh-home", "profiles", dshProfile)
	if err := privateDirectory(profile); err != nil {
		return "", err
	}
	manifest, err := json.MarshalIndent(map[string]any{
		"name":    dshProfile,
		"private": true,
		"dsh":     map[string]any{"profile": map[string]any{"bundles": dshBundles}},
	}, "", "  ")
	if err != nil {
		return "", err
	}
	if err := writeOnce(filepath.Join(profile, "package.json"), append(manifest, '\n')); err != nil {
		return "", err
	}
	if err := writeOnce(filepath.Join(profile, "cordis.patch.yml"), []byte("[]\n")); err != nil {
		return "", err
	}
	if err := writeOnce(filepath.Join(state, "harness", "codex", "config.toml"), []byte(codexConfig)); err != nil {
		return "", err
	}
	return state, nil
}
