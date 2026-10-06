//go:build linux

package lane

import (
	"fmt"
	"syscall"
)

// deviceControl returns a Control hook that binds every outbound socket to
// the given interface name via SO_BINDTODEVICE. On Linux this requires
// CAP_NET_ADMIN (or root); when the capability is missing the first dial
// fails with EPERM and the lane falls back per its bind_mode policy.
func deviceControl(ifName string, ifIndex int) controlFunc {
	return func(network, address string, conn syscall.RawConn) error {
		var serr error
		err := conn.Control(func(fd uintptr) {
			serr = syscall.SetsockoptString(int(fd), syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, ifName+"\x00")
		})
		if err != nil {
			return err
		}
		if serr != nil {
			return fmt.Errorf("SO_BINDTODEVICE(%s): %w", ifName, serr)
		}
		return nil
	}
}

// deviceBindSupported probes whether this process may use SO_BINDTODEVICE.
func deviceBindSupported() bool {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return false
	}
	defer syscall.Close(fd)
	return syscall.SetsockoptString(fd, syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, "lo\x00") == nil
}
