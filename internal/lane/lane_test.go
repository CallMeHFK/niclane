package lane

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/CallMeHFK/niclane/internal/config"
	"github.com/CallMeHFK/niclane/internal/metrics"
)

func newLane(t *testing.T, lc *config.LaneConfig) *Lane {
	t.Helper()
	st := metrics.NewRegistry().Register(lc.Name)
	l, err := New(lc, slog.New(slog.NewTextHandler(io.Discard, nil)), st)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// startEcho starts a TCP echo server on 127.0.0.1 and returns its address.
func startEcho(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					if n > 0 {
						if _, werr := c.Write(buf[:n]); werr != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	return ln.Addr().String()
}

func TestSourceBindDialLoopback(t *testing.T) {
	echo := startEcho(t)
	l := newLane(t, &config.LaneConfig{Name: "lo", Listen: "127.0.0.1:0", BindIP: "127.0.0.1"})
	l.Refresh()
	healthy, mode, lastErr := l.Current()
	if !healthy {
		t.Fatalf("loopback lane unhealthy: %s", lastErr)
	}
	if mode != BindModeSrcIP {
		t.Fatalf("bind mode = %d, want source-ip", mode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := l.DialContext(ctx, "tcp", echo)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	msg := "hello niclane"
	if _, err := conn.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(msg))
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != msg {
		t.Fatalf("echo mismatch: %q", string(buf))
	}
}

func TestFailClosedOnMissingInterface(t *testing.T) {
	echo := startEcho(t)
	l := newLane(t, &config.LaneConfig{
		Name: "dead", Listen: "127.0.0.1:0",
		Interface: "niclane-no-such-iface",
	})
	l.Refresh()
	healthy, _, lastErr := l.Current()
	if healthy {
		t.Fatal("lane bound to a missing interface must be unhealthy")
	}
	if lastErr == "" {
		t.Fatal("expected a lastErr explaining the unhealthiness")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := l.DialContext(ctx, "tcp", echo)
	if !errors.Is(err, ErrFailClosed) {
		t.Fatalf("want ErrFailClosed, got %v", err)
	}
	if l.Stats().RejectsTotal.Load() == 0 {
		t.Fatal("reject counter not incremented")
	}
}

func TestNonStrictUnhealthyLaneDialsAndFails(t *testing.T) {
	no := false
	l := newLane(t, &config.LaneConfig{
		Name: "loose", Listen: "127.0.0.1:0",
		Interface: "niclane-no-such-iface",
		Strict:    &no,
	})
	l.Refresh()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Non-strict: the lane attempts the dial, which must fail (nothing to
	// dial through), but not with ErrFailClosed.
	_, err := l.DialContext(ctx, "tcp", "127.0.0.1:1")
	if err == nil {
		t.Fatal("dial through a broken lane should fail")
	}
	if errors.Is(err, ErrFailClosed) {
		t.Fatal("non-strict lane must not fail closed")
	}
}

func TestActiveConnectionTracking(t *testing.T) {
	echo := startEcho(t)
	l := newLane(t, &config.LaneConfig{Name: "lo", Listen: "127.0.0.1:0", BindIP: "127.0.0.1"})
	l.Refresh()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := l.DialContext(ctx, "tcp", echo)
	if err != nil {
		t.Fatal(err)
	}
	if got := l.Stats().ConnsActive.Load(); got != 1 {
		t.Fatalf("active = %d, want 1", got)
	}
	conn.Close()
	if got := l.Stats().ConnsActive.Load(); got != 0 {
		t.Fatalf("active after close = %d, want 0", got)
	}
}

// startRejectProxy accepts CONNECT and answers 403.
func startRejectProxy(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						return
					}
					if line == "\r\n" {
						break
					}
				}
				_, _ = io.WriteString(c, "HTTP/1.1 403 Forbidden\r\n\r\n")
			}(c)
		}
	}()
	return ln.Addr().String()
}

func TestUpstreamRejectClassification(t *testing.T) {
	addr := startRejectProxy(t)
	l := newLane(t, &config.LaneConfig{Name: "rej", Upstream: "http://" + addr, DNS: config.DNSSystem})
	_, err := l.DialContext(context.Background(), "tcp", "example.com:443")
	if !errors.Is(err, ErrUpstreamReject) {
		t.Fatalf("err = %v, want ErrUpstreamReject", err)
	}
}

func TestUpstreamDialErrorIsNotReject(t *testing.T) {
	l := newLane(t, &config.LaneConfig{Name: "dead", Upstream: "http://127.0.0.1:1", DNS: config.DNSSystem})
	_, err := l.DialContext(context.Background(), "tcp", "example.com:443")
	if err == nil || errors.Is(err, ErrUpstreamReject) {
		t.Fatalf("err = %v, want a transport error", err)
	}
}

type fakeEgress struct {
	err  error
	conn net.Conn
	name string
}

func (f *fakeEgress) DialContext(context.Context, string, string) (net.Conn, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.conn, nil
}
func (f *fakeEgress) ListenUDP(context.Context) (*net.UDPConn, error) {
	return nil, errors.New("no udp")
}
func (f *fakeEgress) Resolve(context.Context, string, string) (net.IP, error) {
	return net.ParseIP("127.0.0.1"), nil
}
func (f *fakeEgress) Reject(string)         {}
func (f *fakeEgress) Stats() *metrics.Stats { return &metrics.Stats{} }
func (f *fakeEgress) Name() string          { return f.name }

type fakeProvider struct {
	egresses []Egress
	next     int
	failed   []Egress
}

func (p *fakeProvider) Acquire(context.Context) (Egress, error) {
	if p.next >= len(p.egresses) {
		return nil, errors.New("no more egresses")
	}
	e := p.egresses[p.next]
	p.next++
	return e, nil
}
func (p *fakeProvider) ReportFailure(eg Egress, _ error) { p.failed = append(p.failed, eg) }
func (p *fakeProvider) Reject(string)                    {}
func (p *fakeProvider) Stats() *metrics.Stats            { return &metrics.Stats{} }
func (p *fakeProvider) Name() string                     { return "fake" }

func TestDialFailsOverOnTransportError(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	first := &fakeEgress{err: errors.New("dial upstream: refused"), name: "first"}
	second := &fakeEgress{conn: a, name: "second"}
	p := &fakeProvider{egresses: []Egress{first, second}}
	conn, err := Dial(context.Background(), p, "tcp", "example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if p.next != 2 || len(p.failed) != 1 || p.failed[0] != first {
		t.Fatalf("next=%d failed=%v", p.next, p.failed)
	}
}

func TestDialDoesNotFailoverOnReject(t *testing.T) {
	first := &fakeEgress{err: &UpstreamRejectError{Reason: "403"}, name: "first"}
	p := &fakeProvider{egresses: []Egress{first, &fakeEgress{err: errors.New("unused")}}}
	_, err := Dial(context.Background(), p, "tcp", "example.com:443")
	if !errors.Is(err, ErrUpstreamReject) {
		t.Fatalf("err = %v, want ErrUpstreamReject", err)
	}
	if p.next != 1 {
		t.Fatalf("must not try a second candidate, next=%d", p.next)
	}
}
