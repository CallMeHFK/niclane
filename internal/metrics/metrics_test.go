package metrics

import (
	"strings"
	"testing"
)

func TestRenderPrometheus(t *testing.T) {
	r := NewRegistry()
	s := r.Register("wifi")
	s.ConnsTotal.Store(3)
	s.RejectsTotal.Store(1)
	s.BytesUp.Store(100)
	s.Healthy.Store(true)
	s.BindMode.Store(2)
	r.Register("dock")

	r.Sweep(map[string]struct{}{"wifi": {}})
	out := r.RenderPrometheus("test")
	if strings.Contains(out, `lane="dock"`) {
		t.Fatal("swept lane still rendered")
	}
	if !strings.Contains(out, "# TYPE niclane_connections_total counter") {
		t.Fatal("connections_total must be typed counter")
	}
	for _, want := range []string{
		`niclane_build_info{version="test"}`,
		`niclane_connections_total{lane="wifi"} 3`,
		`niclane_rejects_total{lane="wifi"} 1`,
		`niclane_bytes_up{lane="wifi"} 100`,
		`niclane_healthy{lane="wifi"} 1`,
		`niclane_bind_mode{lane="wifi"} 2`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered output missing %q\n%s", want, out)
		}
	}
}
