// Package supervisor owns the runtime state of `niclane serve`: it starts
// lanes and the admin endpoint, and reconciles them against the config file
// on SIGHUP hot-reloads.
package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"reflect"
	"sync"
	"time"

	"github.com/CallMeHFK/niclane/internal/config"
	"github.com/CallMeHFK/niclane/internal/httpproxy"
	"github.com/CallMeHFK/niclane/internal/lane"
	"github.com/CallMeHFK/niclane/internal/metrics"
	"github.com/CallMeHFK/niclane/internal/socks5"
	"github.com/CallMeHFK/niclane/internal/version"
)

// Status is the JSON shape served at /status.
type Status struct {
	Version string       `json:"version"`
	Lanes   []LaneStatus `json:"lanes"`
}

// LaneStatus is the runtime state of one lane.
type LaneStatus struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Egress      string `json:"egress"`
	Healthy     bool   `json:"healthy"`
	BindMode    string `json:"bind_mode"`
	LastErr     string `json:"last_error,omitempty"`
	ConnsTotal  int64  `json:"connections_total"`
	ConnsActive int64  `json:"connections_active"`
	Rejects     int64  `json:"rejects_total"`
	BytesUp     int64  `json:"bytes_up"`
	BytesDown   int64  `json:"bytes_down"`
	DNSQueries  int64  `json:"dns_queries_total"`
	UDPRelays   int64  `json:"udp_relays_total"`
}

type instance struct {
	cfg      *config.LaneConfig
	cancel   context.CancelFunc
	listener net.Listener
	lane     *lane.Lane
}

func (i *instance) stop() {
	_ = i.listener.Close()
	i.cancel()
}

// Supervisor manages all running lanes and the admin endpoint.
type Supervisor struct {
	path string
	log  *slog.Logger
	reg  *metrics.Registry

	mu        sync.Mutex
	cfg       *config.Config
	rootCtx   context.Context
	lanes     map[string]*instance
	adminLn   net.Listener
	adminAddr string
}

// New loads the initial config and returns a supervisor (not yet started).
func New(path string, log *slog.Logger) (*Supervisor, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	return &Supervisor{
		path:  path,
		log:   log,
		reg:   metrics.NewRegistry(),
		cfg:   cfg,
		lanes: make(map[string]*instance),
	}, nil
}

// Start applies the initial configuration. The supervisor runs until Stop or
// until the parent context is cancelled.
func (s *Supervisor) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rootCtx = ctx
	return s.applyLocked(s.cfg)
}

// Reload re-reads the config file and reconciles running lanes. On any error
// the previous configuration stays running.
func (s *Supervisor) Reload() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rootCtx == nil {
		return errors.New("supervisor not started")
	}
	cfg, err := config.Load(s.path)
	if err != nil {
		return fmt.Errorf("keeping previous config: %w", err)
	}
	if err := s.applyLocked(cfg); err != nil {
		return fmt.Errorf("keeping previous config: %w", err)
	}
	return nil
}

// Stop shuts down every lane and the admin endpoint.
func (s *Supervisor) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, inst := range s.lanes {
		inst.stop()
		delete(s.lanes, name)
	}
	if s.adminLn != nil {
		_ = s.adminLn.Close()
		s.adminLn = nil
	}
}

// applyLocked reconciles the running state towards cfg; s.mu must be held.
func (s *Supervisor) applyLocked(cfg *config.Config) error {
	desired := make(map[string]*config.LaneConfig, len(cfg.Lanes))
	for _, lc := range cfg.Lanes {
		desired[lc.Name] = lc
	}

	// Lanes to stop: removed, or present with a changed config.
	stopping := make(map[string]*instance)
	for name, inst := range s.lanes {
		lc, keep := desired[name]
		if keep && reflect.DeepEqual(inst.cfg, lc) {
			continue
		}
		stopping[name] = inst
	}

	// Pre-flight new/changed listeners that don't collide with an address
	// about to be freed, so a bad listen port aborts the reload cleanly.
	type preflighted struct {
		lc *config.LaneConfig
		ln net.Listener
	}
	var fresh []preflighted
	for name, lc := range desired {
		if inst, running := s.lanes[name]; running && reflect.DeepEqual(inst.cfg, lc) {
			continue // unchanged: keep as-is (metrics continuity)
		}
		collides := false
		for _, st := range stopping {
			if st.cfg.Listen == lc.Listen {
				collides = true
				break
			}
		}
		if collides {
			continue // created after the old listener is closed
		}
		ln, err := net.Listen("tcp", lc.Listen)
		if err != nil {
			for _, f := range fresh {
				_ = f.ln.Close()
			}
			return fmt.Errorf("lane %q: listen: %w", name, err)
		}
		fresh = append(fresh, preflighted{lc, ln})
	}

	// Phase 2: stop removed/changed lanes.
	for name, inst := range stopping {
		inst.stop()
		delete(s.lanes, name)
	}

	// Phase 3: start pre-flighted lanes, then lanes whose port was freed.
	for _, f := range fresh {
		s.startLane(f.lc, f.ln)
	}
	for name, lc := range desired {
		if _, running := s.lanes[name]; running {
			continue
		}
		ln, err := net.Listen("tcp", lc.Listen)
		if err != nil {
			s.log.Error("lane start failed during reload", "lane", name, "error", err)
			continue
		}
		s.startLane(lc, ln)
	}

	// Phase 4: admin endpoint.
	if err := s.reconcileAdmin(cfg); err != nil {
		return err
	}

	s.cfg = cfg
	return nil
}

