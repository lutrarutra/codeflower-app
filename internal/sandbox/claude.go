package sandbox

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/docker/docker/client"
)

// Claude Code is not in the sandbox image: its license does not allow passing
// it on. The image names the Claude Agent SDK version its ACP adapter needs in
// a label; the app downloads that SDK and its CLI from npm, once per machine,
// into ClaudeDirectory and binds both into every Claude Code worker.
const (
	ClaudeVersionLabel = "codeflower.claude-agent-sdk"
	// ClaudeTarget holds the CLI, at ClaudeTarget/claude (CLAUDE_CODE_EXECUTABLE
	// in the agent's descriptor).
	ClaudeTarget = "/opt/codeflower/agents/claude"
	// ClaudeSDKTarget is the empty folder in the image's node_modules where the
	// adapter imports the SDK from.
	ClaudeSDKTarget = "/opt/codeflower/runtime/node_modules/@anthropic-ai/claude-agent-sdk"
	claudeAgent     = "claude"
)

// npmRegistry is where Claude Code is downloaded from (tests replace it).
var npmRegistry = "https://registry.npmjs.org"

// AgentsDirectory holds the agents the app downloads: $CODEFLOWER_AGENTS_DIR,
// or ~/codeflower/agents next to the images.
func AgentsDirectory() string {
	if directory := os.Getenv("CODEFLOWER_AGENTS_DIR"); directory != "" {
		return expandHome(directory)
	}
	return filepath.Join(filepath.Dir(ImageDirectory()), "agents")
}

// claudePlatform is the sandbox's npm platform. The engines run images of the
// machine's own architecture, and the image is Debian (glibc).
func claudePlatform() string {
	if runtime.GOARCH == "amd64" {
		return "linux-x64"
	}
	return "linux-" + runtime.GOARCH
}

// ClaudeDirectory is where a version of Claude Code is kept: the SDK in sdk/
// and the CLI as claude.
func ClaudeDirectory(version string) string {
	return filepath.Join(AgentsDirectory(), "claude", version+"-"+claudePlatform())
}

func claudeInstalled(directory string) bool {
	_, sdkErr := os.Stat(filepath.Join(directory, "sdk", "package.json"))
	_, cliErr := os.Stat(filepath.Join(directory, "claude"))
	return sdkErr == nil && cliErr == nil
}

// ImageClaudeVersion is the Claude Agent SDK version a resolved image needs;
// empty for an image without the label, such as older ones with Claude Code
// inside.
func ImageClaudeVersion(ctx context.Context, engine EngineStatus, image string) (string, error) {
	labels, err := imageLabels(ctx, engine, image)
	if err != nil {
		return "", fmt.Errorf("could not read the labels of %s: %w", image, err)
	}
	return labels[ClaudeVersionLabel], nil
}

func imageLabels(ctx context.Context, engine EngineStatus, image string) (map[string]string, error) {
	switch engine.Name {
	case EngineDocker:
		docker, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
		if err != nil {
			return nil, err
		}
		defer docker.Close()
		inspected, err := docker.ImageInspect(ctx, image)
		if err != nil {
			return nil, err
		}
		if inspected.Config == nil {
			return nil, nil
		}
		return inspected.Config.Labels, nil
	case EnginePodman:
		output, err := exec.CommandContext(ctx, engine.Executable, "image", "inspect", "--format", "{{json .Labels}}", image).Output()
		if err != nil {
			return nil, err
		}
		var labels map[string]string
		return labels, json.Unmarshal(output, &labels)
	case EngineApptainer:
		return apptainerLabels(ctx, engine.Executable, image)
	}
	return nil, fmt.Errorf("unknown engine %q", engine.Name)
}

