package supervisor

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "niclane.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const cfgA = `
admin: 127.0.0.1:18090
lanes:
  - name: a
    listen: 127.0.0.1:18091
    bind_ip: 127.0.0.1
`

// Identical to cfgA modulo whitespace: reloading must not churn lanes.
const cfgASame = `
admin: 127.0.0.1:18090

lanes:
  - name: a
    listen: 127.0.0.1:18091
    bind_ip: 127.0.0.1
`

const cfgRenamed = `
admin: 127.0.0.1:18090
lanes:
  - name: b
    listen: 127.0.0.1:18092
    bind_ip: 127.0.0.1
`

const cfgInvalid = `
lanes:
  - name: x
`

func TestHotReload(t *testing.T) {
	path := writeCfg(t, cfgA)
	sup, err := New(path, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := sup.Start(ctx); err != nil {
		t.Fatal(err)
	}

	st := sup.Status()
	if len(st.Lanes) != 1 || st.Lanes[0].Name != "a" || !st.Lanes[0].Healthy {
		t.Fatalf("initial status wrong: %+v", st)
	}
	if st.Lanes[0].BindMode != "source-ip" {
		t.Fatalf("bind mode = %q, want source-ip", st.Lanes[0].BindMode)
	}

	// Same config (modulo whitespace): the lane instance must be kept (no churn).
	if err := os.WriteFile(path, []byte(cfgASame), 0o600); err != nil {
		t.Fatal(err)
	}
	before := sup.lanes["a"]
	if err := sup.Reload(); err != nil {
		t.Fatalf("reload same config: %v", err)
	}
	if sup.lanes["a"] != before {
		t.Fatal("unchanged lane was recreated")
	}

	// Renamed lane on another port: old stops, new starts.
	if err := os.WriteFile(path, []byte(cfgRenamed), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := sup.Reload(); err != nil {
		t.Fatalf("reload renamed: %v", err)
	}
	st = sup.Status()
	if len(st.Lanes) != 1 || st.Lanes[0].Name != "b" || !st.Lanes[0].Healthy {
		t.Fatalf("status after rename wrong: %+v", st)
	}
	if _, ok := sup.lanes["a"]; ok {
		t.Fatal("old lane still running")
	}

	// Invalid config: reload must fail and keep the previous state running.
	if err := os.WriteFile(path, []byte(cfgInvalid), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := sup.Reload(); err == nil {
		t.Fatal("reload of invalid config must fail")
	}
	st = sup.Status()
	if len(st.Lanes) != 1 || st.Lanes[0].Name != "b" {
		t.Fatalf("state corrupted after failed reload: %+v", st)
	}

	// A reload whose new admin address cannot bind must be rejected wholesale:
	// the old admin endpoint keeps serving and the lanes stay untouched.
	holdLn, err := net.Listen("tcp", "127.0.0.1:18099") // occupies the target port
	if err != nil {
		t.Fatal(err)
	}
	defer holdLn.Close()
	broken := `
admin: 127.0.0.1:18099
lanes:
  - name: c
    listen: 127.0.0.1:18093
    bind_ip: 127.0.0.1
`
	if err := os.WriteFile(path, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := sup.Reload(); err == nil {
		t.Fatal("reload with unbindable admin must fail")
	}
	if got := len(sup.Status().Lanes); got != 1 || sup.Status().Lanes[0].Name != "b" {
		t.Fatalf("lanes changed despite failed admin reload: %+v", sup.Status().Lanes)
	}
	resp, err := http.Get("http://127.0.0.1:18090/status")
	if err != nil {
		t.Fatalf("old admin endpoint lost after failed reload: %v", err)
	}
	resp.Body.Close()

	sup.Stop()
	if len(sup.lanes) != 0 {
		t.Fatal("lanes still running after Stop")
	}
	if sup.adminAddr != "" {
		t.Fatal("adminAddr not reset by Stop")
	}
}
