// Package reproduce opens pipeline archives that codeflower-repro (in the
// sandbox image) packs: it unpacks them safely, reads their manifest and
// downloads their data, checked against its checksums, for `codeflower
// reproduce`. Running the pipeline is the sandbox package's Reproduction.
package reproduce

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// ManifestFile is the manifest at the top of a pipeline.
const ManifestFile = "codeflower-repro.yaml"

// Schema is the manifest version this app reads.
const Schema = 1

// Manifest is what the app needs from codeflower-repro.yaml; the sandbox
// reads the rest.
type Manifest struct {
	Version     int    `yaml:"codeflower_repro"`
	Title       string `yaml:"title"`
	Platform    string `yaml:"platform"`
	Image       Image  `yaml:"image"`
	Environment struct {
		Snapshot *Snapshot `yaml:"snapshot"`
	} `yaml:"environment"`
	Resources struct {
		WallSeconds float64 `yaml:"wall_seconds"`
		PeakRSSMB   float64 `yaml:"peak_rss_mb"`
		Cores       int     `yaml:"cores"`
	} `yaml:"resources"`
}

// Image is the image the archive was made with.
type Image struct {
	Ref    string `yaml:"ref"`
	Digest string `yaml:"digest"`
	ID     string `yaml:"id"`
}

// Snapshot is an environment packed beside the archive (pack --with-env).
type Snapshot struct {
	File     string `yaml:"file"`
	SHA256   string `yaml:"sha256"`
	Platform string `yaml:"platform"`
}

// DataFile is an input listed in data/manifest.tsv: downloaded from URL, or
// bundled in the archive at Path when URL is empty (or packed anyway with
// pack --bundle-data).
type DataFile struct {
	Path   string
	URL    string
	SHA256 string
	Size   int64
}

// ReadManifest reads a pipeline folder's manifest.
func ReadManifest(pipeline string) (Manifest, error) {
	var manifest Manifest
	content, err := os.ReadFile(filepath.Join(pipeline, ManifestFile))
	if err != nil {
		return manifest, fmt.Errorf("not a pipeline archive (%s is missing): %w", ManifestFile, err)
	}
	if err := yaml.Unmarshal(content, &manifest); err != nil {
		return manifest, fmt.Errorf("%s: %w", ManifestFile, err)
	}
	if manifest.Version != Schema {
		return manifest, fmt.Errorf("%s: codeflower_repro is %d; this app reads version %d", ManifestFile, manifest.Version, Schema)
	}
	return manifest, nil
}

// ReadData reads data/manifest.tsv; a pipeline without it downloads nothing.
func ReadData(pipeline string) ([]DataFile, error) {
	file, err := os.Open(filepath.Join(pipeline, "data", "manifest.tsv"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var files []DataFile
	scanner := bufio.NewScanner(file)
	var columns map[string]int
	for scanner.Scan() {
		fields := strings.Split(strings.TrimRight(scanner.Text(), "\r"), "\t")
		if columns == nil {
			columns = map[string]int{}
			for i, name := range fields {
				columns[strings.TrimSpace(name)] = i
			}
			continue
		}
		get := func(name string) string {
			if i, ok := columns[name]; ok && i < len(fields) {
				return strings.TrimSpace(fields[i])
			}
			return ""
		}
		entry := DataFile{Path: get("path"), URL: get("url"), SHA256: strings.ToLower(get("sha256"))}
		entry.Size, _ = strconv.ParseInt(get("size"), 10, 64)
		if entry.Path == "" {
			continue
		}
		if err := checkDataPath(entry.Path); err != nil {
			return nil, err
		}
		if len(entry.SHA256) != 64 {
			return nil, fmt.Errorf("data/manifest.tsv: %s has no sha256", entry.Path)
		}
		files = append(files, entry)
	}
	return files, scanner.Err()
}

func checkDataPath(name string) error {
	clean := path.Clean(name)
	if clean != name || !strings.HasPrefix(name, "data/") || strings.Contains(name, "..") || path.IsAbs(name) {
		return fmt.Errorf("data/manifest.tsv: %q is not a path under data/", name)
	}
	return nil
}

// Unpack extracts a .tar.gz archive into dir and returns the pipeline folder
// in it (its one top-level folder). Entries that would land outside dir, links
// and devices are refused.
func Unpack(archive, dir string) (string, error) {
	file, err := os.Open(archive)
	if err != nil {
		return "", err
	}
	defer file.Close()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		return "", fmt.Errorf("%s is not a .tar.gz archive: %w", archive, err)
	}
	defer compressed.Close()
	reader := tar.NewReader(compressed)
	top := ""
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("%s: %w", archive, err)
		}
		name := path.Clean(strings.TrimPrefix(header.Name, "./"))
		if name == "." || path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return "", fmt.Errorf("%s holds %q, outside its folder", archive, header.Name)
		}
		first, _, _ := strings.Cut(name, "/")
		if top == "" {
			top = first
		} else if first != top {
			return "", fmt.Errorf("%s holds more than one folder (%s and %s)", archive, top, first)
		}
		target := filepath.Join(dir, filepath.FromSlash(name))
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return "", err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return "", err
			}
			mode := os.FileMode(0o644)
			if header.Mode&0o111 != 0 {
				mode = 0o755
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return "", err
			}
			_, err = io.Copy(out, reader)
			if closeErr := out.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				return "", err
			}
		default:
			return "", fmt.Errorf("%s holds %q, which is not a file or folder", archive, header.Name)
		}
	}
	if top == "" {
		return "", fmt.Errorf("%s is empty", archive)
	}
	return filepath.Join(dir, top), nil
}

