package attach

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// readLine reads one line the user typed (the terminal is in its usual mode,
// which echoes it).
func readLine(input <-chan []byte) (string, error) {
	var line []byte
	for data := range input {
		line = append(line, data...)
		if index := strings.IndexAny(string(line), "\r\n"); index >= 0 {
			return strings.TrimSpace(string(line[:index])), nil
		}
	}
	return "", io.EOF
}

func pickTarget(out io.Writer, input <-chan []byte, targets []Target) (Target, error) {
	fmt.Fprintln(out, "Sandboxes:")
	for index, target := range targets {
		fmt.Fprintf(out, "  %2d  %-24s %-6s %s\n", index+1, target.Label(), target.Source, target.ID)
	}
	fmt.Fprint(out, "Choose a sandbox (number): ")
	answer, err := readLine(input)
	if err != nil {
		return Target{}, err
	}
	return ResolveTarget(answer, targets)
}

// newChat is the terminal/start of a new chat: with agent, or with the one
// agent that has a terminal UI, or else the one the user picks.
func newChat(out io.Writer, input <-chan []byte, bridge status, agent string) (map[string]any, error) {
	agents := bridge.terminalAgents()
	if agent != "" {
		for _, candidate := range agents {
			if candidate.ID == agent {
				return map[string]any{"kind": "chat", "new": true, "agent": agent}, nil
			}
		}
		return nil, fmt.Errorf("%s has no terminal UI in this sandbox", bridge.agent(agent).Name)
	}
	switch len(agents) {
	case 0:
		return nil, errors.New("no agent of this sandbox has a terminal UI in code flower")
	case 1:
		return map[string]any{"kind": "chat", "new": true, "agent": agents[0].ID}, nil
	}
	fmt.Fprintln(out, "New chat with:")
	for index, candidate := range agents {
		fmt.Fprintf(out, "  %2d  %s\n", index+1, candidate.Name)
	}
	fmt.Fprint(out, "Which agent (number): ")
	for {
		answer, err := readLine(input)
		if err != nil {
			return nil, err
		}
		if number, err := strconv.Atoi(answer); err == nil && number >= 1 && number <= len(agents) {
			return map[string]any{"kind": "chat", "new": true, "agent": agents[number-1].ID}, nil
		}
		for _, candidate := range agents {
			if strings.EqualFold(answer, candidate.ID) {
				return map[string]any{"kind": "chat", "new": true, "agent": candidate.ID}, nil
			}
		}
		fmt.Fprint(out, "Which agent (number): ")
	}
}

// pickChat lists the sandbox's chats and asks which to open. It returns the
// terminal/start parameters, or nil when the user goes back (Enter, or Esc).
// current: the chat the user comes from, if any.
func pickChat(ctx context.Context, c *conn, out io.Writer, input <-chan []byte, target Target, bridge status, current string) (map[string]any, error) {
	list, err := chats(ctx, c)
	if err != nil {
		return nil, err
	}
	fmt.Fprint(out, clearScreen)
	fmt.Fprintf(out, "%s · chats\n\n", target.Label())
	PrintChats(out, list, current)
	fmt.Fprintf(out, "   n  + New chat\n\n")
	prompt := "Open which chat (number, n for a new one"
	if current != "" {
		prompt += ", Enter to go back"
	}
	fmt.Fprint(out, prompt+"): ")
	for {
		answer, err := readLine(input)
		if err != nil {
			return nil, err
		}
		switch {
		case answer == "" || answer == "\x1b":
			if current != "" {
				return nil, nil
			}
			if len(list) == 0 {
				return newChat(out, input, bridge, "")
			}
			return nil, errors.New("no chat chosen")
		case strings.EqualFold(answer, "n"):
			return newChat(out, input, bridge, "")
		}
		chat, err := ResolveChat(answer, list)
		if err == nil {
			return map[string]any{"kind": "chat", "sessionKey": chat.Key}, nil
		}
		fmt.Fprintf(out, "%v\nOpen which chat: ", err)
	}
}

// PrintChats lists chats, numbered for ResolveChat: their title, when they
// last changed, what they do, and ● when their terminal UI runs.
func PrintChats(out io.Writer, list []Chat, current string) {
	if len(list) == 0 {
		fmt.Fprintln(out, "   (no chats yet)")
	}
	for index, chat := range list {
		mark := " "
		if chat.Key == current {
			mark = ">"
		}
		running := ""
		if chat.Terminal != nil {
			running = "● terminal"
		}
		fmt.Fprintf(out, " %s%2d  %-44s %-9s %-10s %-8s %s\n", mark, index+1, truncate(chat.Title, 44), chat.Harness, ago(chat.UpdatedAt), chat.State, running)
	}
}

func truncate(text string, width int) string {
	runes := []rune(text)
	if len(runes) <= width {
		return text
	}
	return string(runes[:width-1]) + "…"
}

func ago(timestamp string) string {
	at, err := time.Parse(time.RFC3339, timestamp)
	if err != nil {
		return ""
	}
	elapsed := time.Since(at)
	switch {
	case elapsed < time.Minute:
		return "just now"
	case elapsed < time.Hour:
		return strconv.Itoa(int(elapsed.Minutes())) + " min ago"
	case elapsed < 24*time.Hour:
		return strconv.Itoa(int(elapsed.Hours())) + " h ago"
	}
	return strconv.Itoa(int(elapsed.Hours()/24)) + " d ago"
}
