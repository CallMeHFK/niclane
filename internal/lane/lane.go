// Package lane implements a proxy egress "lane": outbound sockets pinned to
// one network interface, with fail-closed health monitoring and per-lane DNS.
package lane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/CallMeHFK/niclane/internal/config"
	"github.com/CallMeHFK/niclane/internal/metrics"
)

// DeviceBindSupported reports whether this process may bind sockets to an
// interface (SO_BINDTODEVICE / IP_BOUND_IF).
func DeviceBindSupported() bool { return deviceBindSupported() }

var (
	bindCapOnce   sync.Once
	bindCapCached bool
)

// deviceBindAvailable caches the platform capability probe. Lanes consult it
// so they degrade to source-IP mode instead of reporting healthy while every
// device-bound dial would fail with EPERM.
func deviceBindAvailable() bool {
	bindCapOnce.Do(func() { bindCapCached = deviceBindSupported() })
	return bindCapCached
}

// Refresh forces an immediate health snapshot update.
func (l *Lane) Refresh() { l.refresh() }

// trackedConn decrements the active-connection gauge on close.
type trackedConn struct {
	net.Conn
	once   sync.Once
	active *atomic.Int64
}

func (t *trackedConn) Close() error {
	t.once.Do(func() { t.active.Add(-1) })
	return t.Conn.Close()
}

// ErrFailClosed is returned when the lane's egress interface is unhealthy and
// the lane runs in strict (fail-closed) mode. Connections are rejected, never
// silently routed through another interface.
var ErrFailClosed = errors.New("lane unhealthy (fail-closed)")

// Bind-mode gauge values exposed via metrics.
const (
	BindModeNone   int64 = 0
	BindModeSrcIP  int64 = 1
	BindModeDevice int64 = 2
)

const healthInterval = 2 * time.Second

// controlFunc is the signature of net.Dialer/ListenConfig Control hooks.
type controlFunc func(network, address string, conn syscall.RawConn) error

func isUDPNetwork(network string) bool {
	return network == "udp" || network == "udp4" || network == "udp6"
}

type snapshot struct {
	healthy bool
	lastErr string
	ifIndex int
	src4    net.IP
	src6    net.IP
	control controlFunc // non-nil when device binding is active
	mode    int64       // BindModeNone / BindModeSrcIP / BindModeDevice
}

func (s snapshot) srcFor(ip net.IP) net.IP {
	if ip != nil && ip.To4() != nil {
		return s.src4
	}
	return s.src6
}

// Egress is the interface the SOCKS5/HTTP servers consume.
type Egress interface {
	// DialContext connects to address through the lane. When an upstream
	// proxy is configured it is dialed through the lane and asked to reach
	// address.
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
	// ListenUDP returns a UDP socket bound to the lane's interface.
	ListenUDP(ctx context.Context) (*net.UDPConn, error)
	// Resolve resolves host per the lane's DNS mode (lane/doh/system) and
	// returns a single IP usable with the lane's source addresses.
	Resolve(ctx context.Context, network, host string) (net.IP, error)
	// Reject records a rejected connection.
	Reject(reason string)
	// Stats returns the lane's metrics bucket.
	Stats() *metrics.Stats
	// Name returns the lane name.
	Name() string
}

// Lane is one proxy egress lane.
type Lane struct {
	cfg   *config.LaneConfig
	log   *slog.Logger
	stats *metrics.Stats
	upURL *url.URL // parsed upstream, nil when direct

	mu   sync.RWMutex
	snap snapshot
	doh  *dohClient
}

// New creates a lane from its configuration.
func New(cfg *config.LaneConfig, log *slog.Logger, stats *metrics.Stats) (*Lane, error) {
	if cfg.BindIP != "" && net.ParseIP(cfg.BindIP) == nil {
		return nil, fmt.Errorf("lane %q: invalid bind_ip %q", cfg.Name, cfg.BindIP)
	}
	l := &Lane{cfg: cfg, log: log.With("lane", cfg.Name), stats: stats}
	if cfg.Upstream != "" {
		u, err := url.Parse(cfg.Upstream)
		if err != nil {
			return nil, fmt.Errorf("lane %q: parse upstream: %w", cfg.Name, err)
		}
		l.upURL = u
	}
	l.snap = l.computeSnapshot()
	l.doh = newDoHClient(cfg.EffectiveDoHURLs(), l.dohDial)
	return l, nil
}

// Name returns the lane name.
func (l *Lane) Name() string { return l.cfg.Name }

// Stats returns the lane's metrics bucket.
func (l *Lane) Stats() *metrics.Stats { return l.stats }

// Reject records a rejected connection.
func (l *Lane) Reject(reason string) {
	l.stats.RejectsTotal.Add(1)
	l.log.Debug("connection rejected", "reason", reason)
}

