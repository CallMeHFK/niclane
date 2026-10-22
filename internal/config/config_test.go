package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "niclane.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const sample = `
admin: 127.0.0.1:9000
log_level: debug
lanes:
  - name: wifi
    listen: 127.0.0.1:7891
    interface: wlan0
  - name: dock
    listen: 127.0.0.1:7892
    type: http
    bind_ip: 198.51.100.100
    strict: false
    upstream: socks5://u:p@10.0.0.1:1080
    idle_timeout: 5m
`

func TestLoadSample(t *testing.T) {
	cfg, err := Load(write(t, sample))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Admin != "127.0.0.1:9000" || cfg.LogLevel != "debug" {
		t.Fatalf("unexpected top level: %+v", cfg)
	}
	if len(cfg.Lanes) != 2 {
		t.Fatalf("want 2 lanes, got %d", len(cfg.Lanes))
	}
	a, b := cfg.Lanes[0], cfg.Lanes[1]
	if a.TypeOrDefault() != TypeSocks5 || !a.StrictEnabled() || a.DNSMode() != DNSLane {
		t.Fatalf("lane a defaults wrong: %+v", a)
	}
	if b.StrictEnabled() {
		t.Fatal("lane b should not be strict")
	}
	if b.DNSMode() != DNSLane {
		t.Fatalf("lane b dns = %q, want lane", b.DNSMode())
	}
	if d, _ := b.IdleTimeoutDur(); d.String() != "5m0s" {
		t.Fatalf("lane b idle = %v", d)
	}
	if schemeOf(b.Upstream) != "socks5" {
		t.Fatalf("upstream scheme = %q", schemeOf(b.Upstream))
	}
}

func TestValidateErrors(t *testing.T) {
	cases := map[string]string{
		"duplicate name": `
lanes:
  - {name: a, listen: 127.0.0.1:1, interface: wlan0}
  - {name: a, listen: 127.0.0.1:2, interface: wlan0}`,
		"duplicate listen": `
lanes:
  - {name: a, listen: 127.0.0.1:1, interface: wlan0}
  - {name: b, listen: 127.0.0.1:1, interface: wlan0}`,
		"bad type": `
lanes:
  - {name: a, listen: 127.0.0.1:1, type: shadowsocks}`,
		"interface and bind_ip": `
lanes:
  - {name: a, listen: 127.0.0.1:1, interface: wlan0, bind_ip: 10.0.0.1}`,
		"device without interface": `
lanes:
  - {name: a, listen: 127.0.0.1:1, bind_mode: device}`,
		"bad upstream scheme": `
lanes:
  - {name: a, listen: 127.0.0.1:1, upstream: vmess://x}`,
		"auth on http": `
lanes:
  - {name: a, listen: 127.0.0.1:1, type: http, auth: {user: u, password: p}}`,
		"missing listen": `
lanes:
  - {name: a, interface: wlan0}`,
		"bad idle_timeout": `
lanes:
  - {name: a, listen: 127.0.0.1:1, idle_timeout: fast}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(write(t, body)); err == nil {
				t.Fatalf("expected error for case %q", name)
			} else if !strings.Contains(err.Error(), "") {
				t.Fatal("unreachable")
			}
		})
	}
}

func TestDNSModeDefault(t *testing.T) {
	cfg, err := Load(write(t, `
lanes:
  - {name: plain, listen: 127.0.0.1:1}
  - {name: bound, listen: 127.0.0.1:2, interface: wlan0}
  - {name: sys, listen: 127.0.0.1:3, interface: wlan0, dns: system}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Lanes[0].DNSMode() != DNSSystem {
		t.Fatalf("unbound lane dns = %q, want system", cfg.Lanes[0].DNSMode())
	}
	if cfg.Lanes[1].DNSMode() != DNSLane {
		t.Fatalf("bound lane dns = %q, want lane", cfg.Lanes[1].DNSMode())
	}
	if cfg.Lanes[2].DNSMode() != DNSSystem {
		t.Fatalf("explicit system lane dns = %q", cfg.Lanes[2].DNSMode())
	}
}

func TestCandidateConfigDefaults(t *testing.T) {
	cfg, err := Load(write(t, `
discovery:
  ports: [10808, 10808, 7890]
lanes:
  - name: internet
    listen: 127.0.0.1:7893
    candidates:
      - interface: enx111111111111
        upstream: http://198.51.100.10:10808
      - upstream: socks5://10.0.0.1:1080
        priority: 5
    discover:
      interfaces: [device, "enx*"]
      via: {enx111111111111: 198.51.100.10}
`))
	if err != nil {
		t.Fatal(err)
	}
	lc := cfg.Lanes[0]
	if !lc.HasCandidates() {
		t.Fatal("lane should be in candidate mode")
	}
	if got := lc.Candidates[0].EffectivePriority(); got != StaticCandidatePriority {
		t.Fatalf("default priority = %d", got)
	}
	if got := lc.Candidates[1].EffectivePriority(); got != 5 {
		t.Fatalf("explicit priority = %d", got)
	}
	if got := lc.Discover.EffectiveProbeURL(); got != DefaultProbeURL {
		t.Fatalf("probe url = %q", got)
	}
	if d, err := lc.Discover.EffectiveInterval(); err != nil || d != DefaultProbeInterval {
		t.Fatalf("interval = %v, %v", d, err)
	}
	ports := lc.Discover.EffectivePorts(cfg.Discovery)
	if len(ports) != 2 || ports[0] != 10808 || ports[1] != 7890 {
		t.Fatalf("lanes inherit global ports, got %v", ports)
	}
}

func TestEffectivePortsPrecedence(t *testing.T) {
	global := &DiscoveryConfig{Ports: []int{1, 2, 2}}
	none := &DiscoverConfig{}
	if got := none.EffectivePorts(nil); len(got) != len(DefaultDiscoveryPorts) {
		t.Fatalf("fallback defaults = %v", got)
	}
	if got := none.EffectivePorts(global); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("global ports = %v", got)
	}
	own := &DiscoverConfig{Ports: []int{9}}
	if got := own.EffectivePorts(global); len(got) != 1 || got[0] != 9 {
		t.Fatalf("lane override = %v", got)
	}
}

func TestCandidateValidationErrors(t *testing.T) {
	cases := map[string]string{
		"legacy plus candidates": `
lanes:
  - {name: a, listen: 127.0.0.1:1, interface: wlan0, candidates: [{interface: eth0}]}`,
		"empty candidate": `
lanes:
  - {name: a, listen: 127.0.0.1:1, candidates: [{}]}`,
		"bad candidate upstream": `
lanes:
  - {name: a, listen: 127.0.0.1:1, candidates: [{upstream: vmess://x}]}`,
		"duplicate candidate": `
lanes:
  - {name: a, listen: 127.0.0.1:1, candidates: [{interface: eth0, upstream: http://1.2.3.4:1}, {interface: eth0, upstream: http://1.2.3.4:1}]}`,
		"port out of range": `
discovery: {ports: [70000]}
lanes:
  - {name: a, listen: 127.0.0.1:1, discover: {}}`,
		"bad via": `
lanes:
  - {name: a, listen: 127.0.0.1:1, discover: {via: {eth0: not-an-ip}}}`,
		"loopback probe": `
lanes:
  - {name: a, listen: 127.0.0.1:1, discover: {probe_url: "http://127.0.0.1:9/"}}`,
		"short interval": `
lanes:
  - {name: a, listen: 127.0.0.1:1, discover: {interval: 1s}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(write(t, body)); err == nil {
				t.Fatalf("expected error for case %q", name)
			}
		})
	}
}
