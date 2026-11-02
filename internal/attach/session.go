package attach

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/moby/term"
)

// Options are what `codeflower attach` was asked for.
type Options struct {
	Sandbox string
	Chat    string
	New     bool
	Last    bool
	// Watch only: what is typed is not sent, and the size stays the typist's.
	Watch bool
	// Links: how the files the agent shows are linked (LinksAuto, …).
	Links string
	// Config is the config directory, whose running client's page the links open.
	Config string
	In     *os.File
	Out    *os.File
	Err    io.Writer
}

// How the user's terminal is left when a TUI that took it over is left:
// the main screen, a cursor, no mouse or bracketed paste or keyboard modes,
// plain colors.
const resetTerminal = "\x1b[?1049l\x1b[?25h\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?2004l\x1b[<u\x1b[0m\r\n"

const clearScreen = "\x1b[2J\x1b[H"

// Help for the escapes, which work after Enter, as ssh's do.
const escapeHelp = `code flower escapes (type them after Enter):
  ~.  detach: the terminal UI keeps running in the sandbox
  ~s  switch to another chat of the sandbox
  ~q  quit the terminal UI
  ~l  links: the chat in your browser, and the files the agent showed
  ~o  open the file the agent showed last in your browser
  ~?  this help
  ~~  type a ~`

// How long the rail shows a file the agent showed.
const railTime = 8 * time.Second

// Run attaches the user's terminal to a chat's terminal UI until they detach
// or quit it.
func Run(ctx context.Context, options Options) error {
	if !term.IsTerminal(options.In.Fd()) || !term.IsTerminal(options.Out.Fd()) {
		return errors.New("attach needs a terminal: run it in one, or use `codeflower chats` in scripts")
	}
	input := readInput(options.In)
	targets := Targets()
	target, err := ResolveTarget(options.Sandbox, targets)
	if errors.Is(err, errAmbiguous) {
		target, err = pickTarget(options.Out, input, targets)
	}
	if err != nil {
		return err
	}
	c, err := dial(ctx, target.Connection)
	if err != nil {
		return err
	}
	defer c.close()
	var bridge status
	if err := c.call(ctx, "_codeflower/status", map[string]any{}, &bridge); err != nil {
		return err
	}
	label := bridge.AgentLabel
	if !bridge.Capabilities.ChatTerminals {
		return fmt.Errorf("%s in %s has no terminal UI in code flower (Claude Code, Codex and OpenCode have), or its image is older than `codeflower attach`", label, target.Label())
	}
	if !bridge.SignedIn {
		fmt.Fprintf(options.Err, "%s is not signed in in %s: sign in from its chat (Sign in…) first.\n", label, target.Label())
	}

	start := map[string]any{"kind": "chat"}
	switch {
	case options.New:
		start["new"] = true
	case options.Chat != "" || options.Last:
		list, err := chats(ctx, c)
		if err != nil {
			return err
		}
		if options.Last {
			if len(list) == 0 {
				start["new"] = true
				break
			}
			start["sessionKey"] = list[0].Key
			break
		}
		chat, err := ResolveChat(options.Chat, list)
		if err != nil {
			return err
		}
		start["sessionKey"] = chat.Key
	default:
		chosen, err := pickChat(ctx, c, options.Out, input, target, label, "")
		if err != nil {
			return err
		}
		if chosen == nil {
			return nil
		}
		start = chosen
	}

	page := findPage(options.Links, options.Config, target)
	if page.note != "" {
		fmt.Fprintln(options.Err, page.note)
	}
	session := &session{conn: c, options: options, input: input, target: target, label: label, page: page, handsOver: bridge.Agent == "claude"}
	return session.run(ctx, start)
}

type session struct {
	conn    *conn
	options Options
	input   <-chan []byte
	target  Target
	label   string
	page    pageLinks
	// The agent's terminal UI takes the chat over (Claude Code) instead of sharing it.
	handsOver bool
	// The files the agent showed, newest last, with their links.
	shown []shownFile
}

type shownFile struct {
	Path string `json:"path"`
	URL  string `json:"url"`
}

// What ended a chat's view.
type ending int

const (
	ended ending = iota
	detached
	switching
)

func (s *session) run(ctx context.Context, start map[string]any) error {
	for {
		started, how, err := s.attach(ctx, start)
		if err != nil {
			return err
		}
		switch how {
		case detached:
			fmt.Fprintf(s.options.Err, "Detached. The terminal UI keeps running; `codeflower attach %s %s` returns to it.\n", s.target.ID, started.SessionKey)
			return nil
		case ended:
			return nil
		}
		chosen, err := pickChat(ctx, s.conn, s.options.Out, s.input, s.target, s.label, started.SessionKey)
		if err != nil {
			return err
		}
		if chosen == nil {
			// Back to the same chat.
			chosen = map[string]any{"kind": "chat", "sessionKey": started.SessionKey}
		}
		start = chosen
	}
}

