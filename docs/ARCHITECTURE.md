# Akastr Agent 架构

## 1. 进程模型

每个安装实例只存在一个 `akastr-agent.service` 和一个 Go 主进程。主进程主动连接 AkastrCloud 的 WSS 控制端点，并在内部检查主控批准的更新（见第 8 节）；节点没有额外 updater service、timer 或常驻辅助进程，也不开放通用管理 HTTP 服务。实例只公布配置中明确启用的能力。

每项能力是一个可单独开关的模块：公网 IP 观察、ChangeIP、SOCKS5 端点描述、IPQuality Runner。节点配置里出现哪个模块就启用哪个（格式见 [PROTOCOL.md](PROTOCOL.md#节点配置与模块)）。同一个二进制服务所有节点；目标节点与 Runner 只是 Cloud 选择的两种模块组合：绑定服务器的目标节点必须观察公网 IP，Runner 只执行 IPQuality，避免资源占用和目标网络变化互相影响。

项目只发布 Debian 12/13 amd64 binary 和版本专用的 `install.sh`。节点由后台先创建，得到节点 UUID 与长期机器 token；provider secret 只在以机器 token 加密的持久 bootstrap 里，不以明文进入 PostgreSQL。节点上的配置就是 Cloud 下发的 bootstrap 明文，Agent 不再转写成第二种格式。配置有单调的 desired/applied revision，两者不等时 Cloud 不派发操作；配置更新由主进程自动取回收敛，不需要重跑安装命令。

## 2. 主控边界

AkastrCloud 持有所有持久业务决策。Agent 不知道 Telegram 用户、订阅、每日限额、通知偏好、ChangeIP 预设或自动更换规则。

主控派发一次 IPQuality 前必须同时取得两种资源：

1. 目标节点上没有冲突的 ChangeIP 操作；
2. 对应 Runner 有一个空闲执行槽。

请求合并、次数限制及缓存失效由 [Cloud Carpool 契约](https://github.com/akastrmix/AkastrCloud/blob/main/docs/CARPOOL.md#5-changeip-与-ipquality)定义，Agent 不维护第二套业务缓存。

机器 token 只用于 HTTPS bootstrap 和注册；日常 WSS 与更新检查通过 Ed25519 身份认证。Cloud 核对当前部署、配置和能力后才允许业务执行。消息按至少一次投递，本地日志和数据库唯一约束共同保证执行与结果幂等；认证格式、消息字段及握手顺序以 [PROTOCOL.md](PROTOCOL.md) 为准。

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

- `internal/config`：严格读取节点配置的外层；各模块的配置段由模块自己校验。
- `internal/module`：模块接入的三种形态——不可重复的一次性任务（`Commands`）、Cloud 下发并可反复应用的目标状态（`Desired`）、节点主动上报的事实（`Reporter`）。
- `internal/modules/*`：每个模块一个目录，放齐它的全部代码：配置段、命令、目标、上报与回执、它在 Root/StateDir 下的文件，以及只供它使用的执行方式（子包）；`internal/app/modules.go` 是唯一列出全部模块并连接依赖的地方。
- `internal/layout`：固定的磁盘布局——A/B 两个 slot、`current` 原子切换、安装/更新共用的锁，以及交给模块的 Root 与 StateDir；不认识任何模块的文件。
- `internal/state`：带 schema 标记的原子 JSON 状态持久化。
- `internal/operation`：统一拥有实时执行、终态重放、重启恢复、本地 exclusive group 和有界操作日志；不保存 command payload。能力 handler 只提供执行和恢复结果，不各自维护 journal 流程。
- `internal/desired`：保存各 `Desired` 模块的目标（只在内存）、决定何时应用（变化后、定期、失败退避）并回报状态；不认识任何模块。
- `internal/lifecycle`：command execution 与自动更新共用的进程级 lease；不保存持久业务状态。
- `internal/modules/ipwatch`：通过固定 HTTPS 来源独立观察公网 IPv4/IPv6，并持久保存各自尚未确认的事实及活动 ChangeIP 核对状态。
- `internal/modules/changeip`：换 IP 模块；统一描述明确触发、结果未知和明确失败，子包 `httpcurl` 只接受固定 curl 配置与 HTTP `200`，`command` 不用 shell 解释 payload并以固定 argv 运行本机程序。
- `internal/modules/xui`：按目标对齐本机 3x-ui 中 `ak-` 前缀的受管客户端，并上报入站与受管客户端流量的快照。
- `internal/modules/ipqualityrunner/script`：用任务带来的 SOCKS5 登录执行 checksum 固定的 Bash 脚本；执行前后验证代理 IPv4，并有界解析输出。
- `internal/identity`、`internal/protocol`、`internal/transport/ws`：本地 Ed25519 身份和可重连的受控 WSS 通道。
- `internal/bootstrap`：安装时下载密封配置，以节点 UUID 作为 AAD 完成认证解密。
- `internal/install`：一次安装命令的全部收敛步骤（第 8 节）。
- `internal/update`：进程内更新检查与候选版本启动（第 8 节）。
- `internal/app`：解析启用的模块并组成运行时；连接层只收发消息，命令、上报与确认都交给对应模块。
- `internal/daemon`：组织 WSS、更新循环、候选版本激活与退出；CLI 只负责参数和进程信号。

新增能力：在 `internal/modules` 下新建模块目录，实现 `module.Commands`、`module.Desired` 或 `module.Reporter` 中合适的形态，在 `internal/app/modules.go` 加一个字段和它的解析、构建分支，并在 [PROTOCOL.md](PROTOCOL.md#节点配置与模块) 登记配置段以及它的命令类型、[目标格式](PROTOCOL.md#目标状态)或[上报 kind](PROTOCOL.md#上报与回执)。连接层、更新与安装都不需要改。

节点配置只放模块开关和很少变化的设置：配置一变，整个进程就经第 8 节的更新路径重启，新配置先校验，失败回到旧 slot。经常变化的业务数据（例如防火墙规则、xray 用户）不进节点配置，否则每次修改都会重启节点；Cloud 通过 WSS 以[目标状态](PROTOCOL.md#目标状态)下发，`Desired` 模块对比本机现状，只改有差别的部分。同一目标重复应用结果不变，所以断线后全量重发、定期纠正手工改动都是安全的；只有不可重复的动作才用 operation。

## 5. 本地操作状态

同一进程的重复调用加入正在进行的执行；只有没有实时执行的 active journal 才按重启恢复处理。每个可执行操作都有一个 exclusive group。引擎在执行前持久化 active 记录，得到终态后再移动到有界 recent 历史。即使主控调度错误，同一组内的第二个操作也会被本地拒绝。

操作记录保存 operation ID、kind、exclusive group、时间、状态、稳定终态 code 和最多 8 KiB 的终态结果，最近历史保持 64 条。操作记录不保存 offer payload、面板密码、IPQuality 临时 SOCKS5 凭据或脚本输出。

终态记录只包含重发所需的有界安全结果。Cloud 的 accepted ack 是执行授权：Cloud 已持久 acceptance 但节点尚未收到 ack 就断线时，相同 offer 可在原窗口后重新握手并开始一次本地执行；从未 accepted 的过期 command 得不到授权。进程重启后的 active ChangeIP 记录直接收敛为 `change_trigger_unknown`，其他 active 操作按各自恢复语义处理，ChangeIP/IPQuality provider 不会再次执行；recent 终态直接重放。未知或损坏的状态 schema 会被拒绝，不会被静默重置。

## 6. 公网 IP 观察

观察器通过固定 HTTPS 来源 `api.ipify.org` 和 Cloudflare trace 获取公网地址，明确按 IPv4 或 IPv6 建立连接，拒绝重定向、非公网地址和过大响应。IPv4 的非公网范围包括 private、loopback、link-local、CGNAT、文档/基准测试、组播和保留网段。

- Target 的 IPv4 watch 始终启用。地址历史只在 Cloud：每个进程与每次业务连接先观察并上报当前地址（`ip.address`），之后只在地址变化时上报；未确认的上报被新会话的上报取代，本地不保存地址。**当前会话的 IPv4 上报被确认前不接受 ChangeIP。**
- `observe_ipv6=true` 时 IPv6 独立循环；无公网 IPv6 或探测失败只重试，不影响 IPv4、ChangeIP 或更新前的空闲判断。
- 观察器出现不可恢复的状态错误时整个 Agent 退出、由 systemd 重启，避免出现“WSS 在线但已停止观察”的半失效进程。

ChangeIP handler 在执行 provider 前把 command、旧 IP 和核对起点写入 ChangeIP 核对文件（节点上唯一持久的 IP 状态）。HTTP provider 只有收到 `200` 才返回 `change_triggered`；固定程序退出 `0` 也返回该结果。请求可能已经送达但响应、进程或 WSS 被换 IP 断开的情况返回 `change_trigger_unknown`，不会重发 provider。明确的非 `200`、非零退出或启动失败会取消核对并失败。

唯一的常驻 IPv4 monitor 随后负责事实判定：核对期间改为每 10 秒观察一次；观察到新 IP 时上报带 command ID 的 `ip.address`；触发两分钟后连续两次成功观察仍是旧 IP 时上报 `changeip.unchanged`。换 IP 通常伴随断网，恢复后看到的地址即为结果；网络或观察源失败不计次数。结果先写入核对文件再发送，主控确认前跨断线和进程重启重发同一结果；主控按 command ID 收敛同一次换 IP，且不要求 `operation.result` 必须先到达。只有带 command ID 的变化才归因于换 IP。45 分钟兜底只由 AkastrCloud session 持有，Agent 不维护第二个业务计时器。

## 7. SOCKS5 与 IPQuality

SOCKS5 的端口和登录都属于目标节点的 `socks5` 模块，由 Cloud 从自己保存的配置读取。AkastrCloud 始终把该端口与 Agent 最近一次上报的公网 IPv4 组合为 SOCKS5 入口；如果尚无有效公网 IPv4 观测，就不会派发 IPQuality。Runner 没有任何代理配置：每次检测由 Cloud 把目标的登录放进任务，Runner 只在执行期间持有，因此增删目标或改密码都不需要改动或重启 Runner，多个 Runner 也无需各自配置。

Agent 程序内固定官方 IPQuality 脚本的 commit 与 SHA-256（`internal/modules/ipqualityrunner/script/pin.go`），脚本按摘要存放，候选版本改变固定版本也不会影响可回退的旧版本；新部署确定生效后删除其他摘要的脚本；CI 会实际下载并验证固定输入与 Debian 依赖声明。Runner 使用指定目标的 SOCKS5 端点运行该脚本；执行前后都会通过 SOCKS5 观察 IPv4，并与任务中的预期目标 IPv4 代际比对。代际在完成前变化时，即使脚本退出成功，AkastrCloud 也不会把结果作为该代际的有效报告。

官方脚本在仅 IPv4 模式下可能生成有效报告 URL，却返回非零 Bash 状态。因此，“输出中包含有界、有效的 `https://report.check.place/...` URL，且代理 postflight 成功”视为完成；非零退出且没有报告 URL 是 `script_failed`。

通过代理运行不等于直接在目标主机运行：依赖 Runner DNS 或直连网络的脚本检查应在验收时识别，并标记或省略。

## 8. 安装与自动更新

**磁盘布局固定为 A/B 两个 slot。** `/usr/local/lib/akastr-agent/slots/{a,b}` 各放一份程序与配置，`current` 符号链接指向正在使用的 slot，systemd 只启动 `current`。切换就是一次原子 rename；另一个 slot 自然就是上一版，不需要清理逻辑。identity 在 `/etc/akastr-agent`，执行日志与 IP 状态在 `/var/lib/akastr-agent`，二者都不属于 slot，安装和更新都不改写它们。

**安装（fix-forward）。** `install.sh` 只下载并核对批准版本的程序，其余由 `akastr-agent install` 完成：取锁 → 核对归属（已有 identity 属于别的节点，或有执行状态却没有 identity，都拒绝）→ 取回并解密配置 → Runner 安装依赖与固定版本脚本 → 写入非活动 slot 并验证运行依赖 → 生成新 Ed25519 key 并注册。注册成功前旧部署照常运行；成功后停止服务、写入 identity、切换 `current`、清理旧版布局遗留、写入 unit 并重启。任何一步失败都重跑同一命令修复，不做本机回滚。

**自动更新。** 主进程每 60 秒、以及业务连接每次结束或就绪时，向 Cloud 发一次签名的更新检查（协议见 [PROTOCOL.md](PROTOCOL.md#更新检查)）。发现目标后：

- 把目标程序与配置写入非活动 slot，由候选程序自己执行 `version` 与 `prepare` 验证；
- 取得进程级更新 lease 并确认本地没有进行中的 operation 或 ChangeIP 核对，否则稍后重试；
- 在 `update-attempt.json` 记下一次尝试后，以候选程序原地替换进程；
- 候选程序收到 `hello.accepted` 后先切换 `current`，再处理业务；45 秒内未被接受就记录原因并退出，systemd 重启仍指向旧 slot 的 `current`。

同一目标连续失败两次后暂停，每六小时再自动试一次，所以临时故障能自行恢复，坏版本也不会造成重启风暴。失败原因随下一次检查上报，后台显示在节点详情中。

**进程与 service。** 唯一主 service 为 `Type=notify`、`ProtectSystem=strict`，只可写状态目录与 `/usr/local/lib/akastr-agent`。本地依赖损坏导致运行时构建失败时记录 `runtime_initialization_failed`，只保留更新检查，以便接收修复版本或配置。不提供手工 `--update` 或本地回退 CLI；身份或部署损坏时用后台一键命令修复。

更新检查的请求/响应与 `version`、`prepare`、`run` 三个 CLI 是旧版本升级到新版本所依赖的稳定契约，改变它们须单独设计迁移方案。

本机 3x-ui HTTP 适配集中在 `internal/modules/xui`，不新增监听端口。Cloud 计算每个受管客户端的业务目标（启停、上限、重置），Agent 只对齐 `ak-` 前缀的客户端，不接管其他客户端，也不生成链接。xui 唯一的持久文件是 `StateDir/xui/resets.json`，记录每个客户端已执行的流量重置序号，防止重复清零。

## 9. 节点接入边界

操作者必须从 AkastrCloud 后台生成完整的一键命令，按 [安装与使用教程](INSTALLATION.md) 安装或重装。Agent 不提供 HTTP 控制面；未接入或离线的节点由主控安全拒绝操作。
