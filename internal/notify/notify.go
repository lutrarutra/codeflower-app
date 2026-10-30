// Package notify shows desktop notifications through the desktop's own
// notification service (Linux), with or without a tray icon. On macOS and
// Windows the tray icon shows them instead (package tray).
package notify

import (
	"errors"
	"os"
)

// ErrUnavailable: this session has no notification service to talk to.
var ErrUnavailable = errors.New("no desktop notifications here")

// Notice is one notification; a click on it opens URL.
type Notice struct {
	Title string
	Body  string
	URL   string
}

// Notifier shows notices until it is closed.
type Notifier interface {
	Show(Notice) error
	Close() error
}

// New connects to the notification service; open is called with a
// notice's URL when the user clicks it. Not over SSH, where the
// notifications would show on the remote machine's desktop if anywhere.
func New(open func(url string)) (Notifier, error) {
	if os.Getenv("SSH_CONNECTION") != "" {
		return nil, ErrUnavailable
	}
	return newNotifier(open)
}
