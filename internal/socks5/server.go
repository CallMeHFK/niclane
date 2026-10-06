// Package socks5 implements the inbound SOCKS5 server (RFC 1928) with
// CONNECT and UDP ASSOCIATE, relaying through a lane.Egress.
package socks5

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/CallMeHFK/niclane/internal/config"
	"github.com/CallMeHFK/niclane/internal/lane"
	"github.com/CallMeHFK/niclane/internal/relay"
)

// SOCKS5 protocol constants.
const (
	ver5          = 0x05
	cmdConnect    = 0x01
	cmdBind       = 0x02
	cmdUDP        = 0x03
	atypIPv4      = 0x01
	atypDomain    = 0x03
	atypIPv6      = 0x04
	methodNone    = 0x00
	methodUserPas = 0x02
	methodReject  = 0xFF
)

// Reply codes.
const (
	repSucceeded       = 0x00
	repGeneralFailure  = 0x01
	repNotAllowed      = 0x02
	repNetUnreachable  = 0x03
	repHostUnreachable = 0x04
	repRefused         = 0x05
	repTTLExpired      = 0x06
	repCmdNotSupported = 0x07
)

// Server serves SOCKS5 for one lane.
type Server struct {
	Egress lane.Egress
	Auth   *config.AuthConfig
	Log    *slog.Logger
	Idle   time.Duration
	// ListenHost is the host the TCP listener binds ("0.0.0.0" when
	// unspecified); the client-facing UDP socket binds the same address.
	ListenHost string
}

// Serve accepts connections until the listener is closed.
func (s *Server) Serve(ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go s.handle(conn)
	}
}

func (s *Server) log() *slog.Logger {
	if s.Log == nil {
		return slog.Default()
	}
	return s.Log
}

func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return
	}
	if err := s.auth(conn); err != nil {
		s.reject("auth", err)
		return
	}
	req, err := readRequest(conn)
	if err != nil {
		s.reject("request", err)
		return
	}
	// Handshake done; tunnels may live arbitrarily long.
	_ = conn.SetDeadline(time.Time{})

	switch req.cmd {
	case cmdConnect:
		s.handleConnect(conn, req)
	case cmdUDP:
		s.handleUDP(conn)
	default:
		_ = s.reply(conn, repCmdNotSupported, nil, 0)
		s.Egress.Reject("unsupported command")
	}
}

func (s *Server) reject(stage string, err error) {
	s.Egress.Reject(stage + ": " + err.Error())
	s.log().Debug("socks5 reject", "stage", stage, "error", err)
}

