//go:build !windows

package jobs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/shirou/gopsutil/v4/process"
)

// localBackend runs jobs on this computer, each in a session of its own so
// it outlives the app and a terminal's Ctrl+C.
type localBackend struct{}

func (localBackend) submit(ctx context.Context, data ScriptData, record *Record) (string, error) {
	output, err := os.OpenFile(data.Output, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return "", err
	}
	defer output.Close()
	command := exec.Command("/bin/sh", data.Script)
	command.Dir = data.Directory
	command.Stdout, command.Stderr = output, output
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return "", fmt.Errorf("could not start the job: %w", err)
	}
	// Reaped while the app runs; after that, the system adopts it.
	go command.Wait()
	record.PID = command.Process.Pid
	record.Host, _ = os.Hostname()
	return strconv.Itoa(record.PID), nil
}

// running says whether a local job's process still runs: the same PID,
// running the job (not a later process given the same number).
func (localBackend) running(record *Record) bool {
	if record.PID <= 0 {
		return false
	}
	proc, err := process.NewProcess(int32(record.PID))
	if err != nil {
		return false
	}
	if status, err := proc.Status(); err == nil && len(status) > 0 && status[0] == process.Zombie {
		return false
	}
	// The script, or what it became: `codeflower job-run …/ID/spec.json`.
	line, err := proc.Cmdline()
	return err == nil && strings.Contains(line, string(filepath.Separator)+record.ID+string(filepath.Separator))
}

func (b localBackend) status(_ context.Context, records []*Record) (map[string]string, error) {
	host, _ := os.Hostname()
	states := map[string]string{}
	for _, record := range records {
		switch {
		case record.Host != "" && record.Host != host:
			// Another computer's: it is the one to tell.
			states[record.ID] = record.State
		case b.running(record):
			states[record.ID] = StateRunning
		}
	}
	return states, nil
}

func (localBackend) history(context.Context, []*Record) (map[string]map[int]taskEnd, error) {
	return map[string]map[int]taskEnd{}, nil
}

func (b localBackend) cancel(_ context.Context, record *Record) error {
	host, _ := os.Hostname()
	if record.Host != "" && record.Host != host {
		return fmt.Errorf("the job runs on %s: cancel it there", record.Host)
	}
	if !b.running(record) {
		return nil
	}
	// The whole session: the script, the app's job-run and the engine's client.
	if err := syscall.Kill(-record.PID, syscall.SIGTERM); err != nil {
		return syscall.Kill(record.PID, syscall.SIGTERM)
	}
	return nil
}