func (s *Supervisor) startLane(lc *config.LaneConfig, ln net.Listener) {
	st := s.reg.Register(lc.Name)
	l, err := lane.New(lc, s.log, st)
	if err != nil {
		_ = ln.Close()
		s.log.Error("lane creation failed", "lane", lc.Name, "error", err)
		return
	}
	ctx, cancel := context.WithCancel(s.rootCtx)
	l.Refresh() // populate health metrics before the first Status() read
	go l.Run(ctx)
	go serveLane(l, ln, lc, s.log)
	s.lanes[lc.Name] = &instance{cfg: lc, cancel: cancel, listener: ln, lane: l}
	s.log.Info("lane listening", "listen", lc.Listen, "type", lc.TypeOrDefault(),
		"egress", EgressDesc(lc), "strict", lc.StrictEnabled())
}

func (s *Supervisor) reconcileAdmin(cfg *config.Config) error {
	if cfg.Admin == s.adminAddr {
		return nil
	}
	if s.adminLn != nil {
		_ = s.adminLn.Close()
		s.adminLn = nil
		s.adminAddr = ""
	}
	if cfg.Admin == "" {
		return nil
	}
	ln, err := net.Listen("tcp", cfg.Admin)
	if err != nil {
		return fmt.Errorf("admin: listen: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = fmt.Fprint(w, s.reg.RenderPrometheus(version.Version))
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.Status())
	})
	s.adminLn = ln
	s.adminAddr = cfg.Admin
	go func() {
		s.log.Info("admin endpoint", "addr", cfg.Admin, "paths", "/metrics /status")
		_ = (&http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}).Serve(ln)
	}()
	return nil
}

// serveLane runs the protocol loop of one lane on its listener.
func serveLane(l *lane.Lane, ln net.Listener, lc *config.LaneConfig, log *slog.Logger) {
	idle, _ := lc.IdleTimeoutDur()
	switch lc.TypeOrDefault() {
	case config.TypeSocks5:
		host, _, err := net.SplitHostPort(lc.Listen)
		if err != nil || host == "" {
			host = "0.0.0.0"
		}
		srv := &socks5.Server{Egress: l, Auth: lc.Auth, Log: log, Idle: idle, ListenHost: host}
		if err := srv.Serve(ln); err != nil && !errors.Is(err, net.ErrClosed) {
			log.Error("socks5 lane stopped", "lane", lc.Name, "error", err)
		}
	case config.TypeHTTP:
		srv := &http.Server{Handler: httpproxy.NewHandler(l, log, idle), ReadHeaderTimeout: 30 * time.Second}
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			log.Error("http lane stopped", "lane", lc.Name, "error", err)
		}
	}
}

// EgressDesc renders the lane's egress binding for logs and status.
func EgressDesc(lc *config.LaneConfig) string {
	switch {
	case lc.Interface != "":
		return "device:" + lc.Interface
	case lc.BindIP != "":
		return "ip:" + lc.BindIP
	default:
		return "default-route"
	}
}

func bindModeStr(m int64) string {
	switch m {
	case lane.BindModeDevice:
		return "device"
	case lane.BindModeSrcIP:
		return "source-ip"
	default:
		return "none"
	}
}

// Status snapshots the current runtime state.
func (s *Supervisor) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Status{Version: version.Version}
	for _, lc := range s.cfg.Lanes {
		ls := LaneStatus{Name: lc.Name, Type: lc.TypeOrDefault(), Egress: EgressDesc(lc)}
		if inst, ok := s.lanes[lc.Name]; ok {
			st := inst.lane.Stats()
			ls.Healthy = st.Healthy.Load()
			ls.BindMode = bindModeStr(st.BindMode.Load())
			ls.LastErr = st.GetLastErr()
			ls.ConnsTotal = st.ConnsTotal.Load()
			ls.ConnsActive = st.ConnsActive.Load()
			ls.Rejects = st.RejectsTotal.Load()
			ls.BytesUp = st.BytesUp.Load()
			ls.BytesDown = st.BytesDown.Load()
			ls.DNSQueries = st.DNSQueries.Load()
			ls.UDPRelays = st.UDPRelays.Load()
		} else {
			ls.LastErr = "not running"
		}
		out.Lanes = append(out.Lanes, ls)
	}
	return out
}
