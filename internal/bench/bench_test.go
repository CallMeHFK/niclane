package bench

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/CallMeHFK/niclane/internal/config"
	"github.com/CallMeHFK/niclane/internal/lane"
	"github.com/CallMeHFK/niclane/internal/metrics"
)

func newTestLane(t *testing.T, lc *config.LaneConfig) *lane.Lane {
	t.Helper()
	l, err := lane.New(lc, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.NewRegistry().Register(lc.Name))
	if err != nil {
		t.Fatal(err)
	}
	l.Refresh()
	return l
}

func startSink(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() { _ = ServeSink(ln) }()
	return ln.Addr().String()
}

func TestRunLaneLoopback(t *testing.T) {
	target := startSink(t)
	l := newTestLane(t, &config.LaneConfig{Name: "lo", Listen: "127.0.0.1:0", BindIP: "127.0.0.1"})

	res := RunLane(context.Background(), l, target, 800*time.Millisecond, 32*1024)
	if res.Err != nil {
		t.Fatalf("bench failed: %v", res.Err)
	}
	if res.Down < 1<<20 {
		t.Fatalf("echoed too little: %d bytes", res.Down)
	}
	if res.Up == 0 {
		t.Fatal("no bytes written")
	}
	if res.MBytesPerSec(res.Down) <= 0 {
		t.Fatal("MB/s must be positive")
	}
}

func TestRunLaneFailClosed(t *testing.T) {
	l := newTestLane(t, &config.LaneConfig{
		Name: "dead", Listen: "127.0.0.1:0", Interface: "niclane-no-such-iface",
	})
	res := RunLane(context.Background(), l, "127.0.0.1:1", 500*time.Millisecond, 16*1024)
	if res.Err == nil {
		t.Fatal("bench through a dead lane must fail")
	}
}

func TestServeSinkEcho(t *testing.T) {
	addr := startSink(t)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	msg := []byte("ping-through-sink")
	if _, err := conn.Write(msg); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != string(msg) {
		t.Fatalf("echo mismatch: %q", string(buf))
	}
}