// auth performs method negotiation and optional username/password auth.
func (s *Server) auth(conn net.Conn) error {
	head := make([]byte, 2)
	if _, err := io.ReadFull(conn, head); err != nil {
		return fmt.Errorf("read greeting: %w", err)
	}
	if head[0] != ver5 {
		return fmt.Errorf("unsupported socks version %#x", head[0])
	}
	methods := make([]byte, int(head[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return fmt.Errorf("read methods: %w", err)
	}

	want := byte(methodNone)
	if s.Auth != nil {
		want = methodUserPas
	}
	found := false
	for _, m := range methods {
		if m == want {
			found = true
			break
		}
	}
	if !found {
		_, _ = conn.Write([]byte{ver5, methodReject})
		return errors.New("no acceptable auth method")
	}
	if _, err := conn.Write([]byte{ver5, want}); err != nil {
		return err
	}
	if want == methodUserPas {
		if err := s.userPass(conn); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) userPass(conn net.Conn) error {
	head := make([]byte, 2)
	if _, err := io.ReadFull(conn, head); err != nil || head[0] != 0x01 {
		return errors.New("malformed username/password subnegotiation")
	}
	user := make([]byte, int(head[1]))
	if _, err := io.ReadFull(conn, user); err != nil {
		return fmt.Errorf("read username: %w", err)
	}
	plen := make([]byte, 1)
	if _, err := io.ReadFull(conn, plen); err != nil {
		return fmt.Errorf("read password len: %w", err)
	}
	pass := make([]byte, int(plen[0]))
	if _, err := io.ReadFull(conn, pass); err != nil {
		return fmt.Errorf("read password: %w", err)
	}
	if string(user) != s.Auth.User || string(pass) != s.Auth.Password {
		_, _ = conn.Write([]byte{0x01, 0x01})
		return errors.New("auth failed")
	}
	_, err := conn.Write([]byte{0x01, 0x00})
	return err
}

type socksRequest struct {
	cmd  byte
	host string
	port uint16
}

func readRequest(conn net.Conn) (*socksRequest, error) {
	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return nil, fmt.Errorf("read request: %w", err)
	}
	if head[0] != ver5 {
		return nil, fmt.Errorf("unexpected request ver %#x", head[0])
	}
	req := &socksRequest{cmd: head[1]}
	switch head[3] {
	case atypIPv4:
		b := make([]byte, 4)
		if _, err := io.ReadFull(conn, b); err != nil {
			return nil, fmt.Errorf("read ipv4: %w", err)
		}
		req.host = net.IP(b).String()
	case atypDomain:
		l := make([]byte, 1)
		if _, err := io.ReadFull(conn, l); err != nil {
			return nil, fmt.Errorf("read domain len: %w", err)
		}
		b := make([]byte, int(l[0]))
		if _, err := io.ReadFull(conn, b); err != nil {
			return nil, fmt.Errorf("read domain: %w", err)
		}
		req.host = string(b)
	case atypIPv6:
		b := make([]byte, 16)
		if _, err := io.ReadFull(conn, b); err != nil {
			return nil, fmt.Errorf("read ipv6: %w", err)
		}
		req.host = net.IP(b).String()
	default:
		return nil, fmt.Errorf("unsupported atyp %#x", head[3])
	}
	pb := make([]byte, 2)
	if _, err := io.ReadFull(conn, pb); err != nil {
		return nil, fmt.Errorf("read port: %w", err)
	}
	req.port = binary.BigEndian.Uint16(pb)
	return req, nil
}

func (s *Server) reply(conn net.Conn, rep byte, bnd net.IP, port uint16) error {
	if bnd == nil {
		bnd = net.IPv4zero
	}
	var atyp byte = atypIPv4
	addr := bnd.To4()
	if addr == nil {
		atyp = atypIPv6
		addr = bnd.To16()
	}
	out := []byte{ver5, rep, 0x00, atyp}
	out = append(out, addr...)
	var pb [2]byte
	binary.BigEndian.PutUint16(pb[:], port)
	out = append(out, pb[:]...)
	_, err := conn.Write(out)
	return err
}

// replyFor maps a dial error to a SOCKS5 reply code.
func replyFor(err error) byte {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return repTTLExpired
	}
	if errors.Is(err, lane.ErrFailClosed) {
		return repGeneralFailure
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "connection refused"):
		return repRefused
	case strings.Contains(msg, "network is unreachable"), strings.Contains(msg, "network unreachable"):
		return repNetUnreachable
	case strings.Contains(msg, "no route to host"), strings.Contains(msg, "host unreachable"):
		return repHostUnreachable
	default:
		return repGeneralFailure
	}
}

func (s *Server) handleConnect(conn net.Conn, req *socksRequest) {
	target := net.JoinHostPort(req.host, fmt.Sprint(req.port))
	egress, err := s.Egress.DialContext(context.Background(), "tcp", target)
	if err != nil {
		_ = s.reply(conn, replyFor(err), nil, 0)
		s.Egress.Reject("connect " + target + ": " + err.Error())
		s.log().Debug("connect failed", "target", target, "error", err)
		return
	}
	defer egress.Close()

	if err := s.reply(conn, repSucceeded, localIP(egress), 0); err != nil {
		return
	}
	s.log().Debug("tunnel established", "target", target)
	relay.Pipe(conn, egress, s.Idle, nil)
}

func localIP(conn net.Conn) net.IP {
	if addr, ok := conn.LocalAddr().(*net.TCPAddr); ok {
		return addr.IP
	}
	return nil
}

