//go:build !windows

package attach

import (
	"os"
	"os/signal"
	"syscall"
)

// watchSize reports the terminal's new size when it changes.
func watchSize(out *os.File) (<-chan size, func()) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGWINCH)
	sizes := make(chan size, 1)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case <-signals:
				select {
				case sizes <- windowSize(out):
				default:
				}
			}
		}
	}()
	return sizes, func() {
		signal.Stop(signals)
		close(done)
	}
}
