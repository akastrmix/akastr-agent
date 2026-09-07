# Akastr Agent

Akastr Agent 是 AkastrCloud 服务节点上的轻量被控程序。它以单个 Go 进程运行，由节点主动建立出站 WSS 连接；主控只能调用节点本地预先配置、带类型的能力，不能下发任意命令或打开远程终端。

> 节点操作只走 Akastr Agent；未接入、离线或暂停的节点会被主控安全拒绝操作。

## 使用模型

Debian 12/13 amd64 节点的唯一推荐入口，是 AkastrCloud 后台为持久节点生成的一行安装命令：

1. 管理员在后台创建目标节点或 IPQuality Runner；
2. 管理员在后台填写节点、ChangeIP、SOCKS5 或 Runner profile 的全部参数；
3. 后台创建长期机器 token，并用它加密节点配置；安装命令不直接包含 ChangeIP 或 SOCKS5 secret；
4. 操作者把命令复制到节点执行，安装器全程非交互；
5. Agent 通过 HTTPS 取回并在本机解密配置，随后自动完成依赖、注册、root-only 文件和 systemd service；
6. 同一个节点可以反复执行同一条命令；安装器拒绝跨节点覆盖和版本降级，复用同节点 identity，并以 fix-forward 方式收敛残缺安装；主动轮换 token 后原命令立即失效；
7. 唯一的 `akastr-agent.service` 在 WSS 前、独立 HTTPS 更新通知与定期检查时协调 AkastrCloud 已批准的软件与配置；binary/config 作为一个 deployment 试运行，Cloud 接受 trial 后才提交 `current`，随后完成最终 WSS readiness。

用户不需要安装 Git 或 Go，不需要复制 JSON，不需要创建 token 文件，也不用手工核对 SHA-256。后台生成的命令从固定版本 GitHub Release 完整下载 installer，核对固定 SHA-256 后才执行；不要改用仓库 raw 地址或浮动版本。

目标节点参数包括：

- 公网 IPv4 与可选 IPv6 定时观察；
- HTTP POST + Bearer token 的 ChangeIP provider，或固定本机程序与参数；HTTP provider 只把状态码 `200` 视为明确触发成功，固定程序以退出码 `0` 为成功；
- 不含凭据的 SOCKS5 端口描述；代理地址始终使用 Agent 观测到的公网 IPv4。

