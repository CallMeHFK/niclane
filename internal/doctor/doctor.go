// Package doctor inspects the host's interfaces and binding capabilities and
// prints a suggested niclane configuration.
package doctor

import (
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/CallMeHFK/niclane/internal/lane"
)

// Run prints the environment report to w.
func Run(w io.Writer) error {
	fmt.Fprintln(w, "niclane doctor")
	fmt.Fprintln(w, "=============")

	capable := lane.DeviceBindSupported()
	if capable {
		fmt.Fprintln(w, "device binding (SO_BINDTODEVICE / IP_BOUND_IF): AVAILABLE")
		fmt.Fprintln(w, "  -> device-mode lanes get true per-NIC isolation")
	} else {
		fmt.Fprintln(w, "device binding (SO_BINDTODEVICE / IP_BOUND_IF): NOT AVAILABLE")
		fmt.Fprintln(w, "  -> lanes fall back to source-IP binding (best-effort isolation, may leak on route changes)")
		fmt.Fprintln(w, "  -> on Linux run niclane with CAP_NET_ADMIN, e.g.:")
		fmt.Fprintln(w, "       sudo setcap cap_net_admin+ep $(command -v niclane)")
		fmt.Fprintln(w, "     or run under systemd with AmbientCapabilities=CAP_NET_ADMIN")
	}
	fmt.Fprintln(w)

	ifaces, err := net.Interfaces()
	if err != nil {
		return err
	}
	names := make([]string, 0, len(ifaces))
	byName := map[string]*net.Interface{}
	for i := range ifaces {
		ife := ifaces[i]
		ife.Addrs() // prefetch errors are handled below
		names = append(names, ife.Name)
		byName[ife.Name] = &ifaces[i]
	}
	sort.Strings(names)

	fmt.Fprintf(w, "%-24s %-6s %-8s %s\n", "INTERFACE", "STATE", "FAMILY", "ADDRESS")
	for _, name := range names {
		ife := byName[name]
		state := "down"
		if ife.Flags&net.FlagUp != 0 {
			state = "UP"
		}
		addrs, err := ife.Addrs()
		if err != nil {
			fmt.Fprintf(w, "%-24s %-6s %-8s %s\n", name, state, "-", err.Error())
			continue
		}
		var v4s, v6s []string
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipn.IP
			s := ip.String()
			if ip.To4() != nil {
				if ip.IsLoopback() {
					continue
				}
				v4s = append(v4s, s)
			} else if !ip.IsLinkLocalUnicast() && !ip.IsLoopback() {
				v6s = append(v6s, strings.SplitN(s, "%", 2)[0])
			}
		}
		first := true
		emit := func(family string, list []string) {
			if len(list) == 0 {
				return
			}
			for _, a := range list {
				blank := ""
				if first {
					blank = name
				}
				fmt.Fprintf(w, "%-24s %-6s %-8s %s\n", blank, state, family, a)
				first = false
			}
		}
		emit("IPv4", v4s)
		emit("IPv6", v6s)
		if first {
			fmt.Fprintf(w, "%-24s %-6s %-8s %s\n", name, state, "-", "(no global address)")
			first = false
		}
	}
	fmt.Fprintln(w)

	defaultIface := defaultEgress()
	if defaultIface != "" {
		fmt.Fprintf(w, "current default egress: %s\n", defaultIface)
	} else {
		fmt.Fprintln(w, "current default egress: unknown (offline or no route)")
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, "suggested config (edit freely, one lane per NIC):")
	n := 0
	for _, name := range names {
		ife := byName[name]
		if ife.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := ife.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil || ipn.IP.IsLoopback() || ipn.IP.IsLinkLocalUnicast() {
				continue
			}
			n++
			fmt.Fprintf(w, "  - name: lane%d\n", n)
			fmt.Fprintf(w, "    listen: 127.0.0.1:%d\n", 7890+n)
			fmt.Fprintf(w, "    type: socks5\n")
			if capable {
				fmt.Fprintf(w, "    interface: %s\n", name)
			} else {
				fmt.Fprintf(w, "    bind_ip: %s\n", ipn.IP.String())
			}
			break
		}
	}
	return nil
}

// defaultEgress reports the interface a default-route dial would leave from.
// Offline hosts yield "".
func defaultEgress() string {
	conn, err := net.DialTimeout("udp", "8.8.8.8:80", 2*time.Second)
	if err != nil {
		return ""
	}
	defer conn.Close()
	local, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return ""
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for i := range ifaces {
		addrs, err := ifaces[i].Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.Equal(local.IP) {
				return ifaces[i].Name
			}
		}
	}
	return ""
}
