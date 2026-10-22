// Package config loads and validates the niclane YAML configuration.
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	BindModeDevice = "device" // SO_BINDTODEVICE / IP_BOUND_IF
	BindModeIP     = "ip"     // bind to source address only (weaker)
	BindModeAuto   = "auto"   // prefer device, fall back to source IP

	TypeSocks5 = "socks5"
	TypeHTTP   = "http"

	DNSLane   = "lane" // resolve through the lane's own interface
	DNSSystem = "system"
	DNSDoH    = "doh" // DNS-over-HTTPS through the lane

	// StaticCandidatePriority is the default priority of a candidate listed
	// in the config; auto-discovered candidates sort after them.
	StaticCandidatePriority     = 1000
	DiscoveredCandidatePriority = 100

	DefaultProbeURL      = "https://www.gstatic.com/generate_204"
	DefaultProbeInterval = 10 * time.Second
)

// DefaultDiscoveryPorts are the proxy ports probed on a tether gateway when
// neither the lane nor the global discovery block lists any.
var DefaultDiscoveryPorts = []int{10808, 10809, 7890, 7891}

// Config is the top-level configuration file.
type Config struct {
	Admin     string           `yaml:"admin"`
	LogLevel  string           `yaml:"log_level"`
	Discovery *DiscoveryConfig `yaml:"discovery"`
	Lanes     []*LaneConfig    `yaml:"lanes"`
}

// DiscoveryConfig holds global defaults shared by every lane's discover block.
type DiscoveryConfig struct {
	Ports []int `yaml:"ports"`
}

// CandidateConfig is one statically configured egress candidate.
type CandidateConfig struct {
	Interface string `yaml:"interface"`
	Upstream  string `yaml:"upstream"`
	Priority  *int   `yaml:"priority"`
}

// DiscoverConfig enables automatic candidate discovery on a lane.
type DiscoverConfig struct {
	Interfaces []string          `yaml:"interfaces"`
	Ports      []int             `yaml:"ports"`
	Via        map[string]string `yaml:"via"`
	ProbeURL   string            `yaml:"probe_url"`
	Interval   string            `yaml:"interval"`
}

// LaneConfig describes one proxy lane: a local listener whose egress is
// pinned to one network interface.
type LaneConfig struct {
	Name        string             `yaml:"name"`
	Listen      string             `yaml:"listen"`
	Type        string             `yaml:"type"`
	Interface   string             `yaml:"interface"`
	BindIP      string             `yaml:"bind_ip"`
	BindMode    string             `yaml:"bind_mode"`
	Strict      *bool              `yaml:"strict"`
	Auth        *AuthConfig        `yaml:"auth"`
	Upstream    string             `yaml:"upstream"`
	Candidates  []*CandidateConfig `yaml:"candidates"`
	Discover    *DiscoverConfig    `yaml:"discover"`
	DNS         string             `yaml:"dns"`
	DNSServers  []string           `yaml:"dns_servers"`
	DohURLs     []string           `yaml:"doh"`
	IdleTimeout string             `yaml:"idle_timeout"`
}

// HasCandidates reports whether the lane runs in candidate mode.
func (c *LaneConfig) HasCandidates() bool {
	return len(c.Candidates) > 0 || c.Discover != nil
}

// EffectivePriority returns the candidate priority, defaulting to the static
// default.
func (c *CandidateConfig) EffectivePriority() int {
	if c.Priority == nil {
		return StaticCandidatePriority
	}
	return *c.Priority
}

// EffectiveInterfaces returns the interface selectors, defaulting to device.
func (d *DiscoverConfig) EffectiveInterfaces() []string {
	if d == nil || len(d.Interfaces) == 0 {
		return []string{"device"}
	}
	return d.Interfaces
}

// EffectiveProbeURL returns probe_url or the default.
func (d *DiscoverConfig) EffectiveProbeURL() string {
	if d == nil || d.ProbeURL == "" {
		return DefaultProbeURL
	}
	return d.ProbeURL
}

// EffectiveInterval returns the probe/discovery period, defaulting to 10s.
func (d *DiscoverConfig) EffectiveInterval() (time.Duration, error) {
	if d == nil || d.Interval == "" {
		return DefaultProbeInterval, nil
	}
	dur, err := time.ParseDuration(d.Interval)
	if err != nil || dur < 2*time.Second {
		return 0, fmt.Errorf("discover: interval %q must be a duration of at least 2s", d.Interval)
	}
	return dur, nil
}

