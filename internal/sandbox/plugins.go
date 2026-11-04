package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"codeflower-app/internal/agents"
	"codeflower-app/internal/config"
)

// PluginsVariable carries the config's plugins for the worker's agent to the
// bridge, as JSON by agent id; the bridge removes it from its environment.
const PluginsVariable = "CODEFLOWER_PLUGINS"

// PluginsFetchVariable starts the bridge as a fetch run: it fetches the
// project's missing plugins with the agent's own installer, prints JSON lines
// and exits (docs/plugins.md).
const PluginsFetchVariable = "CODEFLOWER_PLUGINS_FETCH"

// ErrImageWithoutPlugins means the fetch run started a worker instead: the
// image is older than plugins.
var ErrImageWithoutPlugins = errors.New("this sandbox image cannot fetch plugins (it started a worker instead); " +
	"update it with `codeflower pull`, or choose a newer --image")

type pluginJSON struct {
	Source  string `json:"source"`
	Enabled bool   `json:"enabled"`
}

// PluginsJSON is the value of PluginsVariable for the agents' entries.
func PluginsJSON(plugins map[string][]config.PluginEntry) string {
	byAgent := make(map[string][]pluginJSON, len(plugins))
	for agent, entries := range plugins {
		list := make([]pluginJSON, 0, len(entries))
		for _, entry := range entries {
			list = append(list, pluginJSON{Source: entry.Source, Enabled: entry.IsEnabled()})
		}
		byAgent[agent] = list
	}
	data, _ := json.Marshal(byAgent)
	return string(data)
}

// HasPlugins is whether the agent's adapter takes plugins.
func HasPlugins(agent string) bool {
	descriptor, ok := agents.Get(agent)
	return ok && len(descriptor.Plugins.Kinds) > 0
}

// PluginHosts are the hosts a fetch of the agent's plugins needs; only the
// fetch run reaches them, never the sandbox.
func PluginHosts(agent string) []string {
	descriptor, _ := agents.Get(agent)
	return descriptor.Plugins.Hosts
}

// The project's plugins/plugins.json and plugins/<agent>/state.json, which the
// bridge writes (plugins.js).
type pluginsProject struct {
	Plugins map[string][]struct {
		Source  string `json:"source"`
		Enabled *bool  `json:"enabled"`
	} `json:"plugins"`
	Catalogs map[string][]struct {
		Source string `json:"source"`
	} `json:"catalogs"`
}

type pluginsState struct {
	Fetched  map[string]json.RawMessage `json:"fetched"`
	Catalogs map[string]json.RawMessage `json:"catalogs"`
}

func readJSONFile(path string, value any) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// PluginsMissing says which enabled plugins and project catalogs of the agent
// were never fetched, from the config's entries and the project's files. A
// fetch that failed is retried only on request (codeflower plugins fetch).
func PluginsMissing(state, agent string, configured []config.PluginEntry) ([]string, error) {
	if !HasPlugins(agent) {
		return nil, nil
	}
	directory := filepath.Join(state, "plugins")
	var project pluginsProject
	var fetched pluginsState
	if err := readJSONFile(filepath.Join(directory, "plugins.json"), &project); err != nil {
		return nil, err
	}
	if err := readJSONFile(filepath.Join(directory, agent, "state.json"), &fetched); err != nil {
		return nil, err
	}
	enabled := map[string]bool{}
	var order []string
	for _, entry := range configured {
		if _, seen := enabled[entry.Source]; !seen {
			order = append(order, entry.Source)
		}
		enabled[entry.Source] = entry.IsEnabled()
	}
	for _, entry := range project.Plugins[agent] {
		_, source, err := config.ParsePluginSource(entry.Source)
		if err != nil {
			continue
		}
		if _, seen := enabled[source]; !seen {
			order = append(order, source)
		}
		enabled[source] = entry.Enabled == nil || *entry.Enabled
	}
	var missing []string
	for _, source := range order {
		if _, ok := fetched.Fetched[agent+":"+source]; enabled[source] && !ok {
			missing = append(missing, source)
		}
	}
	for _, catalog := range project.Catalogs[agent] {
		if _, ok := fetched.Catalogs[catalog.Source]; !ok {
			missing = append(missing, catalog.Source)
		}
	}
	return missing, nil
}