// attach shows one chat's terminal until it ends, the user detaches, or
// switches.
func (s *session) attach(ctx context.Context, start map[string]any) (Terminal, ending, error) {
	out := s.options.Out
	size := windowSize(out)
	start["columns"], start["rows"] = size.columns, size.rows
	if s.options.Watch {
		start["watch"] = true
	}
	if base := s.page.fileBase(); base != "" {
		start["linkBase"] = base
	}
	var started Terminal
	fmt.Fprintf(out, "%s · %s · opening the terminal UI… (after Enter: ~. detach, ~s switch chat, ~? help)\r\n", s.target.Label(), s.label)
	if s.handsOver {
		fmt.Fprint(out, "It takes the chat over once the chat's current turn, if any, has ended.\r\n")
	}
	if err := s.conn.call(ctx, "_codeflower/terminal/start", start, &started); err != nil {
		return started, ended, err
	}

	state, err := term.MakeRaw(s.options.In.Fd())
	if err != nil {
		return started, ended, err
	}
	restore := func() { _ = term.RestoreTerminal(s.options.In.Fd(), state) }
	defer restore()
	fmt.Fprint(out, clearScreen)

	resizes, stopResizes := watchSize(out)
	defer stopResizes()
	escapes := &escaper{atLineStart: true}
	var railClear <-chan time.Time
	for {
		select {
		case <-railClear:
			railClear = nil
			s.clearRail(ctx, started)
		case <-ctx.Done():
			s.leave(started)
			return started, detached, nil
		case notification, ok := <-s.conn.notifications:
			if !ok {
				fmt.Fprint(out, resetTerminal)
				return started, ended, errors.New("the connection to the sandbox ended; `codeflower attach` again to return")
			}
			switch notification.Method {
			case "_codeflower/terminal/output":
				var output struct {
					TerminalID string `json:"terminalId"`
					Data       string `json:"data"`
				}
				if json.Unmarshal(notification.Params, &output) == nil && output.TerminalID == started.TerminalID {
					_, _ = io.WriteString(out, output.Data)
				}
			case "_codeflower/terminal/exit":
				var exit struct {
					TerminalID string `json:"terminalId"`
					ExitCode   int    `json:"exitCode"`
				}
				if json.Unmarshal(notification.Params, &exit) == nil && exit.TerminalID == started.TerminalID {
					fmt.Fprint(out, resetTerminal)
					restore()
					fmt.Fprintf(s.options.Err, "The terminal UI ended (%d).\n", exit.ExitCode)
					return started, ended, nil
				}
			case "_codeflower/terminal/left":
				var left struct {
					TerminalID string `json:"terminalId"`
				}
				if json.Unmarshal(notification.Params, &left) == nil && left.TerminalID == started.TerminalID {
					fmt.Fprintf(out, "\x1b7\x1b[%d;1H\x1b[2K\x1b[7m this terminal left its chat, which went back to the page; ~s switches chats \x1b[0m\x1b8", windowSize(out).rows)
					railClear = time.After(railTime)
				}
			case "_codeflower/terminal/show":
				var shown struct {
					TerminalID string      `json:"terminalId"`
					Files      []shownFile `json:"files"`
				}
				if json.Unmarshal(notification.Params, &shown) == nil && shown.TerminalID == started.TerminalID && len(shown.Files) > 0 {
					s.shown = append(s.shown, shown.Files...)
					if len(s.shown) > maxShown {
						s.shown = s.shown[len(s.shown)-maxShown:]
					}
					s.drawRail(shown.Files)
					railClear = time.After(railTime)
				}
			}
		case size := <-resizes:
			_ = s.conn.call(ctx, "_codeflower/terminal/resize", map[string]any{"terminalId": started.TerminalID, "columns": size.columns, "rows": size.rows}, nil)
		case data, ok := <-s.input:
			if !ok {
				s.leave(started)
				return started, detached, nil
			}
			typed, action := escapes.feed(data)
			if len(typed) > 0 && !s.options.Watch {
				if err := s.conn.call(ctx, "_codeflower/terminal/input", map[string]any{"terminalId": started.TerminalID, "data": string(typed)}, nil); err != nil {
					return started, ended, err
				}
			}
			switch action {
			case '.':
				s.leave(started)
				return started, detached, nil
			case 's':
				s.leave(started)
				return started, switching, nil
			case 'q':
				_ = s.conn.call(ctx, "_codeflower/terminal/close", map[string]any{"terminalId": started.TerminalID}, nil)
			case '?':
				fmt.Fprint(out, "\r\n"+strings.ReplaceAll(escapeHelp, "\n", "\r\n")+"\r\n")
			case 'l':
				s.printLinks(started)
			case 'o':
				if len(s.shown) == 0 {
					fmt.Fprint(out, "\r\nThe agent showed no file yet.\r\n")
				} else if err := openBrowser(s.shown[len(s.shown)-1].URL); err != nil {
					fmt.Fprintf(out, "\r\n%v\r\n", err)
				}
			}
		}
	}
}

