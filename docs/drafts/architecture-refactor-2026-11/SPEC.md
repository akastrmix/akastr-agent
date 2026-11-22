# AkastrAgent 内部规整重构方案（Spec）

> **目录生命周期**：临时复核草稿；重构完成上线后删除整个 `docs/drafts/` 目录。
>
> **状态：待复核、未批准。** 本方案为纯 Agent 内部代码规整，不涉及协议或持久格式变更（不触发 Cloud ADR 0024）；文档内容不构成批准，经复核与主人确认后再执行。

## 1. 标题与背景定位

- **基准仓库**：`/root/AkastrAgent`
- **基准提交**：`a37e6ea`（紧随 `v1.30.1`）
- **运行时基线**：Go `1.27.0`（toolchain `go1.27.4`）、WSS 协议版本 `2026-11-01.v12`、节点配置 `schema_version = 5`

**核心判断：不做推倒重来的底层大重构，只做一次纯内部的收敛规整。**

1. **底层底座稳固，无需伤筋动骨**：核心框架包（`daemon`、`transport`、`operation`、`desired`、`lifecycle`、`layout`、`install`、`update`、`state`）以及 Xray 读取底座（`xray/process`、`xray/accesslog`、`xray/ledger`、`panel`、`sockdiag`、`hostroute`）边界清晰、无业务泄漏，经受住了近期连续新增 6 个功能模块的考验。
2. **凌乱感集中在上层模块与公共边角料**：近期功能迭代密集，在 4 个位置积累了结构性杂质——**跨模块直接引用、周期性最新状态上报样板重复（含边缘缺陷）、`firewall` 单包职责过度集中、装配层清理协程与周边小工具零散重复**。通过纯内部收敛即可让代码重新规整，同时保持零线上变更风险。

---

## 2. 红线与明确不改范围（Non-Goals & Invariants）

> [!IMPORTANT]
> 本次规整为 **纯 Agent 内部重构**：不修改任何线上通信协议、不修改任何本地持久化文件格式、不涉及 `/root/AkastrCloud` 联动修改，也不触发 Cloud ADR 0024 的高风险变更审批。

1. **零协议与跨仓库变更**
   - [PROTOCOL.md](../../PROTOCOL.md) 定义的全部消息类型、字段、错误码、上报 `kind`、周期、bootstrap/enrollment/maintenance 签名及 CLI 契约（`version`、`prepare`、`run`、`install`）保持完全不变。
   - 不改动共享夹具 [agent-protocol-v12.json](../../../internal/protocol/testdata/agent-protocol-v12.json)，无需修改 `/root/AkastrCloud`。
2. **零持久状态格式变更**
   - `identity.json`（v3）、`/usr/local/lib/akastr-agent/{current,slots/{a,b},.maintenance.lock}`、`state.json`（v1）、`update-attempt.json`（v1）、`changeip-reconciliation.json`（v1）、`xui/state.json`（v1）、`firewall/state.json`（v1）的文件路径与 JSON schema 完全不变，确保已部署节点原地升级平滑交接。
3. **不动核心状态机与一级风险算法**
   - `transport`、`operation`、`desired`、`install`、`update` 的状态机与锁/租约逻辑不重构。
   - [ledger.go](../../../internal/xray/ledger/ledger.go) 的 TCP/UDP 连接认领、争议置空与闭环计数算法不改动（防一级风险：避免将连接数、限速或审计流量错算到其他客户头上）。
4. **拒绝过度工程与形式主义检查**（遵守 [AGENTS.md](../../../AGENTS.md) 第 25–29 行）
   - 不新增检查源码文本或 AST/`go list` import 关系的测试；
   - 不为 `firewall` 与 `metrics` 中各十余行且字段需求不同的 `/proc/meminfo`、`boot_id` 读取强行新建 `internal/procfs` 杂物包；
   - 保留 [host.go](../../../internal/modules/firewall/host.go)（第 166–189 行）中检查 `nat_prerouting` 链是否存在的防御逻辑（既兼容早期无转发阶段的旧表，也防止外部残缺表导致 `nft list chain` 报错卡死 `apply`）。

---

## 3. 改动一：清零 `internal/modules/*` 跨模块引用（落实模块隔离规则）

