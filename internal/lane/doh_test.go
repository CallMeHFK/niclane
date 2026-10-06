package lane

import (
	"context"
	"encoding/json"

	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/CallMeHFK/niclane/internal/config"
	"github.com/CallMeHFK/niclane/internal/metrics"
)

// startFakeDoH serves DNS-JSON answers: every name maps to 127.0.0.1 (A).
func startFakeDoH(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		qtype := r.URL.Query().Get("type")
		if r.Header.Get("Accept") != "application/dns-json" {
			http.Error(w, "missing accept header", http.StatusBadRequest)
			return
		}
		resp := dohResponse{Status: 0}
		if qtype == "1" { // A
			resp.Answer = append(resp.Answer, struct {
				Name string `json:"name"`
				Type int    `json:"type"`
				Data string `json:"data"`
			}{Name: name, Type: 1, Data: "127.0.0.1"})
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func newTestLane(t *testing.T, lc *config.LaneConfig) *Lane {
	t.Helper()
	l, err := New(lc, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.NewRegistry().Register(lc.Name))
	if err != nil {
		t.Fatal(err)
	}
	l.Refresh()
	return l
}

func TestDoHResolutionThroughLane(t *testing.T) {
	dohURL := startFakeDoH(t)
	echoAddr := startEcho(t)
	_, echoPort, _ := net.SplitHostPort(echoAddr)

	l := newTestLane(t, &config.LaneConfig{
		Name:    "lo",
		Listen:  "127.0.0.1:0",
		BindIP:  "127.0.0.1",
		DNS:     config.DNSDoH,
		DohURLs: []string{dohURL},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// The hostname only exists inside the fake DoH server; if the lane
	// resolved any other way the dial would fail.
	conn, err := l.DialContext(ctx, "tcp", net.JoinHostPort("test.example", echoPort))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	msg := "via-doh"
	if _, err := conn.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(msg))
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Read(buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != msg {
		t.Fatalf("echo mismatch: %q", string(buf))
	}
	if l.Stats().DNSQueries.Load() == 0 {
		t.Fatal("dns_queries_total not counted")
	}
}

func TestDoHFailsClosedOnBadEndpoint(t *testing.T) {
	// Port 1 on loopback: nothing listens there; the DoH dial must fail.
	l := newTestLane(t, &config.LaneConfig{
		Name:    "lo",
		Listen:  "127.0.0.1:0",
		BindIP:  "127.0.0.1",
		DNS:     config.DNSDoH,
		DohURLs: []string{"http://127.0.0.1:1/resolve"},
	})
	_, err := l.Resolve(context.Background(), "tcp", "test.example")
	if err == nil {
		t.Fatal("resolve via unreachable DoH must fail")
	}
}

func TestDoHDefaultsFromConfig(t *testing.T) {
	lc := &config.LaneConfig{Name: "lo", Listen: "127.0.0.1:0", BindIP: "127.0.0.1", DNS: config.DNSDoH}
	if got := lc.EffectiveDoHURLs(); len(got) == 0 || got[0] != config.DefaultDoHURLs[0] {
		t.Fatalf("default doh urls = %v", got)
	}
}
