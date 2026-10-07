---
name: niclane
description: Operate niclane: local SOCKS5/HTTP proxy lanes pinned per network interface (one NIC, one lane, fail-closed). Use whenever an app's traffic must egress via a specific NIC, be split across WiFi/dock/hotspot/TUN, or the user mentions 多网卡, 网卡分流, 指定网卡出口, SocksBypass, egress isolation.
---

# niclane — per-NIC proxy lanes

niclane turns each network interface into an independent egress lane: a local
SOCKS5/HTTP listener whose outbound sockets are pinned to one NIC
(`SO_BINDTODEVICE` on Linux, `IP_BOUND_IF` on macOS). Apps pick a lane by
proxy port; lanes never leak into each other and **fail closed** — if the
lane's NIC goes down, connections are rejected, never silently rerouted.

## Standard workflow (follow in order)

1. **Install if missing** (single Go binary, no runtime deps):
   ```bash
   command -v niclane || go install github.com/CallMeHFK/niclane/cmd/niclane@latest
   ```
2. **Inspect the host first — always**:
   ```bash
   niclane doctor
   ```
   It lists interfaces, probes whether device binding is permitted for this
   user, and prints a suggested config. Trust its capability report: if it
   says device binding is NOT AVAILABLE, lanes must use `bind_ip` (weaker) or
   you need `sudo setcap cap_net_admin+ep $(command -v niclane)`.
3. **Write a minimal config** — one lane per NIC, `127.0.0.1` listen, distinct
   ports. Start from this template and edit interface names from `doctor`:
   ```yaml
   admin: 127.0.0.1:9000
   lanes:
     - name: wifi
       listen: 127.0.0.1:7891
       type: socks5
       interface: wlan0          # NIC name from doctor
       dns_servers: [223.5.5.5]  # replaces loopback NS (systemd-resolved)
     - name: dock
       listen: 127.0.0.1:7892
       type: socks5
       interface: enx1a2b3c4d5e6f
   ```
4. **Run it** in the background and **verify every lane's real egress**:
   ```bash
   niclane serve -c niclane.yaml &
   niclane test -c niclane.yaml -url http://members.3322.org/dyndns/getip
   ```
   Each lane must report its own `exit-ip=`. `niclane test` is the
   acceptance gate — never deliver a config without running it.
5. **Point the app at the lane**: `curl --socks5-hostname 127.0.0.1:7891 …`,
   `ssh -o ProxyCommand='nc -x 127.0.0.1:7891 %h %p' …`, or the app's proxy
   settings. HTTP lanes accept `http_proxy=http://127.0.0.1:7892`. For SOCKS
   libraries prefer the `socks5h://` scheme so DNS is resolved through the
   lane too, not by the host resolver.
6. **Check live state**: `niclane status -c niclane.yaml` (health, bind mode,
   counters) or `curl 127.0.0.1:9000/metrics` (Prometheus).
7. **Config changes are hot-reloaded**: edit the YAML, then
   `kill -HUP $(pidof niclane)`. A broken config is rejected and the previous
   one keeps running — confirm via the log line `config reloaded` or `failed`.

## Choosing the binding

| Situation | Config |
|---|---|
| NIC known, device binding available (doctor says AVAILABLE) | `interface: <name>` — hard isolation |
| No device-binding capability and no privileges | `bind_ip: <NIC address>` — best-effort, may leak on route changes; say so to the user |
| Deliberately ride a TUN-mode proxy (mihomo/sing-box) | `interface: <tun device>` |
| Chain to another proxy (e.g. SocksBypass on a phone over its hotspot NIC) | `interface: <hotspot nic>` + `upstream: socks5://<phone>:9876` |

## Troubleshooting

- **Dial fails with EPERM / device binding unavailable** → `setcap` (above)
  or run under systemd with `AmbientCapabilities=CAP_NET_ADMIN`; or fall back
  to `bind_ip` and disclose the weaker isolation.
- **DNS times out on a device-bound lane** → the host uses a loopback
  resolver (systemd-resolved), unreachable from a pinned socket. Set
  `dns_servers: [223.5.5.5, 1.1.1.1]`, or `dns: doh` for DNS-over-HTTPS
  through the lane.
- **`niclane test` FAILs on foreign targets (ipify.org etc.) but the lane is
  healthy** → direct egress from CN networks gets RST for some foreign
  endpoints. Re-test with `-url http://members.3322.org/dyndns/getip`. This
  is network interference, not a niclane bug.
- **All lanes show the same exit IP** → not a binding failure. Per-NIC
  binding is about which interface packets leave from; uplinks behind the
  same ISP line NAT to the same address. Verify binding with
  `curl --interface <nic> …` cross-checks and `niclane bench`.
- **A lane is DOWN in status and connections are rejected** → fail-closed by
  design (NIC down / no address). Check `last_error` in `/status`; do not
  "fix" it by removing `strict`.
- **`bind_mode: source-ip` in /status when you expected device** → capability
  missing or `bind_ip` used; source-IP mode is best-effort (asymmetric
  routing possible).

## Benchmarks

Compare lane throughput against an echo sink when the user asks which
uplink is faster:

```bash
niclane bench serve --listen :9999        # on a peer host reachable via the lanes
niclane bench -c niclane.yaml -target <peer>:9999 -duration 10s
```

## Hard rules

- Never promise per-NIC isolation for `bind_ip` lanes — device mode only.
- Never route around a fail-closed rejection (e.g. by dropping `strict` or
  adding fallback lanes) without saying so explicitly.
- Do not put two lanes on the same `listen` port; validation rejects the
  whole config on reload.

Full reference: https://github.com/CallMeHFK/niclane (README, docs/coexistence.md).
