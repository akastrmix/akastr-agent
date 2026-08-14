# Akastr Agent 协议 `2026-08-13.v1`

AkastrCloud 提供 HTTPS enrollment endpoint 和仅供 Agent 主动连接的 WSS 控制路由。每个 JSON envelope 必须且只能包含 `protocol`、`message_id`、`type`、`sent_at` 和 `body`；text frame 最大 64 KiB。未知字段、未知 message type、未知 capability 字段、binary frame、无效 UUID 和未来协议版本均会失败关闭。

## Enrollment 与身份认证

管理员先在 AkastrCloud 后台创建 installation。后台签发一次性的 32-byte canonical base64url token，并生成预填 installation UUID、mode 和 WSS endpoint 的 v0.2.0 bootstrap 命令；token 不嵌入命令。交互安装器从 TTY 静默读取 token，只在 enrollment 前写入 root-only 文件。`akastr-agent enroll` 在本地生成 Ed25519 keypair，通过 HTTPS 发送 token、raw 32-byte public key、Agent version 和不含秘密的 capability list，然后把服务端返回的 installation UUID 与 private key 原子写入 `credential_file`。事务成功后 token 立即失效并从节点删除，private key 从不发送给主控。

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

Agent 发送 `auth.response` 并收到 `auth.accepted` 后发送 `agent.hello`；只有收到 `hello.accepted`，连接才进入 ready。相同 installation 的新认证连接会替换旧连接。

## Operation

`operation.offer` 包含：

- `command_id`：稳定 UUID，也是执行幂等键与本地 journal key；
- `command_type`：v0.2.0 runtime 只接受 `changeip.execute` 和 `ipquality.execute`；
- `payload_version=1` 与对应类型的严格 payload；
- `not_before` 和 `expires_at`。

Agent 先回复 `operation.accepted`。只有主控再发送 `operation.accepted_ack` 且 `accepted=true`，Agent 才持久化 running 状态并调用本地已配置 provider。终态先持久化，再通过 `operation.result` 发送；AkastrCloud 仅在数据库事务和下游事件接纳成功后发送 `operation.result_ack`。

断线后 offer 和 result 都可能重复。相同 `command_id` 的本地终态只会重放，不会再次执行。payload 不得选择 program、argv、shell fragment、文件、凭据或任意 URL。

### `changeip.execute`

payload 只包含 `expected_ipv4`。Agent 在调用 provider 前观察当前公网 IPv4：不匹配时返回 `stale_expected_ipv4`，不执行 provider。provider 使用本机配置中的固定 `program` 与 `args`，其 stdout/stderr 不进入协议或日志。

provider 零退出并不等于换 IP 成功。之后观察到新地址才返回 `ipv4_changed`；反复看到旧地址返回 `ipv4_unchanged`；始终无法重新观察则返回 `ipv4_observe_timed_out`。其他稳定 code 包括 `start_failed`、`exited_nonzero`、`timed_out`、`cancelled`、`local_conflict` 和状态恢复相关错误。

### `ipquality.execute`

payload 只包含 `target_server_id`、`expected_ipv4`、不含秘密的 `proxy_host` / `proxy_port`、本地 `proxy_profile_id` 和 `script_version`。SOCKS5 username/password 只存在 Runner 的 root-only profile 文件中。

Runner 同一时间只允许一个 command。每次执行前都重新校验脚本 SHA-256，通过 SOCKS5 做 IPv4 preflight，随后以固定参数执行：

```text
/bin/bash <script_path> -4 -n -x <local_socks5_relay_url>
```

本地 relay 只监听 `127.0.0.1` 的随机端口，并使用 profile 中的凭据连接上游 SOCKS5。脚本结束后 Runner 再做 postflight；代理地址改变、预期 IPv4 过期、checksum 不一致或找不到 profile 都会失败。

有效 `https://report.check.place/...` URL 加成功 postflight 是 `report_ready`，即使官方 IPv4-only Bash 进程返回非零；非零且无报告 URL 是 `script_failed`。输出上限为 2 MiB，超限返回 `script_output_too_large`。

## 自然 IPv4 事件

`ip.observed` 表示一次自然 IPv4 变化，包含 `observation_id`、`family=ipv4`、`previous_address`、`address` 和 `observed_at`。首次观察只建立本地 baseline，不产生消息。

Agent 在本地只保留一个 pending transition，收到相同 `observation_id` 且 `persisted=true` 的 `ip.observed_ack` 后才清除；连接不可用时会继续保留并在重连后重发。AkastrCloud 随后应用既有私聊订阅条件并重置该 IP 代际的 IPQuality 缓存。协议没有 Telegram channel delivery。

## 安全与兼容性

- 协议固定为 `2026-08-13.v1`，没有版本自动降级。
- v0.2.0 是 Agent release/bootstrap 版本，不会改变线协议标识。
- 认证只使用一次性 token 和本地 Ed25519 private key，不使用长期 bearer token 建立 WSS。
- 后台生成的安装命令不得包含一次性 token、SOCKS5 password 或 ChangeIP bearer。
- capability list、journal 和日志不得含密码或脚本输出。
- Agent 不实现任意命令、远程 shell 或旧 IPChanger HTTP endpoint。
- 修改认证、消息字段、持久 payload 或 rollout 边界时，必须与 AkastrCloud 侧按 ADR 0024 一并批准和实现。
