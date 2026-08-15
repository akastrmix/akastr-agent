# Akastr Agent

Akastr Agent 是 AkastrCloud 服务节点上的轻量被控程序。它以单个 Go 进程运行，由节点主动建立出站 WSS 连接；主控只能调用节点本地预先配置、带类型的能力，不能下发任意命令或打开远程终端。

> 正式节点必须逐台迁移。安装成功前保留旧 IPChanger；主控验收并切换该节点路由前，也不要停用旧服务。

## v0.3.3 使用模型

Debian 12/13 amd64 节点的唯一推荐入口，是 AkastrCloud 后台为某个 installation 生成的一行安装命令：

1. 管理员在后台创建目标节点或 IPQuality Runner；
2. 管理员在后台填写节点、ChangeIP、SOCKS5 或 Runner profile 的全部参数；
3. 后台把长期 secret 加密为短期密封配置，只生成一行带一次性 token 的命令；
4. 操作者把命令复制到节点执行，安装器不再询问任何参数；
5. Agent 通过 HTTPS 取回并在本机解密配置，随后自动完成依赖、enrollment、root-only 文件和 systemd service。

用户不需要安装 Git 或 Go，不需要复制 JSON，不需要创建 token 文件，也不需要手工下载或核对 SHA-256。不要从仓库 raw 地址直接运行模板脚本，也不要把 wget/curl 的网络输出直接通过管道交给 shell。

目标节点参数包括：

- 公网 IPv4 定时观察；
- HTTP POST + Bearer token 的 ChangeIP provider，或固定本机程序与参数；
- 不含凭据的 SOCKS5 地址来源和端口描述。

Runner 可配置 1–128 个以 server key 索引的 SOCKS5 credential profile。安装器固定下载官方 [xykt/IPQuality](https://github.com/xykt/IPQuality) commit `0ee5f192fed70c04615852efba0e4b8bd43546c7`，自动校验并把并发严格固定为 `max_concurrency=1`。

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

ChangeIP 与同一目标的 IPQuality 逻辑互斥；专用 Runner 另有单并发资源限制。Telegram channel 播报、通用离线告警、通用主机监控、任意远程命令和 HTTP-flow ChangeIP 均不属于 v0.3.3。

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
internal/bootstrap/     密封配置下载、验证和本地 root-only 文件生成
internal/protocol/      WSS 协议模型
internal/transport/ws/  带认证和重连的控制连接
scripts/                release 构建与非交互安装模板
```

未来能力会作为 `internal/features/` 或 `internal/providers/` 下职责单一的同级包加入；仓库不会提前创建空实现。

## 本地开发

需要 Go 1.25 或 `go.mod` 指定的兼容版本：

```bash
go test ./...
go vet ./...
go build ./cmd/akastr-agent
```

每次推送到 `main` 或提交 Pull Request，GitHub Actions 都会自动运行 Go 测试、静态检查、构建和 shell 语法检查。正式发布只需从已经验证并合入 `main` 的提交创建并推送语义化版本标签：

```bash
git tag -a vX.Y.Z -m "Akastr Agent vX.Y.Z"
git push origin vX.Y.Z
```

标签必须严格符合 `vX.Y.Z`。发布工作流会再次运行完整验证，只构建 Linux amd64 binary 与该版本 `install.sh`，验证版本、架构和资产集合后，自动创建 GitHub Release。构建或验证失败时不会创建 Release，也不会覆盖已经发布的同名版本；因此旧下载地址始终不可变，新版本下载地址在工作流成功后自动可用。

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

`install.sh` 是由模板生成的版本专用资产，内部自动验证对应 binary。项目不发布 ARM binary、独立 `.sha256` 或额外维护脚本；同一个文件以 `--install`、`--update`、`--status` 和显式确认的 `--uninstall` 处理生命周期。

自动发布与生产节点迁移是两项独立操作。Release 成功不代表允许启用 WSS Gate、让 AkastrCloud 改用新版本或替换任何节点。
