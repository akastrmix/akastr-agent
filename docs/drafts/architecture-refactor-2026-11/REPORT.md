# AkastrAgent 内部规整重构执行报告

> 临时草稿，随 `docs/drafts/` 一起删除。执行依据：同目录 SPEC.md（fd4b86c，主人已确认 R1–R6）。未发布 Agent。

## 1. 提交

| 项 | 提交 | 内容 |
|---|---|---|
| R1 | `ab52fd0` | 受管用户判断移到 `internal/xray` 根包（`xray.Owned`）；`changeip` 改收观察函数 `ObserveIPv4`，由 `app` 包装 `ipwatch` 观察器 |
| R2 | `245f133` | 新增 `module.Latest`；`xui.snapshot`、`firewall.status`、`metrics.sample` 只留采样；出站检测不动 |
| R3 | `1143be8` | changeip 两个子包并回主包并自己选触发方式；ipqualityrunner、ipwatch、xui、firewall 自己组装；宿主依赖由模块导出 |
| R4 | `f158ddb` | UUID、控制地址校验与 HTTPS 换算、SOCKS5 登录校验各留一份在 `protocol` |
| R5 | `14fb613` | 删除 `ip-state.json` 清理；`RemoveStaleAssets` 照旧无条件清理旧版固定脚本 |
| R6 | `8e386be` | 新增 `module.Cleanup`；限速、防火墙残留清理与诊断清扫共用 |

## 2. 与 SPEC 不一致或 SPEC 未定的地方

**R1**
- `routing` 包内已有一个名为 `xray` 的内部接口，与新根包同名，`routing.go` 用别名 `xrayuser` 导入。
- `xui` 自己校验目标客户端时用的同一条正则（`decodeTarget`）也改用 `xray.Owned`，正则不变；`apply.go`、`snapshot.go` 的 `strings.HasPrefix(email, "ak-")` 按 SPEC 保持原样。
- `changeip` 的测试也不再导入 `ipwatch`：观察方式改成函数后，假观察器跟着改成函数。

**R2**
- 命名：`module.Latest` / `module.NewLatest(kind, interval, sample)`。三个模块原来的导出类型（`xui.Snapshots`、`firewall.Status`、`metrics.Sampler`）改为包内的采样结构，构造函数返回 `*module.Latest`。
- 测试：`TestPublishedSampleIsAnObject` 移到 `internal/module/latest_test.go`；ID 规则断言移到同文件的 `TestLatestSendsTheNewestUntilAcknowledged`。这个测试另外断言了 SPEC R2 写明的两条规则：内容变化就换新 ID（旧 ID 的回执不再认），采样失败什么也不发。没有新增测试函数。`xui` 的 `TestSnapshotTellsEachMinute` 改名为 `TestSnapshotSaysOwnedConnectionsAndSplice`，直接测采样内容。

**R3**
- changeip 并包后两个触发方式是包内类型 `httpProvider`、`commandProvider`；对外只有 `changeip.New(cfg, observe, reconciler) (*Handler, error)`，时限常量（触发 1 分钟、触发前观察 5 分钟）和 curl 路径在 `changeip` 内。HTTP 方式不再接收程序路径参数，原来“程序必须是 `/usr/bin/curl`”的检查随之去掉：它检查的是一个固定常量。两种方式的时限范围检查保留（测试用更短的时限）。三个同值结果码合为一份放在 `provider.go`，所有结果码字符串逐字不变。`command` 测试函数从 `TestProvider*` 改名为 `TestCommand*`（同一个包里现在有两种 provider）。
- `ipwatch.New()` 不再带参数，也不再返回错误：原来的两项检查只检查 `app` 传进来的常量，常量（每个来源 10 秒、User-Agent `Akastr-Agent`）现在放在 `ipwatch` 内。
- ipqualityrunner 对外提供 `New(root)`、`PrepareScript(ctx, client, root)`、`RemoveStaleScripts(root)`；`script` 子包与 `pin.go` 不动。
- 宿主依赖：`ratelimit`、`firewall`、`ipqualityrunner` 各导出 `HostCommands`、`HostPackages` 两个变量；`app` 收集的顺序和内容与原来相同。
- `firewall.New` 改为返回 `(*Firewall, error)`，nft 缺失时报原来的错误文字。
- `xui.NewSnapshots(panel, connections)` 默认自己读 Xray 是否直传；包内测试直接给采样结构设 `spliced`。面板端口为 `xui.Config.PanelPort()`。
- `app` 里还剩 `ipwatch` 间隔秒数换算成 `time.Duration` 这一行，以及 SPEC 第 6 节说维持现状的 `xui`、`ipwatch` 状态路径拼接。

