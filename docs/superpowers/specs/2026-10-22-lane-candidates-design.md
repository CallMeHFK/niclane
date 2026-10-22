# niclane 车道候选集（多上游健康选路 + 自动发现）设计

日期：2026-10-22
状态：已评审（CodeBuddy + QwenPaw），按评审修订

## 1. 背景与目标

现状：一条 lane = 一个网卡 + 一个上游代理，网卡掉了 fail-closed。真实场景（两台手机 USB
共享、各自在网关跑 HTTP 代理）需要靠"两条 lane + 外部 fallback 组 + 主机侧 adb/口名脚本"
拼出回退。

目标：一条 lane 一个稳定监听口，运行期维护一组 `(网卡, 上游代理)` 候选；按优先级用健康
候选承载新连接，失败自动换下一个，全挂 fail-closed。候选可静态配置，也可按"带物理设备的
接口 + 网关 + 端口列表"自动发现。不绑定任何厂商/机器/固定 IP。

非目标（本期不做）：设备激活（adb/USB 共享）、UDP 上游代理、mihomo/其他代理配置生成、
网段扫描、mDNS、macOS/Windows 的自动发现。

## 2. 配置面（向后兼容）

```yaml
discovery:                      # 全局：用户维护的常见代理端口
  ports: [10808, 10809, 7890, 7891]

lanes:
  - name: internet
    listen: 127.0.0.1:7893
    type: socks5
    strict: true
    candidates:                 # 静态候选（可选）
      - interface: enx020000000001
        upstream: http://198.51.100.10:10808
        priority: 1000          # 可选，默认 1000
    discover:                   # 自动发现（可选）
      interfaces: [device]      # device | any | glob（如 "enx*"）；默认 [device]
      ports: []                 # 非空时覆盖全局 discovery.ports
      via: {}                   # 可选：interface -> 网关IP 显式覆盖
      probe_url: https://www.gstatic.com/generate_204
      interval: 10s             # 探活/重扫周期，最小 2s
```

规则：
- 旧写法 `interface`/`upstream` 等价于单候选；与 `candidates` 或 `discover` 同时出现时
  `Validate` 直接报错（不静默择一）。
- 候选至少要有 `interface` 或 `upstream` 之一；`(interface, upstream)` 去重。
- `priority` 越大越优先；静态默认 1000，发现候选固定 100；同优先级按探活延迟、再按配置顺序。
- `ports`：全局默认 `[10808,10809,7890,7891]`；lane 非空则整体覆盖；端口 1..65535 去重。
- `probe_url`：默认 `https://www.gstatic.com/generate_204`；必须 http(s) 绝对 URL，
  且 host 不得为回环/链路本地/组播/未指定地址（防 SSRF/误打本机）。
- 热重载：配置变更 = 该 lane 重启（在跑连接被切，与现 supervisor 行为一致，文档如实写）；
  运行期发现集变化不重启。

## 3. 运行期架构：Selector 包裹子 Lane

- 配置面仍是"一个 lane = 一个监听口"。运行期由 `internal/selector.Selector` 持有
  `[]*lane.Lane` 子车道（每个候选一条），并实现 `lane.Provider`：

```go
type Provider interface {
    Acquire(ctx context.Context) (Egress, error) // 每连接 pin 一个子 lane
    Reject(reason string)
    Stats() *metrics.Stats
    Name() string
}
```

- `lane.Lane` 也实现 `Provider`（`Acquire` 返回自身），socks5 / httpproxy 改为面向
  `Provider`：**每条客户端连接在开始处 Acquire 一次**，随后 Resolve/Dial/UDP/Reject 全程
  使用同一个子 lane，杜绝"DNS 走 A、数据走 B"。
- httpproxy 的转发路径按子 lane 缓存 `http.Transport`（每个候选独立连接池），避免跨候选
  复用隧道。
- 无候选可用时：strict=true 返回 `lane.ErrFailClosed`；strict=false 退化到"任一网卡在位的
  子 lane"（保持现有非严格语义）。
- 子 lane 复用现有 `lane.New`，只派生候选相关字段（Interface/Upstream/BindMode/Strict/
  DNS/DNSServers/Doh），监听/认证/类型不进入子 lane。

## 4. 健康模型

- 网卡健康：沿用子 lane 自带 2s 快照（网卡在位/有地址）。
- 外网健康：Selector 每 `interval` 对每个候选做一次探活：经子 lane `DialContext` 连到
  `probe_url` 主机（含上游 CONNECT/SOCKS 握手），https 走 TLS，要求 2xx 且总超时 5s。
