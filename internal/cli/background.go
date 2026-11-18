package cli

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"codeflower-app/internal/client"
	"codeflower-app/internal/sandbox"
	"codeflower-app/internal/tunnel"
)

// backgroundWait is how long a client started in the background has to come
// up; it waits for the tunnel (at most 5 s) before it answers.
const backgroundWait = 20 * time.Second

// ensureClient makes sure a client runs for the config directory, starting
// `codeflower client --background` when none does. That client keeps the
// tunnel connected and lists every sandbox on this computer, whoever started
// it. It says whether it started one.
func ensureClient(directory string) (bool, error) {
	if _, running := client.Running(directory); running {
		return false, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return false, err
	}
	command := exec.Command(executable, "client", "--background", "--config", directory)
	detach(command)
	if err := command.Start(); err != nil {
		return false, err
	}
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	deadline := time.After(backgroundWait)
	for {
		if _, running := client.Running(directory); running {
			return true, nil
		}
		select {
		case err := <-exited:
			if err == nil {
				err = errors.New("it exited")
			}
			return false, fmt.Errorf("the client did not start (%v); see %s", err, clientLogPath(directory))
		case <-deadline:
			return false, fmt.Errorf("the client did not start within %s; see %s", backgroundWait, clientLogPath(directory))
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// linkOptions say how a sandbox is reached, for openLinks.
type linkOptions struct {
	// tunnel: paired browsers may reach it (no --no-tunnel).
	tunnel bool
	// local: VS Code on this computer reaches it on 127.0.0.1
	// (reachableOnLoopback).
	local bool
	// page: the worker serves its chat page (--server).
	page bool
	// forwardTo is the SSH host whose 127.0.0.1 reaches the worker
	// (--forward-to); empty for none.
	forwardTo string
}

// openLinks returns what to call once the sandbox that `run` or `sandbox`
// starts is ready: it prints where to open it, one link a row (the worker
// then prints only that it is ready: Worker.AppPrintsLinks).
//
// When this computer is signed in to the tunnel with remote access on, it
// first starts the client in the background, so paired browsers reach the
// sandbox, and prints a QR code for phones above the links. With the config
// directory shared between computers (a cluster's home), it takes the tunnel
// over from a computer whose hold was released or lapsed, and says so when
// another still holds it.
func openLinks(directory string, out io.Writer, options linkOptions) func(sandbox.Connection) {
	var state *tunnel.State
	var tunnelNote string
	started := make(chan error, 1)
	started <- nil
	if options.tunnel {
		loaded, err := tunnel.LoadState(directory)
		switch {
		case err != nil || loaded.Off:
		case !loaded.MayConnect(time.Now()):
			tunnelNote = fmt.Sprintf("Paired browsers cannot reach this sandbox: %v.", &tunnel.ErrHeld{Host: loaded.Host})
		default:
			state = loaded
			<-started
			go func() {
				_, err := ensureClient(directory)
				started <- err
			}()
		}
	}
	return func(connection sandbox.Connection) {
		if err := <-started; err != nil {
			tunnelNote = fmt.Sprintf("Paired browsers cannot reach this sandbox: %v", err)
			state = nil
		}
		// After the worker's ready line, which reaches the terminal through the engine.
		time.Sleep(500 * time.Millisecond)
		route := "#/chat/" + client.LocalID(connection.ID)
		links := sandboxLinks{connection: connection, options: options}
		if page, running := client.Running(directory); running {
			links.localPage = page + route
		}
		if state != nil {
			links.remote = tunnel.PageURL(state) + route
			links.paired = len(state.Devices) > 0
		}
		printOpenLinks(out, links)
		if tunnelNote != "" {
			fmt.Fprintf(out, "%s\n\n", tunnelNote)
		}
	}
}

// sandboxLinks are where a ready sandbox opens.
type sandboxLinks struct {
	connection sandbox.Connection
	options    linkOptions
	// remote is its page through the tunnel; empty without the tunnel.
	remote string
	paired bool
	// localPage is its page in the client running on this computer.
	localPage string
}

// printOpenLinks prints where to open a sandbox: the QR code of its tunnel
// link first, then every link as a table (as the bridge prints its own when
// it runs without the app: readyMessage in its server.js).
func printOpenLinks(out io.Writer, links sandboxLinks) {
	connection := links.connection
	fmt.Fprintln(out)
	var rows [][2]string
	if links.remote != "" {
		printQR(out, links.remote)
		fmt.Fprintf(out, "Scan the code with a phone, or open a link:\n\n")
		rows = append(rows, [2]string{"Paired browsers", links.remote})
	} else {
		fmt.Fprintf(out, "Open the sandbox:\n\n")
	}
	// The line on the forwarding host's 127.0.0.1 (--forward-to).
	forwarded, host := "", links.options.forwardTo
	if host != "" {
		forwarded = localLine(connection, true)
	}
	switch {
	case links.options.page:
		rows = append(rows, [2]string{"Browser", pageLink(connection)})
		if forwarded != "" && !isLoopback(connection.Host) {
			onHost := connection
			onHost.Host = "127.0.0.1"
			rows = append(rows, [2]string{"Browser on " + host, pageLink(onHost)})
		}
	case links.localPage != "":
		rows = append(rows, [2]string{"This computer", links.localPage})
	default:
		line := connection.URL
		if forwarded != "" {
			line = forwarded
		}
		rows = append(rows, [2]string{"Browser (app running)", fmt.Sprintf("http://127.0.0.1:%d/#add=%s", client.DefaultPort, url.QueryEscape(line))})
	}
	here := localLine(connection, links.options.local)
	switch {
	case links.options.local && forwarded != "":
		rows = append(rows, [2]string{"VS Code here or on " + host, client.VSCodeLink(here)})
	case links.options.local:
		rows = append(rows, [2]string{"VS Code here", client.VSCodeLink(here)})
	case forwarded != "":
		rows = append(rows, [2]string{"VS Code on " + host, client.VSCodeLink(forwarded)})
	}
	label := "VS Code (paste)"
	if links.options.local && here != connection.URL {
		label = "VS Code elsewhere"
	}
	rows = append(rows, [2]string{label, connection.URL})
	fmt.Fprintf(out, "%s\n\n", linkRows(rows))
	fmt.Fprintln(out, `In VS Code, Ctrl+click a link, or run "code flower: Connect to worker" and paste the codeflower:// line.`)
	if links.options.page {
		fmt.Fprintln(out, "The page is plain HTTP: its traffic and the key in the link are not encrypted. On a shared\n"+
			"network, listen on 127.0.0.1 (--host 127.0.0.1) behind a TLS reverse proxy such as nginx,\n"+
			"or reach it through ssh -L.")
	}
	fmt.Fprintln(out, "Anyone with these links can control the agents. Keep job logs private,\n"+
		"e.g. sbatch --output=$HOME/.codeflower-logs/%j.out with that directory chmod 700.")
	if links.remote != "" && !links.paired {
		fmt.Fprintln(out, `No browser is paired yet: run "codeflower tunnel pair".`)
	}
	fmt.Fprintln(out)
}

// pageLink is the chat page a worker serves with --server, with its key
// (pageLink in the bridge's server.js).
func pageLink(connection sandbox.Connection) string {
	return "http://" + net.JoinHostPort(connection.Host, strconv.Itoa(connection.Port)) + "/?key=" + url.QueryEscape(connection.Token)
}

// linkRows is links as a table: each label padded to the longest, then its
// link, alone up to the line's end so a terminal's Ctrl+click takes it all.
func linkRows(rows [][2]string) string {
	width := 0
	for _, row := range rows {
		width = max(width, len(row[0]))
	}
	lines := make([]string, len(rows))
	for index, row := range rows {
		lines[index] = fmt.Sprintf("  %-*s   %s", width, row[0], row[1])
	}
	return strings.Join(lines, "\n")
}

// reachableOnLoopback says whether VS Code on this computer reaches the
// worker on 127.0.0.1, as the bridge decides for its own VS Code link:
// Docker and Podman publish the port there, and Apptainer's worker listens on
// all interfaces or loopback. An isolated sandbox's port is the app's, which
// the bridge cannot see, so it keeps the advertised address; so does this.
func reachableOnLoopback(engine, listenHost string, isolated bool) bool {
	if isolated {
		return false
	}
	if engine != sandbox.EngineApptainer {
		return true
	}
	switch listenHost {
	case "", "0.0.0.0", "::", "127.0.0.1", "localhost":
		return true
	}
	return false
}

// localLine is the worker's codeflower:// line for VS Code on this computer:
// on 127.0.0.1 when it is reachable there, else the advertised one.
func localLine(connection sandbox.Connection, local bool) string {
	if !local || connection.Port == 0 {
		return connection.URL
	}
	if isLoopback(connection.Host) {
		return connection.URL
	}
	line := "codeflower://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(connection.Port)) + "?token=" + url.QueryEscape(connection.Token)
	if connection.Fingerprint != "" {
		line += "&fp=" + connection.Fingerprint
	}
	return line
}

// isLoopback says whether host names this computer's loopback.
func isLoopback(host string) bool {
	switch host {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

// quitWhenSignedOut quits a background client once this computer is signed
// out of the tunnel: it ran for the tunnel only.
func quitWhenSignedOut(control *client.Control, directory string) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if _, err := tunnel.LoadState(directory); errors.Is(err, tunnel.ErrNotSignedIn) {
			control.Quit(true)
			return
		}
	}
}
