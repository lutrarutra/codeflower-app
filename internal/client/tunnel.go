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

// NoticePrefix marks the lines a confined connector writes to tell the
// client something to show the user.
const NoticePrefix = "notice "

func (s *Server) runTunnel(ctx context.Context, options TunnelOptions) {
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
		s.runConfinedConnector(ctx, notify)
		return
	}
	_ = tunnel.Run(ctx, tunnel.Options{
		ConfigDirectory: directory,
		Version:         s.options.Version,
		DialApp:         func(ctx context.Context) (net.Conn, error) { return (&net.Dialer{}).DialContext(ctx, "unix", socket) },
		Notify:          notify,
		Log:             s.options.Log,
	})
}

// runConfinedConnector keeps `codeflower tunnel connect` running, passing its
// notices on, until ctx ends.
func (s *Server) runConfinedConnector(ctx context.Context, notify func(tunnel.Notice)) {
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
