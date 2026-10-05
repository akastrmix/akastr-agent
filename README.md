# Akastr Agent

Akastr Agent 是 AkastrCloud 服务节点上的轻量被控程序。它以单个 Go 进程运行，由节点主动建立出站 WSS 连接；主控只能调用节点本地预先配置、带类型的能力，不能下发任意命令或打开远程终端。

> 节点操作只走 Akastr Agent；未接入、离线或暂停的节点会被主控安全拒绝操作。

## 使用模型

Debian 12/13 amd64 节点的唯一推荐入口，是 AkastrCloud 后台为持久节点生成的一行安装命令：

1. 管理员在后台创建目标节点或 IPQuality Runner，并填写全部参数；
2. 操作者完整复制后台的一键命令到节点执行；
3. 按[安装教程](docs/INSTALLATION.md#5-安装后验收)检查服务与业务连接。

安装全程非交互，不需要 Git、Go 或手写配置。命令含长期节点凭据，不要公开；同节点重装和修复可重跑，跨节点覆盖及降级会被拒绝。命令的信任边界、凭据轮换与修复方式见[安装教程](docs/INSTALLATION.md#3-添加节点并执行一键命令)。

目标节点参数包括：

- 公网 IPv4 与可选 IPv6 定时观察；
- HTTP POST + Bearer token 的 ChangeIP provider，或固定本机程序与参数；HTTP provider 只把状态码 `200` 视为明确触发成功，固定程序以退出码 `0` 为成功；
- 已有 SOCKS5 代理的端口与登录（只供主控派发 IPQuality，Agent 不运行代理）；代理地址始终使用 Agent 观测到的公网 IPv4。

Runner 使用主控随任务下发的目标 SOCKS5 登录，执行固定版本、经过摘要校验的官方 IPQuality 脚本，严格单并发，本身不保存代理密码。配置范围与固定版本入口见[安装教程](docs/INSTALLATION.md#ipquality-runner)。

完整步骤、更新、卸载和故障处理见 [安装与使用教程](docs/INSTALLATION.md)。

## 职责边界

Akastr Agent 只负责节点本地执行与观察：

- 观察公网 IPv4，并在启用时独立观察 IPv6；
- 执行本地固定的 ChangeIP provider；
- 在专用 Runner 上执行固定版本、固定 checksum 的 IPQuality 脚本；
- 可选的 `xui` 模块在本机 3x-ui 管理 Cloud 创建的套餐客户端；可与换 IP 同时开启。

AkastrCloud 负责业务编排：

- Telegram 命令、私聊通知、绑定关系和使用资格；
- ChangeIP session、默认延迟 5 分钟、预设/自动计划和持久队列；
- 目标节点与 Runner 的冲突检查和排队；
- IPQuality 每个服务节点每天一次真实执行及缓存复用。

IPQuality 的次数与缓存由 Cloud 管理，重装 Agent 不能绕过；具体规则见 [Cloud Carpool 契约](https://github.com/akastrmix/AkastrCloud/blob/main/docs/CARPOOL.md#5-changeip-与-ipquality)。

目标节点按动态公网 IPv4、触发 ChangeIP 后可能立即断网、恢复后可能换 IP 也可能保持原 IP 的网络模型运行；provider 结果只描述触发，不直接代表地址已经改变。完整假设与收敛流程见 [目标节点网络模型](docs/ARCHITECTURE.md#3-目标节点网络模型)，消息契约见 [WSS 协议](docs/PROTOCOL.md#ip-观察changeip-与-ipv4-核对)。ChangeIP 与同一目标的 IPQuality 逻辑互斥；专用 Runner 另有单并发资源限制。Agent 不提供 Telegram channel 播报、通用离线告警、通用主机监控、任意远程命令或浏览器 HTTP-flow ChangeIP。

## 文档

- [安装与使用教程](docs/INSTALLATION.md)：安装、重装、验收、维护与排障。
- [架构说明](docs/ARCHITECTURE.md)：运行时职责、本地状态与 lifecycle。
- [WSS 协议](docs/PROTOCOL.md)：Cloud ↔ Agent HTTPS/WSS wire contract。

## 文档维护

- 禁止在文档中记流水账或逐项复述本轮改动。只记录真正值得注意的规则、风险、操作前提，或无法从代码直接推导的背景、决策理由与外部约束；普通细节和实现变化由代码、测试及 Git 历史表达，不因完成一次改动就追加文档。
- 现行文档只描述当前有效行为，不保留迁移流水和已取代实现；长期跨仓库决定进入 Cloud ADR，历史变化通过 Git 查询。
- 同一事实只维护一份权威说明；职责由上方文档入口确定，其他位置只保留必要概览和链接，不复制完整字段、状态机、命令或流程。README 负责项目入口、产品边界、目录和验证入口。
- 文档可拆分、合并、重命名或删除，路径不是兼容接口；结构调整不改变未批准的产品或架构语义。只有新的稳定职责无法由现有文档自然承载时才新建权威文档，并明确 authority、更新入口和链接、移除重复内容。

## 仓库结构

```text
cmd/akastr-agent/       CLI 入口
docs/                   架构、协议和安装教程
internal/               运行时实现
scripts/                release 构建与非交互安装模板
```

包职责以[架构说明](docs/ARCHITECTURE.md#4-包职责)为准。

## 本地开发

状态转换、冲突规则、解析/校验和恢复语义的变化须有相应测试覆盖。代码变更交付前运行下面的 Go Gate；Debian 本机与 Linux CI 执行同一个原生 Shell 脚本。纯文档变更检查内容、链接和差异。

开发环境使用 Debian 13、Git、Go、Bash；容器回归还需要可运行的 Docker Engine。需要 Go 1.25 或 `go.mod` 指定的兼容版本。Debian 自带 Go 低于该版本时，由 Go 的默认工具链选择按 `go.mod` 获取所需版本；首次执行需要网络。脚本可以从其他目录调用，输出目录按调用者提供的路径解析。

2 核 2 GB 开发机上，验证默认单并发，并将 Go 的软内存目标设为 256 MiB；这不是进程内存硬上限。运行前检查可用内存，同一时间只运行一组验证，结束后核对测试进程和容器已退出。不要同时运行 Cloud 浏览器、数据库和 Agent 压力测试。需要调整时显式设置 `GOMAXPROCS`、`GOMEMLIMIT`。

运行完整验证：

```bash
./scripts/verify-go.sh
```

安装与更新逻辑都在 Go 里（`internal/install`、`internal/update`），由上面的 Go 测试用模拟主控覆盖；`install.sh` 只负责下载、校验并交给程序。

每次推送到 `main` 或提交 Pull Request，GitHub Actions 都会自动运行 Go 测试、静态检查、构建、shell 语法检查、固定 IPQuality 源校验和 Debian 12/13 Runner 依赖检查。正式发布统一从 AkastrCloud 的同步发布入口执行，范围、顺序、CI 验真和重跑规则见 [Cloud 更新指南](https://github.com/akastrmix/AkastrCloud/blob/main/docs/UPDATE_GUIDE.md#5-发布范围与操作者配置)，不在本仓库手工拆分发布步骤。

本地排查发布构建时可以运行：

```bash
scripts/build-release.sh vX.Y.Z /path/to/new-output-directory
```

输出目录必须尚不存在。脚本只生成：

```text
akastr-agent-linux-amd64
install.sh
```

`install.sh` 是由模板生成的版本专用资产，内部自动验证对应 binary。项目不发布 ARM binary、独立 `.sha256` 或额外维护脚本；同一个文件接受安装码安装、只读 `--status` 和显式确认的 `--uninstall`。

GitHub Release 只是同步发布中的不可变制品阶段；只有后续 AkastrCloud backend 激活成功，节点的更新检查才会看到该版本。同协议版本走普通同步发布；业务协议的破坏性版本使用相同入口加 `--breaking-protocol`，Cloud 短暂只读切换后恢复，节点通过独立的 HTTPS 更新检查自动更新；更新检查本身保持稳定，不维护多套业务协议。