// How many of the files the agent showed ~l lists.
const maxShown = 20

// drawRail shows the files the agent showed on the terminal's last row, as
// links, until the program draws over it or railTime passes.
func (s *session) drawRail(files []shownFile) {
	rows := windowSize(s.options.Out).rows
	var line strings.Builder
	line.WriteString("\x1b[7m ▸ ")
	for index, file := range files {
		if index > 0 {
			line.WriteString("  ")
		}
		line.WriteString(hyperlink(file.URL, filepath.Base(file.Path)))
	}
	line.WriteString("  open ↗ (~o, ~l) \x1b[0m")
	fmt.Fprintf(s.options.Out, "\x1b7\x1b[%d;1H\x1b[2K%s\x1b8", rows, line.String())
}

// clearRail gives the last row back: the program draws itself again.
func (s *session) clearRail(ctx context.Context, started Terminal) {
	size := windowSize(s.options.Out)
	fmt.Fprintf(s.options.Out, "\x1b7\x1b[%d;1H\x1b[2K\x1b8", size.rows)
	if s.options.Watch {
		return
	}
	nudge := map[string]any{"terminalId": started.TerminalID, "columns": size.columns, "rows": max(size.rows-1, 5)}
	_ = s.conn.call(ctx, "_codeflower/terminal/resize", nudge, nil)
	nudge["rows"] = size.rows
	_ = s.conn.call(ctx, "_codeflower/terminal/resize", nudge, nil)
}

// printLinks prints the chat's link and the files the agent showed.
func (s *session) printLinks(started Terminal) {
	out := s.options.Out
	if s.page.base == "" {
		fmt.Fprint(out, "\r\nNo links: "+strings.TrimPrefix(s.page.note, "files show as paths: ")+"\r\n")
	} else {
		fmt.Fprintf(out, "\r\nThis chat: %s\r\n", hyperlink(s.page.chat(started.SessionKey), s.page.chat(started.SessionKey)))
		if s.page.keyed != "" {
			fmt.Fprintf(out, "First time in this browser? Open %s\r\n", hyperlink(s.page.keyed, "the page with its key"))
		}
	}
	for _, file := range s.shown {
		fmt.Fprintf(out, "  %s  %s\r\n", file.Path, hyperlink(file.URL, file.URL))
	}
}

// leave stops watching a chat's terminal, which keeps running.
func (s *session) leave(started Terminal) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.conn.call(ctx, "_codeflower/terminal/detach", map[string]any{"terminalId": started.TerminalID}, nil)
	fmt.Fprint(s.options.Out, resetTerminal)
}

// escaper finds the escapes in what the user types: `~` and a letter after
// Enter (or at the start), as ssh's.
type escaper struct {
	atLineStart bool
	pending     bool
}

// feed returns what to send on, and the escape's letter if one was typed;
// anything typed after an escape in the same read is dropped.
func (e *escaper) feed(data []byte) ([]byte, byte) {
	var typed []byte
	for _, b := range data {
		if e.pending {
			e.pending = false
			switch b {
			case '.', 's', 'q', '?', 'l', 'o':
				e.atLineStart = b == '?' || b == 'l' || b == 'o'
				return typed, b
			case '~':
				typed = append(typed, '~')
				e.atLineStart = false
				continue
			}
			typed = append(typed, '~')
		} else if b == '~' && e.atLineStart {
			e.pending = true
			continue
		}
		typed = append(typed, b)
		e.atLineStart = b == '\r' || b == '\n'
	}
	return typed, 0
}

// readInput reads what the user types, for the whole run: the views and the
// pickers between them take turns.
func readInput(in *os.File) <-chan []byte {
	typed := make(chan []byte, 16)
	go func() {
		defer close(typed)
		buffer := make([]byte, 4096)
		for {
			n, err := in.Read(buffer)
			if n > 0 {
				typed <- append([]byte(nil), buffer[:n]...)
			}
			if err != nil {
				return
			}
		}
	}()
	return typed
}

type size struct{ columns, rows int }

func windowSize(out *os.File) size {
	winsize, err := term.GetWinsize(out.Fd())
	if err != nil || winsize.Width == 0 || winsize.Height == 0 {
		return size{columns: 100, rows: 30}
	}
	return size{columns: int(winsize.Width), rows: int(winsize.Height)}
}
