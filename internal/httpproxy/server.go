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

	transport *http.Transport
}

// NewHandler builds the handler and its lane-bound transport.
func NewHandler(egress lane.Egress, log *slog.Logger, idle time.Duration) *Handler {
	h := &Handler{Egress: egress, Log: log, Idle: idle}
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
		http.Error(w, "hijack failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer client.Close()

	resp := "HTTP/1.1 200 Connection established\r\n\r\n"
	if _, err := client.Write([]byte(resp)); err != nil {
		return
	}
	_ = rw.Flush()
	h.log().Debug("http tunnel established", "target", target)

	// Hijacked conns bypass net/http deadlines; relay owns them now.
	relay.Pipe(client, egress, h.Idle, nil)
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
	out.URL.Host = r.URL.Host
	out.URL.Scheme = r.URL.Scheme
	for _, k := range hopHeaders {
		out.Header.Del(k)
	}

	resp, err := h.transport.RoundTrip(out)
	if err != nil {
		if errors.Is(err, lane.ErrFailClosed) || isNetFail(err) {
			h.Egress.Reject("forward " + r.URL.Host + ": " + err.Error())
		}
		http.Error(w, "forward through lane failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		if strings.EqualFold(k, "Connection") {
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func isNetFail(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) || errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "status")
}
