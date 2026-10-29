# AGENTS.md — niclane agent 工作手册

> **权威源关系**：本文件是「在这个仓库怎么干活」的唯一入口，也是 OSS 规则集要求的
> **项目 overlay**（`repo-hygiene-rules.md` 对多 agent 仓库为必需项）。
> 通用规则权威源在 `~/develop/opensource/`（下称 **OSS 规则集**，
> **10 份** `*-rules.md` + `README.md` 索引，2026-10-29 对齐），
> 本文件只做**索引**和**本项目裁剪**：共享规则说*必须满足什么*，本文件说*在这里怎么验*。
> **overlay 只能收紧共享规则，不能放宽**；需要放宽时改的是规则本体，不是本文件。
> 本文件自身要能在公开仓库里提交 —— 它按 OSS Commit Rules ③ 做过隐私筛查
> （四类 + 本仓库追加的网卡名枚举），
> 因此**不复述任何真实网卡名 / 内网 IP / 家目录路径**，需要现场枚举时用 §6.1 的命令。
> 两者冲突时按 OSS 规则集 `README.md` 的裁决顺序：branch protection 与 security
> 这类硬约束 > 流程建议；规则里的 *Done when* 检查 > 周边散文。
> 改规则本体（OSS 规则集）是独立的、需人明确提出的动作，不在本仓库任务内。

---

## 0. 先读：四个最容易踩的坑

1. **真实网卡名 / 内网 IP 是本仓库的泄漏红线。**
   线上（`origin/main`）的历史已被整体脱敏（RFC 5737 文档 IP + 通用网卡名），
   本地 `main` 也已于 2026-10-26 对齐线上，工作区**已无真实值**（见 §6）。
   **任何文档、示例、测试夹具里只允许出现脱敏值** —— 每次提交前跑 §4.1 的
   五道筛查（OSS 四类 + 本机网卡名枚举），那是把真实值挡在历史之外的唯一闸门。
   写新示例前先看 §6.2 的对照表。
2. **`fail-closed` 语义不可破坏。** 车道的网卡挂了就必须拒绝连接，
   绝不静默改道。README 明文要求："keep the fail-closed semantics intact in
   any change that touches dialing"。碰 `internal/lane`、`internal/selector`
   的拨号路径时，这条优先于任何便利性改动。
3. **改一处能力 = 同步 5 个文件。**
   `README.md`（英文）、`README.zh-CN.md`（中文，逐段对应）、
   `config.example.yaml`、`skills/niclane/SKILL.md`（会被 goreleaser 打进每个
   release 压缩包）、`CHANGELOG.md`。少一个，发布出去的文档就是自相矛盾的。
4. **修 bug 不等于"让现象消失"。** OSS `bugfix-rules.md` 是本仓库的硬约束：
   复现 → 定位到 `file:line + 机制` → 找同类实例 → 在拥有不变量的那层修 →
   回归测试**在旧代码上必须红** → 复验爆炸半径。做不到根因时，提交信息与 PR
   只能写 **mitigation**，不许写 fix（§4.3）。

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
| 当前版本 | 0.4.1（`CHANGELOG.md` 顶部条目即最新版本，无 standing `[Unreleased]`）——**以文件为准，别信这行** |

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
  撰写时（2026-10-29，`fe71b67`）：**622 节点 / 1699 边 / 24 社区**，
  `built_at_commit` == `HEAD`，无垃圾节点。
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

