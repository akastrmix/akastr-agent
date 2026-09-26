# Akastr Agent 架构

## 1. 进程模型

每个安装实例只存在一个 `akastr-agent.service` 和一个 Go 主进程。主进程主动连接 AkastrCloud 的 WSS 控制端点，并在内部每六小时检查一次主控批准的更新；节点没有额外 updater service、timer 或常驻辅助进程，也不开放通用管理 HTTP 服务。实例只公布本机 root 所有配置中明确启用的能力。

同一个二进制支持两种部署形态：

- 目标节点：公网 IP 观察、ChangeIP，以及可选的不含秘密的 SOCKS5 端点描述；
- 专用 Runner：IPQuality 执行，`max_concurrency=1`。

运行时能力可以组合，`target` 和 `runner` 不是不同二进制，也不是协议中的永久角色。后台只生成其中一种部署配置：目标节点不执行 IPQuality，专用 Runner 也不承担目标节点能力，避免资源占用和目标网络变化互相影响。

项目只发布 Debian 12/13 amd64 binary 和版本专用的 `install.sh`。节点由后台先创建，得到节点 UUID 与长期机器 token；provider secret 只在以机器 token 加密的持久 bootstrap 里，不以明文进入 PostgreSQL。配置有单调的 desired/applied revision，两者不等时 Cloud 不派发操作；配置更新由主进程自动取回密封 bootstrap 收敛，不需要重跑安装命令。

## 2. 主控边界

AkastrCloud 持有所有持久业务决策。Agent 不知道 Telegram 用户、订阅、每日限额、通知偏好、ChangeIP 预设或自动更换规则。

主控派发一次 IPQuality 前必须同时取得两种资源：

1. 目标节点上没有冲突的 ChangeIP 操作；
2. 对应 Runner 有一个空闲执行槽。

