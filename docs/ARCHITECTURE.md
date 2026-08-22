# Akastr Agent 架构

## 1. 进程模型

每个安装实例只存在一个 `akastr-agent.service` 和一个 Go 主进程。主进程主动连接 AkastrCloud 的 WSS 控制端点，并在内部每六小时检查一次主控批准的更新；节点没有额外 updater service、timer 或常驻辅助进程，也不开放通用管理 HTTP 服务。实例只公布本机 root 所有配置中明确启用的能力。

同一个二进制支持两种部署形态：

- 目标节点：公网 IP 观察、ChangeIP，以及可选的不含秘密的 SOCKS5 端点描述；
- 专用 Runner：IPQuality 执行，`max_concurrency=1`。

运行时能力可以组合，`target` 和 `runner` 不是不同二进制，也不是协议中的永久角色。后台只生成其中一种部署配置：目标节点不执行 IPQuality，专用 Runner 也不承担目标节点能力，避免资源占用和目标网络变化互相影响。

项目只发布 Debian 12/13 amd64 binary 和版本专用的 `install.sh`。AkastrCloud 后台先创建持久节点，再生成节点 UUID 与长期机器 token。机器 token 的 hash 用于认证，可恢复副本由主控 wrapping key 认证加密；provider secret 只存在于以机器 token 加密的持久 bootstrap 中，不以明文进入 PostgreSQL。配置有单调递增的 desired/applied revision；两者不相等时 Cloud 不派发操作。配置更新保持节点角色、服务器绑定、token 与 identity，只替换密封 bootstrap 并要求重跑安装命令。后台短命令以一个 HTTPS curl 从固定 release 取得 installer；安装器拒绝跨节点覆盖和版本降级，同节点复用 identity，残缺状态通过重跑原命令 fix-forward。Runner 依赖齐全时不运行 apt，固定脚本摘要正确时不重复下载。

## 2. 主控边界

AkastrCloud 持有所有持久业务决策。Agent 不知道 Telegram 用户、订阅、每日限额、通知偏好、ChangeIP 预设或自动更换规则。

主控派发一次 IPQuality 前必须同时取得两种资源：

1. 目标节点上没有冲突的 ChangeIP 操作；
2. 对应 Runner 有一个空闲执行槽。

相同目标、香港日历日及 IPv4 代际的请求合并为一次真实执行，之后返回缓存报告。香港时间跨日或观测到 IPv4 改变时，主控创建新的缓存代际。

协议 `2026-08-20.v5` 使用有效期 15 秒的服务端 nonce，以及绑定上下文、以换行分隔的 Ed25519 签名文本。机器 token 只用于 HTTPS bootstrap 和注册；WSS hello 同时绑定本地 configuration revision，Cloud 只允许 desired/applied 精确收敛的身份进入 ready；只读更新检查使用有效公钥身份。offer、accept、终态结果、结果确认、初始 IPv4 snapshot 和自然 IPv4 变化均使用稳定 UUID。消息按至少一次投递，本地日志和数据库唯一约束共同保证执行与结果幂等。

## 3. 目标节点网络模型

目标节点按动态公网 IPv4 网络设计，而不是按“请求发出后连接保持稳定”的普通 RPC 环境设计。正式支持的运行模型包含以下事实：

- 公网 IPv4 可以在没有 ChangeIP command 时自然变化；此类变化由常驻观察器独立上报。
- ChangeIP provider 只负责触发服务商或本机网络动作。HTTP `200` 或固定程序退出码 `0` 只证明触发被明确接受，不证明地址已经改变。
- provider 被触发后，节点可能立即断网，WSS、HTTP 响应或本地进程结果都可能来不及返回。连接中断不能单独解释为执行失败，也不能据此再次调用 provider。
- 网络恢复后可能获得新 IPv4，也可能仍是原 IPv4。实际结果只由持续的公网 IPv4 观察收敛，不由 provider 返回文本或 WSS 是否及时断开推断。
- WSS 使用相同本地身份自动重连；command、待确认 IPv4 事件和核对状态先持久化，因此消息可以乱序或跨重启送达，而本地网络动作不会重复执行。
- 最近一次有效公网 IPv4 同时是 SOCKS5 公布地址的来源，也是 AkastrCloud 划分 IPQuality 缓存代际的依据；未取得有效公网 IPv4 时不应把节点当作可用代理目标。

典型流程是：Cloud 下发 command → Agent 持久化并触发 provider → 节点可能立即断网 → 网络恢复后 WSS 重连 → IPv4 观察器上报新地址，或确认仍为旧地址 → Cloud 收敛原 ChangeIP session。具体消息与核对契约见 [PROTOCOL.md](PROTOCOL.md)；业务等待窗口、冷却、通知和缓存规则由 AkastrCloud 的 Carpool 契约负责。

