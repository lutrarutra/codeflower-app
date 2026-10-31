package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetTunnelKeepsTheRestOfTheFileAndItsComments(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, FileName)
	original := "# my settings\nengine: docker # the one I have\nclient:\n  tray: false\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SetTunnel(directory, "https://tunnel.example.org", true); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	text := string(data)
	for _, want := range []string{"# my settings", "engine: docker # the one I have", "tray: false", "url: https://tunnel.example.org", "eu_only: true"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("permissions changed to %v", info.Mode().Perm())
	}
	loaded, err := Load(directory)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Tunnel.EUOnly == nil || !*loaded.Tunnel.EUOnly || loaded.Tunnel.URL != "https://tunnel.example.org" {
		t.Fatalf("loaded %+v", loaded.Tunnel)
	}

	// Answering again changes the answer, and an empty url keeps the url.
	if err := SetTunnel(directory, "", false); err != nil {
		t.Fatal(err)
	}
	loaded, _ = Load(directory)
	if *loaded.Tunnel.EUOnly || loaded.Tunnel.URL != "https://tunnel.example.org" {
		t.Fatalf("loaded %+v", loaded.Tunnel)
	}
}

func TestSetTunnelCreatesTheFile(t *testing.T) {
	directory := t.TempDir()
	if err := SetTunnel(directory, "", false); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(directory)
	if err != nil || loaded.Tunnel.EUOnly == nil || *loaded.Tunnel.EUOnly {
		t.Fatalf("loaded %+v, %v", loaded.Tunnel, err)
	}
}
