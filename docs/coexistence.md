# Coexistence with TUN-mode proxies and policy routing

This document describes how niclane lanes behave on hosts that run a global
TUN-mode proxy (mihomo, sing-box, Clash variants) or custom `ip rule` policy
routing — and where the sharp edges are.

## The setup

A typical TUN-mode proxy creates a device (say `Mihomo`) with a /30 address,
a routing rule set that steers nearly all locally generated traffic into it,
and optionally fwmark rules to exclude its own traffic:

```
9000: from all fwmark 0x80000/0xff0000 lookup main
9002: not from all iif lo lookup 2022        # table 2022 -> TUN device
...
32766: from all lookup main                  # real default routes live here
```

Everything a normal process sends is pulled into the tunnel. Global, silent,
and hard to scope per application.

## What niclane does differently

niclane lanes do **not** touch the routing table at all. Each lane's sockets
are created with `SO_BINDTODEVICE(<iface>)` (or `IP_BOUND_IF` on macOS). The
kernel then performs the route lookup *restricted to that device*:

- `interface: wlan0` → only routes whose output device is `wlan0` are
  considered. The TUN's hijacked default route is invisible to these
  sockets; the physical NIC's default route (table `main`) applies.
- `interface: Mihomo` → the lane deliberately rides *into* the tunnel. This
  is a legitimate, useful lane: "send this app through the proxy exit".
- Traffic that the TUN proxy itself originates (fwmark-excluded) is untouched.

No fwmark, no `ip rule` edits, no UID rules, no route metric juggling. The
isolation lives in the socket, which is exactly why two lanes can coexist
without perceiving each other.

## Sharp edges

### 1. DNS

A device-bound socket cannot reach `127.0.0.53` (systemd-resolved): the route
lookup on the physical device has no route to loopback. niclane therefore
substitutes loopback nameservers with `dns_servers` (default `223.5.5.5`) in
`dns: lane` mode. If your environment requires a specific resolver, set it
explicitly:

```yaml
lanes:
  - name: wifi
    interface: wlan0
    dns_servers: [10.0.0.1, 223.5.5.5]   # first reachable wins, dialed via the lane
```

`dns: system` uses the host resolver — convenient, but DNS then follows the
host default route (i.e. into the TUN). That is a documented leak relative to
the lane; use it knowingly.

### 2. TUN proxies with aggressive interception

Some setups redirect *all* traffic including device-bound sockets (eBPF/TPROXY
based interception, or `rp_filter`-coupled setups). If a physical-NIC lane's
traffic still disappears into the tunnel, check the proxy's TUN route-exclude
table (`tun.route-exclude-address`) or its bypass rules for the lane's
upstream destinations. niclane itself needs no exclusion — the destinations it
dials are the internet at large — but the *TUN* may still want to claim them.

### 3. LAN reachability through the "wrong" NIC

A lane bound to NIC A cannot send to subnets that are only reachable via NIC
B — by design. If you need intranet hosts on the other NIC, either run a
second lane for them, or keep those destinations out of the proxied app.

### 4. IPv6

Device-bound sockets use whichever address family the target resolves to;
the kernel picks the source address *on the bound device*. A device with only
IPv4 addresses will fail to dial IPv6 targets in source-IP mode (family
mismatch) and may fail in device mode if the device has no IPv6 route.

## Quick verification

```bash
niclane doctor            # capability probe + interface inventory
niclane serve -c ...      # run your lanes
niclane test -c ...       # observed exit IP per lane
curl --interface wlan0 -s https://api.ipify.org   # cross-check a lane's NIC
```

If a lane's exit IP matches the manual `curl --interface` on the same device,
the binding is doing its job.
