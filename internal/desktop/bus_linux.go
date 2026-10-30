package desktop

import (
	"errors"
	"fmt"
	"os"

	"github.com/godbus/dbus/v5"
)

// ErrNoSession: no graphical session, or no bus for it.
var ErrNoSession = errors.New("no desktop session")

// SessionBus connects to the session bus of a graphical session.
func SessionBus() (*dbus.Conn, error) {
	if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return nil, ErrNoSession
	}
	address := os.Getenv("DBUS_SESSION_BUS_ADDRESS")
	if address == "" {
		// Without the variable, only the user's bus socket; never start a bus.
		socket := fmt.Sprintf("/run/user/%d/bus", os.Getuid())
		if _, err := os.Stat(socket); err != nil {
			return nil, ErrNoSession
		}
		address = "unix:path=" + socket
	}
	conn, err := dbus.Connect(address)
	if err != nil {
		return nil, ErrNoSession
	}
	return conn, nil
}

// HasOwner says whether a service owns name on the bus.
func HasOwner(conn *dbus.Conn, name string) bool {
	var owned bool
	err := conn.BusObject().Call("org.freedesktop.DBus.NameHasOwner", 0, name).Store(&owned)
	return err == nil && owned
}
