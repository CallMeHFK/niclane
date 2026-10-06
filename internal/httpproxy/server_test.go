package httpproxy

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/CallMeHFK/niclane/internal/config"
	"github.com/CallMeHFK/niclane/internal/lane"
	"github.com/CallMeHFK/niclane/internal/metrics"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newLane(t *testing.T, lc *config.LaneConfig) *lane.Lane {
	t.Helper()
	l, err := lane.New(lc, discardLogger(), metrics.NewRegistry().Register(lc.Name))
	if err != nil {
		t.Fatal(err)
	}
	l.Refresh()
	return l
}

// startProxy launches an HTTP proxy handler on 127.0.0.1 and returns its URL.
func startProxy(t *testing.T, egress lane.Egress) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(NewHandler(egress, discardLogger(), 0))
	t.Cleanup(srv.Close)
	return srv
}

func TestHTTPForwardAbsoluteURI(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Backend", "yes")
		_, _ = io.WriteString(w, "from-backend")
	}))
	defer backend.Close()

	l := newLane(t, &config.LaneConfig{Name: "lo", Listen: "127.0.0.1:0", BindIP: "127.0.0.1"})
	proxy := startProxy(t, l)

	req, err := http.NewRequest(http.MethodGet, backend.URL+"/hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Send the request through the proxy with the standard proxy transport.
	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(mustURL(t, proxy.URL))},
		Timeout:   5 * time.Second,
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "from-backend" {
		t.Fatalf("body = %q", string(body))
	}
	if resp.Header.Get("X-Backend") != "yes" {
		t.Fatal("backend header lost")
	}
}

func TestHTTPConnectTunnel(t *testing.T) {
	// A raw TCP echo backend behind CONNECT.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
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
						c.Write(buf[:n])
					}
					if err != nil {
						return
					}
				}
			}(conn)
		}
	}()

	l := newLane(t, &config.LaneConfig{Name: "lo", Listen: "127.0.0.1:0", BindIP: "127.0.0.1"})
	proxy := startProxy(t, l)

	conn, err := net.Dial("tcp", proxy.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	connect := "CONNECT " + ln.Addr().String() + " HTTP/1.1\r\nHost: " + ln.Addr().String() + "\r\n\r\n"
	if _, err := conn.Write([]byte(connect)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1024)
	n, _ := conn.Read(buf)
	if string(buf[:min(n, 4)]) != "HTTP" {
		t.Fatalf("unexpected CONNECT response: %q", buf[:n])
	}
	// Scan past the header terminator.
	head := string(buf[:n])
	idx := -1
	for i := 0; i+3 < n; i++ {
		if buf[i] == '\r' && buf[i+1] == '\n' && buf[i+2] == '\r' && buf[i+3] == '\n' {
			idx = i + 4
			break
		}
	}
	if idx == -1 {
		// Read more until terminator.
		extra := make([]byte, 1024)
		m, _ := conn.Read(extra)
		head += string(extra[:m])
		n += m
		for i := 0; i+3 < n; i++ {
			if buf[i] == '\r' && buf[i+1] == '\n' && buf[i+2] == '\r' && buf[i+3] == '\n' {
				idx = n // whole reply consumed; payload comes next
				break
			}
		}
		if idx == -1 {
			t.Fatalf("no CONNECT header terminator: %q", head)
		}
	}
	if _, err := conn.Write([]byte("tunnel-ping")); err != nil {
		t.Fatal(err)
	}
	m, err := conn.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:m]) != "tunnel-ping" {
		t.Fatalf("echo mismatch: %q", string(buf[:m]))
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
