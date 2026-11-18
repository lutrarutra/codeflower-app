package sandbox

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Forward keeps the worker's port open on another machine's loopback
// (`--forward-to HOST`) with a reverse SSH tunnel, `ssh -R`: from a compute
// node to the login node where VS Code or the client runs, when that node
// cannot reach the compute node's port (Docker and Podman publish it on the
// compute node's 127.0.0.1 only, or a firewall stands between the nodes).
// 127.0.0.1:PORT on HOST then reaches the worker, so its 127.0.0.1 links
// work there unchanged, fingerprint included.
//
// The tunnel lives as long as the sandbox: it starts once the worker
// listens, connects again when it drops, and stops with the sandbox.
type Forward struct {
	// Host is the SSH destination: a host name, user@host or a Host of the
	// user's SSH config.
	Host string
	// Port is the worker's port, opened on Host's 127.0.0.1 too.
	Port int
	// Target is the address the worker's port is open on here (ForwardTarget).
	Target string
	// Log gets what happens to the tunnel, one line each.
	Log io.Writer
	// SSH is the ssh program; "ssh" when empty. A variable for tests.
	SSH string
	// Retry is the longest wait before connecting again; a minute when zero.
	Retry time.Duration
}

// CheckForwardHost checks a --forward-to value: an SSH destination, which
// must not be read as one of ssh's options.
func CheckForwardHost(host string) error {
	if host == "" {
		return nil
	}
	if strings.HasPrefix(host, "-") || strings.ContainsFunc(host, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return fmt.Errorf("--forward-to %q is not a host name", host)
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		return fmt.Errorf("--forward-to needs ssh, which is not on this machine's PATH")
	}
	return nil
}

// ForwardTarget is the address the worker's port is open on, for the
// tunnel to connect to: Docker and Podman publish it on 127.0.0.1, an
// Apptainer worker listens on --host (all interfaces include loopback).
func ForwardTarget(engine, listenHost string) string {
	if engine != EngineApptainer {
		return "127.0.0.1"
	}
	switch listenHost {
	case "", "0.0.0.0", "::", "localhost":
		return "127.0.0.1"
	}
	return listenHost
}

// arguments are ssh's: no command, no terminal, no prompts (nobody could
// answer them in a batch job), its own connection rather than a shared
// master's, a timely end to a connection that died, and an exit when the
// port cannot be opened, rather than a tunnel that forwards nothing.
// -v prints "remote forward success" once the port is open on Host.
func (f Forward) arguments() []string {
	port := strconv.Itoa(f.Port)
	target := f.Target
	if strings.Contains(target, ":") {
		target = "[" + target + "]"
	}
	return []string{
		"-v", "-N", "-T",
		"-o", "BatchMode=yes",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
		"-o", "ControlMaster=no",
		"-o", "ControlPath=none",
		"-R", "127.0.0.1:" + port + ":" + target + ":" + port,
		"--", f.Host,
	}
}

// Start keeps the tunnel up in the background until stop is called, which
// returns once ssh has exited.
func (f Forward) Start(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.keep(ctx)
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
}

// keep runs ssh again whenever it exits, waiting longer after each failure
// in a row, up to Retry. It reports changes only: the first failure (and a
// different one later), the tunnel coming up, and it dropping.
func (f Forward) keep(ctx context.Context) {
	longest := f.Retry
	if longest <= 0 {
		longest = time.Minute
	}
	wait := longest / 12
	up, lastProblem := false, ""
	for {
		opened, problem := f.once(ctx, func() {
			if up {
				fmt.Fprintf(f.Log, "  forward  %s: open again\n", f.Host)
			} else {
				fmt.Fprintf(f.Log, "  forward  127.0.0.1:%d on %s reaches this sandbox while it runs\n", f.Port, f.Host)
			}
			up, lastProblem = true, ""
		})
		if ctx.Err() != nil {
			return
		}
		if opened {
			// It was up for a while: try again soon.
			wait = longest / 12
			fmt.Fprintf(f.Log, "  forward  %s: the tunnel closed (%s); connecting again\n", f.Host, problem)
		} else if problem != lastProblem {
			fmt.Fprintf(f.Log, "  forward  %s: could not open 127.0.0.1:%d there: %s; trying again\n", f.Host, f.Port, problem+forwardHint(problem, f.Host))
		}
		lastProblem = problem
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if !opened {
			wait = min(wait*2, longest)
		}
	}
}

// once runs ssh until it exits or ctx ends: opened says whether the port
// was opened on Host, problem what ssh said last.
func (f Forward) once(ctx context.Context, open func()) (opened bool, problem string) {
	program := f.SSH
	if program == "" {
		program = "ssh"
	}
	command := exec.Command(program, f.arguments()...)
	// Not the terminal's: its first Ctrl+C wraps the sandbox up, which
	// needs the tunnel. The app stops ssh itself.
	ownProcessGroup(command)
	dieWithParent(command)
	output, err := command.StderrPipe()
	if err != nil {
		return false, err.Error()
	}
	if err := command.Start(); err != nil {
		return false, err.Error()
	}
	exited := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = command.Process.Kill()
		case <-exited:
		}
	}()
	var said []string
	scanner := bufio.NewScanner(output)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case strings.Contains(line, "remote forward success"):
			if !opened {
				opened = true
				open()
			}
		case line == "", strings.HasPrefix(line, "debug"), strings.HasPrefix(line, "OpenSSH_"),
			strings.HasPrefix(line, "Authenticated to"), strings.HasPrefix(line, "Warning: Permanently added"),
			strings.HasPrefix(line, "Transferred:"), strings.HasPrefix(line, "Bytes per second"):
			// -v's chatter.
		default:
			said = append(said, line)
			if len(said) > 3 {
				said = said[1:]
			}
		}
	}
	err = command.Wait()
	close(exited)
	if len(said) > 0 {
		return opened, strings.Join(said, " ")
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return opened, fmt.Sprintf("ssh exited with %d", exit.ExitCode())
	}
	if err != nil {
		return opened, err.Error()
	}
	return opened, "ssh exited"
}

// forwardHint says what to do about ssh's most common complaints.
func forwardHint(problem, host string) string {
	switch {
	case strings.Contains(problem, "Host key verification failed"):
		return fmt.Sprintf(" (ssh does not know %s's host key yet: run `ssh %s true` once from this node and accept it)", host, host)
	case strings.Contains(problem, "Permission denied"):
		return fmt.Sprintf(" (this node needs to log in to %s without a password: an SSH key in ~/.ssh/authorized_keys)", host)
	case strings.Contains(problem, "remote port forwarding failed"):
		return fmt.Sprintf(" (the port is taken on %s, or its sshd allows no forwarding: start with another --port)", host)
	}
	return ""
}
