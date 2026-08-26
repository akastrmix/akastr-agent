# Akastr Agent — 仓库约束

接手任务先读本文件与 `README.md`，再按任务只读取必要的权威文档。优先用 `rg` 定位代码和说明，不默认加载整个 `docs/`。

## 1. 产品与仓库边界

- Akastr Agent 是部署在服务节点上的受控 node runtime；AkastrCloud 是 control plane 与跨节点业务事实来源。
- Agent 只负责本机 identity、配置物化、观察、typed operation、本地 durable recovery、provider、安装/systemd 与自动维护；Telegram、用户资格、业务队列、缓存策略和跨节点调度不属于本仓库。
- Cloud × Agent 的 ownership 与跨仓库路由以 AkastrCloud `docs/AGENT_INTEGRATION.md` 为总地图。跨仓库任务可用 `$local-project-paths` 定位两个仓库。
- 共享 wire contract 以本仓库 `docs/PROTOCOL.md` 为权威；Cloud HTTP、PostgreSQL 与业务语义由 Cloud 对应权威文档负责。
- Agent 是 systemd 管理的单 Go 进程，只主动建立出站控制连接，不提供 HTTP 控制面、远程终端或任意主机执行入口。
- Agent 应保持轻量、低资源、少依赖且适合长期运行，并在不增加复杂度的前提下适当优化性能。

## 2. 文档职责与自动维护

- `README.md`：项目入口、产品边界、目录与验证入口。
- `docs/ARCHITECTURE.md`：当前长期运行时架构、模块职责、本地状态与 lifecycle。
- `docs/PROTOCOL.md`：Cloud ↔ Agent HTTPS/WSS wire contract。
- `docs/INSTALLATION.md`：安装、重装、验收、维护与排障。
- 同一事实只维护一份权威说明；其他文档引用而不复制完整字段、状态机、命令或流程。
- 长期跨仓库架构决定记录在 AkastrCloud `docs/ADR/`；现行文档只描述当前有效行为，不保留 rollout 流水、迁移过程、已取代实现或历史版本说明。
- 修改产品行为、架构职责、持久状态、协议、配置、安装/升级或恢复语义时，交付前必须做 documentation impact review：先列出本次实际改变的事实，为每个事实找到唯一权威文档，更新后再搜索仓库中的旧描述并删除、修正或改为引用。
- 文档结构本身允许演进。只有出现新的、稳定且长期内聚的责任，而现有权威文档无法自然承载时，才新建权威文档；新建后必须明确其 authority 边界、更新入口/文档地图与相关链接，并从其他文档移除被它接管的重复事实。
- 为保持职责清晰，可以拆分、合并、重命名或删除文档；文档路径不是兼容接口。结构调整不得借机改变未批准的产品或架构语义。
- 文档维护以语义一致性为目标，不以文件数量或字符数为目标。若存在文档 CI/Gate，体量阈值只作为异常膨胀信号，不能成为删除必要当前行为或决策理由的依据。

## 3. 安全、执行与状态边界

- Agent 只能执行本机预配置、协议明确允许的 typed operation；不得加入远程 shell、任意 argv/script/URL、通用终端或开放式主机控制能力。
- secret 只存在 Git 外、root-only 的本地文件；不得进入 capability、普通日志、测试 fixture、错误正文或 wire payload（协议明确需要的受控 credential 流程除外）。
- 配置严格解析并拒绝未知字段；配置型 provider 不经 `/bin/sh -c`。
- 本地持久状态必须有界；schema 未知、状态损坏或无法证明安全恢复时 fail closed，不静默重置可能涉及副作用的状态。
- ChangeIP 与目标 IPQuality 按当前协议/业务模型互斥；Runner 并发是独立资源约束。
- 任何可能导致不可重复副作用再次执行的恢复路径都必须由稳定幂等键和 durable state 约束。

## 4. 跨仓库与高风险变更

- 修改 bootstrap、enrollment、WSS authentication/hello/readiness、capability、operation wire、IP event/ACK、maintenance/configuration/update/trial contract 时，必须读取 Cloud `docs/AGENT_INTEGRATION.md` 和双方相关权威文档，并验证两边实现。
- **paired review 不等于 paired edit**：wire 未变时只修改真正拥有该行为的一侧。
- 生产 schema、持久契约、认证/secret、安全边界或发布模型的高风险变化遵循 Cloud ADR 0024；真实批准不能由代码注释、ADR 或 AI 自行推定。
- 值得长期保存的跨仓库职责、持久契约、安全和升级决定统一记录在 Cloud `docs/ADR/`，不要在 Agent 新建第二套 ADR 注册表。

## 5. 实现与验证

- package 职责应与 `docs/ARCHITECTURE.md` 一致；新增 feature 优先放入清晰的同级 package，不创建空实现、占位 wire 字段或未被真实需求使用的框架。
- 新增 feature 前先定位可复用的现有 package、协议、测试与相近 feature 接入方式，并按需参考成熟实现；只有现有边界确实无法自然承载时才新增机制，不用固定模板限制具体设计。
- 修改状态转换、冲突规则、解析/校验、恢复语义时必须补相应测试。
- 修改 installer 或安装状态转换时，运行 `scripts/test-installer-container.sh` 的 Debian 12/13 容器回归，覆盖首次安装、同节点覆盖、残缺/failed 状态、失败重跑、Target/Runner 与卸载收敛。
- 交付前运行 `powershell.exe -NoProfile -ExecutionPolicy Bypass -File scripts\verify-go.ps1`；Linux CI 使用 `pwsh` 执行同一 Gate。
- 行为或边界变化只更新对应权威文档；README 仅在入口或顶层产品边界变化时修改。
