package tray

import (
	"runtime"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"

	"codeflower-launcher/assets"
)

// The menu bar icon talks to AppKit through purego, so the launcher still
// builds without cgo for every system.

func init() {
	// AppKit's run loop must run on the main thread: the main goroutine stays
	// on it, and Run is called from there.
	runtime.LockOSThread()
}

// available: every Mac session that is not over SSH has a menu bar.
func available() error { return nil }

type nsPoint struct{ X, Y float64 }
type nsSize struct{ Width, Height float64 }

const (
	activationPolicyAccessory   = 1
	variableStatusItemLength    = -1.0
	eventTypeApplicationDefined = 15
)

// Menu item tags, which tell clicked: what was chosen.
const (
	tagOpen = iota
	tagStopNow
	tagKeepRunning
	tagQuit
	tagNone = -1
)

var sel = objc.RegisterName

// shown is what the handlers on the main thread read.
var shown struct {
	sync.Mutex
	menu                       Menu
	status                     Status
	app                        objc.ID
	line, stopNow, keepRunning objc.ID
}

func class(name string) objc.ID { return objc.ID(objc.GetClass(name)) }

func nsString(text string) objc.ID {
	bytes := append([]byte(text), 0)
	result := class("NSString").Send(sel("stringWithUTF8String:"), unsafe.Pointer(&bytes[0]))
	runtime.KeepAlive(bytes)
	return result
}

func clicked(_ objc.ID, _ objc.SEL, sender objc.ID) {
	shown.Lock()
	menu := shown.menu
	shown.Unlock()
	switch objc.Send[int](sender, sel("tag")) {
	case tagOpen:
		go menu.Open()
	case tagStopNow:
		go menu.StopNow()
	case tagKeepRunning:
		go menu.KeepRunning()
	case tagQuit:
		go menu.Quit()
	}
}

// refresh shows the latest status; it runs on the main thread.
func refresh(_ objc.ID, _ objc.SEL, _ objc.ID) {
	shown.Lock()
	defer shown.Unlock()
	shown.line.Send(sel("setTitle:"), nsString(shown.status.Text))
	shown.stopNow.Send(sel("setHidden:"), !shown.status.WrappingUp)
	shown.keepRunning.Send(sel("setHidden:"), !shown.status.WrappingUp)
}

// stop ends the run loop; it runs on the main thread. The run loop notices
// only after its next event, so one is posted.
func stop(_ objc.ID, _ objc.SEL, _ objc.ID) {
	shown.Lock()
	app := shown.app
	shown.Unlock()
	app.Send(sel("stop:"), objc.ID(0))
	event := class("NSEvent").Send(
		sel("otherEventWithType:location:modifierFlags:timestamp:windowNumber:context:subtype:data1:data2:"),
		uint(eventTypeApplicationDefined), nsPoint{}, uint(0), float64(0), int(0), objc.ID(0), int16(0), int(0), int(0),
	)
	app.Send(sel("postEvent:atStart:"), event, true)
}

func run(menu Menu, status <-chan Status, notices <-chan Notice, done <-chan struct{}) error {
	if _, err := purego.Dlopen("/System/Library/Frameworks/AppKit.framework/AppKit", purego.RTLD_GLOBAL|purego.RTLD_NOW); err != nil {
		return ErrUnavailable
	}
	targetClass, err := objc.RegisterClass("CodeflowerTrayTarget", objc.GetClass("NSObject"), nil, nil, []objc.MethodDef{
		{Cmd: sel("clicked:"), Fn: clicked},
		{Cmd: sel("refresh:"), Fn: refresh},
		{Cmd: sel("stop:"), Fn: stop},
		{Cmd: sel("notice:"), Fn: postNotices},
	})
	if err != nil {
		return err
	}
	pool := class("NSAutoreleasePool").Send(sel("new"))
	target := objc.ID(targetClass).Send(sel("new"))
	app := class("NSApplication").Send(sel("sharedApplication"))
	// A menu bar app: no Dock icon, no menu of its own.
	app.Send(sel("setActivationPolicy:"), activationPolicyAccessory)

	icon := class("NSData").Send(sel("dataWithBytes:length:"), unsafe.Pointer(&assets.TrayTemplate[0]), uint(len(assets.TrayTemplate)))
	image := class("NSImage").Send(sel("alloc")).Send(sel("initWithData:"), icon)
	image.Send(sel("setTemplate:"), true)
	image.Send(sel("setSize:"), nsSize{Width: 18, Height: 18})

	item := class("NSStatusBar").Send(sel("systemStatusBar")).Send(sel("statusItemWithLength:"), float64(variableStatusItemLength))
	item.Send(sel("retain"))
	button := item.Send(sel("button"))
	button.Send(sel("setImage:"), image)
	button.Send(sel("setToolTip:"), nsString(menu.Tooltip))

	nsMenu := class("NSMenu").Send(sel("new"))
	nsMenu.Send(sel("setAutoenablesItems:"), false)
	add := func(title string, tag int) objc.ID {
		entry := class("NSMenuItem").Send(sel("alloc")).Send(sel("initWithTitle:action:keyEquivalent:"),
			nsString(title), sel("clicked:"), nsString(""))
		entry.Send(sel("setTarget:"), target)
		entry.Send(sel("setTag:"), tag)
		entry.Send(sel("setEnabled:"), tag != tagNone)
		nsMenu.Send(sel("addItem:"), entry)
		return entry
	}
	add("Open code flower", tagOpen)
	line := add(menu.Tooltip, tagNone)
	stopNow := add("Stop now", tagStopNow)
	keepRunning := add("Keep running", tagKeepRunning)
	stopNow.Send(sel("setHidden:"), true)
	keepRunning.Send(sel("setHidden:"), true)
	nsMenu.Send(sel("addItem:"), class("NSMenuItem").Send(sel("separatorItem")))
	add("Quit code flower", tagQuit)
	item.Send(sel("setMenu:"), nsMenu)

	shown.Lock()
	shown.menu, shown.app, shown.line, shown.stopNow, shown.keepRunning = menu, app, line, stopNow, keepRunning
	shown.Unlock()
	if !setupNotices(menu.OpenLink) {
		// Outside the app bundle: notices are dropped.
		notices = nil
	}
	pool.Send(sel("drain"))

	onMainThread := sel("performSelectorOnMainThread:withObject:waitUntilDone:")
	go func() {
		for {
			select {
			case current := <-status:
				shown.Lock()
				shown.status = current
				shown.Unlock()
				target.Send(onMainThread, sel("refresh:"), objc.ID(0), false)
			case notice := <-notices:
				queueNotice(notice)
				target.Send(onMainThread, sel("notice:"), objc.ID(0), false)
			case <-done:
				target.Send(onMainThread, sel("stop:"), objc.ID(0), false)
				return
			}
		}
	}()
	app.Send(sel("run"))
	return nil
}
