package notify

import (
	"bytes"
	"image"
	"image/draw"
	"image/png"
	"slices"
	"strings"
	"sync"

	"github.com/godbus/dbus/v5"

	"codeflower-launcher/assets"
	"codeflower-launcher/internal/desktop"
)

// The freedesktop notification service, which GNOME, KDE and the other
// desktops run (https://specifications.freedesktop.org/notification-spec/).
const (
	service   = "org.freedesktop.Notifications"
	path      = "/org/freedesktop/Notifications"
	appName   = "code flower"
	clickedOn = "default"
)

type dbusNotifier struct {
	conn *dbus.Conn
	open func(string)
	// actions: the service shows actions (a click on the notice is one);
	// markup: it reads the body as markup, which must be escaped.
	actions, markup bool
	icon            *imageData
	mu              sync.Mutex
	// urls of the notices shown, by their id, until they close.
	urls map[uint32]string
}

// imageData is the image-data hint: raw RGBA rows (iiibiiay).
type imageData struct {
	Width, Height, RowStride int32
	HasAlpha                 bool
	BitsPerSample, Channels  int32
	Data                     []byte
}

func newNotifier(open func(string)) (Notifier, error) {
	conn, err := desktop.SessionBus()
	if err != nil {
		return nil, ErrUnavailable
	}
	if !desktop.HasOwner(conn, service) && !activatable(conn) {
		conn.Close()
		return nil, ErrUnavailable
	}
	var capabilities []string
	_ = conn.Object(service, path).Call(service+".GetCapabilities", 0).Store(&capabilities)
	notifier := &dbusNotifier{
		conn:    conn,
		open:    open,
		actions: slices.Contains(capabilities, "actions"),
		markup:  slices.Contains(capabilities, "body-markup"),
		icon:    iconData(assets.Icon),
		urls:    map[uint32]string{},
	}
	if err := conn.AddMatchSignal(dbus.WithMatchObjectPath(path), dbus.WithMatchInterface(service)); err != nil {
		conn.Close()
		return nil, ErrUnavailable
	}
	signals := make(chan *dbus.Signal, 16)
	conn.Signal(signals)
	go notifier.listen(signals)
	return notifier, nil
}

// activatable: the bus starts the service on the first notice.
func activatable(conn *dbus.Conn) bool {
	var names []string
	if conn.BusObject().Call("org.freedesktop.DBus.ListActivatableNames", 0).Store(&names) != nil {
		return false
	}
	return slices.Contains(names, service)
}

func (n *dbusNotifier) listen(signals <-chan *dbus.Signal) {
	for signal := range signals {
		if len(signal.Body) < 2 {
			continue
		}
		id, ok := signal.Body[0].(uint32)
		if !ok {
			continue
		}
		switch signal.Name {
		case service + ".ActionInvoked":
			n.mu.Lock()
			url := n.urls[id]
			n.mu.Unlock()
			if action, _ := signal.Body[1].(string); action == clickedOn && url != "" {
				go n.open(url)
			}
		case service + ".NotificationClosed":
			n.mu.Lock()
			delete(n.urls, id)
			n.mu.Unlock()
		}
	}
}

func (n *dbusNotifier) Show(notice Notice) error {
	var id uint32
	if err := n.conn.Object(service, path).Call(service+".Notify", 0, notifyArgs(notice, n.actions, n.markup, n.icon)...).Store(&id); err != nil {
		return err
	}
	if notice.URL != "" && n.actions {
		n.mu.Lock()
		n.urls[id] = notice.URL
		n.mu.Unlock()
	}
	return nil
}

func (n *dbusNotifier) Close() error { return n.conn.Close() }

// notifyArgs are Notify's arguments: app name, replaces id, icon, summary,
// body, actions, hints, timeout (-1: the service's default).
func notifyArgs(notice Notice, actions, markup bool, icon *imageData) []any {
	body := notice.Body
	if markup {
		body = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(body)
	}
	actionList := []string{}
	if actions && notice.URL != "" {
		actionList = []string{clickedOn, "Open in VS Code"}
	}
	hints := map[string]dbus.Variant{"category": dbus.MakeVariant("im.received")}
	if icon != nil {
		hints["image-data"] = dbus.MakeVariant(*icon)
	}
	return []any{appName, uint32(0), "", notice.Title, body, actionList, hints, int32(-1)}
}

// iconData decodes the app's PNG for the image-data hint; nil if it cannot.
func iconData(encoded []byte) *imageData {
	decoded, err := png.Decode(bytes.NewReader(encoded))
	if err != nil {
		return nil
	}
	bounds := decoded.Bounds()
	rgba := image.NewNRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(rgba, rgba.Bounds(), decoded, bounds.Min, draw.Src)
	return &imageData{
		Width: int32(bounds.Dx()), Height: int32(bounds.Dy()), RowStride: int32(rgba.Stride),
		HasAlpha: true, BitsPerSample: 8, Channels: 4, Data: rgba.Pix,
	}
}
