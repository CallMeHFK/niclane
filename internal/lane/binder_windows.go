//go:build windows

package lane

import (
	"encoding/binary"
	"fmt"
	"syscall"
)

// IP_UNICAST_IF / IPV6_UNICAST_IF pin unicast egress to an interface index.
// Unlike SO_BINDTODEVICE they do not require privileges on Windows, at the
// cost of not constraining the receive path (documented best-effort).
const (
	ipUnicastIf   = 31
	ipv6UnicastIf = 31
)

// deviceControl returns a Control hook that binds every outbound socket to
// the given interface via IP_UNICAST_IF (v4) / IPV6_UNICAST_IF (v6).
func deviceControl(ifName string, ifIndex int) controlFunc {
	return func(network, address string, conn syscall.RawConn) error {
		var serr error
		err := conn.Control(func(fd uintptr) {
			if isIPv6Network(network) {
				// IPV6_UNICAST_IF takes the index in host byte order.
				serr = syscall.SetsockoptInt(syscall.Handle(fd), syscall.IPPROTO_IPV6, ipv6UnicastIf, ifIndex)
				return
			}
			// IP_UNICAST_IF takes the index in NETWORK byte order (htonl).
			var b [4]byte
			binary.BigEndian.PutUint32(b[:], uint32(ifIndex))
			v := int(binary.LittleEndian.Uint32(b[:]))
			serr = syscall.SetsockoptInt(syscall.Handle(fd), syscall.IPPROTO_IP, ipUnicastIf, v)
		})
		if err != nil {
			return err
		}
		if serr != nil {
			return fmt.Errorf("IP_UNICAST_IF(%s#%d): %w", ifName, ifIndex, serr)
		}
		return nil
	}
}

// deviceBindSupported probes whether IP_UNICAST_IF is usable (interface
// index 1 is the loopback interface on every Windows installation).
func deviceBindSupported() bool {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(syscall.Handle(fd))
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], 1)
	return syscall.SetsockoptInt(fd, syscall.IPPROTO_IP, ipUnicastIf, int(binary.LittleEndian.Uint32(b[:]))) == nil
}
