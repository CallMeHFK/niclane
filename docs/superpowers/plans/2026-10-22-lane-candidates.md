# Lane Candidate Sets Implementation Plan

> **Status:** implemented on 2026-10-22 (commits `1ffdd15..38c8a23`; spec
> `0bebefc`). Kept as the design/implementation record — do not re-execute
> this plan.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** One lane listener fronts an ordered set of `(interface, upstream)` candidates, selected per connection by health, with Linux auto-discovery of gateway proxies.

**Architecture:** Each candidate is a full `lane.Lane`; a `selector.Selector` implements `lane.Provider` and hands one pinned child per client connection. Servers acquire/failover through `lane.Dial`. Discovery (separate package) enumerates physical interfaces, resolves each interface's own gateway, TCP-prefilters configured ports.

**Tech Stack:** Go 1.25, stdlib only (crypto/tls, net/http, bufio), gopkg.in/yaml.v3.

**Spec:** `docs/superpowers/specs/2026-10-22-lane-candidates-design.md`

## Global Constraints

- Config backwards compatibility: a lane with only `interface`/`upstream` behaves exactly as today.
- Fail-closed: strict lanes reject when no candidate is usable; never silently fall back to another NIC.
- Application-level upstream rejections (HTTP CONNECT non-2xx, SOCKS reply≠0) never trigger failover or health changes.
- Discovery is Linux-only; other platforms report unsupported, never silently no-op.
- No new third-party dependencies.
- Every task ends with `go test ./...` green.

---

### Task 1: Config schema and validation

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `Config.Discovery *DiscoveryConfig`, `LaneConfig.Candidates []*CandidateConfig`, `LaneConfig.Discover *DiscoverConfig`, `LaneConfig.HasCandidates() bool`, `DiscoverConfig.EffectiveProbeURL() string`, `DiscoverConfig.EffectiveInterval() (time.Duration, error)`, `DiscoverConfig.EffectivePorts(global *DiscoveryConfig) []int`, `DiscoverConfig.EffectiveInterfaces() []string`, constants `DefaultProbeURL`, `DefaultProbeInterval`, `DefaultDiscoveryPorts`.

- [ ] Add types:

```go
type DiscoveryConfig struct {
	Ports []int `yaml:"ports"`
}

type CandidateConfig struct {
	Interface string `yaml:"interface"`
	Upstream  string `yaml:"upstream"`
	Priority  *int   `yaml:"priority"`
}

type DiscoverConfig struct {
	Interfaces []string          `yaml:"interfaces"`
	Ports      []int             `yaml:"ports"`
	Via        map[string]string `yaml:"via"`
	ProbeURL   string            `yaml:"probe_url"`
	Interval   string            `yaml:"interval"`
}
```

- [ ] Validation rules: legacy `interface`/`upstream` must not coexist with `Candidates`/`Discover`; candidate needs interface or upstream, upstream scheme http/socks5, no duplicate `(interface, upstream)`; ports 1..65535 and deduped; `via` values parse as IP; interval ≥ 2s; probe URL http(s), host not loopback/link-local/unspecified/multicast.
- [ ] Add helpers with defaults: probe URL `https://www.gstatic.com/generate_204`, interval `10s`, ports `[10808,10809,7890,7891]`, interfaces `["device"]`; lane-level ports replace global.
- [ ] Tests in `config_test.go`: legacy+new conflict error, defaults, port dedupe/range, probe URL loopback rejection, via IP validation.
- [ ] Run `go test ./internal/config/`; commit `feat(config): candidate and discovery schema`.

### Task 2: Provider interface and upstream rejection classification

**Files:**
- Modify: `internal/lane/lane.go`, `internal/lane/client.go`
- Test: `internal/lane/lane_test.go`

**Interfaces:**
- Produces: `lane.Provider` (`Acquire(ctx) (Egress, error)`, `ReportFailure(eg Egress, err error)`, `Reject(string)`, `Stats() *metrics.Stats`, `Name() string`), `(*Lane).Acquire`, `(*Lane).ReportFailure`, `lane.ErrUpstreamReject`, `(*UpstreamRejectError)`, `lane.Dial(ctx, p Provider, network, address string) (net.Conn, error)`.

