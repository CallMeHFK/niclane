// Package selector serves one lane over an ordered set of egress
// candidates. Each candidate is a full lane.Lane, so interface binding, DNS
// and fail-closed semantics are reused rather than duplicated.
package selector

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/CallMeHFK/niclane/internal/config"
	"github.com/CallMeHFK/niclane/internal/discover"
	"github.com/CallMeHFK/niclane/internal/lane"
	"github.com/CallMeHFK/niclane/internal/metrics"
)

const (
	probeFailThreshold = 2
	probeTimeout       = 5 * time.Second
	probeConcurrency   = 4
	suspectCooldown    = 2 * time.Second
)

// CandidateStatus is the per-candidate runtime state exposed via /status.
type CandidateStatus struct {
	Interface string `json:"interface,omitempty"`
	Upstream  string `json:"upstream,omitempty"`
	Source    string `json:"source"`
	Priority  int    `json:"priority"`
	Healthy   bool   `json:"healthy"`
	DelayMS   int64  `json:"delay_ms,omitempty"`
	LastError string `json:"last_error,omitempty"`
}

// Counts aggregates per-candidate counters for lane-level status.
type Counts struct {
	ConnsTotal  int64
	ConnsActive int64
	Rejects     int64
	BytesUp     int64
	BytesDown   int64
	DNSQueries  int64
	UDPRelays   int64
}

// TestResult is one candidate's answer to the exit-IP probe used by
// `niclane test`.
type TestResult struct {
	Candidate CandidateStatus
	ExitIP    string
	Err       error
}

type candidate struct {
	iface    string
	upstream string
	source   string
	priority int
	order    int
	lane     *lane.Lane
	stats    *metrics.Stats

	// The fields below are guarded by Selector.mu.
	known        bool
	probeOK      bool
	fails        int
	delay        time.Duration
	lastErr      string
	suspectUntil time.Time
	probing      bool
	announced    bool
}

func (c *candidate) label() string {
	if c.iface != "" && c.upstream != "" {
		return c.iface + " " + c.upstream
	}
	if c.iface != "" {
		return c.iface
	}
	return c.upstream
}

// Selector is one stable listener backed by an ordered candidate set.
type Selector struct {
	name     string
	log      *slog.Logger
	stats    *metrics.Stats
	strict   bool
	probeURL *url.URL
	interval time.Duration
	parent   *config.LaneConfig
	global   *config.DiscoveryConfig

	mu       sync.RWMutex
	cands    []*candidate
	byKey    map[string]*candidate
	active   *candidate
	lastErr  string
	lastScan []Discovered

	// discoverFn is replaced in tests; nil means use the real discovery.
	discoverFn        func(context.Context, *Selector) []Discovered
	unsupportedLogged bool
}

// Discovered is a candidate found at runtime.
type Discovered struct {
	Interface string
	Upstream  string
}

// New builds a selector for a lane in candidate mode.
func New(lc *config.LaneConfig, global *config.DiscoveryConfig, log *slog.Logger, stats *metrics.Stats) (*Selector, error) {
	probeURL, err := url.Parse(lc.Discover.EffectiveProbeURL())
	if err != nil {
		return nil, fmt.Errorf("lane %q: %w", lc.Name, err)
	}
	interval, err := lc.Discover.EffectiveInterval()
	if err != nil {
		return nil, fmt.Errorf("lane %q: %w", lc.Name, err)
	}
	s := &Selector{
		name:     lc.Name,
		log:      log.With("lane", lc.Name),
		stats:    stats,
		strict:   lc.StrictEnabled(),
		probeURL: probeURL,
		interval: interval,
		parent:   lc,
		global:   global,
		byKey:    make(map[string]*candidate),
	}
	for _, c := range lc.Candidates {
		if err := s.addCandidate(c.Interface, c.Upstream, c.EffectivePriority(), "static"); err != nil {
			return nil, fmt.Errorf("lane %q: %w", lc.Name, err)
		}
	}
	return s, nil
}

// Name returns the lane name.
func (s *Selector) Name() string { return s.name }

// Stats returns the lane-level metrics bucket.
func (s *Selector) Stats() *metrics.Stats { return s.stats }

// Reject records a rejection that happened before a candidate was chosen.
func (s *Selector) Reject(reason string) {
	s.stats.RejectsTotal.Add(1)
	s.log.Debug("connection rejected", "reason", reason)
}

func candidateKey(iface, upstream string) string { return iface + "\x00" + upstream }

