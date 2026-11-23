# AkastrAgent 内部规整重构方案（Spec）

> **目录生命周期**：临时草稿；执行并发布后删除整个 `docs/drafts/` 目录，不采纳清单随 Git 历史保留。
>
> **状态：Claude 改写稿（基于 agy 初稿 61d769b），agy 已复核（同目录 REVIEW.md，0421449，无阻塞异议）并已并入其补充。主人 2026-11-23 在对话中确认按本 Spec 执行 R1–R6。**

- 基准代码：`a37e6ea`（v1.30.1 之后；`61d769b` 只加了本草稿）。协议 `2026-11-01.v12`，节点配置 `schema_version = 5`。
- 性质：纯 Agent 内部整理。不改协议与共享夹具、不改本地持久文件路径与格式、不改 Cloud、无新增界面术语/存储名/协议名，不触发 Cloud ADR 0024；节点上看不出区别。

## 1. 判断

底座（`daemon`、`transport`、`operation`、`desired`、`lifecycle`、`layout`、`install`、`update`、`state`、`protocol`，以及 `xray/*`、`panel`、`sockdiag`、`hostroute`）边界清楚，不动。凌乱集中在最近加功能时留下的 4 类问题：

1. **模块互相引用**：违反 AGENTS.md“模块之间互不引用”（R1）。
2. **同一段“只报最新一份”的上报样板抄了多份**：已出过事故——v1.29.0 的 `metrics` 抄样板时漏了 `json.RawMessage`，`[]byte` 被编码成 base64 字符串，Cloud 解析失败断开连接，test 节点每次重连 1 秒内断线（a564a18）（R2）。
3. **装配文件知道模块内部细节**：`internal/app/modules.go` 替 changeip 选触发方式并写死 curl 路径与时限、替 IPQuality 拼固定脚本参数、替限速和防火墙写 apt 包名、替防火墙解析面板端口（R3）。
4. **小重复与到期兼容代码**（R4、R5）。

## 2. 不变量（执行全程守住）

1. PROTOCOL.md 的消息类型、字段、错误码与 `kind`、上报周期、重发与 `report_id` 语义不变；`internal/protocol/testdata/agent-protocol-v12.json` 不改。
2. `identity.json`、`state.json`、`update-attempt.json`、`changeip-reconciliation.json`、`xui/state.json`、`firewall/state.json` 的路径与 schema 不变。
3. `transport`、`operation`、`desired`、`install`、`update` 的状态机、锁与租约不动；`xray/ledger` 的连接认领与计数算法不动；`firewall` 的 `apply` 顺序、规则渲染与摘要、`Started()` 先于控制连接的顺序不动。
4. ChangeIP 与 IPQuality 的执行、恢复与结果码（`completed`、`timed_out`、`request_failed` 等字符串）逐字不变。
5. `internal/modules/ipqualityrunner/script/pin.go` 的路径，以及其中 `PinnedCommit`、`PinnedSHA256`、`RunnerPackages`、`RunnerCommands` 四个常量的单行写法不变：`scripts/verify-ipquality-source.sh` 与 `scripts/verify-debian-runtime-dependencies.sh` 按这个路径和格式用 sed 读取它们。
6. 只搬代码不改行为。下文唯一声明的行为差异见 R2，Cloud 侧无可观察变化。

## 3. 改动（每项一个提交，按序执行）

### R1 断开模块之间的引用

现状（非测试代码）：

- `ratelimit`、`connectionrecords`、`routing` 只为调用 `xui.Owned(email)`（`^ak-[a-z0-9]{1,61}$`）而导入 `internal/modules/xui`。
- `changeip/handler.go` 只为 `ipwatch.AddressObserver` 与 `ipwatch.IPv4` 导入 `internal/modules/ipwatch`（对 `Monitor` 已用消费方接口 `changeReconciler`）。

做法：

1. 新建 `internal/xray` 根包（例如 `internal/xray/owned.go`），放“哪些 Xray 用户归 Cloud 管”的判断；`xui`、`ratelimit`、`connectionrecords`、`routing` 都改用它，删除 `xui.Owned`。
   - `xui` 内部 `apply.go`、`snapshot.go` 对面板客户端用的 `strings.HasPrefix(email, "ak-")` 保持原样，不并入正则：两者覆盖范围不同，合并会改变哪些客户端被清理或计入流量。
