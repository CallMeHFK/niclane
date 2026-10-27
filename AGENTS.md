# AGENTS.md — niclane agent 工作手册

> **权威源关系**：本文件是「在这个仓库怎么干活」的唯一入口。
> 开源流程的通用细则权威源在 `~/develop/opensource/`（下称 **OSS 规则集**，
> 9 份 `*-rules.md` + `README.md` 索引），本文件只做**索引**和**本项目裁剪**。
> 本文件自身要能在公开仓库里提交 —— 它按 OSS 规则③做过四类隐私筛查，
> 因此**不复述任何真实网卡名 / 内网 IP / 家目录路径**，需要现场枚举时用 §6.2 的命令。
> 两者冲突时按 OSS 规则集 `README.md` 的裁决顺序：branch protection 与 security
> 这类硬约束 > 流程建议；规则里的 *Done when* 检查 > 周边散文。
> 改规则本体（OSS 规则集）是独立的、需人明确提出的动作，不在本仓库任务内。

---

## 0. 先读：三个最容易踩的坑

1. **真实网卡名 / 内网 IP 是本仓库的泄漏红线。**
   线上（`origin/main`）的历史已被整体脱敏（RFC 5737 文档 IP + 通用网卡名），
   本地 `main` 也已于 2026-10-26 对齐线上，工作区**已无真实值**（见 §6）。
   **任何文档、示例、测试夹具里只允许出现脱敏值** —— 每次提交前跑 §4.1 的
   四类筛查，那是把真实值挡在历史之外的唯一闸门。写新示例前先看 §6.2 的对照表。
2. **`fail-closed` 语义不可破坏。** 车道的网卡挂了就必须拒绝连接，
   绝不静默改道。README 明文要求："keep the fail-closed semantics intact in
   any change that touches dialing"。碰 `internal/lane`、`internal/selector`
   的拨号路径时，这条优先于任何便利性改动。
3. **改一处能力 = 同步 5 个文件。**
   `README.md`（英文）、`README.zh-CN.md`（中文，逐段对应）、
   `config.example.yaml`、`skills/niclane/SKILL.md`（会被 goreleaser 打进每个
   release 压缩包）、`CHANGELOG.md`。少一个，发布出去的文档就是自相矛盾的。

---

## 1. 项目速览

**niclane** —— 一个网卡一条车道（one NIC, one lane）：把每个网络接口变成独立的
本地 SOCKS5/HTTP 出口，出站 socket 强制绑到指定网卡（Linux `SO_BINDTODEVICE`、
macOS `IP_BOUND_IF`、Windows `IP_UNICAST_IF`），车道之间不串流、失败即关闭。
v0.4.0 起支持 `candidates:` / `discover:` 的多网卡候选集与自动发现（故障切换仍 fail-closed）。

| 项 | 值 |
|---|---|
| 语言 / 版本 | Go（`go.mod` 声明 `go 1.25`） |
| 唯一依赖 | `gopkg.in/yaml.v3` |
| 入口 | `cmd/niclane`（子命令：`doctor` `serve` `test` `status` `discover` `bench` `version` `debug` `help`） |
| 许可 | MIT |
| 平台 | linux / darwin / windows（amd64 + arm64，windows 无 arm64） |
| CI | `.github/workflows/ci.yml`：gofmt → vet → build → `go test -race`（3 OS 矩阵） |
| 发布 | `.github/workflows/release.yml`：推 `v*` tag → goreleaser |
| 当前版本 | 0.4.0（`CHANGELOG.md` 顶部条目即最新版本，无 standing `[Unreleased]`） |

---

## 2. 环境与命令

**Go 不在默认 PATH 上**（本机装在 `~/.local/go/bin`，实测 `go1.27.1`）。开新 shell 先：

```bash
export PATH="$HOME/.local/go/bin:$PATH"
```

```bash
make build      # → ./niclane
make test       # go test -count=1 ./...        （全套 ~1.1s）
make race       # go test -race -count=1 ./...  ← CI 真正跑的那条
make vet        # go vet ./...
make fmt        # gofmt -w .   （提交前跑，CI 用 gofmt -l 卡）
```

**提交前最小门禁**（与 CI 对齐，本机实测全绿）：

```bash
test -z "$(gofmt -l .)" && go vet ./... && go build ./... && go test -race -count=1 ./...
```

