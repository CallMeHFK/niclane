---
name: niclane
description: Operate niclane: local SOCKS5/HTTP proxy lanes pinned per network interface (one NIC, one lane, fail-closed), with optional candidate sets and auto-discovery that fail over between tethered/hotspot devices. Use whenever an app's traffic must egress via a specific NIC, be split across WiFi/dock/hotspot/TUN, need automatic fallback when one device drops, or the user mentions 多网卡, 网卡分流, 指定网卡出口, 多设备回退, 自动发现, SocksBypass, egress isolation.
---

# niclane — per-NIC proxy lanes

niclane turns each network interface into an independent egress lane: a local
SOCKS5/HTTP listener whose outbound sockets are pinned to one NIC
(`SO_BINDTODEVICE` on Linux, `IP_BOUND_IF` on macOS, `IP_UNICAST_IF` on
Windows). Apps pick a lane by proxy port; lanes never leak into each other
and **fail closed** — if the lane's NIC goes down, connections are rejected,
never silently rerouted.

A lane can also span several NICs via `candidates:` / `discover:`: each
connection is pinned to one healthy candidate, failover is transport-only
inside that candidate set, and the lane still fails closed when none is
healthy. This is the sanctioned way to do device fallback — do not build
external watchers for it.

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
   user, prints the current default egress, and suggests a config. Trust its
   capability report: if it says device binding is NOT AVAILABLE, lanes must
   use `bind_ip` (weaker) or you need
   `sudo setcap cap_net_admin+ep $(command -v niclane)`.
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
4. **Verify every lane's real egress before serving anything**:
   ```bash
   niclane test -c niclane.yaml                    # default echo: api.ipify.org
   ```
   Each lane must report its own `exit-ip=`. `-lane <name>` restricts the run,
   `-timeout` defaults to 10s. On candidate lanes it prints one row per
   candidate and marks the currently preferred one with a trailing `*`.

   `niclane test` builds its own lanes and dials directly, so it needs **no**
   running `serve` — run it against a config you have not started yet, or in
   CI. It is the acceptance gate: never deliver a config without it.
5. **Serve it** once `test` is green:
   ```bash
   niclane serve -c niclane.yaml &
   ```
