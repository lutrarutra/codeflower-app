package client

import (
	"os"
	"runtime"
	"testing"
)

func TestASecondStartFindsTheRunningClient(t *testing.T) {
	directory := t.TempDir()
	if _, ok := Running(directory); ok {
		t.Fatal("found a client before any started")
	}
	_, httpServer := testServer(t)
	url := httpServer.URL + "/?key=secret-key"
	forget := recordInstance(directory, url)
	if found, ok := Running(directory); !ok || found != url {
		t.Fatalf("running: %q %v", found, ok)
	}
	if info, err := os.Stat(instancePath(directory)); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Fatalf("the address with its secret is not private: %v %v", info, err)
	}

	// A wrong secret, or a client that is gone, is not running.
	recordInstance(directory, httpServer.URL+"/?key=wrong")
	if _, ok := Running(directory); ok {
		t.Fatal("a client that refused the secret counts as running")
	}
	// Removing leaves another client's record alone.
	forget()
	if _, err := os.Stat(instancePath(directory)); err != nil {
		t.Fatal("removed another client's record")
	}
	httpServer.Close()
	recordInstance(directory, url)
	if _, ok := Running(directory); ok {
		t.Fatal("a stopped client counts as running")
	}
}
