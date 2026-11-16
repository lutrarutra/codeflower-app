package reproduce

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeArchive(t *testing.T, entries map[string]string, links map[string]string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "a.tar.gz")
	out, err := os.Create(file)
	if err != nil {
		t.Fatal(err)
	}
	compressed := gzip.NewWriter(out)
	archive := tar.NewWriter(compressed)
	for name, content := range entries {
		if err := archive.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		archive.Write([]byte(content))
	}
	for name, target := range links {
		archive.WriteHeader(&tar.Header{Name: name, Linkname: target, Typeflag: tar.TypeSymlink})
	}
	archive.Close()
	compressed.Close()
	out.Close()
	return file
}

const manifest = "codeflower_repro: 1\ntitle: Demo\nplatform: linux/amd64\nimage:\n  ref: r\n  id: sha256:abc\n"

func TestUnpackReadsAPipeline(t *testing.T) {
	archive := writeArchive(t, map[string]string{
		"demo/" + ManifestFile:    manifest,
		"demo/data/manifest.tsv":  "path\turl\tsha256\tsize\ndata/raw/x.csv\thttps://example.org/x.csv\t" + strings.Repeat("a", 64) + "\t3\n",
		"demo/workflow/Snakefile": "rule all:\n",
	}, nil)
	pipeline, err := Unpack(archive, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(pipeline) != "demo" {
		t.Fatalf("pipeline %s", pipeline)
	}
	read, err := ReadManifest(pipeline)
	if err != nil {
		t.Fatal(err)
	}
	if read.Title != "Demo" || read.Image.ID != "sha256:abc" || read.Platform != "linux/amd64" {
		t.Fatalf("manifest %+v", read)
	}
	data, err := ReadData(pipeline)
	if err != nil || len(data) != 1 || data[0].Path != "data/raw/x.csv" || data[0].Size != 3 {
		t.Fatalf("data %+v, %v", data, err)
	}
}

func TestUnpackRefusesEscapesLinksAndSeveralFolders(t *testing.T) {
	for name, archive := range map[string]string{
		"escape":   writeArchive(t, map[string]string{"demo/../../evil": "x"}, nil),
		"absolute": writeArchive(t, map[string]string{"/etc/evil": "x"}, nil),
		"link":     writeArchive(t, map[string]string{"demo/" + ManifestFile: manifest}, map[string]string{"demo/data/x": "/etc/passwd"}),
		"two":      writeArchive(t, map[string]string{"a/x": "1", "b/y": "2"}, nil),
	} {
		dir := t.TempDir()
		if _, err := Unpack(archive, dir); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "evil")); err == nil {
			t.Errorf("%s: wrote outside the folder", name)
		}
	}
}

func TestReadDataRefusesPathsOutsideData(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "data"), 0o755)
	for _, path := range []string{"../x", "data/../../x", "/etc/x", "results/x"} {
		os.WriteFile(filepath.Join(dir, "data", "manifest.tsv"),
			[]byte("path\turl\tsha256\tsize\n"+path+"\thttps://e.org/x\t"+strings.Repeat("a", 64)+"\t1\n"), 0o644)
		if _, err := ReadData(dir); err == nil {
			t.Errorf("%s accepted", path)
		}
	}
}

func TestFetchDataChecksAndCaches(t *testing.T) {
	body := "cell,type\n"
	digest := sha256.Sum256([]byte(body))
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Write([]byte(body))
	}))
	defer server.Close()
	cache := t.TempDir()
	files := []DataFile{{Path: "data/raw/cells.csv", URL: server.URL + "/cells.csv", SHA256: hex.EncodeToString(digest[:])}}
	for range 2 {
		tree, err := FetchData(context.Background(), files, cache, nil)
		if err != nil {
			t.Fatal(err)
		}
		content, _ := os.ReadFile(filepath.Join(tree, "data", "raw", "cells.csv"))
		if string(content) != body {
			t.Fatalf("content %q", content)
		}
	}
	if requests != 1 {
		t.Fatalf("downloaded %d times, expected once", requests)
	}
	files[0].SHA256 = strings.Repeat("0", 64)
	if _, err := FetchData(context.Background(), files, t.TempDir(), nil); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("a wrong checksum was accepted: %v", err)
	}
}

func TestToDownloadSkipsBundledInputs(t *testing.T) {
	pipeline := t.TempDir()
	os.MkdirAll(filepath.Join(pipeline, "data", "raw"), 0o755)
	os.WriteFile(filepath.Join(pipeline, "data", "raw", "in.csv"), []byte("x"), 0o644)
	sum := strings.Repeat("a", 64)
	files := []DataFile{
		{Path: "data/raw/in.csv", SHA256: sum},
		{Path: "data/raw/packed-anyway.csv", URL: "https://e.org/p", SHA256: sum},
		{Path: "data/raw/in.csv", URL: "https://e.org/in", SHA256: sum},
	}
	needed, err := ToDownload(pipeline, files)
	if err != nil || len(needed) != 1 || needed[0].Path != "data/raw/packed-anyway.csv" {
		t.Fatalf("needed %+v, %v", needed, err)
	}
	if _, err := ToDownload(pipeline, []DataFile{{Path: "data/raw/gone.csv", SHA256: sum}}); err == nil {
		t.Fatal("a missing bundled input was accepted")
	}
}
