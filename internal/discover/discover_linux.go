//go:build linux

package discover

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Supported reports whether automatic discovery is available.
func Supported() bool { return true }

func interfaceNames(spec []string) ([]string, error) {
	entries, err := os.ReadDir("/sys/class/net")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		name := e.Name()
		_, statErr := os.Stat(filepath.Join("/sys/class/net", name, "device"))
		if matchInterfaceName(name, statErr == nil, spec) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// gatewayFor resolves the gateway of one interface, never the host's global
// default route: via override, then the interface's own default route, then
// its systemd-networkd lease, then NetworkManager's effective gateway, then
// NetworkManager's DHCP lease. The last one is what keeps a phone link
// discoverable under ipv4.never-default=yes, where the link installs no
// default route and NM consequently reports an empty IP4.GATEWAY.
func gatewayFor(ctx context.Context, ifName string, via map[string]string) (net.IP, error) {
	if v, ok := via[ifName]; ok {
		if ip := net.ParseIP(v); ip != nil {
			return ip, nil
		}
	}
	if ip, ok := gatewayFromProcRoute(ifName); ok {
		return ip, nil
	}
	if ip, ok := gatewayFromLease(ifName); ok {
		return ip, nil
	}
	if ip, ok := gatewayFromNmcli(ctx, ifName); ok {
		return ip, nil
	}
	if ip, ok := gatewayFromDhcpLease(ctx, ifName); ok {
		return ip, nil
	}
	return nil, fmt.Errorf("no gateway on %s", ifName)
}

func gatewayFromProcRoute(ifName string) (net.IP, bool) {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return nil, false
	}
	defer f.Close()
	return parseProcRoute(f, ifName)
}

func gatewayFromLease(ifName string) (net.IP, bool) {
	raw, err := os.ReadFile(filepath.Join("/sys/class/net", ifName, "ifindex"))
	if err != nil {
		return nil, false
	}
	path := filepath.Join("/run/systemd/netif/leases", strings.TrimSpace(string(raw)))
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	return parseLease(f)
}

func gatewayFromNmcli(ctx context.Context, ifName string) (net.IP, bool) {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "nmcli", "-t", "-f", "IP4.GATEWAY", "device", "show", ifName).Output()
	if err != nil {
		return nil, false
	}
	return parseNmcliGateway(string(out))
}

// gatewayFromDhcpLease reads the gateway NetworkManager learned over DHCP,
// which survives configurations that publish no default route.
func gatewayFromDhcpLease(ctx context.Context, ifName string) (net.IP, bool) {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "nmcli", "-t", "-f", "DHCP4.OPTION", "device", "show", ifName).Output()
	if err != nil {
		return nil, false
	}
	return parseNmcliDhcpRouters(string(out))
}
