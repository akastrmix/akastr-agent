# Akastr Agent 架构

## 1. 进程模型

每个安装实例只存在一个 `akastr-agent.service` 和一个 Go 主进程。主进程主动连接 AkastrCloud 的 WSS 控制端点，并在内部每六小时检查一次主控批准的更新；节点没有额外 updater service、timer 或常驻辅助进程，也不开放通用管理 HTTP 服务。实例只公布本机 root 所有配置中明确启用的能力。

同一个二进制支持两种部署形态：

- 目标节点：公网 IP 观察、ChangeIP，以及可选的不含秘密的 SOCKS5 端点描述；
- 专用 Runner：IPQuality 执行，`max_concurrency=1`。

运行时能力可以组合，`target` 和 `runner` 不是不同二进制，也不是协议中的永久角色。v0.8.1 的后台引导有意只生成其中一种部署配置：目标节点不执行 IPQuality，专用 Runner 也不承担目标节点能力，避免资源占用和目标网络变化互相影响。

v0.8.1 只发布 Debian 12/13 amd64 binary 和版本专用的 `install.sh`。AkastrCloud 后台先创建持久节点，再生成节点 UUID 与长期机器 token。机器 token 的 hash 用于认证，可恢复副本由主控 wrapping key 认证加密；provider secret 只存在于以机器 token 加密的持久 bootstrap 中，不以明文进入 PostgreSQL。安装命令只供 root shell 直接执行，不调用或依赖 `sudo`。操作者可以重复执行同一安装命令：安装器先验证新 binary、密封配置和依赖，随后严格停止全部 Agent unit；确认进程已停止后才读取稳定的 operation state，存在 active 记录就恢复原 unit 并中止，否则暂存 Agent 自有路径并为该节点注册全新的 Ed25519 identity。主控存在 pending、offered 或 accepted command 时拒绝重新注册。安装事务把 `akastr-agent*` systemd 命名空间收敛为唯一主 service，不按旧版本或旧 unit 名称分支。轮换 token 会重新加密 bootstrap 并让旧命令立即失效。安装器没有 TTY、向导或菜单，用户不直接维护 JSON 或 checksum。所有安装期下载都使用 HTTPS-only、三次重试且禁用持久 HSTS 数据库的 wget，文件完整落盘后才会被执行或安装；网络响应不会直接通过管道交给 root shell。

## 2. 主控边界

AkastrCloud 持有所有持久业务决策。Agent 不知道 Telegram 用户、订阅、每日限额、通知偏好、ChangeIP 预设或自动更换规则。

主控派发一次 IPQuality 前必须同时取得两种资源：

1. 目标节点上没有冲突的 ChangeIP 操作；
2. 兼容 Runner 有一个空闲执行槽。

相同目标、香港日历日及 IPv4 代际的请求合并为一次真实执行，之后返回缓存报告。香港时间跨日或观测到 IPv4 改变时，主控创建新的缓存代际。

持久节点认证、SOCKS5 地址收口和单 service 更新模型分别以 `2026-08-16/agent-persistent-node-bootstrap-v1`、`2026-08-16/agent-observed-ip-socks5-only-v1` 和 `2026-08-16/agent-single-systemd-service-v1` 获批。协议 `2026-08-16.v3` 使用有效期 15 秒的服务端 nonce，以及绑定上下文、以换行分隔的 Ed25519 签名文本。机器 token 只用于 HTTPS bootstrap 和注册；WSS 与只读更新检查使用当前公钥身份。offer、accept、终态结果、结果确认和自然 IPv4 观察均使用稳定 UUID。消息按至少一次投递，本地日志和数据库唯一约束共同保证执行与结果幂等。

## 3. 包职责

