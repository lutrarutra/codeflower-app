package tray

import (
	"runtime"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"

	"codeflower-app/assets"
)

// The notification area icon is the shell's own (Shell_NotifyIcon), with a
// hidden window for its messages. Its balloons show as notifications on
// Windows 10 and 11, so the icon also shows the notices.

const notifies = true

var (
	user32                  = windows.NewLazySystemDLL("user32.dll")
	shell32                 = windows.NewLazySystemDLL("shell32.dll")
	kernel32                = windows.NewLazySystemDLL("kernel32.dll")
	procRegisterClassEx     = user32.NewProc("RegisterClassExW")
	procCreateWindowEx      = user32.NewProc("CreateWindowExW")
	procDestroyWindow       = user32.NewProc("DestroyWindow")
	procDefWindowProc       = user32.NewProc("DefWindowProcW")
	procGetMessage          = user32.NewProc("GetMessageW")
	procTranslateMessage    = user32.NewProc("TranslateMessage")
	procDispatchMessage     = user32.NewProc("DispatchMessageW")
	procPostMessage         = user32.NewProc("PostMessageW")
	procPostQuitMessage     = user32.NewProc("PostQuitMessage")
	procRegisterWindowMsg   = user32.NewProc("RegisterWindowMessageW")
	procCreatePopupMenu     = user32.NewProc("CreatePopupMenu")
	procAppendMenu          = user32.NewProc("AppendMenuW")
	procTrackPopupMenu      = user32.NewProc("TrackPopupMenu")
	procDestroyMenu         = user32.NewProc("DestroyMenu")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procGetCursorPos        = user32.NewProc("GetCursorPos")
	procGetSystemMetrics    = user32.NewProc("GetSystemMetrics")
	procCreateIconFromRes   = user32.NewProc("CreateIconFromResourceEx")
	procDestroyIcon         = user32.NewProc("DestroyIcon")
	procShellNotifyIcon     = shell32.NewProc("Shell_NotifyIconW")
	procGetModuleHandle     = kernel32.NewProc("GetModuleHandleW")
)

const (
	wmNull        = 0x0000
	wmDestroy     = 0x0002
	wmClose       = 0x0010
	wmContextMenu = 0x007B
	wmUser        = 0x0400
	wmApp         = 0x8000
	// The icon's messages, and ours from other goroutines.
	wmTrayIcon = wmApp + 1
	wmShow     = wmApp + 2

	ninSelect           = wmUser + 0
	ninKeySelect        = wmUser + 1
	ninBalloonUserClick = wmUser + 5

	nimAdd        = 0
	nimModify     = 1
	nimDelete     = 2
	nimSetVersion = 4

	nifMessage  = 0x01
	nifIcon     = 0x02
	nifTip      = 0x04
	nifInfo     = 0x10
	nifShowTip  = 0x80
	niifUser    = 0x04
	niifLarge   = 0x20
	iconVersion = 4

	mfString    = 0x0000
	mfGrayed    = 0x0001
	mfSeparator = 0x0800

	tpmRightButton = 0x0002
	tpmReturnCmd   = 0x0100
	tpmNoNotify    = 0x0080

	smCxIcon   = 11
	smCxSmIcon = 49
)

// Menu commands; 0 is none.
const (
	commandOpen = iota + 1
	commandStopNow
	commandKeepRunning
	commandQuit
	commandStatus
)

type notifyIconData struct {
	Size             uint32
	Wnd              windows.Handle
	ID               uint32
	Flags            uint32
	CallbackMessage  uint32
	Icon             windows.Handle
	Tip              [128]uint16
	State, StateMask uint32
	Info             [256]uint16
	TimeoutOrVersion uint32
	InfoTitle        [64]uint16
	InfoFlags        uint32
	GUIDItem         windows.GUID
	BalloonIcon      windows.Handle
}

type wndClassEx struct {
	Size, Style        uint32
	WndProc            uintptr
	ClsExtra, WndExtra int32
	Instance, Icon     windows.Handle
	Cursor, Background windows.Handle
	MenuName           *uint16
	ClassName          *uint16
	IconSm             windows.Handle
}

