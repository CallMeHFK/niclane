# Changelog

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