- 防抖：连续 2 次探活失败判死，成功 1 次立即恢复；判死候选仍显示在 `/status`。
- 失败归因（关键）：
  - **传输层失败**（拨上游失败/超时、握手 IO 错误、`ErrFailClosed`、DNS 失败）→ 本次连接
    换下一个候选（上限 min(候选数,3)），并对该候选触发一次单飞即时复测（冷却 2s）；
    不直接改判死状态。
  - **应用层拒绝**（HTTP CONNECT 非 2xx、SOCKS5 reply≠成功）→ 原样返回客户端，
    **不换候选、不影响健康**（避免"一个被墙目标把全部流量切走"）。
  - 在 `internal/lane` 增加哨兵错误 `ErrUpstreamReject`；client.go 在协议层拒绝处返回
    可 `errors.Is` 识别的错误，其余错误保持传输层语义。
- UDP：只在"无上游"候选子集里选（现有上游不支持 UDP 的限制不变）；子集为空则报错。

## 5. 自动发现（Linux，一期先静态/二期发现）

- 接口：`device` = `/sys/class/net/*/device` 存在；`any` = 全部非 lo 接口；glob 用
  `path/filepath.Match`。每 `interval` 重扫。
- 网关优先级：`via` 覆盖 → 该接口的默认路由（解析 `/proc/net/route`，仅 IPv4）→
  systemd-networkd 租约（`/run/systemd/netif/leases/<ifindex>` 的 `ROUTER=`）→
  NetworkManager（`nmcli -t -f IP4.GATEWAY device show <if>`，argv 调用，2s 超时）。
  全部失败则跳过并记日志。绝不使用全局默认路由，绝不扫网段。
- 端口预筛：对该网关逐个端口 TCP 探测（经候选接口绑定，800ms，受控并发 4）；仅 TCP 通的
  端点进入候选集。上游按 `http://gw:port` 处理（SOCKS5 端口发现不在本期）。
- 生命周期：发现集与静态候选合并；`(interface, upstream)` 去重；接口消失/端口关闭的发现
  候选下一轮移除；静态候选永远保留。首次探活成功的发现候选记一条 Info 日志。
- 非 Linux：`discover` 在 doctor/启动时明确报"不支持"，不静默空转。

## 6. 可观测与 CLI

- `/status` 的 `LaneStatus` 增加 `candidates[]`：interface、upstream、source
  (static/discovered)、priority、healthy、delay_ms、last_error。lane 级计数为子车道求和。
- `/metrics`：lane 级仍暴露 healthy/bind_mode；selector 车道不聚合子车道计数器（文档写明，
  `/status` 有完整明细）。
- `niclane test`：对含候选的车道逐个候选实测出口 IP，并标出当前生效候选。
- 新增 `niclane discover -c ...`：只探测不服务，打印"接口 → 网关 → 端口 → 探活结果"。
- `niclane status` 打印 candidates 表。

## 7. 安全

- 默认只探"本接口 DHCP 网关"，`any` 列为高级选项并在文档警示不可信 LAN。
- 自动信任的代理首次转正记 Info；不做端口扫描（仅探测配置的端口列表）。
- `probe_url` 校验禁止回环/链路本地/组播；默认 https。

## 8. 测试

- config：旧字段与新字段冲突报错；默认值；端口校验去重；probe_url 校验。
- lane：`ErrUpstreamReject` 与传输错误的分类（真实 httptest HTTP 代理返回 403 / 拒连）。
- selector：优先级排序、传输失败换候选、应用层拒绝不换、探活 2 次判死/1 次恢复、
  Provider.Acquire pin 同一子 lane、状态聚合。
- discover：`/proc/net/route` 十六进制解析、lease/nmcli 解析、接口 spec 匹配、端口预筛、
  网关优先级（可注入 sources 的 fixture）。
- 服务器：现有 socks5/httpproxy 测试改 Provider 后全部通过；增加"一次 Acquire 贯穿连接"
  的单元测试。
- 真机验收：两台手机插拔/断网，`niclane test`、真流量切换、`discover` 输出。

## 9. 交付分期

1. **一期**：配置 schema + Selector/Provider + pin + 健康归因/防抖 + 静态候选 + status/test。
2. **二期**：discover（接口/网关/端口/预筛）+ `niclane discover` + 文档/示例/skill 更新。

两期都完成后打一个 release；每期独立提交、测试全绿。
