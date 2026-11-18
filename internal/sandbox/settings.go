package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Settings are how `codeflower sandbox` last started a project, saved next
// to the project folder (SettingsFile) for `codeflower sandbox --continue`.
// The file is the user's to read and edit; its keys are the flags' names.
//
// It lies next to the project, not in it, because the project folder is
// mounted read-write in the sandbox: an agent that could edit the settings
// could choose what the next sandbox mounts.
type Settings struct {
	Engine         string   `yaml:"engine"`
	Image          string   `yaml:"image"`
	Inputs         []string `yaml:"input"`
	Outputs        []string `yaml:"output"`
	Code           []string `yaml:"code"`
	Skills         []string `yaml:"skills"`
	Binds          []string `yaml:"bind"`
	IsolateNetwork bool     `yaml:"isolate_network"`
	Allowlist      string   `yaml:"allowlist"`
	EnvDir         string   `yaml:"env_dir"`
	CPUs           int      `yaml:"cpus"`
	Memory         string   `yaml:"memory"`
	TimeLimit      string   `yaml:"time_limit"`
	Env            []string `yaml:"env"`
	EngineArgs     string   `yaml:"engine_args"`
	Server         bool     `yaml:"server"`
	Host           string   `yaml:"host"`
	AdvertiseHost  string   `yaml:"advertise_host"`
	Port           int      `yaml:"port"`
	NoTunnel       bool     `yaml:"no_tunnel"`
	ForwardTo      string   `yaml:"forward_to"`
	// Key is the worker's token (NewToken), and Certificate its TLS
	// certificate and key: kept, so the links VS Code and browsers saved
	// still connect after a restart.
	Key         string              `yaml:"key"`
	Certificate SettingsCertificate `yaml:"tls"`
}

// SettingsCertificate is a Certificate as the settings keep it (PEM).
type SettingsCertificate struct {
	Certificate string `yaml:"certificate"`
	Key         string `yaml:"key"`
}

// settingsComments explain each key in the file.
var settingsComments = map[string]string{
	"engine":          "Container engine: docker, podman or apptainer.",
	"image":           "Sandbox image; empty for the published image this app is made for.",
	"input":           "Folders mounted read-only at /data/input/NAME.",
	"output":          "Folders mounted read-write at /data/output/NAME.",
	"code":            "Repositories and packages mounted read-write at /data/code/NAME.",
	"skills":          "Skills folders mounted read-only at /data/skills/NAME (the config's are added too).",
	"bind":            "More mounts: SOURCE[:TARGET[:ro|rw]].",
	"isolate_network": "Cut the sandbox off the network except for its agents' services and the allowlist.",
	"allowlist":       "Hosts file of an isolated sandbox; empty for the one next to the project.",
	"env_dir":         "Folder for the software environments and caches; empty for in the project.",
	"cpus":            "CPUs the agent may use; 0 for the batch job's allocation.",
	"memory":          "Memory the agent may use, e.g. 32G; empty for the batch job's allocation.",
	"time_limit":      "Time the agent has, e.g. 8h; empty for the batch job's time limit.",
	"env":             "Environment variables: NAME=VALUE, or NAME for its value where the sandbox starts (keeps secrets out of this file).",
	"engine_args":     "Container engine flags, e.g. --gpus all --shm-size 8g.",
	"server":          "Also serve the chat page on the worker's port.",
	"host":            "Interface the worker listens on (Apptainer).",
	"advertise_host":  "Address printed in the links; empty for this node's address.",
	"port":            "Port of the worker; 0 for any free port. Starting fails if it is taken.",
	"no_tunnel":       "Keep the sandbox from browsers paired through the tunnel.",
	"forward_to":      "SSH host whose 127.0.0.1:PORT reaches the worker through ssh -R while it runs; empty for none.",
	"key":             "Secret that VS Code and the page connect with. Anyone who has it controls the agent.",
	"tls":             "The worker's TLS certificate; links name its fingerprint.",
}

// SettingsFile is where a project's Settings are: next to its folder, as
// its allowlist is (DefaultAllowlist).
func SettingsFile(state string) string {
	return filepath.Clean(state) + ".sandbox.yaml"
}

// ErrNoSettings means a project has no saved settings yet.
var ErrNoSettings = errors.New("no saved settings")

// ReadSettings reads the settings file; ErrNoSettings if there is none.
func ReadSettings(path string) (Settings, error) {
	var settings Settings
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return settings, fmt.Errorf("%w: %s", ErrNoSettings, path)
	}
	if err != nil {
		return settings, err
	}
	if err := yaml.Unmarshal(data, &settings); err != nil {
		return settings, fmt.Errorf("%s: %w", path, err)
	}
	if settings.Port < 0 || settings.Port > 65535 {
		return settings, fmt.Errorf("%s: port must be between 0 and 65535", path)
	}
	return settings, nil
}

// WriteSettings writes the settings file privately (the key is a secret),
// with a comment on each key.
func WriteSettings(path, project string, settings Settings) error {
	var node yaml.Node
	if err := node.Encode(settings); err != nil {
		return err
	}
	node.HeadComment = fmt.Sprintf("Settings of `codeflower sandbox` for the project %s, saved each time it starts.\n"+
		"`codeflower sandbox --continue %s` starts it with them again: flags given then\n"+
		"replace a setting, or add folders. Edit it while the sandbox is stopped.\n"+
		"Keep it private: it holds the key that connects to the sandbox.", project, project)
	for index := 0; index+1 < len(node.Content); index += 2 {
		key := node.Content[index]
		// A blank line before each setting, for reading.
		key.HeadComment = "\n" + settingsComments[key.Value]
	}
	var text strings.Builder
	encoder := yaml.NewEncoder(&text)
	encoder.SetIndent(2)
	if err := encoder.Encode(&node); err != nil {
		return err
	}
	if err := encoder.Close(); err != nil {
		return err
	}
	return writePrivately(path, []byte(text.String()))
}

// writePrivately replaces a file with owner-only permissions, atomically.
func writePrivately(path string, data []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	_, err = temporary.Write(data)
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(temporary.Name(), path)
}

// Renew gives the settings a new key and TLS certificate.
func (s *Settings) Renew() error {
	certificate, err := NewCertificate()
	if err != nil {
		return err
	}
	s.Key = NewToken()
	s.Certificate = SettingsCertificate{Certificate: string(certificate.CertPEM), Key: string(certificate.KeyPEM)}
	return nil
}

// TLS is the saved certificate, checked.
func (s Settings) TLS() (Certificate, error) {
	return LoadCertificate([]byte(s.Certificate.Certificate), []byte(s.Certificate.Key))
}
