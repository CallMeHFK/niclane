# Changelog

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
