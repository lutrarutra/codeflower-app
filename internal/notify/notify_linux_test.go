package notify

import (
	"bufio"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"codeflower-launcher/assets"
)

func TestNotifyArguments(t *testing.T) {
	notice := Notice{Title: "Claude Code · atlas", Body: "“a<b” finished", URL: "vscode://x"}
	args := notifyArgs(notice, true, true, nil)
	want := []any{"code flower", uint32(0), "", "Claude Code · atlas", "“a&lt;b” finished",
		[]string{"default", "Open in VS Code"}, map[string]dbus.Variant{"category": dbus.MakeVariant("im.received")}, int32(-1)}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("got %#v", args)
	}
	// Without actions (or a URL) there is nothing to click; without markup
	// the body stays as it is.
	args = notifyArgs(Notice{Title: "t", Body: "a<b"}, true, false, nil)
	if args[4] != "a<b" || len(args[5].([]string)) != 0 {
		t.Fatalf("got %#v", args)
	}
}

func TestIconData(t *testing.T) {
	icon := iconData(assets.Icon)
	if icon == nil || icon.Width == 0 || int(icon.RowStride*icon.Height) != len(icon.Data) || icon.Channels != 4 {
		t.Fatalf("icon %+v", icon)
	}
	// The signature the spec asks for.
	if signature := dbus.SignatureOf(*icon).String(); signature != "(iiibiiay)" {
		t.Fatal(signature)
	}
}

// fakeService is a notification service on a private bus.
type fakeService struct {
	conn  *dbus.Conn
	shown chan []any
}

func (f *fakeService) GetCapabilities() ([]string, *dbus.Error) {
	return []string{"actions", "body"}, nil
}

func (f *fakeService) Notify(app string, replaces uint32, icon, summary, body string, actions []string,
	hints map[string]dbus.Variant, timeout int32) (uint32, *dbus.Error) {
	f.shown <- []any{app, summary, body, actions}
	return 7, nil
}

func TestShowsAndOpensOnClick(t *testing.T) {
	daemon, err := exec.LookPath("dbus-daemon")
	if err != nil {
		t.Skip("no dbus-daemon")
	}
	command := exec.Command(daemon, "--session", "--nofork", "--print-address=1")
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	address, err := bufio.NewReader(output).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", strings.TrimSpace(address))
	t.Setenv("DISPLAY", ":0")
	t.Setenv("SSH_CONNECTION", "")

	conn, err := dbus.Connect(strings.TrimSpace(address))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fake := &fakeService{conn: conn, shown: make(chan []any, 1)}
	if err := conn.Export(fake, path, service); err != nil {
		t.Fatal(err)
	}
	if reply, err := conn.RequestName(service, dbus.NameFlagDoNotQueue); err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		t.Fatal(reply, err)
	}

	opened := make(chan string, 1)
	notifier, err := New(func(url string) { opened <- url })
	if err != nil {
		t.Fatal(err)
	}
	defer notifier.Close()
	if err := notifier.Show(Notice{Title: "Claude Code · atlas", Body: "“Plot QC” finished", URL: "vscode://x"}); err != nil {
		t.Fatal(err)
	}
	got := <-fake.shown
	want := []any{"code flower", "Claude Code · atlas", "“Plot QC” finished", []string{"default", "Open in VS Code"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("shown %#v", got)
	}
	// A click on another notice opens nothing; on this one, its URL.
	if err := conn.Emit(path, service+".ActionInvoked", uint32(8), "default"); err != nil {
		t.Fatal(err)
	}
	if err := conn.Emit(path, service+".ActionInvoked", uint32(7), "default"); err != nil {
		t.Fatal(err)
	}
	select {
	case url := <-opened:
		if url != "vscode://x" {
			t.Fatal(url)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the click opened nothing")
	}
}
