package sandbox

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHostTimeZonePrefersTheHostsTZ(t *testing.T) {
	t.Setenv("TZ", "America/Chicago")
	if zone := HostTimeZone(); zone != "America/Chicago" {
		t.Fatalf("got %q", zone)
	}
	if env := ContainerEnvironment(); env["TZ"] != "America/Chicago" {
		t.Fatalf("TZ = %q in the sandbox", env["TZ"])
	}
}

func TestHostTimeZoneNamesTheZoneLocaltimeLinksTo(t *testing.T) {
	t.Setenv("TZ", "")
	directory := tempDir(t)
	zone := filepath.Join(directory, "usr", "share", "zoneinfo", "Europe", "Helsinki")
	if err := os.MkdirAll(filepath.Dir(zone), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(zone, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "localtime")
	if err := os.Symlink(zone, link); err != nil {
		t.Skip("no symlinks:", err)
	}
	previous := localtimeLink
	localtimeLink = link
	t.Cleanup(func() { localtimeLink = previous })
	if got := HostTimeZone(); got != "Europe/Helsinki" {
		t.Fatalf("got %q", got)
	}
}

func TestZoneNameReadsTheDatabasePath(t *testing.T) {
	for path, want := range map[string]string{
		"/usr/share/zoneinfo/Europe/Helsinki":        "Europe/Helsinki",
		"/var/db/timezone/zoneinfo/America/New_York": "America/New_York",
		"/usr/share/zoneinfo/posix/Asia/Tokyo":       "Asia/Tokyo",
		"/usr/share/zoneinfo/UTC":                    "UTC",
		"/etc/localtime":                             "",
	} {
		if got := zoneName(path); got != want {
			t.Errorf("zoneName(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestFixedZoneInvertsTheSignForPOSIX(t *testing.T) {
	at := func(seconds int) time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.FixedZone("", seconds)) }
	for seconds, want := range map[int]string{
		0:                 "",
		2 * 3600:          "<+02>-2",
		-5 * 3600:         "<-05>5",
		5*3600 + 30*60:    "<+0530>-5:30",
		-(3*3600 + 30*60): "<-0330>3:30",
	} {
		if got := fixedZone(at(seconds)); got != want {
			t.Errorf("fixedZone(%d) = %q, want %q", seconds, got, want)
		}
	}
}
