# Akastr Agent

Akastr Agent 是 AkastrCloud 服务节点上的轻量被控程序。它以单个 Go 进程运行，由节点主动建立出站 WSS 连接；主控只能调用节点本地预先配置、带类型的能力，不能下发任意命令或打开远程终端。

> 当前迁移状态：AkastrCloud 生产环境的 Agent WSS Gate 仍然关闭，正式节点尚未迁移。后台即使已经展示 Agent 管理和安装命令，在主控操作者明确开放 Gate 以前也不要执行，不要停用旧 IPChanger。

## v0.2.0 使用模型

Debian 12/13 amd64 节点的唯一推荐入口，是 AkastrCloud 后台为某个 installation 生成的一行安装命令：

1. 管理员在后台创建目标节点或 IPQuality Runner；
2. 后台生成 installation UUID、一次性 enrollment token 和预填安装命令；
3. 操作者把命令复制到节点的交互式终端；
4. 安装器静默读取一次性 token，再用中文向导收集该节点配置；
5. 安装器自动安装依赖、下载并校验 binary、完成 enrollment、写入 root-only 文件并启动 systemd service。

用户不需要安装 Git 或 Go，不需要复制 JSON，不需要创建 token 文件，也不需要手工下载或核对 SHA-256。不要从仓库 raw 地址直接运行模板脚本，也不要使用 `curl | sh`。

目标节点向导可配置：

- 公网 IPv4 定时观察；
- HTTP POST + Bearer token 的 ChangeIP provider，或固定本机程序与参数；
- 不含凭据的 SOCKS5 地址来源和端口描述。

Runner 向导可配置 1–128 个以 server key 索引的 SOCKS5 credential profile。安装器固定下载官方 [xykt/IPQuality](https://github.com/xykt/IPQuality) commit `0ee5f192fed70c04615852efba0e4b8bd43546c7`，自行校验内嵌 SHA-256，并把并发严格固定为 `max_concurrency=1`。

完整步骤、更新、卸载和迁移边界见 [安装与使用教程](docs/INSTALLATION.md)。

## 职责边界

Akastr Agent 只负责节点本地执行与观察：

- 观察公网 IPv4；
- 执行本地固定的 ChangeIP provider；
- 上报不含凭据的 SOCKS5 端点描述；
- 在专用 Runner 上执行固定版本、固定 checksum 的 IPQuality 脚本。

AkastrCloud 负责业务编排：

- Telegram 命令、私聊通知、绑定关系和使用资格；
- ChangeIP session、默认延迟 5 分钟、预设/自动计划和持久队列；
- 目标节点与 Runner 的冲突检查和排队；
- IPQuality 每个服务节点每天一次真实执行及缓存复用。

IPQuality 的“每天一次”按 `Asia/Hong_Kong` 日历日计算；超过一次直接读取当天缓存。香港时间 `00:00` 或目标节点观测到 IPv4 改变时，主控开启新的缓存代际。此规则由 AkastrCloud 执行，重装 Agent 不能绕过。

ChangeIP 与同一目标的 IPQuality 逻辑互斥；专用 Runner 另有单并发资源限制。Telegram channel 播报、通用离线告警、通用主机监控、任意远程命令和 HTTP-flow ChangeIP 均不属于 v0.2.0。

## 文档

- [安装与使用教程](docs/INSTALLATION.md)
- [架构说明](docs/ARCHITECTURE.md)
- [WSS 协议](docs/PROTOCOL.md)

## 仓库结构

```text
cmd/akastr-agent/       CLI 入口
docs/                   架构、协议和安装教程
internal/app/           应用组装
internal/capability/    不含秘密的能力注册表
internal/config/        严格 JSON 配置
internal/operation/     本地互斥与有界操作日志
internal/state/         原子状态文件
internal/features/      节点能力实现
internal/providers/     固定本地 provider
internal/identity/      Ed25519 enrollment 身份
internal/protocol/      WSS 协议模型
internal/transport/ws/  带认证和重连的控制连接
scripts/                release 构建与交互式 bootstrap 模板
```

未来能力会作为 `internal/features/` 或 `internal/providers/` 下职责单一的同级包加入；仓库不会提前创建空实现。

## 本地开发

需要 Go 1.25 或 `go.mod` 指定的兼容版本：

```bash
go test ./...
go vet ./...
go build ./cmd/akastr-agent
```

维护者生成 v0.2.0 release 资产：

```bash
scripts/build-release.sh vX.Y.Z /path/to/new-output-directory
```

输出目录必须尚不存在。脚本只生成：

```text
akastr-agent-linux-amd64
install.sh
```

`install.sh` 是由模板生成的版本专用资产，内嵌 release version 和 binary SHA-256。v0.2.0 不发布 ARM binary、独立 `.sha256` 或 `update.sh`。同一个生成版 `install.sh` 负责新装、更新、查看状态和卸载。

release 发布与生产节点迁移是两项独立操作。生成 release 不代表允许启用 WSS Gate 或替换任何节点。
