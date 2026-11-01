// Command gendocs writes the reference that the docs site (codeflower-docs)
// generates from the app: one page per CLI command, the JSON Schema of
// config.yaml, the network allowlist presets, and the versions this build pins.
// The workspace's `make site-gen` runs it; it is not part of a release.
//
//	go run ./cmd/gendocs -cli DIR -data DIR -schema FILE [-version vX.Y.Z]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"codeflower-app/internal/cli"
	"codeflower-app/internal/client"
	"codeflower-app/internal/sandbox"
	"codeflower-app/internal/tunnel"
)

func main() {
	cliDir := flag.String("cli", "", "directory for the CLI pages (Markdown)")
	dataDir := flag.String("data", "", "directory for presets.json and app.json")
	schemaFile := flag.String("schema", "", "file for the config.yaml JSON Schema")
	configSource := flag.String("config-source", "internal/config", "the config package's source, for its comments")
	version := flag.String("version", "dev", "the app version being documented")
	flag.Parse()
	if *cliDir == "" || *dataDir == "" || *schemaFile == "" {
		flag.Usage()
		os.Exit(2)
	}
	// Flag defaults and help read these; the docs show those of a clean
	// machine, with the home directory as ~.
	os.Setenv("HOME", "~")
	for _, variable := range os.Environ() {
		name, _, _ := strings.Cut(variable, "=")
		if strings.HasPrefix(name, "CODEFLOWER_") {
			os.Unsetenv(name)
		}
	}
	if err := run(*cliDir, *dataDir, *schemaFile, *configSource, *version); err != nil {
		fmt.Fprintf(os.Stderr, "gendocs: %v\n", err)
		os.Exit(1)
	}
}

func run(cliDir, dataDir, schemaFile, configSource, version string) error {
	if err := writeCLI(cli.NewRootCommand(), cliDir); err != nil {
		return err
	}
	schema, err := configSchema(configSource)
	if err != nil {
		return err
	}
	if err := writeJSON(schemaFile, schema); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dataDir, "presets.json"), sandbox.AllowlistPresets); err != nil {
		return err
	}
	return writeJSON(filepath.Join(dataDir, "app.json"), map[string]string{
		"version":    version,
		"image":      sandbox.ImageRepository,
		"imageTag":   sandbox.DefaultImageTag,
		"tunnelURL":  tunnel.DefaultURL,
		"clientPort": fmt.Sprint(client.DefaultPort),
	})
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}
