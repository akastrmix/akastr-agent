# Akastr Agent

Akastr Agent 是 AkastrCloud 服务节点上的轻量被控程序。它以单个 Go 进程运行，由节点主动建立出站 WSS 连接；主控只能调用节点本地预先配置、带类型的能力，不能下发任意命令或打开远程终端。

> 当前迁移状态：AkastrCloud 生产环境的 Agent WSS Gate 仍然关闭，正式节点尚未迁移。在主控端明确启用 Gate 并签发一次性 enrollment token 之前，只能准备配置和依赖，不要执行 `enroll`、`install.sh` 或停用旧 IPChanger。

## 职责边界

Akastr Agent 负责节点本地执行与观察：

- 观察公网 IPv4，并为未来的 IPv6 观察保留配置字段；
- 执行固定 `program` + `args` 的 ChangeIP command provider；
- 上报不含凭据的 SOCKS5 端点描述；
- 在专用 Runner 上校验并执行固定版本、固定 SHA-256 的 IPQuality 脚本。

AkastrCloud 负责业务编排：

- Telegram 命令、私聊通知、绑定关系和使用资格；
- ChangeIP session、默认延迟 5 分钟、预设/自动计划和持久队列；
- 目标节点与 Runner 的冲突检查和排队；
- IPQuality 每个服务节点每天一次真实执行及缓存复用。

IPQuality 的“每天一次”按 `Asia/Hong_Kong` 日历日计算；超过一次直接读取当天缓存。香港时间 `00:00` 或目标节点观测到 IPv4 改变时，主控开启新的缓存代际。此规则由 AkastrCloud 执行，重装 Agent 不能绕过。

ChangeIP 与同一目标的 IPQuality 逻辑互斥；专用 Runner 另有 `max_concurrency=1` 的资源限制。Telegram channel 播报、通用离线告警、通用主机监控、任意远程命令和 HTTP-flow ChangeIP 均不属于 v0.1.0。

## 从这里开始

新手和节点迁移操作者请阅读 [安装、配置与使用教程](docs/INSTALLATION.md)。其中包括：

- Debian 12/13 的依赖准备；
- v0.1.0 下载与 SHA-256 校验；
- 目标节点和专用 IPQuality Runner 的完整配置；
- 一键安装、等价手动安装、enrollment、systemd 运维；
- 更新、回滚、卸载、故障处理和迁移边界。

维护者还应阅读：

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
scripts/                校验下载的一键安装/更新入口
```

未来能力会作为 `internal/features/` 或 `internal/providers/` 下职责单一的同级包加入；仓库不会提前创建空实现。

## 本地开发

需要 Go 1.25 或 `go.mod` 指定的兼容版本：

```bash
go test ./...
go vet ./...
go build ./cmd/akastr-agent
```

检查示例配置并输出不含秘密的能力清单：

```bash
go run ./cmd/akastr-agent check-config --config ./config.example.json
go run ./cmd/akastr-agent capabilities --config ./config.example.json
```

维护者使用下面的命令生成可复现的 Linux amd64/arm64 二进制和 checksum 文件。输出目录必须尚不存在：

```bash
scripts/build-release.sh vX.Y.Z /path/to/new-output-directory
```

release 发布与生产节点迁移是两项独立操作。生成 release 不代表允许启用 WSS Gate 或替换任何节点。
