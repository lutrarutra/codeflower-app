//go:build !windows

package sandbox

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// output collects a command's output, which runUntilDone also reports to.
type output struct {
	mu    sync.Mutex
	text  bytes.Buffer
	ready chan struct{}
	once  sync.Once
}

func (o *output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.text.Write(p)
	if strings.Contains(o.text.String(), "ready") {
		o.once.Do(func() { close(o.ready) })
	}
	return len(p), nil
}

func (o *output) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.text.String()
}

// runFake runs a shell script as the sandbox, as RunApptainer runs Apptainer,
// cancels ctx once it is ready and closes kill if asked, and returns its exit
// code, output and how long the stop took.
func runFake(t *testing.T, script string, timeout time.Duration, killNow bool) (int, string, time.Duration) {
	t.Helper()
	previous := killTimeout
	killTimeout = timeout
	t.Cleanup(func() { killTimeout = previous })

	out := &output{ready: make(chan struct{})}
	command := exec.Command("sh", "-c", script)
	command.Stdout, command.Stderr = out, out
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	kill := make(chan struct{})
	var stopped time.Time
	go func() {
		select {
		case <-out.ready:
		case <-time.After(10 * time.Second):
		}
		stopped = time.Now()
		cancel()
		if killNow {
			close(kill)
		}
	}()
	code, err := runUntilDone(ctx, kill, command,
		func() error { return command.Process.Signal(syscall.SIGTERM) },
		func() error { return command.Process.Kill() })
	if err != nil {
		t.Fatal(err)
	}
	return code, out.String(), time.Since(stopped)
}

func TestStoppingEndsASandboxThatStops(t *testing.T) {
	code, text, took := runFake(t, "echo ready; exec sleep 30", time.Minute, false)
	if code != 128+int(syscall.SIGTERM) || took > 5*time.Second {
		t.Fatalf("code %d after %s, want SIGTERM's at once", code, took)
	}
	if strings.Contains(text, "killing") {
		t.Fatalf("killed a sandbox that stopped: %q", text)
	}
}

func TestStoppingKillsASandboxThatIgnoresSIGTERM(t *testing.T) {
	code, text, took := runFake(t, "trap '' TERM; echo ready; exec sleep 30", 300*time.Millisecond, false)
	if code != 128+int(syscall.SIGKILL) || took > 5*time.Second {
		t.Fatalf("code %d after %s, want SIGKILL's soon after the timeout", code, took)
	}
	if !strings.Contains(text, "did not stop within 300ms; killing it") {
		t.Fatalf("output %q does not say why it was killed", text)
	}
}

func TestKillEndsAStoppingSandboxAtOnce(t *testing.T) {
	code, _, took := runFake(t, "trap '' TERM; echo ready; exec sleep 30", time.Minute, true)
	if code != 128+int(syscall.SIGKILL) || took > 5*time.Second {
		t.Fatalf("code %d after %s, want SIGKILL's at once", code, took)
	}
}