- [ ] Add `ErrUpstreamReject` and `UpstreamRejectError{Reason string}` (Unwrap → ErrUpstreamReject).
- [ ] In `client.go`, return `&UpstreamRejectError{...}` for HTTP CONNECT non-2xx and SOCKS5 reply≠0; leave IO/auth errors as transport errors.
- [ ] Add `Provider`; `Lane.Acquire` returns itself; `Lane.ReportFailure` is a no-op.
- [ ] Add `lane.Dial`: up to 3 attempts, Acquire → DialContext; return immediately on `ErrUpstreamReject`; otherwise `p.ReportFailure(eg, err)` and retry.
- [ ] Tests: HTTP upstream 403 → `errors.Is(err, ErrUpstreamReject)`; refused upstream → not reject; `lane.Dial` retries with two fake providers (second succeeds).
- [ ] Run `go test ./internal/lane/`; commit `feat(lane): provider pinning and upstream rejection classification`.

### Task 3: Selector core (static candidates)

**Files:**
- Create: `internal/selector/selector.go`
- Test: `internal/selector/selector_test.go`

**Interfaces:**
- Produces: `selector.New(lc *config.LaneConfig, global *config.DiscoveryConfig, log *slog.Logger, stats *metrics.Stats) (*Selector, error)`, `(*Selector).Acquire`, `ReportFailure`, `Reject`, `Stats`, `Name`, `Run`, `Current`, `Candidates() []CandidateStatus`, `Counts() Counts`, `Refresh(ctx)`, `Test(ctx, rawURL string, timeout time.Duration) []TestResult`.
- `CandidateStatus{Interface, Upstream, Source string; Priority int; Healthy bool; DelayMS int64; LastError string}` (JSON tags as in spec).

- [ ] Child lanes: derive per-candidate `LaneConfig` (Interface/Upstream/BindMode/Strict/DNS/DNSServers/Doh), unregistered `metrics.Stats` per child.
- [ ] Ordering: priority desc → probed success with lower delay → config order; dead = 2 consecutive probe failures.
- [ ] `Acquire`: prefer non-suspect selectable; if none, an NIC-healthy suspect; strict + none ⇒ `ErrFailClosed: no healthy candidate`; non-strict falls back to first NIC-healthy child.
- [ ] Probe loop: every interval, concurrent (cap 4), `probeEgress` via child `DialContext` to `probe_url` (TLS for https), expect 2xx, 5s timeout; success resets failures and clears suspect, logs first success.
- [ ] `ReportFailure`: mark suspect 2s and singleflight re-probe; ignore `ErrUpstreamReject`.
- [ ] `Refresh`: run one discovery sync (no-op until Task 7) then probe all; used by `test`/`discover` CLI.
- [ ] Tests with real child lanes pointed at fake local HTTP proxies: priority order, transport failure moves to next, `ErrUpstreamReject` does not, probe debounce (2 fails), recovery (1 success).
- [ ] Run `go test ./internal/selector/`; commit `feat(selector): candidate selection with health probes`.

### Task 4: Servers use Provider

**Files:**
- Modify: `internal/socks5/server.go`, `internal/httpproxy/server.go`
- Test: `internal/socks5/server_test.go`, `internal/httpproxy/server_test.go`

**Interfaces:**
- Consumes: `lane.Provider`, `lane.Dial`.
- Produces: `socks5.Server.Provider lane.Provider`; `httpproxy.NewHandler(p lane.Provider, ...)`.

- [ ] socks5: replace `Egress` field with `Provider`; CONNECT uses `lane.Dial(ctx, Provider, "tcp", target)`; UDP acquires once and uses that child for Resolve/ListenUDP/Stats; pre-acquire rejects go to `Provider.Reject`.
- [ ] httpproxy: single transport whose `DialContext` calls `lane.Dial(ctx, Provider, ...)`; CONNECT uses `lane.Dial`; rejects go to `Provider.Reject`.
- [ ] Update tests (`Server{Provider: l}`, `NewHandler(l, ...)`); add a test that a transport-failing provider causes the next candidate to be used (use selector or a fake provider).
- [ ] Run `go test ./internal/socks5/ ./internal/httpproxy/`; commit `feat(servers): pin egress via provider with failover`.

### Task 5: Supervisor wiring and status

**Files:**
- Modify: `internal/supervisor/supervisor.go`
- Test: `internal/supervisor/supervisor_test.go`

**Interfaces:**
- Produces: `LaneStatus.Candidates []selector.CandidateStatus`; status counters aggregated from children.

