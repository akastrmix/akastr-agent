# AkastrAgent R1–R6 复核

对照每个提交的父提交复核了 `fd4b86c..aada402`。六项重构的运行时代码均未发现问题。REPORT 第 2 节列出的偏差合理。发现两处三级文档问题，均不影响节点运行。下列文件行号以 `aada402` 为准。

## 发现

1. **三级 · R6 `8e386be` · `docs/ARCHITECTURE.md:42`、`internal/module/cleanup.go:9`**。共用清理任务的说明把触发条件概括成“模块关闭后”，遗漏了诊断的例外。`internal/app/modules.go:196` 在每次启动时都登记诊断清理，只有接收诊断请求才取决于配置。诊断已开启、旧任务尚未清完时，新请求会收到 `sandbox_unavailable`（`internal/modules/diagnostics/module.go:152`）。照现有说明排查，会漏掉这个原因。共用说明写成“启动时清理上一进程的残留”即可，各模块的启用条件已有各自的说明。
2. **三级 · 执行报告 `f7d48ad` · `docs/drafts/architecture-refactor-2026-11/REPORT.md:56`**。`sh -n scripts/*.sh` 只解析展开后的第一个文件 `build-release.sh`，后四个文件名都成了它的位置参数。照此复跑会漏查安装脚本等四个文件。本次按 `.github/workflows/ci.yml:37` 对五个脚本逐个执行 `sh -n`，全部通过。

## 逐提交核对

| 提交 | 核对结果 |
| --- | --- |
| R1 `ab52fd0` | 未发现问题。`internal/xray/owned.go:7` 保留原正则，面板清理和流量统计仍按原来的前缀判断。`internal/app/modules.go:210` 固定观察 IPv4 并传回原观察时间和错误，换 IP 前核对地址、记录状态和执行的顺序一致。 |
| R2 `245f133` | 未发现问题。`internal/module/latest.go:36` 保留启动、每分钟和连接就绪三个采样时机，三类报告仍发送 JSON 对象。采样与发送在锁外，上报器先持锁记录待确认报告并取出副本，解锁后才发送。回执只认当前 ID。内容变化或前一份已确认时生成新 ID。监控样本未确认且内容逐字节相同时沿用旧 ID，符合 SPEC，Cloud 的监控处理函数只接收节点和样本内容（AkastrCloud `73ae0210`，`backend/apps/assembly/index.ts:67`）。 |
| R3 `1143be8` | 未发现问题。`internal/modules/changeip/handler.go:36` 按原配置选择触发方式。进程取消、超时、HTTP 结果分类和令牌遮盖均沿用原实现。`internal/modules/ipqualityrunner/handler.go:31` 使用相同的脚本路径、版本、摘要和 15 分钟时限。移除的检查只针对原先写死的参数。宿主命令与包的收集顺序、面板端口、直传探测及 nft 缺失时的错误和返回时机均与原来一致。 |
| R4 `f158ddb` | 未发现问题。`internal/protocol/endpoint.go:17` 保留四个入口原有的控制地址判断和两处 HTTPS 地址换算，各调用方仍返回原错误文字。UUID 正则相同，SOCKS5 登录仍按 1 至 255 **字节**计长并拒绝 NUL。 |
| R5 `14fb613` | 未发现问题。`internal/app/modules.go:129` 仅停止删除旧版的 `ip-state.json`，现行代码没有读取者。已确认的版本仍清理旧版固定脚本，包括 Runner 已关闭的节点。活动换 IP 核对记录继续使用原文件。 |
| R6 `8e386be` | 运行时未发现问题。`internal/module/cleanup.go:31` 首次立即清理，限速和防火墙失败后各等 30 秒再试，诊断等 1 分钟再试。成功后由运行该任务的唯一协程关闭通道并等待退出。只有诊断读取完成信号。启用防火墙时，控制连接仍等防火墙发出 `Started()` 信号后才建立。统一日志保留残留对象和原始错误。文档问题见上。 |

协议文档与共享夹具、现用持久文件的路径和格式、操作日志与更新租约均与基线一致。防火墙应用顺序和连接计数算法也未变。最后两次提交 `f7d48ad`、`aada402` 只修改 REPORT。

## 验证

`./scripts/verify-go.sh` 在独立的网络、进程和挂载环境中串行通过（`GOMAXPROCS=1`、`GOMEMLIMIT=256MiB`），覆盖三组各 20 轮重复测试、全量 Go 测试、`go vet` 和构建。本机有正在运行的 Agent，测试使用独立网络和独立的 `/run`，以隔开防火墙规则和 systemd 服务。需要真实 systemd 的四项诊断测试按自身条件跳过。

五个 Shell 脚本的语法检查及 `sh scripts/verify-ipquality-source.sh` 均通过。另核实了执行报告所引的 CI 记录，`f7d48ad` 的源码验证和 Debian 12/13 依赖检查均通过。[GitHub Actions 运行记录](https://github.com/akastrmix/akastr-agent/actions/runs/45112887758)
