// Package relay pipes bytes between a client connection and the lane egress,
// counting transferred bytes per direction.
package relay

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Pipe copies client<->egress in both directions until both halves finish.
//
// Half-close semantics: a clean EOF on one side only half-closes (CloseWrite)
// the peer, so it can still deliver remaining data — a client that sends its
// request and FINs keeps receiving the response. A hard error on either side
// aborts the whole pipe. Both connections are fully closed when both halves
// are done.
//
// The optional idle timeout is enforced by a watchdog over a shared activity
// timestamp: any byte read or written on EITHER connection refreshes it, so a
// one-directional transfer (e.g. a long download with no upstream traffic) is
// never killed while the tunnel is alive, and a peer that stops reading or
// writing wedges only until the deadline expires.
func Pipe(client, egress net.Conn, idle time.Duration, onDone func(up, down int64)) {
	var up, down int64
	var once sync.Once
	activity := &atomic.Int64{} // unix nanos of the last byte seen on either side
	activity.Store(time.Now().UnixNano())
	stop := func() { once.Do(func() { client.Close(); egress.Close() }) }
	defer stop()

	done := make(chan struct{})
	defer close(done)
	if idle > 0 {
		go idleWatchdog(idle, activity, stop, done)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := io.Copy(
			&countWriter{w: client, n: &down, activity: activity},
			&activityReader{c: egress, activity: activity},
		)
		if err != nil {
			stop()
		} else {
			halfClose(client)
		}
	}()
	_, err := io.Copy(
		&countWriter{w: egress, n: &up, activity: activity},
		&activityReader{c: client, activity: activity},
	)
	if err != nil {
		stop()
	} else {
		halfClose(egress)
	}
	wg.Wait()
	stop()
	if onDone != nil {
		onDone(up, down)
	}
}

// idleWatchdog closes both connections when no byte has moved for idle.
func idleWatchdog(idle time.Duration, activity *atomic.Int64, stop func(), done <-chan struct{}) {
	t := time.NewTimer(idle)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			next := time.Unix(0, activity.Load()).Add(idle)
			if remaining := time.Until(next); remaining <= 0 {
				stop()
				return
			} else {
				t.Reset(remaining)
			}
		}
	}
}

// halfClose shuts down the write half of TCP connections; other conn types
// (net.Pipe in tests) are left untouched.
func halfClose(c net.Conn) {
	if h, ok := c.(interface{ CloseWrite() error }); ok {
		_ = h.CloseWrite()
	}
}

type countWriter struct {
	w        io.Writer
	n        *int64
	activity *atomic.Int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	*c.n += int64(n)
	if n > 0 {
		c.activity.Store(time.Now().UnixNano())
	}
	return n, err
}

// activityReader refreshes the shared activity timestamp on every byte.
type activityReader struct {
	c        net.Conn
	activity *atomic.Int64
}

func (r *activityReader) Read(p []byte) (int, error) {
	n, err := r.c.Read(p)
	if n > 0 {
		r.activity.Store(time.Now().UnixNano())
	}
	return n, err
}