### 3.1 现状证据

[AGENTS.md](../../../AGENTS.md)（第 26 行）明确要求“功能模块各自独立放在 `internal/modules/*`，模块之间互不引用（共用能力放 `internal/` 无业务底层包），只在 `internal/app/modules.go` 集中登记与接线”，但当前存在 **4 处直接跨模块导入**：

1. [ratelimit.go](../../../internal/modules/ratelimit/ratelimit.go)（第 28–65 行）导入 `internal/modules/xui`，仅用于调用 `xui.Owned(email)`。
2. [module.go](../../../internal/modules/connectionrecords/module.go)（`connectionrecords`，第 23–120 行）导入 `internal/modules/xui`，仅用于调用 `xui.Owned(line.Email)`。
3. [routing.go](../../../internal/modules/routing/routing.go)（第 23–158 行）导入 `internal/modules/xui`，仅用于调用 `xui.Owned(user)`。
4. [handler.go](../../../internal/modules/changeip/handler.go)（第 8–68 行）导入 `internal/modules/ipwatch`，仅使用了 `ipwatch.AddressObserver` 与 `ipwatch.IPv4`（而同文件第 19 行对 `Monitor` 已采用消费方窄接口 `changeReconciler` 解耦）。

### 3.2 目标设计

1. **下沉 Xray 受管账号标识判定**
   - 新建 `internal/xray/user.go`（`package xray`），定义 `OwnedUser(email string) bool`（正则 `^ak-[a-z0-9]{1,61}$`）。
   - `xui`、`ratelimit`、`connectionrecords`、`routing` 四处统一调用 `xray.OwnedUser`，移除 `xui.Owned`。
   - **细节约束**：保留 `xui` 内部在 [apply.go](../../../internal/modules/xui/apply.go)（第 277 行）与 [snapshot.go](../../../internal/modules/xui/snapshot.go)（第 177 行）中对面板存量客户端使用的 `strings.HasPrefix(..., ownedPrefix)`（`"ak-"`）前缀判定，确保面板中历史遗留或非规范命名的 `ak-` 客户端仍能被识别清理或统计。
2. **解耦 `changeip` 与 `ipwatch`**
   - 在 [handler.go](../../../internal/modules/changeip/handler.go) 定义消费方函数类型 `type ObserveIPv4 func(context.Context) (netip.Addr, time.Time, error)`（或单方法窄接口），删除对 `internal/modules/ipwatch` 的导入。
   - 在唯一装配入口 [modules.go](../../../internal/app/modules.go)（第 255 行）中用闭包适配 `observer.Observe(ctx, ipwatch.IPv4)` 并传给 `changeip.New(...)`。

### 3.3 验收标准

`internal/modules/*` 下 11 个功能模块目录之间零互相导入（仅保留模块自有子包 `changeip/{command,httpcurl}` 与 `ipqualityrunner/script` 的内部引用）。

---

## 4. 改动二：收敛周期性“最新状态上报器”与可选 `UpdateBlocker`

### 4.1 现状证据

在全部 6 个 `module.Reporter` 实现中，`ipwatch`（维护 `changeip-reconciliation.json` 持久换 IP 核对状态）和 `connectionrecords`（有界内存队列分批上报）各有独立状态语义，保持独立；其余 4 个均为“只保留最新一份、不排队”的周期性上报器，各自手写了一套重复的 `wake`、`mu`、`pending`、`ControlReady`、`Acknowledge`、`UpdateSafe` 样板代码：

| 上报器 | 代码位置 | 周期 | `ControlReady` 行为 |
|---|---|---|---|
| `xui.snapshot` | [snapshot.go](../../../internal/modules/xui/snapshot.go)（第 31–123 行） | 1 分钟 | 立即采样并发送 |
| `firewall.status` | [status.go](../../../internal/modules/firewall/status.go)（第 86–151 行） | 1 分钟 | 立即采样并发送 |
| `metrics.sample` | [metrics.go](../../../internal/modules/metrics/metrics.go)（第 38–97 行） | 1 分钟 | 立即采样并发送 |
| `routing.outbound_health` | [health.go](../../../internal/modules/routing/health.go)（第 38–168 行） | 5 分钟 | **不重新测试**，只原样重发未确认报告 |