| 规则文件 | 领域 | 什么时候用 | 本仓库落点 |
|---|---|---|---|
| `branch-protection-rules.md` | 分支保护 | 任何开源仓库：创建 / 加固 / 定期审计 | §4.1 / §6.4（**真人专属动作**） |
| `repo-hygiene-rules.md` | 必备文件 + 仓库设置 | 建仓、公开就绪审计 | §6.4 缺口表 |
| `pr-rules.md` | PR 流程 | 开 / 评 / 合 PR | §4.1、§4.3 |
| `multi-agent-rules.md` | 并发 agent 协作 | 两个及以上 agent 可能同时写 / 跑有状态命令 / 测同一仓库 | §4.2（**本仓库目前不触发**） |
| `commit-message-rules.md`（已改名 **Commit Rules**） | 提交格式 **+ AI 署名禁令 + 提交前隐私筛查** | 写或审 commit、生成发布说明、**以及每次提交前** | §4.1 ②③ |
| `issue-triage-rules.md` | Issue 分诊 | 有 issue 开着或待分诊 | §6.4（标签/模板尚缺） |
| `bugfix-rules.md` | 根因修复 | 有 bug 被报、复现、或正在修 | §4.3 |
| `release-rules.md` | 版本与发布 | 发版 / 判断是否该发版 | §4.4 |
| `security-policy-rules.md` | 漏洞处理 | 仓库转公开、收到第一份报告 | §6.4 |
| `graphify-rules.md` | 代码知识图谱 | 在有图的仓库里干活 | **已本地化到 §3** |

**通用约定**（照抄自 OSS `README.md`，本仓库同样适用）：

- 每一步都带 *Done when*，**对着真实仓库状态验过才算完成**，命令敲了不算。
- 不绕过分支保护，不重写 `main` 历史。
- 两份文档打架时，更硬的那条赢（branch protection / security > 流程建议）。
- 有 `graphify-out/` 的仓库，结构问题先查图（§3）。
- 发布自动化产出的 notes 只是草稿，不是第二份 changelog（§4.4）。

### 4.1 本仓库的落地要点

