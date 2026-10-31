package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func releaseArchive(t *testing.T, binary string) []byte {
	var archive bytes.Buffer
	zipped := gzip.NewWriter(&archive)
	writer := tar.NewWriter(zipped)
	for name, content := range map[string]string{"README.md": "readme", "codeflower": binary} {
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		writer.Write([]byte(content))
	}
	writer.Close()
	zipped.Close()
	return archive.Bytes()
}

func testUpdater(t *testing.T, current string, sums func(archive []byte) string) (*updater, string, *strings.Builder) {
	archive := releaseArchive(t, "new binary")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest":
			fmt.Fprint(w, `{"tag_name": "v0.11.0"}`)
		case "/download/v0.11.0/codeflower-v0.11.0-linux-amd64.tar.gz":
			w.Write(archive)
		case "/download/v0.11.0/SHA256SUMS":
			fmt.Fprint(w, sums(archive))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	executable := filepath.Join(t.TempDir(), "codeflower")
	if err := os.WriteFile(executable, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := new(strings.Builder)
	return &updater{
		client:      server.Client(),
		latestURL:   server.URL + "/latest",
		downloadURL: server.URL + "/download/%s",
		goos:        "linux",
		goarch:      "amd64",
		current:     current,
		executable:  executable,
		out:         out,
	}, executable, out
}

func validSums(archive []byte) string {
	sum := sha256.Sum256(archive)
	return hex.EncodeToString(sum[:]) + "  codeflower-v0.11.0-darwin-arm64.tar.gz\n" +
		hex.EncodeToString(sum[:]) + "  codeflower-v0.11.0-linux-amd64.tar.gz\n"
}

func TestUpdateReplacesTheBinaryWithTheLatestRelease(t *testing.T) {
	u, executable, out := testUpdater(t, "v0.10.0", validSums)
	if err := u.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	content, _ := os.ReadFile(executable)
	info, _ := os.Stat(executable)
	if string(content) != "new binary" || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o755) {
		t.Fatalf("got %q with mode %v", content, info.Mode())
	}
	if !strings.Contains(out.String(), "Updated codeflower v0.10.0 to v0.11.0") {
		t.Errorf("output %q", out)
	}
	if entries, _ := os.ReadDir(filepath.Dir(executable)); len(entries) != 1 {
		t.Errorf("left files behind: %v", entries)
	}
}

func TestUpdateKeepsTheLatestRelease(t *testing.T) {
	u, executable, out := testUpdater(t, "v0.11.0", validSums)
	if err := u.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if content, _ := os.ReadFile(executable); string(content) != "old binary" || !strings.Contains(out.String(), "is the latest release") {
		t.Fatalf("got %q, output %q", content, out)
	}
}

func TestUpdateRefusesAnArchiveWithTheWrongChecksum(t *testing.T) {
	u, executable, _ := testUpdater(t, "dev", func([]byte) string {
		return strings.Repeat("0", 64) + "  codeflower-v0.11.0-linux-amd64.tar.gz\n"
	})
	if err := u.run(context.Background()); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("got %v", err)
	}
	if content, _ := os.ReadFile(executable); string(content) != "old binary" {
		t.Fatalf("replaced the binary with %q", content)
	}
}

func TestNewerComparesReleases(t *testing.T) {
	cases := []struct {
		latest, current string
		want            bool
	}{
		{"v0.11.0", "v0.10.0", true},
		{"v0.10.0", "v0.10.0", false},
		{"v0.9.9", "v0.10.0", false},
		{"v1.0.0", "v0.10.3", true},
		{"v0.10.0", "dev", true},
		{"v0.10.0", "v0.10.0-3-gabc1234-dirty", true},
		{"nightly", "v0.10.0", false},
	}
	for _, c := range cases {
		if got := newer(c.latest, c.current); got != c.want {
			t.Errorf("newer(%q, %q) = %v", c.latest, c.current, got)
		}
	}
}