同时存在两处现存瑕疵：

1. **采样失败时漏发未确认的 `pending` 报告**：在 [snapshot.go](../../../internal/modules/xui/snapshot.go)（第 109–112 行）与 [metrics.go](../../../internal/modules/metrics/metrics.go)（第 74–80 行）中，如果上一份报告尚未收到 `report.ack`，而本次定时或重连触发的采样临时失败（例如 3x-ui 正在重启），`tick()` 直接提前返回，导致原本待确认的 `pending` 报告在本次重连或周期中漏发（`metrics` 甚至只存了 `pending string` 的 `ReportID` 而未保存报告体，导致读取失败时根本无法重发）。
2. **强制的空 `UpdateSafe()` 样板**：[module.go](../../../internal/module/module.go)（第 65 行）将 `UpdateSafe() error` 放在 `Reporter` 必需接口中，导致 6 个实现里有 5 个写死 `return nil`，仅 [module.go](../../../internal/modules/ipwatch/module.go)（`ipwatch`，第 69 行）真正使用。

### 4.2 目标设计

1. **拆出可选接口 `module.UpdateBlocker`**
   - 在 [module.go](../../../internal/module/module.go) 将 `UpdateSafe() error` 从 `Reporter` 移出，定义为可选接口（与同文件第 76 行的 `Starter` 模式一致）：
     ```go
     type UpdateBlocker interface {
         UpdateSafe() error
     }
     ```
   - 在 [runtime.go](../../../internal/app/runtime.go)（第 167–173 行）中通过类型断言 `if blocker, ok := reporter.(module.UpdateBlocker); ok` 调用；保留 `ipwatch.Reporter.UpdateSafe()`，删除其余模块中写死 `return nil` 的空方法。

2. **新增 `internal/module/latest.go`（`LatestReporter`，约 80 行）**
   - 构造参数：`kind string`、`interval time.Duration`、`sampleOnReady bool`、`sample func(context.Context) ([]byte, error)`。
   - 采样、去重与重发规则（严格对齐 [PROTOCOL.md](../../PROTOCOL.md) 第 280–329 行）：
     - **定时周期到期（含启动首次）**：调用 `sample(ctx)`：
       - 若成功：当 `pending == nil`（上一份已确认——哪怕本次字节完全未变也生成新 `ReportID`，满足 `xui.snapshot`“内容没变也发”的要求）或 `!bytes.Equal(pendData, data)`（上一份未确认但内容已变化，取代旧报告）时，生成新 `protocol.ReportBody{ReportID: protocol.NewUUID(), Kind: kind, Data: json.RawMessage(data)}`；否则保留原 `pending` 与 `ReportID`。
       - 若失败：保留原 `pending` 不清空。
       - 最后只要 `pending != nil` 即调用 `publish(*pending)`（修复采样临时失败时漏发未确认旧报告的问题）。
     - **`ControlReady()` 唤醒时**：
       - `sampleOnReady == true`（`xui.snapshot`、`firewall.status`、`metrics.sample`）：立即执行上述采样与发送流程（不重置周期 `Ticker`）；
       - `sampleOnReady == false`（`routing.outbound_health`）：不调用 `sample`，仅在 `pending != nil` 时原样调用 `publish(*pending)`（严格遵守“同一轮在连接 ready 时原样重发”，避免重连抖动时额外拉起临时 Xray 测试）。

---

## 5. 改动三：`internal/modules/firewall` 包内职责拆解与宿主接口统一

### 5.1 现状证据

- `internal/modules/firewall` 目前有 8 个非测试文件，合计 2587 行，其中 [firewall.go](../../../internal/modules/firewall/firewall.go) 单文件达 623 行，混杂了三套正交逻辑：
  1. ADR 0109 入站筛选探测、候选落盘、20 秒确认与失败回退状态机；
  2. ADR 0110 端口转发同步、按 `boot_id` 开关 `net.ipv4.ip_forward`、`Forwards`/`Stale` 状态流转与 netlink 旧连接清理；
  3. 每 2 分钟域名解析与集合成员热刷新。
