package client

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// instance is the running client of a config directory, so that starting
// the client again (a second double-click) opens the running one. Its URL
// holds the page's secret: the file is private to the user.
type instance struct {
	PID int    `json:"pid"`
	URL string `json:"url"`
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
	// The client answers its secret with the cookie and a redirect to the page.
	client := &http.Client{
		Timeout:       2 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	response, err := client.Get(running.URL)
	if err != nil {
		return "", false
	}
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther {
		return "", false
	}
	return running.URL, true
}

// recordInstance writes the running client's address; the returned function
// removes it, unless another client replaced it meanwhile.
func recordInstance(configDirectory, url string) func() {
	if configDirectory == "" {
		return func() {}
	}
	path := instancePath(configDirectory)
	data, _ := json.Marshal(instance{PID: os.Getpid(), URL: url})
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