// EffectivePorts returns lane ports when set, else global ports, else the
// built-in defaults. The returned slice is deduped and never aliases config.
func (d *DiscoverConfig) EffectivePorts(global *DiscoveryConfig) []int {
	var ports []int
	switch {
	case d != nil && len(d.Ports) > 0:
		ports = d.Ports
	case global != nil && len(global.Ports) > 0:
		ports = global.Ports
	default:
		ports = DefaultDiscoveryPorts
	}
	seen := make(map[int]bool, len(ports))
	out := make([]int, 0, len(ports))
	for _, p := range ports {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// AuthConfig is the SOCKS5 username/password auth for inbound clients.
type AuthConfig struct {
	User     string `yaml:"user"`
	Password string `yaml:"password"`
}

// Strict returns the fail-closed flag; defaults to true.
func (c *LaneConfig) StrictEnabled() bool {
	return c.Strict == nil || *c.Strict
}

// TypeOrDefault returns the proxy type, defaulting to socks5.
func (c *LaneConfig) TypeOrDefault() string {
	if c.Type == "" {
		return TypeSocks5
	}
	return c.Type
}

// DNSMode returns the DNS mode: resolution goes through the lane interface
// whenever one is configured, unless explicitly set to "system" or "doh".
func (c *LaneConfig) DNSMode() string {
	if c.DNS == "" {
		if c.Interface != "" || c.BindIP != "" {
			return DNSLane
		}
		return DNSSystem
	}
	return c.DNS
}

// DefaultDoHURLs are the JSON-API DoH endpoints used when dns: doh is set
// without an explicit doh list. Both are IP-literal URLs, so the DoH query
// needs no bootstrap resolution. `http://` endpoints work for tests and
// self-hosted resolvers but should not be used in production.
var DefaultDoHURLs = []string{
	"https://223.5.5.5/resolve",
	"https://1.1.1.1/dns-query",
}

// EffectiveDoHURLs returns the configured DoH endpoints or the defaults.
func (c *LaneConfig) EffectiveDoHURLs() []string {
	if len(c.DohURLs) > 0 {
		return c.DohURLs
	}
	return DefaultDoHURLs
}

// IdleTimeoutDur parses idle_timeout; 0 means no idle timeout.
func (c *LaneConfig) IdleTimeoutDur() (time.Duration, error) {
	if c.IdleTimeout == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(c.IdleTimeout)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("lane %q: invalid idle_timeout %q", c.Name, c.IdleTimeout)
	}
	return d, nil
}

// Load reads and validates a configuration file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Validate checks cross-field constraints and fills the log_level default.
func (c *Config) Validate() error {
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.Discovery != nil {
		if err := validatePorts("discovery", c.Discovery.Ports); err != nil {
			return err
		}
	}
	seenName := map[string]bool{}
	seenListen := map[string]bool{}
	for i, ln := range c.Lanes {
		if ln == nil {
			return fmt.Errorf("lanes[%d]: empty lane", i)
		}
		if ln.Name == "" {
			return fmt.Errorf("lanes[%d]: name is required", i)
		}
		if seenName[ln.Name] {
			return fmt.Errorf("lane %q: duplicate lane name", ln.Name)
		}
		seenName[ln.Name] = true

		if ln.Listen == "" {
			return fmt.Errorf("lane %q: listen is required", ln.Name)
		}
		if seenListen[ln.Listen] {
			return fmt.Errorf("lane %q: duplicate listen address %q", ln.Name, ln.Listen)
		}
		seenListen[ln.Listen] = true

		switch ln.TypeOrDefault() {
		case TypeSocks5, TypeHTTP:
		default:
			return fmt.Errorf("lane %q: unknown type %q (want socks5 or http)", ln.Name, ln.Type)
		}

		if ln.Interface != "" && ln.BindIP != "" {
			return fmt.Errorf("lane %q: interface and bind_ip are mutually exclusive", ln.Name)
		}
		switch ln.BindMode {
		case "", BindModeDevice, BindModeIP, BindModeAuto:
		default:
			return fmt.Errorf("lane %q: unknown bind_mode %q (want device, ip or auto)", ln.Name, ln.BindMode)
		}
		if ln.BindMode == BindModeDevice && ln.Interface == "" {
			return fmt.Errorf("lane %q: bind_mode=device requires interface", ln.Name)
		}

		switch ln.DNS {
		case "", DNSLane, DNSSystem, DNSDoH:
		default:
			return fmt.Errorf("lane %q: unknown dns %q (want lane, system or doh)", ln.Name, ln.DNS)
		}
		for _, u := range ln.DohURLs {
			if sch := schemeOf(u); sch != "https" && sch != "http" {
				return fmt.Errorf("lane %q: doh URL %q must be an http(s) URL", ln.Name, u)
			}
		}

		if ln.Auth != nil && ln.TypeOrDefault() == TypeHTTP {
			return fmt.Errorf("lane %q: auth is only supported for socks5 lanes", ln.Name)
		}
		if ln.Upstream != "" {
			switch schemeOf(ln.Upstream) {
			case "socks5", "http":
			default:
				return fmt.Errorf("lane %q: upstream must be socks5:// or http://", ln.Name)
			}
		}
		if ln.HasCandidates() {
			if ln.Interface != "" || ln.Upstream != "" {
				return fmt.Errorf("lane %q: interface/upstream cannot be combined with candidates or discover", ln.Name)
			}
			seenCand := map[string]bool{}
			for j, cand := range ln.Candidates {
				if cand == nil {
					return fmt.Errorf("lane %q: candidates[%d]: empty candidate", ln.Name, j)
				}
				if cand.Interface == "" && cand.Upstream == "" {
					return fmt.Errorf("lane %q: candidates[%d]: interface or upstream is required", ln.Name, j)
				}
				if cand.Upstream != "" {
					switch schemeOf(cand.Upstream) {
					case "socks5", "http":
					default:
						return fmt.Errorf("lane %q: candidates[%d]: upstream must be socks5:// or http://", ln.Name, j)
					}
				}
				key := cand.Interface + "\x00" + cand.Upstream
				if seenCand[key] {
					return fmt.Errorf("lane %q: candidates[%d]: duplicate candidate %s %s", ln.Name, j, cand.Interface, cand.Upstream)
				}
				seenCand[key] = true
			}
			if ln.Discover != nil {
				if _, err := ln.Discover.EffectiveInterval(); err != nil {
					return fmt.Errorf("lane %q: %w", ln.Name, err)
				}
				if err := validatePorts(fmt.Sprintf("lane %q discover", ln.Name), ln.Discover.EffectivePorts(c.Discovery)); err != nil {
					return err
				}
				for ifName, gw := range ln.Discover.Via {
					if ifName == "" {
						return fmt.Errorf("lane %q: discover.via: empty interface name", ln.Name)
					}
					if net.ParseIP(gw) == nil {
						return fmt.Errorf("lane %q: discover.via[%s]: %q is not an IP address", ln.Name, ifName, gw)
					}
				}
				for _, spec := range ln.Discover.Interfaces {
					if spec == "" {
						return fmt.Errorf("lane %q: discover.interfaces contains an empty entry", ln.Name)
					}
				}
				if err := validateProbeURL(ln.Discover.EffectiveProbeURL()); err != nil {
					return fmt.Errorf("lane %q: %w", ln.Name, err)
				}
			}
		}
		if _, err := ln.IdleTimeoutDur(); err != nil {
			return err
		}
	}
	return nil
}

func validatePorts(what string, ports []int) error {
	for _, p := range ports {
		if p < 1 || p > 65535 {
			return fmt.Errorf("%s: port %d out of range 1..65535", what, p)
		}
	}
	return nil
}

// validateProbeURL keeps the probe target out of local/management address
// space so a misconfigured discovery rule cannot poke the host itself.
func validateProbeURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return fmt.Errorf("discover: probe_url %q must be an absolute http(s) URL", raw)
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
			return fmt.Errorf("discover: probe_url host %q is not allowed", u.Hostname())
		}
	}
	return nil
}

func schemeOf(raw string) string {
	for i := 0; i < len(raw); i++ {
		if raw[i] == ':' {
			if i+2 < len(raw) && raw[i+1] == '/' && raw[i+2] == '/' {
				return raw[:i]
			}
			return ""
		}
	}
	return ""
}
