package sandbox

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

// npmTarball packs files as npm does, under package/.
func npmTarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	compressed := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(compressed)
	for name, content := range files {
		mode := int64(0o644)
		if strings.HasSuffix(name, "claude") {
			mode = 0o755
		}
		if err := archive.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		archive.Write([]byte(content))
	}
	archive.Close()
	compressed.Close()
	return buffer.Bytes()
}

// fakeRegistry serves packages by name at one version; corrupt serves
// tarballs that do not match their integrity hash.
func fakeRegistry(t *testing.T, version string, packages map[string][]byte, corrupt bool) *atomic.Int32 {
	t.Helper()
	var downloads atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for name, tarball := range packages {
			switch r.URL.Path {
			case "/" + name + "/" + version:
				sum := sha512.Sum512(tarball)
				json.NewEncoder(w).Encode(map[string]any{"dist": map[string]string{
					"tarball":   server.URL + "/tarballs/" + name + ".tgz",
					"integrity": "sha512-" + base64.StdEncoding.EncodeToString(sum[:]),
				}})
				return
			case "/tarballs/" + name + ".tgz":
				downloads.Add(1)
				if corrupt {
					w.Write(append([]byte{0}, tarball...))
				} else {
					w.Write(tarball)
				}
				return
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	previous := npmRegistry
	npmRegistry = server.URL
	t.Cleanup(func() { npmRegistry = previous })
	t.Setenv("CODEFLOWER_AGENTS_DIR", t.TempDir())
	return &downloads
}

func claudePackages(t *testing.T) map[string][]byte {
	return map[string][]byte{
		"@anthropic-ai/claude-agent-sdk": npmTarball(t, map[string]string{
			"package/package.json": `{"name":"@anthropic-ai/claude-agent-sdk"}`,
			"package/sdk.mjs":      "export {}",
			"package/../escape":    "outside",
		}),
		"@anthropic-ai/claude-agent-sdk-" + claudePlatform(): npmTarball(t, map[string]string{
			"package/package.json": "{}",
			"package/claude":       "#!/bin/sh\n",
		}),
	}
}

func TestEnsureClaudeDownloadsOnceAndBindsIt(t *testing.T) {
	downloads := fakeRegistry(t, "1.2.3", claudePackages(t), false)
	directory, err := EnsureClaude(context.Background(), "1.2.3", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if directory != filepath.Join(AgentsDirectory(), "claude", "1.2.3-"+claudePlatform()) {
		t.Fatalf("directory = %s", directory)
	}
	if data, err := os.ReadFile(filepath.Join(directory, "sdk", "sdk.mjs")); err != nil || string(data) != "export {}" {
		t.Fatalf("sdk.mjs = %q, %v", data, err)
	}
	// Windows keeps no executable bit; Docker Desktop binds the file executable.
	info, err := os.Stat(filepath.Join(directory, "claude"))
	if err != nil || runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("claude is not an executable: %v %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(directory, "cli")); !os.IsNotExist(err) {
		t.Fatalf("the CLI package's folder is left: %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, "escape")); !os.IsNotExist(err) {
		t.Fatalf("a path outside package/ was unpacked: %v", err)
	}
	if _, err := EnsureClaude(context.Background(), "1.2.3", io.Discard); err != nil || downloads.Load() != 2 {
		t.Fatalf("a second call downloaded again (%d downloads), %v", downloads.Load(), err)
	}
	binds := ClaudeBinds(directory)
	if len(binds) != 2 || binds[0].Target != ClaudeTarget || binds[1].Target != ClaudeSDKTarget ||
		binds[1].Source != filepath.Join(directory, "sdk") || !binds[0].ReadOnly || !binds[1].ReadOnly {
		t.Fatalf("binds = %+v", binds)
	}
}

func TestEnsureClaudeRejectsADownloadThatDoesNotMatchItsHash(t *testing.T) {
	fakeRegistry(t, "1.2.3", claudePackages(t), true)
	_, err := EnsureClaude(context.Background(), "1.2.3", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "integrity") {
		t.Fatalf("err = %v", err)
	}
	if claudeInstalled(ClaudeDirectory("1.2.3")) {
		t.Fatal("a rejected download was installed")
	}
}

func TestTasksOfOtherAgentsNeedNoClaude(t *testing.T) {
	binds, err := Worker{Agent: "codex", Task: "task-1"}.claudeBinds(context.Background())
	if err != nil || binds != nil {
		t.Fatalf("binds = %v, %v", binds, err)
	}
}

func TestASandboxStartsWithoutClaudeWhenItCannotBeDownloaded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a shell script as the engine")
	}
	directory := t.TempDir()
	t.Setenv("CODEFLOWER_AGENTS_DIR", directory)
	previous := npmRegistry
	npmRegistry = "http://127.0.0.1:9"
	t.Cleanup(func() { npmRegistry = previous })
	engine := filepath.Join(directory, "podman")
	// An image that names a Claude Code version, which nothing can download.
	script := "#!/bin/sh\necho '{\"codeflower.claude-agent-sdk\":\"0.0.1\"}'\n"
	if err := os.WriteFile(engine, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var log strings.Builder
	worker := Worker{Engine: EngineStatus{Name: EnginePodman, Executable: engine}, Image: "image", Stderr: &log}
	if binds, err := worker.claudeBinds(context.Background()); err != nil || binds != nil {
		t.Fatalf("a sandbox for windows: binds = %v, %v", binds, err)
	}
	if !strings.Contains(log.String(), "could not be downloaded") {
		t.Fatalf("logged %q", log.String())
	}
	task := worker
	task.Agent, task.Task = "claude", "task-1"
	if _, err := task.claudeBinds(context.Background()); err == nil {
		t.Fatal("a Claude Code task started without Claude Code")
	}
}
