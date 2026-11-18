package cli

import (
	"strings"
	"testing"

	"codeflower-app/internal/client"
)

func TestOnlyAnOlderReleaseIsRestarted(t *testing.T) {
	cases := []struct {
		running, this string
		older         bool
	}{
		{"v0.39.0", "v0.40.0", true},
		{"v0.40.0", "v0.40.1", true},
		{"v0.40.0", "v0.40.0", false},
		{"v0.41.0", "v0.40.0", false},
		// Development builds on either side: nothing restarts by itself.
		{"v0.39.0-3-gabc1234", "v0.40.0", false},
		{"v0.39.0", "v0.40.0-dirty", false},
		{"dev", "v0.40.0", false},
		{"", "v0.40.0", false},
	}
	for _, c := range cases {
		if got := olderRelease(c.running, c.this); got != c.older {
			t.Errorf("olderRelease(%q, %q) = %v", c.running, c.this, got)
		}
	}
}

func TestARestartRunsTheClientAgainInTheBackground(t *testing.T) {
	join := func(args []string) string { return strings.Join(args, " ") }
	if got := join(backgroundArgs(nil, "/home/me/.codeflower")); got != "--background --config /home/me/.codeflower" {
		t.Fatalf("no arguments: %s", got)
	}
	// Its own options stay; a double-click's flag goes, and the config it named is kept.
	got := join(backgroundArgs([]string{"--desktop", "--ui", "/tmp/page", "--connect-only", "--config=/srv/cf"}, "/home/me/.codeflower"))
	if got != "--background --ui /tmp/page --connect-only --config=/srv/cf" {
		t.Fatalf("its options: %s", got)
	}
	if got := join(backgroundArgs([]string{"--background", "--config", "/srv/cf"}, "/x")); got != "--background --config /srv/cf" {
		t.Fatalf("a background client: %s", got)
	}
	if got := join(clientArgs([]string{"gui", "--port", "8000"})); got != "--port 8000" {
		t.Fatalf("after the alias: %s", got)
	}
	if got := clientArgs([]string{"--desktop"}); got == nil || len(got) != 0 {
		t.Fatalf("started without the command: %v", got)
	}
}

func TestTheNoteSaysWhatHappenedToTheClient(t *testing.T) {
	previous := Version
	Version = "v0.41.0"
	t.Cleanup(func() { Version = previous })
	if note := (clientStart{started: true, pid: 42}).note(); !strings.Contains(note, "Started code flower client v0.41.0") || !strings.Contains(note, "pid 42") {
		t.Fatalf("started: %s", note)
	}
	if note := (clientStart{replaced: "v0.40.0", pid: 43}).note(); !strings.Contains(note, "v0.40.0 → v0.41.0") {
		t.Fatalf("replaced: %s", note)
	}
	window := clientStart{stale: &client.Client{Version: "v0.40.0"}}.note()
	if !strings.Contains(window, "runs in a window") || !strings.Contains(window, "codeflower client restart") {
		t.Fatalf("a window: %s", window)
	}
	busy := clientStart{stale: &client.Client{Version: "v0.40.0", Background: true, Sandboxes: 2}}.note()
	if !strings.Contains(busy, "would stop the 2 sandboxes it started") {
		t.Fatalf("running sandboxes: %s", busy)
	}
	if note := (clientStart{}).note(); note != "" {
		t.Fatalf("nothing happened: %s", note)
	}
}
