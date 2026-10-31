package client

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"codeflower-app/internal/tunnel"
)

// TunnelOptions turn on remote access through a code flower tunnel; the
// connector stays idle until `codeflower tunnel login` signed this computer in.
type TunnelOptions struct {
	// Hardened runs the connector as a separate, confined process
	// (`codeflower tunnel connect`).
	Hardened bool
	// Manage lets remote browsers start sandboxes and browse folders.
	Manage bool
}

// NoticePrefix and ConnectedPrefix mark the lines a confined connector
// writes to tell the client something to show the user, and that it connected
// (with the page's address through the tunnel).
const (
	NoticePrefix    = "notice "
	ConnectedPrefix = "connected "
)

func (s *Server) runTunnel(ctx context.Context, options TunnelOptions, connected func(pageURL string)) {
	directory := s.options.ConfigDirectory
	socket := tunnel.SocketPath(directory)
	go func() {
		if err := s.serveRemote(ctx, socket, options.Manage); err != nil {
			fmt.Fprintf(s.options.Log, "tunnel: %v\n", err)
		}
	}()
	notify := func(notice tunnel.Notice) {
		fmt.Fprintf(s.options.Log, "tunnel: %s: %s\n", notice.Title, notice.Body)
		if s.options.Notify != nil {
			s.options.Notify(Notice{Title: notice.Title, Body: notice.Body})
		}
	}
	if options.Hardened {
		s.runConfinedConnector(ctx, notify, connected)
		return
	}
	_ = tunnel.Run(ctx, tunnel.Options{
		ConfigDirectory: directory,
		Version:         s.options.Version,
		DialApp:         func(ctx context.Context) (net.Conn, error) { return (&net.Dialer{}).DialContext(ctx, "unix", socket) },
		Notify:          notify,
		Log:             s.options.Log,
		Connected:       connected,
	})
}

// runConfinedConnector keeps `codeflower tunnel connect` running, passing its
// notices on, until ctx ends.
func (s *Server) runConfinedConnector(ctx context.Context, notify func(tunnel.Notice), connected func(string)) {
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintf(s.options.Log, "tunnel: cannot start the connector: %v\n", err)
		return
	}
	backoff := time.Second
	for ctx.Err() == nil {
		command := exec.CommandContext(ctx, executable, "tunnel", "connect", "--config", s.options.ConfigDirectory, "--version", s.options.Version)
		command.Stderr = s.options.Log
		output, err := command.StdoutPipe()
		if err == nil {
			err = command.Start()
		}
		if err != nil {
			fmt.Fprintf(s.options.Log, "tunnel: cannot start the connector: %v\n", err)
			return
		}
		started := time.Now()
		lines := bufio.NewScanner(output)
		for lines.Scan() {
			line := lines.Text()
			if rest, ok := strings.CutPrefix(line, ConnectedPrefix); ok {
				connected(rest)
				continue
			}
			if rest, ok := strings.CutPrefix(line, NoticePrefix); ok {
				var notice tunnel.Notice
				if json.Unmarshal([]byte(rest), &notice) == nil {
					notify(notice)
					continue
				}
			}
			fmt.Fprintln(s.options.Log, line)
		}
		_ = command.Wait()
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		fmt.Fprintf(s.options.Log, "tunnel: the connector stopped; restarting in %s\n", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

// tunnelWait is how long the client waits for the tunnel before printing its
// addresses, so they come out together.
const tunnelWait = 5 * time.Second

// startTunnel starts the connector when configured. The returned function
// waits (at most tunnelWait, and only when the computer is signed in) for its
// first connection and returns the page's address through the tunnel, or "".
// A first connection after that goes to Options.TunnelReady.
func (s *Server) startTunnel(ctx context.Context) func() string {
	if s.options.Tunnel == nil || s.options.ConfigDirectory == "" {
		return func() string { return "" }
	}
	var mu sync.Mutex
	first := make(chan string, 1)
	waited, announced := false, false
	go s.runTunnel(ctx, *s.options.Tunnel, func(pageURL string) {
		mu.Lock()
		defer mu.Unlock()
		select {
		case first <- pageURL:
		default:
		}
		if waited && !announced {
			announced = true
			if s.options.TunnelReady != nil {
				s.options.TunnelReady(pageURL)
			}
		}
	})
	return func() string {
		var pageURL string
		if tunnel.Expected(s.options.ConfigDirectory) {
			select {
			case pageURL = <-first:
			case <-time.After(tunnelWait):
			}
		}
		mu.Lock()
		defer mu.Unlock()
		waited = true
		if pageURL == "" {
			// It may have connected just now.
			select {
			case pageURL = <-first:
			default:
			}
		}
		announced = pageURL != ""
		return pageURL
	}
}
