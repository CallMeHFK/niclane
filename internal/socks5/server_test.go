package socks5

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/CallMeHFK/niclane/internal/config"
	"github.com/CallMeHFK/niclane/internal/lane"
	"github.com/CallMeHFK/niclane/internal/metrics"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
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

// startSocks5 launches a SOCKS5 server for the given lane config and returns
// its listen address.
func startSocks5(t *testing.T, lc *config.LaneConfig, auth *config.AuthConfig) string {
	t.Helper()
	l, err := lane.New(lc, discardLogger(), metrics.NewRegistry().Register(lc.Name))
	if err != nil {
		t.Fatal(err)
	}
	l.Refresh()
	ln, err := net.Listen("tcp", lc.Listen)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	srv := &Server{Egress: l, Auth: auth, Log: discardLogger(), ListenHost: "127.0.0.1"}
	go srv.Serve(ln)
	time.Sleep(50 * time.Millisecond)
	return ln.Addr().String()
}

// socks5Client speaks just enough SOCKS5 for the tests.
type socks5Client struct {
	t          *testing.T
	conn       net.Conn
	authFailed bool
}

func dialSocks5(t *testing.T, addr string, user, pass string) *socks5Client {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	c := &socks5Client{t: t, conn: conn}

	var methods []byte
	if user != "" {
		methods = []byte{0x00, 0x02}
	} else {
		methods = []byte{0x00}
	}
	c.mustWrite(append([]byte{0x05, byte(len(methods))}, methods...))
	resp := c.mustRead(2)
	if resp[1] == 0x02 {
		if user == "" {
			t.Fatal("server requested auth unexpectedly")
		}
		c.mustWrite(append([]byte{0x01, byte(len(user))}, append([]byte(user), append([]byte{byte(len(pass))}, []byte(pass)...)...)...))
		ares := c.mustRead(2)
		if ares[1] != 0x00 {
			return &socks5Client{t: t, conn: conn, authFailed: true}
		}
	} else if resp[1] != 0x00 {
		t.Fatalf("unexpected method %#x", resp[1])
	}
	return c
}

func (c *socks5Client) mustWrite(b []byte) {
	if _, err := c.conn.Write(b); err != nil {
		c.t.Fatal(err)
	}
}

func (c *socks5Client) mustRead(n int) []byte {
	buf := make([]byte, n)
	if _, err := io.ReadFull(c.conn, buf); err != nil {
		c.t.Fatal(err)
	}
	return buf
}

// connect sends a CONNECT request and expects success; returns nothing.
func (c *socks5Client) connect(host string, port uint16) {
	req := []byte{0x05, 0x01, 0x00, 0x01}
	req = append(req, net.ParseIP(host).To4()...)
	var pb [2]byte
	binary.BigEndian.PutUint16(pb[:], port)
	req = append(req, pb[:]...)
	c.mustWrite(req)
	rep := c.mustRead(10)
	if rep[1] != 0x00 {
		c.t.Fatalf("CONNECT failed, reply code %#x", rep[1])
	}
}

func (c *socks5Client) connectExpectCode(host string, port uint16) byte {
	req := []byte{0x05, 0x01, 0x00, 0x01}
	req = append(req, net.ParseIP(host).To4()...)
	var pb [2]byte
	binary.BigEndian.PutUint16(pb[:], port)
	req = append(req, pb[:]...)
	c.mustWrite(req)
	rep := c.mustRead(10)
	return rep[1]
}

func TestSocks5ConnectEcho(t *testing.T) {
	echoAddr := startEcho(t)
	echoHost, echoPortRaw, _ := net.SplitHostPort(echoAddr)
	echoPort := uint16(0)
	fmt.Sscanf(echoPortRaw, "%d", &echoPort)

	addr := startSocks5(t, &config.LaneConfig{
		Name: "lo", Listen: "127.0.0.1:0", BindIP: "127.0.0.1",
	}, nil)

	c := dialSocks5(t, addr, "", "")
	c.connect(echoHost, echoPort)
	msg := "hello over socks5"
	c.mustWrite([]byte(msg))
	buf := c.mustRead(len(msg))
	if string(buf) != msg {
		t.Fatalf("echo mismatch: %q", string(buf))
	}
}

func TestSocks5Auth(t *testing.T) {
	echoAddr := startEcho(t)
	echoHost, echoPortRaw, _ := net.SplitHostPort(echoAddr)
	var echoPort uint16
	fmt.Sscanf(echoPortRaw, "%d", &echoPort)

	addr := startSocks5(t, &config.LaneConfig{
		Name: "lo", Listen: "127.0.0.1:0", BindIP: "127.0.0.1",
	}, &config.AuthConfig{User: "han", Password: "secret"})

	// Wrong credentials are rejected at subnegotiation.
	bad := dialSocks5(t, addr, "han", "wrong")
	if !bad.authFailed {
		t.Fatal("auth with wrong password must be rejected at subnegotiation")
	}

	// Correct credentials pass and relay.
	good := dialSocks5(t, addr, "han", "secret")
	good.connect(echoHost, echoPort)
	good.mustWrite([]byte("ok"))
	if buf := good.mustRead(2); string(buf) != "ok" {
		t.Fatalf("echo mismatch: %q", string(buf))
	}
}

