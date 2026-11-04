package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"codeflower-app/internal/config"
)

func enabled(value bool) *bool { return &value }

func TestPluginsJSONGivesTheAgentsEntries(t *testing.T) {
	got := PluginsJSON(map[string][]config.PluginEntry{"claude": {{Source: "github:a/b"}, {Source: "path:/x", Enabled: enabled(false)}}})
	want := `{"claude":[{"source":"github:a/b","enabled":true},{"source":"path:/x","enabled":false}]}`
	if got != want {
		t.Fatalf("got %s", got)
	}
	if !HasPlugins("claude") || HasPlugins("codex") || !slices.Contains(PluginHosts("claude"), "github.com") {
		t.Fatal("the Claude descriptor's plugins block is not read")
	}
}

func TestProjectPluginsMergeWithTheConfigsAndAreEditedInPlace(t *testing.T) {
	state := t.TempDir()
	directory := filepath.Join(state, "plugins")
	if err := os.MkdirAll(filepath.Join(directory, "claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	// As the bridge writes them, with a catalog this app does not change.
	project := `{"plugins":{"claude":[{"source":"github:org/from-config","enabled":false},{"source":"github:org/mine"},{"source":"github:org/bad","enabled":true}]},"catalogs":{"claude":[{"source":"github:org/catalog"}]}}`
	if err := os.WriteFile(filepath.Join(directory, "plugins.json"), []byte(project), 0o600); err != nil {
		t.Fatal(err)
	}
	fetched := `{"fetched":{"claude:github:org/mine":{"version":"1.0.0"},"claude:github:org/bad":{"error":"not found"}}}`
	if err := os.WriteFile(filepath.Join(directory, "claude", "state.json"), []byte(fetched), 0o600); err != nil {
		t.Fatal(err)
	}
	configured := []config.PluginEntry{{Source: "github:org/from-config"}, {Source: "github:org/new"}}

	list, err := ListPlugins(state, "claude", configured)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, entry := range list {
		lines = append(lines, strings.Join([]string{entry.Source, entry.Origin, entry.State, entry.Version, entry.Error}, " "))
		if entry.Source == "github:org/from-config" && entry.Enabled {
			t.Error("the project's switch did not apply to the config's entry")
		}
	}
	want := []string{
		"github:org/from-config config pending  ",
		"github:org/new config pending  ",
		"github:org/mine project fetched 1.0.0 ",
		"github:org/bad project error  not found",
	}
	if !slices.Equal(lines, want) {
		t.Fatalf("got %q", lines)
	}

	// Never fetched and on: the new config entry and the catalog; failures wait for a fetch on request.
	missing, err := PluginsMissing(state, "claude", configured)
	if err != nil || !slices.Equal(missing, []string{"github:org/new", "github:org/catalog"}) {
		t.Fatalf("got %v, %v", missing, err)
	}

	if err := EditProjectPlugins(state, "claude", func(entries []PluginEntryJSON) []PluginEntryJSON {
		return append(entries[:1], PluginEntryJSON{Source: "path:/x", Enabled: true})
	}); err != nil {
		t.Fatal(err)
	}
	var document map[string]json.RawMessage
	data, _ := os.ReadFile(filepath.Join(directory, "plugins.json"))
	var compact bytes.Buffer
	if err := json.Compact(&compact, data); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(compact.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if string(document["catalogs"]) != `{"claude":[{"source":"github:org/catalog"}]}` ||
		!strings.Contains(string(document["plugins"]), `"path:/x"`) || strings.Contains(string(document["plugins"]), "org/mine") {
		t.Fatalf("got %s", data)
	}
}

func TestWorkerFetchesMissingPluginsFirstWithoutKeys(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a shell script as the engine")
	}
	directory := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(directory, "config"))
	state := filepath.Join(directory, "project")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(directory, "log")
	// The fetch run records the plugin in the project as the bridge does, and prints its lines.
	engine := filepath.Join(directory, "apptainer")
	script := "#!/bin/sh\n[ \"$1\" = inspect ] && { echo '{}'; exit 0; }\n" +
		"if [ \"$APPTAINERENV_CODEFLOWER_PLUGINS_FETCH\" = 1 ]; then\n" +
		"  echo \"fetch key=$APPTAINERENV_CODEFLOWER_API_KEYS port=$APPTAINERENV_CODEFLOWER_PORT plugins=$APPTAINERENV_CODEFLOWER_PLUGINS\" >> " + log + "\n" +
		"  mkdir -p " + state + "/plugins/claude\n" +
		"  echo '{\"fetched\":{\"claude:github:org/mine\":{\"version\":\"1.0.0\"}}}' > " + state + "/plugins/claude/state.json\n" +
		"  echo 'some log line'\n  echo '{\"plugin\":\"claude:github:org/mine\",\"version\":\"1.0.0\"}'\n  echo '{\"done\":true,\"errors\":0}'\n  exit 0\nfi\n" +
		"echo \"worker fetch=$APPTAINERENV_CODEFLOWER_PLUGINS_FETCH plugins=$APPTAINERENV_CODEFLOWER_PLUGINS\" >> " + log + "\n"
	if err := os.WriteFile(engine, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	worker := Worker{
		Agent:   "claude",
		Engine:  EngineStatus{Name: EngineApptainer, Executable: engine},
		Image:   filepath.Join(directory, "image.sif"),
		State:   state,
		APIKeys: map[string]string{"claude": "sk-secret"},
		Plugins: map[string][]config.PluginEntry{"claude": {{Source: "github:org/mine"}}},
		Stdout:  io.Discard, Stderr: io.Discard,
	}
	for range 2 {
		if code, err := worker.Run(context.Background(), nil); code != 0 || err != nil {
			t.Fatalf("run: %d, %v", code, err)
		}
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	plugins := `{"claude":[{"source":"github:org/mine","enabled":true}]}`
	want := "fetch key= port=0 plugins=" + plugins + "\nworker fetch= plugins=" + plugins + "\nworker fetch= plugins=" + plugins + "\n"
	if string(data) != want {
		t.Fatalf("got\n%s\nwant\n%s", data, want)
	}

	results, err := worker.FetchPlugins(context.Background())
	if err != nil || len(results) != 1 || results[0].Plugin != "claude:github:org/mine" || results[0].Version != "1.0.0" {
		t.Fatalf("got %+v, %v", results, err)
	}
	fetch := worker
	fetch.PluginsFetch = true
	if hosts := fetch.providerHosts(); slices.Contains(hosts, "api.anthropic.com") || !slices.Contains(hosts, "github.com") {
		t.Fatalf("the fetch run reaches %v", hosts)
	}
}
