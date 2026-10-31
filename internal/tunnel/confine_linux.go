//go:build linux

package tunnel

import (
	"net/url"
	"strconv"

	"github.com/landlock-lsm/go-landlock/landlock"
)

// Confine limits this process (the hardened connector) with Landlock: files
// only in the tunnel's directory, the TLS certificates and the resolver's
// configuration, all else unreadable (~/.ssh among them); TCP connections only
// to the tunnel's port. It needs Linux 6.7 or later (Landlock ABI 4).
func Confine(configDirectory, tunnelURL string) error {
	port := uint16(443)
	if parsed, err := url.Parse(tunnelURL); err == nil {
		if parsed.Port() != "" {
			if value, err := strconv.ParseUint(parsed.Port(), 10, 16); err == nil {
				port = uint16(value)
			}
		} else if parsed.Scheme == "http" {
			port = 80
		}
	}
	return landlock.V4.Restrict(
		landlock.RWDirs(Directory(configDirectory)),
		landlock.RODirs("/etc/ssl", "/etc/pki", "/etc/ca-certificates", "/usr/share/ca-certificates", "/usr/local/share/ca-certificates").IgnoreIfMissing(),
		landlock.ROFiles("/etc/resolv.conf", "/etc/hosts", "/etc/nsswitch.conf", "/etc/gai.conf").IgnoreIfMissing(),
		landlock.ConnectTCP(port),
	)
}
