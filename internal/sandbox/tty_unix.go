//go:build !windows

package sandbox

import (
	"os"
	"os/signal"
	"syscall"
)

// watchTerminalSize calls changed when the terminal's size changes.
func watchTerminalSize(changed func()) func() {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGWINCH)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case <-signals:
				changed()
			}
		}
	}()
	return func() {
		signal.Stop(signals)
		close(done)
	}
}