`go build ./...` 不是多余的 —— `go test` 会**静默跳过没有测试文件的目录**，
历史上因此漏掉过整个未跟踪的 `cmd/`（见提交 `fix: cmd/niclane was ignored…`）。

其他常用命令：

```bash
niclane doctor            # 先探主机能力（是否允许 device binding、列出网卡、给建议配置）
niclane serve -c niclane.yaml
niclane test -c niclane.yaml -url http://members.3322.org/dyndns/getip   # 验收闸：每条车道报自己的 exit-ip
```

---

## 3. 代码地图与 graphify（查图先行）

本仓库已初始化代码知识图谱（CLI：`graphify 0.9.95`，位于 `~/.local/bin/graphify`）。

- **当前状态**：图的规模与 `built_at_commit` **用命令读，别信本文件里的数字**
  （社区数在两次重建间会波动 —— 本仓库同一份代码实测出过 29 和 23 个社区，
  所以这里不写死）：
  ```bash
  python3 -c "import json;d=json.load(open('graphify-out/graph.json'));print(len(d['nodes']),'nodes /',len(d['links']),'links /',len({n['community'] for n in d['nodes']}),'communities |',d['built_at_commit'])"
  git rev-parse HEAD        # 两个 SHA 必须相等，见 §3.3 的 Done when
  ```
  撰写时：**618 节点 / 1688 边 / 23 社区**，`built_at_commit` == `HEAD`，无垃圾节点。
- **钩子已装**：`post-commit` / `post-checkout` / `post-merge` / `post-rewrite`
  + `graph.json` 的 git merge driver（`graphify hook status` 复核，四项 + driver 全 installed）。
  由 `graphify hook install` 一并创建的 `.gitattributes`（`graphify-out/graph.json merge=graphify`）
  **保留并提交**。
- **`graphify-out/` 是本地产物，永不提交、永不手改 `graph.json`**（已在 `.gitignore`）。
  要改就改源码或 `.graphifyignore`，然后重建。

### 3.1 规则：结构问题先查图，grep 是第二顺位

「X 怎么工作的」「谁调用它」「改了会影响什么」「两个概念怎么连起来」——
这类问题先走图，一次查询带出 `file:line` 引用，比跨文件 grep 十几次便宜且可审计。

| 问题形态 | 命令（本仓库实测过） |
|---|---|
| 宽泛的「X 怎么运作」 | `graphify query "<问题>"` （BFS，宽上下文） |
| 一条具体调用链 | `graphify query "..." --dfs --budget 1500` |
| 两个概念之间的最短路 | `graphify path "Selector" "Lane"` → `Selector → candidate → Lane` |
| 某个节点是什么 | `graphify explain "Selector"` （`--all` 列全部连接，默认 20 条最连通的） |
| 改 X 会波及谁 | `graphify affected "LaneConfig" --depth 2`（反向遍历，默认深度 2） |
| 架构枢纽 | `graphify god-nodes --top 8` → `Lane(42) Selector(38) LaneConfig(28) Stats(21) Egress(20) Provider(18)` |

### 3.2 退出码语义（已实测，可依赖）

| 码 | 含义 | 你要做的 |
|---|---|---|
| `0` | 有答案 | 引用其中的节点 label / id / `file:line`，**改代码前抽查一次源码** |
| `3` | 名字歧义，列出了候选 | **用完整 id 重跑**（如 `internal_selector_selector_go_selector_selector`），不要猜 |
| `4` | 没匹配到 | 先查图是否过期（见下）——新写的代码没进图是常态，别急着断言"它不存在" |

*Done when*：答案里的 `file:line` 抽查能对上源码，才在它之上做改动；
对不上的引用一律当作图错了，改源码或重建，不要照着改。

### 3.3 保持图新鲜

