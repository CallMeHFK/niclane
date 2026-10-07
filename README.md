# niclane

**One NIC, one lane.** Per-interface egress isolation for local SOCKS5/HTTP proxies.

[![CI](https://github.com/CallMeHFK/niclane/actions/workflows/ci.yml/badge.svg)](https://github.com/CallMeHFK/niclane/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/go-1.25%2B-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![License: MIT](https://img.shields.io/badge/license-MIT-green.svg)](LICENSE)

Modern laptops rarely have "one network": WiFi on the office LAN, a USB dock on
a second uplink, a VPN/tunnel TUN device, Tailscale, Docker bridges — all
coexisting, all sharing one routing table, and all too often hijacked by a
global TUN-mode proxy that makes *everything* leave through *everywhere*.

**niclane** turns each network interface into an independent *lane*: a local
SOCKS5/HTTP proxy listener whose egress is hard-pinned to one NIC. Point an
app at `127.0.0.1:7891` and its traffic leaves through the WiFi card; point
another app at `127.0.0.1:7892` and its traffic leaves through the dock — the
lanes do not see each other, do not share routes, and never leak into one
another's interface.

It is the laptop-side sibling of
[Nanako0129/SocksBypass](https://github.com/Nanako0129/SocksBypass), the
phone-side SOCKS5 server that pins every outbound socket to the cellular
interface. niclane borrows its most important engineering property —
**fail-closed**: when a lane's interface goes down, new connections are
rejected immediately, never silently rerouted through another NIC.

---

## How it works

```
app A ──> 127.0.0.1:7891 (socks5, lane "wifi")  ──[SO_BINDTODEVICE wlan0]──>  WiFi NIC  ──> uplink A
app B ──> 127.0.0.1:7892 (http,  lane "dock")   ──[SO_BINDTODEVICE enx...]───>  USB NIC   ──> uplink B
app C ──> 127.0.0.1:7893 (socks5, lane "tun")   ──[SO_BINDTODEVICE Mihomo]───>  TUN       ──> proxy exit
```

Each lane:

1. **Pins its egress to one interface.** Linux uses `SO_BINDTODEVICE`;
   macOS uses `IP_BOUND_IF`/`IPV6_BOUND_IF`; other platforms fall back to
   binding the interface's source IP (weaker — see [limitations](#guarantees-and-honest-limitations)).
2. **Fails closed.** A monitor re-checks the interface every 2 s (up? has an
   address?). While unhealthy and `strict: true` (the default), dials are
   rejected with `SOCKS5 0x01` / HTTP 502. No fallback, no leak.
3. **Resolves DNS through its own lane.** Domain lookups are dialed through
   the pinned interface, so DNS cannot leak out of another NIC. Loopback
   nameservers (systemd-resolved) are substituted with `dns_servers` /
   a public resolver, since loopback is unreachable from a device-bound socket.
   Alternatively `dns: doh` resolves via DNS-over-HTTPS (default endpoints:
   AliDNS, Cloudflare) with the HTTPS query itself going through the lane.
4. **Optionally chains an upstream proxy** (`socks5://` or `http://`) — the
   upstream itself is dialed *through the lane*. This is how you chain to a
   SocksBypass phone over the hotspot NIC.
5. **Counts everything**: connections, rejects, per-direction bytes, DNS
   queries, UDP datagrams — exposed on the admin endpoint in Prometheus
   format and via `niclane status`.

UDP ASSOCIATE (RFC 1928) is supported: client datagrams are relayed through a
lane-bound UDP socket.

## Feature summary

- SOCKS5 (CONNECT + UDP ASSOCIATE, optional user/pass auth) and HTTP proxy
  (CONNECT + absolute-URI forwarding) per lane
- Device-level egress pinning: Linux `SO_BINDTODEVICE`, macOS `IP_BOUND_IF`
- Fail-closed health monitoring, per-lane, 2 s granularity
- Per-lane DNS with anti-leak defaults: lane resolver, DNS-over-HTTPS
  (`dns: doh`), or system; configurable `dns_servers` / `doh`
- Upstream chaining (socks5/http), dialed through the lane
- `niclane doctor` — interfaces, capability probe, suggested config
- `niclane test` — verify each lane's real exit IP
- `niclane status` + `/metrics` Prometheus endpoint
- `niclane bench` — per-lane throughput measurement and comparison
- SIGHUP hot-reload: add/remove/change lanes without restarting
- Single static binary, no runtime dependencies

## Quick start

```bash
# install (or grab a binary from Releases, or: make build)
go install github.com/CallMeHFK/niclane/cmd/niclane@latest

# see your interfaces and whether device binding is available
niclane doctor

# example config — one lane per NIC (see config.example.yaml)
cat > niclane.yaml <<'EOF'
admin: 127.0.0.1:9000
lanes:
  - name: wifi
    listen: 127.0.0.1:7891
    type: socks5
    interface: wlan0        # egress pinned to this NIC
  - name: dock
    listen: 127.0.0.1:7892
    type: http
    interface: enxeaa1c366f80d
  - name: tunnel
    listen: 127.0.0.1:7893
    type: socks5
    interface: Mihomo          # the TUN device is just another "NIC"
EOF

niclane serve -c niclane.yaml

# verify each lane's real exit IP
niclane test -c niclane.yaml

# hot-reload after editing the config (add/remove/change lanes live)
kill -HUP $(pidof niclane)
```

Use it like any SOCKS5 proxy: `curl --socks5-hostname 127.0.0.1:7891 …`,
`ssh -o ProxyCommand='nc -x 127.0.0.1:7891 %h %p' …`, or set the app's proxy
setting to the lane of your choice.

### Privileges

- **Linux**: `SO_BINDTODEVICE` needs `CAP_NET_ADMIN` on older kernels; recent
  kernels (≥ 5.7) allow unprivileged binding. `niclane doctor` probes this
  empirically — trust its report. If it reports unavailable:
  `sudo setcap cap_net_admin+ep $(command -v niclane)` or run under systemd
  with `AmbientCapabilities=CAP_NET_ADMIN`.
- **macOS**: `IP_BOUND_IF` is unprivileged.
- **Other platforms**: source-IP binding only.

## Benchmarks

`niclane bench` measures how much traffic each lane can push through its
pinned NIC. Start the echo sink on a peer host reachable through the lanes:

```bash
niclane bench serve --listen :9999        # on the peer host
niclane bench -c niclane.yaml -target <peer>:9999 -duration 10s
```

Example from the developer's laptop — the sink sits on the dock NIC's own
address. The dock lane pushes ~3 GB/s through its NIC; the WiFi lane fails
*because it has no route into the dock subnet at all* — that failure is the
per-NIC isolation working as designed:

```
LANE             TARGET                      UP MB/s   DOWN MB/s  RESULT
dock             198.51.100.23:18999         3059.7      3057.1  ok (4.0s)
wifi             198.51.100.23:18999              -           -  FAIL: dial through lane: i/o timeout

fastest lane by downstream: dock (3057.1 MB/s)
```

## Configuration reference

| Field | Default | Meaning |
|---|---|---|
| `name` | required | unique lane name (metrics label) |
| `listen` | required | local `host:port` for the proxy listener |
| `type` | `socks5` | `socks5` or `http` |
| `interface` | — | pin egress to this NIC (mutually exclusive with `bind_ip`) |
| `bind_ip` | — | pin egress by source address (weaker) |
| `bind_mode` | `auto` | `device`, `ip`, or `auto` (device if possible, else ip) |
| `strict` | `true` | fail-closed: reject while the interface is down/unaddressed |
| `auth` | — | SOCKS5 `user`/`password` for inbound clients |
| `upstream` | — | chain through `socks5://[user:pass@]host:port` or `http://…` (dialed via the lane) |
| `dns` | `lane` | `lane` (resolve through the NIC), `doh` (DNS-over-HTTPS via the lane), or `system` |
| `dns_servers` | `223.5.5.5` | substitutes for loopback NS in lane-DNS mode |
| `doh` | AliDNS + Cloudflare | DoH JSON-API base URLs for `dns: doh`; IP-literal `https://` URLs recommended (no bootstrap needed) |
| `idle_timeout` | none | close tunnels idle longer than this (`5m`, `300s`, …) |
| `admin` | — | global: `host:port` for `/metrics` and `/status` |
| `log_level` | `info` | `debug`, `info`, `warn`, `error` |

## Guarantees and honest limitations

Verified by the test suite and/or on real hardware:

- ✅ Device-bound egress: TCP and UDP traffic leaves *only* through the pinned
  interface, even when other default routes exist (Linux ≥ 5.7 unprivileged,
  macOS unprivileged).
- ✅ Fail-closed: missing/down/unaddressed interface ⇒ immediate reject, never
  a silent fallback.
- ✅ Per-lane DNS: queries egress through the lane; loopback NS substituted;
  `dns: doh` verified live against AliDNS.
- ✅ UDP target resolution through the lane's DNS mode (SOCKS5 UDP
  ASSOCIATE with domain targets).
- ✅ Upstream chaining dialed through the lane.
- ✅ Loopback end-to-end: SOCKS5 CONNECT/UDP, HTTP CONNECT/forward, auth,
  reject paths, byte counters (CI: ubuntu/macos/windows, `-race`).

Honest limitations:

- ⚠️ **Source-IP mode (`bind_ip`, or `bind_mode: ip`) is best-effort.** The
  kernel may still route packets out a different interface (asymmetric
  routing). Only device mode gives a hard isolation guarantee. doctor tells
  you which mode you got, and the lane exposes `bind_mode` in `/status`.
- ⚠️ **Windows pinning uses `IP_UNICAST_IF`** (unprivileged): it constrains
  the *transmit* path only, so treat it as best-effort.
- ⚠️ **Source-IP lanes bind one UDP socket per association**, preferring the
  interface's IPv4 address; IPv6 UDP targets are unreachable on such lanes
  when both families exist (device-mode lanes do not have this limitation).
- ⚠️ **UDP target resolution uses the lane's DNS mode; UDP target *IP
  literals* need no resolution.** Prior to v0.3.0 domains were resolved with
  the system resolver.
- ⚠️ **`dns: system`** lets DNS follow the host default route — a potential
  DNS leak relative to the lane. Default is `dns: lane` for bound lanes.
- ⚠️ **Byte counters** cover SOCKS5 relays and HTTP CONNECT tunnels; plain
  HTTP forward traffic is connection-counted only.
- ⚠️ Exit IPs of different lanes may coincide when the physical uplinks NAT
  through the same ISP line — per-NIC binding is about *which interface the
  packets leave from*, not about the far-end topology.

## Recipes

### Split WiFi vs dock

```yaml
lanes:
  - { name: wifi, listen: 127.0.0.1:7891, interface: wlan0 }
  - { name: dock, listen: 127.0.0.1:7892, interface: enxeaa1c366f80d }
```

`curl --socks5-hostname 127.0.0.1:7891 https://example.com` leaves via WiFi;
the same command against `7892` leaves via the dock. `niclane test -c niclane.yaml`
prints each lane's observed exit IP.

### Chain to a SocksBypass phone over the hotspot NIC

Connect the laptop to the phone's hotspot (`wlan1` say), run
[SocksBypass](https://github.com/Nanako0129/SocksBypass) on the phone
(:9876), then:

```yaml
lanes:
  - name: cell
    listen: 127.0.0.1:7894
    interface: wlan1                     # only the hotspot NIC is used
    upstream: socks5://198.51.100.1:9876 # dialed through wlan1
```

Traffic egresses the laptop's hotspot NIC into the phone, then the phone's
cellular — with SocksBypass binding the phone side to cellular as well. The
LAN on `wlan0` never sees any of it.

### Coexist with a TUN-mode proxy (mihomo / sing-box)

A global TUN device hijacks the default route. niclane lanes with
`interface: <tun>` deliberately ride *into* the tunnel; lanes bound to
physical NICs keep working because `SO_BINDTODEVICE` performs its route
lookup constrained to that device — no fwmark or policy-route changes needed.
Details and caveats (DNS interplay, UID rules) in
[docs/coexistence.md](docs/coexistence.md).

### systemd unit

```ini
[Unit]
Description=niclane per-NIC proxy lanes
After=network-online.target

[Service]
ExecStart=/usr/local/bin/niclane serve -c /etc/niclane/niclane.yaml
Restart=on-failure
AmbientCapabilities=CAP_NET_ADMIN
NoNewPrivileges=yes

[Install]
WantedBy=multi-user.target
```

## How niclane compares

| | niclane | SocksBypass | proxychains-ng | `curl --interface` |
|---|---|---|---|---|
| Runs on | laptop / server (Linux, macOS, Windows best-effort) | phone (iOS/Android) | laptop | per-command |
| Mechanism | local proxy with device-pinned egress | proxy server pinning *its* egress to cellular | LD_PRELOAD socket hooks | per-process source bind |
| Per-NIC isolation | hard (device mode) | hard | no | soft (source bind) |
| Fail-closed | yes, default | yes | no | no |
| Per-app selection | by proxy port | n/a (phone is the proxy) | config file | per invocation |
| Complementary to SocksBypass | **yes — chain it** | — | — | — |

## Roadmap

- [ ] TTL-aware DNS answer caching
- [ ] SOCKS5 BIND command (rarely used)

## Development

```bash
make build    # ./niclane
make test     # go test -race ./...
make vet      # go vet ./...
```

Pull requests welcome — especially platform binders (BSD, Windows) and
benchmarks. Please keep the fail-closed semantics intact in any change that
touches dialing.

## License

MIT — see [LICENSE](LICENSE).
