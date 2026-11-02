package attach

import (
	"errors"
	"strings"
	"testing"

	"codeflower-app/internal/sandbox"
)

func TestEscapesWorkAfterEnterOnly(t *testing.T) {
	cases := []struct {
		name   string
		reads  []string
		typed  string
		action byte
	}{
		{"plain text", []string{"hello"}, "hello", 0},
		{"detach at the start", []string{"~."}, "", '.'},
		{"detach after Enter", []string{"ls\r", "~", "."}, "ls\r", '.'},
		{"switch", []string{"\r~s"}, "\r", 's'},
		{"quit", []string{"\n~q"}, "\n", 'q'},
		{"a tilde mid-line is typed", []string{"a~.b"}, "a~.b", 0},
		{"a doubled tilde types one", []string{"~~x"}, "~x", 0},
		{"another letter types both", []string{"~x"}, "~x", 0},
		{"after an escape the rest is dropped", []string{"~.more"}, "", '.'},
		{"links", []string{"~l"}, "", 'l'},
		{"open the last file", []string{"\r~o"}, "\r", 'o'},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			escapes := &escaper{atLineStart: true}
			var typed strings.Builder
			var action byte
			for _, read := range c.reads {
				sent, got := escapes.feed([]byte(read))
				typed.Write(sent)
				if got != 0 {
					action = got
					break
				}
			}
			if typed.String() != c.typed || action != c.action {
				t.Fatalf("typed %q action %q, want %q %q", typed.String(), action, c.typed, c.action)
			}
		})
	}
}

func TestResolvesSandboxes(t *testing.T) {
	targets := []Target{
		{ID: "local-1a2b3c4d", RunID: "1a2b3c4d-aaaa", Name: "study42", Source: "local"},
		{ID: "local-9f8e7d6c", RunID: "9f8e7d6c-bbbb", Name: "rnaseq", Source: "local"},
		{ID: "w-remote", Name: "hpc", Source: "added"},
	}
	for argument, want := range map[string]string{
		"study42":        "local-1a2b3c4d",
		"RNASEQ":         "local-9f8e7d6c",
		"local-9f8e7d6c": "local-9f8e7d6c",
		"1a2b":           "local-1a2b3c4d",
		"3":              "w-remote",
		"hpc":            "w-remote",
	} {
		got, err := ResolveTarget(argument, targets)
		if err != nil || got.ID != want {
			t.Errorf("%q: got %q, %v; want %q", argument, got.ID, err, want)
		}
	}
	if _, err := ResolveTarget("", targets); !errors.Is(err, errAmbiguous) {
		t.Errorf("no argument with several: %v", err)
	}
	if got, err := ResolveTarget("", targets[:1]); err != nil || got.ID != "local-1a2b3c4d" {
		t.Errorf("no argument with one: %v %v", got, err)
	}
	if _, err := ResolveTarget("", nil); err == nil || !strings.Contains(err.Error(), "no sandbox runs") {
		t.Errorf("none: %v", err)
	}
	if _, err := ResolveTarget("nope", targets); err == nil {
		t.Error("an unknown name resolved")
	}
	line, err := ResolveTarget("codeflower://10.0.0.5:41234?token=abc", targets)
	if err != nil || line.Connection != (sandbox.Connection{URL: "codeflower://10.0.0.5:41234?token=abc", Host: "10.0.0.5", Port: 41234, Token: "abc"}) {
		t.Errorf("line: %+v %v", line, err)
	}
}

func TestResolvesChats(t *testing.T) {
	chats := []Chat{
		{Key: "opencode-ses_1", Title: "PCA of batch 3"},
		{Key: "opencode-ses_2", Title: "PCA of batch 4"},
		{Key: "opencode-ses_3", Title: "Fix QC thresholds"},
	}
	for argument, want := range map[string]string{
		"2":              "opencode-ses_2",
		"opencode-ses_3": "opencode-ses_3",
		"fix":            "opencode-ses_3",
		"PCA of batch 3": "opencode-ses_1",
	} {
		got, err := ResolveChat(argument, chats)
		if err != nil || got.Key != want {
			t.Errorf("%q: got %q, %v; want %q", argument, got.Key, err, want)
		}
	}
	if _, err := ResolveChat("pca", chats); err == nil || !strings.Contains(err.Error(), "2 chats") {
		t.Errorf("ambiguous: %v", err)
	}
}

func TestLinksToTheSandboxsPage(t *testing.T) {
	page := pageLinks{base: "http://127.0.0.1:7720/", worker: "local-1a2b3c4d"}
	if got := page.fileBase(); got != "http://127.0.0.1:7720/#/file/local-1a2b3c4d/" {
		t.Errorf("file base %q", got)
	}
	if got := page.chat("opencode-ses_1"); got != "http://127.0.0.1:7720/#/chat/local-1a2b3c4d/opencode-ses_1" {
		t.Errorf("chat %q", got)
	}
	tunnel := pageLinks{base: "https://tunnel.example/m/c1/", worker: "w@x"}
	if got := tunnel.fileBase(); got != "https://tunnel.example/m/c1/#/file/w@x/" {
		t.Errorf("tunnel file base %q", got)
	}
	if (pageLinks{}).fileBase() != "" || (pageLinks{}).chat("k") != "" {
		t.Error("links without a page")
	}
	if got := findPage(LinksPath, t.TempDir(), Target{ID: "local-1"}); got.base != "" || got.note != "" {
		t.Errorf("--links path: %+v", got)
	}
	if got := findPage(LinksAuto, t.TempDir(), Target{ID: "local-1"}); got.base != "" || !strings.Contains(got.note, "codeflower client") {
		t.Errorf("no client: %+v", got)
	}
	if got := findPage(LinksAuto, t.TempDir(), Target{Source: "line"}); got.base != "" || !strings.Contains(got.note, "line") {
		t.Errorf("a line: %+v", got)
	}
}