2. `changeip` 定义自己的观察函数类型（例如 `func(context.Context) (netip.Addr, time.Time, error)`），由 `internal/app` 用闭包包装 `observer.Observe(ctx, ipwatch.IPv4)` 传入；`changeip` 不再导入 `ipwatch`。

验收：`internal/modules/*` 的非测试代码之间零互相导入（模块自己的子包除外）。`internal/transport/connection_test.go` 用真实 `ipwatch.Reporter` 做连接测试，属测试代码，保留。

### R2 三个“快照型”上报器共用一个实现

现状：`xui.snapshot`、`firewall.status`、`metrics.sample` 各自手写同一套循环（启动先发、每分钟一次、连接 ready 立即重新采样）与 `wake`/`mu`/`pending`/`ControlReady`/`Acknowledge`/`UpdateSafe`。

做法：在 `internal/module` 加一个共用上报器（名字执行者定，例如 `module.Latest`），参数为 `kind`、周期、`sample func(context.Context) ([]byte, error)`：

- 启动时、每个周期、每次 `ControlReady` 各采样一次；采样失败什么也不发（与现状一致；日志由模块的 `sample` 自己记，`metrics` 的“只在开始失败时记一次”保留在 `metrics` 内）。
- 采样成功：没有待确认报告，或内容与待确认报告逐字节不同，就生成新 `report_id`；否则沿用原报告原样重发。沿用现行写法：先在锁内更新待确认报告并取出副本，解锁后再发送（反过来写时，回执可能先于记录到达而对不上）。
- 报告的 `data` 一律以 `json.RawMessage` 发出（杜绝 a564a18 那类事故）。
- `Acknowledge` 只认当前待确认报告的 `report_id`；`UpdateSafe` 恒为 nil。
- 各模块只保留采样逻辑：`xui` 的 30 秒读取时限、`metrics` 的上一份 CPU 读数与 `systemd-detect-virt` 结果都留在各自模块。

唯一的行为差异：`metrics` 现在每份都换新 `report_id`；改后若一份未确认、下一份内容逐字节相同则沿用原 ID。实际几乎不会发生（`uptime_seconds` 每秒变化），且 Cloud 不按 `report_id` 处理监控样本，无可观察变化。

**`routing.outbound_health` 不并入，保持现状。** 它与另外三个有两处本质不同：连接 ready 时只原样重发、不重新测试（每轮要拉起临时 Xray）；每一轮必须是新的 `report_id`——Cloud 按 `report_id` 区分轮次累计连续失败（`AkastrCloud backend/features/routing/health.ts` 的 `WHERE h.report_id<>EXCLUDED.report_id`）。若套用“内容相同沿用原 ID”，连续两轮相同的 down 结果在回执丢失时会被 Cloud 当成同一轮，推迟出站故障提醒。为一个上报器给共用件加两个开关不值得。

测试：`TestPublishedSampleIsAnObject`（防断线事故）与 `TestSnapshotTellsEachMinute` 中的 ID 规则断言移到共用件的测试里；模块测试改为直接测各自的采样内容。不新增其他测试。

### R3 模块自己按配置组装自己

现状：`internal/app/modules.go` 顶部的 `changeIPTimeout`、`changeIPObserveWindow`、`ipQualityTimeout`、`observationTimeout`，`build` 里的 `http_bearer` 判断与 `/usr/bin/curl`，IPQuality 的 `PinnedPath`/`PinnedVersion`/`PinnedSHA256` 拼装，`hostCommands` 里替 `ratelimit`（`iproute2`）和 `firewall`（`nftables kmod procps`）写的包名，以及 `urlPort`。changeip 的两个触发方式子包导入了父包，父包因此无法自己组装，只能放在 `app`。

做法：

