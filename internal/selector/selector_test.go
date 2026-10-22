package selector

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CallMeHFK/niclane/internal/config"
	"github.com/CallMeHFK/niclane/internal/lane"
	"github.com/CallMeHFK/niclane/internal/metrics"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func boolPtr(b bool) *bool { return &b }
func intPtr(i int) *int    { return &i }

// fakeProxy is a minimal HTTP CONNECT proxy whose refusals can be toggled.
type fakeProxy struct {
	ln     net.Listener
	reject atomic.Bool
}

func newFakeProxy(t *testing.T) *fakeProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &fakeProxy{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go p.serve()
	return p
}

func (p *fakeProxy) addr() string { return p.ln.Addr().String() }

func (p *fakeProxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		go p.handle(c)
	}
}

func (p *fakeProxy) handle(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	line, err := br.ReadString('\n')
	if err != nil {
		return
	}
	fields := strings.Fields(line)
	for {
		l, err := br.ReadString('\n')
		if err != nil {
			return
		}
		if l == "\r\n" {
			break
		}
	}
	if p.reject.Load() || len(fields) < 2 || fields[0] != "CONNECT" {
		fmt.Fprint(c, "HTTP/1.1 403 Forbidden\r\n\r\n")
		return
	}
	up, err := net.Dial("tcp", fields[1])
	if err != nil {
		fmt.Fprint(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	defer up.Close()
	fmt.Fprint(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
	go io.Copy(up, br)
	_, _ = io.Copy(c, up)
}

// startTarget serves a 204 page and returns its host:port.
func startTarget(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func newSelector(t *testing.T, probeAddr string, cands ...*config.CandidateConfig) *Selector {
	t.Helper()
	lc := &config.LaneConfig{
		Name:       "test",
		Listen:     "127.0.0.1:0",
		Strict:     boolPtr(true),
		Candidates: cands,
		Discover:   &config.DiscoverConfig{ProbeURL: "http://" + probeAddr + "/", Interval: "2s"},
	}
	sel, err := New(lc, nil, discardLogger(), metrics.NewRegistry().Register(lc.Name))
	if err != nil {
		t.Fatal(err)
	}
	sel.discoverFn = func(context.Context, *Selector) []Discovered { return nil }
	return sel
}

func cand(proxyAddr string, priority int) *config.CandidateConfig {
	return &config.CandidateConfig{Upstream: "http://" + proxyAddr, Priority: intPtr(priority)}
}

func TestAcquireRespectsPriority(t *testing.T) {
	target := startTarget(t)
	low := newFakeProxy(t)
	high := newFakeProxy(t)
	sel := newSelector(t, target, cand(low.addr(), 1), cand(high.addr(), 100))

	eg, err := sel.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := "test#" + high.addr()
	if eg.Name() != want {
		t.Fatalf("acquired %q, want %q", eg.Name(), want)
	}
}

func TestDialFailsOverToNextCandidate(t *testing.T) {
	target := startTarget(t)
	dead, _ := net.Listen("tcp", "127.0.0.1:0")
	deadAddr := dead.Addr().String()
	dead.Close() // nothing listens here now
	good := newFakeProxy(t)
	sel := newSelector(t, target, cand(deadAddr, 100), cand(good.addr(), 1))

	conn, err := lane.Dial(context.Background(), sel, "tcp", target)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()

	// The good candidate served the connection; the dead one was tried once.
	found := false
	for _, c := range sel.Candidates() {
		if strings.Contains(c.Upstream, good.addr()) {
			found = true
		}
	}
	if !found {
		t.Fatal("good candidate missing from status")
	}
	sel.mu.RLock()
	defer sel.mu.RUnlock()
	for _, c := range sel.cands {
		switch {
		case strings.Contains(c.upstream, good.addr()):
			if c.stats.ConnsTotal.Load() != 1 {
				t.Fatalf("good conns = %d, want 1", c.stats.ConnsTotal.Load())
			}
		case strings.Contains(c.upstream, deadAddr):
			// One dial plus the out-of-band re-probe triggered by the failure.
			if c.stats.ConnsTotal.Load() < 1 {
				t.Fatalf("dead conns = %d, want at least 1", c.stats.ConnsTotal.Load())
			}
			if !c.suspectUntil.After(time.Now().Add(-time.Second)) {
				t.Fatal("dead candidate should be marked suspect")
			}
		}
	}
}

func TestDialDoesNotFailoverOnUpstreamReject(t *testing.T) {
	target := startTarget(t)
	rejecting := newFakeProxy(t)
	rejecting.reject.Store(true)
	good := newFakeProxy(t)
	sel := newSelector(t, target, cand(rejecting.addr(), 100), cand(good.addr(), 1))

	_, err := lane.Dial(context.Background(), sel, "tcp", target)
	if err == nil || !strings.Contains(err.Error(), "upstream rejected") {
		t.Fatalf("err = %v, want upstream rejection", err)
	}
	sel.mu.RLock()
	defer sel.mu.RUnlock()
	for _, c := range sel.cands {
		if strings.Contains(c.upstream, good.addr()) && c.stats.ConnsTotal.Load() != 0 {
			t.Fatalf("good candidate must not be tried, conns = %d", c.stats.ConnsTotal.Load())
		}
	}
}

func TestProbeDebounceAndRecovery(t *testing.T) {
	target := startTarget(t)
	proxy := newFakeProxy(t)
	proxy.reject.Store(true)
	sel := newSelector(t, target, cand(proxy.addr(), 100))

	sel.Refresh(context.Background())
	sel.Refresh(context.Background())
	if got := sel.Candidates()[0].Healthy; got {
		t.Fatal("candidate should be dead after two failed probes")
	}

	proxy.reject.Store(false)
	sel.Refresh(context.Background())
	if got := sel.Candidates()[0].Healthy; !got {
		t.Fatal("candidate should recover after one successful probe")
	}
}

func TestStrictFailsClosedWhenAllDead(t *testing.T) {
	target := startTarget(t)
	proxy := newFakeProxy(t)
	proxy.reject.Store(true)
	sel := newSelector(t, target, cand(proxy.addr(), 100))
	sel.Refresh(context.Background())
	sel.Refresh(context.Background())

	if _, err := sel.Acquire(context.Background()); err == nil {
		t.Fatal("expected fail-closed error")
	}
}

func TestDiscoveryAddsAndRemovesCandidates(t *testing.T) {
	target := startTarget(t)
	static := newFakeProxy(t)
	sel := newSelector(t, target, cand(static.addr(), config.StaticCandidatePriority))

	var found []Discovered
	sel.discoverFn = func(context.Context, *Selector) []Discovered { return found }

	found = []Discovered{
		{Interface: "eth9", Upstream: "http://10.9.9.1:10808"},
		{Interface: "eth8", Upstream: "http://10.8.8.1:10808"},
	}
	sel.Refresh(context.Background())
	got := sel.Candidates()
	if len(got) != 3 {
		t.Fatalf("candidates = %d, want 3: %+v", len(got), got)
	}
	if scan := sel.Discovered(); len(scan) != 2 {
		t.Fatalf("last scan = %+v, want 2 endpoints", scan)
	}
	for _, c := range got {
		if c.Source == "discovered" && c.Priority != config.DiscoveredCandidatePriority {
			t.Fatalf("discovered priority = %d, want %d", c.Priority, config.DiscoveredCandidatePriority)
		}
	}

	// eth9 vanished from the scan; the static candidate must survive.
	found = []Discovered{{Interface: "eth8", Upstream: "http://10.8.8.1:10808"}}
	sel.Refresh(context.Background())
	got = sel.Candidates()
	if len(got) != 2 {
		t.Fatalf("candidates after rescan = %d, want 2: %+v", len(got), got)
	}
	var haveStatic, haveEth8 bool
	for _, c := range got {
		switch {
		case c.Source == "static":
			haveStatic = true
		case c.Interface == "eth8":
			haveEth8 = true
		case c.Interface == "eth9":
			t.Fatal("eth9 should have been removed")
		}
	}
	if !haveStatic || !haveEth8 {
		t.Fatalf("static=%v eth8=%v, want both kept: %+v", haveStatic, haveEth8, got)
	}
}
