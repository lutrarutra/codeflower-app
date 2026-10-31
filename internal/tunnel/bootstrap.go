package tunnel

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// bootstrapHashes are the SHA-256 hashes of the browser code the tunnel serves
// (ui's tunnel build, `npm run build:tunnel`, at the version this app pins),
// by path. `make -C app tunnel-hashes` updates the file.
//
//go:embed bootstrap-hashes.json
var bootstrapHashes []byte

// VerifyBootstrap downloads the bootstrap files from the tunnel and compares
// them with the expected hashes. It returns one line per file.
func VerifyBootstrap(ctx context.Context, base string) ([]string, error) {
	var expected map[string]string
	if err := json.Unmarshal(bootstrapHashes, &expected); err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(expected))
	for path := range expected {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	client := &http.Client{Timeout: 30 * time.Second}
	var lines []string
	failed := 0
	for _, path := range paths {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+path, nil)
		if err != nil {
			return lines, err
		}
		request.Header.Set("Cache-Control", "no-cache")
		response, err := client.Do(request)
		if err != nil {
			return lines, err
		}
		body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
		response.Body.Close()
		if err != nil {
			return lines, err
		}
		sum := sha256.Sum256(body)
		got := hex.EncodeToString(sum[:])
		if response.StatusCode == http.StatusOK && got == expected[path] {
			lines = append(lines, "ok        "+path)
		} else {
			failed++
			lines = append(lines, fmt.Sprintf("MISMATCH  %s (status %d, sha256 %s, expected %s)", path, response.StatusCode, got, expected[path]))
		}
	}
	if failed > 0 {
		return lines, fmt.Errorf("%d of %d files differ from what this code flower expects: do not pair browsers through this tunnel until you know why (a newer tunnel release, or tampering)", failed, len(paths))
	}
	return lines, nil
}
