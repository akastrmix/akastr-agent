# SPEC（1557576）复核意见

> **复核对象**：同目录 [SPEC.md](SPEC.md)（提交 `1557576`）
>
> **说明**：本文仅记录基于 `AkastrAgent` 与 `AkastrCloud` 现行代码的核实结论与实施提醒，供主人决定下一步；本文任何内容均不构成对实施或变更的授权。

## 总体结论

改写后的 R1–R5（及可选 R6）边界干净，特别是新抽出的 **R3（模块按自身配置组装自己、收回散落在 `internal/app/modules.go` 的单模块细节）** 比初稿抓得更准；第 4 节“不采纳清单”逐项核对代码后**全部成立，没有需要恢复的条目**（其中排除“采样失败重发旧报告”和排除“出站检测并入共用上报器”两项，避免了两处真实的线上退化）。

对 [SPEC.md](SPEC.md) 整体无阻塞异议；下文逐题回答第 6 节的 4 个问题，并在第 1–3 题附几处供执行者顺手留意的实现细节。

---

## 1. 第 6.1 题：R2 的 ID 规则与 `PROTOCOL.md` 是否逐条一致？还有没有别的上报依赖 `report_id`？

### 1.1 与 `PROTOCOL.md` 三段契约及 Cloud 接收端逐条核对

1. **`xui.snapshot`**（[PROTOCOL.md](../../PROTOCOL.md) 第 280 行；现行 [snapshot.go](../../../internal/modules/xui/snapshot.go) 第 105–123 行）
   - **代码核实**：协议要求启动先发、每分钟一次、连接 ready 立即发；上一份已确认时哪怕内容未变也换新 `report_id` 发送（供 Cloud 计算闲置分钟速率），上一份未确认且内容逐字节相同时原样重发，内容变了才换新 `report_id` 取代。Cloud 侧 `AkastrCloud` `backend/apps/assembly/index.ts` 第 103 行与 `backend/application/xui-snapshot.ts` 第 19–50 行完全不读 `report_id`。
   - **结论**：与 R2 **逐字一致**。
   - **具体后果 / 生产发生 / 防护级别**：行为零变化，生产无差异；属 [AGENTS.md](../../../AGENTS.md) 三级。

2. **`firewall.status`**（[PROTOCOL.md](../../PROTOCOL.md) 第 305 行；现行 [status.go](../../../internal/modules/firewall/status.go) 第 119–132 行）
   - **代码核实**：协议要求每分钟一次、连接 ready 立即发；未确认的旧一份原样重发，内容变了才被取代。Cloud 侧 `AkastrCloud` `backend/apps/assembly/index.ts` 第 133 行与 `backend/features/firewall/nodes.ts` 第 254–287 行不读 `report_id`。
   - **结论**：与 R2 **逐字一致**。
   - **具体后果 / 生产发生 / 防护级别**：行为零变化，生产无差异；属三级。

3. **`metrics.sample`**（[PROTOCOL.md](../../PROTOCOL.md) 第 316、329 行；现行 [metrics.go](../../../internal/modules/metrics/metrics.go) 第 72–97 行）
   - **代码核实**：启动先发、每分钟一份、连接 ready 立即发、必填项读不到则当分钟跳过且只在开始失败时记一次日志，均与 R2 一致。唯一字面差别如 R2 所述：`PROTOCOL.md` 第 316 行写“未确认的被下一份取代”（现行代码每次采样都生成新 UUID），R2 在“上一份未确认且两次 JSON 逐字节完全一致”时沿用原 `report_id`。核对 `AkastrCloud` `backend/apps/assembly/index.ts` 第 67 行与 `backend/core/node-metrics/metrics.ts` 第 125–198 行：Cloud 入口直接丢弃 `report_id`，仅在 `metrics.ts` 第 157 行用 `previous.data.boot_id === sample.boot_id && sample.uptime_seconds <= previous.data.uptime_seconds` 防重与防乱序倒退（保护流量差值不被算成计数器归零重加）。
   - **具体后果**：无任何可观察后果，连 `PROTOCOL.md` 都不必为此改字眼。
   - **生产上会不会发生**：仅在进程刚启动、首次本地采样（此时 WSS 尚未 ready，报文未发出）后不到 1 秒内握手完成触发 `ControlReady`、且两次读取恰好落在同一整数 `uptime_seconds` 且网卡字节未变时才会命中沿用，对线上报文零影响。
   - **防护级别**：三级。

