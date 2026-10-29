// Package config reads the launcher's persistent settings: config.yaml in the
// config directory (default ~/.codeflower), and the skills folder next to it.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"go.yaml.in/yaml/v3"
)

// FileName is the config file in the config directory.
const FileName = "config.yaml"

// SkillsFolder is mounted read-only in every sandbox when it exists in the
// config directory.
const SkillsFolder = "skills"

// Agent holds the defaults for one agent.
type Agent struct {
	Model   string            `yaml:"model"`
	Effort  string            `yaml:"effort"`
	Options map[string]string `yaml:"options"`
	// The API key, from one of: the host environment variable named by
	// APIKeyEnv, the file APIKeyFile, or APIKey itself.
	APIKey     string `yaml:"api_key"`
	APIKeyEnv  string `yaml:"api_key_env"`
	APIKeyFile string `yaml:"api_key_file"`
}

// Client configures codeflower client.
type Client struct {
	// ConnectOnly: never start sandboxes here, e.g. on a login node.
	ConnectOnly bool `yaml:"connect_only"`
}

// Config is the content of config.yaml; every field is optional.
type Config struct {
	Engine         string           `yaml:"engine"`
	Image          string           `yaml:"image"`
	IsolateNetwork *bool            `yaml:"isolate_network"`
	Skills         []string         `yaml:"skills"`
	Agents         map[string]Agent `yaml:"agents"`
	Client         Client           `yaml:"client"`

	// Directory is the config directory; Path is the file, whether or not it exists.
	Directory string `yaml:"-"`
	Path      string `yaml:"-"`
}

// Directory returns the config directory: the explicit one, else
// $CODEFLOWER_CONFIG, else ~/.codeflower.
func Directory(explicit string) string {
	if explicit == "" {
		explicit = os.Getenv("CODEFLOWER_CONFIG")
	}
	if explicit != "" {
		return expandHome(explicit)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codeflower")
}

// Load reads the config of a directory. A missing directory or file gives the
// defaults. Unknown keys are errors, so a typo is not silently ignored, and a
// file with an inline API key must not be readable by other users.
func Load(directory string) (*Config, error) {
	config := &Config{Directory: directory}
	if directory == "" {
		return config, nil
	}
	config.Path = filepath.Join(directory, FileName)
	data, err := os.ReadFile(config.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return config, nil
	}
	if err != nil {
		return nil, fmt.Errorf("could not read %s: %w", config.Path, err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(config); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", config.Path, err)
	}
	for name := range config.Agents {
		if !knownAgent(name) {
			return nil, fmt.Errorf("%s: unknown agent %q under agents: use claude, codex or dsh", config.Path, name)
		}
	}
	if config.hasInlineKey() {
		if err := checkPrivate(config.Path); err != nil {
			return nil, err
		}
	}
	return config, nil
}

func knownAgent(name string) bool {
	return name == "claude" || name == "codex" || name == "dsh"
}

func (c *Config) hasInlineKey() bool {
	for _, agent := range c.Agents {
		if agent.APIKey != "" {
			return true
		}
	}
	return false
}

// checkPrivate refuses a file other users can read; Windows has no such mode.
func checkPrivate(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s holds an API key but can be read by other users: run chmod 600 %s", path, path)
	}
	return nil
}

// Agent returns the defaults of an agent, empty when it has none.
func (c *Config) Agent(name string) Agent {
	if c == nil {
		return Agent{}
	}
	return c.Agents[name]
}

// SkillFolders are the folders always mounted as skills: the config
// directory's skills folder when it exists, then the existing folders of the
// skills list. Listed folders that are missing are returned in missing.
func (c *Config) SkillFolders() (folders, missing []string) {
	if c == nil || c.Directory == "" {
		return nil, nil
	}
	own := filepath.Join(c.Directory, SkillsFolder)
	if info, err := os.Stat(own); err == nil && info.IsDir() {
		folders = append(folders, own)
	}
	for _, listed := range c.Skills {
		path := expandHome(listed)
		if !filepath.IsAbs(path) {
			path = filepath.Join(c.Directory, path)
		}
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			missing = append(missing, path)
			continue
		}
		folders = append(folders, path)
	}
	return folders, missing
}

// APIKey resolves the agent's API key; it is empty when none is configured.
func (c *Config) APIKey(agent string) (string, error) {
	entry := c.Agent(agent)
	given := 0
	for _, set := range []bool{entry.APIKey != "", entry.APIKeyEnv != "", entry.APIKeyFile != ""} {
		if set {
			given++
		}
	}
	switch {
	case given > 1:
		return "", fmt.Errorf("%s: agents.%s has more than one of api_key, api_key_env and api_key_file", c.Path, agent)
	case entry.APIKey != "":
		return entry.APIKey, nil
	case entry.APIKeyEnv != "":
		key := strings.TrimSpace(os.Getenv(entry.APIKeyEnv))
		if key == "" {
			return "", fmt.Errorf("%s: agents.%s.api_key_env names %s, which is not set", c.Path, agent, entry.APIKeyEnv)
		}
		return key, nil
	case entry.APIKeyFile != "":
		path := expandHome(entry.APIKeyFile)
		if !filepath.IsAbs(path) {
			path = filepath.Join(c.Directory, path)
		}
		if err := checkPrivate(path); err != nil {
			return "", err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("%s: agents.%s.api_key_file: %w", c.Path, agent, err)
		}
		key := strings.TrimSpace(string(data))
		if key == "" {
			return "", fmt.Errorf("%s is empty", path)
		}
		return key, nil
	}
	return "", nil
}

func expandHome(value string) string {
	if value == "~" || strings.HasPrefix(value, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return home + value[1:]
		}
	}
	return value
}
