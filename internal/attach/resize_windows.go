//go:build windows

package attach

import (
	"os"
	"time"
)

// watchSize reports the console's new size when it changes; Windows sends no
// signal for it, so it is looked at twice a second.
func watchSize(out *os.File) (<-chan size, func()) {
	sizes := make(chan size, 1)
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		last := windowSize(out)
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if current := windowSize(out); current != last {
					last = current
					select {
					case sizes <- current:
					default:
					}
				}
			}
		}
	}()
	return sizes, func() { close(done) }
}