func candidateLabel(iface, upstream string) string {
	if iface != "" {
		return iface
	}
	if u, err := url.Parse(upstream); err == nil && u.Host != "" {
		return u.Host
	}
	return "default"
}

func (s *Selector) addCandidate(iface, upstream string, priority int, source string) error {
	key := candidateKey(iface, upstream)
	if _, ok := s.byKey[key]; ok {
		return nil
	}
	child := &config.LaneConfig{
		Name:       s.name + "#" + candidateLabel(iface, upstream),
		Interface:  iface,
		Upstream:   upstream,
		BindMode:   s.parent.BindMode,
		Strict:     s.parent.Strict,
		DNS:        s.parent.DNS,
		DNSServers: s.parent.DNSServers,
		DohURLs:    s.parent.DohURLs,
	}
	st := &metrics.Stats{}
	l, err := lane.New(child, s.log, st)
	if err != nil {
		return fmt.Errorf("candidate %q: %w", key, err)
	}
	c := &candidate{
		iface:    iface,
		upstream: upstream,
		source:   source,
		priority: priority,
		order:    len(s.cands),
		lane:     l,
		stats:    st,
	}
	s.cands = append(s.cands, c)
	s.byKey[key] = c
	return nil
}

func (s *Selector) deadLocked(c *candidate) bool { return c.fails >= probeFailThreshold }

func (s *Selector) betterLocked(a, b *candidate) bool {
	if a.priority != b.priority {
		return a.priority > b.priority
	}
	aOK := a.known && a.probeOK
	bOK := b.known && b.probeOK
	if aOK != bOK {
		return aOK
	}
	if aOK && a.delay != b.delay {
		return a.delay < b.delay
	}
	return a.order < b.order
}

// Acquire returns one pinned child lane for a new connection.
func (s *Selector) Acquire(context.Context) (lane.Egress, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	var best, suspect, nic *candidate
	for _, c := range s.cands {
		ok, _, _ := c.lane.Current()
		if !ok {
			continue
		}
		if nic == nil {
			nic = c
		}
		if s.deadLocked(c) {
			continue
		}
		if suspect == nil {
			suspect = c
		}
		if c.suspectUntil.After(now) {
			continue
		}
		if best == nil || s.betterLocked(c, best) {
			best = c
		}
	}
	pick := best
	if pick == nil {
		pick = suspect
	}
	if pick == nil && !s.strict {
		pick = nic
	}
	if pick == nil {
		s.lastErr = "no healthy candidate"
		s.stats.SetLastErr(s.lastErr)
		return nil, fmt.Errorf("%w: no healthy candidate", lane.ErrFailClosed)
	}
	s.active = pick
	s.lastErr = ""
	s.stats.SetLastErr("")
	return pick.lane, nil
}

// ReportFailure demotes a candidate after a transport-level dial failure and
// re-probes it out of band so it can recover quickly.
func (s *Selector) ReportFailure(eg lane.Egress, err error) {
	if errors.Is(err, lane.ErrUpstreamReject) {
		return
	}
	s.mu.Lock()
	var c *candidate
	for _, cand := range s.cands {
		if cand.lane == eg {
			c = cand
			break
		}
	}
	if c == nil {
		s.mu.Unlock()
		return
	}
	trigger := !c.suspectUntil.After(time.Now())
	if trigger {
		c.suspectUntil = time.Now().Add(suspectCooldown)
	}
	s.mu.Unlock()
	if trigger {
		go s.probeOne(context.Background(), c)
	}
}

// Current reports the lane-level health snapshot.
func (s *Selector) Current() (bool, int64, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now()
	healthy := false
	for _, c := range s.cands {
		ok, _, _ := c.lane.Current()
		if ok && !s.deadLocked(c) && !c.suspectUntil.After(now) {
			healthy = true
			break
		}
	}
	mode := int64(lane.BindModeNone)
	errMsg := s.lastErr
	if s.active != nil {
		_, m, e := s.active.lane.Current()
		mode = m
		if e != "" {
			errMsg = e
		}
	}
	return healthy, mode, errMsg
}

// Candidates snapshots every candidate's state.
func (s *Selector) Candidates() []CandidateStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]CandidateStatus, 0, len(s.cands))
	for _, c := range s.cands {
		out = append(out, s.statusLocked(c))
	}
	return out
}