// PluginFetchResult is one line of the fetch run's output.
type PluginFetchResult struct {
	Plugin  string `json:"plugin,omitempty"`
	Catalog string `json:"catalog,omitempty"`
	Version string `json:"version,omitempty"`
	Name    string `json:"name,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
	Errors  int    `json:"errors,omitempty"`
}

// parseFetchOutput reads the fetch run's JSON lines among anything else the
// sandbox printed.
func parseFetchOutput(output []byte) (results []PluginFetchResult, done bool) {
	scanner := bufio.NewScanner(bytes.NewReader(output))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var result PluginFetchResult
		if json.Unmarshal([]byte(line), &result) != nil {
			continue
		}
		switch {
		case result.Done:
			done = true
		case result.Plugin != "" || result.Catalog != "":
			results = append(results, result)
		}
	}
	return results, done
}

// FetchPlugins runs the fetch run: the worker's image and project, started
// briefly with the network (an isolated worker's reaches only the agent's
// plugin hosts), no port and no keys. It returns one result per plugin and
// catalog it fetched.
func (w Worker) FetchPlugins(ctx context.Context) ([]PluginFetchResult, error) {
	if !HasPlugins(w.Agent) {
		return nil, fmt.Errorf("%s has no plugins in code flower", AgentLabel(w.Agent))
	}
	run := w
	run.PluginsFetch = true
	run.Task, run.TaskAttempt, run.InitialPrompt = "", 0, ""
	run.APIKeys, run.Images, run.MCPServers = nil, nil, nil
	run.ServePage, run.InspectPort, run.Port = false, 0, 0
	run.Stdin = nil
	var output bytes.Buffer
	if w.Stdout == nil {
		run.Stdout = &output
	} else {
		run.Stdout = io.MultiWriter(&output, w.Stdout)
	}
	code, err := run.Run(ctx, nil)
	if err != nil {
		return nil, err
	}
	results, done := parseFetchOutput(output.Bytes())
	if !done {
		return results, fmt.Errorf("the plugin fetch ended with %d before it was done", code)
	}
	return results, nil
}

// fetchMissingPlugins runs the agent's fetch run before a worker starts when
// the project has plugins of it that were never fetched. A failure is
// reported, and the agent starts without those plugins.
func (w Worker) fetchMissingPlugins(ctx context.Context, agent string) {
	missing, err := PluginsMissing(w.State, agent, w.Plugins[agent])
	if err != nil {
		fmt.Fprintf(w.Stderr, "  plugins  could not read the project's plugins: %v\n", err)
		return
	}
	if len(missing) == 0 {
		return
	}
	fmt.Fprintf(w.Stderr, "  plugins  fetching %s for %s…\n", strings.Join(missing, ", "), AgentLabel(agent))
	fetch := w
	fetch.Agent = agent
	fetch.Stdout = io.Discard
	results, err := fetch.FetchPlugins(ctx)
	if err != nil {
		fmt.Fprintf(w.Stderr, "  plugins  could not fetch them: %v\n", err)
		return
	}
	ReportFetch(w.Stderr, results)
}

// ReportFetch prints a fetch run's results, one line each.
func ReportFetch(out io.Writer, results []PluginFetchResult) {
	for _, result := range results {
		what := result.Plugin
		if what == "" {
			what = "catalog " + result.Catalog
		}
		switch {
		case result.Error != "":
			fmt.Fprintf(out, "  plugins  %s: %s\n", what, result.Error)
		case result.Version != "":
			fmt.Fprintf(out, "  plugins  %s %s fetched\n", what, result.Version)
		default:
			fmt.Fprintf(out, "  plugins  %s fetched\n", what)
		}
	}
}

// PluginStatus is one of an agent's plugins in a project, for `codeflower plugins list`.
type PluginStatus struct {
	Source  string
	Origin  string // config or project
	Enabled bool
	// State: pending (never fetched), fetched, or error.
	State   string
	Version string
	Error   string
}

type fetchedPlugin struct {
	Version string `json:"version"`
	Error   string `json:"error"`
}

// ListPlugins merges the config's and the project's entries, as the bridge does.
func ListPlugins(state, agent string, configured []config.PluginEntry) ([]PluginStatus, error) {
	directory := filepath.Join(state, "plugins")
	var project pluginsProject
	var fetched struct {
		Fetched map[string]fetchedPlugin `json:"fetched"`
	}
	if err := readJSONFile(filepath.Join(directory, "plugins.json"), &project); err != nil {
		return nil, err
	}
	if err := readJSONFile(filepath.Join(directory, agent, "state.json"), &fetched); err != nil {
		return nil, err
	}
	var result []PluginStatus
	index := map[string]int{}
	for _, entry := range configured {
		index[entry.Source] = len(result)
		result = append(result, PluginStatus{Source: entry.Source, Origin: "config", Enabled: entry.IsEnabled()})
	}
	for _, entry := range project.Plugins[agent] {
		_, source, err := config.ParsePluginSource(entry.Source)
		if err != nil {
			continue
		}
		enabled := entry.Enabled == nil || *entry.Enabled
		if at, ok := index[source]; ok {
			result[at].Enabled = enabled
			continue
		}
		index[source] = len(result)
		result = append(result, PluginStatus{Source: source, Origin: "project", Enabled: enabled})
	}
	for at := range result {
		record, ok := fetched.Fetched[agent+":"+result[at].Source]
		switch {
		case !ok:
			result[at].State = "pending"
		case record.Error != "":
			result[at].State, result[at].Error = "error", record.Error
		default:
			result[at].State, result[at].Version = "fetched", record.Version
		}
	}
	return result, nil
}

// EditProjectPlugins changes the agent's entries in the project's
// plugins/plugins.json, keeping everything else in it. The bridge owns the
// file while a worker of the project runs, so callers check that none does.
func EditProjectPlugins(state, agent string, change func([]PluginEntryJSON) []PluginEntryJSON) error {
	file := filepath.Join(state, "plugins", "plugins.json")
	document := map[string]json.RawMessage{}
	if err := readJSONFile(file, &document); err != nil {
		return err
	}
	plugins := map[string][]PluginEntryJSON{}
	if raw, ok := document["plugins"]; ok {
		if err := json.Unmarshal(raw, &plugins); err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
	}
	plugins[agent] = change(plugins[agent])
	raw, err := json.Marshal(plugins)
	if err != nil {
		return err
	}
	document["plugins"] = raw
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(file), ".plugins.json.*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	_, err = temporary.Write(append(data, '\n'))
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(temporary.Name(), file)
}

// PluginEntryJSON is an entry of plugins.json as the bridge writes it.
type PluginEntryJSON struct {
	Source  string `json:"source"`
	Enabled bool   `json:"enabled"`
}
