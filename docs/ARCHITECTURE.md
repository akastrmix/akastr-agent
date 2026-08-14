# Akastr Agent 架构

## 1. 进程模型

每个安装实例只运行一个由 systemd 管理的 `akastr-agent` 进程。Agent 主动连接 AkastrCloud 的 WSS 控制端点，不对外开放通用管理 HTTP 服务。实例只公布本机 root 所有配置中明确启用的能力。

同一个二进制支持两种部署形态：

- 目标节点：公网 IP 观察、ChangeIP，以及可选的不含秘密的 SOCKS5 端点描述；
- 专用 Runner：IPQuality 执行，`max_concurrency=1`。

运行时能力可以组合，`target` 和 `runner` 不是不同二进制，也不是协议中的永久角色。v0.3.0 的后台引导有意只生成其中一种部署配置：目标节点不执行 IPQuality，专用 Runner 也不承担目标节点能力，避免资源占用和目标网络变化互相影响。

v0.3.0 只发布 Debian 12/13 amd64 binary 和版本专用的 `install.sh`。AkastrCloud 后台收集完整配置，把长期 secret 加密为短期密封配置，并生成只含 installation UUID、HTTPS bootstrap endpoint 与一次性 token 的一行命令。Agent 使用 token 派生本地 AES-256-GCM key，验证并解密后生成严格 root-only 文件；主控从不保存 provider plaintext。安装器没有新装 TTY、向导或菜单，维护动作使用显式参数。用户不直接维护 JSON 或 checksum。

## 2. 主控边界

AkastrCloud 持有所有持久业务决策。Agent 不知道 Telegram 用户、订阅、每日限额、通知偏好、ChangeIP 预设或自动更换规则。

主控派发一次 IPQuality 前必须同时取得两种资源：

1. 目标节点上没有冲突的 ChangeIP 操作；
2. 兼容 Runner 有一个空闲执行槽。

相同目标、香港日历日及 IPv4 代际的请求合并为一次真实执行，之后返回缓存报告。香港时间跨日或观测到 IPv4 改变时，主控创建新的缓存代际。

配套 ADR 0024 方案以 `2026-08-13/akastr-agent-wss-v1` 获批。协议 `2026-08-13.v1` 使用有效期 15 秒的服务端 nonce，以及绑定上下文、以换行分隔的 Ed25519 签名文本。enrollment token 只能使用一次；进入 AkastrCloud 的只有公钥身份和不含秘密的能力描述。offer、accept、终态结果、结果确认和自然 IPv4 观察均使用稳定 UUID。消息按至少一次投递，本地日志和数据库唯一约束共同保证执行与结果幂等。

## 3. 包职责

- `internal/config`：严格读取和验证操作者配置；未知字段直接报错。
- `internal/capability`：生成确定性且不含秘密的能力描述。
- `internal/state`：带 schema 标记的原子 JSON 状态持久化。
- `internal/operation`：本地 exclusive group 和有界操作日志；不保存 command payload 或凭据。
- `internal/features/ipwatch`：通过固定 HTTPS 来源观察公网 IP，并持久保存尚未确认的自然 IPv4 事件。
- `internal/providers/changeip/command`：不用 shell 解释 payload，以固定 argv 运行本地程序；支持超时和终止进程树。
- `internal/providers/ipquality/script`：通过秘密 SOCKS5 profile 执行 checksum 固定的 Bash 脚本；执行前后验证代理 IPv4，并有界解析输出。
- `internal/identity`、`internal/protocol`、`internal/transport/ws`：本地 Ed25519 身份和可重连的受控 WSS 通道。
- `internal/bootstrap`：下载密封配置，以 installation UUID 作为 AAD 完成认证解密，并生成 root-only 运行文件。
- `internal/app`：组合配置、executor 与运行时入口。

只有在行为得到批准后，预留能力才会成为同级包，例如：

```text
internal/providers/changeip/httpflow/
internal/features/xraytraffic/
internal/features/ratelimit/
```

v0.3.0 不包含这些目录或空接口。

## 4. 本地操作状态

每个可执行操作都有一个 exclusive group。引擎在执行前持久化 active 记录，得到终态后再移动到有界 recent 历史。即使主控调度错误，同一组内的第二个操作也会被本地拒绝。

日志只保存 operation ID、kind、exclusive group、时间、状态和稳定终态 code，不保存 payload、SOCKS5 凭据、脚本输出、客户信息或 Telegram 标识。

终态记录只包含重发所需的有界安全结果。进程重启后，遗留的 active 记录继续占用互斥组；相同 command 再次到达时，它会被标记为 `interrupted_unknown`，不会猜测成功，也不会重新执行，然后释放互斥组。未知或损坏的状态 schema 会使启动失败，不会被静默重置。

## 5. 公网 IP 观察

观察器通过固定 HTTPS 来源 `api.ipify.org` 和 Cloudflare trace 获取公网地址，明确按 IPv4 或 IPv6 建立连接，拒绝重定向、非公网地址和过大响应。

v0.3.0 后台只生成 IPv4 watch：首次成功观察只建立基线，不上报；之后的变化先写入 `ip_state_file`，再发送 `ip.observed`。收到主控的 `ip.observed_ack` 之前，同一事件会在重连后继续重试。配置固定 `observe_ipv6=false`，当前运行时不会生成自然 IPv6 变化事件。

ChangeIP 成功执行 provider 只代表调用完成。Agent 随后必须观察到不同公网 IPv4 才返回 `ipv4_changed`；反复观察到原地址返回 `ipv4_unchanged`；如果网络切换后一直无法重新观察公网 IPv4，则返回 `ipv4_observe_timed_out`。

## 6. SOCKS5 与 IPQuality

目标节点的 capability metadata 可以公布地址来源和端口，但绝不包含用户名或密码。Runner 上的凭据位于独立 root-only profile 文件，以 AkastrCloud 的稳定 server key 索引。

v0.3.0 bootstrap 固定官方 xykt/IPQuality commit `0ee5f192fed70c04615852efba0e4b8bd43546c7` 及其 SHA-256。Runner 使用指定目标的 SOCKS5 端点运行该脚本；执行前后都会通过 SOCKS5 观察 IPv4，并与任务中的预期目标 IPv4 代际比对。代际在完成前变化时，即使脚本退出成功，AkastrCloud 也不会把结果作为当前代际的有效报告。

官方脚本在仅 IPv4 模式下可能已经生成有效报告 URL，却返回非零 Bash 状态。因此 v0.3.0 将“输出中包含有界、有效的 `https://report.check.place/...` URL，且代理 postflight 成功”视为完成；非零退出且没有报告 URL 仍是 `script_failed`。

通过代理运行不等于直接在目标主机运行：依赖 Runner DNS 或直连网络的脚本检查应在验收时识别，并标记或省略。

## 7. 迁移边界

六个旧 IPChanger 实例的 HTTP endpoint 和事件 callback 会一直保留，直到对应节点逐台完成 WSS 迁移以及回滚、观察 Gate。Akastr Agent 不模拟旧 HTTP contract；分阶段期间由 AkastrCloud 同时持有两套 integration，这是部署职责，不是 Agent 兼容层。

操作者必须从 AkastrCloud 后台生成完整的一键命令，按 [安装与使用教程](INSTALLATION.md) 逐台迁移；不得因 Agent release 或管理页面已经就绪就提前停用旧服务。
