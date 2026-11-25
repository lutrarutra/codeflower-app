package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAddPresetsAppendsOnlyWhatIsMissing(t *testing.T) {
	path, err := PrepareAllowlist(filepath.Join(t.TempDir(), "hosts.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(must(os.ReadFile(path)), []byte("@github\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := AddPresets(path, []string{"literature", "github"}, "for a test"); err != nil {
			t.Fatal(err)
		}
	}
	text := string(must(os.ReadFile(path)))
	if strings.Count(text, "\n@literature\n") != 1 || strings.Count(text, "\n@github\n") != 1 {
		t.Errorf("allowlist:\n%s", text)
	}
	if !strings.Contains(text, "# Added for a test:\n@literature\n") {
		t.Errorf("no reason given:\n%s", text)
	}
	if err := AddPresets(path, []string{"nope"}, "x"); err == nil {
		t.Error("an unknown preset was added")
	}
}

func must[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}
