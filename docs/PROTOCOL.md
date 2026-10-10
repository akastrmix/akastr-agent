# Akastr Agent 协议 `2026-10-10.v10`

AkastrCloud 提供 HTTPS enrollment endpoint 和仅供 Agent 主动连接的 WSS 控制路由。每个 JSON envelope 必须且只能包含 `protocol`、`message_id`、`type`、`sent_at` 和 `body`；text frame 最大 1 MiB。未知字段、未知 message type、未知上报 kind、binary frame、无效 UUID 和未来协议版本均会失败关闭；Agent 收到的消息、模块配置与目标中，必填字段为 null 视同缺失。

## Enrollment 与身份认证

管理员先在 AkastrCloud 后台创建持久节点并填写全部参数。后台签发 32-byte canonical base64url 机器 token，以 token 和节点 UUID 加密 `akastr-agent-bootstrap.v4` 信封中的节点配置（格式见下节），并生成版本化一键命令。Agent 以节点 UUID 和机器 token 从 `POST /internal/agents/bootstrap` 获取 nonce/ciphertext，在本机认证解密，并把这份明文原样保存为 root-only 的 slot 配置；Agent 没有第二种配置格式。

`POST /internal/agents/enroll` 请求必须且只能包含 `machine_token`、raw 32-byte `public_key`、当前批准的语义化 `agent_version`、正整数 `configuration_revision`。版本必须精确等于 Cloud 当前批准 release，revision 必须精确等于节点的 desired revision，否则分别返回 `agent_release_required` 或 `agent_configuration_stale`。每次安装都生成新的 Ed25519 keypair；注册成功即以新公钥替换该节点原有公钥，把 applied revision 推进到 desired revision，并断开既有 WSS。Agent 只在注册成功后才写入新 identity 并切换本机部署，注册失败时原部署继续运行；重跑同一安装命令即可修复。主控在该节点存在 pending、offered 或 accepted command，或 Target 对应服务器仍有 active ChangeIP session 时，以 `agent_node_busy` 拒绝注册。

机器 token 是长期安装凭据，不是 WSS bearer，Agent 不在磁盘上保存它。主控只保存 SHA-256 hash、认证加密的可恢复 token 和密封 bootstrap。HTTP ChangeIP 可含 `source_command`（最多 8192 字符），保存 Cloud 已解析校验的编辑原文，仅供回填，不作为 shell 执行；缺少该字段的既有配置仍有效。配置更新保持节点 ID、服务器绑定、机器 token 和 identity 不变；Cloud 管理员添加/编辑共用完整配置，空字符串不表示保留；Runner 省略项表示移除，停用服务器仅可保留原有凭据。主控在一个事务中解封并比较完整配置，无变化不推进 revision、不打断连接；真实变更才递增 desired revision 并重新密封 bootstrap，随后断开业务连接；新 revision ready 前禁止 preflight、command 创建与 offer。管理员可审计地重新显示安装命令，也可轮换 token；轮换只接受当前 bootstrap v4，并在一个事务中更新 token hash、可恢复密文和 bootstrap 密文。删除节点会永久删除身份、bootstrap 和已完成 command 记录；存在未完成 command 或 active ChangeIP session 时拒绝删除。节点永久丢失且遗留 accepted command 时，管理员可显式将其记录为执行结果未知，并撤销旧 identity、终结同节点其他未接受 command 后把节点重置为 pending；机器 token 与密封 bootstrap 保留，供重装机器注册新 identity。主控不会自动超时放弃 accepted command。

## 节点配置与模块

节点配置是 UTF-8 JSON：`schema_version=5`、正整数 `configuration_revision`、`agent_id`、`name`、`control_endpoint` 与 `modules`。`modules` 的每个键是一个启用的模块，缺少的键就是关闭；Agent 拒绝未知模块、模块内的未知或缺失字段。当前模块：