### 1.2 Cloud 侧全部 7 种 `report` 对 `report_id` 的依赖全量核查

核对 `AkastrCloud` `backend/core/agent-control/gateway.ts`（第 270–276 行）与 `backend/apps/assembly/index.ts` 注册的全部 7 种上报：

- 通道层 `gateway.ts` 只负责在 handler 成功后回发 `report.ack { report_id }`，自身不存 `report_id`。
- 业务层实际读取并使用 `report_id` 的**只有 2 种**，且均不在 R2 合并的三个模块内：
  1. **`routing.outbound_health`**（`backend/features/routing/health.ts` 第 66–73 行）：存入 `routing_outbound_health.report_id`，靠 `WHERE h.report_id <> EXCLUDED.report_id` 区分轮次累加 `down_rounds`。R2 将其排除完全准确。
  2. **`ip.address`**（`backend/core/agent-control/service.ts` 第 196–205 行 → `backend/core/nodes/ip-state.ts` 第 48–69 行）：存入 `node_ip_events.source_id`，靠 `SELECT 1 FROM node_ip_events WHERE family = $1 AND source_id = $2` 去重，防止回执丢失重发时重复写入 IP 变化事件、重复结算换 IP 或重复通知客户（属一级/二级边界；`ipwatch` 本就不在 R2 内，不受影响）。
- 其余 5 种（`xui.snapshot`、`firewall.status`、`metrics.sample`、`connection_records.batch`、`changeip.unchanged`）均忽略 `report_id`（`connection_records` 在 `backend/core/usage/records.ts` 第 113 行按 `(node_id, at, source, source_port)` 去重，`changeip.unchanged` 在 `backend/features/changeip/agent-handler.ts` 第 66–82 行按 `command_id` 收敛）。

### 1.3 R2 实施细节提醒：保持“先锁内落 `pending`、解锁后再 `publish`”的顺序

- **代码核实**：现行 [snapshot.go](../../../internal/modules/xui/snapshot.go) 第 113–122 行与 [status.go](../../../internal/modules/firewall/status.go) 第 124–131 行都是先持锁更新 `pending` 并拷出局部 `report` 副本，解锁后再调 `publish(report)`。共用件 `module.Latest` 应保持同一顺序，不要写成“先 `publish` 成功后再持锁写 `pending`”。
- **具体后果**：若反过来写，低延迟链路上对端 `report.ack` 可能在 `publish` 返回并重新拿锁前先到达 `Acknowledge()`，导致当次 `ack` 未匹配上、`pending` 多留到下一分钟重发一次。
- **生产上会不会发生**：低概率竞态，即使发生也只是下一分钟多发一次同 ID 报文，无数据错误。
- **防护级别**：三级（沿用现行 6 行写法即可自然避免，无需额外机制）。

---

## 2. 第 6.2 题：R3 把 `changeip` 两个子包并回父包，有没有更省改动的做法？

