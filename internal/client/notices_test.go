package client

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// poll decodes a /status body as the bridge sends it.
func poll(t *testing.T, body string) *activity {
	t.Helper()
	var current activity
	if err := json.Unmarshal([]byte(body), &current); err != nil {
		t.Fatal(err)
	}
	return &current
}

func TestNoticesForWaitingAndEndedChats(t *testing.T) {
	tracker := &noticeTracker{}
	// The first poll only sets what is known: an old question, an old turn.
	first := poll(t, `{"turns":1,"ended":[{"seq":1,"key":"k0","title":"Old","stopReason":"end_turn"}],
		"chats":[{"key":"k1","title":"Plot QC","state":"waiting","waitingFor":"question"},{"key":"k2","title":"Load data","state":"working"}]}`)
	if lines := tracker.next(first); lines != nil {
		t.Fatalf("first poll told %q", lines)
	}
	// The same question is not told again; a new permission request and an
	// ended turn are, a cancelled one is not.
	second := poll(t, `{"turns":4,"ended":[
		{"seq":1,"key":"k0","title":"Old","stopReason":"end_turn"},
		{"seq":2,"key":"k2","title":"Load data","stopReason":"end_turn"},
		{"seq":3,"key":"k3","title":"Stopped","stopReason":"cancelled"},
		{"seq":4,"key":"k4","title":"","stopReason":"max_tokens"}],
		"chats":[{"key":"k1","title":"Plot QC","state":"waiting","waitingFor":"question"},{"key":"k5","title":"Clean","state":"waiting","waitingFor":"permission"}]}`)
	want := []string{"“Load data” finished", "“Untitled session” stopped (max tokens)", "“Clean” asks for your permission"}
	if lines := tracker.next(second); !reflect.DeepEqual(lines, want) {
		t.Fatalf("got %q, want %q", lines, want)
	}
	// Waiting for something else is told again.
	third := poll(t, `{"turns":4,"ended":[],"chats":[{"key":"k1","title":"Plot QC","state":"waiting","waitingFor":"plan"}]}`)
	if lines := tracker.next(third); !reflect.DeepEqual(lines, []string{"“Plot QC” has a plan for you to approve"}) {
		t.Fatalf("got %q", lines)
	}
	// A bridge that did not answer starts over, and a restarted one counts
	// from 0: its first turn is told once polls resume.
	tracker.next(nil)
	if lines := tracker.next(poll(t, `{"turns":0,"chats":[]}`)); lines != nil {
		t.Fatalf("after a gap told %q", lines)
	}
	restarted := poll(t, `{"turns":1,"ended":[{"seq":1,"key":"k6","title":"Again","stopReason":"error"}],"chats":[]}`)
	if lines := tracker.next(restarted); !reflect.DeepEqual(lines, []string{"“Again” stopped with an error"}) {
		t.Fatalf("got %q", lines)
	}
}

func TestNoticesFromAnOlderBridge(t *testing.T) {
	tracker := &noticeTracker{}
	tracker.next(poll(t, `{"chats":[{"key":"k1","title":"Plot QC","state":"working"},{"key":"k2","title":"Other","state":"working"}]}`))
	lines := tracker.next(poll(t, `{"chats":[{"key":"k2","title":"Other","state":"working"}]}`))
	if !reflect.DeepEqual(lines, []string{"“Plot QC” finished"}) {
		t.Fatalf("got %q", lines)
	}
}

func TestNoticeNamesTheSandboxAndOpensIt(t *testing.T) {
	server, _ := testServer(t)
	var told []Notice
	server.options.Notify = func(notice Notice) { told = append(told, notice) }
	item := &worker{ID: "w1", Project: "atlas", label: "Claude Code", url: "codeflower://127.0.0.1:4100?token=t"}
	polled := server.noticesFor(item)
	polled(poll(t, `{"turns":0,"chats":[]}`))
	polled(poll(t, `{"turns":5,"ended":[
		{"seq":1,"key":"a","title":"One","stopReason":"end_turn"},
		{"seq":2,"key":"b","title":"Two","stopReason":"end_turn"},
		{"seq":3,"key":"c","title":"Three","stopReason":"end_turn"},
		{"seq":4,"key":"d","title":"Four","stopReason":"end_turn"},
		{"seq":5,"key":"e","title":"Five","stopReason":"cancelled"}],"chats":[]}`))
	if len(told) != 1 {
		t.Fatalf("told %d notices, want one per poll", len(told))
	}
	want := Notice{
		Title: "Claude Code · atlas",
		Body:  "“One” finished\n“Two” finished\nand 2 more",
		URL:   "vscode://codeflower.codeflower/connect?address=codeflower%3A%2F%2F127.0.0.1%3A4100%3Ftoken%3Dt",
	}
	if told[0] != want {
		t.Fatalf("got %+v, want %+v", told[0], want)
	}

	// While the client quits, the wrap-up's ended turns are not told.
	server.mu.Lock()
	server.quitting = &quitting{Workers: map[string]string{}, cancel: func() {}}
	server.mu.Unlock()
	polled(poll(t, `{"turns":6,"ended":[{"seq":6,"key":"a","title":"One","stopReason":"end_turn"}],"chats":[]}`))
	if len(told) != 1 {
		t.Fatalf("told %q while quitting", told[1:])
	}
}

func TestNoNoticesWhenTurnedOff(t *testing.T) {
	server, _ := testServer(t)
	if server.noticesFor(&worker{}) != nil {
		t.Fatal("polls are watched without Notify")
	}
	if !strings.HasPrefix(vscodeLink("x"), "vscode://codeflower.codeflower/connect?address=") {
		t.Fatal(vscodeLink("x"))
	}
}
