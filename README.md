# Akastr Agent

Akastr Agent 是 AkastrCloud 服务节点上的轻量被控程序。它以单个 Go 进程（`akastr-agent.service`）主动建立出站 WSS 连接，只执行本机预配置、协议允许的 typed operation，不提供 HTTP 控制面或远程终端；唯一例外是默认关闭的 `diagnostics` 模块，只在一次性只读 systemd 沙盒里运行诊断脚本（读得到凭据且可联网，输出不落盘、不进日志，见 Cloud ADR 0102）。

> 节点操作只走 Akastr Agent；未接入、离线或暂停的节点会被主控安全拒绝操作。

## 职责与模块

Debian 12/13 amd64 节点的唯一推荐入口，是 AkastrCloud 后台生成的一行非交互安装命令（同节点重装和修复可重跑，跨节点覆盖与降级会被拒绝，信任边界与验收见 [安装教程](docs/INSTALLATION.md#3-添加节点并执行一键命令) 与 [安装后验收](docs/INSTALLATION.md#5-安装后验收)）。

节点配置由下列模块按需组合（完整字段与消息契约见 [PROTOCOL.md](docs/PROTOCOL.md#节点配置与模块)）：

| 模块键 | 职责 | 前置依赖 / 说明 |
|---|---|---|
| `ip_watch` | 定时观测公网 IPv4 与可选 IPv6 并主动上报 | 无（ChangeIP 启用时强制锁定开启） |
| `changeip` | 调用 HTTP `POST`（仅 `200` 为明确触发）或固定无参本机程序触发换 IP | 需要 `ip_watch` |
| `socks5` | 公布已有 SOCKS5 代理端口与登录供主控派发 IPQuality（使用观测到的公网 IPv4） | Agent 不运行代理 |
| `ipquality_runner` | 执行固定 commit 与 SHA-256 的官方 IPQuality 脚本（单并发，凭据随任务下发不落盘） | 见 [IPQuality Runner](docs/INSTALLATION.md#ipquality-runner) |
| `xui` | 按目标管理本机 3x-ui 的 `ak-` 受管客户端，每分钟上报入站、流量、直传状态与 TCP 连接数 | 3x-ui 与 Xray 版本固定，见 [PROTOCOL.md](docs/PROTOCOL.md#xui) |
| `rate_limit` | 按连接账本在默认路由网卡 `clsact egress` 用 `flower` + `police` 限制下载速率 | 需要 `xui` 与 Xray 访问日志 |
| `connection_records` | 把受管客户端访问日志与连接关闭时的内核字节/请求数分批上报供审计 | 需要 `xui` 与 Xray 访问日志 |
| `routing` | 接管 3x-ui 的 Xray 模板路由与出站、热更新运行中的 Xray 并每 5 分钟测试受管出站 | 需要 `xui` |
| `diagnostics` | 默认关闭；在一次性只读 systemd 沙盒内运行主控转来的 bash 诊断脚本 | 仅在主控开启 AI 授权时执行 |
| `firewall` | 用 `inet akastr_firewall` 筛选本机入站并做端口转发，改表前后端到端探测并保留或回退 | 仅开启转发时按 `boot_id` 开 IPv4 转发 |
| `metrics` | 每分钟只读 `/proc`、`/sys` 与网卡硬件统计上报机器状态与累计流量计数 | 无 |

AkastrCloud 持有所有持久业务决策（Telegram 通知与权限、ChangeIP 计划与 45 分钟会话、IPQuality 每日配额与缓存，重装 Agent 不能绕过，见 [Cloud Carpool 契约](https://github.com/akastrmix/AkastrCloud/blob/main/docs/CARPOOL.md#5-changeip-与-ipquality)）。目标节点按“触发换 IP 后可能立即断网，实际地址只由持续公网 IPv4 观测收敛”的网络模型运行，见 [目标节点网络模型](docs/ARCHITECTURE.md#3-目标节点网络模型) 与 [WSS 协议](docs/PROTOCOL.md#ip-观察changeip-与-ipv4-核对)。ChangeIP 与同一目标的 IPQuality 逻辑互斥；每台 Runner 另有单并发资源限制。Agent 不提供 Telegram channel 播报、离线或资源告警、任意远程命令或浏览器 HTTP-flow ChangeIP。

## 代码结构与联动点

```text
cmd/akastr-agent/       CLI 入口（run、prepare、install、version）
internal/app/           唯一模块清单、依赖校验与装配入口（modules.go）
internal/modules/*      每个功能模块一个自包含目录
internal/               共用底座（主要包，完整职责见架构说明第 4 节）：
  protocol/ transport/ identity   协议编解码、Ed25519 认证与可重连 WSS
  module/ operation/ desired/ lifecycle  五类模块接口、幂等 journal、目标状态与进程租约
  xray/ panel/                    Xray 进程/访问日志/连接账本与 3x-ui 面板客户端
  hostroute/ sockdiag/ netpolicy/ 默认路由网卡、netlink TCP 连接统计与公网 IP 判定
  layout/ state/ install/ update/ A/B slot、原子 JSON 状态、安装与自更新
docs/                   安装手册、架构与协议契约
scripts/                Go 验证、IPQuality 源校验、构建与安装模板
```

**改代码必查联动点**：
- **新增或修改模块/通信契约**：先改 [PROTOCOL.md](docs/PROTOCOL.md) → 在 `internal/modules/<name>/` 实现 `internal/module` 接口（`Commands`/`Desired`/`Reporter`/`Worker`/`Queries`；进程外留状态的须写明正常停止、原地换进程、崩溃、关模块 `Leftovers` 与卸载的清理或接受残留）→ 模块按自己的配置组装自己并导出所需宿主机命令与 Debian 包，`internal/app/modules.go` 只登记解析、收集宿主机依赖并连接跨模块依赖 → 同步 Cloud `core/nodes/{bootstrap,module-summary}.ts` 与 `backend/apps/assembly/index.ts`。
- **修改跨版本升级契约**：`POST /internal/agents/maintenance` 与 CLI `version`/`prepare`/`run` 是旧版升新版的桥梁，改动须按 Cloud ADR 0024 先获批准并说明已装节点迁移方案。
- **修改 IPQuality 固定脚本**：改 `internal/modules/ipqualityrunner/script/pin.go` 的 commit 与 SHA-256 后，须运行 `sh scripts/verify-ipquality-source.sh` 与 Debian 12/13 依赖检查。

## 文档

单模块任务先用 `grep -n '^#' docs/<文件>` 找到对应章节的行号再分段读取，不整篇通读：

- [安装与使用教程](docs/INSTALLATION.md)：安装、重装、验收、维护与故障排查表。
- [架构说明](docs/ARCHITECTURE.md)：进程模型、包职责、本地持久状态与 A/B 自更新。
- [WSS 协议](docs/PROTOCOL.md)：Cloud ↔ Agent HTTPS/WSS 通信契约（wire contract）。

## 文档维护

- 禁止在文档中记流水账或逐项复述本轮改动。只记录真正值得注意的规则、风险、操作前提，或无法从代码直接推导的背景、决策理由与外部约束；普通细节和实现变化由代码、测试及 Git 历史表达，不因完成一次改动就追加文档。
- 现行文档只描述当前有效行为，不保留迁移流水和已取代实现；长期跨仓库决定进入 Cloud ADR，历史变化通过 Git 查询。
- 同一事实只维护一份权威说明；职责由上方文档入口确定，其他位置只保留必要概览和链接，不复制完整字段、状态机、命令或流程。README 负责项目入口、产品边界、目录和验证入口。
- 文档可拆分、合并、重命名或删除，路径不是兼容接口；结构调整不改变未批准的产品或架构语义。只有新的稳定职责无法由现有文档自然承载时才新建权威文档，并明确 authority、更新入口和链接、移除重复内容。

## 本地开发

开发环境使用 Debian 13、Git、Go（低于 `go.mod` 版本时由默认工具链自动拉取，首次需要网络）和 Bash；Debian 依赖回归需要 Docker。2 核 2 GB 开发机上默认单并发、Go 软内存目标 `256MiB`，一次只跑一组验证，不同时跑 Cloud 重型测试（可显式设 `GOMAXPROCS`、`GOMEMLIMIT`）：

```bash
./scripts/verify-go.sh
scripts/build-release.sh vX.Y.Z /path/to/new-output-directory   # 仅输出 akastr-agent-linux-amd64 与 install.sh
```

`build-release.sh` 的输出目录必须尚不存在；项目不发布 ARM binary、独立 `.sha256` 或额外维护脚本。

安装与更新逻辑都在 Go 内（`internal/install`、`internal/update`）并用模拟主控验证，`install.sh` 只负责下载校验并移交程序；`--status` 与 `--uninstall` 同样由该模板资产承载。每次 `push main` 或 PR 由 GitHub Actions 自动运行 `./scripts/verify-go.sh`、对 `scripts/*.sh` 逐个执行 `sh -n`、`scripts/verify-ipquality-source.sh`、构建校验及 Debian 12/13 容器依赖检查。

正式发布统一从 AkastrCloud 的同步发布入口执行，范围、顺序、CI 验真与重跑规则见 [Cloud Agent 发布指南](https://github.com/akastrmix/AkastrCloud/blob/main/docs/UPDATE_GUIDE.md#7-agent-发布)，不在本仓库手工打标签。
