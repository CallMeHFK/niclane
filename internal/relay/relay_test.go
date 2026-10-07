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

// TestPipeHalfCloseKeepsResponseFlowing: a client that sends its request and
// half-closes must still receive the server's answer — the pipe must not
// full-close on the first EOF.
func TestPipeHalfCloseKeepsResponseFlowing(t *testing.T) {
	// A server that waits for EOF (client half-close), then replies and closes.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		one := make([]byte, 1)
		for {
			if _, err := conn.Read(one); err != nil {
				break // EOF: client half-closed
			}
		}
		_, _ = conn.Write([]byte("goodbye")) // still deliverable via the half-open conn
	}()

	egress, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer egress.Close()

	// Real TCP pair for the client side: net.Pipe conns lack CloseWrite.
	cln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer cln.Close()
	cCh := make(chan net.Conn, 1)
	go func() {
		c2, err := cln.Accept()
		if err != nil {
			cCh <- nil
			return
		}
		cCh <- c2
	}()
	c, err := net.Dial("tcp", cln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	srv := <-cCh
	if srv == nil {
		t.Fatal("accept failed")
	}
	defer srv.Close()

	done := make(chan struct{})
	go func() {
		Pipe(srv, egress, 0, nil)
		close(done)
	}()

	if _, err := c.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	// Half-close: with the old full-close relay this killed the tunnel before
	// "goodbye" could arrive.
	if err := c.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}

	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 7)
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("no response after half-close: %v", err)
	}
	if string(buf) != "goodbye" {
		t.Fatalf("got %q, want %q", string(buf), "goodbye")
	}
	c.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("pipe did not finish")
	}
	<-serverDone
}

// TestPipeSharedIdleDeadline: a one-directional transfer (client never
// writes) must not be killed by the idle timeout while data still flows.
func TestPipeSharedIdleDeadline(t *testing.T) {
	c, srv := net.Pipe()
	defer c.Close()

	const chunks = 8
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		defer srv.Close()
		for i := 0; i < chunks; i++ {
			srv.SetWriteDeadline(time.Now().Add(2 * time.Second))
			if _, err := srv.Write([]byte("chunk")); err != nil {
				return
			}
			time.Sleep(80 * time.Millisecond)
		}
	}()

	done := make(chan struct{})
	var up, down int64
	go func() {
		Pipe(c, srv, 250*time.Millisecond, func(u, d int64) { up, down = u, d; close(done) })
	}()

	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	got := 0
	buf := make([]byte, 5)
	for {
		n, err := c.Read(buf)
		got += n
		if err != nil {
			break
		}
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("pipe did not finish")
	}
	if got != chunks*5 {
		t.Fatalf("idle timeout killed an active one-directional transfer: got %d/%d bytes", got, chunks*5)
	}
	<-serverDone
	_ = up
	_ = down
}