Runner 可配置 1–128 个以 server key 索引的 SOCKS5 credential profile。安装器固定下载官方 [xykt/IPQuality](https://github.com/xykt/IPQuality) commit `0ee5f192fed70c04615852efba0e4b8bd43546c7`，自动校验并把并发严格固定为 `max_concurrency=1`。

完整步骤、更新、卸载和故障处理见 [安装与使用教程](docs/INSTALLATION.md)。

## 职责边界

Akastr Agent 只负责节点本地执行与观察：

- 观察公网 IPv4，并在启用时独立观察 IPv6；
- 执行本地固定的 ChangeIP provider；
- 上报不含凭据的 SOCKS5 端点描述；
- 在专用 Runner 上执行固定版本、固定 checksum 的 IPQuality 脚本。

AkastrCloud 负责业务编排：

- Telegram 命令、私聊通知、绑定关系和使用资格；
- ChangeIP session、默认延迟 5 分钟、预设/自动计划和持久队列；
- 目标节点与 Runner 的冲突检查和排队；
- IPQuality 每个服务节点每天一次真实执行及缓存复用。

IPQuality 的“每天一次”按 `Asia/Hong_Kong` 日历日计算；超过一次直接读取当天缓存。香港时间 `00:00` 或目标节点观测到 IPv4 改变时，主控开启新的缓存代际。此规则由 AkastrCloud 执行，重装 Agent 不能绕过。

目标节点按动态公网 IPv4、触发 ChangeIP 后可能立即断网、恢复后可能换 IP 也可能保持原 IP 的网络模型运行；provider 结果只描述触发，不直接代表地址已经改变。完整假设与收敛流程见 [目标节点网络模型](docs/ARCHITECTURE.md#3-目标节点网络模型)，消息契约见 [WSS 协议](docs/PROTOCOL.md#changeip-与-ipv4-核对)。ChangeIP 与同一目标的 IPQuality 逻辑互斥；专用 Runner 另有单并发资源限制。Agent 不提供 Telegram channel 播报、通用离线告警、通用主机监控、任意远程命令或浏览器 HTTP-flow ChangeIP。

## 文档

- [安装与使用教程](docs/INSTALLATION.md)：安装、重装、验收、维护与排障。
- [架构说明](docs/ARCHITECTURE.md)：运行时职责、本地状态与 lifecycle。
- [WSS 协议](docs/PROTOCOL.md)：Cloud ↔ Agent HTTPS/WSS wire contract。

## 文档维护

- README 负责项目入口、产品边界、目录和验证入口，这些事实变化时同步更新。每个事实只维护一份权威说明，其他文档只概览或引用，不复制完整字段、状态机、命令或流程。
- 现行文档只描述当前有效行为，不保留迁移流水和已取代实现；长期跨仓库决定进入 Cloud ADR，历史变化通过 Git 查询。行为变化同步更新对应说明并清理相关旧描述。
- 文档可拆分、合并、重命名或删除，路径不是兼容接口；结构调整不改变未批准的产品或架构语义。只有新的稳定职责无法由现有文档自然承载时才新建权威文档，并明确 authority、更新入口和链接、移除重复内容。
- 文档维护以语义一致性为目标；若存在文档 Gate，体量阈值仅作为异常膨胀信号，不为控制文件数量或字符数删除必要行为、决策理由与恢复说明。

## 仓库结构

```text
cmd/akastr-agent/       CLI 入口
docs/                   架构、协议和安装教程
internal/app/           应用组装与空闲检查
internal/daemon/        启动、维护与退出协调
internal/capability/    不含秘密的能力注册表
internal/config/        严格 JSON 配置
internal/operation/     有界操作日志和能力内互斥
internal/lifecycle/     command 与自动更新的进程级互斥
internal/state/         原子状态文件
internal/features/      节点能力实现
internal/providers/     固定本地 provider
internal/identity/      Ed25519 enrollment 身份
internal/bootstrap/     密封配置下载、验证和本地 root-only 文件生成
internal/autoupdate/    主控批准清单、六小时循环、版本校验和原子切换
internal/protocol/      WSS 协议模型
internal/transport/ws/  带认证和重连的控制连接
scripts/                release 构建与非交互安装模板
```

## 本地开发

状态转换、冲突规则、解析/校验和恢复语义的变化须有相应测试覆盖。代码变更交付前运行下面的 Go Gate；Linux CI 使用 `pwsh` 执行同一脚本。纯文档变更检查内容、链接和差异。

需要 Go 1.25 或 `go.mod` 指定的兼容版本：

```bash
powershell.exe -NoProfile -ExecutionPolicy Bypass -File scripts\verify-go.ps1
```

修改 installer 或安装状态转换时，在本机 Docker/WSL 运行 Debian 12/13 回归，覆盖首次安装、同节点覆盖、残缺/failed 状态、失败重跑、Target/Runner 与卸载收敛；容器不连接 Cloud，也不使用真实 token：

```bash
for version in 12 13; do
  docker run --rm \
    -e AKASTR_INSTALLER_CONTAINER_TEST=1 \
    --volume "$PWD:/source:ro" \
    --workdir /source \
    "debian:${version}-slim" \
    sh scripts/test-installer-container.sh
done
```

每次推送到 `main` 或提交 Pull Request，GitHub Actions 都会自动运行 Go 测试、静态检查、构建、shell 语法检查和 Debian 12/13 installer 容器回归。正式发布统一从 AkastrCloud 的同步发布入口执行，范围、顺序、CI 验真和重跑规则见 [Cloud 更新指南](https://github.com/akastrmix/AkastrCloud/blob/main/docs/UPDATE_GUIDE.md#5-发布范围)，不在本仓库手工拆分发布步骤。

本地排查发布构建时可以运行：

```bash
scripts/build-release.sh vX.Y.Z /path/to/new-output-directory
# Windows PowerShell：
scripts/build-release.ps1 vX.Y.Z C:\path\to\new-output-directory
```

输出目录必须尚不存在。脚本只生成：

```text
akastr-agent-linux-amd64
install.sh
```

`install.sh` 是由模板生成的版本专用资产，内部自动验证对应 binary。项目不发布 ARM binary、独立 `.sha256` 或额外维护脚本；同一个文件只提供可重复的 `--install`、只读 `--status` 和显式确认的 `--uninstall`。

GitHub Release 只是同步发布中的不可变制品阶段；只有后续 AkastrCloud backend 激活成功，主进程的独立维护协调才会看到该版本。同协议版本走普通同步发布；业务协议的破坏性版本使用相同入口加 `-BreakingProtocol`，Cloud 短暂只读切换后恢复，节点通过独立 HTTPS 维护通道自动更新；维护契约本身保持稳定，不维护多套业务协议。
