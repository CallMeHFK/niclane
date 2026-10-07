//go:build darwin || windows

package lane

// isIPv6Network selects the socket option family for per-interface binding
// on platforms whose API differs between IPv4 and IPv6 (IP_BOUND_IF vs
// IPV6_BOUND_IF, IP_UNICAST_IF vs IPV6_UNICAST_IF).
func isIPv6Network(network string) bool {
	return network == "tcp6" || network == "udp6"
}