// Download fetches url into the file, through a partial file, and returns its
// sha256.
func Download(ctx context.Context, url, file string, out io.Writer) (string, error) {
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("User-Agent", "codeflower")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", url, response.Status)
	}
	partial := file + ".part"
	stream, err := os.Create(partial)
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	progress := &progressWriter{out: out, total: response.ContentLength, next: time.Now().Add(5 * time.Second)}
	_, err = io.Copy(io.MultiWriter(stream, digest, progress), response.Body)
	if closeErr := stream.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(partial)
		return "", fmt.Errorf("%s: %w", url, err)
	}
	return hex.EncodeToString(digest.Sum(nil)), os.Rename(partial, file)
}

type progressWriter struct {
	out   io.Writer
	total int64
	done  int64
	next  time.Time
}

func (p *progressWriter) Write(data []byte) (int, error) {
	p.done += int64(len(data))
	if p.out != nil && time.Now().After(p.next) {
		p.next = time.Now().Add(5 * time.Second)
		if p.total > 0 {
			fmt.Fprintf(p.out, "  %d of %d MB\n", p.done>>20, p.total>>20)
		} else {
			fmt.Fprintf(p.out, "  %d MB\n", p.done>>20)
		}
	}
	return len(data), nil
}

// FileSHA256 is a file's sha256 in hex.
func FileSHA256(file string) (string, error) {
	stream, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer stream.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, stream); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// ToDownload are the inputs the archive does not hold itself. An input that
// is neither in the archive nor has a URL makes it invalid.
func ToDownload(pipeline string, files []DataFile) ([]DataFile, error) {
	var needed []DataFile
	for _, entry := range files {
		if info, err := os.Stat(filepath.Join(pipeline, filepath.FromSlash(entry.Path))); err == nil && info.Mode().IsRegular() {
			continue
		}
		if entry.URL == "" {
			return nil, fmt.Errorf("%s is listed as bundled, but the archive does not hold it", entry.Path)
		}
		needed = append(needed, entry)
	}
	return needed, nil
}

// FetchData makes sure each input is in the cache (cache/files/<sha256>),
// downloading and checking what is missing, and lays them out at their paths
// in a new folder under cache/trees, which it returns: hard links, or copies
// where links do not work. Remove the folder when done.
func FetchData(ctx context.Context, files []DataFile, cache string, out io.Writer) (string, error) {
	if out == nil {
		out = io.Discard
	}
	store := filepath.Join(cache, "files")
	if err := os.MkdirAll(store, 0o755); err != nil {
		return "", err
	}
	for _, entry := range files {
		cached := filepath.Join(store, entry.SHA256)
		if sum, err := FileSHA256(cached); err == nil && sum == entry.SHA256 {
			continue
		}
		if entry.URL == "" {
			return "", fmt.Errorf("%s has no URL to download it from", entry.Path)
		}
		fmt.Fprintf(out, "Downloading %s from %s…\n", entry.Path, entry.URL)
		sum, err := Download(ctx, entry.URL, cached, out)
		if err != nil {
			return "", fmt.Errorf("could not download %s: %w", entry.Path, err)
		}
		if sum != entry.SHA256 {
			os.Remove(cached)
			return "", fmt.Errorf("%s from %s has sha256 %s, but the archive expects %s: the file there changed",
				entry.Path, entry.URL, sum, entry.SHA256)
		}
	}
	trees := filepath.Join(cache, "trees")
	if err := os.MkdirAll(trees, 0o755); err != nil {
		return "", err
	}
	tree, err := os.MkdirTemp(trees, "data-")
	if err != nil {
		return "", err
	}
	for _, entry := range files {
		target := filepath.Join(tree, filepath.FromSlash(entry.Path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", err
		}
		source := filepath.Join(store, entry.SHA256)
		if err := os.Link(source, target); err != nil {
			if err := copyFile(source, target); err != nil {
				os.RemoveAll(tree)
				return "", err
			}
		}
	}
	return tree, nil
}

func copyFile(source, target string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(target)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// Report is the result codeflower-repro verify writes (reproduce-report.json).
type Report struct {
	Title   string `json:"title"`
	OK      bool   `json:"ok"`
	Checked int    `json:"checked"`
	Matched int    `json:"matched"`
	Results []struct {
		Path    string `json:"path"`
		Compare string `json:"compare"`
		OK      bool   `json:"ok"`
		Detail  string `json:"detail"`
	} `json:"results"`
	Unexpected []string `json:"unexpected"`
}

// CheckResult is what codeflower-repro check writes (check.json).
type CheckResult struct {
	OK       bool     `json:"ok"`
	Errors   []string `json:"errors"`
	Warnings []string `json:"warnings"`
}