1. **changeip**：把 `changeip/httpcurl` 与 `changeip/command` 并回 `changeip` 包（例如 `http.go`、`command.go`、`process_unix.go`，测试随之移动），由 `changeip` 按自己的配置选择触发方式；时限常量与 curl 路径移入 `changeip`。纯搬家，两份同值的结果码常量（`completed`、`timed_out`、`cancelled`）合为一份，字符串不变。
2. **ipqualityrunner**：保留 `script` 子包（父包导入子包，没有环）。由 `ipqualityrunner` 用固定版本常量和时限构建 `script` 执行器，并对外提供准备与清理固定脚本的入口；`app` 不再导入 `script`。
3. **宿主依赖**：`ratelimit`、`firewall`、`ipqualityrunner` 各自导出所需命令与 Debian 包；`app` 只按启用的模块收集。
4. **其余**：面板端口解析移为 `xui` 配置的方法；`ipwatch` 观察器的时限与 User-Agent 移入 `ipwatch`；`xui` 快照默认自己读 Xray 是否直传（`app` 不再为此导入 `internal/xray/process`，包内测试仍可替换）；`firewall.Available()` 并入防火墙的构造函数，`app` 不必记得先调它。

`app` 保留的只有跨模块连线与跨模块依赖校验：两份面板登录（`xui`/`routing` 共用一份、出站检测一份）、访问日志 → 连接账本 → 限速/连接记录/快照、给防火墙的控制地址与面板端口、诊断清理、模块关闭时的残留清理，以及“changeip 需要 ip_watch”等依赖检查。

### R4 核心里的小重复各留一份

1. UUID 正则：`internal/config` 与 `internal/identity` 各自定义了与 `protocol` 相同的 `canonicalUUID`，改用 `protocol.ValidUUID`。
2. 控制地址：`config`、`transport`、`identity`（enroll）、`update`（maintenance）各写了一遍“必须是 `wss://<host>/internal/agents/ws`、无用户/查询/片段”的检查，后两处还各自换算 HTTPS 地址。收成 `protocol` 里一个校验加一个换算函数；各处错误文案不变。
3. SOCKS5 登录：`socks5` 与 `ipqualityrunner` 有逐字相同的 `validLogin`，移入 `protocol`。

### R5 删除到期的兼容代码

`internal/app/modules.go` 中删除 `ip-state.json` 的几行（协议 v8 以前的遗留，注释写明“没有节点再跑那种 Agent 就删”）。按记录，所有节点 2026-11-19 起都在 v1.30.1。即使某台离线很久的老节点以后直接升到新版：新 Agent 不读这个文件；安装时只在缺 identity 的情况下才检查状态目录，那时本来就会拒绝；卸载会删除整个目录。所以遗留的只是一个无人读取的小文件，属三级，**不需要再查生产**。`RemoveStaleAssets` 只剩清理旧版固定脚本：清理实现随 R3 归到 `ipqualityrunner`，`app.RemoveStaleAssets` 保留为 `daemon` 调用的入口并照旧无条件调用它（`daemon` 不直接导入模块；关掉 Runner 的节点也要清掉旧脚本）。

### R6 关模块后的残留清理共用一个 worker

现状：`ratelimit.Leftovers`、`firewall.Leftovers`、`diagnostics.Sweeper` 各写了一遍“清理，失败隔一段时间重试，成功后空等到进程结束”。

做法：在 `internal/module` 加一个普通 `Worker` 实现（名字执行者定），参数为名字、重试间隔、清理函数，可选在成功时关闭一个通道（诊断用它放行脚本）。**不改 `Runtime`**：它仍是一个常驻 worker，“任一 worker 退出即进程退出”的规则不变。各模块的重试间隔（30 秒、30 秒、1 分钟）与日志意思不变。

理由：AGENTS.md 要求在进程外留状态的模块都要处理“模块关闭”这种情况，以后的新模块会再抄一遍这个循环。

## 4. 不采纳清单（防止执行或复核时再加回来）

