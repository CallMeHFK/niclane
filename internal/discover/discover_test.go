package discover

import (
	"strings"
	"testing"
)

const procRouteFixture = `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
enx020000000005	00000000	016433C6	0003	0	0	100	00000000	0	0	0
enx020000000004	00000000	0A6433C6	0003	0	0	102	00000000	0	0	0
wlp130s0	0000000A	FEED9F0A	0003	0	0	600	000000FF	0	0	0
`

func TestParseProcRoute(t *testing.T) {
	ip, ok := parseProcRoute(strings.NewReader(procRouteFixture), "enx020000000004")
	if !ok || ip.String() != "198.51.100.10" {
		t.Fatalf("gateway = %v %v, want 198.51.100.10", ip, ok)
	}
	if _, ok := parseProcRoute(strings.NewReader(procRouteFixture), "wlp130s0"); ok {
		t.Fatal("a non-default route must not be treated as a gateway")
	}
	if _, ok := parseProcRoute(strings.NewReader(procRouteFixture), "missing0"); ok {
		t.Fatal("unknown interface must yield no gateway")
	}
}

func TestParseRouteHex(t *testing.T) {
	ip, ok := parseRouteHex("146433C6")
	if !ok || ip.String() != "198.51.100.20" {
		t.Fatalf("route hex = %v %v", ip, ok)
	}
	if _, ok := parseRouteHex("xyz"); ok {
		t.Fatal("bad hex must fail")
	}
}

func TestParseLease(t *testing.T) {
	ip, ok := parseLease(strings.NewReader("ADDRESS=198.51.100.11\nROUTER=198.51.100.10\n"))
	if !ok || ip.String() != "198.51.100.10" {
		t.Fatalf("lease router = %v %v", ip, ok)
	}
}

func TestParseNmcliGateway(t *testing.T) {
	ip, ok := parseNmcliGateway("IP4.GATEWAY:198.51.100.10\n")
	if !ok || ip.String() != "198.51.100.10" {
		t.Fatalf("nmcli gateway = %v %v", ip, ok)
	}
	if _, ok := parseNmcliGateway("IP4.GATEWAY:\n"); ok {
		t.Fatal("empty gateway must fail")
	}
}

func TestParseNmcliDhcpRouters(t *testing.T) {
	// Terse `nmcli -t -f DHCP4.OPTION device show <if>` output for a link that
	// holds a DHCP lease but installs no default route (ipv4.never-default=yes):
	// IP4.GATEWAY is empty there, yet the lease still carries the gateway.
	lease := `DHCP4.OPTION[5]:domain_name_servers = 198.51.100.53
DHCP4.OPTION[8]:ip_address = 198.51.100.86
DHCP4.OPTION[22]:requested_routers = 1
DHCP4.OPTION[27]:routers = 198.51.100.10
DHCP4.OPTION[28]:subnet_mask = 255.255.255.0
`
	ip, ok := parseNmcliDhcpRouters(lease)
	if !ok || ip.String() != "198.51.100.10" {
		t.Fatalf("dhcp routers = %v %v, want 198.51.100.10", ip, ok)
	}

	// requested_routers is a different option and must not be mistaken for it.
	if _, ok := parseNmcliDhcpRouters("DHCP4.OPTION[22]:requested_routers = 1\n"); ok {
		t.Fatal("requested_routers must not be treated as the gateway")
	}
	// A link with no DHCP lease at all.
	if _, ok := parseNmcliDhcpRouters(""); ok {
		t.Fatal("empty nmcli output must yield no gateway")
	}
	// DHCP option 3 may list several routers; the first is the default gateway.
	if ip, ok := parseNmcliDhcpRouters("DHCP4.OPTION[27]:routers = 198.51.100.10 198.51.100.11\n"); !ok || ip.String() != "198.51.100.10" {
		t.Fatalf("multi-router lease = %v %v, want 198.51.100.10", ip, ok)
	}
}

func TestMatchInterfaceName(t *testing.T) {
	cases := []struct {
		name      string
		hasDevice bool
		spec      []string
		want      bool
	}{
		{"lo", true, []string{"any"}, false},
		{"enx1", true, []string{"device"}, true},
		{"docker0", false, []string{"device"}, false},
		{"docker0", false, []string{"any"}, true},
		{"enx1", false, []string{"enx*"}, true},
		{"wlp130s0", true, []string{"enx*"}, false},
	}
	for _, c := range cases {
		if got := matchInterfaceName(c.name, c.hasDevice, c.spec); got != c.want {
			t.Fatalf("matchInterfaceName(%q, %v, %v) = %v, want %v", c.name, c.hasDevice, c.spec, got, c.want)
		}
	}
}