## 4. 包职责

- `internal/config`：严格读取和验证操作者配置；未知字段直接报错。
- `internal/capability`：生成确定性且不含秘密的能力描述。
- `internal/state`：带 schema 标记的原子 JSON 状态持久化。
- `internal/operation`：本地 exclusive group 和有界操作日志；不保存 command payload 或凭据。
- `internal/lifecycle`：command execution 与自动更新共用的进程级 lease；不保存持久业务状态。
- `internal/features/ipwatch`：通过固定 HTTPS 来源观察公网 IP，并持久保存尚未确认的 IPv4 事件和活动 ChangeIP 核对状态。
- `internal/providers/changeip`：统一描述明确触发、结果未知和明确失败；`httpcurl` 只接受固定 curl 配置与 HTTP `200`，`command` 不用 shell 解释 payload并以固定 argv 运行本机程序。
- `internal/providers/ipquality/script`：通过秘密 SOCKS5 profile 执行 checksum 固定的 Bash 脚本；执行前后验证代理 IPv4，并有界解析输出。
- `internal/identity`、`internal/protocol`、`internal/transport/ws`：本地 Ed25519 身份和可重连的受控 WSS 通道。
- `internal/bootstrap`：下载持久密封配置，以节点 UUID 作为 AAD 完成认证解密，并生成 root-only 运行文件。
- `internal/autoupdate`：主进程内六小时循环，签名请求 Cloud 批准清单，只接受同一 WSS 协议的前向语义版本，并完成有界下载、内部 digest 校验和不可变 release 切换。
- `internal/app`：组合配置、executor 与运行时入口。

新增能力只有在行为得到批准后才能实现，并应使用职责单一的同级包。未实现能力不创建目录、空接口或占位协议字段。

## 5. 本地操作状态

每个可执行操作都有一个 exclusive group。引擎在执行前持久化 active 记录，得到终态后再移动到有界 recent 历史。即使主控调度错误，同一组内的第二个操作也会被本地拒绝。

日志只保存 operation ID、kind、exclusive group、时间、状态和稳定终态 code，不保存 payload、SOCKS5 凭据、脚本输出、客户信息或 Telegram 标识。

终态记录只包含重发所需的有界安全结果。Cloud 的 accepted ack 是执行授权：Cloud 已持久 acceptance 但节点尚未收到 ack 就断线时，相同 offer 可在原窗口后重新握手并开始一次本地执行；从未 accepted 的过期 command 得不到授权。进程重启后的 active ChangeIP 记录直接收敛为 `change_trigger_unknown`，其他 active 操作按各自恢复语义终结，provider 都不会再次执行；recent 终态直接重放。未知或损坏的状态 schema 会被拒绝，不会被静默重置。

## 6. 公网 IP 观察

观察器通过固定 HTTPS 来源 `api.ipify.org` 和 Cloudflare trace 获取公网地址，明确按 IPv4 或 IPv6 建立连接，拒绝重定向、非公网地址和过大响应。IPv4 的非公网范围包括 private、loopback、link-local、CGNAT、文档/基准测试、组播和保留网段。

后台只生成 IPv4 watch：首次成功观察先把 baseline 与待确认 `ip.snapshot` 一起写入 `ip_state_file`，Cloud 持久确认前跨重连、重启重发；后续变化同样先持久化，再发送 `ip.observed`。未确认 snapshot 时不接受 ChangeIP。配置固定 `observe_ipv6=false`，运行时不生成自然 IPv6 变化事件。观察器发生不可恢复的状态错误时，整个 Agent 退出并由 systemd 重启，不会留下 WSS 在线但停止观察的半失效进程。

ChangeIP handler 在执行 provider 前把 command、旧 IP 和五分钟核对起点写入同一个 IP 状态文件。HTTP provider 只有收到 `200` 才返回 `change_triggered`；固定程序退出 `0` 也返回该结果。请求可能已经送达但响应、进程或 WSS 被换 IP 断开的情况返回 `change_trigger_unknown`，不会重发 provider。明确的非 `200`、非零退出或启动失败会取消核对并失败。

唯一的常驻 IPv4 monitor 随后负责事实判定：观察到新 IP 时持久上报 `ip.observed`；五分钟宽限后连续三次成功观察仍是旧 IP 时持久上报 `changeip.unchanged`。网络或观察源失败不计次数。两类消息在主控确认前都会跨断线和进程重启重发；主控按 command ID 收敛同一 session，且不要求 `operation.result` 必须先到达。45 分钟兜底只由 AkastrCloud session 持有，Agent 不维护第二个业务计时器。

