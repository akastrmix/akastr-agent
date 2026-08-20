# Akastr Agent 协议 `2026-08-20.v5`

AkastrCloud 提供 HTTPS enrollment endpoint 和仅供 Agent 主动连接的 WSS 控制路由。每个 JSON envelope 必须且只能包含 `protocol`、`message_id`、`type`、`sent_at` 和 `body`；text frame 最大 64 KiB。未知字段、未知 message type、未知 capability 字段、binary frame、无效 UUID 和未来协议版本均会失败关闭。

## Enrollment 与身份认证

管理员先在 AkastrCloud 后台创建持久节点并填写全部参数。后台签发 32-byte canonical base64url 机器 token，以 token 和节点 UUID 加密 `akastr-agent-bootstrap.v3` 配置，并生成版本化一键命令。bootstrap 明文必须使用 `schema_version=3`，并包含正整数 `configuration_revision`。Agent 以节点 UUID 和机器 token 从 `POST /internal/agents/bootstrap` 获取 nonce/ciphertext，在本机认证解密并生成 root-only config v3 与 secret 文件。

`POST /internal/agents/enroll` 请求必须且只能包含 `machine_token`、raw 32-byte `public_key`、当前批准的语义化 `agent_version`、正整数 `configuration_revision` 和不含秘密的 `capabilities`。版本必须精确等于 Cloud 当前批准 release，revision 必须精确等于节点的 desired revision，否则分别返回 `agent_release_required` 或 `agent_configuration_stale`。首次安装生成 Ed25519 keypair；同节点完整重装复用已确认 identity，并再次提交同一公钥。注册成功把 applied revision 推进到 desired revision，并断开既有 WSS。只有状态码与已知 enrollment 业务错误严格匹配的 JSON 4xx 响应才会删除新生成的 pending identity；已确认 identity 不因重装被拒绝而删除。重定向、408、425、429、5xx、非 JSON 代理响应、网络中断和无法严格解析的响应均保留 identity 供重试。主控在该节点存在 pending、offered 或 accepted command，或 Target 对应服务器仍有 active ChangeIP session 时，以 `agent_node_busy` 拒绝注册。

机器 token 是长期安装凭据，不是 WSS bearer。主控只保存 SHA-256 hash、认证加密的可恢复 token 和密封 bootstrap。配置更新保持节点 ID、target/Runner 角色、服务器绑定、机器 token 和 identity 不变；主控递增 desired revision、替换密封 bootstrap、断开连接，并在新 revision enrollment 完成前禁止 preflight、command 创建与 offer。管理员可审计地重新显示安装命令，也可轮换 token；轮换只接受 bootstrap v3，并在一个事务中更新 token hash、可恢复密文和 bootstrap 密文。删除节点会永久删除身份、bootstrap 和已完成 command 记录；存在未完成 command 或 active ChangeIP session 时拒绝删除。节点永久丢失且遗留 accepted command 时，管理员可显式将其记录为执行结果未知，并撤销旧 identity、终结同节点其他未接受 command 后把节点重置为 pending；机器 token 与密封 bootstrap 保留，供重装机器注册新 identity。主控不会自动超时放弃 accepted command。

enrollment HTTPS 地址由 WSS 地址确定：`wss://<host>/internal/agents/ws` 对应 `https://<host>/internal/agents/enroll`。客户端不提供关闭 TLS 校验或绕过主机名校验的选项。

WSS 连接 query 中包含 `agent_id`。AkastrCloud 发送 `auth.challenge`，其中 nonce 为 32 bytes、有效期为 15 秒。Agent 对下面这些 UTF-8 行签名，末尾没有换行：

```text
akastr-agent-auth-v1
<agent_id>
<challenge_id>
<nonce>
<issued_at exactly as received>
<expires_at exactly as received>
```

Agent 发送 `auth.response` 并收到 `auth.accepted` 后发送 `agent.hello`；hello 必须且只能包含语义化 `agent_version`、本地正整数 `configuration_revision` 与 capability。Cloud 要求 hello revision 同时精确等于 desired/applied，才返回 `hello.accepted` 并使连接进入 ready；同协议旧 release 可以进入 ready 后使用签名更新接口。绑定服务节点的 Target 必须公布 `ip.observe` 且不得公布 `ipquality.runner`；不绑定服务节点的 Runner 只能公布 `ipquality.runner`，主控只允许一个 active Runner。相同节点的新认证连接会替换既有连接。