| 场景 | 命令 | 代价 |
|---|---|---|
| 改了代码 | `graphify update .` | 增量 AST 重抽，无 LLM，本仓库实测 **0.6s** |
| 大量删代码后 | `graphify update . --force` | 否则收缩守卫会拒绝变小的图（也可 `GRAPHIFY_FORCE=1`） |
| **`built_at_commit` 落后于 HEAD** | `rm graphify-out/graph.json && graphify update .` | **`--force` 不是"强制重建"**（它的语义只是"允许写入更小的图"）。拓扑没变时 `update` 会打印 `No code-graph topology changes detected; outputs left untouched` 并**连 `built_at_commit` 一起不写** —— 只有让图从零重建才会刷新元数据。本仓库实测：`update`/`update --force` 都刷不动，删掉 `graph.json` 后重跑即 `== HEAD`（节点/边数不变） |
| 每次 commit 自动更新 | `graphify hook install`（**已装**） | post-commit 等五个钩子，只管代码层 |
| 长会话边写边更 | `graphify watch .` | 文件变更即重建 |
| 语义层 / 文档层 | `graphify extract . --backend <gemini\|kimi\|claude\|openai\|deepseek\|ollama>` | **需要 LLM key，本机未配** → 当前只有代码层（`EXTRACTED` 边），没有 `INFERRED` 边 |
| 例行体检 | `graphify check-update .` | 只读，语义重抽待办时才提示（本仓库当前静默退出 0 = 无需处理） |

*Done when*：`graph.json` 的 `built_at_commit` == `git rev-parse HEAD`，
且**最新改动里的某个符号**能 `graphify explain "<新符号>"` 退出 0。

```bash
python3 -c "import json;print(json.load(open('graphify-out/graph.json'))['built_at_commit'])"
git rev-parse HEAD
```

> 注意：
> - commit 之后钩子会自动重建，正常情况不需要手动 `update`；只有钩子被跳过
>   （rebase/merge 中钩子会主动跳过）或 `git checkout` 到别的分支后才要补跑。
> - **文档改动也会触发重建** —— 本文件和两份 README 自己就是图里的节点
>   （`graphify explain "AGENTS.md"` 实测退出 0），所以改文档提交后 `built_at_commit`
>   通常会自动跟上。
> - 唯一会"跟不上的"是**拓扑没变**的提交（比如只改了 `.go` 里的字符串字面量）：
>   `update` 打印 `No code-graph topology changes detected` 就连元数据一起不写。
>   要刷新就按上表最后那行，删掉 `graph.json` 从零重建。

### 3.4 别污染图

- **建图前先写 `.graphifyignore`**（本仓库目前不需要：仓库只有 33 个 `.go`、
  52 个被跟踪文件（`git ls-files | wc -l`），无 vendored/minified 产物，`.codebuddy/` `.zcode/` 已被 `.gitignore`
  排除，graphify 默认尊重 `.gitignore`）。**将来一旦引入大体积生成物/三方 bundle，
  先加 ignore 再重建**；症状是 `god-nodes` 顶上出现单字母函数名。
- `graphify-out/` 里的 `graph.html` / `GRAPH_REPORT.md` / `memory/` / `reflections/`
  都是产物，同上不提交。
- 语义层引用优先信 `EXTRACTED` 边，`INFERRED` 边是推断 —— 图是地图不是疆界。

### 3.5 反馈环（查询有意义时顺手做）

```bash
graphify save-result --question "Q" --answer "A" --outcome useful|dead_end|corrected \
  [--nodes NodeA NodeB] [--correction "正确答案是…"]
# 聚合：
graphify reflect    # → graphify-out/reflections/LESSONS.md
```

图把人引错了就标 `corrected` + `--correction`，那是未来查询最该学的信号。

### 3.6 包结构速查（图谱的自然入口）

| 包 | 职责 |
|---|---|
| `cmd/niclane` | CLI 入口与子命令 |
| `internal/config` | YAML schema（lanes / candidates / discover / global） |
| `internal/lane` | 核心：网卡绑定、拨号、fail-closed、DoH、UDP（**红线区**） |
| `internal/selector` | 候选集选择 + 健康探测（`probeAll` / `probeOne` / `deadLocked`） |
| `internal/discover` | 自动发现：`/proc/net/route` → systemd lease → `nmcli` 取每网卡自己的网关，再探端口 |
| `internal/socks5` / `internal/httpproxy` | 两种入站协议实现 |
| `internal/relay` | 字节搬运（含半关闭语义） |
| `internal/supervisor` | 车道生命周期 + SIGHUP 热重载 |
| `internal/doctor` | 主机能力探测（无测试文件） |
| `internal/metrics` | Prometheus 渲染 |
| `internal/bench` | 基准测试 |
| `internal/version` | goreleaser ldflags 注入版本号 |

---

## 4. 开源规范（OSS 规则集索引）

