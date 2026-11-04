package sandbox

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// process is a process as /proc shows it.
type process struct {
	pid, parent int
	name        string
	// started is the start time, in clock ticks after boot.
	started uint64
}

// processes reads every process on the machine from /proc.
func processes() []process {
	var result []process
	entries, _ := os.ReadDir("/proc")
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
		if err != nil {
			continue
		}
		// pid (comm) state ppid … starttime (the 22nd field) …; comm may hold
		// spaces and parentheses.
		stat := string(data)
		open, end := strings.IndexByte(stat, '('), strings.LastIndexByte(stat, ')')
		if open < 0 || end < open {
			continue
		}
		fields := strings.Fields(stat[end+1:])
		if len(fields) < 20 {
			continue
		}
		parent, _ := strconv.Atoi(fields[1])
		started, _ := strconv.ParseUint(fields[19], 10, 64)
		result = append(result, process{pid: pid, parent: parent, name: stat[open+1 : end], started: started})
	}
	return result
}

// stopContainer sends SIGTERM to the process Apptainer runs in the container
// (the bridge): the oldest child of the container's init, appinit, as orphans
// that move to it come later. Apptainer passes signals on only when it is not
// in the terminal's foreground process group, and `codeflower sandbox` in a
// terminal is: Apptainer leaves them to the terminal, and a stop from
// elsewhere, such as the client's, never reached the bridge. Before that
// process runs, it signals Apptainer.
func stopContainer(starter *os.Process) error {
	all := processes()
	for _, shim := range all {
		if shim.parent != starter.Pid || shim.name != "appinit" {
			continue
		}
		var main *process
		for index := range all {
			if all[index].parent == shim.pid && (main == nil || all[index].started < main.started) {
				main = &all[index]
			}
		}
		if main != nil && syscall.Kill(main.pid, syscall.SIGTERM) == nil {
			return nil
		}
	}
	return starter.Signal(syscall.SIGTERM)
}

// killTree kills a process and every process it started. Apptainer's starter
// takes the container down when it dies, but not squashfuse_ll, which mounts
// the image and would keep running. The tree is read before anything is
// killed: orphans move to init and out of it.
func killTree(pid int) error {
	children := map[int][]int{}
	for _, process := range processes() {
		children[process.parent] = append(children[process.parent], process.pid)
	}
	tree := []int{pid}
	for index := 0; index < len(tree); index++ {
		tree = append(tree, children[tree[index]]...)
	}
	err := syscall.Kill(pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		err = os.ErrProcessDone
	}
	for _, process := range tree[1:] {
		_ = syscall.Kill(process, syscall.SIGKILL)
	}
	return err
}