- **代码核实**：[provider.go](../../../internal/modules/changeip/provider.go) 定义了 `Result` 与 `TriggerState`，子包 [httpcurl/provider.go](../../../internal/modules/changeip/httpcurl/provider.go) 与 [command/provider.go](../../../internal/modules/changeip/command/provider.go) 都导入了 `internal/modules/changeip`。受 Go 禁止循环导入限制，如果不并包又要让 `changeip` 自己按配置选择触发器，要么新增第三个微型子包（如 `changeip/trigger`）放 29 行的 `Result`，要么在父子包各写一份 `Result` 结构体再做字段拷贝。整个 `changeip` 连同两个子包一共只有约 540 行非测试代码，拆四包或重复定义类型都比并回单包更碎。
- **结论**：**没有比并包更简洁的做法，支持 R3.1 直接并回 `changeip` 包。**
- **并包时的两处核实细节（供执行者留意）**：
  1. 原两个子包均导出了同名的 `Config`、`Provider`、`New`，而父包已有 `Config`（[module.go](../../../internal/modules/changeip/module.go) 第 26 行）、`Provider` 接口（[provider.go](../../../internal/modules/changeip/provider.go) 第 27 行）与 `New`（[handler.go](../../../internal/modules/changeip/handler.go) 第 26 行）；并包后将两个触发器改为包内私有类型与构造函数（如 `httpProvider`/`commandProvider`），对外暴露面反而更小。
  2. [command/provider_test.go](../../../internal/modules/changeip/command/provider_test.go) 第 16–21 行定义了 `TestMain`（仅当环境变量 `AKASTR_AGENT_HELPER_PROCESS=1` 时把测试进程本身当作无参外部程序退出）。核实 [handler_test.go](../../../internal/modules/changeip/handler_test.go) 与 [httpcurl/provider_test.go](../../../internal/modules/changeip/httpcurl/provider_test.go) 均无 `TestMain` 且不设该环境变量（`httpcurl` 测试运行真实 `/usr/bin/curl`），并入同包后单测互不干扰。
- **具体后果 / 生产发生 / 防护级别**：纯包内符号收敛，线上行为零变化，生产不会发生问题；属三级。

---

## 3. 第 6.3 题：还有没有没列出的跨模块耦合，或 `internal/app` 里仍属于单个模块的细节？

### 3.1 跨模块耦合核查

全仓检索 `internal/modules/` 导入（`cmd/` 与 `internal/` 共 29 处）：除 `internal/app` 集中登记与 [connection_test.go](../../../internal/transport/connection_test.go) 测试外，模块之间的耦合**只有 R1 列出的那 4 处，无遗漏**。

- **顺手提醒**：[handler_test.go](../../../internal/modules/changeip/handler_test.go) 第 10、26 行在测试代码里也导入了 `internal/modules/ipwatch` 构造 `fakeObserver`；随 R1.2 改用 `changeip` 自己的观察函数类型后，该测试导入会一并自然消除（纯三级整理）。

### 3.2 `internal/app` 及脚本契约中值得留意的 4 处细节

1. **`app/modules.go` 里专为 `xui` 写的 `spliced` 探测闭包**（[modules.go](../../../internal/app/modules.go) 第 35、189–196 行）
   - **代码核实**：`modules.go` 导入 `internal/xray/process` 只为构造 7 行的 `spliced` 函数传给 `xui.NewSnapshots`。全仓仅 `xui` 使用 `Spliced()`，且 `xui` 原本就导入 `internal/xray/*`；将默认读取收进 `xui.NewSnapshots`（包内测试仍可改写字段），`app/modules.go` 即可去掉 `internal/xray/process` 导入。
   - **具体后果 / 生产发生 / 防护级别**：纯模块内聚整理，无线上运行差异，生产不发生问题；属三级，建议顺手归入 R3.4。