## 自动更新检查

`POST /internal/agents/update` 是独立于 WSS envelope 的只读长期合同。请求必须且只能包含 `agent_id`、`agent_version`、`protocol`、32-byte nonce、`sent_at` 和 64-byte Ed25519 signature；时间与主控相差超过五分钟即拒绝。签名文本为以下 UTF-8 行，末尾没有换行，时间保持 Agent 发送的原文：

```text
akastr-agent-update-check-v1
<agent_id>
<agent_version>
<protocol>
<nonce>
<sent_at>
```

主控使用当前 active identity 验签，并返回严格的 `akastr-agent-update.v1` manifest：`status`、目标 `version`、相同 WSS `protocol`、精确 immutable `binary_url` 和内部 `binary_sha256`。`status` 只有 `current`、`busy` 或 `update_available`；存在 pending/offered/accepted command 时只能返回 `busy`。该接口不接收机器 token、不修改数据库，也不允许降级或跨 WSS 协议更新。

## Operation

`operation.offer` 包含：

- `command_id`：稳定 UUID，也是执行幂等键与本地 journal key；
- `command_type`：runtime 只接受 `changeip.execute` 和 `ipquality.execute`；
- `payload_version=1` 与对应类型的严格 payload；
- `not_before` 和 `expires_at`。

Agent 在当前时间达到 `not_before` 且进程级更新 lease 空闲时发送 `operation.accepted`，但该消息只发起持久化握手，不代表本地已经获得执行权。主控只会在未过 `expires_at` 的 offered command 上首次建立 accepted；已经 accepted 的 command 即使原窗口已过仍返回 `operation.accepted_ack accepted=true`，从未 accepted 的过期 command 返回 `false`。Agent 只有收到 `true` 才调用本地已配置 provider，此后不再用本地时钟复核原 offer 截止时间。终态先持久化，再释放 operation lease 并通过 `operation.result` 发送；AkastrCloud 只接受数据库状态已经是 `accepted` 的首个终态，在数据库事务和下游事件接纳成功后发送 `operation.result_ack`。

已被主控接受的 command 不因原 `expires_at` 自动终结。节点重连时主控继续发送相同 offer，并以 accepted ack 恢复执行权；这覆盖主控已提交 acceptance、但 ack 尚未到达节点便断线的窗口。active ChangeIP journal 一律收敛为 `change_trigger_unknown`，recent 记录重放原终态，两者都不会再次执行 provider；首次 accepted ack 后尚无 journal 的 command 才开始一次本地执行。主控从未 accepted 的过期 command 不会获得执行权。

断线后 offer 和 result 都可能重复。相同 `command_id` 的本地终态只会重放，不会再次执行。payload 不得选择 program、argv、shell fragment、文件、凭据或任意 URL。

### `changeip.execute`

payload 只包含 `expected_ipv4`。Agent 在调用 provider 前观察当前公网 IPv4：不匹配时返回 `stale_expected_ipv4`，不执行 provider。Agent 先持久化 command 与该旧 IP 的核对状态，再调用本机固定 provider；stdout/stderr 不进入协议或日志。

HTTP API provider 只把状态码 `200` 作为明确成功；真实非 `200` 或请求建立前失败是明确失败。固定程序 provider 以退出码 `0` 为明确成功、非零为明确失败。明确成功返回 `change_triggered`；请求可能已送达但响应、进程或 WSS 因断网中断时返回 `change_trigger_unknown`。两种成功终态都只包含触发前 IPv4，不宣称地址已经变化，也不会重发 provider。其他对外稳定 code 包括 `http_status_not_200`、`request_failed`、`stale_expected_ipv4`、`ipv4_observe_failed`、`start_failed`、`exited_nonzero` 和 `reconciliation_state_failed`。

### `ipquality.execute`

payload 只包含 `expected_ipv4`、不含秘密的 `proxy_port`、本地 `proxy_profile_id` 和 `script_version`。Runner 直接把 `expected_ipv4` 作为 SOCKS5 地址，不存在另一个 hostname/IP 或目标 ID 字段。SOCKS5 username/password 只存在 Runner 的 root-only profile 文件中。

