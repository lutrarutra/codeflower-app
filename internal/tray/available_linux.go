package tray

import "codeflower-app/internal/desktop"

// available: a graphical session whose bus has a StatusNotifierItem host
// (KDE, XFCE, Cinnamon, GNOME with the AppIndicator extension). Stock GNOME,
// WSL, and login nodes have none.
func available() error {
	conn, err := desktop.SessionBus()
	if err != nil {
		return ErrUnavailable
	}
	defer conn.Close()
	if !desktop.HasOwner(conn, "org.kde.StatusNotifierWatcher") {
		return ErrUnavailable
	}
	return nil
}