**R4**
- 新文件 `internal/protocol/endpoint.go`：`ValidControlEndpoint(endpoint) bool` 与 `ControlHTTPS(endpoint, path) (string, bool)`；`protocol.ValidSOCKS5Login`。config、transport、identity、update 四处错误文字不变。

**R6**
- 命名：`module.Cleanup` / `module.NewCleanup(leftovers, retry, clean)`。SPEC 写的是“可选在成功时关闭一个通道”；实现改为成功时总会关闭自带通道，经 `Done()` 读取，只有诊断用。这样构造函数少一个参数，结果相同。故意不叫 `Started()`：`Runtime` 会把带 `Started()` 的 worker 当成 `module.Starter`，让控制连接等清理完成才建立。
- 日志文字有变化：原来是三句各自的英文（`rate limit rules … remain`、`firewall table … remains`、`diagnostics units … remain`）。现在统一为 `leftovers of an earlier process remain; retrying`，用 `leftovers` 字段区分 `rate limit rules`、`firewall table`、`diagnostics units`，意思不变。重试间隔 30 秒、30 秒、1 分钟不变。
- `ratelimit.Leftovers()`、`firewall.Leftovers(stateDir)` 由类型改为返回 `*module.Cleanup` 的函数；`diagnostics.New` 改收 `swept <-chan struct{}`（`app` 传 `sweeper.Done()`）。

**文档**：按 SPEC §5 同步 README 的“改代码必查联动点”与 ARCHITECTURE.md 第 4 节（`internal/module` 的 `Latest`/`Cleanup`、`internal/xray` 根包、`changeip` 不再有子包），各随对应的提交改。PROTOCOL.md、INSTALLATION.md 未改。Cloud ADR 0076 提到删除 `ip-state.json`，那是当时的决定记录，没有改（本次也不改 Cloud）。

## 3. 不变量核对

- `internal/protocol/testdata/`、`docs/PROTOCOL.md`、`docs/INSTALLATION.md`、`internal/modules/ipqualityrunner/script/pin.go` 与 fd4b86c 相比无差异。
- `internal/operation`、`desired`、`install`、`state`、`xray/ledger`，以及防火墙的 `render.go`、`state.go` 无差异；`transport/client.go`、`update/client.go` 只按 R4 替换了控制地址校验与 HTTPS 换算。
- 本地持久文件路径与格式没有动。
- `internal/modules/*` 非测试代码之间的互相导入为零（只剩 `ipqualityrunner` 导入自己的 `script` 子包）。

## 4. 验证（2 核 2 GB，串行、`GOMAXPROCS=1`、`GOMEMLIMIT=256MiB`）

- `./scripts/verify-go.sh`：通过（约 45 秒，含 `ipwatch`/`daemon`/`desired` 各重复 20 次、全量测试、`go vet` 与构建）。
- 对 `scripts/*.sh` 逐个执行 `sh -n`（与 CI 相同的循环）：通过。
- `sh scripts/verify-ipquality-source.sh`：通过（commit `0ee5f192…`，sha256 `9823c560…`）。
- 本机跳过的测试：防火墙真实内核测试 `TestRulesInARealKernel`、`TestForwardingInARealKernel` 因为本机没装 nft 而跳过。本次对 `kernel_test.go` 只改了一个局部变量与类型名，`go vet` 已编译通过。诊断沙盒测试（root + systemd）实际运行并通过，包括 `TestSweeperStopsUnitsOfAnEarlierProcess`。
- Debian 12/13 容器依赖检查没在本机跑（需要 Docker，不在 SPEC §5 的列表里）；推送后 GitHub Actions `Verify`（run 45112887758，f7d48ad）全部通过，其中包括这项检查。