| 模块 | 字段 | 作用 |
|---|---|---|
| `ip_watch` | `interval_seconds`（10–300）、`ipv6` | 上报 `ip.address` 与 `changeip.unchanged` |
| `changeip` | `provider=http_bearer` 加 `url`、`bearer_token`、可选 `source_command`；或 `provider=command` 加 `program`、`args` | 执行 `changeip.execute`；需要 `ip_watch` |
| `socks5` | `port`、`username`、`password`（该代理的登录） | 只供 Cloud 读取；Agent 不运行代理 |
| `ipquality_runner` | 无字段（`{}`） | 执行 `ipquality.execute` |
| `xui` | `panel_url`、`username`、`password` | 按目标状态管理本机 3x-ui 的受管客户端，上报 `xui.snapshot` |

`xui` 的 `panel_url` 只允许 literal loopback IP 的 HTTP(S)，包含面板 base path，不含查询、fragment 或 URL 凭据，拒绝重定向；只对回环 HTTPS 允许自签名证书。3x-ui 自带的订阅服务应关闭：链接由 Cloud 生成。

Agent 只检查模块自身与技术依赖；哪些模块可以组合（例如绑定服务器的节点必须开 `ip_watch`、Runner 只开 `ipquality_runner`）由 Cloud 决定。节点做什么由 Cloud 自己保存的配置决定，Agent 不再回报能力清单：Cloud 只向已按包含该模块的配置完成 hello 的节点派发它的命令。新增能力只增加一个模块键，以及它的命令类型、目标状态或上报 kind；配置与程序总是一起更新，旧节点收不到新模块的配置，所以不需要改协议版本；改变已有消息的含义才需要新协议版本。

enrollment HTTPS 地址由 WSS 地址确定：`wss://<host>/internal/agents/ws` 对应 `https://<host>/internal/agents/enroll`。客户端不提供关闭 TLS 校验或绕过主机名校验的选项。

WSS 连接 query 中包含 `agent_id`。AkastrCloud 发送 `auth.challenge`，其中 nonce 为 32 bytes、有效期为 15 秒；有效期只由主控用自己的时钟判断，Agent 不用本机时钟复核，两边时钟相差不影响认证。Agent 对下面这些 UTF-8 行签名，末尾没有换行：

```text
akastr-agent-auth-v1
<agent_id>
<challenge_id>
<nonce>
<issued_at exactly as received>
<expires_at exactly as received>
```

Agent 发送 `auth.response` 并收到 `auth.accepted` 后发送 `agent.hello`；hello 必须且只能包含语义化 `agent_version` 与本地正整数 `configuration_revision`。版本不得低于密封 bootstrap 的最低版本，也不得高于 Cloud 当前批准 release，并须精确匹配 desired revision。校验后 Cloud 原子推进尚未收敛的 applied revision 与版本，按自己保存的该 revision 配置记下节点运行的模块，再返回 `hello.accepted` 并使连接进入 ready。不满足时 Cloud 以 close code `4001`（reason `agent update required`）关闭连接，Agent 随即检查更新。绑定服务器和不绑定服务器的节点都按模块组合校验，绑定节点可以同时运行换 IP 与 xui；主控只允许一个 active Runner。相同节点的新认证连接会替换既有连接。

## 更新检查

`POST /internal/agents/maintenance` 是唯一的更新通道。它独立于 WSS 业务协议，因此业务协议升级后，旧 Agent 仍能取得新版本。请求必须且只能包含 `agent_id`、`agent_version`、正整数 `configuration_revision`、`error_code`（空字符串，或 1–64 位小写稳定错误码）、32-byte `nonce`、`sent_at` 与 64-byte Ed25519 `signature`；只接受 active identity，时间与主控相差超过五分钟即拒绝。签名文本为以下 UTF-8 行，末尾没有换行，时间保持 Agent 发送的原文：

```text
akastr-agent-maintenance-v2
<agent_id>
<agent_version>
<configuration_revision>
<error_code>
<nonce>
<sent_at>
```

