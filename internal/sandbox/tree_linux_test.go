package sandbox

import (
	"bufio"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// running reports whether pid runs; a zombie, which nothing reaped yet, does not.
func running(pid int) bool {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	fields := strings.Fields(string(data[strings.LastIndexByte(string(data), ')')+1:]))
	return len(fields) > 0 && fields[0] != "Z"
}

func TestKillTreeKillsWhatTheProcessStarted(t *testing.T) {
	// Like Apptainer's starter: a child and a grandchild that ignore SIGTERM.
	command := exec.Command("sh", "-c", `trap "" TERM
sleep 300 & echo $!
sh -c 'sleep 300 & echo $!; wait' &
wait`)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	var started []int
	lines := bufio.NewScanner(stdout)
	for len(started) < 2 && lines.Scan() {
		pid, err := strconv.Atoi(strings.TrimSpace(lines.Text()))
		if err != nil {
			t.Fatal(err)
		}
		started = append(started, pid)
	}
	if err := killTree(command.Process.Pid); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for _, pid := range started {
		for running(pid) && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		if running(pid) {
			t.Errorf("process %d still runs", pid)
			_ = exec.Command("kill", "-9", strconv.Itoa(pid)).Run()
		}
	}
}

func TestStopContainerSignalsTheContainersProcess(t *testing.T) {
	// Like Apptainer: starter, then appinit (a shell under that name), then
	// the container's process, which reports SIGTERM, and a later child of
	// appinit, as an orphan that moved to it would be.
	dir := t.TempDir()
	if err := os.Symlink("/bin/sh", dir+"/appinit"); err != nil {
		t.Fatal(err)
	}
	script := func(name, text string) {
		if err := os.WriteFile(dir+"/"+name, []byte(text), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	script("main.sh", `trap 'echo $$ > `+dir+`/stopped; exit 0' TERM
echo $$ > `+dir+`/main
while :; do sleep 0.05; done
`)
	script("init.sh", `sh `+dir+`/main.sh &
until [ -s `+dir+`/main ]; do sleep 0.05; done
sleep 300 & echo $! > `+dir+`/orphan
wait
`)
	starter := exec.Command("sh", "-c", dir+"/appinit "+dir+"/init.sh; :")
	if err := starter.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = killTree(starter.Process.Pid); _ = starter.Wait() }()
	read := func(name string) int {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if data, err := os.ReadFile(dir + "/" + name); err == nil && len(data) > 0 {
				pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
				return pid
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("no %s", name)
		return 0
	}
	main, orphan := read("main"), read("orphan")
	if err := stopContainer(starter.Process); err != nil {
		t.Fatal(err)
	}
	if stopped := read("stopped"); stopped != main {
		t.Fatalf("process %d got SIGTERM, want the container's %d", stopped, main)
	}
	if !running(orphan) || !running(starter.Process.Pid) {
		t.Fatal("SIGTERM reached more than the container's process")
	}
}

func TestStopContainerSignalsApptainerBeforeTheContainerRuns(t *testing.T) {
	command := exec.Command("sleep", "300")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if err := stopContainer(command.Process); err != nil {
		t.Fatal(err)
	}
	if code := exitCode(command.Wait()); code != 128+15 {
		t.Fatalf("exit code %d, want SIGTERM's", code)
	}
}