- [status.go](../../../internal/modules/firewall/status.go)（第 102 行）中的 `NewStatus` 绕开 `Firewall.host` 接口，私自调用 `newSystem("")` 创建第二个 `*system` 实例，并在第 171 行直接调用包级函数 `toolMissing()` 与 `nftablesLoaded()`，导致宿主访问边界分裂，且 `Status.read` 无法基于假宿主做纯内存单测。

### 5.2 目标设计（纯包内拆解，不拆子包）

1. **拆出 `forwarding.go`**：将 [firewall.go](../../../internal/modules/firewall/firewall.go)（第 256–360 行）中的转发与旧连接清理逻辑（`placement`、`openForwarding`、`closeForwarding`、`placed`、`clean` 及启动时的 `ForwardingOpened` 跨重启核对）提取到同包 `forwarding.go`。
2. **拆出 `domains.go`**：将 [firewall.go](../../../internal/modules/firewall/firewall.go)（第 527–623 行）中的域名解析与定时热刷逻辑（`refresh`、`resolveMissing`、`resolveOne`、`lookup`、`domains`）提取到同包 `domains.go`。`firewall.go` 精简至约 380 行，只保留模块入口、`start`、`apply`、`confirm`、`restore`、`raiseConntrack`。
3. **统一宿主接口**：扩展包内宿主接口（或在 `status.go` 定义只读 `statusHost` 接口并由 `*system` 实现），让 `Firewall` 与 `Status` 共用同一个宿主实例（或测试桩），移除 `NewStatus` 内隐藏的第二个 `newSystem("")` 以及 `status.go` 对全局 `toolMissing()`、`nftablesLoaded()` 的直接调用。

### 5.3 必须死守的 4 条不变量

1. `/var/lib/akastr-agent/firewall/state.json` 字段与 `schema_version = 1` 零变化；
2. `inet akastr_firewall` 规则渲染与 `akastr:<mode>:<digest>` 摘要计算零变化；
3. `apply()` 执行顺序零变化：表已存在且带合法标记时，仍须**先执行 `syncForwarding()` 与 `placed()`**，再比较规则摘要与失败摘要（保证筛选规则失败锁定时，端口转发仍能跟随落地机变化）；
4. `Started()` 阻塞控制连接建立的启动顺序零变化。

---

## 6. 改动四：装配层一次性清理器、公共小工具去重与边角加固

### 6.1 一次性残留清理不再伪装成常驻协程

- **现状**：[runtime.go](../../../internal/app/runtime.go)（第 89–126 行）规定任一 `Worker.Run` 提前返回都会终止整个 Agent 进程，因此 [ratelimit.go](../../../internal/modules/ratelimit/ratelimit.go)（`Leftovers`，第 256–275 行）、[leftovers.go](../../../internal/modules/firewall/leftovers.go)（`firewall.Leftovers`，第 25–41 行）和 [module.go](../../../internal/modules/diagnostics/module.go)（`diagnostics.Sweeper`，第 292–311 行）三处各自手写了一套“失败按周期重试，成功后 `<-ctx.Done()` 挂起占住协程直到进程退出”的样板。
- **目标设计**：在 [runtime.go](../../../internal/app/runtime.go) 增加一次性清理任务机制（例如 `addCleanup(retry time.Duration, fn func(context.Context) error)`），在 `Runtime.Run` 中用独立 `sync.WaitGroup` 运行：返回非 nil 错误时按 `retry` 间隔等待重试，返回 `nil` 后直接正常结束协程，既不触发进程退出，也不白占空挂协程；`diagnostics.Sweeper` 在清理函数成功返回 `nil` 时 `close(done)` 放行脚本执行。

### 6.2 新建无业务底层小包 `internal/subproc`（约 55 行）消除重复子进程工具

1. 合并三处逻辑完全相同的有界头截断 `io.Writer`：
   - [module.go](../../../internal/modules/diagnostics/module.go)（`capped`，第 315–331 行）
   - [provider.go](../../../internal/modules/changeip/httpcurl/provider.go)（`headBuffer`，第 146–163 行）
   - [provider.go](../../../internal/modules/ipqualityrunner/script/provider.go)（`limitedBuffer`，第 253–272 行）
   统一收口为 `subproc.HeadBuffer`（支持 `Bytes()` / `Text()` / `Truncated()`）。