// Config exposes the lane configuration (read-only use).
func (l *Lane) Config() *config.LaneConfig { return l.cfg }

// Current reports the latest health snapshot.
func (l *Lane) Current() (healthy bool, bindMode int64, lastErr string) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.snap.healthy, l.snap.mode, l.snap.lastErr
}

// Run periodically refreshes the lane health snapshot until ctx is done.
func (l *Lane) Run(ctx context.Context) {
	l.refresh()
	t := time.NewTicker(healthInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			l.refresh()
		}
	}
}

func (l *Lane) refresh() {
	s := l.computeSnapshot()
	l.mu.Lock()
	prev := l.snap
	l.snap = s
	l.mu.Unlock()

	l.stats.Healthy.Store(s.healthy)
	l.stats.BindMode.Store(s.mode)
	l.stats.SetLastErr(s.lastErr)
	if prev.healthy != s.healthy || prev.lastErr != s.lastErr {
		if s.healthy {
			l.log.Info("lane healthy",
				"bind_mode", s.mode,
				"src4", ipStr(s.src4), "src6", ipStr(s.src6))
		} else {
			l.log.Warn("lane unhealthy (connections rejected while strict)", "error", s.lastErr)
		}
	}
}

func (l *Lane) computeSnapshot() snapshot {
	cfg := l.cfg
	switch {
	case cfg.Interface != "":
		s := snapshot{mode: BindModeDevice}
		ife, err := net.InterfaceByName(cfg.Interface)
		if err != nil {
			return unhealthy(s, fmt.Sprintf("interface %s: %v", cfg.Interface, err))
		}
		if ife.Flags&net.FlagUp == 0 {
			return unhealthy(s, fmt.Sprintf("interface %s is down", cfg.Interface))
		}
		s.ifIndex = ife.Index
		addrs, err := ife.Addrs()
		if err != nil {
			return unhealthy(s, fmt.Sprintf("interface %s addrs: %v", cfg.Interface, err))
		}
		s.src4, s.src6 = pickSrcAddrs(addrs)
		if s.src4 == nil && s.src6 == nil {
			return unhealthy(s, fmt.Sprintf("interface %s has no usable address", cfg.Interface))
		}
		// Degrade to source-IP binding when device binding is unavailable
		// (missing capability / unsupported platform) or explicitly disabled:
		// otherwise the lane would look healthy and EPERM on every dial.
		if ctrl := deviceControl(cfg.Interface, ife.Index); ctrl != nil && cfg.BindMode != config.BindModeIP && deviceBindAvailable() {
			s.control = ctrl
			s.mode = BindModeDevice
		} else {
			s.control = nil
			s.mode = BindModeSrcIP
		}
		s.healthy = true
		return s

	case cfg.BindIP != "":
		s := snapshot{mode: BindModeSrcIP}
		ip := net.ParseIP(cfg.BindIP)
		ife, ok := interfaceHolding(ip)
		if !ok {
			return unhealthy(s, fmt.Sprintf("no up interface holds bind_ip %s", cfg.BindIP))
		}
		s.ifIndex = ife.Index
		if ip.To4() != nil {
			s.src4 = ip
		} else {
			s.src6 = ip
		}
		s.healthy = true
		return s

	default:
		return snapshot{healthy: true, mode: BindModeNone}
	}
}

func unhealthy(s snapshot, msg string) snapshot {
	s.healthy = false
	s.lastErr = msg
	return s
}

func ipStr(ip net.IP) string {
	if ip == nil {
		return ""
	}
	return ip.String()
}

// pickSrcAddrs selects the first non-link-local (else first usable) IPv4 and
// IPv6 address from an interface address list.
func pickSrcAddrs(addrs []net.Addr) (net.IP, net.IP) {
	var v4, v6, v4Any, v6Any net.IP
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipn.IP
		if ip.IsLoopback() || ip.IsMulticast() {
			continue
		}
		if four := ip.To4(); four != nil {
			if v4Any == nil {
				v4Any = four
			}
			if v4 == nil && !ip.IsLinkLocalUnicast() {
				v4 = four
			}
		} else {
			if v6Any == nil {
				v6Any = ip
			}
			if v6 == nil && !ip.IsLinkLocalUnicast() {
				v6 = ip
			}
		}
	}
	if v4 == nil {
		v4 = v4Any
	}
	if v6 == nil {
		v6 = v6Any
	}
	return v4, v6
}

func interfaceHolding(ip net.IP) (*net.Interface, bool) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, false
	}
	for i := range ifaces {
		ife := ifaces[i]
		if ife.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := ife.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.Equal(ip) {
				return &ife, true
			}
		}
	}
	return nil, false
}