## 7. SOCKS5 与 IPQuality

目标节点的 capability metadata 只公布端口，不包含地址来源、主机名、用户名或密码。AkastrCloud 始终把该端口与 Agent 最近一次上报的公网 IPv4 组合为 SOCKS5 入口；如果尚无有效公网 IPv4 观测，就不会派发 IPQuality。Runner 上的凭据位于独立 root-only profile 文件，以 AkastrCloud 的稳定 server key 索引。

bootstrap 固定官方 xykt/IPQuality commit `0ee5f192fed70c04615852efba0e4b8bd43546c7` 的 GitHub Raw 原始字节及其 SHA-256；Release workflow 会在发布前实际下载并验证该固定输入与 Debian 依赖声明。Runner 使用指定目标的 SOCKS5 端点运行该脚本；执行前后都会通过 SOCKS5 观察 IPv4，并与任务中的预期目标 IPv4 代际比对。代际在完成前变化时，即使脚本退出成功，AkastrCloud 也不会把结果作为该代际的有效报告。

官方脚本在仅 IPv4 模式下可能生成有效报告 URL，却返回非零 Bash 状态。因此，“输出中包含有界、有效的 `https://report.check.place/...` URL，且代理 postflight 成功”视为完成；非零退出且没有报告 URL 是 `script_failed`。

通过代理运行不等于直接在目标主机运行：依赖 Runner DNS 或直连网络的脚本检查应在验收时识别，并标记或省略。

## 8. Fix-forward 安装与自动更新

后台的一键命令描述节点的期望安装状态，可用于空白主机、同节点覆盖安装和残缺安装修复。覆盖安装先核对已有 identity/config 的节点 ID 和已装版本；不同节点或降级直接拒绝。installer 复用摘要正确的同版本 binary 和 Runner 脚本，在停止唯一 `akastr-agent.service` 前完成其余下载、bootstrap、依赖与 maintenance-safe 检查，停止后再对稳定状态检查并写入新配置。配置 revision 已前进时旧配置不能重新 ready，因此本机不维护目录或 unit 回滚事务；后续失败保留可重跑状态，由同一命令 fix-forward。同节点 identity 会复制到新配置并用新 configuration revision 重新 enrollment，不重新生成 private key。主控在未完成 command、active ChangeIP session 或目标 IPQuality run 存在时拒绝配置更新和注册。

自动维护不由 GitHub `latest` 驱动。主进程在建立 WSS 前先使用 Ed25519 identity 协调 Cloud 批准的软件与配置目标，ready 后等待 1–5 分钟随机抖动并每六小时复查。取得 exclusive update lease 后，candidate binary 写入不可变 release；desired bootstrap 由 candidate 严格解析到 root-only revision 目录，生成 capability 并由主控 accept。`deployments/<version>-r<revision>` 同时引用该 binary 与配置，trial 原位执行这一对目标；45 秒内重新完成 WSS auth/hello 后才原子替换并 fsync `current`。失败时 systemd 仍从旧 deployment 重启；清理只保留 current 与 previous deployment。

正式版本由 AkastrCloud 仓库的同步发布入口生成。发布器先验证并推送 Agent `main` 与不可变标签，等待 GitHub Release 的两个精确资产并核对 binary 版本与内部摘要，再提交 Cloud 的唯一更新目标并走正常 backend 发布。该顺序允许同一版本在任一阶段中断后续跑，但不会覆盖已发布资产或让 Cloud 指向尚未验真的 binary。

唯一主 service 使用 `Type=notify`，只有 WSS 完成 auth、hello 并收到 `hello.accepted` 后才向 systemd 报告 ready。service 使用 `ProtectSystem=strict`，并明确允许写状态目录与 Agent release root；固定 ChangeIP 程序可使用 service 可见的任意干净绝对路径，运行时不经 shell 且系统目录只读。Agent 不提供手工 `--update` 或本地回退 CLI；更新故障由 AkastrCloud 批准修复版本，或由操作者重新运行后台的一键 `--install`。WSS、认证或配置的破坏性版本必须走人工维护 Gate，不能通过自动更新跨协议部署。

## 9. 节点接入边界

操作者必须从 AkastrCloud 后台生成完整的一键命令，按 [安装与使用教程](INSTALLATION.md) 安装或重装。Agent 不提供 HTTP 控制面；未接入或离线的节点由主控安全拒绝操作。