响应严格为 `akastr-agent-maintenance.v2`，必须且只能包含 `schema`、`status`（`current|busy|update_available`）、批准的 `version`、固定 GitHub release 地址 `binary_url` 与 `binary_sha256`、desired `configuration_revision` 和 `configuration`。版本与 revision 都和请求相同时为 `current`；否则节点已开始的工作未结束时为 `busy`，即存在已 accepted 的 command，或其 command 已被接受或已终结、仍在等待 IP 核对的 active ChangeIP session；尚未送达或未被接受的 command 不阻止更新，因为节点更新完成前它们本就无法送达，更新后重新下发。其余为 `update_available`。只有 `update_available` 且 revision 变化时，`configuration` 才是完整的节点配置（与 bootstrap 明文相同），其他情况为 `null`。主控不返回更低版本或更低 revision；Agent 版本高于批准版本时返回 409 `agent_release_required`。`error_code` 是本节点上一次未能应用目标的原因，主控把它记为节点的最近更新状态。签名与响应样例由双方测试共用，见 `internal/protocol/testdata/agent-protocol-v9.json`。

Agent 每 60 秒检查一次，并在业务连接每次结束或进入 ready 时立即检查。发布新版本会重启 Cloud 后端，所有连接断开重连，节点因此立即发现新版本；保存配置后主控断开该节点，节点重连时 hello 被拒绝，同样立即检查。

发现 `update_available` 后，Agent 把目标程序（复用 SHA-256 一致的本地文件，否则从 `binary_url` 下载并核对）与配置写入非活动 slot，再调用候选程序：`version` 必须输出目标版本，`prepare --config <slot>/config.json --agent-id <id> --revision <n>` 必须成功（Runner 在此取得固定版本的 IPQuality 脚本）。本地没有进行中的 operation 或 ChangeIP 核对时，Agent 先持久记录一次尝试，再以 `run --config <slot>/config.json` 原地替换进程。候选程序按普通流程连接，收到 `hello.accepted` 后先取得与安装器共用的锁，把 `current` 原子切到自己的 slot，再处理任何业务消息；重装正在进行时取锁失败，候选按切换失败退出；45 秒内未被接受、启动失败或切换失败时记录原因并退出，systemd 重新启动仍指向旧 slot 的 `current`。同一目标连续失败两次后暂停，六小时后自动再试一次；新版本、新配置或重跑安装命令都重新开始计数。

本节请求、响应与 `version`、`prepare`、`run` 三个 CLI 是跨版本稳定契约：旧 Agent 依靠它们升级到新 Agent。修改它们须按 Cloud ADR 0024 单独批准，并说明已部署节点的迁移方式。

## Operation

`operation.offer` 包含：

- `command_id`：稳定 UUID，也是执行幂等键与本地 journal key；
- `command_type`：runtime 只接受启用模块已登记的 `changeip.execute`、`ipquality.execute`；
- `payload_version=1` 与对应类型的严格 payload；
- `not_before` 和 `expires_at`。

Agent 在进程没有准备替换为更新候选时发送 `operation.accepted`；Cloud 只在 `not_before` 到达后才 offer 并自行判断过期，Agent 不用本机时钟复核这两个时间，但该消息只发起持久化握手，不代表本地已经获得执行权。主控只会在未过 `expires_at` 的 offered command 上首次建立 accepted；已经 accepted 的 command 即使原窗口已过仍返回 `operation.accepted_ack accepted=true`，从未 accepted 的过期 command 返回 `false`。Agent 只有收到 `true` 才调用本地已配置 provider，此后不再用本地时钟复核原 offer 截止时间。终态先持久化，再释放 operation lease 并通过 `operation.result` 发送；AkastrCloud 只接受数据库状态已经是 `accepted` 的首个终态，在数据库事务和下游事件接纳成功后发送 `operation.result_ack`。

