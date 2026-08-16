# Akastr Agent 协议 `2026-08-16.v3`

AkastrCloud 提供 HTTPS enrollment endpoint 和仅供 Agent 主动连接的 WSS 控制路由。每个 JSON envelope 必须且只能包含 `protocol`、`message_id`、`type`、`sent_at` 和 `body`；text frame 最大 64 KiB。未知字段、未知 message type、未知 capability 字段、binary frame、无效 UUID 和未来协议版本均会失败关闭。

## Enrollment 与身份认证

管理员先在 AkastrCloud 后台创建持久节点并填写全部参数。后台签发 32-byte canonical base64url 机器 token，以 token 和节点 UUID 加密 provider 配置，并生成 v0.8.1 一键命令。Agent 以节点 UUID 和机器 token 从 `POST /internal/agents/bootstrap` 获取 nonce/ciphertext，在本机认证解密并生成 root-only 文件。随后 `akastr-agent enroll` 每次生成新的 Ed25519 keypair，通过 HTTPS 发送机器 token、raw 32-byte public key、Agent version 和不含秘密的 capability list。注册成功后节点删除本机 token 副本，主控保留加密 bootstrap，供同一节点重装；private key 从不发送给主控。主控在该节点存在 pending、offered 或 accepted command 时以 `agent_node_busy` 拒绝注册；注册成功会替换公钥并断开旧 WSS。

机器 token 是长期安装凭据，不是 WSS bearer。主控只保存 SHA-256 hash、认证加密的可恢复 token 和密封 bootstrap。管理员可审计地重新显示安装命令，也可轮换 token；轮换在一个事务中更新 token hash、可恢复密文和 bootstrap 密文。再次注册同一节点会替换公钥并断开旧 WSS 连接。删除节点会永久删除身份、bootstrap 和已完成 command 记录；存在未完成 command 时拒绝删除。

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

Agent 发送 `auth.response` 并收到 `auth.accepted` 后发送 `agent.hello`；只有收到 `hello.accepted`，连接才进入 ready。相同节点的新认证连接会替换旧连接。

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
- `command_type`：v0.8.1 runtime 只接受 `changeip.execute` 和 `ipquality.execute`；
- `payload_version=1` 与对应类型的严格 payload；
- `not_before` 和 `expires_at`。

Agent 只有在当前时间已达到 `not_before`、尚未到 `expires_at` 且进程级更新 lease 空闲时，才先取得 operation lease 并回复 `operation.accepted`。只有主控再发送 `operation.accepted_ack` 且 `accepted=true`，Agent 才持久化 running 状态并调用本地已配置 provider。终态先持久化，再释放 operation lease 并通过 `operation.result` 发送；AkastrCloud 只接受数据库状态已经是 `accepted` 的首个终态，在数据库事务和下游事件接纳成功后发送 `operation.result_ack`。终态重放仍按原 command ID 幂等接纳。

断线后 offer 和 result 都可能重复。相同 `command_id` 的本地终态只会重放，不会再次执行。payload 不得选择 program、argv、shell fragment、文件、凭据或任意 URL。

### `changeip.execute`

payload 只包含 `expected_ipv4`。Agent 在调用 provider 前观察当前公网 IPv4：不匹配时返回 `stale_expected_ipv4`，不执行 provider。provider 使用本机配置中的固定 `program` 与 `args`，其 stdout/stderr 不进入协议或日志。

provider 零退出并不等于换 IP 成功。之后观察到新地址才返回 `ipv4_changed`；反复看到旧地址返回 `ipv4_unchanged`；始终无法重新观察则返回 `ipv4_observe_timed_out`。其他稳定 code 包括 `start_failed`、`exited_nonzero`、`timed_out`、`cancelled`、`local_conflict` 和状态恢复相关错误。

### `ipquality.execute`

payload 只包含 `target_server_id`、`expected_ipv4`、不含秘密的 `proxy_port`、本地 `proxy_profile_id` 和 `script_version`。Runner 直接把 `expected_ipv4` 作为 SOCKS5 地址，不存在另一个 hostname/IP 字段。SOCKS5 username/password 只存在 Runner 的 root-only profile 文件中。

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

- 协议固定为 `2026-08-16.v3`，没有版本自动降级，也不接受 v1/v2 字段。
- v0.8.1 是 Agent release/bootstrap 版本；SOCKS5 capability 只允许 `port`，不接受 `address_source` 或 `advertised_host`。
- bootstrap/enrollment 使用机器 token，WSS 与自动更新检查只使用本地 Ed25519 private key；机器 token 不进入 WSS query、frame、更新请求或服务端日志。
- 后台安装命令可以包含长期机器 token，但不得包含 SOCKS5 password 或 ChangeIP bearer；token 不得写入 URL。
- capability list、journal 和日志不得含密码或脚本输出。
- Agent 不实现任意命令、远程 shell 或旧 IPChanger HTTP endpoint。
- 修改认证、消息字段、持久 payload 或 rollout 边界时，必须与 AkastrCloud 侧按 ADR 0024 一并批准和实现。
