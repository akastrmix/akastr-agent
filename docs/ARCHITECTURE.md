# Akastr Agent 架构

## 1. 进程模型

每个安装实例只存在一个 `akastr-agent.service` 和一个 Go 主进程。主进程主动连接 AkastrCloud 的 WSS 控制端点，并在内部每六小时检查一次主控批准的更新；节点没有额外 updater service、timer 或常驻辅助进程，也不开放通用管理 HTTP 服务。实例只公布本机 root 所有配置中明确启用的能力。

同一个二进制支持两种部署形态：

- 目标节点：公网 IP 观察、ChangeIP，以及可选的不含秘密的 SOCKS5 端点描述；
- 专用 Runner：IPQuality 执行，`max_concurrency=1`。

运行时能力可以组合，`target` 和 `runner` 不是不同二进制，也不是协议中的永久角色。后台只生成其中一种部署配置：目标节点不执行 IPQuality，专用 Runner 也不承担目标节点能力，避免资源占用和目标网络变化互相影响。

项目只发布 Debian 12/13 amd64 binary 和版本专用的 `install.sh`。AkastrCloud 后台先创建持久节点，再生成节点 UUID 与长期机器 token。机器 token 的 hash 用于认证，可恢复副本由主控 wrapping key 认证加密；provider secret 只存在于以机器 token 加密的持久 bootstrap 中，不以明文进入 PostgreSQL。安装命令只供 root shell 直接执行，不调用或依赖 `sudo`。操作者可以重复执行同一安装命令：安装器先验证 binary、密封配置和依赖，随后严格停止全部 Agent unit；确认进程已停止后才读取稳定的 operation state，存在 active 记录就保持原 unit 并中止，否则暂存 Agent 自有路径并为该节点注册全新的 Ed25519 identity。主控存在 pending、offered 或 accepted command 时拒绝重新注册。安装事务把 `akastr-agent*` systemd 命名空间收敛为唯一主 service。轮换 token 会重新加密 bootstrap 并让原命令立即失效。安装器没有 TTY、向导或菜单，用户不直接维护 JSON 或 checksum。所有安装期下载都使用 HTTPS-only、三次重试且禁用持久 HSTS 数据库的 wget，文件完整落盘后才会被执行或安装；网络响应不会直接通过管道交给 root shell。Runner 会安装 Bash、jq、curl、bc、netcat、DNS 与 iproute 依赖，并在变更本地 Agent 前逐项确认实际命令可执行；发布门禁会在 Debian 12/13 slim 环境真实安装同一清单。

## 2. 主控边界

AkastrCloud 持有所有持久业务决策。Agent 不知道 Telegram 用户、订阅、每日限额、通知偏好、ChangeIP 预设或自动更换规则。

主控派发一次 IPQuality 前必须同时取得两种资源：

1. 目标节点上没有冲突的 ChangeIP 操作；
2. 对应 Runner 有一个空闲执行槽。

相同目标、香港日历日及 IPv4 代际的请求合并为一次真实执行，之后返回缓存报告。香港时间跨日或观测到 IPv4 改变时，主控创建新的缓存代际。

协议 `2026-08-16.v3` 使用有效期 15 秒的服务端 nonce，以及绑定上下文、以换行分隔的 Ed25519 签名文本。机器 token 只用于 HTTPS bootstrap 和注册；WSS 与只读更新检查使用有效公钥身份。offer、accept、终态结果、结果确认和自然 IPv4 观察均使用稳定 UUID。消息按至少一次投递，本地日志和数据库唯一约束共同保证执行与结果幂等。

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

终态记录只包含重发所需的有界安全结果。进程重启后，遗留的 active 记录继续占用互斥组；Cloud 保留已 accepted command 并在重连后重发相同 offer，即使原执行窗口已结束，Agent 也只对本地相同 command/type 进入恢复。它会被标记为 `interrupted_unknown` 或继续既有 ChangeIP 核对，不会重新执行 provider，然后释放互斥组；recent 终态直接重放。未知过期 command 和未知或损坏的状态 schema都会被拒绝，不会被静默重置。

## 6. 公网 IP 观察

观察器通过固定 HTTPS 来源 `api.ipify.org` 和 Cloudflare trace 获取公网地址，明确按 IPv4 或 IPv6 建立连接，拒绝重定向、非公网地址和过大响应。IPv4 的非公网范围包括 private、loopback、link-local、CGNAT、文档/基准测试、组播和保留网段。

后台只生成 IPv4 watch：首次成功观察只建立基线，不上报；之后的变化先写入 `ip_state_file`，再发送 `ip.observed`。收到主控的 `ip.observed_ack` 之前，同一事件会在重连后继续重试。配置固定 `observe_ipv6=false`，运行时不生成自然 IPv6 变化事件。观察器发生不可恢复的状态错误时，整个 Agent 退出并由 systemd 重启，不会留下 WSS 在线但停止观察的半失效进程。

ChangeIP handler 在执行 provider 前把 command、旧 IP 和五分钟核对起点写入同一个 IP 状态文件。HTTP provider 只有收到 `200` 才返回 `change_triggered`；固定程序退出 `0` 也返回该结果。请求可能已经送达但响应、进程或 WSS 被换 IP 断开的情况返回 `change_trigger_unknown`，不会重发 provider。明确的非 `200`、非零退出或启动失败会取消核对并失败。