权威源：`~/develop/opensource/`。**按 *Fire when* 判断是否适用，读完再动手。**

| 规则文件 | 领域 | 什么时候用 |
|---|---|---|
| `branch-protection-rules.md` | 分支保护 | 任何开源仓库：创建 / 加固 / 定期审计 |
| `repo-hygiene-rules.md` | 必备文件 + 仓库设置 | 建仓、公开就绪审计 |
| `pr-rules.md` | PR 流程 | 开 / 评 / 合 PR |
| `commit-message-rules.md`（已改名 **Commit Rules**） | 提交格式 **+ AI 署名禁令 + 提交前隐私筛查** | 写或审 commit、生成发布说明、**以及每次提交前** |
| `issue-triage-rules.md` | Issue 分诊 | 有 issue 开着或待分诊 |
| `release-rules.md` | 版本与发布 | 发版 / 判断是否该发版 |
| `security-policy-rules.md` | 漏洞处理 | 仓库转公开、收到第一份报告 |
| `graphify-rules.md` | 代码知识图谱 | 在有图的仓库里干活（**已本地化到 §3**） |

**通用约定**（照抄自 OSS `README.md`，本仓库同样适用）：

- 每一步都带 *Done when*，**对着真实仓库状态验过才算完成**，命令敲了不算。
- 不绕过分支保护，不重写 `main` 历史。
- 两份文档打架时，更硬的那条赢。
- 有 `graphify-out/` 的仓库，结构问题先查图（§3）。

### 4.1 本仓库的落地要点

- **提交信息**：Conventional Commits —— `<type>(<scope>): <subject>`，
  type 只能取 `feat|fix|docs|style|refactor|perf|test|build|ci|chore|revert`，
  subject 祈使句、无句号、≤72 字符；body 每行 72 列、只解释 *why*。
  ```bash
  git log --format='%s' -10 | grep -vE '^(feat|fix|docs|style|refactor|perf|test|build|ci|chore|revert)(\(.+\))?!?: .+'
  # 期望：空输出
  # 实测：最近十条有 1 条不合规（release: v0.4.0 …），全量 28 条有 6 条
  #       （5 条 release:/v0.x 前缀 + 1 条首提交）—— 只约束新提交，不去改史
  ```
- **② 不给 AI 署名**（OSS Commit Rules 第 2 步）：改动由 agent 做的，commit 仍然算
  指挥它的那个人。**禁止**出现 `Co-Authored-By:` 指向 AI/工具的 trailer、
  `Generated with …` 尾注、`🤖` 标记、代码注释或 PR 描述里的 "AI-generated" 说明。
  工具在历史里必须是隐形的。
  ```bash
  # 历史里的署名（主检查）
  git log --format='%B' -10 | grep -iE 'co-authored-by|generated (with|by)|🤖|ai-generated'
  # 本次暂存的代码/配置里有没有暗署名
  git diff --cached -- '*.go' '*.yaml' '*.yml' '*.json' | grep -inE 'co-authored-by|generated (with|by)|🤖|ai-generated'
  # 期望：两条都无命中
  ```
  > **自匹配提示**：不加 `-- '*.go' …` 收窄、对整个暂存区跑上式，会命中本文件
  > §4.1 里记录这条规则的那 4 行 —— 那是规则文本本身，不是署名。按 OSS 规则③
  > Done-when 的口径，命中要能说出为什么留：**这里是"记录禁令"，不是"违反禁令"**。
  > 本仓库实测：历史消息 ✅ 无命中、代码/配置暂存 ✅ 无命中、全量暂存 ❌ 4 行自匹配。