6. **Confirm the packets really left that NIC** before claiming isolation —
   see [Verifying isolation](#verifying-isolation-exit-ip-is-not-proof). Two
   uplinks behind one ISP line NAT to the same address, so identical exit IPs
   prove nothing.
7. **Point the app at the lane**: `curl --socks5-hostname 127.0.0.1:7891 …`,
   `ssh -o ProxyCommand='nc -x 127.0.0.1:7891 %h %p' …`, or the app's proxy
   settings. HTTP lanes accept `http_proxy=http://127.0.0.1:7892`. For SOCKS
   libraries prefer the `socks5h://` scheme so DNS is resolved through the
   lane too, not by the host resolver.
8. **Check live state**: `niclane status -c niclane.yaml` (health, bind mode,
   egress, per-lane byte counters, rejects) or `curl 127.0.0.1:9000/metrics`
   (Prometheus). `status` needs the config only to find `admin:`.
9. **Config changes are hot-reloaded**: edit the YAML, then
   `kill -HUP $(pidof niclane)`. The log says `config reloaded` or
   `config reload failed`; a rejected config leaves the previous one fully
   running. Adding/removing/editing a lane — or its `candidates`/`discover` —
   restarts just that lane (its live connections are cut). Devices coming and
   going during discovery restart nothing.

## Verifying isolation (exit IP is not proof)

The lane's `BYTES` column in `status` proves the proxy carried the traffic;
`/sys/class/net/<nic>/statistics/rx_bytes` proves *which* interface received
it. Take both the counters before and after a known download:

```bash
nic=wlan0
before=$(cat /sys/class/net/$nic/statistics/rx_bytes)
curl --socks5-hostname 127.0.0.1:7891 -o /dev/null https://speed.cloudflare.com/__down?bytes=20000000
after=$(cat /sys/class/net/$nic/statistics/rx_bytes)
echo "$(( (after - before) / 1024 )) KiB via $nic"
```

That interface must move roughly the payload size and the other lanes must
not. Repeat per lane when the user is paying for metered data — this is the
only check that distinguishes "the lane works" from "traffic leaked out the
default route while the lane reported success".

## Choosing the binding

| Situation | Config |
|---|---|
| NIC known, device binding available (doctor says AVAILABLE) | `interface: <name>` — hard isolation |
| No device-binding capability and no privileges | `bind_ip: <NIC address>` — best-effort, may leak on route changes; say so to the user |
| Want source-IP mode *anyway* (debugging a suspected binder problem) | `interface: <name>` + `bind_mode: ip` |
| Deliberately ride a TUN-mode proxy (mihomo/sing-box) | `interface: <tun device>` |
| Chain to another proxy (e.g. SocksBypass on a phone over its hotspot NIC) | `interface: <hotspot nic>` + `upstream: socks5://<phone>:9876` |
| macOS | `IP_BOUND_IF`, unprivileged — same isolation as Linux device mode |
| Windows | `IP_UNICAST_IF`, unprivileged but **transmit path only** — treat as best-effort and disclose it |

`interface` and `bind_ip` are mutually exclusive; `bind_mode: device` without
an `interface` is rejected.

## Failover lanes (candidates + discovery)

When the user wants "use whatever device has internet, fall back to another",
do not hand-build watch scripts: use a candidate lane.

```yaml
lanes:
  - name: phone
    listen: 127.0.0.1:7893
    candidates:
      - { interface: enx020000000001, upstream: "http://198.51.100.10:10808" }
      - { interface: enx020000000003, upstream: "http://198.51.100.20:10808", priority: 500 }
    discover:
      interfaces: ["device"]      # or globs / "any"; lo always excluded
      ports: [10808, 10809]       # per-lane override of discovery.ports
```

Rules of the model:

- One candidate is pinned per client connection (resolve + dial + UDP).
  Preference: priority (default 1000) → probe delay → config order.
- Health is a real fetch of `probe_url` through each candidate's own NIC;
  2 consecutive failures = dead, 1 success = recovered (a single success also
  clears the stale error and delay the candidate was showing).
- Only transport failures fail over. An upstream SOCKS5 reply ≠ 0 or HTTP
  CONNECT non-2xx is the app's answer — do not retry around it.
- `strict: true` still means fail-closed when no candidate is healthy.
- `interface`/`upstream` cannot be combined with `candidates`/`discover` —
  validation rejects the mix.
- Discovery is Linux-only, per-interface gateway, configured ports only — it
  never scans subnets. Extend `discovery.ports` / `discover.ports` when the
  device's proxy listens elsewhere; check with `niclane discover -c …`.
- Gateway lookup order per interface: `discover.via` override →
  `/proc/net/route` → systemd-networkd lease → NM `IP4.GATEWAY` → NM DHCP
  option 3. The last one exists for links that publish no default route.
- Static candidates outrank discovered ones (1000 vs 100). Adding/changing
  candidates or discovery restarts that lane on SIGHUP; devices coming and
  going do not.
- Discovery assumes the phone's USB/LAN gateway runs the proxy. If the user's
  topology differs (proxy on a host inside the LAN), use an explicit static
  candidate, or `discover.via` to point the interface at the right gateway.

### The re-plug trap: why fixed interface names rot

A USB-tethered phone re-enumerates with a fresh random MAC, so its `enx…`
name changes on every reconnect. A lane with `interface: enx<old>` is then
permanently unhealthy — the name no longer exists — and re-plugging does not
bring it back, because the config still names the dead device.

Discovery does not have this problem: it re-enumerates `/sys/class/net` each
round, so it follows the rename on its own. For any device that can be
unplugged, prefer `discover:` over a hardcoded interface, or keep a static
candidate only for a NIC whose name is stable (onboard Ethernet, a TUN).

When discovery is on, a stale static entry is dead weight rather than a
failure: the lane survives on the discovered candidate at priority 100. Check
`niclane discover` — if the static row is `healthy:false` while a discovered
row for the *same device* is healthy, delete the static entry.

## Troubleshooting

- **Dial fails with EPERM / device binding unavailable** → `setcap` (above)
  or run under systemd with `AmbientCapabilities=CAP_NET_ADMIN`; or fall back
  to `bind_ip` and disclose the weaker isolation.
- **DNS times out on a device-bound lane** → the host uses a loopback
  resolver (systemd-resolved), unreachable from a pinned socket. Set
  `dns_servers: [223.5.5.5, 1.1.1.1]`, or `dns: doh` for DNS-over-HTTPS
  through the lane. The default mode for a bound lane is `dns: lane`; only
  `dns: system` follows the host resolver — that is a leak, never use it when
  the point is isolation.
- **`niclane test` FAILs on foreign targets (ipify.org etc.) but the lane is
  healthy** → direct egress from CN networks gets RST for some foreign
  endpoints. Re-test with `-url http://members.3322.org/dyndns/getip`. This
  is network interference, not a niclane bug.
- **All lanes show the same exit IP** → not a binding failure. Per-NIC
  binding is about which interface packets leave from; uplinks behind the
  same ISP line NAT to the same address. Verify per
  [Verifying isolation](#verifying-isolation-exit-ip-is-not-proof).
- **A lane is DOWN in status and connections are rejected** → fail-closed by
  design (NIC down / no address). Check `last_error` in `/status`; do not
  "fix" it by removing `strict`. SOCKS5 clients see reply `0x01`, HTTP
  clients see 502.
- **`bind_mode: source-ip` in /status when you expected device** → capability
  missing, `bind_ip`/`bind_mode: ip` in use, or the platform is Windows.
  Source-IP mode is best-effort (asymmetric routing possible).
- **Candidate lane rejects everything** → inspect `candidates` in
  `niclane status` / `/status`: all `healthy:false` means every probe failed.
  Check `last_error`, the upstream process on the device, and that each
  candidate's interface exists (`ip link`). Do not "fix" it by setting
  `strict: false` — on a candidate lane that silently hands connections to a
  candidate whose probes are failing, which is the leak `strict` exists to
  prevent.
- **A candidate is unhealthy on a link with no default route** → typical for
  a phone link set to `ipv4.never-default=yes`: NetworkManager reports an
  empty `IP4.GATEWAY`, so discovery must fall back to the DHCP lease. On
  v0.4.1+ that fallback exists; on older builds the lane fails closed while
  the proxy is perfectly reachable. Upgrade rather than adding `via` for a
  link whose gateway DHCP already knows.
- **`niclane discover` finds nothing** → the proxy port is not in
  `discovery.ports`/`discover.ports`, the device proxy is not running, or the
  gateway does not expose it (host firewall / proxy bound to another IP).
  Add the port first; use `discover.via` only when the gateway is genuinely
  not readable from DHCP.
- **`discover` reports "all are already static candidates"** → not a failure.
  The scan found the endpoint but the lane already names it statically, so
  nothing new is added. Look at that static row's health instead.
- **`discover` picks up a NIC you did not mean to include** →
  `interfaces: ["device"]` matches every NIC backed by a physical device, so
  WiFi and spare wired ports show up too. Narrow it with a glob
  (`["enx*"]`) or list the names.
- **UDP (QUIC, DNS-over-UDP, game traffic) not working** → SOCKS5 UDP
  ASSOCIATE is supported and relayed through a lane-bound UDP socket, but the
  client must actually use it: `curl --socks5-hostname` is TCP-only, and HTTP
  proxy lanes have no UDP path at all. Use a SOCKS5 lane and a client that
  speaks UDP ASSOCIATE.
- **Connections hang open after the client is gone** → no `idle_timeout` is
  set (0 = none). Set `idle_timeout: 5m` on that lane.

## Benchmarks

Compare lane throughput against an echo sink when the user asks which
uplink is faster:

```bash
niclane bench serve --listen :9999        # on a peer host reachable via the lanes
niclane bench -c niclane.yaml -target <peer>:9999 -duration 10s
```

`-lane a,b` restricts the run, `-block` sets the write size.

## Hard rules

- Never promise per-NIC isolation for `bind_ip` lanes, or for Windows — device
  mode on Linux/macOS only.
- Never route around a fail-closed rejection (e.g. by dropping `strict` or
  adding fallback lanes) without saying so explicitly. Candidate failover is
  the sanctioned exception: it stays inside the lane and still fails closed
  when every candidate is dead.
- Do not put two lanes on the same `listen` port; validation rejects the
  whole config on reload.
- Do not report success from `exit-ip=` alone. Say which interface carried the
  bytes and how you checked.

Full reference: https://github.com/CallMeHFK/niclane (README, docs/coexistence.md).