package tray

import (
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
)

// Notifications on macOS come from the UserNotifications framework, which
// serves only an app with a bundle (code flower.app) and asks the user once
// whether to show them.

const notifies = true

const (
	authorizationSound = 1 << 1
	authorizationAlert = 1 << 2
	presentSound       = 1 << 1
	presentList        = 1 << 3
	presentBanner      = 1 << 4
	utf8Encoding       = 4
)

var noticeState struct {
	sync.Mutex
	queue  []Notice
	center objc.ID
	open   func(string)
}

// setupNotices asks for permission and listens to clicks; false outside an
// app bundle, where the framework refuses to work. Runs on the main thread.
func setupNotices(open func(string)) bool {
	if class("NSBundle").Send(sel("mainBundle")).Send(sel("bundleIdentifier")) == 0 {
		return false
	}
	if _, err := purego.Dlopen("/System/Library/Frameworks/UserNotifications.framework/UserNotifications", purego.RTLD_GLOBAL|purego.RTLD_NOW); err != nil {
		return false
	}
	var protocols []*objc.Protocol
	if protocol := objc.GetProtocol("UNUserNotificationCenterDelegate"); protocol != nil {
		protocols = append(protocols, protocol)
	}
	delegateClass, err := objc.RegisterClass("CodeflowerNoticeDelegate", objc.GetClass("NSObject"), protocols, nil, []objc.MethodDef{
		{Cmd: sel("userNotificationCenter:willPresentNotification:withCompletionHandler:"), Fn: willPresent},
		{Cmd: sel("userNotificationCenter:didReceiveNotificationResponse:withCompletionHandler:"), Fn: didReceive},
	})
	if err != nil {
		return false
	}
	center := class("UNUserNotificationCenter").Send(sel("currentNotificationCenter"))
	if center == 0 {
		return false
	}
	// The center holds its delegate weakly; this one is never released.
	center.Send(sel("setDelegate:"), objc.ID(delegateClass).Send(sel("new")))
	asked := objc.NewBlock(func(_ objc.Block, _ bool, _ objc.ID) {})
	center.Send(sel("requestAuthorizationWithOptions:completionHandler:"), uint(authorizationAlert|authorizationSound), asked)
	noticeState.Lock()
	noticeState.center, noticeState.open = center, open
	noticeState.Unlock()
	return true
}

// postNotices shows the queued notices; it runs on the main thread.
func postNotices(_ objc.ID, _ objc.SEL, _ objc.ID) {
	noticeState.Lock()
	queue, center := noticeState.queue, noticeState.center
	noticeState.queue = nil
	noticeState.Unlock()
	if center == 0 {
		return
	}
	for _, notice := range queue {
		content := class("UNMutableNotificationContent").Send(sel("new"))
		content.Send(sel("setTitle:"), nsString(notice.Title))
		content.Send(sel("setBody:"), nsString(notice.Body))
		content.Send(sel("setSound:"), class("UNNotificationSound").Send(sel("defaultSound")))
		if notice.URL != "" {
			content.Send(sel("setUserInfo:"), class("NSDictionary").Send(sel("dictionaryWithObject:forKey:"), nsString(notice.URL), nsString("url")))
		}
		identifier := class("NSUUID").Send(sel("UUID")).Send(sel("UUIDString"))
		request := class("UNNotificationRequest").Send(sel("requestWithIdentifier:content:trigger:"), identifier, content, objc.ID(0))
		center.Send(sel("addNotificationRequest:withCompletionHandler:"), request, objc.ID(0))
		content.Send(sel("release"))
	}
}

// willPresent shows notices while the app is active too (its menu is open).
func willPresent(_ objc.ID, _ objc.SEL, _ objc.ID, _ objc.ID, handler objc.Block) {
	callBlock(handler, presentBanner|presentList|presentSound)
}

// didReceive: the user clicked a notice.
func didReceive(_ objc.ID, _ objc.SEL, _ objc.ID, response objc.ID, handler objc.Block) {
	url := response.Send(sel("notification")).Send(sel("request")).Send(sel("content")).
		Send(sel("userInfo")).Send(sel("objectForKey:"), nsString("url"))
	noticeState.Lock()
	open := noticeState.open
	noticeState.Unlock()
	if url != 0 && open != nil {
		go open(goString(url))
	}
	callBlock(handler)
}

// callBlock calls a block that Objective-C made, e.g. a completion handler,
// through the invoke function in its layout.
func callBlock(block objc.Block, args ...uintptr) {
	if block == 0 {
		return
	}
	layout := (*struct {
		isa             uintptr
		flags, reserved int32
		invoke          uintptr
	})(*(*unsafe.Pointer)(unsafe.Pointer(&block)))
	purego.SyscallN(layout.invoke, append([]uintptr{uintptr(block)}, args...)...)
}

func goString(text objc.ID) string {
	length := objc.Send[uint](text, sel("lengthOfBytesUsingEncoding:"), uint(utf8Encoding))
	bytes := objc.Send[*byte](text, sel("UTF8String"))
	if bytes == nil || length == 0 {
		return ""
	}
	return string(unsafe.Slice(bytes, length))
}

func queueNotice(notice Notice) {
	noticeState.Lock()
	noticeState.queue = append(noticeState.queue, notice)
	noticeState.Unlock()
}