2. 合并两份逐字相同的进程组配置与终止文件：
   - [process_unix.go](../../../internal/modules/changeip/command/process_unix.go)
   - [process_unix.go](../../../internal/modules/ipqualityrunner/script/process_unix.go)
   统一为 `subproc.SetProcessGroup(*exec.Cmd)` 与 `subproc.KillProcessGroup(*exec.Cmd)`，删除两处重复文件。

### 6.3 收口 `internal/protocol` 重复校验与地址派生

1. 合并 [module.go](../../../internal/modules/socks5/module.go)（`socks5`，第 39 行）与 [module.go](../../../internal/modules/ipqualityrunner/module.go)（`ipqualityrunner`，第 47 行）完全相同的 `validLogin` 为 `protocol.ValidSOCKS5Login`。
2. 在 [config.go](../../../internal/config/config.go)（第 24 行）与 [identity.go](../../../internal/identity/identity.go)（第 29 行）中直接复用 [protocol.go](../../../internal/protocol/protocol.go)（第 188–190 行）的 `protocol.ValidUUID`，删除重复的 `canonicalUUID` 正则定义。
3. 在 [protocol.go](../../../internal/protocol/protocol.go) 新增 `ValidateControlEndpoint(raw string) (*url.URL, error)` 与 `ControlHTTPSURL(controlEndpoint, path string) (string, error)`，收口以下 4 处重复的 `wss://<host>/internal/agents/ws` 校验及 HTTPS 路径派生逻辑：
   - [config.go](../../../internal/config/config.go)（第 78–82 行）
   - [client.go](../../../internal/transport/client.go)（`transport`，第 108–113 行）
   - [identity.go](../../../internal/identity/identity.go)（第 138–143 行）
   - [client.go](../../../internal/update/client.go)（`update`，第 114–119 行）

### 6.4 加固卸载脚本 `install.sh`

- **现状**：[install.sh](../../../scripts/install.sh)（第 78 行）在卸载清理 IPv4 转发开关时，依赖固定两空格行首缩进的正则 `sed -n 's/^  "forwarding_opened": "\([^"]*\)".*/\1/p'` 读取 `/var/lib/akastr-agent/firewall/state.json`，若 JSON 排版变化就会静默跳过关闭转发。
- **目标**：改为不依赖缩进与换行格式的 POSIX `sh`/`sed` 匹配，同时保持零外部工具依赖（兼容未装 `jq` 的精简节点或二进制已损坏的卸载场景）。

### 6.5 补齐 `app_test.go` 的模块覆盖

- 在 [app_test.go](../../../internal/app/app_test.go)（第 140–149 行）的 `decodeCloudMessage` 中，`state.put` 分支目前写死仅识别 `xui`，补齐按 `put.Module` 分发至 `xui`、`rate_limit`、`routing`、`firewall` 四个 `Desired` 模块的 `Validate` 检查；
- 在 [app_test.go](../../../internal/app/app_test.go)（第 34–53 行）的 `TestModuleConfigurationFailsClosed` 补齐 `rate_limit without xui` 与 `routing without xui` 两个依赖校验用例。

### 6.6 按删除条件清理协议 v8 前遗留代码

- [modules.go](../../../internal/app/modules.go)（第 145–150 行）中删除 `ip-state.json` 的兼容代码已明确写明删除条件：“`Delete these lines once no node runs such an Agent.`”。
- **前置核验**：执行本次重构前，经 `AkastrCloud` 的正式只读入口 `npm run prod:sql` 核查线上节点版本；若确认所有节点已高于协议 v8，则一并删除这几行（若仍存在旧版待删/离线节点则保留并记录原因）。

---

## 7. 文档同步、分步执行顺序与验证计划

### 7.1 文档同步（最小变更）

- 仅在 [README.md](../../../README.md)（第 31–43 行）的代码结构树与 [ARCHITECTURE.md](../../ARCHITECTURE.md)（第 39–70 行）第 4 节“包职责”对 `internal/subproc`、`module.UpdateBlocker`/`LatestReporter` 及清理任务机制做几行现行描述同步。
- [PROTOCOL.md](../../PROTOCOL.md) 与 [INSTALLATION.md](../../INSTALLATION.md) 不改动。