type message struct {
	Wnd            windows.Handle
	Message        uint32
	WParam, LParam uintptr
	Time           uint32
	X, Y           int32
	Private        uint32
}

// icon is the window's state; the window procedure runs on Run's thread,
// the channels are read on another goroutine.
var icon struct {
	sync.Mutex
	menu           Menu
	status         Status
	queue          []Notice
	window         windows.Handle
	small, large   windows.Handle
	taskbarCreated uint32
	lastURL        string
}

func available() error { return nil }

func run(menu Menu, status <-chan Status, notices <-chan Notice, done <-chan struct{}) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	instance, _, _ := procGetModuleHandle.Call(0)
	className, _ := windows.UTF16PtrFromString("CodeflowerTray")
	class := wndClassEx{WndProc: windows.NewCallback(windowProc), Instance: windows.Handle(instance), ClassName: className}
	class.Size = uint32(unsafe.Sizeof(class))
	if atom, _, _ := procRegisterClassEx.Call(uintptr(unsafe.Pointer(&class))); atom == 0 {
		return ErrUnavailable
	}
	taskbarCreated, _ := windows.UTF16PtrFromString("TaskbarCreated")
	created, _, _ := procRegisterWindowMsg.Call(uintptr(unsafe.Pointer(taskbarCreated)))

	icon.Lock()
	icon.menu = menu
	icon.taskbarCreated = uint32(created)
	icon.small = loadIcon(smCxSmIcon)
	icon.large = loadIcon(smCxIcon)
	icon.Unlock()

	// A hidden top-level window, not a message-only one: only those hear
	// TaskbarCreated when Explorer restarts.
	window, _, _ := procCreateWindowEx.Call(0, uintptr(unsafe.Pointer(className)), 0, 0, 0, 0, 0, 0, 0, 0, instance, 0)
	if window == 0 {
		return ErrUnavailable
	}
	icon.Lock()
	icon.window = windows.Handle(window)
	icon.Unlock()
	if !addIcon() {
		procDestroyWindow.Call(window)
		return ErrUnavailable
	}

	go func() {
		for {
			select {
			case current := <-status:
				icon.Lock()
				icon.status = current
				icon.Unlock()
				procPostMessage.Call(window, wmShow, 0, 0)
			case notice := <-notices:
				icon.Lock()
				icon.queue = append(icon.queue, notice)
				icon.Unlock()
				procPostMessage.Call(window, wmShow, 0, 0)
			case <-done:
				procPostMessage.Call(window, wmClose, 0, 0)
				return
			}
		}
	}()

	var msg message
	for {
		got, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(got) <= 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessage.Call(uintptr(unsafe.Pointer(&msg)))
	}
	icon.Lock()
	procDestroyIcon.Call(uintptr(icon.small))
	procDestroyIcon.Call(uintptr(icon.large))
	icon.Unlock()
	return nil
}

// loadIcon makes an icon of the system's size from the app's PNG, which
// Windows reads since Vista.
func loadIcon(metric uintptr) windows.Handle {
	size, _, _ := procGetSystemMetrics.Call(metric)
	handle, _, _ := procCreateIconFromRes.Call(uintptr(unsafe.Pointer(&assets.Icon[0])), uintptr(len(assets.Icon)), 1, 0x00030000, size, size, 0)
	return windows.Handle(handle)
}

func iconData() notifyIconData {
	icon.Lock()
	defer icon.Unlock()
	data := notifyIconData{Wnd: icon.window, ID: 1}
	data.Size = uint32(unsafe.Sizeof(data))
	return data
}

// addIcon puts the icon in the notification area, again after Explorer
// restarts.
func addIcon() bool {
	data := iconData()
	icon.Lock()
	data.Flags = nifMessage | nifIcon | nifTip | nifShowTip
	data.CallbackMessage = wmTrayIcon
	data.Icon = icon.small
	setText(data.Tip[:], tooltip())
	icon.Unlock()
	if ok, _, _ := procShellNotifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(&data))); ok == 0 {
		return false
	}
	// Version 4: clicks come as NIN_SELECT and WM_CONTEXTMENU.
	data.TimeoutOrVersion = iconVersion
	procShellNotifyIcon.Call(nimSetVersion, uintptr(unsafe.Pointer(&data)))
	return true
}

