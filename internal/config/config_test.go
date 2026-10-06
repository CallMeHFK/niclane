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
    bind_ip: 192.168.1.100
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
