package sandbox

import (
	"net"
	"os"
	"strconv"
	"strings"
)

func itoa(value int) string {
	return strconv.Itoa(value)
}

// DefaultAdvertiseHost returns this node's first non-loopback IPv4 address,
// or its host name when it has none.
func DefaultAdvertiseHost() string {
	addresses, err := net.InterfaceAddrs()
	if err == nil {
		for _, address := range addresses {
			network, ok := address.(*net.IPNet)
			if !ok || network.IP.IsLoopback() || network.IP.IsLinkLocalUnicast() {
				continue
			}
			if ip := network.IP.To4(); ip != nil {
				return ip.String()
			}
		}
	}
	name, err := os.Hostname()
	if err != nil {
		return "localhost"
	}
	return strings.TrimSuffix(name, ".")
}

// FreePort asks the kernel for an unused TCP port on the loopback interface.
func FreePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}
