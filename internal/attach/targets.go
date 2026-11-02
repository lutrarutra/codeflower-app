package attach

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"codeflower-app/internal/client"
	"codeflower-app/internal/sandbox"
)

// Target is a sandbox to attach to: one running on this computer (its run
// registered it) or one the client remembers (Add remote sandbox).
type Target struct {
	// ID is how the app's page names it: local-1a2b3c4d, or the added one's id.
	ID      string
	Name    string
	Agent   string
	Source  string // local or added
	Started time.Time
	// RunID is a local sandbox's full run id.
	RunID string
	// NoTunnel keeps a local sandbox from browsers paired through the tunnel.
	NoTunnel   bool
	Connection sandbox.Connection
}

func (t Target) Label() string {
	if t.Name != "" {
		return t.Name
	}
	return t.ID
}

// Targets lists the sandboxes this computer knows that have a chat: those it
// runs, newest first, then those the client remembers.
func Targets() []Target {
	var local []Target
	for _, running := range sandbox.ListRunning() {
		if running.Kind == "task" {
			continue
		}
		connection, ok := running.ReadConnection()
		if !ok {
			continue
		}
		local = append(local, Target{
			ID:         client.LocalID(running.ID),
			Name:       projectName(running),
			Agent:      running.Agent,
			Source:     "local",
			Started:    running.Started,
			RunID:      running.ID,
			NoTunnel:   running.NoTunnel,
			Connection: connection,
		})
	}
	sort.SliceStable(local, func(i, j int) bool { return local[i].Started.After(local[j].Started) })
	targets := local
	for _, saved := range client.SavedWorkers() {
		targets = append(targets, Target{ID: saved.ID, Name: saved.Name, Agent: saved.Agent, Source: "added", Connection: saved.Connection})
	}
	return targets
}

func projectName(running sandbox.Running) string {
	if running.Project != "" {
		return running.Project
	}
	if running.State != "" {
		return filepath.Base(running.State)
	}
	return ""
}

// ResolveTarget finds the sandbox an argument names: a codeflower:// line, a
// page id (local-1a2b3c4d), a run id or its start, a project or sandbox name,
// or a number from the list. With no argument and one sandbox, that one.
func ResolveTarget(argument string, targets []Target) (Target, error) {
	argument = strings.TrimSpace(argument)
	if strings.Contains(argument, "://") {
		connection, err := client.ParseAddress(argument)
		if err != nil {
			return Target{}, err
		}
		return Target{ID: connection.Host, Name: hostPort(connection), Source: "line", Connection: connection}, nil
	}
	if argument == "" {
		switch len(targets) {
		case 0:
			return Target{}, errors.New("no sandbox runs on this computer: start one with `codeflower interactive`, or pass its codeflower:// line")
		case 1:
			return targets[0], nil
		}
		return Target{}, errAmbiguous
	}
	if index, err := strconv.Atoi(argument); err == nil && index >= 1 && index <= len(targets) {
		return targets[index-1], nil
	}
	var matches []Target
	for _, target := range targets {
		if target.ID == argument || target.RunID == argument || strings.EqualFold(target.Name, argument) {
			return target, nil
		}
		if target.RunID != "" && strings.HasPrefix(target.RunID, argument) {
			matches = append(matches, target)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return Target{}, fmt.Errorf("%q names more than one sandbox: give more of its id", argument)
	}
	return Target{}, fmt.Errorf("no sandbox %q: `codeflower chats` lists them", argument)
}

func hostPort(connection sandbox.Connection) string {
	return fmt.Sprintf("%s:%d", connection.Host, connection.Port)
}

// errAmbiguous asks for a choice among several sandboxes.
var errAmbiguous = errors.New("several sandboxes run: name one")

// Chat is a chat of a sandbox, as `_codeflower/sessions/list` lists it.
type Chat struct {
	Key       string    `json:"key"`
	Title     string    `json:"title"`
	UpdatedAt string    `json:"updatedAt"`
	Terminal  *Terminal `json:"terminal,omitempty"`
	// State: idle, working or waiting (from `_codeflower/activity`).
	State string `json:"state,omitempty"`
}

// Terminal is a chat's terminal UI, running in the sandbox.
type Terminal struct {
	TerminalID string `json:"terminalId"`
	SessionKey string `json:"sessionKey"`
	SessionID  string `json:"sessionId"`
	Viewers    int    `json:"viewers"`
	StartedAt  string `json:"startedAt"`
}

// ResolveChat finds the chat an argument names: its key, a number from the
// list, or the start of its title when only one chat's does.
func ResolveChat(argument string, chats []Chat) (Chat, error) {
	argument = strings.TrimSpace(argument)
	if index, err := strconv.Atoi(argument); err == nil && index >= 1 && index <= len(chats) {
		return chats[index-1], nil
	}
	var matches []Chat
	for _, chat := range chats {
		if chat.Key == argument || chat.Title == argument {
			return chat, nil
		}
		if strings.HasPrefix(strings.ToLower(chat.Title), strings.ToLower(argument)) {
			matches = append(matches, chat)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return Chat{}, fmt.Errorf("no chat %q: `codeflower chats` lists them", argument)
	}
	return Chat{}, fmt.Errorf("%q begins the titles of %d chats: give more of it, or the chat's number", argument, len(matches))
}

// status is what the bridge says of itself (`_codeflower/status`).
type status struct {
	Agent        string `json:"agent"`
	AgentLabel   string `json:"agentLabel"`
	SignedIn     bool   `json:"signedIn"`
	Capabilities struct {
		ChatTerminals bool `json:"chatTerminals"`
	} `json:"capabilities"`
}

type activity struct {
	Chats []struct {
		Key   string `json:"key"`
		State string `json:"state"`
	} `json:"chats"`
}

// chats lists a sandbox's chats, newest first, with what each is doing.
func chats(ctx context.Context, c *conn) ([]Chat, error) {
	var listed struct {
		Sessions []Chat `json:"sessions"`
	}
	if err := c.call(ctx, "_codeflower/sessions/list", map[string]any{}, &listed); err != nil {
		return nil, err
	}
	var now activity
	_ = c.call(ctx, "_codeflower/activity", map[string]any{}, &now)
	states := map[string]string{}
	for _, chat := range now.Chats {
		states[chat.Key] = chat.State
	}
	for index := range listed.Sessions {
		listed.Sessions[index].State = states[listed.Sessions[index].Key]
		if listed.Sessions[index].State == "" {
			listed.Sessions[index].State = "idle"
		}
	}
	return listed.Sessions, nil
}
