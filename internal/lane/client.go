package lane

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"time"
)

// socks5ClientHandshake performs the client side of the SOCKS5 handshake on
// conn, requesting a CONNECT to target (host:port, host may be a domain).
func socks5ClientHandshake(conn net.Conn, target string, userinfo *url.Userinfo) error {
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	defer conn.SetDeadline(time.Time{})

	methods := []byte{0x00}
	if userinfo != nil {
		methods = []byte{0x00, 0x02}
	}
	if _, err := conn.Write(append([]byte{0x05, byte(len(methods))}, methods...)); err != nil {
		return fmt.Errorf("write greeting: %w", err)
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return fmt.Errorf("read method: %w", err)
	}
	if resp[0] != 0x05 {
		return fmt.Errorf("unexpected socks version %#x", resp[0])
	}
	switch resp[1] {
	case 0x00:
	case 0x02:
		if userinfo == nil {
			return fmt.Errorf("upstream requested auth but no credentials configured")
		}
		user, _ := userinfo.Password()
		if err := writeUnamePass(conn, userinfo.Username(), user); err != nil {
			return err
		}
		if err := expectUnamePassOK(conn); err != nil {
			return err
		}
	default:
		return fmt.Errorf("upstream rejected auth (method %#x)", resp[1])
	}

	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return fmt.Errorf("invalid target %q: %w", target, err)
	}
	port, err := parsePort(portStr)
	if err != nil {
		return err
	}

	req := []byte{0x05, 0x01, 0x00} // VER, CMD=CONNECT, RSV
	var atyp byte
	var addrPart []byte
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			atyp, addrPart = 0x01, ip4
		} else {
			atyp, addrPart = 0x04, ip.To16()
		}
	} else {
		atyp = 0x03
		addrPart = append([]byte{byte(len(host))}, host...)
	}
	req = append(req, atyp)
	req = append(req, addrPart...)
	var portb [2]byte
	binary.BigEndian.PutUint16(portb[:], port)
	req = append(req, portb[:]...)
	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("write connect: %w", err)
	}

	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return fmt.Errorf("read reply: %w", err)
	}
	if head[0] != 0x05 {
		return fmt.Errorf("unexpected upstream reply version %#x", head[0])
	}
	if head[1] != 0x00 {
		return fmt.Errorf("upstream connect failed, socks reply %#x", head[1])
	}
	switch head[3] {
	case 0x01:
		if _, err := io.CopyN(io.Discard, conn, 4+2); err != nil {
			return fmt.Errorf("read reply bnd: %w", err)
		}
	case 0x03:
		lenb := make([]byte, 1)
		if _, err := io.ReadFull(conn, lenb); err != nil {
			return fmt.Errorf("read reply bnd: %w", err)
		}
		if _, err := io.CopyN(io.Discard, conn, int64(lenb[0])+2); err != nil {
			return fmt.Errorf("read reply bnd: %w", err)
		}
	case 0x04:
		if _, err := io.CopyN(io.Discard, conn, 16+2); err != nil {
			return fmt.Errorf("read reply bnd: %w", err)
		}
	default:
		return fmt.Errorf("malformed upstream reply atyp %#x", head[3])
	}
	return nil
}

func writeUnamePass(conn net.Conn, user, pass string) error {
	if len(user) > 255 || len(pass) > 255 {
		return fmt.Errorf("upstream credentials too long")
	}
	buf := []byte{0x01, byte(len(user))}
	buf = append(buf, user...)
	buf = append(buf, byte(len(pass)))
	buf = append(buf, pass...)
	if _, err := conn.Write(buf); err != nil {
		return fmt.Errorf("write upstream auth: %w", err)
	}
	return nil
}

func expectUnamePassOK(conn net.Conn) error {
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return fmt.Errorf("read upstream auth reply: %w", err)
	}
	if resp[1] != 0x00 {
		return fmt.Errorf("upstream auth rejected (status %#x)", resp[1])
	}
	return nil
}

// httpConnectHandshake performs an HTTP CONNECT to target through conn.
func httpConnectHandshake(ctx context.Context, conn net.Conn, target string, userinfo *url.Userinfo) error {
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	defer conn.SetDeadline(time.Time{})

	req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\nUser-Agent: niclane\r\n", target, target)
	if userinfo != nil {
		pass, _ := userinfo.Password()
		token := base64.StdEncoding.EncodeToString([]byte(userinfo.Username() + ":" + pass))
		req += "Proxy-Authorization: Basic " + token + "\r\n"
	}
	req += "\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		return fmt.Errorf("write CONNECT: %w", err)
	}
	br := newByteReader(conn)
	status, err := br.readResponseHead()
	if err != nil {
		return err
	}
	if len(status) < 12 || status[:5] != "HTTP/" {
		return fmt.Errorf("malformed upstream CONNECT response: %q", truncate(status, 80))
	}
	// "HTTP/1.1 200 ..." — status code at offset 9..12.
	if status[9:12] != "200" {
		return fmt.Errorf("upstream CONNECT failed: %q", truncate(firstLine(status), 120))
	}
	return nil
}

func parsePort(s string) (uint16, error) {
	p, err := strconv.Atoi(s)
	if err != nil || p <= 0 || p > 65535 {
		return 0, fmt.Errorf("invalid port %q", s)
	}
	return uint16(p), nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\r' || s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}

// byteReader reads until CRLFCRLF without buffering past the header, so no
// pipelined bytes are lost.
type byteReader struct {
	conn io.Reader
}

func newByteReader(conn io.Reader) *byteReader { return &byteReader{conn: conn} }

func (b *byteReader) readResponseHead() (string, error) {
	buf := make([]byte, 1)
	var head []byte
	for {
		n, err := b.conn.Read(buf)
		if n > 0 {
			head = append(head, buf[:n]...)
			if len(head) >= 4 && string(head[len(head)-4:]) == "\r\n\r\n" {
				return string(head), nil
			}
			if len(head) > 16*1024 {
				return "", fmt.Errorf("upstream response head too large")
			}
		}
		if err != nil {
			if len(head) == 0 && err == io.EOF {
				return "", fmt.Errorf("upstream closed during CONNECT")
			}
			return "", fmt.Errorf("read CONNECT response: %w", err)
		}
	}
}