唯一的常驻 IPv4 monitor 随后负责事实判定：观察到新 IP 时持久上报 `ip.observed`；五分钟宽限后连续三次成功观察仍是旧 IP 时持久上报 `changeip.unchanged`。网络或观察源失败不计次数。两类消息在主控确认前都会跨断线和进程重启重发；主控按 command ID 收敛同一 session，且不要求 `operation.result` 必须先到达。45 分钟兜底只由 AkastrCloud session 持有，Agent 不维护第二个业务计时器。

## 7. SOCKS5 与 IPQuality

目标节点的 capability metadata 只公布端口，不包含地址来源、主机名、用户名或密码。AkastrCloud 始终把该端口与 Agent 最近一次上报的公网 IPv4 组合为 SOCKS5 入口；如果尚无有效公网 IPv4 观测，就不会派发 IPQuality。Runner 上的凭据位于独立 root-only profile 文件，以 AkastrCloud 的稳定 server key 索引。

bootstrap 固定官方 xykt/IPQuality commit `0ee5f192fed70c04615852efba0e4b8bd43546c7` 的 GitHub Raw 原始字节及其 SHA-256；Release workflow 会在发布前实际下载并验证该固定输入与 Debian 依赖声明。Runner 使用指定目标的 SOCKS5 端点运行该脚本；执行前后都会通过 SOCKS5 观察 IPv4，并与任务中的预期目标 IPv4 代际比对。代际在完成前变化时，即使脚本退出成功，AkastrCloud 也不会把结果作为该代际的有效报告。

官方脚本在仅 IPv4 模式下可能生成有效报告 URL，却返回非零 Bash 状态。因此，“输出中包含有界、有效的 `https://report.check.place/...` URL，且代理 postflight 成功”视为完成；非零退出且没有报告 URL 是 `script_failed`。

通过代理运行不等于直接在目标主机运行：依赖 Runner DNS 或直连网络的脚本检查应在验收时识别，并标记或省略。

## 8. 事务安装与自动更新

后台的一键命令描述节点的期望安装状态，可用于空白主机、覆盖安装和修复。安装器先在临时目录完成 bootstrap、binary、依赖和 Runner 脚本验证；随后严格停止全部 Agent unit，确认均为 inactive，再检查稳定的 operation journal。存在 active 记录就保持原 unit 并中止；空闲时才把配置、状态和 release root 移到有界事务备份，并只写 `akastr-agent.service`。每次 `--install` 都使用机器 token 重新注册公钥，不复制既有 identity 或 state。主控在同一节点存在未完成 command 时拒绝注册；拒绝发生在公钥替换前，因此安装器保持原目录和启停状态。注册请求已经改变主控公钥后，安装器保留新安装；故障修复方式是重跑同一安装命令。

自动更新不由 GitHub `latest` 驱动。主进程的六小时循环使用当前 Ed25519 identity 向 `POST /internal/agents/update` 发起带时间和 nonce 的签名只读检查；AkastrCloud 只有在目标版本已经随生产 release 固定、WSS 协议相同且节点没有未完成 command 时才返回 `update_available`。看到可用 manifest 后，更新必须取得进程级 exclusive lease；Agent 从发送 `operation.accepted` 前到 executor 已持久化终态期间持有 operation lease，两者使用同一个生命周期门，因此不存在“检查完空闲后又接单”的间隙。更新持锁完成下载、校验、`current` 切换和原位执行；持锁时收到 offer 会断开本次会话而不接受，主控保留并在重连后重发。更新只接受精确 GitHub immutable asset URL、前向 `vMAJOR.MINOR.PATCH` 和主控返回的内部 SHA-256，最大下载 32 MiB。新 binary 必须报告目标版本并通过当前 `check-config`，切换成功后只保留 current。

正式版本由 AkastrCloud 仓库的同步发布入口生成。发布器先验证并推送 Agent `main` 与不可变标签，等待 GitHub Release 的两个精确资产并核对 binary 版本与内部摘要，再提交 Cloud 的唯一更新目标并走正常 backend 发布。该顺序允许同一版本在任一阶段中断后续跑，但不会覆盖已发布资产或让 Cloud 指向尚未验真的 binary。

唯一主 service 使用 `Type=notify`，只有 WSS 完成 auth、hello 并收到 `hello.accepted` 后才向 systemd 报告 ready。service 使用 `ProtectSystem=strict`，并明确允许写状态目录与 Agent release root。Agent 只维护 `current` release，不提供手工 `--update` 或本地回退 CLI；更新故障由 AkastrCloud 批准修复版本，或由操作者重新运行后台的一键 `--install`。WSS、认证或配置的破坏性版本必须走人工维护 Gate，不能通过自动更新跨协议部署。

## 9. 节点接入边界

操作者必须从 AkastrCloud 后台生成完整的一键命令，按 [安装与使用教程](INSTALLATION.md) 安装或重装。Agent 不提供 HTTP 控制面；未接入或离线的节点由主控安全拒绝操作。
