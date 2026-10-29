package cli

import (
	"syscall"
	"unsafe"
)

// ownConsole says whether Windows made a console just for this program,
// which happens when it is double-clicked in Explorer rather than run from a
// terminal that other processes share.
func ownConsole() bool {
	getConsoleProcessList := syscall.NewLazyDLL("kernel32.dll").NewProc("GetConsoleProcessList")
	if getConsoleProcessList.Find() != nil {
		return false
	}
	processes := make([]uint32, 4)
	count, _, _ := getConsoleProcessList.Call(uintptr(unsafe.Pointer(&processes[0])), uintptr(len(processes)))
	return count == 1
}
