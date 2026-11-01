package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// localtimeLink is where Linux and macOS link the host's zone file.
var localtimeLink = "/etc/localtime"

// HostTimeZone is the host's zone as a TZ value for the sandbox, which has no
// zone of its own and would otherwise run in UTC: the host's TZ, else the zone
// name /etc/localtime links to (Europe/Helsinki), else the current offset as a
// POSIX zone (<+02>-2: right now, without daylight-saving changes), else "".
func HostTimeZone() string {
	if zone := os.Getenv("TZ"); zone != "" {
		return zone
	}
	if target, err := filepath.EvalSymlinks(localtimeLink); err == nil {
		if name := zoneName(target); name != "" {
			return name
		}
	}
	return fixedZone(time.Now())
}

// zoneName is the IANA name in a zone file's path, or "".
func zoneName(path string) string {
	path = filepath.ToSlash(path)
	index := strings.LastIndex(path, "/zoneinfo/")
	if index < 0 {
		return ""
	}
	name := path[index+len("/zoneinfo/"):]
	// posix/ and right/ are copies of the database with other leap-second rules.
	name = strings.TrimPrefix(strings.TrimPrefix(name, "posix/"), "right/")
	if name == "" || name == "localtime" {
		return ""
	}
	return name
}

// fixedZone is the offset of now as a POSIX zone, whose sign is inverted
// (two hours ahead of UTC is -2), or "" at UTC.
func fixedZone(now time.Time) string {
	_, offset := now.Zone()
	if offset == 0 {
		return ""
	}
	sign, posix := "+", "-"
	if offset < 0 {
		sign, posix, offset = "-", "", -offset
	}
	hours, minutes := offset/3600, offset%3600/60
	if minutes == 0 {
		return fmt.Sprintf("<%s%02d>%s%d", sign, hours, posix, hours)
	}
	return fmt.Sprintf("<%s%02d%02d>%s%d:%02d", sign, hours, minutes, posix, hours, minutes)
}
