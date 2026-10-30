// Package tray shows the client as an icon in the system tray (Windows, Linux
// desktops with StatusNotifierItem) or the menu bar (macOS), from which the
// user opens the client's page or quits it.
package tray

import (
	"errors"
	"os"
)

// ErrUnavailable: this session has no tray to show an icon in.
var ErrUnavailable = errors.New("no system tray here")

// Menu is what the icon offers. Each function runs on a goroutine of its own.
type Menu struct {
	Tooltip string
	// Open opens the client's page; also a left click on Windows and Linux.
	Open func()
	// Quit asks before quitting, as the page does.
	Quit func()
	// StopNow and KeepRunning are shown while a quit waits for chats to
	// wrap up.
	StopNow     func()
	KeepRunning func()
	// OpenLink opens a notice's URL when the user clicks it.
	OpenLink func(url string)
}

// Notice is a notification the icon shows (macOS, Windows); a click on it
// opens URL.
type Notice struct {
	Title string
	Body  string
	URL   string
}

// Notifies says whether the icon shows notices; on Linux the desktop's own
// notification service does (package notify), with or without an icon.
func Notifies() bool { return notifies }

// Status is the client's state as the menu shows it.
type Status struct {
	// Text is a line that cannot be clicked, e.g. "2 sandboxes running".
	Text string
	// WrappingUp shows Stop now and Keep running.
	WrappingUp bool
}

// Available says whether an icon can be shown: not over SSH, and only where
// a tray (or a menu bar) runs.
func Available() error {
	if os.Getenv("SSH_CONNECTION") != "" {
		return ErrUnavailable
	}
	return available()
}

// Run shows the icon until done closes, with the status of each value on
// status and, where it Notifies, each notice on notices. On macOS it must run
// on the main goroutine, which this package keeps on the main thread.
func Run(menu Menu, status <-chan Status, notices <-chan Notice, done <-chan struct{}) error {
	return run(menu, status, notices, done)
}
