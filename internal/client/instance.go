package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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

// QuitWait is how long Quit waits for the client to be gone.
const QuitWait = 10 * time.Second

// Quit quits the client running for this config directory, if one does,
// stopping the sandboxes it started without asking their chats to wrap up,
// and waits until it is gone.
func Quit(configDirectory string) error {
	running, ok := Running(configDirectory)
	if !ok {
		return nil
	}
	address, err := url.Parse(running)
	if err != nil {
		return err
	}
	request, err := http.NewRequest(http.MethodPost, (&url.URL{Scheme: address.Scheme, Host: address.Host, Path: "/api/quit"}).String(),
		strings.NewReader(`{"wrapUp":false}`))
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
	for deadline := time.Now().Add(QuitWait); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if _, ok := Running(configDirectory); !ok {
			return nil
		}
	}
	return fmt.Errorf("the client did not quit within %s", QuitWait)
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