// tooltip is the menu's tooltip with the status line; icon is locked.
func tooltip() string {
	if icon.status.Text == "" {
		return icon.menu.Tooltip
	}
	return icon.menu.Tooltip + ": " + icon.status.Text
}

// setText copies text into a fixed buffer, cut to fit with its NUL.
func setText(buffer []uint16, text string) {
	encoded, _ := windows.UTF16FromString(text)
	if len(encoded) > len(buffer) {
		encoded = append(encoded[:len(buffer)-1], 0)
	}
	copy(buffer, encoded)
}

// show brings the tooltip up to date and shows the queued notices as
// balloons, one at a time as the shell queues them itself.
func show() {
	icon.Lock()
	queue, text := icon.queue, tooltip()
	icon.queue = nil
	icon.Unlock()
	data := iconData()
	data.Flags = nifTip | nifShowTip
	setText(data.Tip[:], text)
	procShellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(&data)))
	for _, notice := range queue {
		data := iconData()
		data.Flags = nifInfo
		setText(data.InfoTitle[:], notice.Title)
		setText(data.Info[:], notice.Body)
		icon.Lock()
		data.InfoFlags = niifUser | niifLarge
		data.BalloonIcon = icon.large
		icon.lastURL = notice.URL
		icon.Unlock()
		procShellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(&data)))
	}
}

// popUp shows the menu at the cursor and runs what was chosen.
func popUp(window uintptr) {
	icon.Lock()
	menu, status := icon.menu, icon.status
	icon.Unlock()
	popup, _, _ := procCreatePopupMenu.Call()
	if popup == 0 {
		return
	}
	defer procDestroyMenu.Call(popup)
	add := func(flags, command uintptr, text string) {
		encoded, _ := windows.UTF16PtrFromString(text)
		procAppendMenu.Call(popup, flags, command, uintptr(unsafe.Pointer(encoded)))
	}
	add(mfString, commandOpen, "Open code flower")
	if status.Text != "" {
		add(mfString|mfGrayed, commandStatus, status.Text)
	}
	if status.WrappingUp {
		add(mfString, commandStopNow, "Stop now")
		add(mfString, commandKeepRunning, "Keep running")
	}
	add(mfSeparator, 0, "")
	add(mfString, commandQuit, "Quit")
	var point struct{ X, Y int32 }
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&point)))
	// The menu closes on a click elsewhere only if the window is in front.
	procSetForegroundWindow.Call(window)
	chosen, _, _ := procTrackPopupMenu.Call(popup, tpmRightButton|tpmReturnCmd|tpmNoNotify, uintptr(point.X), uintptr(point.Y), 0, window, 0)
	procPostMessage.Call(window, wmNull, 0, 0)
	switch chosen {
	case commandOpen:
		go menu.Open()
	case commandStopNow:
		go menu.StopNow()
	case commandKeepRunning:
		go menu.KeepRunning()
	case commandQuit:
		go menu.Quit()
	}
}

func windowProc(window, msg, wParam, lParam uintptr) uintptr {
	icon.Lock()
	taskbarCreated, menu, lastURL := icon.taskbarCreated, icon.menu, icon.lastURL
	icon.Unlock()
	switch {
	case msg == wmTrayIcon:
		switch lParam & 0xFFFF {
		case ninSelect, ninKeySelect:
			go menu.Open()
		case wmContextMenu:
			popUp(window)
		case ninBalloonUserClick:
			if lastURL != "" && menu.OpenLink != nil {
				go menu.OpenLink(lastURL)
			}
		}
		return 0
	case msg == wmShow:
		show()
		return 0
	case taskbarCreated != 0 && msg == uintptr(taskbarCreated):
		addIcon()
		return 0
	case msg == wmClose:
		procDestroyWindow.Call(window)
		return 0
	case msg == wmDestroy:
		data := iconData()
		procShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&data)))
		procPostQuitMessage.Call(0)
		return 0
	}
	result, _, _ := procDefWindowProc.Call(window, msg, wParam, lParam)
	return result
}