- `internal/config`：严格读取和验证操作者配置；未知字段直接报错。
- `internal/capability`：生成确定性且不含秘密的能力描述。
- `internal/state`：带 schema 标记的原子 JSON 状态持久化。
- `internal/operation`：本地 exclusive group 和有界操作日志；不保存 command payload 或凭据。
- `internal/lifecycle`：command execution 与自动更新共用的进程级 lease；不保存持久业务状态。
- `internal/features/ipwatch`：通过固定 HTTPS 来源观察公网 IP，并持久保存尚未确认的自然 IPv4 事件。
- `internal/providers/changeip/command`：不用 shell 解释 payload，以固定 argv 运行本地程序；支持超时和终止进程树。
- `internal/providers/ipquality/script`：通过秘密 SOCKS5 profile 执行 checksum 固定的 Bash 脚本；执行前后验证代理 IPv4，并有界解析输出。
- `internal/identity`、`internal/protocol`、`internal/transport/ws`：本地 Ed25519 身份和可重连的受控 WSS 通道。
- `internal/bootstrap`：下载持久密封配置，以节点 UUID 作为 AAD 完成认证解密，并生成 root-only 运行文件。
- `internal/autoupdate`：主进程内六小时循环，签名请求 Cloud 批准清单，只接受同一 WSS 协议的前向语义版本，并完成有界下载、内部 digest 校验和不可变 release 切换。
- `internal/app`：组合配置、executor 与运行时入口。

只有在行为得到批准后，预留能力才会成为同级包，例如：

```text
internal/providers/changeip/httpflow/
internal/features/xraytraffic/
internal/features/ratelimit/
```

v0.8.1 不包含这些目录或空接口。

## 4. 本地操作状态

每个可执行操作都有一个 exclusive group。引擎在执行前持久化 active 记录，得到终态后再移动到有界 recent 历史。即使主控调度错误，同一组内的第二个操作也会被本地拒绝。

日志只保存 operation ID、kind、exclusive group、时间、状态和稳定终态 code，不保存 payload、SOCKS5 凭据、脚本输出、客户信息或 Telegram 标识。

终态记录只包含重发所需的有界安全结果。进程重启后，遗留的 active 记录继续占用互斥组；相同 command 再次到达时，它会被标记为 `interrupted_unknown`，不会猜测成功，也不会重新执行，然后释放互斥组。未知或损坏的状态 schema 会使启动失败，不会被静默重置。

## 5. 公网 IP 观察

观察器通过固定 HTTPS 来源 `api.ipify.org` 和 Cloudflare trace 获取公网地址，明确按 IPv4 或 IPv6 建立连接，拒绝重定向、非公网地址和过大响应。

v0.8.1 后台只生成 IPv4 watch：首次成功观察只建立基线，不上报；之后的变化先写入 `ip_state_file`，再发送 `ip.observed`。收到主控的 `ip.observed_ack` 之前，同一事件会在重连后继续重试。配置固定 `observe_ipv6=false`，当前运行时不会生成自然 IPv6 变化事件。观察器发生不可恢复的状态错误时，整个 Agent 退出并由 systemd 重启，不会留下 WSS 在线但停止观察的半失效进程。

ChangeIP 成功执行 provider 只代表调用完成。Agent 随后必须观察到不同公网 IPv4 才返回 `ipv4_changed`；反复观察到原地址返回 `ipv4_unchanged`；如果网络切换后一直无法重新观察公网 IPv4，则返回 `ipv4_observe_timed_out`。

## 6. SOCKS5 与 IPQuality

目标节点的 capability metadata 只公布端口，不包含地址来源、主机名、用户名或密码。AkastrCloud 始终把该端口与 Agent 最近一次上报的公网 IPv4 组合为 SOCKS5 入口；如果尚无有效公网 IPv4 观测，就不会派发 IPQuality。Runner 上的凭据位于独立 root-only profile 文件，以 AkastrCloud 的稳定 server key 索引。

