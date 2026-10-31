package cli

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// releaseRepository publishes the app's builds, as install.sh downloads them.
const releaseRepository = "lutrarutra/codeflower-app"

// updater replaces this binary with the latest release; the URLs are fields
// so that tests can serve the release themselves.
type updater struct {
	client *http.Client
	// latestURL answers with the latest release as JSON ({"tag_name": "v1.2.3"}).
	latestURL string
	// downloadURL is the folder of a release's files, with %s for its tag.
	downloadURL string
	goos, goarch string
	current      string
	executable   string
	out          io.Writer
}

func newUpdater(out io.Writer) (*updater, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("could not find this program's file: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}
	return &updater{
		client:      &http.Client{Timeout: 5 * time.Minute},
		latestURL:   "https://api.github.com/repos/" + releaseRepository + "/releases/latest",
		downloadURL: "https://github.com/" + releaseRepository + "/releases/download/%s",
		goos:        runtime.GOOS,
		goarch:      runtime.GOARCH,
		current:     Version,
		executable:  executable,
		out:         out,
	}, nil
}

// archiveName is the file of release tag with the binary for this system.
func (u *updater) archiveName(tag string) string {
	if u.goos == "windows" {
		return "codeflower-" + tag + "-" + u.goos + "-" + u.goarch + ".zip"
	}
	return "codeflower-" + tag + "-" + u.goos + "-" + u.goarch + ".tar.gz"
}

func (u *updater) run(ctx context.Context) error {
	if strings.Contains(filepath.ToSlash(u.executable), ".app/Contents/MacOS/") {
		return fmt.Errorf("this is the macOS app: download codeflower-VERSION-macos.dmg from https://github.com/%s/releases/latest and drag code flower to Applications again", releaseRepository)
	}
	// Left by the previous update on Windows, where the running file can only be moved aside.
	_ = os.Remove(u.executable + ".old")

	latest, err := u.latestTag(ctx)
	if err != nil {
		return err
	}
	if !newer(latest, u.current) {
		fmt.Fprintf(u.out, "codeflower %s is the latest release.\n", u.current)
		return nil
	}

	base := fmt.Sprintf(u.downloadURL, latest)
	name := u.archiveName(latest)
	fmt.Fprintf(u.out, "Downloading %s…\n", name)
	archive, err := u.get(ctx, base+"/"+name)
	if err != nil {
		return err
	}
	sums, err := u.get(ctx, base+"/SHA256SUMS")
	if err != nil {
		return err
	}
	if err := checkSum(archive, sums, name); err != nil {
		return err
	}
	binary, err := extractBinary(archive, u.goos == "windows")
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if err := replaceExecutable(u.executable, binary, u.goos == "windows"); err != nil {
		return err
	}
	fmt.Fprintf(u.out, "Updated codeflower %s to %s in %s.\n", u.current, latest, u.executable)
	fmt.Fprintln(u.out, `A "codeflower client" window or sandbox that is already running keeps the old version until you start it again.`)
	return nil
}

func (u *updater) latestTag(ctx context.Context) (string, error) {
	body, err := u.get(ctx, u.latestURL)
	if err != nil {
		return "", err
	}
	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.Unmarshal(body, &release); err != nil || release.TagName == "" {
		return "", fmt.Errorf("could not read the latest release from %s", u.latestURL)
	}
	return release.TagName, nil
}

func (u *updater) get(ctx context.Context, url string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	response, err := u.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("could not download %s: %w", url, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("could not download %s: %s", url, response.Status)
	}
	return io.ReadAll(response.Body)
}

// newer says whether the release latest should replace current. A build that
// is not a release (dev, or git describe's v1.2.3-4-gabcdef) is always replaced.
func newer(latest, current string) bool {
	l, ok := parseRelease(latest)
	if !ok {
		return false
	}
	c, ok := parseRelease(current)
	if !ok {
		return true
	}
	for i := range l {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

func parseRelease(tag string) ([3]int, bool) {
	var version [3]int
	parts := strings.Split(strings.TrimPrefix(tag, "v"), ".")
	if len(parts) != 3 {
		return version, false
	}
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return version, false
		}
		version[i] = n
	}
	return version, true
}

// checkSum compares the archive with its line in SHA256SUMS ("HASH  NAME").
func checkSum(archive, sums []byte, name string) error {
	scanner := bufio.NewScanner(bytes.NewReader(sums))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			sum := sha256.Sum256(archive)
			if !strings.EqualFold(hex.EncodeToString(sum[:]), fields[0]) {
				return fmt.Errorf("checksum mismatch for %s", name)
			}
			return nil
		}
	}
	return fmt.Errorf("%s is not in SHA256SUMS", name)
}

// extractBinary reads codeflower (codeflower.exe on Windows) out of a release archive.
func extractBinary(archive []byte, windows bool) ([]byte, error) {
	if windows {
		reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			return nil, err
		}
		for _, file := range reader.File {
			if file.Name == "codeflower.exe" {
				opened, err := file.Open()
				if err != nil {
					return nil, err
				}
				defer opened.Close()
				return io.ReadAll(opened)
			}
		}
		return nil, fmt.Errorf("no codeflower.exe in the archive")
	}
	unzipped, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	reader := tar.NewReader(unzipped)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("no codeflower in the archive")
		}
		if err != nil {
			return nil, err
		}
		if header.Name == "codeflower" && header.Typeflag == tar.TypeReg {
			return io.ReadAll(reader)
		}
	}
}

// replaceExecutable puts binary in place of the file at path in one rename, so
// a running codeflower keeps its file. Windows cannot replace a running
// program, only move it aside, so it becomes path.old until the next update.
func replaceExecutable(path string, binary []byte, windows bool) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".codeflower-update-*")
	if err != nil {
		return fmt.Errorf("cannot write to %s (%w); update with the permissions you installed it with, or install it again", directory, err)
	}
	defer os.Remove(temporary.Name())
	if _, err := temporary.Write(binary); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Chmod(temporary.Name(), 0o755); err != nil {
		return err
	}
	if windows {
		if err := os.Rename(path, path+".old"); err != nil {
			return err
		}
		if err := os.Rename(temporary.Name(), path); err != nil {
			_ = os.Rename(path+".old", path)
			return err
		}
		return nil
	}
	return os.Rename(temporary.Name(), path)
}