Runner 同一时间只允许一个 command。每次执行前都重新校验脚本 SHA-256，通过 SOCKS5 做 IPv4 preflight，随后以固定参数执行：

```text
/bin/bash <script_path> -4 -n -x <local_socks5_relay_url>
```

本地 relay 只监听 `127.0.0.1` 的随机端口，并使用 profile 中的凭据连接上游 SOCKS5。脚本结束后 Runner 再做 postflight；代理地址改变、预期 IPv4 过期、checksum 不一致或找不到 profile 都会失败。

只有精确来源 `https://report.check.place/...`、无用户信息和显式端口的 URL 加成功 postflight 才是 `report_ready`，即使官方 IPv4-only Bash 进程返回非零；非零且无报告 URL 是 `script_failed`。输出上限为 2 MiB，超限返回 `script_output_too_large`。

## ChangeIP 与 IPv4 核对

首次成功观察必须先持久化并发送 `ip.snapshot`，body 只包含 `snapshot_id`、`family=ipv4`、`address` 和 `observed_at`。Cloud 以同一 snapshot ID 幂等建立或刷新 baseline，再返回 `ip.snapshot_ack`；当前 identity 的 snapshot readiness 与该提交原子持久化，重连和进程重启保留，重新 enrollment 时清除。重装后的 snapshot 若与既有 baseline 不同且节点没有未终结 command，Cloud 以既有地址作为 previous address 原子记录一次自然变化。存在未终结 command 时地址冲突安全失败。Agent 在确认前不得接受 ChangeIP，并跨重连、重启重发尚未确认的 snapshot。

`ip.observed` 包含 `observation_id`、`family=ipv4`、`previous_address`、`address` 和 `observed_at`。事件时间必须晚于 Cloud 当前 baseline 且不得超前主控超过五分钟。只有 command 已 accepted、观测不早于 session 开始且仍在 session 窗口内，变化才归因于 ChangeIP；消息可以先于 `operation.result` 到达。尚未接受 command 时发生的变化仍是自然变化，不会被错误归因。

若五分钟宽限后连续三次成功观察仍是触发前 IP，Agent 发送 `changeip.unchanged`，body 必须且只能包含 `command_id`、`address` 和 `observed_at`。网络失败不计确认次数。AkastrCloud 持久接纳后返回 `changeip.unchanged_ack`，body 为相同 `command_id` 和 `persisted=true`；45 分钟兜底只属于 Cloud 业务 session。

Agent 在本地只保留一个待确认 IPv4 事实或 ChangeIP 核对状态。`ip.snapshot`、`ip.observed` 和 `changeip.unchanged` 分别由相同 snapshot ID、observation ID 或 command ID 的 ack 清除；连接不可用时跨重连和进程重启重发。AkastrCloud 对已成功或未变化的 session 只投影一次终态；没有 Agent 快速结果时，业务 session 仍在 45 分钟到期时收敛，并同步终结尚未 accepted 的 command。

## 自然 IPv4 变化

没有活动 ChangeIP session 的 `ip.observed` 是自然变化。AkastrCloud 应用既有私聊订阅条件并重置该 IP 代际的 IPQuality 缓存；协议没有 Telegram channel delivery。

## 安全边界

- 协议固定为 `2026-08-20.v5`，不自动降级，也不接受协议之外的字段。
- SOCKS5 capability 只允许 `port`，不接受地址来源或自定义主机名字段。
- bootstrap/enrollment 使用机器 token，WSS 与自动更新检查只使用本地 Ed25519 private key；机器 token 不进入 WSS query、frame、更新请求或服务端日志。
- 后台安装命令可以包含长期机器 token，但不得包含 SOCKS5 password 或 ChangeIP bearer；token 不得写入 URL。
- capability list、journal 和日志不得含密码或脚本输出。
- 公网 IPv4 字段拒绝 private、loopback、link-local、CGNAT、文档/基准测试、组播和保留网段。
- Agent 不实现任意命令、远程 shell 或 HTTP 控制端点。
- 修改认证、消息字段、持久 payload 或发布边界时，必须与 AkastrCloud 侧按 ADR 0024 一并批准和实现。
