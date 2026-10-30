package client

import (
	"fmt"
	"net/url"
	"strings"
)

// Notice is a desktop notification: a chat in a connected sandbox waits for
// the user or ended its turn. URL opens the sandbox in VS Code.
type Notice struct {
	Title string
	Body  string
	URL   string
}

// endedTurn is a turn the bridge saw end; StopReason is the agent's
// (end_turn, cancelled, max_tokens, …) or error.
type endedTurn struct {
	Seq        int    `json:"seq"`
	Key        string `json:"key"`
	Title      string `json:"title"`
	StopReason string `json:"stopReason"`
}

// maxNoticeLines is how many chats one notice names before "and N more".
const maxNoticeLines = 3

// noticeTracker compares each poll of a worker with the one before.
type noticeTracker struct {
	last *activity
	// turns is the bridge's count of ended turns seen so far.
	turns int
}

// next returns what changed since the last poll, one line per chat. The
// first poll (and the first after the bridge did not answer) only sets what
// is known, so nothing from before is told again.
func (t *noticeTracker) next(current *activity) []string {
	if current == nil {
		t.last = nil
		return nil
	}
	previous := t.last
	t.last = current
	var lines []string
	if current.Turns != nil {
		if *current.Turns < t.turns {
			// A new bridge (the sandbox restarted) counts from 0 again.
			t.turns = 0
		}
		if previous != nil {
			for _, turn := range current.Ended {
				if turn.Seq > t.turns {
					if line := endedLine(turn); line != "" {
						lines = append(lines, line)
					}
				}
			}
		}
		t.turns = *current.Turns
	}
	if previous == nil {
		return nil
	}
	before := make(map[string]chatActivity, len(previous.Chats))
	for _, chat := range previous.Chats {
		before[chat.Key] = chat
	}
	now := make(map[string]bool, len(current.Chats))
	for _, chat := range current.Chats {
		now[chat.Key] = true
		if chat.State != "waiting" {
			continue
		}
		if old, ok := before[chat.Key]; ok && old.State == "waiting" && old.WaitingFor == chat.WaitingFor {
			continue
		}
		lines = append(lines, waitingLine(chat))
	}
	if current.Turns == nil {
		// An older bridge: a working chat that is gone has finished.
		for _, chat := range previous.Chats {
			if chat.State == "working" && !now[chat.Key] {
				lines = append(lines, fmt.Sprintf("%s finished", quoted(chat.Title)))
			}
		}
	}
	return lines
}

func waitingLine(chat chatActivity) string {
	switch chat.WaitingFor {
	case "question":
		return quoted(chat.Title) + " asks you a question"
	case "plan":
		return quoted(chat.Title) + " has a plan for you to approve"
	default:
		return quoted(chat.Title) + " asks for your permission"
	}
}

// endedLine is "" for a turn the user cancelled.
func endedLine(turn endedTurn) string {
	switch turn.StopReason {
	case "cancelled":
		return ""
	case "end_turn", "":
		return quoted(turn.Title) + " finished"
	case "error":
		return quoted(turn.Title) + " stopped with an error"
	default:
		return fmt.Sprintf("%s stopped (%s)", quoted(turn.Title), strings.ReplaceAll(turn.StopReason, "_", " "))
	}
}

func quoted(title string) string {
	if title == "" {
		title = "Untitled session"
	}
	return "“" + title + "”"
}

// notice puts one poll's lines for a worker into one notification.
func notice(title, address string, lines []string) Notice {
	body := lines
	if len(lines) > maxNoticeLines {
		body = append(slicesClone(lines[:maxNoticeLines-1]), fmt.Sprintf("and %d more", len(lines)-maxNoticeLines+1))
	}
	result := Notice{Title: title, Body: strings.Join(body, "\n")}
	if address != "" {
		result.URL = vscodeLink(address)
	}
	return result
}

// vscodeLink opens the worker in VS Code, as the page's Open in VS Code does.
func vscodeLink(address string) string {
	return "vscode://codeflower.codeflower/connect?" + url.Values{"address": {address}}.Encode()
}

// noticesFor returns the worker's poll callback for desktop notifications,
// or nil when they are off. Nothing is told while the client quits: the
// wrap-up ends every chat's turn.
func (s *Server) noticesFor(item *worker) func(*activity) {
	if s.options.Notify == nil {
		return nil
	}
	tracker := &noticeTracker{}
	return func(current *activity) {
		lines := tracker.next(current)
		if len(lines) == 0 {
			return
		}
		s.mu.Lock()
		quitting := s.quitting != nil
		s.mu.Unlock()
		select {
		case <-s.quit:
			quitting = true
		default:
		}
		if quitting {
			return
		}
		item.mu.Lock()
		title, address := item.label, item.url
		item.mu.Unlock()
		if item.Project != "" {
			title += " · " + item.Project
		}
		s.options.Notify(notice(title, address, lines))
	}
}
