package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func write(t *testing.T, directory, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, FileName), []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func TestMissingConfigGivesDefaults(t *testing.T) {
	loaded, err := Load(filepath.Join(t.TempDir(), "none"))
	if err != nil || loaded.Engine != "" || loaded.Agent("claude").Model != "" {
		t.Fatalf("got %+v, %v", loaded, err)
	}
	folders, missing := loaded.SkillFolders()
	if len(folders) != 0 || len(missing) != 0 {
		t.Fatalf("got %v %v", folders, missing)
	}
}

func TestLoadReadsAgents(t *testing.T) {
	directory := t.TempDir()
	write(t, directory, `
engine: podman
isolate_network: true
agents:
  claude: {model: opus, effort: high, options: {fast: "on"}}
`, 0o644)
	loaded, err := Load(directory)
	if err != nil {
		t.Fatal(err)
	}
	claude := loaded.Agent("claude")
	if loaded.Engine != "podman" || loaded.IsolateNetwork == nil || !*loaded.IsolateNetwork ||
		claude.Model != "opus" || claude.Effort != "high" || claude.Options["fast"] != "on" {
		t.Fatalf("got %+v", loaded)
	}
}

func TestLoadRejectsTyposAndUnknownAgents(t *testing.T) {
	for _, content := range []string{"engin: docker\n", "agents:\n  gemini: {model: x}\n"} {
		directory := t.TempDir()
		write(t, directory, content, 0o600)
		if _, err := Load(directory); err == nil {
			t.Fatalf("accepted %q", content)
		}
	}
}

func TestInlineKeyNeedsPrivateFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no file modes")
	}
	directory := t.TempDir()
	write(t, directory, "agents:\n  dsh: {api_key: secret}\n", 0o644)
	if err := os.Chmod(filepath.Join(directory, FileName), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(directory); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("got %v", err)
	}
	if err := os.Chmod(filepath.Join(directory, FileName), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(directory)
	if err != nil {
		t.Fatal(err)
	}
	if key, err := loaded.APIKey("dsh"); err != nil || key != "secret" {
		t.Fatalf("got %q, %v", key, err)
	}
}

func TestAPIKeySources(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("CF_TEST_KEY", " from-env \n")
	keyFile := filepath.Join(directory, "key")
	if err := os.WriteFile(keyFile, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	write(t, directory, "agents:\n  claude: {api_key_env: CF_TEST_KEY}\n  codex: {api_key_file: key}\n  dsh: {api_key_env: CF_TEST_KEY, api_key_file: key}\n", 0o644)
	loaded, err := Load(directory)
	if err != nil {
		t.Fatal(err)
	}
	if key, _ := loaded.APIKey("claude"); key != "from-env" {
		t.Fatalf("env: %q", key)
	}
	if key, _ := loaded.APIKey("codex"); key != "from-file" {
		t.Fatalf("file: %q", key)
	}
	if _, err := loaded.APIKey("dsh"); err == nil {
		t.Fatal("accepted two key sources")
	}
}

func TestSkillFolders(t *testing.T) {
	directory := t.TempDir()
	extra := filepath.Join(t.TempDir(), "extra")
	absent := filepath.Join(t.TempDir(), "absent")
	os.Mkdir(extra, 0o755)
	os.Mkdir(filepath.Join(directory, SkillsFolder), 0o755)
	write(t, directory, "skills: ["+extra+", "+absent+"]\n", 0o644)
	loaded, err := Load(directory)
	if err != nil {
		t.Fatal(err)
	}
	folders, missing := loaded.SkillFolders()
	if len(folders) != 2 || folders[0] != filepath.Join(directory, SkillsFolder) || folders[1] != extra ||
		len(missing) != 1 || missing[0] != absent {
		t.Fatalf("got %v, missing %v", folders, missing)
	}
}

func TestIconsFolder(t *testing.T) {
	directory := t.TempDir()
	loaded, err := Load(directory)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.IconsFolder(); got != filepath.Join(directory, IconsFolder) {
		t.Fatalf("default: %q", got)
	}
	write(t, directory, "icons: {folder: gallery}\n", 0o644)
	if loaded, _ = Load(directory); loaded.IconsFolder() != filepath.Join(directory, "gallery") {
		t.Fatalf("relative: %q", loaded.IconsFolder())
	}
	write(t, directory, "icons: {folder: \"off\"}\n", 0o644)
	if loaded, _ = Load(directory); loaded.IconsFolder() != "" {
		t.Fatalf("off: %q", loaded.IconsFolder())
	}
	if (&Config{}).IconsFolder() != "" {
		t.Fatal("a config without a directory has no gallery")
	}
}

func TestImageGeneration(t *testing.T) {
	directory := t.TempDir()
	loaded, err := Load(directory)
	if err != nil {
		t.Fatal(err)
	}
	if images, err := loaded.ImageGeneration(); images != nil || err != nil {
		t.Fatalf("none configured: %+v, %v", images, err)
	}
	t.Setenv("CF_TEST_IMAGE_KEY", "sk-image")
	write(t, directory, "images: {provider: openai, model: gpt-image-2, api_key_env: CF_TEST_IMAGE_KEY}\n", 0o644)
	if loaded, err = Load(directory); err != nil {
		t.Fatal(err)
	}
	images, err := loaded.ImageGeneration()
	if err != nil || *images != (ImageGeneration{Provider: "openai", Model: "gpt-image-2", Key: "sk-image"}) {
		t.Fatalf("got %+v, %v", images, err)
	}
	write(t, directory, "images: {provider: google}\n", 0o644)
	if loaded, err = Load(directory); err != nil {
		t.Fatal(err)
	}
	if _, err := loaded.ImageGeneration(); err == nil || !strings.Contains(err.Error(), "needs one of api_key") {
		t.Fatalf("no key: %v", err)
	}
	for _, content := range []string{"images: {provider: midjourney}\n", "images: {model: x}\n"} {
		write(t, directory, content, 0o644)
		if _, err := Load(directory); err == nil {
			t.Fatalf("accepted %q", content)
		}
	}
	if runtime.GOOS != "windows" {
		write(t, directory, "images: {provider: openai, api_key: secret}\n", 0o644)
		if err := os.Chmod(filepath.Join(directory, FileName), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(directory); err == nil || !strings.Contains(err.Error(), "chmod 600") {
			t.Fatalf("an inline image key in a readable file: %v", err)
		}
	}
}