- **③ 提交前隐私筛查四类**（OSS Commit Rules 第 3 步，**闸门设在 commit 时** ——
  推出去之后再删就等于改写 `main` 历史，是被禁止的；GitHub secret push protection
  只是兜底不是闸门）。四类全部对 `git diff --cached` 查：
  ```bash
  # [1] 凭据：sk- / ghp_·gho_ / AKIA / xox* / AIza
  git diff --cached | grep -inE 'sk-[A-Za-z0-9]{20,}|gh[pousr]_[A-Za-z0-9]{20,}|AKIA[0-9A-Z]{16}|xox[baprs]-|AIza[0-9A-Za-z_-]{30,}'
  # [2] 泛凭据赋值（api_key / secret / token / password / authorization = 长值）
  git diff --cached | grep -inE '(api[_-]?key|secret|token|passwd|password|authorization)["'"'"']?[[:space:]]*[:=][[:space:]]*["'"'"']?[A-Za-z0-9+/_-]{16,}'
  # [3] 个人信息（邮箱、电话）—— noreply@ 和 example.com 这类夹具是允许的
  git diff --cached | grep -inE '[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+\.[A-Za-z]{2,}'
  # [4] 实机环境：家目录 / 主机名 / 私网 IP —— 本仓库的主雷区，见 §6
  git diff --cached | grep -inE '/home/[a-zA-Z0-9_]+/|/Users/[a-zA-Z0-9_]+/|192\.168\.[0-9]+\.[0-9]+|[^0-9]10\.[0-9]+\.[0-9]+\.[0-9]+|172\.(1[6-9]|2[0-9]|3[01])\.[0-9]+\.[0-9]+'
  ```
  *Done when*：四条全部无命中，或者每一处命中你都能说出它为什么是**故意的非敏感
  夹具**（测试 token、`example.com`）并留着。**默认结论是改掉，不是解释掉。**
  本仓库的第 [4] 条曾拦下本文件自己（家目录路径 + §6 复述的真实网卡名/IP），
  正是这道闸该有的表现。
- **PR 标题 = `main` 上的提交主题**（若采用 squash 合并），因此 PR 标题同样要符合上面的格式。
- **分支保护的 `required_status_checks.contexts`** 本仓库的真实 check 名是
  `lint`、`test (ubuntu-latest)`、`test (macos-latest)`、`test (windows-latest)`
  （用 `gh api --paginate repos/CallMeHFK/niclane/commits/main/check-runs --jq '.check_runs[].name' | sort -u`
  复核；`--paginate` 不能省）。
- **发版**：先改 `CHANGELOG.md` → `git tag -a vX.Y.Z -m "vX.Y.Z"` → 推 tag →
  goreleaser 自动出 5 个平台产物。tag 必须是 annotated（`git ls-remote --tags origin`
  要看到 `^{}` 剥离行），已发布版本永不删除、永不重打，改出新的 patch。
  goreleaser 的 changelog filter 排除 `docs` / `chore` / `test` 三类，**带不带 scope 都算**
  （正则 `^(docs|chore|test)(\([^)]*\))?!?:`，见 `.goreleaser.yaml`）。2026-10-28 之前写的是
  裸 `^docs:` 三条，`docs(agents):` / `chore(release):` 这种带 scope 的写法匹配不上 ⇒
  v0.4.1 的正文因此混进了两条杂务提交。所以**用户可见的改动用 `feat:`/`fix:`/`refactor:`
  等都会进发布说明**；纯文档、杂务、测试改动务必带上 `docs:`/`chore:`/`test:` 前缀（可带
  scope），否则会污染 Release 正文。

---

## 5. 本项目硬约束（OSS 规则之外）

1. **脱敏红线**（见 §0.1 与 §6）：文档 / 示例 / 测试夹具只能用
   `198.51.100.0/24`（RFC 5737 TEST-NET-3）、`127.0.0.1`，网卡名用
   `enx020000000001` 这类占位、无线用 `wlan0` / `wlp130s0`。
   RFC 1918 私网段（`192.168/16`、`10/8`、`172.16/12`）的真实 IP，和由 MAC
   派生的 `enx<12位十六进制>` 网卡名，一律不许出现在会被提交的内容里 ——
   这正是 OSS 规则③第 [4] 条 grep 要抓的东西，**提交前先跑 §4.1 的四条筛查**。
2. **fail-closed 不可破坏**：改动任何拨号 / 候选切换 / 探测逻辑后，
   必须能论证"车道无健康候选时仍然拒绝"。
3. **三平台语义一致**：`internal/lane` 下的 `binder_linux.go` / `binder_darwin.go` /
   `binder_windows.go` 是同一抽象的平台实现，另有 `binder_family.go`（地址族判定）和
   `binder_generic.go`（无绑定能力时的退化路径），共 5 个文件；改绑定接口要几处同步。
   CI 在 ubuntu/macos/windows 上都会跑 `-race` 测试。
4. **双语 README 逐段对应**：`README.md` 与 `README.zh-CN.md` 结构、代码块、
   数值必须一致，只差语言。