func (s *Selector) statusLocked(c *candidate) CandidateStatus {
	ok, _, _ := c.lane.Current()
	st := CandidateStatus{
		Interface: c.iface,
		Upstream:  c.upstream,
		Source:    c.source,
		Priority:  c.priority,
		Healthy:   ok && !s.deadLocked(c),
		LastError: c.lastErr,
	}
	// A stale delay from the last successful probe would mislead: only a
	// healthy candidate reports one.
	if st.Healthy && c.delay > 0 {
		st.DelayMS = c.delay.Milliseconds()
	}
	return st
}

// Active returns the label of the candidate used for the last connection.
func (s *Selector) Active() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.active == nil {
		return ""
	}
	return s.active.label()
}

// Discovered returns the endpoints found by the most recent successful scan.
func (s *Selector) Discovered() []Discovered {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Discovered(nil), s.lastScan...)
}

// Counts aggregates child counters for the lane-level status.
func (s *Selector) Counts() Counts {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var n Counts
	for _, c := range s.cands {
		n.ConnsTotal += c.stats.ConnsTotal.Load()
		n.ConnsActive += c.stats.ConnsActive.Load()
		n.Rejects += c.stats.RejectsTotal.Load()
		n.BytesUp += c.stats.BytesUp.Load()
		n.BytesDown += c.stats.BytesDown.Load()
		n.DNSQueries += c.stats.DNSQueries.Load()
		n.UDPRelays += c.stats.UDPRelays.Load()
	}
	n.Rejects += s.stats.RejectsTotal.Load()
	return n
}

// Run probes and rediscovers until ctx is done.
func (s *Selector) Run(ctx context.Context) {
	s.Refresh(ctx)
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Refresh(ctx)
		}
	}
}

// Refresh runs one discovery sync (when enabled) and probes every candidate.
func (s *Selector) Refresh(ctx context.Context) {
	s.syncDiscovered(ctx)
	s.refreshLanes()
	s.probeAll(ctx)
}

// refreshLanes re-snapshots every candidate's child lane before probing.
// Child lanes are otherwise frozen at their creation snapshot: a candidate
// whose NIC was down when the process started would stay unhealthy forever,
// because the strict dial gate fails even the health probe — the candidate
// could never recover once its NIC returns.
func (s *Selector) refreshLanes() {
	s.mu.RLock()
	list := append([]*candidate(nil), s.cands...)
	s.mu.RUnlock()
	for _, c := range list {
		c.lane.Refresh()
	}
}

func (s *Selector) probeAll(ctx context.Context) {
	s.mu.RLock()
	list := append([]*candidate(nil), s.cands...)
	s.mu.RUnlock()

	sem := make(chan struct{}, probeConcurrency)
	var wg sync.WaitGroup
	for _, c := range list {
		wg.Add(1)
		go func(c *candidate) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			s.probeOne(ctx, c)
		}(c)
	}
	wg.Wait()
	s.refreshHealth()
}

func (s *Selector) refreshHealth() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	healthy := false
	for _, c := range s.cands {
		ok, _, _ := c.lane.Current()
		if ok && !s.deadLocked(c) && !c.suspectUntil.After(now) {
			healthy = true
			break
		}
	}
	mode := int64(lane.BindModeNone)
	if s.active != nil {
		_, m, _ := s.active.lane.Current()
		mode = m
	}
	// A probe round that found a usable candidate also settles the lane-level
	// error: concurrent probes may have written a failure after a success.
	if healthy {
		s.lastErr = ""
		s.stats.SetLastErr("")
	}
	s.stats.Healthy.Store(healthy)
	s.stats.BindMode.Store(mode)
}

func (s *Selector) probeOne(ctx context.Context, c *candidate) {
	s.mu.Lock()
	if c.probing {
		s.mu.Unlock()
		return
	}
	c.probing = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		c.probing = false
		s.mu.Unlock()
	}()

	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	start := time.Now()
	err := probeEgress(pctx, c.lane, s.probeURL)
	cancel()
	delay := time.Since(start)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		c.known, c.probeOK, c.fails, c.delay, c.lastErr = true, true, 0, delay, ""
		c.suspectUntil = time.Time{}
		s.lastErr = ""
		s.stats.SetLastErr("")
		if !c.announced {
			c.announced = true
			s.log.Info("candidate healthy", "candidate", c.label(), "source", c.source, "delay", delay)
		}
		return
	}
	c.known = true
	c.probeOK = false
	dead := s.deadLocked(c)
	c.fails++
	c.lastErr = err.Error()
	s.stats.SetLastErr(c.lastErr)
	if !dead && s.deadLocked(c) {
		s.log.Warn("candidate unhealthy", "candidate", c.label(), "source", c.source, "error", err, "fails", c.fails)
	}
}