已被主控接受的 command 不因原 `expires_at` 自动终结。节点重连时以及连接期间每 30 秒，主控继续发送相同 offer，并以 accepted ack 恢复执行权；这覆盖主控已提交 acceptance、但 ack 尚未到达节点便断线的窗口，也让节点已完成却未送达的结果在一个周期内重放。active ChangeIP journal 一律收敛为 `change_trigger_unknown`，recent 记录重放原终态，两者都不会再次执行 provider；首次 accepted ack 后尚无 journal 的 command 才开始一次本地执行。主控从未 accepted 的过期 command 不会获得执行权。

offer 和 result 都可能重复。相同 `command_id` 的本地终态只会重放，不会再次执行。payload 不得选择 program、argv、shell fragment、文件或任意 URL。凭据只允许下面列出的 IPQuality 临时登录，不得将面板账号密码放入 offer。

### `changeip.execute`

payload 只包含 `expected_ipv4`。Agent 在调用 provider 前观察当前公网 IPv4：不匹配时返回 `stale_expected_ipv4`，不执行 provider。Agent 先持久化 command 与该旧 IP 的核对状态，再调用本机固定 provider；stdout/stderr 不进入协议或日志。

HTTP API provider 只把状态码 `200` 作为明确成功；真实非 `200` 或请求建立前失败是明确失败。固定程序 provider 以退出码 `0` 为明确成功、非零为明确失败。明确成功返回 `change_triggered`；请求可能已送达但响应、进程或 WSS 因断网中断时返回 `change_trigger_unknown`。两种成功终态都只包含触发前 IPv4，不宣称地址已经变化，也不会重发 provider。其他对外稳定 code 包括 `http_status_not_200`、`request_failed`、`stale_expected_ipv4`、`ipv4_observe_failed`、`start_failed`、`exited_nonzero` 和 `reconciliation_state_failed`。

### `ipquality.execute`

payload 只包含 `expected_ipv4`、`proxy_port`、`proxy_username` 和 `proxy_password`；结果中的 `script_version` 由 Runner 报告实际执行的固定脚本版本。Runner 直接把 `expected_ipv4` 作为 SOCKS5 地址，不存在另一个 hostname/IP 或目标 ID 字段。登录来自目标节点的 `socks5` 模块，由 Cloud 在 offer 时解密放入；Runner 只在本次执行的内存中使用，不写入配置、操作日志或普通日志，Cloud 也不把 payload 存入数据库。
Runner 同一时间只允许一个 command。Cloud 给每次检测 20 分钟期限，不论是否已接单，到期即判失败；此后送达的结果仍返回 `persisted=true` 的 ack，但不再采用。每次执行前都重新校验脚本 SHA-256，通过 SOCKS5 做 IPv4 preflight，随后以固定参数执行：

```text
/bin/bash <script_path> -4 -n -x <local_socks5_relay_url>
```

本地 relay 只监听 `127.0.0.1` 的随机端口，并使用 payload 中的登录连接上游 SOCKS5。脚本结束后 Runner 再做 postflight；代理地址改变、预期 IPv4 过期或 checksum 不一致都会失败。

只有精确来源 `https://report.check.place/...`、无用户信息和显式端口的 URL 加成功 postflight 才是 `report_ready`，即使官方 IPv4-only Bash 进程返回非零；非零且无报告 URL 是 `script_failed`。输出上限为 2 MiB，超限返回 `script_output_too_large`。

## 目标状态

Operation 只用于不能重复执行的一次性动作。Cloud 拥有、经常变化、可以反复应用的设置（3x-ui 受管客户端，以后的防火墙规则等）用目标状态：Cloud 按 `(module, key)` 下发完整目标，Agent 反复对齐。

| 方向 | type | body |
|---|---|---|
| Cloud → Agent | `state.put` | `module`、`key`（1–128 位 `[A-Za-z0-9_.-]`）、`version`（1 到 2^53−1，同一节点的同一 `(module, key)` 永不重复使用，即使该 key 曾退役后重新出现）、`state`（JSON object） |
| Cloud → Agent | `state.keys` | `module`、`keys`（该模块当前完整的 key 列表，不重复） |
| Agent → Cloud | `state.status` | `module`、`key`、`version`、`error_code`（空串表示节点已处于该版本的目标；否则为稳定错误码） |