// handleUDP implements UDP ASSOCIATE. Two UDP sockets are involved: a
// client-facing socket on the listener address, and a lane-bound egress
// socket. Only the first observed client address is served; fragmentation is
// rejected. UDP target resolution uses the system resolver (documented
// limitation); the payload itself always egresses through the lane.
func (s *Server) handleUDP(conn net.Conn) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clientIP := net.ParseIP(s.ListenHost)
	cuc, err := net.ListenUDP("udp", &net.UDPAddr{IP: clientIP})
	if err != nil {
		_ = s.reply(conn, repGeneralFailure, nil, 0)
		s.Egress.Reject("udp associate: listen client socket: " + err.Error())
		return
	}
	defer cuc.Close()

	uc, err := s.Egress.ListenUDP(ctx)
	if err != nil {
		_ = s.reply(conn, replyFor(err), nil, 0)
		s.Egress.Reject("udp associate: " + err.Error())
		return
	}
	defer uc.Close()

	bnd := clientIP
	if bnd == nil || bnd.IsUnspecified() {
		bnd = net.IPv4zero
	}
	if err := s.reply(conn, repSucceeded, bnd, uint16(cuc.LocalAddr().(*net.UDPAddr).Port)); err != nil {
		return
	}

	stats := s.Egress.Stats()
	var clientAddr *net.UDPAddr
	var mu sync.Mutex

	// Tear the association down when the TCP control connection closes.
	go func() {
		one := make([]byte, 1)
		for {
			if _, err := conn.Read(one); err != nil {
				cancel()
				return
			}
		}
	}()

	// Egress -> client.
	go func() {
		buf := make([]byte, 65535)
		for {
			n, from, err := uc.ReadFromUDP(buf)
			if err != nil {
				return
			}
			mu.Lock()
			dst := clientAddr
			mu.Unlock()
			if dst == nil {
				continue
			}
			packet := wrapUDPHeader(from, buf[:n])
			if _, err := cuc.WriteToUDP(packet, dst); err == nil {
				stats.UDPRelays.Add(1)
				stats.BytesDown.Add(int64(n))
			}
		}
	}()

	// Client -> egress.
	buf := make([]byte, 65535)
	for {
		n, from, err := cuc.ReadFromUDP(buf)
		if err != nil {
			return
		}
		mu.Lock()
		if clientAddr == nil {
			clientAddr = from
		}
		known := clientAddr.String() == from.String()
		mu.Unlock()
		if !known {
			s.Egress.Reject("udp datagram from unexpected source " + from.String())
			continue
		}
		if n < 4 || buf[0] != 0x00 || buf[1] != 0x00 {
			continue
		}
		if buf[2] != 0x00 {
			// Fragmentation unsupported.
			s.Egress.Reject("udp fragmentation requested")
			continue
		}
		payload, dst, err := parseUDPHeader(buf[:n])
		if err != nil {
			s.Egress.Reject("udp header: " + err.Error())
			continue
		}
		if _, err := uc.WriteToUDP(payload, dst); err == nil {
			stats.UDPRelays.Add(1)
			stats.BytesUp.Add(int64(len(payload)))
		}
	}
}

// parseUDPHeader parses the SOCKS5 datagram header and resolves the target.
func parseUDPHeader(pkt []byte) ([]byte, *net.UDPAddr, error) {
	atyp := pkt[3]
	var host string
	var rest []byte
	switch atyp {
	case atypIPv4:
		if len(pkt) < 10 {
			return nil, nil, errors.New("short ipv4 datagram")
		}
		host = net.IP(pkt[4:8]).String()
		rest = pkt[8:]
	case atypDomain:
		if len(pkt) < 5 || len(pkt) < 5+int(pkt[4])+2 {
			return nil, nil, errors.New("short domain datagram")
		}
		host = string(pkt[5 : 5+int(pkt[4])])
		rest = pkt[5+int(pkt[4]):]
	case atypIPv6:
		if len(pkt) < 22 {
			return nil, nil, errors.New("short ipv6 datagram")
		}
		host = net.IP(pkt[4:20]).String()
		rest = pkt[20:]
	default:
		return nil, nil, fmt.Errorf("unsupported udp atyp %#x", atyp)
	}
	if len(rest) < 2 {
		return nil, nil, errors.New("missing udp port")
	}
	port := binary.BigEndian.Uint16(rest[:2])
	dst, err := net.ResolveUDPAddr("udp", net.JoinHostPort(host, fmt.Sprint(port)))
	if err != nil {
		return nil, nil, fmt.Errorf("resolve udp target: %w", err)
	}
	return rest[2:], dst, nil
}

// wrapUDPHeader prepends a SOCKS5 datagram header for the given source.
func wrapUDPHeader(from *net.UDPAddr, payload []byte) []byte {
	var atyp byte = atypIPv4
	addr := from.IP.To4()
	if addr == nil {
		atyp = atypIPv6
		addr = from.IP.To16()
	}
	out := make([]byte, 0, 4+len(addr)+2+len(payload))
	out = append(out, 0x00, 0x00, 0x00, atyp)
	out = append(out, addr...)
	var pb [2]byte
	binary.BigEndian.PutUint16(pb[:], uint16(from.Port))
	out = append(out, pb[:]...)
	return append(out, payload...)
}
