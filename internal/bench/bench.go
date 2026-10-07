// Package bench measures per-lane throughput against an echo sink and
// provides the sink itself for cross-host measurements.
package bench

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/CallMeHFK/niclane/internal/lane"
)

// DefaultBlock is the write chunk size used by RunLane.
const DefaultBlock = 64 * 1024

// Result reports the outcome of one lane measurement.
type Result struct {
	Lane    string
	Target  string
	Up      int64 // bytes written into the lane (towards the sink)
	Down    int64 // bytes echoed back through the lane
	Elapsed time.Duration
	Err     error
}

// MBytesPerSec converts a byte count over elapsed into MB/s (10^6 bytes).
func (r Result) MBytesPerSec(n int64) float64 {
	if r.Elapsed <= 0 {
		return 0
	}
	return float64(n) / (1024 * 1024) / r.Elapsed.Seconds()
}

// ServeSink runs a TCP echo sink until the listener is closed. Every byte
// received is echoed back to the sender. Use it on a remote host to measure
// real cross-host lane throughput:
//
//	niclane bench serve --listen :9999
func ServeSink(ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go func(c net.Conn) {
			defer c.Close()
			_, _ = io.Copy(c, c)
		}(conn)
	}
}

// RunLane dials target through the lane and hammers it with echo traffic for
// the given duration, counting bytes in both directions. The sink must be
// reachable through the lane's pinned NIC.
func RunLane(ctx context.Context, egress lane.Egress, target string, duration time.Duration, block int) Result {
	res := Result{Lane: egress.Name(), Target: target}
	if duration <= 0 {
		res.Err = errors.New("duration must be positive")
		return res
	}
	if block <= 0 {
		block = DefaultBlock
	}

	conn, err := egress.DialContext(ctx, "tcp", target)
	if err != nil {
		res.Err = fmt.Errorf("dial through lane: %w", err)
		return res
	}
	defer conn.Close()

	start := time.Now()
	deadline := start.Add(duration)
	_ = conn.SetWriteDeadline(deadline)
	_ = conn.SetReadDeadline(deadline.Add(250 * time.Millisecond))

	var up, down atomic.Int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, block)
		for time.Now().Before(deadline) {
			n, err := conn.Write(buf)
			up.Add(int64(n))
			if err != nil {
				return
			}
		}
	}()

	buf := make([]byte, block)
	for {
		n, err := conn.Read(buf)
		down.Add(int64(n))
		if err != nil {
			break
		}
		if time.Now().After(deadline) {
			break
		}
	}
	conn.Close()
	wg.Wait()

	res.Up = up.Load()
	res.Down = down.Load()
	res.Elapsed = time.Since(start)
	// The final read waits out the grace window; do not inflate MB/s with it.
	if maxElapsed := duration + 250*time.Millisecond; res.Elapsed > maxElapsed {
		res.Elapsed = maxElapsed
	}
	if res.Up == 0 && res.Down == 0 {
		res.Err = errors.New("no traffic measured")
	}
	return res
}
