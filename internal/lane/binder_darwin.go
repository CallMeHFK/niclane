//go:build darwin

package lane

import (
	"fmt"
	"net"
	"syscall"
)

// deviceControl returns a Control hook that binds every outbound socket to
// the given interface via IP_BOUND_IF / IPV6_BOUND_IF. Binding to an
// interface by index does not require privileges on macOS.
func deviceControl(ifName string, ifIndex int) controlFunc {
	return func(network, address string, conn syscall.RawConn) error {
		var serr error
		err := conn.Control(func(fd uintptr) {
			level, opt := syscall.IPPROTO_IP, syscall.IP_BOUND_IF
			if isIPv6Network(network) {
				level, opt = syscall.IPPROTO_IPV6, syscall.IPV6_BOUND_IF
			}
			serr = syscall.SetsockoptInt(int(fd), level, opt, ifIndex)
		})
		if err != nil {
			return err
		}
		if serr != nil {
			return fmt.Errorf("IP_BOUND_IF(%s#%d): %w", ifName, ifIndex, serr)
		}
		return nil
	}
}

// deviceBindSupported probes whether interface binding works on this host.
func deviceBindSupported() bool {
	iface, err := net.Interface("lo0")
	if err != nil {
		return false
	}
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return false
	}
	defer syscall.Close(fd)
	return syscall.SetsockoptInt(fd, syscall.IPPROTO_IP, syscall.IP_BOUND_IF, iface.Index) == nil
}