5. **`skills/niclane/SKILL.md` 会被打进 release 压缩包**
   （见 `.goreleaser.yaml` 的 `archives.files`），改它的能力描述等于改对外契约。
6. **本地扫描产物不入库**：`.codebuddy/`、`.zcode/`、`dist/`、`*.test`、`coverage.*`
   已在 `.gitignore`；新增同性质目录时一并加进去。
7. **设计文档惯例**：动手前的方案放 `docs/superpowers/specs/YYYY-MM-DD-<主题>-design.md`，
   执行计划放 `docs/superpowers/plans/YYYY-MM-DD-<主题>.md`；平台交互类的深水区
   单独成文（参考 `docs/coexistence.md` 讲 TUN 代理共存）。
8. **需要特权的部分**：device binding 在无权限时会退化，
   `sudo setcap cap_net_admin+ep $(command -v niclane)` 是文档里给出的唯一提权路径，
   不要引入新的 root 依赖。

---

## 6. 同步状态与泄漏对照（**推送前必读**）

### 6.1 当前状态：**已对齐**（实测 2026-10-28）

下表是**快照，会随新提交过期** —— 每次动 `main` 前用右列命令重测；
其中"左列必须是 `0`"是硬判据（落后于线上就不许推，见 §6.3）：

| 项 | 快照（撰写时） | 复测命令 |
|---|---|---|
| 共同祖先 | `8fed6f3` = `origin/main` → **同源** | `git merge-base origin/main main` |
| 领先 / 落后 | **`0 0`** | `git rev-list --left-right --count origin/main...main` |
| 提交数 | 本地 30 / 线上 30 | `git rev-list --count main` / `… origin/main` |
| 树差异 | **空**（`main` 与 `origin/main` 完全一致） | `git diff --stat origin/main main` |
| 工作区 | `git status` 空 | `git status --porcelain` + 下面的枚举脚本 |

**§6.2 那 9 个真实值在对齐时随 `git reset --hard origin/main` 一起被换掉了**
（本地历史被丢弃，见下）。工作区现在只剩**占位与故意夹具**：
5 个文档占位网卡名（`enx02000000000N`）、3 个 `10.x` 私网示例、
2 个一眼假的占位 MAC（`config_test.go` 2 处、`skills/niclane/SKILL.md` 1 处）——
后者在 `origin/main` 里也都存在，按 §4.1 ③ 的 Done-when 属于"留着并能说出理由"。

#### 这次对齐是怎么做的（历史记录，防止下次又长出第二条历史）

对齐前是**同一批工作的两份平行重写**：`main` 与 `origin/main` 无共同祖先、各 27 笔、
提交消息/作者/时间戳逐一对应，内容只差 8 个文件 / 34 行 —— 全是 §6.2 那类脱敏值差异
（线上脱敏、本地原始）。`git cherry` 显示本地没有线上缺失的独立工作。

经**人明确批准**（§6.3 要求），2026-10-26 执行：

```bash
git branch -f backup/pre-align main        # ① 先留退路，指向旧平行历史
git reset --hard origin/main               # ② 本地对齐线上（方向不能反，见 §6.3）
git cherry-pick f2fc941                    # ③ 把 AGENTS.md 那笔摘回来
```

- 结果（当时）：`0 1`（只领先摘回来的那一笔），`merge-base == origin/main`，工作区干净。
- **未 force-push**，线上一个字节都没动。
- 退路：`backup/pre-align` = `f2fc941`。旧历史的提交对象**仍带真实标识**，它们只能
  留在本地；确认不再需要后 `git branch -D backup/pre-align` 删掉。
  （reflog 里也会残留同样一批对象，但 reflog 不进推送内容。）

#### 现在怎么核对"没有回潮 / 线上仍然干净"

真实值不复述进本文件。对**工作区枚举出来的每个值**，看它的 `origin/main` 列 ——
**判据：该列必须 ≥1**（说明这个值本来就在线上、是故意夹具）；
**若为 0 而工作区有，就是新漏进来的真泄漏，立刻改掉**：

