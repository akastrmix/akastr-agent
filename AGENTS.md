# Akastr Agent — Agent 工作说明

## 产品边界

- Akastr Agent 是一个全新项目。不要复制旧 IPChanger 运行时，也不要以兼容层形式保留它的 HTTP 契约。
- AkastrCloud 始终是主控，也是唯一的业务事实来源。Telegram、使用资格、IPQuality 每日限次策略、队列、缓存元数据和用户消息投递均不属于本仓库。
- 已退役的 IPChanger 仓库只作为归档历史保留。不得将其用于部署、恢复或兼容实现。
- 运行时是一个由 systemd 管理的 Go 单进程。优先使用 Go 标准库；只有第三方依赖具备明确的运行时用途时才可引入。
- Agent 只能执行预先配置、具有明确类型的操作。不得加入远程 shell、任意命令载荷、终端、通用主机监控、通用离线告警或 Telegram channel 投递。

## 当前能力与预留能力

首版能力：

- 观察公网 IPv4，以及可选的 IPv6。
- 执行预先配置的 ChangeIP 命令 provider。
- 描述已配置的 SOCKS5 端点，但不得暴露凭据。
- 当 installation 被配置为 Runner 时，执行固定版本的 IPQuality 脚本。

以下只是扩展点，不属于首版实现：

- ChangeIP HTTP-flow provider。
- Xray 流量与日志观察。
- 长期跑满带宽的策略与限速。

未来功能应作为同级 package 加入。不要创建空实现、占位协议字段或臆想式接口。

## 安全与契约

- secret 只能存放在 Git 之外、仅 root 可读的文件中。不得将凭据写入能力清单、操作日志、普通日志、测试或示例配置。
- 网络控制只允许使用出站 WSS。认证方式和消息字段必须与 AkastrCloud 端一起获得批准后才能实现。
- 任何生产 schema、持久队列或 payload、认证、secret 流程或发布边界的变更，都必须先按照 AkastrCloud ADR 0024 提交方案，并在获得操作者明确批准后实施。
- Agent 本地状态只能包含有界的操作元数据。状态损坏或 schema 未知时必须安全失败，不得静默重置。
- 对同一个目标节点，ChangeIP 与 IPQuality 在逻辑上互斥。Runner 并发限制是另一项独立的资源约束。

## 开发要求

- package 应保持小而清晰，并与 `docs/ARCHITECTURE.md` 的职责划分一致。
- 使用固定参数向量；配置型 provider 不得调用 `/bin/sh -c`。
- 严格校验配置，并拒绝未知字段。
- 每个状态转换、冲突规则和解析行为变更都必须增加测试。
- 交付前运行 `go test ./...`、`go vet ./...` 和 `go build ./cmd/akastr-agent`。
- 行为或边界发生变化时，更新 README 和对应的权威设计文档。