- **提交信息**：Conventional Commits —— `<type>(<scope>): <subject>`，
  type 只能取 `feat|fix|docs|style|refactor|perf|test|build|ci|chore|revert`，
  subject 祈使句、无句号、≤72 字符；body 每行 72 列、只解释 *why*。
  ```bash
  git log --format='%s' -10 | grep -vE '^(feat|fix|docs|style|refactor|perf|test|build|ci|chore|revert)(\(.+\))?!?: .+'
  # 期望：空输出
  # 实测（2026-10-29，31 条）：最近十条有 1 条不合规（release: v0.4.0 …），
  #       全量 6 条（5 条 release:/v0.x 前缀 + 1 条首提交）—— 只约束新提交，不改史
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
- **③ 提交前隐私筛查**（OSS Commit Rules 第 3 步，**闸门设在 commit 时** ——
  推出去之后再删就等于改写 `main` 历史，是被禁止的；GitHub secret push protection
  只是兜底不是闸门）。OSS 的四类 + 本仓库追加的第 [5] 类，全部对 `git diff --cached` 查：
  ```bash
  # [1] 凭据前缀（2026-10-29 起与 OSS 完全一致，含 github_pat_/hf_/glpat-/npm_/pypi-/SG./shpat_）
  git diff --cached | grep -inE 'sk-[A-Za-z0-9_-]{20,}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|hf_[A-Za-z0-9]{20,}|glpat-[A-Za-z0-9_-]{15,}|npm_[A-Za-z0-9]{30,}|pypi-[A-Za-z0-9_-]{40,}|SG\.[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{16,}|shpat_[a-f0-9]{32,}|AKIA[0-9A-Z]{16}|xox[baprs]-|AIza[0-9A-Za-z_-]{30,}'
  # [2] 泛凭据赋值（api_key / secret / token / password / authorization = 长值）
  git diff --cached | grep -inE '(api[_-]?key|secret|token|passwd|password|authorization)["'"'"']?[[:space:]]*[:=][[:space:]]*["'"'"']?[A-Za-z0-9+/_-]{16,}'
  # [3] 个人信息（邮箱、电话）—— noreply@ 和 example.com 这类夹具是允许的
  git diff --cached | grep -inE '[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+\.[A-Za-z]{2,}'
  # [4] 实机环境：家目录 / 主机名 / 私网 IP —— 本仓库的主雷区，见 §6
  git diff --cached | grep -inE '/home/[a-zA-Z0-9_]+/|/Users/[a-zA-Z0-9_]+/|192\.168\.[0-9]+\.[0-9]+|[^0-9]10\.[0-9]+\.[0-9]+\.[0-9]+|172\.(1[6-9]|2[0-9]|3[01])\.[0-9]+\.[0-9]+'
  # [5] 本机真实网卡名（本仓库追加的一类，见下"为什么 [4] 不够"）
  for n in $(ls /sys/class/net | grep -vE '^(lo|docker0|veth|br-|tailscale)'); do
    git diff --cached | grep -nw "$n" && echo "^^ 泄漏：$n 是本机网卡名"
  done
  ```
  *Done when*：五条全部无命中，或者每一处命中你都能说出它为什么是**故意的非敏感
  夹具**（测试 token、`example.com`）并留着。**默认结论是改掉，不是解释掉。**
  本仓库的第 [4] 条曾拦下本文件自己（家目录路径 + §6 复述的真实网卡名/IP），
  正是这道闸该有的表现。
  > **为什么 [4] 不够，要加 [5]**：[4] 只能抓"形状可预测"的名字（`enx`+12hex
  > 之类）。而真实网卡名**没有可预测形状** —— TUN 设备名、`ip link set name`
  > 改过的名、甚至 WiFi 的 `wlp*` 名，都长得像厂商标准命名，grep 无从判断。
  > 唯一可靠的判据是 **「本机 `ls /sys/class/net` 里有没有同名设备」**。
  > 本仓库实测（2026-10-29）：两个本机真实名长期躺在 `README*.md` /
  > `config.example.yaml` / `docs/coexistence.md` / `discover_test.go` 里，
  > 四类 grep 一条都没报 —— 一个是 TUN 设备名（本文件不复述，就是本机代理软件的
  > TUN 设备名），一个是 WiFi 网卡名（`wlp<数字>s<数字>` 形状），
  > **这条 grep 之前根本没覆盖到无线网卡**。
  > 处理：WiFi 那 3 处已换成 `wlan0`；TUN 那个保留（它是文档要说明的第三方
  > TUN 通用叫法，不含宿主身份信息，且改名会让示例与真实配置脱节）。
  > **这类"留还是改"的判断必须写进 PR 描述**，别让下一个人重新纠结。
  > 排查边界（OSS 原文）：筛查只看文本 —— 二进制暂存文件只打印 `Binary files …
  > differ`，`Authorization: Bearer <key>` 这类多词值会绕过 [1][2]。这两类靠
  > GitHub push protection 兜底（本仓库已开，见 §6.4），不要以为 grep 全绿就安全。
- **PR 标题 = `main` 上的提交主题**（若采用 squash 合并），因此 PR 标题同样要符合上面的格式。
  CI 侧强制是 OSS 的 *Recommended* 项，本仓库尚未做（见 §6.4）。
- **分支保护的 `required_status_checks.contexts`** 本仓库的真实 check 名是
  `lint`、`test (ubuntu-latest)`、`test (macos-latest)`、`test (windows-latest)`
  （用 `gh api --paginate repos/CallMeHFK/niclane/commits/main/check-runs --jq '.check_runs[].name' | sort -u`
  复核；`--paginate` 不能省）。**改分支保护是真人专属动作**，agent 只报告不执行。

### 4.2 多 agent 协作（`multi-agent-rules.md`）

**当前不触发**：本仓库同一时间只有一个 agent 在写（2026-10-29 实测
`git worktree list` 只有一个工作区、无 stash、无并行 claim）。只读的检查与评审
不触发本条；单个 agent 走 §2 的常规流程即可。

**一旦有两个及以上 agent 可能同时写 / 跑有状态命令 / 测同一仓库，本节即为
overlay，字段直接照抄下表**（`multi-agent-rules.md` 要求这些值写在 `AGENTS.md`）：

| 字段 | 本仓库的值 |
|---|---|
| `INTEGRATION_REF` | `main` |
| 分支前缀 | `feat/` `fix/` `docs/` `chore/` `test/` `refactor/` + `<task-id>-<slug>`（沿用已有 `<type>/<slug>` 形状） |
| 账本位置 | issue/PR 优先；纯本地时 `<git-common-dir>/claims/ledger.md`（`git rev-parse --path-format=absolute --git-common-dir` → 本仓库目前是仓库内 `.git`，linked worktree 共享） |
| 账本协议 | 只有 integrator 写；`mkdir` 抢占 + 临时文件 rename；UTC ISO-8601 |
| 工作区基线 | `git worktree add <path> -b <branch> origin/main`，每个 worker 一个 |
| lease TTL | 4 小时（本地小时级任务；跨主机必须改用 tracker，本地墙钟 lease 不可靠） |
| scope matcher | 仓库相对路径的 glob，`**` = 任意层；默认来源 `git ls-files` |
| 审查身份 | 所有 agent 共用同一个 GitHub 账号 ⇒ **只算 1 个 reviewer**，`require_code_owner_reviews` 必须保持 `false`，审批走"人"这条路 |

硬约束（照抄 ruleset，不在此重述理由）：

- 唯一 integrator + 具名 deputy（本仓库默认是**人**，因为所有 agent 共用一个
  forge 账号，没有第二个可计数的审批身份）。
- 一个 task 一个 branch 一个 worktree，**绝不共享可变工作树 / index / 测试库**。
- **禁止 `git stash`**（`refs/stash` 跨 worktree 共享）；WIP 用自己分支上的临时提交。
- 作者**不自审、不自合**；review 必须是 reviewer 产生的可追溯 artifact
  （GitHub review / 签名的 review note），聊天记录不算。
- 串行合并：一次只 arm 一个 PR，且 `--match-head-commit <reviewed-sha>`；
  上一个依赖必须 `MERGED` 才动下一个。
- **agent 永远不改 branch protection / ruleset / merge queue**；被挡住时记
  `Status: blocked`，向人报告缺哪个审批。
- release 与 tag 是 integrator 独占资源。

### 4.3 修 bug（`bugfix-rules.md`）——本仓库的强制流程

`symptom ≠ 工作单元`。七步的顺序不可跳：

1. **确定性复现**：把命令、输入、环境、实际输出记进 issue/PR；复现不了的是假设不是修复。
   本仓库的最小复现优先写成 `internal/<pkg>/<x>_test.go` 里的 `TestXxx`。
2. **先定根因再编辑**：答案必须是 `file:line — 机制 — 为什么这个输入会触发`，
   不是"报错了""值是 null"。用 `graphify affected` / `graphify query` 找调用面（§3）。
3. **找同类实例**：同一调用形状、同一错误处理形状、复制粘贴块 —— 一起修，或开
   同根因的 follow-up 并写明为什么缓。
4. **在拥有不变量的层修**。禁止：吞异常 / 降级错误、在调用点补空值兜底、
   加 sleep 或重试掩盖竞态、特判上报的输入、放宽类型消编译错、盲目升依赖、
   削弱或禁用测试与 lint。
5. **回归测试必须先红后绿**：在**改动前的提交**上跑同一条测试确认红（临时
   worktree：`git worktree add /tmp/niclane-prefix <fix 前的 SHA>`，跑完
   `git worktree remove`），再在修复后跑绿。
6. **复验爆炸半径**：原始复现 + 新回归测试 + §2 的全套门禁。
7. **PR 记录根因**：Why 写机制，What 写改了哪一层，How tested 点名回归测试。

> **根因不可达时**：明确标注 **mitigation**，issue 保持打开，建 follow-up 记录
> 未解的问题，PR / commit / release note 里都不许出现 "fix"。把缓解说成修复，
> 比不修更糟。
> 本仓库的历史判例：`fix(discover): read the gateway from NetworkManager's DHCP
> lease` —— 根因是 `gatewayFor` 在 `never-default=yes` 的链路上拿不到
> `IP4.GATEWAY`，修复点是补 DHCP option 3 作为**最后**来源，附 `TestParseNmcliDhcpRouters`。

### 4.4 发版（`release-rules.md`）

**先改 `CHANGELOG.md`，再打 tag。** 发布正文的来源是 changelog 条目，不是
自动生成的 commit 列表（见下"当前偏差"）。

```bash
# 1) 顶部条目必须是即将发布的版本（Keep a Changelog 标题）
#    ## [X.Y.Z] - YYYY-MM-DD  + Added / Changed / Fixed / Removed
# 2) 预检：goreleaser 的正文是不是干净（过滤器必须吃掉带 scope 的 docs/chore/test）
# 3) tag 必须是 annotated，已发布版本永不删除、永不重打
git tag -a vX.Y.Z -m "vX.Y.Z" && git push origin vX.Y.Z
# 4) 发布正文从 CHANGELOG 取（--notes-file 读整文件，所以要切段）
sed -n '/^## \[X\.Y\.Z\]/,/^## \[/p' CHANGELOG.md | sed '$d' > /tmp/release-notes.md
gh release create vX.Y.Z --verify-tag --title "vX.Y.Z" --notes-file /tmp/release-notes.md
```

- 版本号语义：PATCH 只放兼容修复，MINOR 放兼容新功能，MAJOR 是用户必须反应的变化；
  两种都说得通时取大的。
- **正文预检（不可省）**：在临时 clone 里给候选 commit 打本地 tag，跑
  `goreleaser release --clean --skip=archive,publish,announce,validate`，检查
  `dist/CHANGELOG.md`。`--snapshot` 不生成 changelog，**不能**替代这一步。
  生成器不可预检 ⇒ 不 tag，改用 `CHANGELOG.md` 直接发布。
- 过滤器的完整集合定义在 `.goreleaser.yaml`：当前排除 `docs` / `chore` / `test`，
  **带不带 scope、带不带 `!` 都要匹配**（`^(docs|chore|test)(\([^)]*\))?!?:`）。
  判例：2026-10-28 之前写的是裸 `^docs:` 三条，`docs(agents):` / `chore(release):`
  穿透了过滤器 ⇒ v0.4.1 正文混进两条杂务提交。
  纯文档 / 杂务 / 测试改动**必须**带 `docs:`/`chore:`/`test:` 前缀，否则会污染
  Release 正文；`!` 或 `BREAKING CHANGE:` 无论什么类型都留在 breaking 段。
- 校验链：commit → tag → release → changelog 四处版本一致；`gh release view
  vX.Y.Z --json body --jq .body` 与本版本 changelog 条目内容集合相同（生成式正文
  只允许格式差异，不允许条目多/少）。
- tag 是 annotated 才合规：`git ls-remote origin 'refs/tags/vX.Y.Z^{}'` 必须打印
  剥离行（只有一行、没有 `^{}` = 轻量 tag = 违规）。
- 多 agent 并发期间，"切版本 + 推 tag" 是 integrator 独占 claim。

---

## 5. 本项目硬约束（OSS 规则之外）

1. **脱敏红线**（见 §0.1 与 §6）：文档 / 示例 / 测试夹具只能用
   `198.51.100.0/24`（RFC 5737 TEST-NET-3）、`127.0.0.1`，网卡名用
   `enx020000000001` 这类占位、无线用 `wlan0`。
   RFC 1918 私网段（`192.168/16`、`10/8`、`172.16/12`）的真实 IP，由 MAC
   派生的 `enx<12位十六进制>` 网卡名，以及**本机实际存在的网卡名**（含 WiFi 的
   `wlp*` / `wlx*` 形状、自定义 TUN 名），一律不许出现在会被提交的内容里 ——
   这正是 OSS 规则③第 [4] 条 grep 要抓的东西，**提交前先跑 §4.1 的筛查**。
   判据不是"看着像占位"，而是**本机 `ls /sys/class/net` 里有没有同名设备**：
   ```bash
   for n in $(ls /sys/class/net | grep -vE '^(lo|docker0|veth|br-|tailscale)'); do
     git grep -lw "$n" -- . && echo "^^ $n 是本机网卡名"
   done
   # 期望：命中项都能说出保留理由；enx*/wlp* 一律不算可保留
   ```
   > 本仓库判例（2026-10-29）：一个 WiFi 网卡名（`wlp<数字>s<数字>` 形状）
   > 曾被当成"通用无线名"写进 `internal/discover/discover_test.go` 三处夹具，
   > 实际就是本机 WiFi 卡，且自 v0.1.0 起就在线上历史里。已改为 `wlan0`
   > （历史改写不可行，只保证此后不再新增）。**"看起来通用"不等于通用。**
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

### 6.1 当前状态：**已对齐**（实测 2026-10-29）

下表是**快照，会随新提交过期** —— 每次动 `main` 前用右列命令重测；
其中"左列必须是 `0`"是硬判据（落后于线上就不许推，见 §6.3）：

| 项 | 快照（撰写时） | 复测命令 |
|---|---|---|
| 共同祖先 | `fe71b67` = `origin/main` → **同源** | `git merge-base origin/main main` |
| 领先 / 落后 | **`0 0`** | `git rev-list --left-right --count origin/main...main` |
| 提交数 | 本地 31 / 线上 31 | `git rev-list --count main` / `… origin/main` |
| 跟踪文件 | 52 个（其中 33 个 `.go`） | `git ls-files \| wc -l` |
| 树差异 | **空**（`main` 与 `origin/main` 完全一致） | `git diff --stat origin/main main` |
| 工作区 | 快照时刻 `git status` 空 | `git status --porcelain` + 下面的枚举脚本 |

**§6.2 那 9 个真实值在对齐时随 `git reset --hard origin/main` 一起被换掉了**
（本地历史被丢弃，见下）。工作区现在只剩**占位与故意夹具**，逐值复核见本节末尾
"现在怎么核对没有回潮"。

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

**本仓库实测（2026-10-29，`fe71b67`）**：

- 上面的脚本枚举出 **10 个值**，`origin/main` 列**全部 ≥1** → **PASS，没有回潮**。
  构成：**7 个占位网卡名**（`enx02000000000N` ×5、`enx111111111111`、
  `enx1a2b3c4d5e6f`）+ **3 个 `10.x` 私网示例**（值不复述，见 §4.1 ③[4]）。
  后 4 个是一眼假的夹具（`config_test.go`、`selector_test.go`、
  `skills/niclane/SKILL.md`），按 §4.1 ③ 的 Done-when 属于"留着并能说出理由"；
  其余都是 RFC 5737 之外的文档占位。
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

- **每次提交前先跑 §4.1 的②③两道筛查**（AI 署名 + 隐私四类 + 网卡名枚举）。
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

### 6.4 已知开源就绪缺口（按 OSS 规则集审计，实测 2026-10-29）

**先看结论**：硬约束（分支保护 / secret 扫描 / 脱敏 / tag）全部达标；
缺口集中在**流程软配置**（合并方式、治理文件、依赖自动化）和**发布链的落地**
（正文取源没照 CHANGELOG 走）。下表按严重度从上到下。

| 检查项 | 现状 | 依据规则 |
|---|---|---|
| 分支保护 | ✅ `enforce_admins=true`、`allow_force_pushes=false`、`allow_deletions=false`；必需检查 `lint` + 三个 `test (<os>)`（`strict: false`）；`dismiss_stale_reviews=true`、`required_conversation_resolution=true`、`restrictions=null`、`required_approving_review_count=0`；无 ruleset（`GET .../rulesets` → `[]`）。检查没跑完时合并会被明确拒绝（`base branch policy prohibits the merge`） | `branch-protection-rules.md` |
| 密文扫描 | ✅ `secret_scanning=enabled`、`secret_scanning_push_protection=enabled` | `security-policy-rules.md` |
| tag | ✅ 7 个（v0.1.0–v0.4.1）全是 annotated，远端都有 `^{}` 剥离行 | `release-rules.md` |
| 线上历史脱敏 | ✅ 已完成；本地 `main` 亦已对齐（§6.1），工作区只剩 §6.2 的占位夹具 | 本项目红线 |
| 本地 / 线上同步 | ✅ 同源（`merge-base == origin/main == fe71b67`，`0 0`），31 笔对齐 | `pr-rules.md` + §6.3 |
| CI 引导 | ✅ `ci.yml` 在 push/PR 上跑，`permissions: contents: read`，check 名已知 | `repo-hygiene-rules.md` |
| `README` / `LICENSE` / `CHANGELOG` | ✅ 齐 | `repo-hygiene-rules.md` |
| **发布正文取源** | ❌ **`v0.4.1` 的正文是 goreleaser 生成的 commit 列表**（`## Changelog` + 4 条 `* <sha> <subject>`，含 `chore(release)` 与 `docs(agents)`），不是 `CHANGELOG.md` 的条目 —— §4.4 的 "changelog-first" 尚未真正生效。该 release 早于过滤器修复（`fe71b67`），故 docs/chore 穿透 | `release-rules.md` |
| **合并方式** | ⚠️ squash + merge + rebase **三种全开**；`squash_merge_commit_title=COMMIT_OR_PR_TITLE`、`squash_merge_commit_message=COMMIT_MESSAGES`（会吞掉 PR 正文）、`delete_branch_on_merge=false`、`allow_auto_merge=false` | `pr-rules.md` 第 2–3 步 |
| **workflow action 固定** | ⚠️ 1 个第三方 ref 未钉 SHA：`goreleaser/goreleaser-action@v6`（`actions/checkout@v4`、`actions/setup-go@v5` 为 first-party，不在本条范围内） | `security-policy-rules.md` 第 5 步 |
| **私有漏洞报告** | ❌ `private-vulnerability-reporting=false`；`dependabot_security_updates=disabled`；`secret_scanning_non_provider_patterns` / `validity_checks=disabled` | `security-policy-rules.md` |
| 治理文件 | ❌ 缺 `SECURITY.md`、`CONTRIBUTING.md`、`CODE_OF_CONDUCT.md` | `repo-hygiene-rules.md` + `security-policy-rules.md` |
| PR / Issue 模板 | ❌ 缺 `.github/PULL_REQUEST_TEMPLATE.md`、`CODEOWNERS`、`ISSUE_TEMPLATE/`；`.github/` 下只有 `workflows/` | `pr-rules.md` / `repo-hygiene-rules.md` |
| 仓库 metadata | ⚠️ 有描述、MIT；**无 topics**、`homepage=null`；`has_wiki=true` 但 `niclane.wiki.git` 不存在（无内容的空壳） | `repo-hygiene-rules.md` |
| 提交信息规范 | ⚠️ 31 条里 6 条不符合 Conventional Commits（5 条 `release:` / `vX.Y.Z:` 前缀 + 首提交；最近 10 条里仍有 1 条 `release: v0.4.0 …`）。只约束新提交，不改史 | `commit-message-rules.md` |
| `CHANGELOG` 标题风格 | ⚠️ 用的是 `## 0.4.1 — 2026-10-28`，非 Keep a Changelog 的 `## [0.4.1] - 2026-10-28` | `release-rules.md`（要么保持既有风格并在此记录，要么整体迁移一次，别混用） |

> 上表是**审计结果，不是待办授权**。改分支保护、改仓库设置、补治理文件、
> 钉 action SHA、回填 release 正文 —— 都属于"需人明确提出的独立动作"。
> agent 能做的只有本文件与代码侧的改动。
>
> 排序建议（若要开工）：先修 §4.4 的发布链（下个版本就会再污染一次），
> 再补 `SECURITY.md`（公开仓库收到第一份报告前必须有），最后才是合并方式与 metadata。