- [ ] `instance` holds `rt lane.Provider` plus optional `lane`/`selector`; `startLane` chooses selector when `lc.HasCandidates()`, else lane; run `Run(ctx)` on both.
- [ ] `serveLane` accepts `lane.Provider`.
- [ ] `Status`: selector lanes report aggregated counts, `Healthy` from `Current()`, active bind mode, and candidates; `EgressDesc` shows `candidates:N`.
- [ ] Tests: status includes candidates; reload of a candidate list restarts the lane (existing DeepEqual behavior); plain lane status unchanged.
- [ ] Run `go test ./internal/supervisor/`; commit `feat(supervisor): run selector lanes and expose candidates`.

### Task 6: CLI test/status adaptation

**Files:**
- Modify: `cmd/niclane/main.go`
- Test: manual build; existing unit tests unaffected.

- [ ] `niclane test`: selector lanes construct a Selector, `Refresh`, `Test(url, timeout)`, print one row per candidate with source/priority/healthy/delay/exit-IP; active candidate marked.
- [ ] `niclane status`: after the lane table, print candidate rows.
- [ ] `go build ./...`; commit `feat(cli): candidate-aware test and status`.

### Task 7: Discovery package (Linux)

**Files:**
- Create: `internal/discover/discover.go`, `internal/discover/discover_linux.go`, `internal/discover/discover_other.go`
- Test: `internal/discover/discover_test.go`

**Interfaces:**
- Produces: `discover.Device{Interface string; Gateway net.IP; Port int}`, `discover.Devices(ctx, Options) ([]Device, error)`, `Options{Interfaces []string; Via map[string]string; Ports []int; Timeout time.Duration; Log *slog.Logger}`, `ErrUnsupported`.

- [ ] Interface selection: `/sys/class/net`; skip `lo`; `device` requires `device` symlink; `any` includes all; globs via `filepath.Match`; dedupe + sort.
- [ ] Gateway per interface: `via` → `/proc/net/route` (default dest `00000000`, little-endian hex gateway, filter by iface) → systemd lease (`/run/systemd/netif/leases/<ifindex>`, `ROUTER=`) → `nmcli -t -f IP4.GATEWAY device show <if>` (argv, 2s). Never a global default route.
- [ ] Port prefilter: TCP dial `gw:port` bound to the interface via a throwaway `lane.Lane` (interface only), 800ms, concurrency 4.
- [ ] `discover_other.go` returns `ErrUnsupported` (`//go:build !linux`).
- [ ] Tests: parseProcRoute (incl. real hex `0A6433C6` → 198.51.100.10), parseLease, parseNmcli, interface spec matching; port prefilter against a local listener with no interface binding (skip if binding unavailable).
- [ ] Run `go test ./internal/discover/`; commit `feat(discover): enumerate gateway proxies per interface`.

### Task 8: Selector discovery wiring and `niclane discover`

**Files:**
- Modify: `internal/selector/selector.go`, `cmd/niclane/main.go`
- Test: `internal/selector/selector_test.go`

- [ ] `Selector.Run`/`Refresh` calls `discover.Devices` each interval; add new discovered candidates (priority 100, source discovered), remove discovered candidates whose device disappeared; static candidates never removed; discovered duplicate of a static key is ignored.
- [ ] On non-Linux (`ErrUnsupported`), log once at startup and disable discovery; still serve static candidates.
- [ ] `niclane discover -c`: construct selectors for lanes with `Discover`, `Refresh`, print interface/gateway/upstream/port/healthy/delay.
- [ ] Tests: fake discovery source injection (extract `discoverFn` field) adds/removes candidates across cycles; static survives.
- [ ] Run `go test ./...`; commit `feat(selector): wire auto-discovery`.

### Task 9: Docs, example, changelog, skill

**Files:**
- Modify: `README.md`, `README.zh-CN.md`, `config.example.yaml`, `CHANGELOG.md`, `skills/niclane/SKILL.md`

- [ ] Document candidates, discovery, ports maintenance (global + lane override), gateway sources, Linux-only matrix, security notes, hot-reload semantics (config change restarts the lane), `niclane discover`.
- [ ] Add a two-phone fallback recipe replacing the old "two lanes + external group" pattern.
- [ ] CHANGELOG entry under Unreleased.
- [ ] Commit `docs: document lane candidates and discovery`.

### Task 10: Full gate and real-host acceptance

- [ ] `gofmt -l .` clean, `go vet ./...`, `go test ./...`, `go test -race ./...`.
- [ ] Real host: config with static moto + discovered samsung; verify `niclane test` rows, `discover` output, failover by dropping one tether, fail-closed when both down, hot reload.
- [ ] Commit any fixes; report results honestly.