| 提议（来源） | 不采纳理由 |
|---|---|
| 采样失败时也重发上一份未确认报告，并当作缺陷修复（agy §4.1） | 不是缺陷：只报最新一份的数据，失败的那一分钟不发没有后果。对 `metrics` 是改坏：PROTOCOL 写明“任何一项读不到，这一分钟就不发”，Cloud 按收到时间记录监控，重发旧样本会把几分钟前的 CPU、内存当成现在显示 |
| 把出站检测并入共用上报器，用 `sampleOnReady` 开关区分（agy §4.2） | 见 R2：Cloud 按 `report_id` 计轮次，套用“内容相同沿用原 ID”会推迟出站故障提醒；为它加开关不值得 |
| `UpdateSafe` 拆成可选接口 `UpdateBlocker`（agy §4.2） | R2 之后写死 `return nil` 的只剩共用件、出站检测和 `connectionrecords` 三处一行代码，为此改接口形状和文档不值 |
| 把 `firewall.go` 拆出 `forwarding.go`、`domains.go`（agy §5） | 同包、同一把锁、同一份状态，只是按行数拆文件，不降低耦合；防火墙是最可能把节点关在门外的模块，无谓搬家增加审查负担 |
| `firewall.Status` 与 `Firewall` 共用宿主对象，去掉第二个 `newSystem("")`（agy §5） | `system` 不保存状态，多建一个没有任何后果；理由只是“方便写纯内存单测”，不是实际问题 |
| 残留清理改由 `Runtime` 用独立 `WaitGroup` 调度（agy §6.1） | 去重可以（R6），但没必要改核心运行时的退出语义；空等的协程只占几 KB |
| 新建 `internal/subproc` 合并三个截断缓冲区与两份进程组文件（agy §6.2） | 三个缓冲区各十几行、附带功能各不相同；changeip 的那个是令牌遮盖的一环（0819e77 刚修过令牌在截断处泄露），为省约 40 行去动凭据相关代码不划算；进程组文件 15 行、从未改过 |
| 加固 `install.sh` 卸载时读取 `forwarding_opened` 的 sed（agy §6.4） | 文件排版由 `internal/state` 固定（两空格缩进），与同版本二进制一起发布，不会变；属于为推测情况加代码，AGENTS.md 也不要求为安装脚本加防护 |
| 扩展 `app_test.go` 的 `state.put` 解码分支、补两个依赖用例（agy §6.5） | 共享夹具里只有 `xui` 的 `state.put`，扩了分支什么也测不到；两个依赖用例防不住实际后果 |
| 删除 `ip-state.json` 兼容代码前先跑 `npm run prod:sql`（agy §6.6） | 见 R5：遗留文件无害，不需要动生产 |
| 为 `/proc/meminfo`、`boot_id` 读取新建 `procfs` 包；新增 import 结构检查测试（agy §2 已列为不做） | 同意不做 |
| 删除防火墙“没有标记链的表按旧表替换”的判断（`host.go`） | 保留（同意 agy）：除了兼容转发上线前的旧表，也能让被手工改坏的表自动换掉 |

## 5. 执行与验证

1. 开工前在 `/root/AkastrAgent` 执行 `git pull --ff-only`；按 R1 → R6 一项一个提交。
2. 文档只做现行描述的最小同步：README“改代码必查联动点”中宿主机包改为由模块自己声明；ARCHITECTURE.md 第 4 节更新 `internal/module`（共用上报器与清理 worker）、`internal/xray`（受管用户判断）、`internal/modules/changeip`（不再有子包）。PROTOCOL.md 与 INSTALLATION.md 不改。
3. 验证（2 核 2 GB 机器，串行，不与 Cloud 重型测试并行）：`./scripts/verify-go.sh`、`sh -n scripts/*.sh`、`sh scripts/verify-ipquality-source.sh`。
4. 执行者在本目录写 `REPORT.md`：每项的提交、与本 Spec 的偏差及原因、验证结果；完成后推送，供 Claude 验收。不发布，发布由主人另行决定。

## 6. 复核记录

agy 复核（REVIEW.md）：R2 与 PROTOCOL 三段逐条一致；Cloud 7 种上报里只有 `routing.outbound_health` 与 `ip.address` 使用 `report_id`，都不在 R2 内；并包是 R3.1 最简做法；跨模块耦合只有 R1 的 4 处；不采纳清单全部认同。

已并入：不变量 5 补上 `verify-ipquality-source.sh` 与 `pin.go` 路径；R2 的加锁顺序；R3.4 的直传探测与 `firewall.Available()`；R5 的 `RemoveStaleAssets` 调用边界。

未写入：`xui`、`ipwatch` 的状态路径由 `app` 拼接——纯外观，改 `ipwatch.OpenMonitor` 会牵连 `transport` 测试，维持现状；changeip 并包后包内只能有一个 `TestMain`（沿用 `command` 的即可），编译时自然发现，不另写规则。