func TestSocks5FailClosed(t *testing.T) {
	_, portRaw, _ := net.SplitHostPort(startEcho(t))
	var echoPort uint16
	fmt.Sscanf(portRaw, "%d", &echoPort)

	addr := startSocks5(t, &config.LaneConfig{
		Name:      "dead",
		Listen:    "127.0.0.1:0",
		Interface: "niclane-no-such-iface",
	}, nil)

	c := dialSocks5(t, addr, "", "")
	code := c.connectExpectCode("127.0.0.1", echoPort)
	if code != 0x01 {
		t.Fatalf("fail-closed reply code = %#x, want 0x01 (general failure)", code)
	}
}

func TestSocks5UDPEcho(t *testing.T) {
	// UDP echo server on loopback.
	uln, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer uln.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := uln.ReadFromUDP(buf)
			if err != nil {
				return
			}
			uln.WriteToUDP(buf[:n], from)
		}
	}()

	addr := startSocks5(t, &config.LaneConfig{
		Name: "lo", Listen: "127.0.0.1:0", BindIP: "127.0.0.1",
	}, nil)

	c := dialSocks5(t, addr, "", "")
	// UDP ASSOCIATE with DST.ADDR 0.0.0.0:0.
	c.mustWrite([]byte{0x05, 0x03, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	rep := c.mustRead(10)
	if rep[1] != 0x00 {
		t.Fatalf("UDP ASSOCIATE failed: %#x", rep[1])
	}
	bndPort := binary.BigEndian.Uint16(rep[8:10])
	bndAddr := net.JoinHostPort(net.IP(rep[4:8]).String(), fmt.Sprint(bndPort))
	if net.IP(rep[4:8]).IsUnspecified() {
		// Server bound on a specific loopback IP; zero BND is also legal, but
		// here ListenHost is fixed to 127.0.0.1.
		bndAddr = net.JoinHostPort("127.0.0.1", fmt.Sprint(bndPort))
	}
	relayConn, err := net.Dial("udp", bndAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer relayConn.Close()
	relayConn.SetDeadline(time.Now().Add(5 * time.Second))

	// Wrap: RSV RSV FRAG ATYP=1 addr(4) port(2) payload.
	var pb [2]byte
	binary.BigEndian.PutUint16(pb[:], uint16(uln.LocalAddr().(*net.UDPAddr).Port))
	hdr := []byte{0, 0, 0, 0x01, 127, 0, 0, 1, pb[0], pb[1]}
	if _, err := relayConn.Write(append(hdr, []byte("ping")...)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1024)
	n, err := relayConn.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	// Response: header(10) + payload.
	if n < 14 || string(buf[n-4:n]) != "ping" {
		t.Fatalf("udp relay mismatch: %q", buf[:n])
	}
}

// startFakeDoH serves DNS-JSON answers mapping every name to 127.0.0.1.
func startFakeDoH(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{"Status": 0}
		if r.URL.Query().Get("type") == "1" {
			resp["Answer"] = []map[string]any{{
				"name": r.URL.Query().Get("name"), "type": 1, "data": "127.0.0.1",
			}}
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestSocks5UDPDomainTarget verifies that a domain target in a UDP ASSOCIATE
// datagram resolves through the lane's DNS mode (here: DoH), not the system
// resolver.
func TestSocks5UDPDomainTarget(t *testing.T) {
	uln, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer uln.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := uln.ReadFromUDP(buf)
			if err != nil {
				return
			}
			uln.WriteToUDP(buf[:n], from)
		}
	}()

	dohURL := startFakeDoH(t)
	addr := startSocks5(t, &config.LaneConfig{
		Name:    "lo",
		Listen:  "127.0.0.1:0",
		BindIP:  "127.0.0.1",
		DNS:     config.DNSDoH,
		DohURLs: []string{dohURL},
	}, nil)

	c := dialSocks5(t, addr, "", "")
	c.mustWrite([]byte{0x05, 0x03, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	rep := c.mustRead(10)
	if rep[1] != 0x00 {
		t.Fatalf("UDP ASSOCIATE failed: %#x", rep[1])
	}
	bndPort := binary.BigEndian.Uint16(rep[8:10])
	bndAddr := net.JoinHostPort("127.0.0.1", fmt.Sprint(bndPort))
	relayConn, err := net.Dial("udp", bndAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer relayConn.Close()
	relayConn.SetDeadline(time.Now().Add(5 * time.Second))

	// Domain-target datagram: ATYP=3, len=12, "test.example".
	var pb [2]byte
	binary.BigEndian.PutUint16(pb[:], uint16(uln.LocalAddr().(*net.UDPAddr).Port))
	hdr := []byte{0, 0, 0, 0x03, 12}
	hdr = append(hdr, []byte("test.example")...)
	hdr = append(hdr, pb[0], pb[1])
	if _, err := relayConn.Write(append(hdr, []byte("pong")...)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1024)
	n, err := relayConn.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if n < 14 || string(buf[n-4:n]) != "pong" {
		t.Fatalf("udp domain relay mismatch: %q", buf[:n])
	}
}
