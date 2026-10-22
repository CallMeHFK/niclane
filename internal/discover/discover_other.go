//go:build !linux

package discover

import (
	"context"
	"net"
)

// Supported reports whether automatic discovery is available.
func Supported() bool { return false }

func interfaceNames([]string) ([]string, error) { return nil, ErrUnsupported }

func gatewayFor(context.Context, string, map[string]string) (net.IP, error) {
	return nil, ErrUnsupported
}