v0.8.1 bootstrap 固定官方 xykt/IPQuality commit `0ee5f192fed70c04615852efba0e4b8bd43546c7` 的 GitHub Raw 原始字节及其 SHA-256；Release workflow 会在发布前实际下载并验证该固定输入。Runner 使用指定目标的 SOCKS5 端点运行该脚本；执行前后都会通过 SOCKS5 观察 IPv4，并与任务中的预期目标 IPv4 代际比对。代际在完成前变化时，即使脚本退出成功，AkastrCloud 也不会把结果作为当前代际的有效报告。

官方脚本在仅 IPv4 模式下可能已经生成有效报告 URL，却返回非零 Bash 状态。因此 v0.8.1 将“输出中包含有界、有效的 `https://report.check.place/...` URL，且代理 postflight 成功”视为完成；非零退出且没有报告 URL 仍是 `script_failed`。

通过代理运行不等于直接在目标主机运行：依赖 Runner DNS 或直连网络的脚本检查应在验收时识别，并标记或省略。

## 7. 事务安装与自动更新

后台的一键命令是期望安装状态，不是“仅限空白机器”的初始化器。安装器先在临时目录完成 bootstrap、binary、依赖和 Runner 脚本验证；随后严格停止全部 Agent unit，确认均为 inactive，再检查稳定的 operation journal。存在 active 记录就恢复原 unit 并中止；空闲时才把配置、状态和 release root 移到有界事务备份，并只写 `akastr-agent.service`。每次 `--install` 都使用机器 token 重新注册公钥，不复制旧 identity 或 state。主控在同一节点存在未完成 command 时拒绝注册；明确拒绝发生在公钥替换前，因此安装器恢复原目录和启停状态。注册请求可能已经改变主控公钥时，安装进入不可回退点：不再恢复旧 identity，失败后重跑同一安装命令 fix-forward。旧 IPChanger 不在这些路径内，始终不受影响。

自动更新不由 GitHub `latest` 驱动。主进程的六小时循环使用当前 Ed25519 identity 向 `POST /internal/agents/update` 发起带时间和 nonce 的签名只读检查；AkastrCloud 只有在目标版本已经随生产 release 固定、WSS 协议相同且节点没有未完成 command 时才返回 `update_available`。看到可用 manifest 后，更新必须取得进程级 exclusive lease；Agent 从发送 `operation.accepted` 前到 executor 已持久化终态期间持有 operation lease，两者使用同一个生命周期门，因此不存在“检查完空闲后又接单”的间隙。更新持锁完成下载、校验、`current` 切换和原位执行；持锁时收到 offer 会断开本次会话而不接受，主控保留并在重连后重发。更新只接受精确 GitHub immutable asset URL、前向 `vMAJOR.MINOR.PATCH` 和主控返回的内部 SHA-256，最大下载 32 MiB。新 binary 必须报告目标版本并通过当前 `check-config`，切换成功后只保留 current。

唯一主 service 使用 `Type=notify`，只有 WSS 完成 auth、hello 并收到 `hello.accepted` 后才向 systemd 报告 ready；安装器不再把短暂存活的 PID 当成功。service 保持 `ProtectSystem=strict`，但明确允许写状态目录与 Agent release root。项目不保留 previous release、手工 `--update` 或回退 CLI；更新故障由 AkastrCloud 批准修复版本，或由操作者重新运行后台的一键 `--install`。WSS/auth/config 的破坏性版本不会进入当前协议通道，继续走人工维护 Gate。

## 8. 迁移边界

六个旧 IPChanger 实例的 HTTP endpoint 和事件 callback 会一直保留，直到对应节点逐台完成 WSS 迁移以及回滚、观察 Gate。Akastr Agent 不模拟旧 HTTP contract；分阶段期间由 AkastrCloud 同时持有两套 integration，这是部署职责，不是 Agent 兼容层。

操作者必须从 AkastrCloud 后台生成完整的一键命令，按 [安装与使用教程](INSTALLATION.md) 逐台迁移；不得因 Agent release 或管理页面已经就绪就提前停用旧服务。
