// Package config reads the app's persistent settings: config.yaml in the
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

	"codeflower-app/internal/agents"
)

// FileName is the config file in the config directory.
const FileName = "config.yaml"

// SkillsFolder is mounted read-only in every sandbox when it exists in the
// config directory.
const SkillsFolder = "skills"

// IconsFolder is the user's icon gallery in the config directory, mounted
// read-write in every sandbox so all projects share it.
const IconsFolder = "icons"

// ImageProviders are the services `images:` may name, with their API host.
var ImageProviders = map[string]string{
	"openai": "api.openai.com",
	"google": "generativelanguage.googleapis.com",
}

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

// Icons configures the icon gallery.
type Icons struct {
	// Folder replaces <config dir>/icons; "off" mounts no gallery.
	Folder string `yaml:"folder"`
}

// Images configures the image model codeflower-sketch generates icons with.
type Images struct {
	Provider string `yaml:"provider"`
	// Model replaces the provider's default model.
	Model      string `yaml:"model"`
	APIKey     string `yaml:"api_key"`
	APIKeyEnv  string `yaml:"api_key_env"`
	APIKeyFile string `yaml:"api_key_file"`
}

// ImageGeneration is a configured image model with its resolved key.
type ImageGeneration struct {
	Provider string
	Model    string
	Key      string
}

// Client configures codeflower client.
type Client struct {
	// ConnectOnly: never start sandboxes here, e.g. on a login node.
	ConnectOnly bool `yaml:"connect_only"`
	// WrapUpMinutes: how long quitting waits at most for the chats in the
	// client's sandboxes to wrap up (default 10).
	WrapUpMinutes int `yaml:"wrap_up_minutes"`
	// Tray: show an icon in the system tray or menu bar where there is one
	// (default true).
	Tray *bool `yaml:"tray"`
	// Notifications: tell the desktop when a chat waits for an answer or
	// ends its turn (default true).
	Notifications *bool `yaml:"notifications"`
}

// Config is the content of config.yaml; every field is optional.
type Config struct {
	Engine         string           `yaml:"engine"`
	Image          string           `yaml:"image"`
	IsolateNetwork *bool            `yaml:"isolate_network"`
	Skills         []string         `yaml:"skills"`
	Agents         map[string]Agent `yaml:"agents"`
	Icons          Icons            `yaml:"icons"`
	Images         Images           `yaml:"images"`
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
			return nil, fmt.Errorf("%s: unknown agent %q under agents: use %s", config.Path, name, strings.Join(agents.IDs(), ", "))
		}
	}
	if provider := config.Images.Provider; provider != "" {
		if _, ok := ImageProviders[provider]; !ok {
			return nil, fmt.Errorf("%s: unknown provider %q under images: use openai or google", config.Path, provider)
		}
	} else if config.Images != (Images{}) {
		return nil, fmt.Errorf("%s: images: needs a provider (openai or google)", config.Path)
	}
	if config.hasInlineKey() {
		if err := checkPrivate(config.Path); err != nil {
			return nil, err
		}
	}
	return config, nil
}

func knownAgent(name string) bool {
	_, ok := agents.Get(name)
	return ok
}

func (c *Config) hasInlineKey() bool {
	if c.Images.APIKey != "" {
		return true
	}
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
	return c.resolveKey("agents."+agent, entry.APIKey, entry.APIKeyEnv, entry.APIKeyFile)
}

// IconsFolder is the icon gallery's folder on this computer, or "" when the
// config turns the gallery off.
func (c *Config) IconsFolder() string {
	if c == nil || c.Directory == "" || c.Icons.Folder == "off" {
		return ""
	}
	if c.Icons.Folder == "" {
		return filepath.Join(c.Directory, IconsFolder)
	}
	path := expandHome(c.Icons.Folder)
	if !filepath.IsAbs(path) {
		path = filepath.Join(c.Directory, path)
	}
	return path
}

// ImageGeneration is the configured image model, or nil when there is none.
func (c *Config) ImageGeneration() (*ImageGeneration, error) {
	if c == nil || c.Images.Provider == "" {
		return nil, nil
	}
	key, err := c.resolveKey("images", c.Images.APIKey, c.Images.APIKeyEnv, c.Images.APIKeyFile)
	if err != nil {
		return nil, err
	}
	if key == "" {
		return nil, fmt.Errorf("%s: images: needs one of api_key, api_key_env and api_key_file", c.Path)
	}
	return &ImageGeneration{Provider: c.Images.Provider, Model: c.Images.Model, Key: key}, nil
}

// resolveKey reads a key given inline, in a host environment variable, or in
// a private file; section names the config entry in errors.
func (c *Config) resolveKey(section, inline, variable, file string) (string, error) {
	given := 0
	for _, set := range []bool{inline != "", variable != "", file != ""} {
		if set {
			given++
		}
	}
	switch {
	case given > 1:
		return "", fmt.Errorf("%s: %s has more than one of api_key, api_key_env and api_key_file", c.Path, section)
	case inline != "":
		return inline, nil
	case variable != "":
		key := strings.TrimSpace(os.Getenv(variable))
		if key == "" {
			return "", fmt.Errorf("%s: %s.api_key_env names %s, which is not set", c.Path, section, variable)
		}
		return key, nil
	case file != "":
		path := expandHome(file)
		if !filepath.IsAbs(path) {
			path = filepath.Join(c.Directory, path)
		}
		if err := checkPrivate(path); err != nil {
			return "", err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("%s: %s.api_key_file: %w", c.Path, section, err)
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
