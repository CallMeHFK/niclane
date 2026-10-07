// Package httpproxy implements the inbound HTTP proxy (CONNECT tunnels plus
// absolute-URI forwarding) over a lane.Egress.
package httpproxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/textproto"
	"strings"
	"time"

	"github.com/CallMeHFK/niclane/internal/lane"
	"github.com/CallMeHFK/niclane/internal/relay"
)

// Handler serves HTTP proxy requests for one lane.
type Handler struct {
	Egress lane.Egress
	Log    *slog.Logger
	Idle   time.Duration
	// Ctx, when set, aborts in-flight tunnels and forwards when the lane is
	// stopped (config reload / shutdown).
	Ctx context.Context

	transport *http.Transport
}

// NewHandler builds the handler and its lane-bound transport.
func NewHandler(egress lane.Egress, log *slog.Logger, idle time.Duration, ctx context.Context) *Handler {
	h := &Handler{Egress: egress, Log: log, Idle: idle, Ctx: ctx}
	h.transport = &http.Transport{
		Proxy:               nil, // the lane already applies any upstream
		DialContext:         egress.DialContext,
		ForceAttemptHTTP2:   true,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	return h
}

var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive",
	"Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// removeHopHeaders deletes hop-by-hop headers, including those named in the
// Connection header's token list (RFC 7230 §6.1).
func removeHopHeaders(h http.Header) {
	tokens := h.Get("Connection")
	for _, k := range hopHeaders {
		h.Del(k)
	}
	for _, tok := range strings.Split(tokens, ",") {
		if k := textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(tok)); k != "" {
			h.Del(k)
		}
	}
}

func (h *Handler) log() *slog.Logger {
	if h.Log == nil {
		return slog.Default()
	}
	return h.Log
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		h.handleConnect(w, r)
		return
	}
	h.handleForward(w, r)
}

// handleConnect tunnels TCP after an accepted CONNECT.
func (h *Handler) handleConnect(w http.ResponseWriter, r *http.Request) {
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
		port = "443"
	}
	target := net.JoinHostPort(host, port)

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}

	// Dial first: reject with 502 before hijacking on failure.
	egress, err := h.Egress.DialContext(r.Context(), "tcp", target)
	if err != nil {
		h.Egress.Reject("connect " + target + ": " + err.Error())
		http.Error(w, "dial through lane failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer egress.Close()

	client, rw, err := hj.Hijack()
	if err != nil {
		// The connection is unusable; the ResponseWriter may be detached, so
		// do not attempt to write through it.
		h.Egress.Reject("hijack failed: " + err.Error())
		return
	}
	defer client.Close()

	// Bytes the client pipelined past the CONNECT header must not be lost.
	src := &prefixConn{Conn: client, r: io.MultiReader(rw.Reader, client)}

	resp := "HTTP/1.1 200 Connection established\r\n\r\n"
	if _, err := client.Write([]byte(resp)); err != nil {
		return
	}
	h.log().Debug("http tunnel established", "target", target)

	if h.Ctx != nil {
		ctx := h.Ctx
		go func() {
			<-ctx.Done()
			_ = src.Close()
		}()
	}
	// Hijacked conns bypass net/http deadlines; relay owns them now.
	relay.Pipe(src, egress, h.Idle, nil)
}

// handleForward proxies an absolute-URI request (plain HTTP proxying).
func (h *Handler) handleForward(w http.ResponseWriter, r *http.Request) {
	if !r.URL.IsAbs() || r.URL.Host == "" {
		h.Egress.Reject("non-absolute request URI")
		http.Error(w, "niclane: this endpoint is an HTTP proxy; requests must use absolute URIs", http.StatusBadRequest)
		return
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	removeHopHeaders(out.Header)

	resp, err := h.transport.RoundTrip(out)
	if err != nil {
		if isLaneFail(err) {
			h.Egress.Reject("forward " + r.URL.Host + ": " + err.Error())
		}
		http.Error(w, "forward through lane failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	removeHopHeaders(resp.Header)
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	// Stream with explicit flushes so slow responses are not buffered whole.
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			return
		}
	}
}

// prefixConn prepends buffered bytes (pipelined past the CONNECT header) to
// the underlying connection's stream.
type prefixConn struct {
	net.Conn
	r io.Reader
}

func (p *prefixConn) Read(b []byte) (int, error) { return p.r.Read(b) }

// isLaneFail reports whether the error means the lane could not carry the
// request (as opposed to the remote endpoint failing).
func isLaneFail(err error) bool {
	if errors.Is(err, lane.ErrFailClosed) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne)
}
