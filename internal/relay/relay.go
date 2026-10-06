// Package relay pipes bytes between a client connection and the lane egress,
// counting transferred bytes per direction.
package relay

import (
	"io"
	"net"
	"sync"
	"time"
)

// Pipe copies client<->egress in both directions until either side errors or
// closes; both connections are closed when the pipe finishes. onDone receives
// (up, down) = (client->egress, egress->client) byte totals.
func Pipe(client, egress net.Conn, idle time.Duration, onDone func(up, down int64)) {
	var up, down int64
	var once sync.Once
	stop := func() { once.Do(func() { client.Close(); egress.Close() }) }
	defer stop()

	var wg sync.WaitGroup
	wg.Add(1) // only the goroutine side needs a slot; the main copy runs inline
	go func() {
		defer wg.Done()
		_, _ = io.Copy(&countWriter{w: client, n: &down}, &deadlineReader{c: egress, idle: idle})
		stop()
	}()
	_, _ = io.Copy(&countWriter{w: egress, n: &up}, &deadlineReader{c: client, idle: idle})
	stop()
	wg.Wait()
	if onDone != nil {
		onDone(up, down)
	}
}

type countWriter struct {
	w io.Writer
	n *int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	*c.n += int64(n)
	return n, err
}

// deadlineReader refreshes a read deadline before every Read, implementing an
// inactivity timeout over long-lived tunnels.
type deadlineReader struct {
	c    net.Conn
	idle time.Duration
}

func (d *deadlineReader) Read(p []byte) (int, error) {
	if d.idle > 0 {
		_ = d.c.SetReadDeadline(time.Now().Add(d.idle))
	} else {
		_ = d.c.SetReadDeadline(time.Time{})
	}
	return d.c.Read(p)
}