2. **`script/pin.go` 还有另一处 CI 脚本用 `sed` 读取（补充第 2 节不变量第 5 条）**
   - **代码核实**：除 [verify-debian-runtime-dependencies.sh](../../../scripts/verify-debian-runtime-dependencies.sh) 读取 `RunnerPackages` 和 `RunnerCommands` 外，[verify-ipquality-source.sh](../../../scripts/verify-ipquality-source.sh) 第 4–6 行同样写死路径 `internal/modules/ipqualityrunner/script/pin.go`，并用 `sed` 按单行格式提取 `PinnedCommit = "..."` 与 `PinnedSHA256 = "..."`。R3.2 实施时，`pin.go` 的路径与这四个常量的单行写法都需保持原样。
   - **具体后果 / 生产发生 / 防护级别**：若误动路径或格式，验证阶段 `sh scripts/verify-ipquality-source.sh` 与 GitHub Actions 会立即失败拦截，不会流入生产；属三级，提前注明可避免返工。

3. **`RemoveStaleAssets` 在 `daemon` 与 `app` 之间的调用边界**（[run.go](../../../internal/daemon/run.go) 第 91 行、[modules.go](../../../internal/app/modules.go) 第 144–152 行）
   - **代码核实**：R5 提到“`RemoveStaleAssets` 只剩清理旧版固定脚本，随 R3 归到 `ipqualityrunner`”。注意调用方是 `daemon.Run`，为遵守“模块只在 `internal/app/modules.go` 集中登记”，应保留 `app.RemoveStaleAssets(paths)` 作为薄转发函数调用 `ipqualityrunner` 的包级清理入口（且保持现行不依赖 `m.runner != nil` 的无条件调用，使曾开过后来关闭 Runner 的节点也能清掉旧摘要脚本）。
   - **具体后果 / 生产发生 / 防护级别**：若让 `daemon` 直接导入 `ipqualityrunner` 会打破单点登记约定；若绑到 `m.runner != nil` 则关掉 Runner 的节点可能残留旧脚本文件（仅占几十 KB 磁盘，无安全后果）；属三级。

4. **`StateDir` 传递与 `firewall.Available()` 前置检查的小不对称**（[modules.go](../../../internal/app/modules.go) 第 168、207–210、234–239 行）
   - **代码核实**：`firewall.New` 直接收 `paths.StateDir` 自己拼模块目录，而 `xui.NewPanel` 由 `app` 拼 `filepath.Join(paths.StateDir, xui.Name)`，`ipwatch.OpenMonitor` 由 `app` 拼 `ipwatch.ReconciliationFile(paths.StateDir)` 并做 `IntervalSeconds * time.Second` 换算，`firewall` 还需 `app` 先调 `firewall.Available()` 再调 `firewall.New`。这些都可在 R3 顺手收进各自模块构造函数，也可保持现状。
   - **具体后果 / 生产发生 / 防护级别**：零运行影响，生产无差异；属三级（由执行者视改动幅度决定即可）。

---

## 4. 第 6.4 题：不采纳清单中，是否有确有实际后果、应当恢复的项？

**结论：没有任何一项需要恢复，第 4 节不采纳清单全部认同。** 关键条目对照代码的核实如下：

1. **“采样失败时重发上一份未确认报告”（初稿 §4.1）——不恢复（原提议反而有害）**
   - **代码核实**：对照 `AkastrCloud` `backend/core/node-metrics/metrics.ts` 第 138–190 行，Cloud 以服务端接收时刻 `deps.now()` 写入 `node_metrics.received_at` 并归入 `node_metric_minutes` 当分钟桶。若某分钟本机某项读取失败却把几分钟前未确认的旧样本重发上去，且其 `uptime_seconds` 仍大于 Cloud 上一次成功落库的更早样本，就会把几分钟前的过时 CPU/内存/负载写进当前分钟桶，同时直接违反 [PROTOCOL.md](../../PROTOCOL.md) 第 329 行“任何一项读不到，这一分钟就不发”。
   - **具体后果 / 生产发生 / 防护级别**：采纳原提议会在“断线未确认 + 随后某分钟读取失败”叠加时污染当前分钟监控读数；保持现状“失败当轮不发、等下一分钟新采样”完全正确（属二级/三级协议与数据准确性边界）。