- 每次连接进入 ready，Cloud 先为每个 key 发送当前 `state.put`，再发送 `state.keys`；之后目标变化即发 `state.put`，key 集合变化时再发 `state.keys`。同一连接上的消息有序，后到的 `state.put` 取代先前的。
- Agent 只在内存中保存目标。进程启动后，在收到某模块的第一条 `state.keys` 之前不应用该模块，避免用不完整的集合删除仍被需要的东西；`state.keys` 未列出的 key 立即退役，模块应移除它拥有、却没有任何目标指名的东西。
- Cloud 退役一个 key 时，先下发该 key 的“空”目标（xui 为 `clients: []`），收到该版本无错误的 `state.status` 后才把它移出 `state.keys`，因此 Cloud 能确认节点上的东西已清理。
- 未启用模块的 state 消息、格式错误的消息结束连接。模块拒绝的目标不取代该 key 原有的目标，并以 `target_invalid` 回报；该 key 此前没有可用目标时，它覆盖的东西保持原样，不当作已退役而清理。
- Agent 在变化后约 2 秒、此后每 5 分钟以及失败后（某个 key 失败，或不属于任何 key 的清理失败）按 30 秒起、最多 5 分钟的退避重新应用，并在 `(version, error_code)` 与本会话上次发送的不同时发送 `state.status`；Cloud 只记录 version 等于当前版本的状态；version 不重复使用，所以退役前仍在途中的旧状态不会被当成新目标的结果。Agent 不为已退役的 key 发送状态。`state.*` 没有回执：丢失的消息由下一次 ready 时的全量重发补上。
- 目标状态不阻止 Agent 更新：重复应用同一目标不改变结果。

### `xui`

本模块适配原版 3x-ui **2.9.4**，Xray 固定为 **26.7.28**，不自动升级，不维护多版本兼容层；更换这些版本须先实测接口与真实连接行为。

key 是面板入站 ID（十进制正整数）。state 必须且只能包含 `clients`，每项必须且只能包含：

| 字段 | 含义 |
|---|---|
| `email` | `ak-` 加 1–61 位小写字母或数字；Cloud 以这一前缀拥有客户端 |
| `credential` | VLESS 为 UUID；Shadowsocks 2022 为按 method 的 16 或 32 字节标准 Base64；Hysteria2 为 16–256 字节文本 |
| `flow` | 空，或仅 VLESS 的 `xtls-rprx-vision` |
| `enable` | Cloud 的业务启停（到期、额度、手动停用等都在 Cloud 计算） |
| `total_bytes` | 写入 `totalGB` 的流量上限，0 为不限 |
| `reset_seq` | 每当 Cloud 要求清零一次流量就增加 |
| `comment` | 面板中显示的备注（客户与套餐），可为空，最多 200 个字符、不含控制字符；不进入 Xray |

七个字段都必须出现且不为 null；同一目标内 email 与 credential 都不重复。对齐规则：