```bash
for v in $(grep -rhoE '192\.168\.[0-9]+\.[0-9]+|10\.[0-9]+\.[0-9]+\.[0-9]+|172\.(1[6-9]|2[0-9]|3[01])\.[0-9]+\.[0-9]+|/home/[a-zA-Z0-9_]+/|enx[0-9a-f]{12}' \
             --exclude-dir=.git --exclude-dir=graphify-out --exclude-dir=.codebuddy --exclude=AGENTS.md . | sort -u); do
  printf '%-24s 工作区有值  origin/main 命中 %s 提交  旧本地历史命中 %s 提交\n' "$v" \
    "$(git log origin/main    -S"$v" --oneline | wc -l)" \
    "$(git log backup/pre-align -S"$v" --oneline | wc -l 2>/dev/null || echo 'n/a')"
done
```

**本仓库实测（2026-10-26，对齐后）**：

- 上面的脚本枚举出 **10 个值**，`origin/main` 列**全部 ≥1** → **PASS，没有回潮**。
  构成：5 个文档占位网卡名 + 3 个 `10.x` 私网示例 + 2 个占位 MAC；
  `/home/<user>/` 在工作区和提交历史里都是 0 命中（只在被 `.gitignore` 排除的
  `.codebuddy/` 扫描产物里出现）。
- **已清除的 9 个值**逐个复核：`git log backup/pre-align -S<值>` → **1–5 提交**，
  `git log origin/main -S<值>` → **全部 0**。即"当年本地独有的真泄漏只存在于
  废弃分支，线上从未有过"。值不复述进本文件，现取（实测恰好 9 行）：
  ```bash
  git grep -hoE '192\.168\.[0-9]+\.[0-9]+|enx[0-9a-f]{12}' backup/pre-align \
      -- ':(exclude)AGENTS.md' | sort -u | while read -r v; do
    [ "$(git log origin/main -S"$v" --oneline | wc -l)" = 0 ] && echo "$v ← 旧历史独有"
  done
  ```
- 分类与替代值见 §6.2：4 个私网 IPv4（网关 A、热点上游 B、`bind_ip` 示例、
  第二网关）+ 5 个 MAC 派生网卡名，对齐前共 **45 处 / 8 个文件**，
  最集中的是 `internal/discover/discover_test.go`（11 处）。

### 6.2 泄漏对照表（写新示例时照这张表填）

真实值不复述，只记「哪一类 → 换成哪个脱敏值 → 曾经出现在哪」。
**对齐后这些位置都已是脱敏值**，本表的用途是**防止改回去**（尤其是改 README /
config.example 时"顺手"填上手边真实网卡名）。

| 泄漏类型（真实值类别，禁止写入） | ✅ 应当使用（脱敏） | 曾经出现处（对齐后已换掉） |
|---|---|---|
| 网关 / 上游代理的私网 IPv4（A） | `198.51.100.10` | README×2、SKILL、config.example、spec、plan、`config_test.go`、`discover_test.go` |
| 热点上游的私网 IPv4（B） | `198.51.100.20` | README×2、SKILL、config.example、`discover_test.go` |
| `bind_ip` 示例的私网 IPv4 | `198.51.100.100` | `config.example.yaml`、`config_test.go` |
| 第二网关的私网 IPv4 | `198.51.100.67` | `discover_test.go` |
| MAC 派生的 USB 网卡名（`enx`+12 hex） | `enx020000000001` / `…003` | README×2、SKILL、config.example、spec |
| 本机有线网卡名 | `enx020000000002` | README×2、config.example |
| `/proc/net/route` 夹具里的真实网卡名 + 网关 | `enx020000000004` / `…005` + `0A6433C6` | `discover_test.go` |

> **夹具是耦合的**：`discover_test.go` 的 `parseProcRoute` 断言里，网关的
> **点分十进制**和它在 `/proc/net/route` 里的**小端十六进制**必须一起换，
> 否则测试红。脱敏后的文档网关 `198.51.100.10` 对应 `0A6433C6`；
> 真实值要现算就用 `python3` 从 IP 转，别把十六进制抄进文档（同样算泄漏）。

### 6.3 推送纪律

- **每次提交前先跑 §4.1 的②③两道筛查**（AI 署名 + 四类隐私）。
  本仓库长出第二条历史、真实标识被写进提交，根因就是当初缺这道闸 ——
  值一旦进了提交对象，清除它只能靠改写历史，而"改史"正是分支保护要禁止的事。
  闸门必须设在 commit 时，线上（GitHub secret push protection）只是兜底。