2. **“把 `routing.outbound_health` 并入共用上报器”（初稿 §4.2）——不恢复**
   - **代码核实**：对照 `AkastrCloud` `backend/features/routing/health.ts` 第 5–6、66–73 行，Cloud 确实依靠 `WHERE h.report_id <> EXCLUDED.report_id` 累加连续失败轮次 `down_rounds`（满 `DOWN_ROUNDS = 2` 轮才判定出站故障告警）。
   - **具体后果 / 生产发生 / 防护级别**：若套用 R2“未确认且内容相同沿用原 `report_id`”，当上一轮失败报告的 `report.ack` 因网络抖动丢失、5 分钟后新一轮检测结果仍为同样的 `up: false` 时，Cloud 会将第二轮当作重复投递忽略，导致生产出站故障 Telegram 告警推迟 5 分钟（网络不稳时真可能发生，属二级/三级可观测退化）。保持 [health.go](../../../internal/modules/routing/health.go) 独立最稳妥。

3. **“新建 `internal/subproc` 合并三个截断缓冲区与进程组文件”（初稿 §6.2）——不恢复**
   - **代码核实**：核对三处实现，语义确不相同——[httpcurl/provider.go](../../../internal/modules/changeip/httpcurl/provider.go) 第 122–134 行直接读取 `head.data` 与 `head.truncated` 回退 `len(token)-1` 字节，专门防止服务商报错回显的 Bearer Token 在 4 KiB 截断处被劈开从而漏过替换脱敏（提交 `0819e77`）；[script/provider.go](../../../internal/modules/ipqualityrunner/script/provider.go) 第 157 行一旦 `overflow` 直接判整次检测 `script_output_too_large` 失败；[diagnostics/module.go](../../../internal/modules/diagnostics/module.go) 第 331 行则是截断保留并做 UTF-8 替换。
   - **具体后果 / 生产发生 / 防护级别**：`changeip` 的截断逻辑涉及**一级**凭据防泄露边界，省几十行代码没有任何实际收益，却增加了改动敏感代码引入回归的风险。

4. **其余不采纳项（`UpdateBlocker` 拆接口、`firewall.go` 拆文件、去掉第二个无状态 `newSystem("")`、`Runtime` 独立 `WaitGroup`、`install.sh` 放宽 `sed`、`app_test.go` 扩空分支、删 `ip-state.json` 前查生产库）——全部不恢复**
   - **代码核实**：
     - `firewall`：[host.go](../../../internal/modules/firewall/host.go) 第 65–77 行的 `system` 只存常量路径与函数指针，多一个实例零状态、零开销；`firewall.go` 共用单锁 `f.mu` 与单状态文件 `state.json`，纯拆文件不降耦合，而防火墙写错有把远程节点锁在门外的风险（一级/二级运维风险 vs 三级文件长度）。
     - `install.sh`：[json_file.go](../../../internal/state/json_file.go) 第 77 行固定 `SetIndent("", "  ")`，顶层字段缩进恒定，且 [AGENTS.md](../../../AGENTS.md) 明确不为安装脚本加推测性防护（三级）。
     - `app_test.go`：[agent-protocol-v12.json](../../../internal/protocol/testdata/agent-protocol-v12.json) 中确实只有 `xui` 的 `state.put` 样例，扩分支零执行（三级）。
     - `ip-state.json`：核对 `AkastrCloud` `docs/archive/2026-11-feature-rollouts.md` 第 39 行（节点已于 2026-11-19 升至 `v1.30.1`）及 [install.go](../../../internal/install/install.go) 第 144–163 行（仅无 `identity.json` 时才拒绝非空 `StateDir`），残留单文件零读取、零阻塞，无需连接生产查询（三级）。
     - `R6`（可选清理 worker）：若主人决定去重，用普通 `Worker` 包装而不改动 [runtime.go](../../../internal/app/runtime.go) 的核心退出语义，确实比改 `Runtime` 更稳妥（三级）。
