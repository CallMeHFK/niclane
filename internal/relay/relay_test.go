package relay

import (
	"io"
	"net"
	"testing"
	"time"
)

// startEcho returns the address of a TCP echo server on 127.0.0.1.
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
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()
	return ln.Addr().String()
}

func TestPipeCountsAndRelays(t *testing.T) {
	echoConn, err := net.Dial("tcp", startEcho(t))
	if err != nil {
		t.Fatal(err)
	}
	defer echoConn.Close()

	// client: test-driven end; Pipe owns the server end.
	c, srv := net.Pipe()
	defer c.Close()

	done := make(chan struct{})
	var up, down int64
	go func() {
		Pipe(srv, echoConn, 0, func(u, d int64) { up, down = u, d; close(done) })
	}()

	msg := "hello"
	if _, err := c.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(msg))
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != msg {
		t.Fatalf("echo mismatch: %q", string(buf))
	}
	c.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("pipe did not finish")
	}
	if up != int64(len(msg)) || down != int64(len(msg)) {
		t.Fatalf("up = %d, down = %d, want %d/%d", up, down, len(msg), len(msg))
	}
}
