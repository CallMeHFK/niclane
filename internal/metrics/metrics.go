// Package metrics holds per-lane runtime counters and renders them in the
// Prometheus text exposition format.
package metrics

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
)

// Stats holds the runtime counters of a single lane. All fields are safe for
// concurrent use.
type Stats struct {
	ConnsTotal   atomic.Int64
	ConnsActive  atomic.Int64
	RejectsTotal atomic.Int64
	BytesUp      atomic.Int64 // client -> egress
	BytesDown    atomic.Int64 // egress -> client
	DNSQueries   atomic.Int64
	UDPRelays    atomic.Int64

	Healthy atomic.Bool
	// BindMode: 0 = none (default egress), 1 = source IP, 2 = device.
	BindMode atomic.Int64

	// LastErr holds the most recent health-check error message (may be empty).
	LastErr atomic.Value // string
}

// SetLastErr stores the latest health-check error message.
func (s *Stats) SetLastErr(msg string) { s.LastErr.Store(msg) }

// GetLastErr returns the latest health-check error message ("" if none).
func (s *Stats) GetLastErr() string {
	v, _ := s.LastErr.Load().(string)
	return v
}

// Registry aggregates per-lane stats and keeps insertion order for stable
// rendering.
type Registry struct {
	mu    sync.RWMutex
	lanes map[string]*Stats
	order []string
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{lanes: make(map[string]*Stats)}
}

// Register returns the stats bucket for the named lane, creating it if needed.
func (r *Registry) Register(name string) *Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.lanes[name]; ok {
		return s
	}
	s := &Stats{}
	r.lanes[name] = s
	r.order = append(r.order, name)
	return s
}

// Names returns the registered lane names in insertion order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// Get returns the stats bucket for a lane, or nil.
func (r *Registry) Get(name string) *Stats {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.lanes[name]
}

const renderHeader = `# HELP niclane_build_info Build information.
# TYPE niclane_build_info gauge
`

// RenderPrometheus renders all lane counters in the Prometheus text
// exposition format.
func (r *Registry) RenderPrometheus(version string) string {
	var b strings.Builder
	b.WriteString(renderHeader)
	fmt.Fprintf(&b, "niclane_build_info{version=%q} 1\n", version)

	for _, name := range r.Names() {
		s := r.Get(name)
		if s == nil {
			continue
		}
		l := fmt.Sprintf("{lane=%q}", name)
		g := func(name, help string, v int64) {
			fmt.Fprintf(&b, "# HELP niclane_%s %s\n# TYPE niclane_%s gauge\nniclane_%s%s %d\n", name, help, name, name, l, v)
		}
		g("connections_total", "Total accepted connections.", s.ConnsTotal.Load())
		g("connections_active", "Currently active connections.", s.ConnsActive.Load())
		g("rejects_total", "Connections rejected (fail-closed or protocol errors).", s.RejectsTotal.Load())
		g("bytes_up", "Bytes relayed client -> egress.", s.BytesUp.Load())
		g("bytes_down", "Bytes relayed egress -> client.", s.BytesDown.Load())
		g("dns_queries_total", "DNS queries resolved through the lane.", s.DNSQueries.Load())
		g("udp_relays_total", "UDP datagrams relayed through the lane.", s.UDPRelays.Load())
		healthy := int64(0)
		if s.Healthy.Load() {
			healthy = 1
		}
		g("healthy", "Whether the lane egress is currently considered usable.", healthy)
		g("bind_mode", "Egress binding mode: 0=none, 1=source-ip, 2=device.", s.BindMode.Load())
	}
	return b.String()
}
