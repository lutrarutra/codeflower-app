//go:build windows

package sandbox

import "time"

// watchTerminalSize looks at the console's size twice a second: Windows sends
// no signal when it changes (followTerminalSize resizes only when it did).
func watchTerminalSize(changed func()) func() {
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				changed()
			}
		}
	}()
	return func() { close(done) }
}
