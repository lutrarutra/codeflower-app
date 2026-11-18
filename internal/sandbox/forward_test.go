//go:build !windows

package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a log the tunnel writes from its own goroutine.
type syncBuffer struct {
	mu   sync.Mutex
	text strings.Builder
}

func (b *syncBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.text.Write(data)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.text.String()
}

// fakeSSH writes an ssh that runs script, with its arguments in args.
func fakeSSH(t *testing.T, script string) (program, folder string) {
	t.Helper()
	folder = t.TempDir()
	program = filepath.Join(folder, "ssh")
	text := "#!/bin/sh\necho \"$@\" > " + filepath.Join(folder, "args") + "\necho $$ > " + filepath.Join(folder, "pid") + "\n" + script
	if err := os.WriteFile(program, []byte(text), 0o755); err != nil {
		t.Fatal(err)
	}
	return program, folder
}

func waitFor(t *testing.T, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestForwardOpensThePortAndStopsWithTheSandbox(t *testing.T) {
	program, folder := fakeSSH(t, "echo 'debug1: remote forward success for: listen 127.0.0.1:4100, connect 127.0.0.1:4100' >&2\nexec sleep 60\n")
	var log syncBuffer
	stop := Forward{Host: "login1", Port: 4100, Target: "127.0.0.1", Log: &log, SSH: program}.Start(context.Background())
	waitFor(t, "the tunnel", func() bool { return strings.Contains(log.String(), "reaches this sandbox") })
	if want := "  forward  127.0.0.1:4100 on login1 reaches this sandbox while it runs\n"; log.String() != want {
		t.Errorf("log %q, want %q", log.String(), want)
	}
	args, _ := os.ReadFile(filepath.Join(folder, "args"))
	if !strings.Contains(string(args), "-R 127.0.0.1:4100:127.0.0.1:4100 -- login1") || !strings.Contains(string(args), "BatchMode=yes") {
		t.Errorf("ssh arguments %s", args)
	}
	data, _ := os.ReadFile(filepath.Join(folder, "pid"))
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	stop()
	if processAlive(pid) {
		t.Errorf("ssh %d still runs after the sandbox stopped", pid)
	}
}

func TestForwardSaysWhyOnceAndKeepsTrying(t *testing.T) {
	program, folder := fakeSSH(t, "echo runs >> "+"\"$(dirname \"$0\")/runs\"\necho 'Host key verification failed.' >&2\nexit 255\n")
	var log syncBuffer
	stop := Forward{Host: "login1", Port: 4100, Target: "127.0.0.1", Log: &log, SSH: program, Retry: 40 * time.Millisecond}.Start(context.Background())
	waitFor(t, "three attempts", func() bool {
		runs, _ := os.ReadFile(filepath.Join(folder, "runs"))
		return strings.Count(string(runs), "runs") >= 3
	})
	stop()
	text := log.String()
	if strings.Count(text, "Host key verification failed") != 1 || !strings.Contains(text, "ssh login1 true") {
		t.Errorf("log %q", text)
	}
}

func TestForwardSaysWhenItDropsAndComesBack(t *testing.T) {
	// Up, then closed by the other side, then up for good.
	program, _ := fakeSSH(t, `
count=$(cat "$(dirname "$0")/count" 2>/dev/null || echo 0)
echo $((count + 1)) > "$(dirname "$0")/count"
echo 'debug1: remote forward success for: listen 127.0.0.1:4100, connect 127.0.0.1:4100' >&2
if [ "$count" = 0 ]; then echo 'Connection to login1 closed by remote host.' >&2; exit 255; fi
exec sleep 60
`)
	var log syncBuffer
	stop := Forward{Host: "login1", Port: 4100, Target: "127.0.0.1", Log: &log, SSH: program, Retry: 40 * time.Millisecond}.Start(context.Background())
	defer stop()
	waitFor(t, "the tunnel again", func() bool { return strings.Contains(log.String(), "open again") })
	lines := strings.Split(strings.TrimSpace(log.String()), "\n")
	if len(lines) != 3 || !strings.Contains(lines[1], "the tunnel closed (Connection to login1 closed by remote host.)") {
		t.Errorf("log:\n%s", log.String())
	}
}

func TestForwardArguments(t *testing.T) {
	args := Forward{Host: "me@login1", Port: 4100, Target: "fd00::5"}.arguments()
	if !slices.Contains(args, "127.0.0.1:4100:[fd00::5]:4100") || args[len(args)-2] != "--" || args[len(args)-1] != "me@login1" {
		t.Errorf("arguments %v", args)
	}
}

func TestForwardTarget(t *testing.T) {
	for _, test := range []struct{ engine, host, want string }{
		{EnginePodman, "10.0.0.5", "127.0.0.1"},
		{EngineDocker, "", "127.0.0.1"},
		{EngineApptainer, "0.0.0.0", "127.0.0.1"},
		{EngineApptainer, "127.0.0.1", "127.0.0.1"},
		{EngineApptainer, "10.0.0.5", "10.0.0.5"},
	} {
		if got := ForwardTarget(test.engine, test.host); got != test.want {
			t.Errorf("%s --host %s: %s, want %s", test.engine, test.host, got, test.want)
		}
	}
}

func TestForwardHostIsNoOption(t *testing.T) {
	for _, host := range []string{"-oProxyCommand=touch /tmp/x", "login1 -v", "login1\n"} {
		if CheckForwardHost(host) == nil {
			t.Errorf("%q accepted", host)
		}
	}
}
