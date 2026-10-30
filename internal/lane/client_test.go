package lane

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/CallMeHFK/niclane/internal/config"
)

// startStallingProxy accepts the TCP connection and then never answers, which
// is what a wedged proxy (or one whose uplink died between accept and reply)
// looks like from the client side.
func startStallingProxy(t *testing.T) string {
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
			t.Cleanup(func() { c.Close() })
		}
	}()
	return ln.Addr().String()
}

// TestUpstreamHandshakeHonorsContextDeadline is the regression test for a
// probe budget that the handshake silently ignored.
//
// The health probe runs each candidate under a short context deadline, but
// both upstream handshakes overwrote the connection deadline with a fixed
// 15s and never consulted ctx. A proxy that accepted TCP and then stalled —
// exactly the shape seen when the phone uplink drops mid-session — therefore
// held a probe for 15s while its budget was 5s. Rounds stretch to three times
// the configured interval and health updates lag the link they describe.
//
// The caller's deadline is the budget. When it is sooner than the handshake
// default, it must win.
func TestUpstreamHandshakeHonorsContextDeadline(t *testing.T) {
	for _, scheme := range []string{"http", "socks5"} {
		t.Run(scheme, func(t *testing.T) {
			addr := startStallingProxy(t)
			l := newLane(t, &config.LaneConfig{Name: "stall-" + scheme, Upstream: scheme + "://" + addr, DNS: config.DNSSystem})
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()

			start := time.Now()
			_, err := l.DialContext(ctx, "tcp", "example.com:443")
			elapsed := time.Since(start)

			if err == nil {
				t.Fatal("dial through a stalling proxy must fail")
			}
			if elapsed > 3*time.Second {
				t.Fatalf("dial took %s with a 300ms budget; the handshake ignored the context deadline", elapsed.Round(time.Millisecond))
			}
		})
	}
}
