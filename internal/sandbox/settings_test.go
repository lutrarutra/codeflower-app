package sandbox

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSettingsAreSavedPrivatelyAndReadBack(t *testing.T) {
	state := filepath.Join(t.TempDir(), "study42")
	path := SettingsFile(state)
	if path != state+".sandbox.yaml" {
		t.Fatalf("settings file %s", path)
	}
	if _, err := ReadSettings(path); !errors.Is(err, ErrNoSettings) {
		t.Fatalf("missing file: %v", err)
	}
	settings := Settings{Engine: "docker", Inputs: []string{"/data/a", "/data/b"}, Binds: []string{"/ref:/reference:ro"},
		IsolateNetwork: true, Port: 41234, Server: true}
	if err := settings.Renew(); err != nil {
		t.Fatal(err)
	}
	if err := WriteSettings(path, "study42", settings); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v %v", info, err)
		}
	}
	data, _ := os.ReadFile(path)
	text := string(data)
	for _, want := range []string{"--continue study42", "# Folders mounted read-only at /data/input/NAME.",
		"input:\n  - /data/a\n  - /data/b", "port: 41234", "certificate: |"} {
		if !strings.Contains(text, want) {
			t.Errorf("file lacks %q:\n%s", want, text)
		}
	}
	read, err := ReadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if read.Key != settings.Key || read.Port != 41234 || len(read.Inputs) != 2 || !read.IsolateNetwork || !read.Server {
		t.Fatalf("read %+v", read)
	}
	certificate, err := read.TLS()
	if err != nil || !ValidFingerprint(certificate.Fingerprint) {
		t.Fatalf("certificate %v %v", certificate.Fingerprint, err)
	}
}

func TestEditedSettingsAreRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.sandbox.yaml")
	edited := "# mine\nengine: apptainer\ninput:\n  - ~/data\noutput: []\nport: 0\n"
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	settings, err := ReadSettings(path)
	if err != nil || settings.Engine != "apptainer" || settings.Inputs[0] != "~/data" {
		t.Fatalf("%+v %v", settings, err)
	}
	if err := os.WriteFile(path, []byte("port: 70000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSettings(path); err == nil || !strings.Contains(err.Error(), "port") {
		t.Fatalf("bad port: %v", err)
	}
	if err := os.WriteFile(path, []byte("input: [unclosed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSettings(path); err == nil || errors.Is(err, ErrNoSettings) {
		t.Fatalf("broken YAML: %v", err)
	}
}

func TestACertificateMustMatchItsKey(t *testing.T) {
	one, _ := NewCertificate()
	other, _ := NewCertificate()
	loaded, err := LoadCertificate(one.CertPEM, one.KeyPEM)
	if err != nil || loaded.Fingerprint != one.Fingerprint {
		t.Fatalf("%v %v", loaded.Fingerprint, err)
	}
	if _, err := LoadCertificate(one.CertPEM, other.KeyPEM); err == nil {
		t.Fatal("a certificate with another's key loaded")
	}
}

func TestATakenPortIsReported(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	if err := CheckPort("127.0.0.1", port); err == nil || !strings.Contains(err.Error(), "taken") {
		t.Fatalf("taken port: %v", err)
	}
	free, _ := FreePort()
	if err := CheckPort("127.0.0.1", free); err != nil {
		t.Fatalf("free port: %v", err)
	}
}
