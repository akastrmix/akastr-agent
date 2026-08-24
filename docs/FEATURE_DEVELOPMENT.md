# Akastr Feature 开发规范

本文只规定如何判断、设计和验证一个新增或变更的 feature。它不批准任何具体功能，也不复制现行协议、业务状态机或数据库结构。Agent 产品边界与模块职责以 [ARCHITECTURE.md](ARCHITECTURE.md) 为准，HTTPS/WSS 消息以 [PROTOCOL.md](PROTOCOL.md) 为准；Cloud 的业务、持久状态和 HTTP 语义仍由 AkastrCloud 对应权威文档负责。

## 1. 先检查边界，再分类

feature 必须先满足两个条件：存在当前真实需求，并且没有落入两个仓库 `AGENTS.md` 的禁止范围。分类只决定实现和验证方式，不代表功能已经获准；未获批准的能力不得预建字段、接口、package 或兼容路径。

每个 feature 选择一个主类型，混合需求按真实职责拆开：

- **observation**：Agent 主动产生 typed fact；待确认状态、重放和丢弃策略必须有界，采集失败不得阻塞无关能力。
- **typed operation**：Cloud 创建带稳定 ID 的命令，Agent 只调用本地预配置实现；必须区分明确失败与结果未知，不得自动重做可能已发生的副作用。
- **configuration**：Cloud 持有 desired revision，Agent 严格验证 candidate，并只在 trial/commit 完成后切换 current；不得为每个 feature 建立独立热重载框架。
- **artifact**：Cloud 批准不可变版本、受限 URL、摘要和大小，Agent 校验后保存并按引用清理；artifact 不得成为第二份业务配置真相。

## 2. 变更路由

| 变化 | 权威归属与要求 |
|---|---|
| Agent 本地执行、provider、本地有界状态 | Agent `ARCHITECTURE.md`；实现放入职责单一的同级 package |
| bootstrap、enrollment、WSS、capability、operation、IP event、自动更新 | Agent `PROTOCOL.md`；Cloud 与 Agent 必须成对实现、测试和验证 |
| Cloud 业务、HTTP、PostgreSQL、job/outbox | AkastrCloud 对应权威文档；Agent 不复制业务真相 |
| installer、systemd、依赖或 writable path | Agent 安装边界；按变更范围执行 Debian 12/13 回归 |
| 生产 schema、持久 payload、认证、secret 或发布边界 | 实施前按 AkastrCloud ADR 0024 提交具体方案并获得操作者批准 |

## 3. 实现前的六项记录

行为发生变化时，提交说明必须回答：

1. 真实需求、成功标准和明确非目标；
2. feature 分类，以及 Cloud/Agent 各自持有的事实；
3. 本地状态、副作用、幂等、重启与结果未知如何处理；
4. 条数、字节、时间、并发、CPU、内存或磁盘等适用上限；
5. binary、configuration、wire、OS 与发布顺序是否变化；
6. 哪些测试证明正常、失败、重复、乱序和恢复行为。

不改变行为的局部重构只需说明边界未变，不强制填写无关项目。

## 4. 版本与状态兼容

- 相同 WSS protocol 下，Cloud 只支持当前 target 和紧邻它的前一个正式 release。enrollment/trial 只接受 target；前一 release 只能通过现行维护 trial 升级到 target。
- 更旧、跳版或高于 target 的 Agent 不自动兼容；Cloud 拒绝其连接或维护请求，操作者重新运行当前一键安装命令。
- WSS protocol 变化不属于普通自动更新。维护窗口先排空业务，再切换现行单协议并逐节点重装；不得为此保留双协议或跨协议自动更新路径。
- 未知、损坏或未来本地 state schema 继续 fail-closed。只有实际修改 schema 时，才为前一个正式 release 的真实 fixture 实现一次确定性迁移；无法安全迁移时使用获批维护重装，不保留更早 reader。

## 5. 条件式完成标准

- wire、bootstrap、capability 或更新契约变化：两端使用内容完全一致的正常/异常 fixture，并分别通过严格解析测试。
- 本地持久状态变化：覆盖前一正式 release 直接升级、损坏/未来 schema 拒绝、重启恢复和副作用不重复。
- Cloud 事务、锁或唯一约束变化：覆盖并发交错；依赖真实 PostgreSQL 语义时运行 PostgreSQL integration test。
- provider、依赖、systemd 或安装路径变化：运行 Agent 完整门禁和 Debian 12/13 installer 回归。
- observation 或批量输入变化：验证本地上限、Cloud 不可用、持续输入、丢弃顺序和恢复发送。
- 其他行为变化：运行所属 package 的定向测试，再运行两个仓库各自要求的完整门禁。

没有对应变化时不创建占位 fixture、migration、后台任务、抽象层或额外测试框架。
