// Package config loads and validates the niclane YAML configuration.
package config

import (
	"fmt"
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
)

// Config is the top-level configuration file.
type Config struct {
	Admin    string        `yaml:"admin"`
	LogLevel string        `yaml:"log_level"`
	Lanes    []*LaneConfig `yaml:"lanes"`
}

// LaneConfig describes one proxy lane: a local listener whose egress is
// pinned to one network interface.
type LaneConfig struct {
	Name        string      `yaml:"name"`
	Listen      string      `yaml:"listen"`
	Type        string      `yaml:"type"`
	Interface   string      `yaml:"interface"`
	BindIP      string      `yaml:"bind_ip"`
	BindMode    string      `yaml:"bind_mode"`
	Strict      *bool       `yaml:"strict"`
	Auth        *AuthConfig `yaml:"auth"`
	Upstream    string      `yaml:"upstream"`
	DNS         string      `yaml:"dns"`
	DNSServers  []string    `yaml:"dns_servers"`
	DohURLs     []string    `yaml:"doh"`
	IdleTimeout string      `yaml:"idle_timeout"`
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
		if _, err := ln.IdleTimeoutDur(); err != nil {
			return err
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
