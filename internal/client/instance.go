package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// instance is the running client of a config directory, so that starting
// the client again (a second double-click) opens the running one. Its URL
// holds the page's secret: the file is private to the user.
type instance struct {
	PID int    `json:"pid"`
	URL string `json:"url"`
	// Version, Background and Args (its command line after "client") are
	// missing from clients before v0.41.0.
	Version    string   `json:"version,omitempty"`
	Background bool     `json:"background,omitempty"`
	Args       []string `json:"args,omitempty"`
}

func instancePath(configDirectory string) string {
	return filepath.Join(configDirectory, "client.json")
}

// Running returns the page address of the client already running for this
// config directory, if one answers.
func Running(configDirectory string) (string, bool) {
	if configDirectory == "" {
		return "", false
	}
	data, err := os.ReadFile(instancePath(configDirectory))
	if err != nil {
		return "", false
	}
	var running instance
	if json.Unmarshal(data, &running) != nil || running.URL == "" {
		return "", false
	}
	// A client answers its API with its secret, and refuses any other.
	address, err := url.Parse(running.URL)
	if err != nil {
		return "", false
	}
	request, err := http.NewRequest(http.MethodGet, (&url.URL{Scheme: address.Scheme, Host: address.Host, Path: "/api/quit"}).String(), nil)
	if err != nil {
		return "", false
	}
	request.Header.Set("Authorization", "Bearer "+address.Query().Get("key"))
	response, err := (&http.Client{Timeout: 2 * time.Second}).Do(request)
	if err != nil {
		return "", false
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", false
	}
	return running.URL, true
}

// Client is the client running for a config directory, as another
// codeflower sees it, e.g. before restarting it.
type Client struct {
	URL string
	PID int
	// Version is its release ("v0.40.0", or a development build's).
	Version string
	// Background: it runs without a window (`--background`). Args are its
	// command line after "client"; for clients before v0.41.0 both come
	// from the process, where the system tells.
	Background bool
	Args       []string
	// Sandboxes it started that still run: quitting it stops them.
	Sandboxes int
}

// Describe returns the client running for this config directory, if one
// answers.
func Describe(configDirectory string) (Client, bool) {
	data, err := os.ReadFile(instancePath(configDirectory))
	if err != nil {
		return Client{}, false
	}
	var record instance
	if json.Unmarshal(data, &record) != nil || record.URL == "" {
		return Client{}, false
	}
	var plan QuitPlan
	if apiGet(record.URL, "/api/quit", &plan) != nil {
		return Client{}, false
	}
	found := Client{URL: record.URL, PID: record.PID, Version: record.Version, Background: record.Background,
		Args: record.Args, Sandboxes: plan.Sandboxes}
	if found.Version == "" {
		var system struct {
			Version string `json:"version"`
		}
		if apiGet(record.URL, "/api/system", &system) == nil {
			found.Version = system.Version
		}
	}
	if record.Args == nil {
		if args, ok := processClientArgs(record.PID); ok {
			found.Args = args
			found.Background = slices.Contains(args, "--background")
		}
	}
	return found, true
}

// processClientArgs reads a running client's command line after "client"
// from the system (ps), for clients that did not record it.
func processClientArgs(pid int) ([]string, bool) {
	if pid <= 0 || runtime.GOOS == "windows" {
		return nil, false
	}
	output, err := exec.Command("ps", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return nil, false
	}
	fields := strings.Fields(string(output))
	for i, field := range fields {
		if field == "client" || field == "gui" {
			return fields[i+1:], true
		}
	}
	return nil, false
}

// apiGet asks the client's page API, with the secret in its address.
func apiGet(page, path string, into any) error {
	address, err := url.Parse(page)
	if err != nil {
		return err
	}
	request, err := http.NewRequest(http.MethodGet, (&url.URL{Scheme: address.Scheme, Host: address.Host, Path: path}).String(), nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+address.Query().Get("key"))
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("the client answered %s", response.Status)
	}
	return json.NewDecoder(response.Body).Decode(into)
}

// QuitWait is how long Quit waits for the client to be gone.
const QuitWait = 10 * time.Second

// Quit quits the client running for this config directory, if one does,
// stopping the sandboxes it started without asking their chats to wrap up,
// and waits until it is gone.
func Quit(configDirectory string) error {
	return QuitWith(configDirectory, false, QuitWait)
}

// QuitWith quits the client as Quit does; with wrapUp, the working chats of
// the sandboxes it started are asked to wrap up first, and it waits up to
// wait for it to be gone.
func QuitWith(configDirectory string, wrapUp bool, wait time.Duration) error {
	running, ok := Running(configDirectory)
	if !ok {
		return nil
	}
	address, err := url.Parse(running)
	if err != nil {
		return err
	}
	request, err := http.NewRequest(http.MethodPost, (&url.URL{Scheme: address.Scheme, Host: address.Host, Path: "/api/quit"}).String(),
		strings.NewReader(fmt.Sprintf(`{"wrapUp":%t}`, wrapUp)))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+address.Query().Get("key"))
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		return err
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("the client answered %s", response.Status)
	}
	for deadline := time.Now().Add(wait); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if _, ok := Running(configDirectory); !ok {
			return nil
		}
	}
	return fmt.Errorf("the client did not quit within %s", wait)
}

// recordInstance writes the running client's address; the returned function
// removes it, unless another client replaced it meanwhile.
func recordInstance(configDirectory string, record instance) func() {
	if configDirectory == "" {
		return func() {}
	}
	path := instancePath(configDirectory)
	record.PID = os.Getpid()
	data, _ := json.Marshal(record)
	if err := os.MkdirAll(configDirectory, 0o700); err != nil {
		return func() {}
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil || os.Rename(temporary, path) != nil {
		_ = os.Remove(temporary)
		return func() {}
	}
	return func() {
		if current, err := os.ReadFile(path); err == nil && bytes.Equal(current, data) {
			_ = os.Remove(path)
		}
	}
}