func (l *Lane) current() snapshot {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.snap
}

// ListenUDP returns a UDP socket whose egress is bound to the lane.
func (l *Lane) ListenUDP(ctx context.Context) (*net.UDPConn, error) {
	if l.upURL != nil {
		return nil, errors.New("UDP relay is not supported through an upstream proxy")
	}
	s := l.current()
	if l.cfg.StrictEnabled() && !s.healthy {
		l.stats.RejectsTotal.Add(1)
		return nil, fmt.Errorf("%w: %s", ErrFailClosed, s.lastErr)
	}
	lc := net.ListenConfig{Control: s.control}
	bindAddr := ":0"
	if s.control == nil && (l.cfg.Interface != "" || l.cfg.BindIP != "") {
		src := s.src4
		if src == nil {
			src = s.src6
		}
		if src == nil {
			return nil, errors.New("no source address available for UDP binding")
		}
		bindAddr = net.JoinHostPort(src.String(), "0")
	}
	pc, err := lc.ListenPacket(ctx, "udp", bindAddr)
	if err != nil {
		return nil, fmt.Errorf("listen udp on lane: %w", err)
	}
	uc, ok := pc.(*net.UDPConn)
	if !ok {
		pc.Close()
		return nil, errors.New("listen packet did not return a UDP conn")
	}
	return uc, nil
}

// DialContext connects to address through the lane. When an upstream proxy is
// configured it is dialed through the lane and asked to reach address.
func (l *Lane) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	l.stats.ConnsTotal.Add(1)
	l.stats.ConnsActive.Add(1)
	conn, err := l.proxyDial(ctx, network, address)
	if err != nil {
		l.stats.ConnsActive.Add(-1)
		return nil, err
	}
	return &trackedConn{Conn: conn, active: &l.stats.ConnsActive}, nil
}

func (l *Lane) proxyDial(ctx context.Context, network, address string) (net.Conn, error) {
	if l.upURL == nil {
		return l.dialRaw(ctx, network, address)
	}
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("upstream proxy supports TCP only, got %q", network)
	}
	upAddr := upstreamHostPort(l.upURL)
	conn, err := l.dialRaw(ctx, "tcp", upAddr)
	if err != nil {
		return nil, fmt.Errorf("dial upstream %s: %w", upAddr, err)
	}
	switch l.upURL.Scheme {
	case "socks5":
		err = socks5ClientHandshake(conn, address, l.upURL.User)
	case "http":
		err = httpConnectHandshake(ctx, conn, address, l.upURL.User)
	default:
		err = fmt.Errorf("unsupported upstream scheme %q", l.upURL.Scheme)
	}
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("upstream %s handshake: %w", upAddr, err)
	}
	return conn, nil
}

func upstreamHostPort(u *url.URL) string {
	port := u.Port()
	if port == "" {
		if u.Scheme == "socks5" {
			port = "1080"
		} else {
			port = "8080"
		}
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// dialRaw performs one pinned dial. All non-literal hosts are resolved
// through the lane's DNS mode (lane / doh / system) so DNS cannot silently
// escape the lane.
func (l *Lane) dialRaw(ctx context.Context, network, address string) (net.Conn, error) {
	s := l.current()
	if l.cfg.StrictEnabled() && !s.healthy {
		l.stats.RejectsTotal.Add(1)
		return nil, fmt.Errorf("%w: %s", ErrFailClosed, s.lastErr)
	}
	return l.dialRawUnchecked(ctx, network, address, s)
}

func (l *Lane) dialRawUnchecked(ctx context.Context, network, address string, s snapshot) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("invalid address %q: %w", address, err)
	}
	bound := l.cfg.Interface != "" || l.cfg.BindIP != ""

	var ip net.IP
	switch {
	case net.ParseIP(host) != nil:
		ip = net.ParseIP(host)
	case !bound && l.cfg.DNSMode() == config.DNSSystem:
		// Unbound lane with system DNS: let the dialer resolve so Happy
		// Eyeballs can pick a working family.
		d := &net.Dialer{Timeout: 15 * time.Second}
		return d.DialContext(ctx, network, address)
	default:
		ip, err = l.resolveHost(ctx, host, s, network)
		if err != nil {
			return nil, err
		}
	}

	d := &net.Dialer{Timeout: 15 * time.Second, Control: s.control}
	if s.control == nil && bound {
		src := s.srcFor(ip)
		if src == nil {
			l.stats.RejectsTotal.Add(1)
			return nil, fmt.Errorf("no %s source address on lane egress for %q", familyName(ip), host)
		}
		if isUDPNetwork(network) {
			d.LocalAddr = &net.UDPAddr{IP: src}
		} else {
			d.LocalAddr = &net.TCPAddr{IP: src}
		}
	}
	target := net.JoinHostPort(ip.String(), port)
	return d.DialContext(ctx, network, target)
}

