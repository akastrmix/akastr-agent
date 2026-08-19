# Akastr Agent — Agent 工作说明

接手任务先读本文件与 `README.md`，再根据任务只选择一份相关权威文档。先用 `rg` 定位具体代码和说明，不要默认加载整个 `docs/`，避免无关上下文占用 token。

## 产品边界

- Akastr Agent 是 AkastrCloud 服务节点上的唯一受控运行时，不提供 HTTP 控制面或兼容接口。
- AkastrCloud 始终是主控，也是唯一的业务事实来源。Telegram、使用资格、IPQuality 每日限次策略、队列、缓存元数据和用户消息投递均不属于本仓库。
- AkastrCloud 由独立仓库 `akastrmix/AkastrCloud` 维护；跨仓库任务通过 `$local-project-paths` 解析本地位置，不在源码中写死机器路径。
- 修改 bootstrap、enrollment、WSS 认证、capability、operation、IPv4 event 或自动更新契约时，必须同时读取双方 `AGENTS.md` 并验证两边实现。wire contract 以本仓库 `docs/PROTOCOL.md` 为权威；Cloud HTTP 路由、持久状态和业务语义以 AkastrCloud 对应权威文档为准。
- 运行时是一个由 systemd 管理的 Go 单进程。优先使用 Go 标准库；只有第三方依赖具备明确的运行时用途时才可引入。
- Agent 只能执行预先配置、具有明确类型的操作。不得加入远程 shell、任意命令载荷、终端、通用主机监控、通用离线告警或 Telegram channel 投递。

## 能力边界

已实现能力：

- 观察公网 IPv4，以及可选的 IPv6。
- 执行预先配置的 ChangeIP HTTP API 或固定命令 provider。
- 描述已配置的 SOCKS5 端点，但不得暴露凭据。
- 当 installation 被配置为 Runner 时，执行固定版本的 IPQuality 脚本。

明确不在实现范围内的能力：

- ChangeIP HTTP-flow provider。
- Xray 流量与日志观察。
- 长期跑满带宽的策略与限速。

新增能力应作为同级 package 加入。未实现功能不得创建空实现、占位协议字段或臆想式接口。

## 文档职责与防偏移

- 同一事实只允许有一份权威说明。`README.md` 只负责项目入口、产品边界、仓库结构和验证入口；`docs/ARCHITECTURE.md` 负责长期架构与模块职责；`docs/PROTOCOL.md` 负责 HTTPS/WSS 协议契约；`docs/INSTALLATION.md` 负责操作者安装、验收、维护和排障。
- 非权威文档只链接到权威说明，不复制完整流程、命令、字段表、状态表或约束。修改前先判断事实归属，不能为了“方便阅读”在多个文件重复维护。
- 现行文档只描述系统现在如何工作。不得写迁移过程、实施进度、版本阶段、批准流水、已完成事项或被取代方案；历史变化通过 Git commit/diff 查询。只有理解当前安全边界不可缺少的长期原因，才可在权威文档中用最短篇幅说明。
- 文档内容应紧凑并服务当前任务。优先删掉重复、过期和可从代码直接推导的文字；先定位再读取，避免全库读取、长篇复述和无边界扩写，以控制维护成本和模型 token 消耗。
- 具体 release 版本、部署代际和动态生产状态不进入本仓库的长期文档；安装命令由 AkastrCloud 后台生成，协议版本仅在 `docs/PROTOCOL.md` 作为当前契约标识维护。

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
- 行为或边界发生变化时，只更新对应的权威文档；只有项目入口或顶层产品边界变化时才同步修改 README。

## 工作方式

- 保持 Agent 轻量、低资源、少依赖且适合长期运行，并在不增加复杂度的前提下适当优化性能。
- 不写兼容层、临时补丁、启发式兜底或过度防御代码；设计过时或不合理时直接删除，并重构为清晰的现行实现。
- 保持项目结构清晰、模块化、易维护，避免将大量不同组件堆积到一个文件。
- 选择能满足当前需求的最简单实现，避免过度设计、预防性设计等多余复杂度。
- 架构决策面向长期，不接受“先这样以后再换”的临时方案。
