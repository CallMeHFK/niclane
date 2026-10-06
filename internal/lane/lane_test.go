package lane

import (
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