- **不要把本地历史强推到线上覆盖** —— 本仓库的旧平行历史里带着真实标识，
  `push --force` 会把它们写回公开历史。这条**永远禁止**，没有例外。
- 要同步，方向**只能是"本地对齐线上"**（`git fetch` 后以 `origin/main` 为准重建本地，
  再把要保留的提交 cherry-pick 回来；2026-10-26 已按此执行过一次，命令见 §6.1）。
  **仍属需人明确决策的动作，agent 不要自行执行** —— 一旦发现 `main` 与 `origin/main`
  又不同源了（`git merge-base` 失败），先停下来报告，别自己 reset。
- 本仓库是 PUBLIC。分支保护已开启（`enforce_admins=true`，必需检查见 §6.4），但它**只挡
  "检查没绿"**，挡不住"真实值已经写进 diff" —— 推送前的自我筛查仍不可省。
- 推送前最后一遍：`git rev-list --left-right --count origin/main...main` 的
  **左列必须是 0**（不落后），右列只放你打算开 PR 的那几笔。

### 6.4 已知开源就绪缺口（按 OSS 规则集审计，实测 2026-10-28）

| 检查项 | 现状 | 依据规则 |
|---|---|---|
| 分支保护 | ✅ 已开启（2026-10-28 实测）：必需检查 `lint` + 三个 `test (<os>)`（`strict: false`）、PR 必须走但 0 个批准即可（`dismiss_stale_reviews: true`）、`required_conversation_resolution: true`；无 ruleset（`GET .../rulesets` → `[]`）。检查没跑完时合并会被明确拒绝（`base branch policy prohibits the merge`） | `branch-protection-rules.md` |
| `enforce_admins` / 禁 force-push | ✅ `enforce_admins.enabled=true`、`allow_force_pushes=false`、`allow_deletions=false` → admin 也绕不过，`main` 历史不可重写 | 同上 |
| 合并方式 | ⚠️ squash + merge + rebase **三种全开**；`squash_merge_commit_title=COMMIT_OR_PR_TITLE`、`squash_merge_commit_message=COMMIT_MESSAGES`（会吞掉 PR 正文）、`delete_branch_on_merge=false` | `pr-rules.md` |
| CI 引导 | ✅ `ci.yml` 在 push/PR 上跑，`permissions: contents: read`，check 名已知 | `repo-hygiene-rules.md` |
| `README` / `LICENSE` / `CHANGELOG` | ✅ 齐 | `repo-hygiene-rules.md` |
| `CODE_OF_CONDUCT.md` / `CONTRIBUTING.md` / `SECURITY.md` | ❌ 缺 | 同上 + `security-policy-rules.md` |
| `.github/PULL_REQUEST_TEMPLATE.md` / `CODEOWNERS` / Issue 模板 | ❌ 缺 | `pr-rules.md` / `repo-hygiene-rules.md` |
| 仓库 metadata | ⚠️ 有描述、MIT；**无 topics**、homepage 空 | `repo-hygiene-rules.md` |
| 提交信息规范 | ⚠️ 30 条里 6 条不符合 Conventional Commits（全是 0.4.0 之前的 `release:`/`vX.Y.Z:` 前缀 + 首提交；最近 10 条里仍有 1 条，即 `release: v0.4.0`），只约束新提交 | `commit-message-rules.md` |
| `CHANGELOG` 标题风格 | ⚠️ 用的是 `## 0.4.1 — 2026-10-28`，非 Keep a Changelog 的 `## [0.4.1] - 2026-10-28` | `release-rules.md`（要么保持本仓库既有风格并在此记录，要么整体迁移一次，别混用） |
| tag | ✅ 7 个 tag 全是 annotated（远端都有 `^{}` 剥离行） | `release-rules.md` |
| 线上历史脱敏 | ✅ 已完成；本地 `main` 亦已对齐线上、工作区无真实值（§6.1）：只剩占位值 + 2 个既有的占位 MAC 夹具 | 本项目红线 |
| 本地 / 线上同步 | ✅ 同源（`merge-base == origin/main == 8fed6f3`，`0 0`），工作区干净；`v0.4.1` 已发布 | `pr-rules.md` + §6.3 |

> 上表是**审计结果，不是待办授权**。改分支保护、改仓库设置、补治理文件
> 都属于"需人明确提出的独立动作"。