- 只管理 email 以 `ak-` 开头的客户端，绝不改动其他客户端。面板按凭据（Shadowsocks 按 email）定位客户端，并作用于所有匹配项，所以受管客户端或目标的凭据与非受管客户端相同时，该入站返回 `xui_client_conflict`，不做任何写入；也不创建或修改入站。目标中缺少的受管客户端新建，字段不一致的合并更新（保留未托管字段），多余的删除；任何 key 都未指名的入站中的受管客户端也删除。面板拒绝删除入站的最后一个客户端，此时改为停用，视为一致。`expiryTime` 固定为 0。
- 已有上限且已用流量达到上限的客户端，即使目标 `enable=true` 也保持停用：面板会在流量耗尽时同时停用流量记录与客户端配置，写回启用只会被面板再次停用。提高上限或重置后自动恢复启用。
- 目标先与入站核对（协议、method、凭据与冲突），通过后才执行重置。`reset_seq` 大于本机已执行的序号时，先调用面板的重置流量，再记录序号（`/var/lib/akastr-agent/xui/state.json`）；写盘失败时记录留在内存，后续每轮先补写，不再重复重置；面板的重置只恢复流量记录的启用，客户端配置的启用随后按目标写入。重置与记录之间进程中断会再重置一次，最多丢失这一瞬间的计数。不存在的客户端只记录序号。
- 写入任何 Shadowsocks 2022 入站（只改 `comment` 的除外）前先在 `state.json` 记下“待重启”，本轮最后经面板原生 `restartXrayService` 重启一次 Xray，成功后清除：该组合的动态写入可能报告成功却不可用。重启失败或进程在重启前停止时，下一轮即使面板已一致也会补做重启。重启会使同机所有连接短暂中断。
- 目标为空时，面板中已不存在的入站视为一致（不报 `xui_inbound_missing`），使已删除入站的 key 也能完成退役。
- 有写入时重新读取面板核对，不一致返回 `xui_unconfirmed`。其他错误码：面板请求类（`xui_login_failed`、`xui_request_invalid`、`xui_request_unknown`、`xui_http_failed`、`xui_response_invalid`、`xui_business_failed`）、`xui_inbound_missing`、`xui_inbound_changed`（入站协议、method 与目标凭据不再匹配）、`xui_inbound_invalid`、`xui_client_conflict`、`xui_write_failed`、`xui_restart_failed`、`xui_state_failed`。

## 上报与回执

节点主动告知的事实统一用 `report` 消息，body 必须且只能包含 `report_id`（UUID）、`kind` 与 `data`；Cloud 持久接纳后回 `report.ack`，body 只包含相同 `report_id`。Agent 在收到回执前重发同一上报；重复或过期回执是无副作用的确认。Cloud 对格式正确但无法使用的上报（服务器已停用、IPv6 未开启、`changeip.unchanged` 指向主控没有的 command 等）记录日志并照常回执，不断开连接；只有数据库故障等暂时性失败才结束连接，由 Agent 重连重试。新增的上报能力只增加一种 `kind`。当前 kind：

| kind | data |
|---|---|
| `ip.address` | `family`（`ipv4`/`ipv6`）、`address`、`observed_at`、`command_id`（UUID 或 null） |
| `changeip.unchanged` | `command_id`、`address`、`observed_at` |
| `xui.snapshot` | `inbounds`、`traffic`，见下 |

`xui.snapshot` 每分钟读取一次面板，内容变化时发送，至少每 10 分钟发送一次，每次连接 ready 后立即发送；只保留最新一份，未确认的旧快照被新快照取代。`inbounds` 每项为 `id`、`remark`、`protocol`、`listen`、`port`、`enable`、`settings`、`stream_settings`（后两者为 JSON object）；`traffic` 每项为受管客户端的 `email`、`up`、`down`（bytes）与本机已执行的 `reset_seq`，Cloud 只采用与其当前序号相同的计数。

秘密集中的部分按白名单复制，3x-ui 以后新增的字段也不会带出：settings 只保留 VLESS 的 `encryption`，Shadowsocks 的 `method`、`password`、`network`，Hysteria 的 `version`；stream_settings 的 `tlsSettings` 只保留 `serverName`、`alpn` 和 `settings` 中的 `fingerprint`、`allowInsecure`、`echConfigList`，`realitySettings` 只保留 `serverNames`、`shortIds` 和 `settings` 中的 `publicKey`、`fingerprint`、`serverName`、`spiderX`、`mldsa65Verify`；其余传输字段只保留 `externalProxy`、`finalmask`（客户端也须使用的混淆设置）和 `tcpSettings.header.type`，其他（例如服务端的 `hysteriaSettings.auth`）都不上报。其他协议的 settings 为空对象，stream_settings 只有 `network` 与 `security`。其他用户的凭据、服务端私钥与密码、ECH 服务端密钥、Reality 种子与证书不离开节点。

