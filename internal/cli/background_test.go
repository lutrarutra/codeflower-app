package cli

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"codeflower-app/internal/tunnel"
)

func TestPairWaitsForANewBrowser(t *testing.T) {
	directory := t.TempDir()
	state := &tunnel.State{URL: "https://tunnel.example", ConnectorID: "c1", Devices: []tunnel.Device{{Key: "old", Label: "laptop"}}}
	if err := state.Save(directory); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(500 * time.Millisecond)
		_ = tunnel.Update(directory, func(current *tunnel.State) error {
			current.Devices = append(current.Devices, tunnel.Device{Key: "new", Label: "phone"})
			return nil
		})
	}()
	device, err := waitForPairing(context.Background(), directory, map[string]bool{"old": true}, time.Now().Add(10*time.Second))
	if err != nil || device.Label != "phone" {
		t.Fatalf("got %+v, %v", device, err)
	}
}

func TestPairStopsWaitingWhenTheLinkExpires(t *testing.T) {
	directory := t.TempDir()
	if err := (&tunnel.State{URL: "https://tunnel.example"}).Save(directory); err != nil {
		t.Fatal(err)
	}
	_, err := waitForPairing(context.Background(), directory, map[string]bool{}, time.Now().Add(1500*time.Millisecond))
	if err == nil || !strings.Contains(err.Error(), "within 10 minutes") {
		t.Fatalf("got %v", err)
	}
}

func TestPairRefusesAConfigSignedInOnAnotherComputer(t *testing.T) {
	directory := t.TempDir()
	if err := (&tunnel.State{URL: "https://tunnel.example", Host: "login01.cluster"}).Save(directory); err != nil {
		t.Fatal(err)
	}
	command := tunnelCommand()
	command.SilenceUsage = true
	command.SetArgs([]string{"pair", "--config", directory, "--no-wait"})
	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), "login01.cluster signed in") {
		t.Fatalf("got %v", err)
	}
}

func TestOnlyTheComputerThatSignedInConnects(t *testing.T) {
	host, _ := os.Hostname()
	for _, c := range []struct {
		host string
		want bool
	}{{"", true}, {host, true}, {"login01.cluster", false}} {
		if got := (&tunnel.State{Host: c.host}).OnThisHost(); got != c.want {
			t.Errorf("host %q: got %v", c.host, got)
		}
	}
}