func familyName(ip net.IP) string {
	if ip != nil && ip.To4() != nil {
		return "IPv4"
	}
	return "IPv6"
}

// resolveHost looks up host per the lane's DNS mode and returns a single IP
// usable with the lane's source addresses.
func (l *Lane) resolveHost(ctx context.Context, host string, s snapshot, network string) (net.IP, error) {
	var ips []net.IP
	switch l.cfg.DNSMode() {
	case config.DNSDoH:
		l.stats.DNSQueries.Add(1)
		var err error
		ips, err = l.doh.Resolve(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("resolve %q: %w", host, err)
		}
	case config.DNSLane:
		l.stats.DNSQueries.Add(1)
		ipAddrs, err := l.resolver().LookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("resolve %q: %w", host, err)
		}
		for _, ia := range ipAddrs {
			ips = append(ips, ia.IP)
		}
	default: // system
		l.stats.DNSQueries.Add(1)
		ipAddrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("resolve %q: %w", host, err)
		}
		for _, ia := range ipAddrs {
			ips = append(ips, ia.IP)
		}
	}

	var usable []net.IP
	for _, ip := range ips {
		if (network == "tcp4" || network == "udp4") && ip.To4() == nil {
			continue
		}
		if (network == "tcp6" || network == "udp6") && ip.To4() != nil {
			continue
		}
		usable = append(usable, ip)
	}
	for _, ip := range usable {
		if s.srcFor(ip) != nil {
			return ip, nil
		}
	}
	if len(usable) == 0 {
		return nil, fmt.Errorf("resolve %q: no usable address returned", host)
	}
	if s.control != nil || !(l.cfg.Interface != "" || l.cfg.BindIP != "") {
		// Device binding selects the source on the device itself; unbound
		// lanes have no source constraint.
		return usable[0], nil
	}
	return nil, fmt.Errorf("resolve %q: no address family matches the lane source", host)
}

// Resolve implements Egress: it resolves host per the lane's DNS mode and
// fails closed while the lane is unhealthy.
func (l *Lane) Resolve(ctx context.Context, network, host string) (net.IP, error) {
	s := l.current()
	if l.cfg.StrictEnabled() && !s.healthy {
		l.stats.RejectsTotal.Add(1)
		return nil, fmt.Errorf("%w: %s", ErrFailClosed, s.lastErr)
	}
	return l.resolveHost(ctx, host, s, network)
}

// dohDial dials a DoH endpoint through the lane. IP-literal endpoints need no
// bootstrap; a hostname endpoint is resolved with the lane's UDP resolver to
// avoid a resolution loop.
func (l *Lane) dohDial(ctx context.Context, network, address string) (net.Conn, error) {
	if host, port, err := net.SplitHostPort(address); err == nil && net.ParseIP(host) == nil {
		s := l.current()
		l.stats.DNSQueries.Add(1)
		ipAddrs, rerr := l.resolver().LookupIPAddr(ctx, host)
		if rerr != nil {
			return nil, fmt.Errorf("bootstrap doh endpoint %q: %w", host, rerr)
		}
		resolved := false
		for _, ia := range ipAddrs {
			if s.srcFor(ia.IP) != nil || s.control != nil || !(l.cfg.Interface != "" || l.cfg.BindIP != "") {
				address = net.JoinHostPort(ia.IP.String(), port)
				resolved = true
				break
			}
		}
		if !resolved {
			// Falling through would resolve the endpoint via DoH again — a
			// resolution loop through ourselves.
			return nil, fmt.Errorf("doh endpoint %q: no address family usable with the lane source", host)
		}
	}
	s := l.current()
	return l.dialRawUnchecked(ctx, network, address, s)
}

// resolver returns a resolver whose DNS traffic itself egresses through the
// lane. It bypasses the strict health check: when the interface is down the
// query simply fails.
//
// Loopback nameservers (systemd-resolved's 127.0.0.53 and friends) cannot be
// reached through a device-bound socket, so they are substituted with
// dns_servers entries or the public fallbacks below.
func (l *Lane) resolver() *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			if host, port, err := net.SplitHostPort(address); err == nil {
				if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
					address = net.JoinHostPort(l.dnsFallback(), port)
				}
			}
			s := l.current()
			return l.dialRawUnchecked(ctx, network, address, s)
		},
	}
}

// dnsFallback picks the first configured DNS server, else a public default.
func (l *Lane) dnsFallback() string {
	for _, s := range l.cfg.DNSServers {
		if s != "" {
			return s
		}
	}
	return "223.5.5.5"
}