请求合并、次数限制及缓存失效由 [Cloud Carpool 契约](https://github.com/akastrmix/AkastrCloud/blob/main/docs/CARPOOL.md#5-changeip-与-ipquality)定义，Agent 不维护第二套业务缓存。

机器 token 只用于 HTTPS bootstrap 和注册；日常 WSS 与维护通过 Ed25519 身份认证。Cloud 核对当前部署、配置和能力后才允许业务执行。消息按至少一次投递，本地日志和数据库唯一约束共同保证执行与结果幂等；认证格式、消息字段及握手顺序以 [PROTOCOL.md](PROTOCOL.md) 为准。

## 3. 目标节点网络模型

目标节点按动态公网 IPv4 网络设计，而不是按“请求发出后连接保持稳定”的普通 RPC 环境设计。正式支持的运行模型包含以下事实：

- 公网 IPv4 可以在没有 ChangeIP command 时自然变化；此类变化由常驻观察器独立上报。
- ChangeIP provider 只负责触发服务商或本机网络动作。HTTP `200` 或固定程序退出码 `0` 只证明触发被明确接受，不证明地址已经改变。
- provider 被触发后，节点可能立即断网，WSS、HTTP 响应或本地进程结果都可能来不及返回。连接中断不能单独解释为执行失败，也不能据此再次调用 provider。
- 网络恢复后可能获得新 IPv4，也可能仍是原 IPv4。实际结果只由持续的公网 IPv4 观察收敛，不由 provider 返回文本或 WSS 是否及时断开推断。
- WSS 使用相同本地身份自动重连；command、待确认 IPv4 事件和核对状态先持久化，因此消息可以乱序或跨重启送达，而本地网络动作不会重复执行。
- 最近一次有效公网 IPv4 同时是 SOCKS5 公布地址的来源，也是 AkastrCloud 划分 IPQuality 缓存代际的依据；未取得有效公网 IPv4 时不应把节点当作可用代理目标。

典型流程是：Cloud 下发 command → Agent 持久化并触发 provider → 节点可能立即断网 → 网络恢复后 WSS 重连 → IPv4 观察器上报新地址，或确认仍为旧地址 → Cloud 收敛原 ChangeIP session。具体消息与核对契约见 [PROTOCOL.md](PROTOCOL.md)；业务等待窗口、冷却、通知和缓存规则由 AkastrCloud 的 Carpool 契约负责。

WSS 的拨号、认证与试运行提交共用 30 秒建立窗口；会话中每 30 秒发送 Ping，10 秒无 Pong 即断开并按退避重连（稳定 30 秒以上的连接断开后从 1 秒退避重来）。这只管理连接：不取消正在执行的操作，不清除待确认事实，重连后按持久状态重放，**不会因断网再次触发 ChangeIP**。

## 4. 包职责

- `internal/config`：严格读取和验证 Cloud 生成的本地配置；未知字段直接报错。
- `internal/capability`：生成确定性且不含秘密的能力描述。
- `internal/state`：带 schema 标记的原子 JSON 状态持久化。
- `internal/operation`：统一拥有实时执行、终态重放、重启恢复、本地 exclusive group 和有界操作日志；不保存 command payload 或凭据。能力 handler 只提供执行和恢复结果，不各自维护 journal 流程。
- `internal/lifecycle`：command execution 与自动更新共用的进程级 lease；不保存持久业务状态。
- `internal/features/ipwatch`：通过固定 HTTPS 来源独立观察公网 IPv4/IPv6，并持久保存各自尚未确认的事实及活动 ChangeIP 核对状态。
- `internal/providers/changeip`：统一描述明确触发、结果未知和明确失败；`httpcurl` 只接受固定 curl 配置与 HTTP `200`，`command` 不用 shell 解释 payload并以固定 argv 运行本机程序。
- `internal/providers/ipquality/script`：通过秘密 SOCKS5 profile 执行 checksum 固定的 Bash 脚本；执行前后验证代理 IPv4，并有界解析输出。
- `internal/identity`、`internal/protocol`、`internal/transport/ws`：本地 Ed25519 身份和可重连的受控 WSS 通道。
- `internal/bootstrap`：下载持久密封配置，以节点 UUID 作为 AAD 完成认证解密，并生成 root-only 运行文件。
- `internal/autoupdate`：进程内维护循环（见第 8 节）。
- `internal/app`：组合配置与 executor，提供本地空闲检查。
- `internal/daemon`：组织启动检查、WSS、维护循环、trial 和退出；CLI 只负责参数和进程信号。

## 5. 本地操作状态

同一进程的重复调用加入正在进行的执行；只有没有实时执行的 active journal 才按重启恢复处理。每个可执行操作都有一个 exclusive group。引擎在执行前持久化 active 记录，得到终态后再移动到有界 recent 历史。即使主控调度错误，同一组内的第二个操作也会被本地拒绝。

日志只保存 operation ID、kind、exclusive group、时间、状态和稳定终态 code，不保存 payload、SOCKS5 凭据、脚本输出、客户信息或 Telegram 标识。

终态记录只包含重发所需的有界安全结果。Cloud 的 accepted ack 是执行授权：Cloud 已持久 acceptance 但节点尚未收到 ack 就断线时，相同 offer 可在原窗口后重新握手并开始一次本地执行；从未 accepted 的过期 command 得不到授权。进程重启后的 active ChangeIP 记录直接收敛为 `change_trigger_unknown`，其他 active 操作按各自恢复语义终结，provider 都不会再次执行；recent 终态直接重放。未知或损坏的状态 schema 会被拒绝，不会被静默重置。

## 6. 公网 IP 观察

观察器通过固定 HTTPS 来源 `api.ipify.org` 和 Cloudflare trace 获取公网地址，明确按 IPv4 或 IPv6 建立连接，拒绝重定向、非公网地址和过大响应。IPv4 的非公网范围包括 private、loopback、link-local、CGNAT、文档/基准测试、组播和保留网段。

- Target 的 IPv4 watch 始终启用。首次观察把 baseline 与待确认 `ip.snapshot` 一起持久化，后续变化也先持久化再发 `ip.observed`；未获 Cloud 确认前跨重连、重启重发，重复或过期 ACK 不删除新事件。**未确认 IPv4 snapshot 时不接受 ChangeIP。** 重启后若 IPv4 已变，先上报变化再发当前 snapshot，避免与未结束 command 冲突。
- `observe_ipv6=true` 时 IPv6 独立循环；无公网 IPv6 或探测失败只重试，不影响 IPv4 readiness、ChangeIP 或维护空闲判断。
- 观察器出现不可恢复的状态错误时整个 Agent 退出、由 systemd 重启，避免出现“WSS 在线但已停止观察”的半失效进程。

ChangeIP handler 在执行 provider 前把 command、旧 IP 和五分钟核对起点写入同一个 IP 状态文件。HTTP provider 只有收到 `200` 才返回 `change_triggered`；固定程序退出 `0` 也返回该结果。请求可能已经送达但响应、进程或 WSS 被换 IP 断开的情况返回 `change_trigger_unknown`，不会重发 provider。明确的非 `200`、非零退出或启动失败会取消核对并失败。

唯一的常驻 IPv4 monitor 随后负责事实判定：观察到新 IP 时持久上报 `ip.observed`；五分钟宽限后连续三次成功观察仍是旧 IP 时持久上报 `changeip.unchanged`。网络或观察源失败不计次数。两类消息在主控确认前都会跨断线和进程重启重发；主控按 command ID 收敛同一 session，且不要求 `operation.result` 必须先到达。45 分钟兜底只由 AkastrCloud session 持有，Agent 不维护第二个业务计时器。

## 7. SOCKS5 与 IPQuality

目标节点的 capability metadata 只公布端口，不包含地址来源、主机名、用户名或密码。AkastrCloud 始终把该端口与 Agent 最近一次上报的公网 IPv4 组合为 SOCKS5 入口；如果尚无有效公网 IPv4 观测，就不会派发 IPQuality。Runner 上的凭据位于独立 root-only profile 文件，以 AkastrCloud 的稳定 server key 索引。

bootstrap 只接受固定版本、固定摘要的官方 IPQuality 脚本；版本入口见[安装教程](INSTALLATION.md#ipquality-runner)。Release workflow 会在发布前实际下载并验证固定输入与 Debian 依赖声明。Runner 使用指定目标的 SOCKS5 端点运行该脚本；执行前后都会通过 SOCKS5 观察 IPv4，并与任务中的预期目标 IPv4 代际比对。代际在完成前变化时，即使脚本退出成功，AkastrCloud 也不会把结果作为该代际的有效报告。

官方脚本在仅 IPv4 模式下可能生成有效报告 URL，却返回非零 Bash 状态。因此，“输出中包含有界、有效的 `https://report.check.place/...` URL，且代理 postflight 成功”视为完成；非零退出且没有报告 URL 是 `script_failed`。

通过代理运行不等于直接在目标主机运行：依赖 Runner DNS 或直连网络的脚本检查应在验收时识别，并标记或省略。

## 8. Fix-forward 安装与自动更新

**安装器描述期望状态，失败靠重跑同一命令修复（fix-forward），不做本机回滚。**

- 一键命令经官方 HTTPS 短入口取得 Cloud 批准版本的 installer，并以安装码传入节点凭据；发布流程验真 installer，installer 校验 binary 与 IPQuality 摘要。
- 可用于空白主机、同节点覆盖与残缺修复。覆盖前核对已有 identity/config 的节点 ID 与版本：**不同节点或降级直接拒绝**，所有权不明的残留也拒绝。同节点复用 identity（不重新生成私钥），以新 configuration revision 重新 enrollment。
- 停止服务前完成下载、bootstrap、依赖与 maintenance-safe 检查；停止后再核对稳定状态才写入新配置。已证明归属的 revision 可由认证 bootstrap 重建缺失的派生文件，但 bootstrap 摘要不同仍拒绝覆盖；identity 与执行日志不参与重建。
- 主控在有未完成 command、active ChangeIP session 或目标 IPQuality run 时拒绝配置更新和注册。

**自动维护**（不由 GitHub `latest` 驱动，协议细节见 [PROTOCOL.md](PROTOCOL.md#自动维护与配置协调)）：

- 一个串行协调器负责启动检查、六小时定时、失败退避、维护通知与手动重试；网络等待不阻塞进程 ready 或 WSS。
- 先准备（复用或下载不可变 release、严格解析 desired bootstrap），准备期间现有 runtime 仍可接单；准备完成后才取 exclusive update lease，确认本地空闲、Cloud 目标未变且不忙，再进入试运行。复查忙碌或目标变化时 30 秒后重试，不消耗试运行次数。
- `deployments/<version>-r<revision>` 同时引用 binary 与配置。trial 通过 Cloud 校验后，Agent **先原子替换并 fsync `current`，再提交 WSS commit**，Cloud 重验后才推进 applied。
- 试运行次数在 exec 前原子持久计数：首次失败后自动补试一次，然后暂停；管理员“检查更新”授权再试一次，新目标获得新预算，记录损坏拒绝重置。
- 成功后只保留 current、previous 及其引用的 release/configuration；清理失败只告警，不回滚。身份、操作/IP 日志与重试证据不参与回收。
- installer、维护准备、trial 提交与丢弃共用 release root 下 `.maintenance.lock` 内核文件锁（下载期间不占任务执行锁，退出或 exec 自动释放）；清理绝不跟随目录符号链接。

**进程与 service**：唯一主 service 为 `Type=notify`、`ProtectSystem=strict`，只可写状态目录与 release root。依赖损坏导致运行时构建失败时记录 `runtime_initialization_failed`，只保留 HTTPS 维护以便接收修复目标。trial 只在 WSS 提交并收到 `hello.accepted` 后报告 ready，45 秒期限覆盖业务验收。不提供手工 `--update` 或本地回退 CLI；维护身份或部署损坏时用后台一键命令修复。

正式版本由 AkastrCloud 的同步发布入口生成：先验真并发布不可变 Agent 资产，再激活 Cloud 的唯一更新目标。维护外层、身份签名和 candidate CLI 保持稳定，其破坏性变化须单独设计接入方案。

## 9. 节点接入边界

操作者必须从 AkastrCloud 后台生成完整的一键命令，按 [安装与使用教程](INSTALLATION.md) 安装或重装。Agent 不提供 HTTP 控制面；未接入或离线的节点由主控安全拒绝操作。