## IP 观察、ChangeIP 与 IPv4 核对

节点是自身地址的权威，Cloud 保存地址历史。`ip.address` 表示“节点现在的地址”：Cloud 采用它，与自己所持地址不同时以 Cloud 所持地址为 previous 记录一次变化；不因地址不一致或时间而拒绝。地址必须与 family 匹配且为对应协议族的公网地址；IPv6 在比较和持久化前规范化文本，并按固定 IANA special-purpose policy 拒绝非 globally reachable 地址，Cloud 还要求节点配置开启了 IPv6 观察。节点时间（`observed_at`、`checked_at`，包括 `operation.result` 中的）只用于同一节点事实的排序与展示，超前主控或不晚于上一条事实时改用主控时间。

每个 Agent 进程与每次业务连接进入 ready 后，Agent 都先重新观察并上报当前地址，此后只在地址变化时上报。Agent 不在本地保存地址；未确认的上报被新会话的上报取代。IPv4 地址上报在当前会话被确认前，Agent 不接受新的 ChangeIP，Cloud 收到 IPv4 上报后立即重新下发待执行 command。IPv4 与 IPv6 各自独立；IPv6 无地址、探测失败或暂时不可达不发送消失事件，也不影响 IPv4、ChangeIP、IPQuality 或 SOCKS5。

`command_id` 只用于 IPv4：节点为某次 `changeip.execute` 触发 provider 后，核对期间看到的地址变化带上该 command，Cloud 在该次换 IP 仍进行且起始地址就是自己所持地址时把它记为这次换 IP 的结果；消息可以先于 `operation.result` 到达。不带 `command_id` 的变化都是自然变化，即使同时有换 IP 在进行；带了但对不上的变化也按自然变化记录并记日志。

核对期间 Agent 每 10 秒观察一次；若触发两分钟后连续两次成功观察仍是触发前 IP，Agent 发送 `changeip.unchanged`。网络失败不计确认次数。核对状态持久保存在节点上，直到它的结果（带 `command_id` 的 `ip.address` 或 `changeip.unchanged`）被确认；进程重启后重发同一结果，不会变成自然变化。AkastrCloud 对已成功或未变化的换 IP 只投影一次终态；没有 Agent 结果时，业务换 IP 仍在 45 分钟到期时收敛，并同步终结尚未 accepted 的 command。

## 自然 IP 变化

自然 IPv4 变化按既有私聊订阅条件通知；IPQuality 缓存以 IP 变化记录为界自然失效；协议没有 Telegram channel delivery。IPv6 变化始终是自然变化，不重置 IPQuality，只写入 `/iplog` 数据源，不发送主动通知。

## 安全边界

- 协议固定为 `2026-10-10.v10`，不自动降级，也不接受协议之外的字段。
- bootstrap/enrollment 使用机器 token，WSS 与更新检查只使用本地 Ed25519 private key；机器 token 不进入 WSS query、frame、更新请求或服务端日志。
- 后台安装命令可以包含长期机器 token，但不得包含 SOCKS5 password 或 ChangeIP bearer；token 不得写入 URL。
- 上报和普通日志不得含密码、原始链接或脚本输出。受管客户端凭据只出现在 Cloud 下发的 `state.put` 中，Agent 不写入磁盘或日志。
- 公网 IPv4 字段拒绝 private、loopback、link-local、CGNAT、文档/基准测试、组播和保留网段。
- Agent 不实现任意命令、远程 shell 或 HTTP 控制端点。
- 修改认证、消息字段、持久 payload 或发布边界时，仍须按 Cloud ADR 0024 在实施前批准；共享契约变化核对双方实现，仅修改实际受影响的一侧或双方。跨仓库验证范围见 Cloud `docs/AGENT_INTEGRATION.md`。