// EnsureClaude downloads a version of Claude Code unless it is there already,
// and returns its directory. Several jobs may start at once on a cluster: they
// wait for one download, and nobody sees a partial one.
func EnsureClaude(ctx context.Context, version string, out io.Writer) (string, error) {
	directory := ClaudeDirectory(version)
	if claudeInstalled(directory) {
		return directory, nil
	}
	if err := os.MkdirAll(filepath.Dir(directory), 0o755); err != nil {
		return "", err
	}
	unlock, err := lockFile(directory + ".lock")
	if err != nil {
		return "", err
	}
	defer unlock()
	if claudeInstalled(directory) {
		return directory, nil
	}
	fmt.Fprintf(out, "Downloading Claude Code (Claude Agent SDK %s, %s) from %s, once…\n", version, claudePlatform(), npmRegistry)
	temporary := directory + "." + strconv.Itoa(os.Getpid()) + ".tmp"
	os.RemoveAll(temporary)
	defer os.RemoveAll(temporary)
	if err := fetchPackage(ctx, "@anthropic-ai/claude-agent-sdk", version, filepath.Join(temporary, "sdk"), out); err != nil {
		return "", err
	}
	cli := filepath.Join(temporary, "cli")
	if err := fetchPackage(ctx, "@anthropic-ai/claude-agent-sdk-"+claudePlatform(), version, cli, out); err != nil {
		return "", err
	}
	if err := os.Rename(filepath.Join(cli, "claude"), filepath.Join(temporary, "claude")); err != nil {
		return "", fmt.Errorf("the Claude Code package has no CLI: %w", err)
	}
	if err := os.Chmod(filepath.Join(temporary, "claude"), 0o755); err != nil {
		return "", err
	}
	if err := os.RemoveAll(cli); err != nil {
		return "", err
	}
	os.RemoveAll(directory)
	if err := os.Rename(temporary, directory); err != nil {
		return "", err
	}
	fmt.Fprintf(out, "  Saved %s\n", directory)
	return directory, nil
}

// ClaudeBinds mount a downloaded Claude Code into a worker, read-only.
func ClaudeBinds(directory string) []Bind {
	return []Bind{
		{Source: directory, Target: ClaudeTarget, ReadOnly: true},
		{Source: filepath.Join(directory, "sdk"), Target: ClaudeSDKTarget, ReadOnly: true},
	}
}

// claudeBinds installs the Claude Code the worker's image needs and mounts
// it; nothing for other agents and for images with Claude Code inside.
func (w Worker) claudeBinds(ctx context.Context) ([]Bind, error) {
	if w.Agent != claudeAgent {
		return nil, nil
	}
	version, err := ImageClaudeVersion(ctx, w.Engine, w.Image)
	if err != nil || version == "" {
		return nil, err
	}
	directory, err := EnsureClaude(ctx, version, w.Stderr)
	if err != nil {
		return nil, fmt.Errorf("Claude Code %s is not in %s and could not be downloaded: %w\n"+
			"Without internet access here (as on many compute nodes), run `codeflower pull` once where there is",
			version, AgentsDirectory(), err)
	}
	return ClaudeBinds(directory), nil
}

// fetchPackage downloads one version of an npm package, checks it against the
// registry's integrity hash, and unpacks it into directory.
func fetchPackage(ctx context.Context, name, version, directory string, out io.Writer) error {
	var metadata struct {
		Dist struct {
			Tarball   string `json:"tarball"`
			Integrity string `json:"integrity"`
		} `json:"dist"`
	}
	if err := getJSON(ctx, npmRegistry+"/"+name+"/"+version, &metadata); err != nil {
		return fmt.Errorf("%s %s: %w", name, version, err)
	}
	expected, ok := strings.CutPrefix(metadata.Dist.Integrity, "sha512-")
	if metadata.Dist.Tarball == "" || !ok {
		return fmt.Errorf("%s %s: the registry gave no tarball with a sha512 integrity", name, version)
	}
	file, err := os.CreateTemp("", "codeflower-npm-*.tgz")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	digest := sha512.New()
	size, err := download(ctx, metadata.Dist.Tarball, io.MultiWriter(file, digest))
	if err != nil {
		return fmt.Errorf("%s %s: %w", name, version, err)
	}
	if got := base64.StdEncoding.EncodeToString(digest.Sum(nil)); got != expected {
		return fmt.Errorf("%s %s: the download does not match the registry's integrity hash", name, version)
	}
	fmt.Fprintf(out, "  %s %s (%d MB)\n", name, version, size>>20)
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return unpackPackage(file, directory)
}

func getJSON(ctx context.Context, url string, value any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", url, response.Status)
	}
	return json.NewDecoder(response.Body).Decode(value)
}

func download(ctx context.Context, url string, to io.Writer) (int64, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("%s answered %s", url, response.Status)
	}
	return io.Copy(to, response.Body)
}

// unpackPackage unpacks an npm tarball's package/ folder into directory:
// regular files and folders only, none outside it.
func unpackPackage(tarball io.Reader, directory string) error {
	compressed, err := gzip.NewReader(tarball)
	if err != nil {
		return err
	}
	archive := tar.NewReader(compressed)
	for {
		header, err := archive.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := path.Clean(header.Name)
		_, inside, found := strings.Cut(name, "/")
		if !found || inside == "" || !filepath.IsLocal(inside) {
			continue
		}
		target := filepath.Join(directory, filepath.FromSlash(inside))
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if header.Mode&0o111 != 0 {
				mode = 0o755
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return err
			}
			_, err = io.Copy(file, archive)
			if closeErr := file.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				return err
			}
		}
	}
}
