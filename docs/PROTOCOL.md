# Akastr Agent 协议 `2026-11-01.v12`

AkastrCloud 提供 HTTPS enrollment 端点与供 Agent 主动连接的 WSS 控制路由。JSON envelope 必须且只能含 `protocol`、`message_id`、`type`、`sent_at`、`body`；text frame 上限 `1 MiB`。未知字段、未知 `type`/上报 `kind`、binary frame、无效 UUID、未知协议版本及必填字段为 `null` 一律失败关闭。

> **分段读取索引**（单模块任务只用行范围按需读取对应小节）：
> Enrollment与认证(L8) · 节点配置与模块(L24) · 更新检查(L44) · Operation(L62) · changeip(L68) · ipquality(L74) · 目标状态(L86) · xui(L99) · rate_limit(L117) · routing(L123) · firewall(L137) · 诊断(L157) · 上报与回执(L165) · IP观察与核对(L185) · 安全边界(L191)

## Enrollment 与身份认证

- **Bootstrap 与注册**：`POST /internal/agents/bootstrap` 使用节点 UUID 与 32-byte canonical base64url 机器 token 取得以 `akastr-agent-bootstrap.v4` 密封的配置（含 `source_command` 可选编辑原文 ≤8192 字符），解密明文即 root-only slot 配置。`POST /internal/agents/enroll` 必须且只能含 `machine_token`、raw 32-byte `public_key`、批准版本 `agent_version` 与正整数 `configuration_revision`；不匹配批准版本/desired revision 分别返回 `agent_release_required` / `agent_configuration_stale`，存在未终结 command 或 active ChangeIP session 时返回 `agent_node_busy`。成功即替换公钥、推进 applied revision、断开旧 WSS；同事务在 Cloud 轮换 token、删除或永久丢失强制重置详见 [Cloud API](https://github.com/akastrmix/AkastrCloud/blob/main/docs/API.md#agent-控制通道)。
- **WSS 握手**：`wss://<host>/internal/agents/ws?agent_id=<UUID>` 对应 `https://<host>/internal/agents/enroll`，禁止跳过 TLS/主机名校验。`auth.challenge` 下发 32-byte nonce（有效期 15 秒，只由主控时钟判定），Agent 签名下列无末尾换行的 UTF-8 行后回 `auth.response`：

```text
akastr-agent-auth-v1
<agent_id>
<challenge_id>
<nonce>
<issued_at exactly as received>
<expires_at exactly as received>
```

- **Hello**：收到 `auth.accepted` 后发 `agent.hello`（仅含 `agent_version`、`configuration_revision`）。version 须介于密封 bootstrap 最低版本与当前批准 release 之间且精确匹配 desired revision；不满足以 close `4001`（`agent update required`）断开触发更新检查。新认证连接替换旧连接。

## 节点配置与模块

节点配置为 UTF-8 JSON：`schema_version=5`、正整数 `configuration_revision`、`agent_id`、`name`、`control_endpoint`、`modules`（省略即关闭；拒绝未知模块及未知/缺失字段）：

| 模块 | 字段 | 作用与约束 |
|---|---|---|
| `ip_watch` | `interval_seconds`（10–300）、`ipv6` | 上报 `ip.address` 与 `changeip.unchanged` |
| `changeip` | `provider=http_bearer`（`url`、`bearer_token`、可选 `source_command`）；或 `provider=command`（`program`） | 执行 `changeip.execute`；依赖 `ip_watch`；无参固定程序 |
| `socks5` | `port`、`username`、`password` | 纯 Cloud 凭据段（Agent 不读、不运行代理） |
| `ipquality_runner` | `{}` | 执行 `ipquality.execute` |
| `xui` | `panel_url`、`username`、`password` | 管理 `ak-` 客户端并报 `xui.snapshot`（`panel_url` 须 literal loopback HTTP(S)+base path，无 query/fragment/URL 凭据、禁重定向，仅回环 HTTPS 允自签） |
| `rate_limit` | `{}` | 限制受管用户下载；依赖 `xui` |
| `connection_records` | `{}` | 报 `connection_records.batch`；依赖 `xui` |
| `routing` | `{}` | 管理 Xray 路由与出站；依赖 `xui` |
| `diagnostics` | `{}` | 只读 systemd 沙盒回答 `diagnostics.run` |
| `firewall` | `{}` | 管理 `inet akastr_firewall` 与转发，报 `firewall.status`（须 `/usr/sbin/nft`） |
| `metrics` | `{}` | 每分钟报 `metrics.sample` |

Agent 只校验自身字段与技术依赖，不回报能力清单；新增能力只增模块键与消息，不改已有消息语义时不升协议版本。

## 更新检查

`POST /internal/agents/maintenance` 独立于 WSS 业务协议。请求只含 `agent_id`、`agent_version`、正整数 `configuration_revision`、`error_code`（空串或 1–64 位小写稳定错误码）、32-byte `nonce`、`sent_at`（偏差 >5 分钟拒绝）与 64-byte Ed25519 `signature`，签名原文为无末尾换行的 UTF-8 行：

```text
akastr-agent-maintenance-v2
<agent_id>
<agent_version>
<configuration_revision>
<error_code>
<nonce>
<sent_at>
```

响应严格为 `akastr-agent-maintenance.v2`：`schema`、`status`（`current|busy|update_available`）、批准 `version`、`binary_url`、`binary_sha256`、`configuration_revision`、`configuration`。版本与 revision 都相同回 `current`；否则存在 accepted command 或等待 IP 核对的 active ChangeIP session 时回 `busy`（未送达或未接受的 command 不阻塞）；其余回 `update_available`（仅 revision 变化时携带完整 `configuration`，否则 `null`）。版本高于批准值回 409 `agent_release_required`；双端测试共用 `internal/protocol/testdata/agent-protocol-v12.json`。

Agent 每 60 秒及连接断开/ready 时检查；下载核对写入非活动 slot 并执行 `version` / `prepare` 后原地 `run` 切换（细节与失败退避见 [ARCHITECTURE.md](ARCHITECTURE.md#8-安装与自动更新)）。本节请求/响应与 `version`、`prepare`、`run` 三个 CLI 属跨版本稳定契约，修改须按 Cloud ADR 0024 批准并给出迁移方案。

## Operation

`operation.offer` 含 `command_id`（幂等/journal 键）、`command_type`、`payload_version=1`、`payload`、`not_before`、`expires_at`（只由 Cloud 判断）。非更新重启中回 `operation.accepted`；Cloud 已接单即使过原窗也回 `operation.accepted_ack accepted=true`（未接单过期回 `false`），收到 `true` 才执行。终态先写本地 journal（最多 64 条、单条 result ≤8 KiB，不存敏感凭据）再发 `operation.result` 等 `operation.result_ack`。重连及每 30 秒重发未终结 offer；重启对 active ChangeIP 判 `change_trigger_unknown`，recent 直接重放不重执行。

exclusive group 互斥（`changeip.execute` → `target-network`，`ipquality.execute` → `ipquality-runner`）：组内忙直接回 `failed` + `operation_busy`（不写 journal）。payload 禁传 program/argv/shell/文件/URL/面板密码。

### `changeip.execute`

payload 只含 `expected_ipv4`；调用前先测公网 IPv4，不符回 `stale_expected_ipv4` 不执行。执行前写核对文件；固定程序 stdout/stderr 不进协议或日志。

HTTP `200` 或固定程序退出 `0` 回 `change_triggered`；被信号终止或断网/中断回 `change_trigger_unknown`（都不代表地址已变且不重发）。其他错误码：`http_status_not_200`、`request_failed`、`stale_expected_ipv4`、`ipv4_observe_failed`、`start_failed`、`exited_nonzero`、`reconciliation_state_failed`。其中 **`http_status_not_200` 另附 `provider_reply`**：`HTTP <状态码>` 加回复正文前 300 字符（脱敏 bearer token，控制字符与连续空白合为空格，截断尾缀 `…`，空正文只带状态码）；其余结果无此字段。

### `ipquality.execute`

payload 含 `expected_ipv4`、`proxy_port`、`proxy_username`、`proxy_password`（内存专用不落盘），result 含 `script_version`。Cloud 派发 60 秒未接单过期、接单 20 分钟超时（晚到 result 仍回 `persisted=true` ack）。

Runner 单并发，执行前校验固定脚本 SHA-256 与 SOCKS5 IPv4 preflight，按固定命令执行：

```text
/bin/bash <script_path> -4 -n -x <local_socks5_relay_url>
```

本地 relay 只听 `127.0.0.1` 随机端口连上游 SOCKS5；脚本后做 postflight（地址/预期 IP/checksum 不符即失败）。精确来源 `https://report.check.place/...`（无 userinfo 和显式端口）且 postflight 成功即 `report_ready`（哪怕 IPv4-only 脚本非零退出）；否则 `script_failed`。输出上限 `2 MiB`（`script_output_too_large`）。

## 目标状态

经常变化且可幂等应用的配置按 `(module, key)` 全量对齐：

| 方向 | type | body |
|---|---|---|
| Cloud → Agent | `state.put` | `module`、`key`（1–128 位 `[A-Za-z0-9_.-]`）、`version`（1..`2^53−1`，同 node 同 `(module, key)` 永不复用）、`state`（JSON object） |
| Cloud → Agent | `state.keys` | `module`、`keys`（完整不重复 key 列表） |
| Agent → Cloud | `state.status` | `module`、`key`、`version`、`error_code`（空串表一致） |

- **握手与退役**：每次 ready 先逐 key 发 `state.put`，再发 `state.keys`；Agent 仅内存持有，收到模块首条 `state.keys` 前不应用，未列出的 key 立即清理。退役先发“空”目标，收到无错 `state.status` 再移出 `state.keys`。格式错或未启用模块消息即断连；拒绝的目标回 `target_invalid` 且保留旧可用目标（无旧目标时不清理）。
- **重试与上报**：Agent 在变化后 ~2 秒、每 5 分钟及失败退避（30 秒起至 5 分钟）重算，`(version, error_code)` 变更时发 `state.status`（Cloud 仅认当前版本，已退役不发；丢失靠下次 ready 重发；不阻断自更新）。

### `xui`

适配原版 3x-ui **2.9.4** / Xray **26.7.28**。key 为面板入站 ID（正整数），`state` 仅含 `clients`，每项 7 字段均必填非 `null` 且同目标内 `email`/`credential` 不重复：

| 字段 | 含义 |
|---|---|
| `email` | `ak-` 加 1–61 位小写字母/数字 |
| `credential` | VLESS UUID；SS2022 16/32B 标准 Base64；Hysteria2 16–256B 文本 |
| `flow` | 空串或 `xtls-rprx-vision` |
| `enable` | 业务启停 |
| `total_bytes` | 写 `totalGB` 字节上限，0 为不限 |
| `reset_seq` | 流量清零递增序号 |
| `comment` | 面板备注，可为空，≤200 字符不含控制符 |

- **对齐规则**：只管理 `ak-` 前缀客户端；凭据/email 与非受管项冲突整入站返回 `xui_client_conflict` 且不写入；不建改入站，`expiryTime` 固定 0；面板拒删最后一项时改停用并视同一致；超额耗尽客户端即使 `enable=true` 也保持停用直到提额或重置；目标为空时已删入站视同一致（不报 `xui_inbound_missing`）。
- **重置与 SS2022**：通过入站核对后，若 `reset_seq` 大于本机 `StateDir/xui/state.json` 记录，先调重置再落盘序号；写入任何 SS2022 入站（哪怕只改 `comment`）前先写盘待重启标记，本轮末经面板 `restartXrayService` 重启 Xray。写后回读核对，异常错误码：`xui_login_failed`、`xui_request_invalid`、`xui_request_unknown`、`xui_http_failed`、`xui_response_invalid`、`xui_business_failed`、`xui_inbound_missing`、`xui_inbound_changed`、`xui_inbound_invalid`、`xui_client_conflict`、`xui_write_failed`、`xui_restart_failed`、`xui_state_failed`、`xui_unconfirmed`。
- **访问日志与连接账本契约**：须开启访问日志（`log.access` 非 `none` 且 `maskAddress` 为空）。Agent 仅在内存中把行 1 对 1 归属至 TCP 连接或 UDP 流：凡涉及进程启动前沿续、同时收 TCP+UDP 的入站、多用户争议或同 `来源地址:端口` 连同本机双监听导致的归属歧义，一律保持**不归属**（宁漏不误算或混算给他人）；可走 UDP 入站未被 TCP 认领的行作 2 小时 UDP 流（内核与日志跟读、副本补读细节见 [ARCHITECTURE.md](ARCHITECTURE.md#4-包职责)）。未开日志只影响连接认领，不影响面板对齐与快照。

### `rate_limit`

key 为自动限速单位或 `manual.` 手动限速（Cloud ADR 0108），`state` 仅含 `down_mbps`（0–10000 整数，0 为不限，退役发 `{"down_mbps":0,"emails":[]}`）与 `emails`（不重复 `ak-` 列表）。同用户跨组取较低 `down_mbps`。

- 从 `xui` 账本取 TCP 连接与 UDP 流，在默认路由网卡 `clsact egress` 挂 `flower`（TCP 匹配 `本机:端口 + 客户端:端口`，UDP 匹配 `入站端口 + 客户端:端口`；IPv4 优先级 49001、IPv6 49002，同组共用 `police` `0xAC000001+`，突发 ~100 ms / ≥64 KiB），每 0.5 秒同步，仅限下载；新连接收敛约 <1 秒，超过滤上限不限。日志/连接信息不可用继续限已知连接并报 `rate_limit_log_unavailable`；`tc` 失败全撤本模块规则并报 `rate_limit_apply_failed`。

### `routing`

仅 key `node`（空目标 `{"default_outbound":"direct","outbounds":[],"rules":[]}`，开模块时不退役），`state` 含：

| 字段 | 含义 |
|---|---|
| `default_outbound` | 无命中兜底：`direct` 或 `outbounds` 中标签 |
| `outbounds` | 受管出站（`tag` `ak-o-`+1..100 小写字母/数字/`-` 不重复；`config` 含 `protocol` 不含 `tag`） |
| `rules` | 有序规则：`inbounds`、`users`、`sources`（写为 Xray 2.9.4 的 `sourceIP`）、`domains`、`ips`、`protocols`、`ports`、`networks` 至少其一（未知字段拒整份）；必填 `outbound` 为 `direct`/`blocked`/受管标签或节点自有出站（1–100 `[A-Za-z0-9._-]`、非 `ak-`/api，不存在时命中直接断开，Xray 26.7.28 / Cloud ADR 0111） |

- 先写 3x-ui 模板中 `outbounds`（`[default_outbound, direct, blocked, ...]`）、`routing.rules`（面板 `api` 规则 + 目标规则，删 `balancers`）与 `api.services`（加 `RoutingService`），保留 `domainStrategy` 等其余字段；写入前经 `xray run -test`（剥离入站与日志字段）校验并写回核对。
- 无 `RoutingService`、首出站变更或 `direct`/`blocked` 偏离时经面板重启 Xray；否则 `xray api` 热更新（不断连）。规则更新中途清空失败且 api 失联时按已校验模板重启一次 Xray。关闭或卸载保留最后配置交还面板（Cloud ADR 0099）。
- 错误码：`routing_inbound_missing`、`routing_config_invalid`、`routing_panel_failed`、`routing_xray_unavailable`、`routing_apply_failed`、`routing_restart_failed`（输出不上报、不写日志）。

### `firewall`

仅 key `node`（开模块时不退役；空目标 `{"mode":"off","retry":0,"sets":{},"blocked":null,"exempt":null,"ceilings":[],"rules":[],"forwards":[]}`；最大目标 ~220 KiB），`state` 仅含 8 项：

| 字段 | 含义与上限 |
|---|---|
| `mode` | `off`（有转发只装转发三链，无转发删表）、`observe`（封禁与 `rules` 生效，`ceilings`/兜底只记不丢）、`enforce` |
| `retry` | `0..2^53−1`（后台点重试递增以强制重装失败目标） |
| `sets` | ≤64 个命名集合 `[a-z0-9_]{1,32}`，含规范 `addresses` 与小写 `domains`；总计 ≤4096 地址、≤64 域名；空集不匹配 |
| `blocked` / `exempt` | 封禁集合名 / 豁免封禁集合名（或 `null`） |
| `ceilings` | ≤256 条入站上限：`protocol`（`tcp`/`udp`）、`ports`、1–64 `sources` 集合名（目的端口在内且来源不在任一集合则拦） |
| `rules` | ≤512 条有序规则：`action`（`allow`/`drop`）、`sources`（`"any"` 或 1–64 集合名，非空）、`protocol`（`tcp`/`udp`/`ping`/`any`）、`ports` |
| `forwards` | ≤256 条 IPv4 转发：`protocol`（`tcp`/`udp`）、`port`、`to`（规范单 IPv4，非 `0.0.0.0`/回环/组播/广播）、`to_port`（同协议+端口唯一） |

`ports` 为 1–64 段 `[起, 止]`（`ping`/`any` 须为空数组）。

- **表与五链（`inet akastr_firewall`，不读改他表/`sysctl`/ssh）**：1) `prerouting filter`（有 `blocked` 时：放行回环，来自 `blocked` 且不在 `exempt` 的原始方向丢弃）；2) `nat prerouting`（IPv4 且 `fib daddr type local`，注释带 `akastr:<mode>:<digest>`，按映射改 DNAT）；3) `nat postrouting`（仅对本模块转发连接做 masquerade）；4) `forward`（非网桥默认路由网卡同进同出且未 NAT 的过路包丢弃）；5) `input filter`（回环放行 → 保底 ICMP/ICMPv6 RFC4890/已建立/DHCPv6，不含 ping → `ceilings` → `rules` 首条命中 → 兜底）。转发只受 1、4 约束。
- **命中集合**：`ceilings`（IPv4/v6 各 ≤512）与兜底（TCP/UDP `socket` 匹配后 ≤512、ICMP/v6 与其他协议各 ≤64）1 小时无命中过期，整表重装清零。
- **应用顺序与误差边界**：`off`+无转发即删表清状态；需转发且 `ip_forward=0` 时经一次性 root 单元开并按 `boot_id` 记入 `StateDir/firewall/state.json`；成员（域名每 2 分钟解析最多取 32 IP、失败保留旧址并标 `failed`；转发映射与默认网卡）只在表已存在时原子替换且优先执行；骨架/模式变化时对比表摘要与失败摘要，不足 `131072 × GiB` 先调高 `nf_conntrack_max`；有筛选模式新开探测连接（pre/post 20 秒确认，不通回退并记失败摘要直到变更或 `retry` 增加）；转发改删后经 netlink 清旧 DNAT conntrack（待删最多留 512 条）。错误码：`firewall_tool_missing`、`firewall_conntrack_failed`、`firewall_forwarding_failed`、`firewall_probe_unavailable`、`firewall_unconfirmed`、`firewall_apply_failed`、`firewall_state_failed`；内核依赖 `nf_tables`、`nf_conntrack`、`nft_chain_nat`、`nft_nat`、`nft_masq`、`nft_ct`、`nft_socket`、`nft_fib_*`。重启装回、关闭与卸载清理见 [ARCHITECTURE.md](ARCHITECTURE.md#4-包职责)。

## 诊断

默认关闭（Cloud ADR 0102），不走 Operation；脚本只读且无重放，丢了重问。`diagnostics.run` 只含 `request_id`（UUID）与 `script`（非空 ≤65536 字节无 NUL bash）；格式错或未开模块均断连。

Agent 用 `systemd-run` 跑 `akastr-diagnostics-<request_id>.service`：`DynamicUser=yes` + `CAP_DAC_READ_SEARCH`、`NoNewPrivileges`、`ProtectSystem=strict`、`ProtectHome=read-only`、屏蔽 `/tmp`/`/var/tmp` 用 `/dev/shm` `TMPDIR`（`UMask=0077`）、`PrivateDevices`、屏蔽 journal 写入 socket、`IPAddressDeny=localhost`（放行 `127.0.0.53`）+ 执行前 1 秒连本机临时端口预检防失效、上限 256M RAM / 50% CPU / 64 任务 / 5 分钟超时（`RuntimeMaxSec` +30 秒）、空 stdin、单并发。

`diagnostics.result` 只含：`request_id`、`code`（`completed|timed_out|busy|sandbox_unavailable|start_failed`）、`exit_code`（仅 `completed` 含整数或信号 `null`）、`stdout`/`stderr`（UTF-8，非法字节换 `U+FFFD`，各上限 256 KiB 且总包超 1 MiB 时裁剪较长者）、`stdout_truncated`/`stderr_truncated`、`duration_ms`。输出不写盘、不写日志（日志只记请求号、脚本、code、退出码、时长与字节数）。

## 上报与回执

Agent 主动报 `report`（`report_id` UUID、`kind`、`data`），Cloud 持久化后回 `report.ack`（`report_id`）。格式对但当前不可用的上报照常 ack 并记日志（不为此断连；临时库错才断连重试）。

| kind | data 字段 | 保留与重发 |
|---|---|---|
| `ip.address` | `family`、`address`、`observed_at`、`command_id` | 仅留未确认最新态；详见下一节 |
| `changeip.unchanged` | `command_id`、`address`、`observed_at` | 写核对文件直到 ack |
| `xui.snapshot` | `inbounds`、`traffic`、`splice`、`connections` | 每分+ready 全量覆盖旧未确认 |
| `connection_records.batch` | `records`（≤1000）、`dropped` | 10 秒/千条，内存上限 20000 条 |
| `routing.outbound_health` | `outbounds`（`tag`、`up`、`delay_ms`） | 每 5 分+启动，只留最新未确认 |
| `firewall.status` | `mode`、`ssh_ports`、`panel_port`、`listening`、`conntrack`、`hits`、`domains`、`foreign_tables` | 每分+ready，只留最新未确认 |
| `metrics.sample` | `boot_id`、`uptime_seconds`、`cpu`、`load`、`memory`、`swap`、`disk`、`connections`、`processes`、`interfaces`、`system` | 每分+ready，只留最新未确认 |

- **`xui.snapshot`**：`inbounds` 含 `id`、`remark`、`protocol`、`listen`、`port`、`enable`、`settings`、`stream_settings`（秘密按白名单提取：settings 仅 VLESS `encryption`、SS `method/password/network`、Hysteria `version`；tlsSettings 仅 `serverName/alpn/settings.{fingerprint,allowInsecure,echConfigList}`；realitySettings 仅 `serverNames/shortIds/settings.{publicKey,fingerprint,serverName,spiderX,mldsa65Verify}`；其余只含 `externalProxy`、`finalmask`、`tcpSettings.header.type` 等公开传输项，拒绝带出服务端私钥/密码/auth）。`traffic` 含 `email`、`up`、`down`、`reset_seq`（仅同序号才 adopt）。`splice` 为 `off`/`on`/`null`（按 Xray env `xray.buf.splice`/`XRAY_BUF_SPLICE` 判，关闭见 [INSTALLATION.md](INSTALLATION.md)）。`connections` 含 `error_code`（`xui_log_unavailable` / `xui_connections_unavailable`）与有连接的 `clients`（`email`、`connections`）。
- **`connection_records.batch`**：Cloud 按 `(节点, at, source)` 幂等识别（先结束再开始或重发均无副作用；落盘失败照常 ack 允许丢失，Cloud ADR 0098）。**开始记录**必含 `at`（本机时区解析转 UTC RFC3339 6 位小数）、`email`、`source`、`network`（`tcp`/`udp` 或空串）、`host`（≤255）、`port`、`outbound`（≤128）、`blocked`。**结束记录**对已绑定且关闭后保留 30 秒的 TCP 连接再补 `ended_at`、`up`、`down`（内核 `bytes_received`/`bytes_acked` 含开销）、`requests`（4 项同现或同缺；行无 `network` 的结束记录补 `tcp`）。启动前旧行、停机期间与停机前 30 秒内关闭的连接无开始/结束记录；不写盘，重启清空。
- **`routing.outbound_health`**：对模板内 `ak-o-` 出站依次调 `POST panel/xray/testOutbound`（独立面板会话）；通 `up=true`+`delay_ms`，面板报 `Request failed` 为 `false`+`null`，测不了（忙/未起/错）双 `null`。空 `ak-o-` 发空数组；读不到模板不发；面板错误不外泄。
- **`firewall.status`**：`mode`（实际加载）；`ssh_ports`（非回环/链路本地监听 TCP ≤16）；`panel_port`；`listening`（非回环/链路本地 TCP+未连 UDP ≤256，不含 DHCP 68/546）；`conntrack`（`count`/`max` 或 `null`）；`hits`（`final`、`ceiling`、`truncated`）；`domains`（`name`、`addresses`、`resolved_at`、`failed`）；`foreign_tables`（`族 名` ≤32 只读名）。
- **`metrics.sample`**：字节均为整数。`boot_id`；`uptime_seconds`（`/proc/uptime` 取整）；`cpu`（两轮 `/proc/stat` 忙碌占比 0–100 保留 1 位小数，首轮 `null`）；`load`（`/proc/loadavg` 前 3 项）；`memory`/`swap`（`MemTotal-MemAvailable`、`SwapTotal-SwapFree`）；`disk`（`/proc/self/mounts` `/dev/` 非 loop 按设备号去重跳过沙盒遮挡目录，全无为 0）；`connections`（`sockstat`+`sockstat6` inuse 不含 TIME-WAIT）；`processes`；`interfaces`（有 `/device` 硬件网卡统计按名排序 ≤16 块；容器无硬件设备退化为默认路由网卡）；`system`（`os`、`kernel`、`arch`、`cpu_model`、`cpu_cores`、`virtualization`——`systemd-detect-virt` 输出 `none` 时照报）。除标明可 `null` 项外任一读取失败本轮不发且记一次日志。

## IP 观察、ChangeIP 与 IPv4 核对

节点是自身公网地址权威，Cloud 存历史。`ip.address` 必须匹配 `family` 且为公网地址（IPv6 规范化文本并按 IANA special-purpose 拒非 globally reachable，Cloud 亦要求开启 IPv6 观察）；`observed_at`/`checked_at` 仅同节点排序展示，超前或倒退由主控校正。每次重启或 WSS ready 先报当前地址，此后仅变化时上报（本地不留历史）。**当前会话 IPv4 确认前不接 ChangeIP**；IPv6 独立不触发 `changeip.unchanged`、不重置缓存、不挡业务。

核对期间 Agent 每 10 秒测一次，触发 2 分钟后连续 2 次仍为原 IP 发 `changeip.unchanged`（网络报错不计次）。带 `command_id` 且起始相符的变化收敛对应换 IP（可先于 `operation.result` 到）；核对状态持久保存在节点直到结果被 ack。无 `command_id` 的变化（及所有 IPv6 变化）均按自然 IP 变化处理。

## 安全边界

- 协议固定 `2026-11-01.v12`，不自动降级、不收协议外字段。
- 机器 token 只用于 bootstrap/enrollment 且不进 URL/WSS/更新/日志；日常认证全用本地 Ed25519 私钥。
- 安装命令不含 SOCKS5 密码或 ChangeIP bearer；普通日志与上报不含密码、原始链接、Xray 秘钥或脚本输出；受管客户端凭据只在下发 `state.put` 中存在，不落盘不记日志。
- 公网 IP 字段拒绝 private、loopback、link-local、CGNAT、文档/基准、组播与保留段。
- 除默认关闭且按 [诊断](#诊断) 沙盒运行的 `diagnostics` 外，不实现任意命令、远程 shell 或 HTTP 控制端。
- 认证、字段、持久 payload 或升级边界变更须按 Cloud ADR 0024 审批；跨仓库验证范围见 Cloud `docs/AGENT_INTEGRATION.md`。
