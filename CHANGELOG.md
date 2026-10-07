# Changelog

## 0.3.1 — 2026-10-07

Full-codebase review (three parallel audit passes + staticcheck); fixes:

- **UDP ASSOCIATE leak (critical)**: closing the TCP control connection never
  tore the association down — the client-facing socket, the lane-bound socket
  and two goroutines leaked per association. Both sockets are now closed on
  association end.
- **Half-close semantics**: relays no longer full-close both connections on
  the first EOF; a clean EOF half-closes the peer so CONNECT tunnels keep
  flowing (a client that FINs after its request still gets the response).
- **Shared idle timeout**: the idle deadline is now a watchdog over a shared
  activity timestamp — one-directional transfers (long downloads) are no
  longer killed, and a wedged peer cannot pin a tunnel forever.
- **UDP hardening**: datagrams from sources other than the current relay
  target are dropped (RFC 1928 replies come from the target); CONNECT
  success replies use a zero BND.ADDR instead of leaking the egress NIC IP.
- **Pipelined bytes after CONNECT** are delivered to the backend instead of
  being dropped (prefix conn over the hijacked buffer).
- **DoH bootstrap loop guard**: an endpoint whose family never matches the
  lane source fails explicitly instead of recursing through itself.
- **Capability-aware binding**: without SO_BINDTODEVICE permission (old
  kernels) lanes degrade to source-IP mode instead of reporting healthy while
  every dial would EPERM; `tcp6`/`udp6` now filter IPv4 answers.
- **Reload all-or-nothing**: the admin listener is pre-flighted before any
  lane is touched — a bad admin address no longer loses the endpoint or
  desyncs `/status`; lanes now cancel in-flight connections on reload/stop;
  `/metrics` prunes removed lanes and types counters correctly.
- **CLI hardening**: `niclane test` no longer panics on an unparseable
  `--url`; SIGHUP is registered before startup so an early signal cannot kill
  the process; `bench` reports honest elapsed time and picks the fastest lane
  only among successful ones.
- Unbound lanes with `dns: system` regain dialer-side resolution (Happy
  Eyeballs across address families).

## 0.3.0 — 2026-10-07

- **DNS-over-HTTPS lanes**: `dns: doh` resolves domains via the DNS-JSON API
  (default endpoints: AliDNS `223.5.5.5`, Cloudflare `1.1.1.1`, both
  IP-literal so no bootstrap resolution is needed) with the HTTPS query
  itself dialed through the lane. Custom endpoints via `doh: [...]`.
- **UDP ASSOCIATE domain targets now resolve through the lane's DNS mode**
  (lane / doh / system) instead of always using the system resolver — no more
  DNS leak through the UDP relay path.
- **Release binaries**: goreleaser-based GitHub Releases (linux/darwin/windows,
  amd64+arm64, checksums) on `v*` tags.
- Verified live: device-pinned lanes resolving through AliDNS DoH and
  egressing their own NIC.

## 0.2.0 — 2026-10-07

- **`niclane bench` / `niclane bench serve`**: per-lane throughput measurement
  and comparison against an echo sink (`-target`, `-duration`, `-block`).
  Verified on real hardware: a device-pinned lane cannot even route into
  another NIC's subnet — isolation you can measure.
- **SIGHUP hot-reload**: `niclane serve` now re-reads the config on SIGHUP.
  Lanes are added, removed and replaced live; unchanged lanes keep running
  with their metrics continuity; an invalid config is rejected and the
  previous state keeps serving. New `internal/supervisor` owns the runtime.
- **Windows**: unprivileged egress pinning via `IP_UNICAST_IF` /
  `IPV6_UNICAST_IF` (transmit-path only, documented best-effort).
- Health metrics are populated synchronously at lane start, so `/status` and
  `niclane status` no longer race the first health-check tick.

## 0.1.0 — 2026-10-07

- Initial release: SOCKS5 (CONNECT + UDP ASSOCIATE) and HTTP proxy lanes with
  device-pinned egress (`SO_BINDTODEVICE` on Linux, `IP_BOUND_IF` on macOS,
  source-IP fallback elsewhere), fail-closed health monitoring, per-lane DNS
  with loopback-NS substitution, upstream chaining, `doctor` / `test` /
  `status` commands, Prometheus `/metrics`, bilingual README, three-platform
  CI with `-race`.
