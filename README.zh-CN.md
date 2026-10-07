# niclane

**一张网卡，一条车道。** 为本地 SOCKS5/HTTP 代理提供按网卡隔离的出口绑定。

[![CI](https://github.com/CallMeHFK/niclane/actions/workflows/ci.yml/badge.svg)](https://github.com/CallMeHFK/niclane/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/go-1.25%2B-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![License: MIT](https://img.shields.io/badge/license-MIT-green.svg)](LICENSE)

现在的笔记本很少有"一个网络"：WiFi 连着局域网、USB 扩展坞接着第二条上行、
VPN/TUN 虚拟网卡、Tailscale、Docker 网桥……全部共存于一张路由表，还经常被
全局 TUN 模式代理整体劫持——所有流量从所有出口乱窜。

**niclane** 把每张网卡变成一条独立的**车道（lane）**：一个本地 SOCKS5/HTTP
代理监听端口，出站被硬绑定到一张指定网卡。应用 A 指向 `127.0.0.1:7891`，
流量从 WiFi 出去；应用 B 指向 `127.0.0.1:7892`，流量从扩展坞出去。车道之间
互不感知、不共享路由、绝不向彼此的网卡泄漏。

它是 [Nanako0129/SocksBypass](https://github.com/Nanako0129/SocksBypass) 的
笔记本侧兄弟项目——后者是手机侧 SOCKS5 服务器，把每个出站 socket 绑到蜂窝
网卡以绕过热点共享检测。niclane 继承了它最重要的工程性质——**fail-closed
（故障即拒绝）**：车道绑定的网卡掉线时，新连接立即被拒绝，绝不静默改走
其他网卡。

---

## 工作原理

```
应用 A ──> 127.0.0.1:7891 (socks5, 车道 wifi) ──[SO_BINDTODEVICE wlan0]──> WiFi 网卡 ──> 上行 A
应用 B ──> 127.0.0.1:7892 (http,  车道 dock)  ──[SO_BINDTODEVICE enx...]───> USB 网卡 ──> 上行 B
应用 C ──> 127.0.0.1:7893 (socks5, 车道 tun)  ──[SO_BINDTODEVICE Mihomo]───> TUN     ──> 代理出口
```

每条车道：

1. **出口钉死在一张网卡上**。Linux 用 `SO_BINDTODEVICE`；macOS 用
   `IP_BOUND_IF`/`IPV6_BOUND_IF`；其他平台回退为绑定网卡源 IP（较弱，见
   [保证与诚实的限制](#保证与诚实的限制)）。
2. **Fail-closed。** 健康监控每 2 秒复查网卡（是否 UP、是否有地址）。
   不健康且 `strict: true`（默认）时，拨号直接以 `SOCKS5 0x01` / HTTP 502
   拒绝。不回退、不泄漏。
3. **DNS 从本车道解析**。域名查询通过被绑定的网卡发出，DNS 无法从其他网卡
   泄漏。回环域名服务器（systemd-resolved）会被 `dns_servers` / 公共解析器
   替换——设备绑定 socket 无法到达回环地址。也可用 `dns: doh` 走
   DNS-over-HTTPS（默认端点：阿里 DNS、Cloudflare），且 HTTPS 查询本身
   也从车道出站。
4. **可选上游链**（`socks5://` 或 `http://`），且上游本身从车道内拨出。
   这正是"经热点网卡串联手机上的 SocksBypass"的用法。
5. **全量计数**：连接数、拒绝数、双向字节数、DNS 查询、UDP 报文——通过
   admin 端点以 Prometheus 格式暴露，`niclane status` 直接查看。

支持 UDP ASSOCIATE（RFC 1928）：客户端数据报经车道绑定的 UDP socket 转发。

## 功能一览

- 每车道支持 SOCKS5（CONNECT + UDP ASSOCIATE，可选用户名密码）与 HTTP 代理
  （CONNECT + 绝对 URI 转发）
- 设备级出口绑定：Linux `SO_BINDTODEVICE`、macOS `IP_BOUND_IF`
- Fail-closed 健康监控，逐车道独立，2 秒粒度
- 每车道独立 DNS，默认防泄漏：车道解析器 / DNS-over-HTTPS（`dns: doh`）/
  系统解析；`dns_servers` / `doh` 可配置
- 上游链（socks5/http），从车道内拨出
- `niclane doctor` —— 网卡清单、能力探测、配置建议
- `niclane test` —— 验证每条车道的真实出口 IP
- `niclane status` + `/metrics` Prometheus 端点
- `niclane bench` —— 按车道吞吐测量与对比
- SIGHUP 热重载：不重启进程增删改车道
- 单一静态二进制，零运行时依赖

## 快速开始

```bash
# 安装（或从 Releases 下载，或 make build）
go install github.com/CallMeHFK/niclane/cmd/niclane@latest

# 查看网卡与设备绑定能力
niclane doctor

# 示例配置——每网卡一条车道（完整参考见 config.example.yaml）
cat > niclane.yaml <<'EOF'
admin: 127.0.0.1:9000
lanes:
  - name: wifi
    listen: 127.0.0.1:7891
    type: socks5
    interface: wlan0        # 出口钉在这张网卡
  - name: dock
    listen: 127.0.0.1:7892
    type: http
    interface: enxeaa1c366f80d
  - name: tunnel
    listen: 127.0.0.1:7893
    type: socks5
    interface: Mihomo          # TUN 设备也是一张"网卡"
EOF

niclane serve -c niclane.yaml

# 验证每条车道的真实出口 IP
niclane test -c niclane.yaml

# 改完配置后热重载（增删改车道无需重启）
kill -HUP $(pidof niclane)
```

像使用普通 SOCKS5 代理一样使用它：`curl --socks5-hostname 127.0.0.1:7891 …`、
`ssh -o ProxyCommand='nc -x 127.0.0.1:7891 %h %p' …`，或在应用代理设置里填
对应车道的端口。

### 权限要求

- **Linux**：旧内核上 `SO_BINDTODEVICE` 需要 `CAP_NET_ADMIN`；较新内核
  （≥ 5.7）允许非特权绑定。`niclane doctor` 会实测探测——以它的报告为准。
  若报告不可用：`sudo setcap cap_net_admin+ep $(command -v niclane)`，或用
  systemd `AmbientCapabilities=CAP_NET_ADMIN` 运行。
- **macOS**：`IP_BOUND_IF` 无需特权。
- **其他平台**：仅源 IP 绑定。

## 基准测试

`niclane bench` 测量每条车道经其绑定网卡能推多少流量。先在车道可达的对端
主机上启动 echo sink：

```bash
niclane bench serve --listen :9999            # 对端主机上执行
niclane bench -c niclane.yaml -target <对端IP>:9999 -duration 10s
```

开发者笔记本实测示例——sink 在扩展坞网卡自身地址上：dock 车道从其网卡推出
约 3 GB/s；wifi 车道直接失败，**因为它根本没有通往坞网段的路由**——这正是
按网卡隔离按设计生效的表现：

```
LANE             TARGET                      UP MB/s   DOWN MB/s  RESULT
dock             198.51.100.23:18999         3059.7      3057.1  ok (4.0s)
wifi             198.51.100.23:18999              -           -  FAIL: dial through lane: i/o timeout

fastest lane by downstream: dock (3057.1 MB/s)
```

## 配置参考

| 字段 | 默认 | 说明 |
|---|---|---|
| `name` | 必填 | 车道名（唯一，作 metrics 标签） |
| `listen` | 必填 | 本地代理监听 `host:port` |
| `type` | `socks5` | `socks5` 或 `http` |
| `interface` | — | 出口钉到此网卡（与 `bind_ip` 互斥） |
| `bind_ip` | — | 按源地址钉出口（较弱） |
| `bind_mode` | `auto` | `device` / `ip` / `auto`（尽量 device，否则 ip） |
| `strict` | `true` | fail-closed：网卡掉线/无地址时拒绝连接 |
| `auth` | — | SOCKS5 入站 `user`/`password` |
| `upstream` | — | 经 `socks5://[user:pass@]host:port` 或 `http://…` 链式上游（从车道内拨出） |
| `dns` | `lane` | `lane`（经本车道解析）、`doh`（DNS-over-HTTPS 经车道）或 `system` |
| `dns_servers` | `223.5.5.5` | lane-DNS 模式下替换回环 NS 的解析器 |
| `doh` | 阿里 DNS + Cloudflare | `dns: doh` 的 DoH JSON-API 基础 URL；建议用 IP 字面量 `https://`（无需引导解析） |
| `idle_timeout` | 无 | 空闲超时（`5m`、`300s`…） |
| `admin` | — | 全局：`/metrics` 与 `/status` 的 `host:port` |
| `log_level` | `info` | `debug` / `info` / `warn` / `error` |

## 保证与诚实的限制

已由测试套件和/或真机验证：

- ✅ 设备绑定出口：TCP 与 UDP 流量只从被绑定的网卡出站，即使存在其他默认
  路由（Linux ≥ 5.7 非特权可用，macOS 非特权可用）。
- ✅ Fail-closed：网卡缺失/掉线/无地址 ⇒ 立即拒绝，绝不静默回退。
- ✅ 每车道 DNS：查询从车道出站；回环 NS 自动替换；`dns: doh` 已对阿里
  DoH 实连验证。
- ✅ UDP 目标解析走车道 DNS 模式（SOCKS5 UDP ASSOCIATE 域名目标）。
- ✅ 上游链从车道内拨出。
- ✅ 回环端到端：SOCKS5 CONNECT/UDP、HTTP CONNECT/转发、认证、拒绝路径、
  字节计数（CI：ubuntu/macos/windows，`-race`）。

诚实的限制：

- ⚠️ **源 IP 模式（`bind_ip` 或 `bind_mode: ip`）是尽力而为**：内核仍可能
  把包从其他接口发出（非对称路由）。只有 device 模式提供硬隔离。doctor 会
  告诉你实际处于哪种模式，`/status` 里有每车道的 `bind_mode`。
- ⚠️ **Windows 用 `IP_UNICAST_IF` 绑定**（无需特权）：仅约束发送路径，
  按尽力而为对待。
- ⚠️ **源 IP 模式的车道每条 UDP 关联只绑一个 socket**，优先取网卡 IPv4
  地址；双栈并存时该类车道无法到达 IPv6 UDP 目标（device 模式无此限制）。
- ⚠️ **UDP 目标解析走车道 DNS 模式**；UDP 目标为 IP 字面量时无需解析。
  v0.3.0 之前域名走系统解析器。
- ⚠️ **`dns: system`** 时 DNS 走宿主默认路由——相对车道是潜在 DNS 泄漏。
  绑定车道默认 `dns: lane`。
- ⚠️ **字节计数**覆盖 SOCKS5 中继与 HTTP CONNECT 隧道；普通 HTTP 转发仅计
  连接数。
- ⚠️ 不同车道的出口 IP 可能相同（多条物理上行经同一宽带 NAT）——按网卡
  绑定关心的是"包从哪个接口出"，不是远端拓扑。

## 场景配方

### WiFi 与扩展坞分流

```yaml
lanes:
  - { name: wifi, listen: 127.0.0.1:7891, interface: wlan0 }
  - { name: dock, listen: 127.0.0.1:7892, interface: enxeaa1c366f80d }
```

`curl --socks5-hostname 127.0.0.1:7891 https://example.com` 从 WiFi 出；
同一命令打 `7892` 从扩展坞出。`niclane test -c niclane.yaml` 打印每条车道
实测出口 IP。

### 经热点网卡串联手机上的 SocksBypass

笔记本连手机热点（设为 `wlan1`），手机上跑
[SocksBypass](https://github.com/Nanako0129/SocksBypass)（:9876），然后：

```yaml
lanes:
  - name: cell
    listen: 127.0.0.1:7894
    interface: wlan1                     # 只用热点网卡
    upstream: socks5://198.51.100.1:9876 # 从 wlan1 拨出
```

流量从笔记本热点网卡进入手机，再由手机侧绑蜂窝发出——`wlan0` 上的局域网
完全看不到这些流量。

### 与 TUN 模式代理共存（mihomo / sing-box）

全局 TUN 设备会劫持默认路由。`interface: <tun>` 的车道是**有意骑进隧道**；
绑定物理网卡的车道照常工作——`SO_BINDTODEVICE` 的路由查找被约束在该设备上，
无需 fwmark 或策略路由改动。细节与坑（DNS 交互、UID 规则）见
[docs/coexistence.md](docs/coexistence.md)。

### systemd 单元

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

## 与同类工具对比

| | niclane | SocksBypass | proxychains-ng | `curl --interface` |
|---|---|---|---|---|
| 运行于 | 笔记本/服务器（Linux、macOS、Windows 尽力支持） | 手机（iOS/Android） | 笔记本 | 单条命令 |
| 机制 | 本地代理 + 设备级出口绑定 | 代理服务器把自身出口绑到蜂窝 | LD_PRELOAD 钩子 | 进程级源绑定 |
| 按网卡隔离 | 硬（device 模式） | 硬 | 无 | 软（源绑定） |
| Fail-closed | 默认开启 | 开启 | 无 | 无 |
| 按应用选择 | 按代理端口 | 不适用（手机即代理） | 配置文件 | 每次调用指定 |
| 与 SocksBypass 关系 | **互补——可串联** | — | — | — |

## 路线图

- [ ] 带 TTL 的 DNS 应答缓存
- [ ] SOCKS5 BIND 命令（很少使用）

## Agent 技能

niclane 自带一份 [Agent Skill](skills/niclane/SKILL.md)，教会编码 Agent 完整
操作闭环（doctor → 配置 → serve → 以 `test` 为验收门 → 热重载）以及需要如实
披露的失败模式。安装后 Agent 即可自主驱动 niclane：

```bash
mkdir -p ~/.agents/skills && cp -r skills/niclane ~/.agents/skills/
```

## 参与开发

```bash
make build    # ./niclane
make test     # go test -race ./...
make vet      # go vet ./...
```

欢迎 PR——尤其是平台绑定器（BSD、Windows）与基准测试。任何触及拨号路径的
改动请保持 fail-closed 语义不变。

## 许可证

MIT —— 见 [LICENSE](LICENSE)。