// probeEgress asks the candidate to fetch the probe URL and accepts any 2xx.
func probeEgress(ctx context.Context, eg lane.Egress, u *url.URL) error {
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	conn, err := eg.DialContext(ctx, "tcp", net.JoinHostPort(u.Hostname(), port))
	if err != nil {
		return err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if u.Scheme == "https" {
		tc := tls.Client(conn, &tls.Config{ServerName: u.Hostname()})
		if err := tc.HandshakeContext(ctx); err != nil {
			return err
		}
		conn = tc
	}
	path := u.RequestURI()
	if path == "" {
		path = "/"
	}
	if _, err := fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nUser-Agent: niclane-probe\r\nConnection: close\r\n\r\n", path, u.Host); err != nil {
		return err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodGet})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("probe status %d", resp.StatusCode)
	}
	return nil
}

// Test fetches rawURL through every candidate and reports the exit body.
func (s *Selector) Test(ctx context.Context, rawURL string, timeout time.Duration) []TestResult {
	s.mu.RLock()
	list := append([]*candidate(nil), s.cands...)
	s.mu.RUnlock()
	out := make([]TestResult, 0, len(list))
	for _, c := range list {
		res := TestResult{Candidate: s.statusFor(c)}
		cctx, cancel := context.WithTimeout(ctx, timeout)
		client := &http.Client{
			Transport: &http.Transport{DialContext: c.lane.DialContext, TLSHandshakeTimeout: 5 * time.Second},
		}
		req, err := http.NewRequestWithContext(cctx, http.MethodGet, rawURL, nil)
		if err == nil {
			var resp *http.Response
			resp, err = client.Do(req)
			if err == nil {
				b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
				resp.Body.Close()
				res.ExitIP = strings.TrimSpace(string(b))
			}
		}
		client.CloseIdleConnections()
		cancel()
		res.Err = err
		out = append(out, res)
	}
	return out
}

func (s *Selector) statusFor(c *candidate) CandidateStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.statusLocked(c)
}

// syncDiscovered refreshes the discovered candidate set. Static candidates
// are never removed; a discovered candidate that disappeared is dropped on
// the next pass.
func (s *Selector) syncDiscovered(ctx context.Context) {
	if s.parent.Discover == nil {
		return
	}
	var (
		found []Discovered
		err   error
	)
	if s.discoverFn != nil {
		found = s.discoverFn(ctx, s)
	} else {
		var devs []discover.Device
		devs, err = discover.Devices(ctx, discover.Options{
			Interfaces: s.parent.Discover.EffectiveInterfaces(),
			Via:        s.parent.Discover.Via,
			Ports:      s.parent.Discover.EffectivePorts(s.global),
			Log:        s.log,
		})
		if err == nil {
			found = make([]Discovered, 0, len(devs))
			for _, d := range devs {
				found = append(found, Discovered{
					Interface: d.Interface,
					Upstream:  "http://" + net.JoinHostPort(d.Gateway.String(), strconv.Itoa(d.Port)),
				})
			}
		}
	}
	if err != nil {
		if errors.Is(err, discover.ErrUnsupported) {
			s.mu.Lock()
			if !s.unsupportedLogged {
				s.unsupportedLogged = true
				s.log.Info("auto-discovery disabled: unsupported platform")
			}
			s.mu.Unlock()
			return
		}
		s.log.Warn("discovery failed", "error", err)
		return
	}

	want := make(map[string]bool, len(found))
	for _, d := range found {
		want[candidateKey(d.Interface, d.Upstream)] = true
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastScan = append(s.lastScan[:0], found...)
	kept := s.cands[:0]
	for _, c := range s.cands {
		if c.source == "discovered" && !want[candidateKey(c.iface, c.upstream)] {
			s.log.Info("candidate removed", "interface", c.iface, "upstream", c.upstream)
			delete(s.byKey, candidateKey(c.iface, c.upstream))
			if s.active == c {
				s.active = nil
			}
			continue
		}
		kept = append(kept, c)
	}
	s.cands = kept
	for i, c := range s.cands {
		c.order = i
	}
	for _, d := range found {
		key := candidateKey(d.Interface, d.Upstream)
		if _, ok := s.byKey[key]; ok {
			continue
		}
		if err := s.addCandidate(d.Interface, d.Upstream, config.DiscoveredCandidatePriority, "discovered"); err != nil {
			s.log.Warn("discover: cannot add candidate", "interface", d.Interface, "upstream", d.Upstream, "error", err)
		}
	}
}