### 7.2 建议分步执行顺序（4 个原子阶段）

1. **阶段一：底层原语收口与模块解耦**
   - 新增 `internal/xray/user.go`（`OwnedUser`）、`internal/subproc`（`HeadBuffer`、`SetProcessGroup`、`KillProcessGroup`），收口 `internal/protocol` 公共校验与控制面地址派生；
   - 切断 4 处 `internal/modules/*` 跨模块引用；
   - 补齐 [app_test.go](../../../internal/app/app_test.go) 的模块校验覆盖。
2. **阶段二：周期性上报器与一次性清理器收敛**
   - 抽取 `module.UpdateBlocker` 与 `module.LatestReporter`，迁移 `xui.Snapshots`、`firewall.Status`、`routing.Health`、`metrics.Sampler`；
   - 在 [runtime.go](../../../internal/app/runtime.go) 引入一次性清理任务执行器，迁移 `ratelimit.Leftovers`、`firewall.Leftovers` 与 `diagnostics.Sweeper`。
3. **阶段三：`firewall` 包内拆解与周边收敛**
   - 将 [firewall.go](../../../internal/modules/firewall/firewall.go) 拆出 `forwarding.go` 与 `domains.go`，统一 `Status` 与 `Firewall` 的宿主接口；
   - 加固 [install.sh](../../../scripts/install.sh)（第 78 行）卸载提取；
   - 经 `AkastrCloud` `npm run prod:sql` 确认线上无 v8 前旧节点后删除 `ip-state.json` 兼容清理；
   - 最小同步 [README.md](../../../README.md) 与 [ARCHITECTURE.md](../../ARCHITECTURE.md)。
4. **阶段四：低并发全量验证**
   - 严格遵守 2 核 2 GB 开发机约束（`GOMAXPROCS=1`、`GOMEMLIMIT=256MiB`），不与 Cloud 重型测试并行，串行执行：
     - `./scripts/verify-go.sh`
     - `sh -n scripts/*.sh`
     - `./scripts/verify-ipquality-source.sh`

---

## 8. 附：供 Claude 复核的 5 个关键审查点

1. **上报器语义等价性**：`LatestReporter` 的两种 `ControlReady` 模式（`sampleOnReady=true` 用于 `xui.snapshot`/`firewall.status`/`metrics.sample`；`sampleOnReady=false` 用于 `routing.outbound_health`），以及“已确认后哪怕内容未变也生成新 `ReportID`、未确认时仅内容变化才替换并换 ID、采样失败保留并继续重发 `pending`”的规则，是否与 [PROTOCOL.md](../../PROTOCOL.md)（第 280–329 行）的四项上报契约完全一致？
2. **`metrics.Sampler` 在重连时的采样策略**：当前 [PROTOCOL.md](../../PROTOCOL.md)（第 316–320 行）明确写着 `metrics.sample`“连接 ready 后立即发一份”且 `cpu` 为“本进程相邻两次读 `/proc/stat` 第一行之间的忙碌占比”。维持现行“重连即采样”（仅修复读取失败时重发未确认报告）是否最为稳妥，还是有调整采样语义或修改协议的必要？
3. **`firewall` 包内拆解与状态机顺序**：拆出 `forwarding.go`、`domains.go` 并统一 `Status` 的宿主接口时，是否有任何细节可能影响 ADR 0109/0110 的执行顺序不变量（例如转发成员先于摘要检查同步）、`f.mu` 锁粒度，或 `state.json` 的崩溃恢复原子性？
4. **一次性清理协程的退出同步**：`Runtime` 改用独立清理任务（成功返回 `nil` 即退出 goroutine）后，与 `diagnostics.Sweeper` 的 `done` 同步通道配合，在进程快速取消退出或反复重试场景下是否存在竞态或 `close of closed channel` 风险？
5. **底层小包收敛边界**：`internal/subproc` 与 `internal/protocol` 的收口粒度是否恰当？是否有隐含的多余抽象（例如合并 `SetProcessGroup`/`KillProcessGroup` 时是否需要考虑跨平台编译约束），或还有其他确属高价值的重复点被遗漏？
