//go:build !linux && !darwin

package lane

// deviceControl is unavailable on this platform: device binding degrades to
// source-IP binding (handled in lane.go). We still report it as unsupported
// so doctor and the lane status say so honestly.
func deviceControl(ifName string, ifIndex int) controlFunc { return nil }

// deviceBindSupported reports that device-level binding is not available.
func deviceBindSupported() bool { return false }
