# Akastr Agent 安装、配置与使用教程

本文面向第一次安装或手动迁移节点的操作者。所有示例都使用 `example.com`、示例 UUID 和占位凭据；复制配置后必须替换标注值。

> 当前不能开始正式迁移：AkastrCloud 生产环境的 Agent WSS Gate 仍然关闭，正式 installation 和 command 数量均为零。请先完成下载、依赖与配置准备；只有主控操作者明确启用 Gate，并通过安全渠道提供 installation UUID 和一次性 enrollment token 后，才能执行 enrollment 或安装脚本。不要提前停用旧 IPChanger。

## 1. 支持范围和依赖

v0.1.0 release 提供静态 Linux 二进制：

| 系统 | 架构 | release asset |
| --- | --- | --- |
| Debian 12/13（systemd） | x86_64 / amd64 | `akastr-agent-linux-amd64` |
| Debian 12/13（systemd） | aarch64 / arm64 | `akastr-agent-linux-arm64` |

完整安装、enrollment、WSS 重连、自然 IPv4 确认、systemd sandbox、正常更新和失败更新回滚已在 Debian 12 amd64 验证；Debian 13 使用相同路径。其他 Linux 发行版不是 v0.1.0 的已验证目标。容器、OpenRC 和 Windows service 不在支持范围。release 安装不需要 Git 或 Go。

基础安装需要 root、systemd、有效 CA、`curl`、`sha256sum` 以及 Debian 基础工具。下文按 root shell 编写：普通 sudo 管理员可先执行 `sudo -i`；如果已直接以 root 登录且系统没有 `sudo`，去掉示例中的 `sudo` 前缀即可。先确认 systemd 是 PID 1：

```bash
ps -p 1 -o comm=
sudo apt-get update
sudo apt-get install --yes ca-certificates curl coreutils systemd
```

ChangeIP 示例以 `/usr/bin/curl` 为固定 provider，因此目标节点也需要 `curl`。IPQuality Runner 还必须具有运行时硬检查的全部命令：`/bin/bash`、`jq`、`curl`、`bc`、`nc`、`dig` 和 `ip`。Debian 可安装：

```bash
sudo apt-get install --yes bash jq curl bc netcat-openbsd dnsutils iproute2
command -v /bin/bash jq curl bc nc dig ip
```

Agent 还需要出站 HTTPS/WSS、DNS 和准确系统时间。不得关闭 TLS 或主机名校验来绕过连接问题。

## 2. 下载并校验 v0.1.0

在目标机建立 root-only 暂存目录：

```bash
sudo install -d -m 0700 /root/akastr-agent-install
cd /root/akastr-agent-install
```

先确认架构：

```bash
uname -m
```

amd64 下载：

```bash
sudo curl --fail --silent --show-error --location \
  https://github.com/akastrmix/akastr-agent/releases/download/v0.1.0/akastr-agent-linux-amd64 \
  --output akastr-agent-linux-amd64
sudo curl --fail --silent --show-error --location \
  https://github.com/akastrmix/akastr-agent/releases/download/v0.1.0/akastr-agent-linux-amd64.sha256 \
  --output akastr-agent-linux-amd64.sha256
sudo sha256sum --check akastr-agent-linux-amd64.sha256
```

arm64 把两个文件名换为 `akastr-agent-linux-arm64`。v0.1.0 的已发布摘要为：

| asset | SHA-256 |
| --- | --- |
| `akastr-agent-linux-amd64` | `c81619997d4733dbf7afc0cdd956d230d9886e8ea12eaa24d7ed706d76a47a0f` |
| `akastr-agent-linux-arm64` | `582d91f4a762e90d1794b7f9f37bd952653271a9ae4a3e88b6a42927e469a2d5` |

只有 `sha256sum --check` 输出 `OK` 且摘要与可信发布记录一致时才继续。不要使用 `curl | sh`，也不要跳过 checksum。

可在安装前验证二进制版本。amd64 示例：

```bash
sudo chmod 0755 akastr-agent-linux-amd64
./akastr-agent-linux-amd64 version
```

输出必须为 `v0.1.0`。

## 3. 从 AkastrCloud 取得 enrollment 信息

v0.1.0 没有面向节点操作者的自助管理 UI，也没有在本仓库提供“生成 token”的命令。不要猜测 API 或手工生成 token。

主控端启用 Agent WSS Gate 后，由 AkastrCloud 操作者通过获批的管理流程创建 installation，并通过安全渠道提供三项信息：

1. installation UUID：写入 `node.id`，必须与 enrollment 响应一致；
2. WSS endpoint：必须是 `wss://<可信主控域名>/internal/agents/ws`；
3. 一次性 enrollment token：canonical、无 padding 的 32-byte base64url 字符串。

token 必须单独放进权限 `0600` 的 regular file，不能写入 Git、聊天记录或教程配置：

```bash
sudo install -m 0600 /dev/null /root/akastr-agent-install/enrollment-token
sudo nano /root/akastr-agent-install/enrollment-token
sudo chmod 0600 /root/akastr-agent-install/enrollment-token
```

一次 enrollment 成功后 token 立即失效。`enroll` 只把公钥发给主控，私钥保存在 `/etc/akastr-agent/identity.json`。

## 4. 配置目标节点

目标节点通常启用 `ip_watch`、`change_ip` 和 `socks5`，关闭 `ipquality_runner`。下面配置中的 UUID、endpoint、显示名和 provider 内容都必须替换：

```json
{
  "schema_version": 1,
  "node": {
    "id": "11111111-1111-4111-8111-111111111111",
    "name": "example-target"
  },
  "control": {
    "endpoint": "wss://control.example.com/internal/agents/ws",
    "credential_file": "/etc/akastr-agent/identity.json",
    "enrollment_token_file": "/etc/akastr-agent/enrollment-token"
  },
  "state_file": "/var/lib/akastr-agent/state.json",
  "ip_state_file": "/var/lib/akastr-agent/ip-state.json",
  "recent_operation_limit": 64,
  "capabilities": {
    "ip_watch": {
      "enabled": true,
      "interval_seconds": 60,
      "observe_ipv6": false
    },
    "change_ip": {
      "enabled": true,
      "program": "/usr/bin/curl",
      "args": [
        "--config",
        "/etc/akastr-agent/changeip.curl.conf"
      ],
      "timeout_seconds": 30,
      "observe_timeout_seconds": 300
    },
    "socks5": {
      "enabled": true,
      "address_source": "observed_ipv4",
      "advertised_host": "",
      "port": 1080
    },
    "ipquality_runner": {
      "enabled": false
    }
  }
}
```

先把它保存到暂存目录并限制权限：

```bash
sudo install -m 0600 /dev/null /root/akastr-agent-install/config.json
sudo nano /root/akastr-agent-install/config.json
sudo chmod 0600 /root/akastr-agent-install/config.json
```

### 固定 argv ChangeIP provider

Agent 不接受远端程序名、参数、shell 或 URL。上例始终执行两个固定 argv：

```text
/usr/bin/curl --config /etc/akastr-agent/changeip.curl.conf
```

把服务商 endpoint 和 bearer token 放在另一个 root-only curl 配置中，避免出现在 capability 或 Agent 主配置：

```bash
sudo install -d -m 0700 /etc/akastr-agent
sudo install -m 0600 /dev/null /etc/akastr-agent/changeip.curl.conf
sudo nano /etc/akastr-agent/changeip.curl.conf
```

文件示例：

```text
url = "https://panel.example.com/api/v1/changeIP/"
request = "POST"
fail
silent
show-error
header = "Authorization: Bearer REPLACE_WITH_PROVIDER_TOKEN"
```

随后确认：

```bash
sudo chmod 0600 /etc/akastr-agent/changeip.curl.conf
test -x /usr/bin/curl
sudo test "$(stat -c '%a' /etc/akastr-agent/changeip.curl.conf)" = 600
```

不要把真实 token 写进 JSON 示例、Git 或命令行。provider stdout/stderr 会被丢弃；服务商接口必须用退出码表示成功或失败。Agent 调用成功后还会最多等待 `observe_timeout_seconds`，只有观测到新公网 IPv4 才报告成功。

`socks5` 只描述已有代理服务，Agent 不负责安装或启动 SOCKS5。`address_source=observed_ipv4` 使用 Agent 观察到的公网 IPv4，此时 `advertised_host` 必须为空；固定域名或地址则用 `address_source=static` 并填写 `advertised_host`。用户名和密码不放在目标节点 capability 中。

## 5. 配置专用 IPQuality Runner

Runner 不需要提供目标节点的 SOCKS5 服务；它使用目标 SOCKS5 执行官方脚本。建议 Runner 只启用 `ipquality_runner`：

```json
{
  "schema_version": 1,
  "node": {
    "id": "22222222-2222-4222-8222-222222222222",
    "name": "example-ipquality-runner"
  },
  "control": {
    "endpoint": "wss://control.example.com/internal/agents/ws",
    "credential_file": "/etc/akastr-agent/identity.json",
    "enrollment_token_file": "/etc/akastr-agent/enrollment-token"
  },
  "state_file": "/var/lib/akastr-agent/state.json",
  "ip_state_file": "/var/lib/akastr-agent/ip-state.json",
  "recent_operation_limit": 64,
  "capabilities": {
    "ip_watch": {
      "enabled": false
    },
    "change_ip": {
      "enabled": false
    },
    "socks5": {
      "enabled": false
    },
    "ipquality_runner": {
      "enabled": true,
      "script_path": "/opt/akastr-agent/tools/ipquality/ip.sh",
      "proxy_profiles_file": "/etc/akastr-agent/proxies.json",
      "timeout_seconds": 600,
      "max_concurrency": 1,
      "script_version": "xykt-reviewed-commit",
      "script_sha256": "0000000000000000000000000000000000000000000000000000000000000000"
    }
  }
}
```

`script_version` 与 64 个零的 `script_sha256` 都是占位值，直接使用会在 checksum 检查处失败。它们必须换成操作者实际审核并固定的上游 commit 标识和真实小写 SHA-256。

### 固定官方脚本

官方项目是 [xykt/IPQuality](https://github.com/xykt/IPQuality)。Akastr Agent v0.1.0 **没有内置**上游 URL、commit、release version 或 SHA-256；这是刻意的操作者固定边界。不要使用上游 README 中的在线 `bash <(curl ...)` 形式，因为它每次可能取得不同内容，无法满足 Agent 的 checksum 固定要求。

先在官方仓库选择并审核一个不可变的完整 commit SHA，然后下载该 commit 的 `ip.sh`：

```bash
ipq_commit='REPLACE_WITH_REVIEWED_40_HEX_COMMIT'
sudo install -d -m 0755 /opt/akastr-agent/tools/ipquality
sudo curl --fail --silent --show-error --location \
  "https://raw.githubusercontent.com/xykt/IPQuality/${ipq_commit}/ip.sh" \
  --output /opt/akastr-agent/tools/ipquality/ip.sh
sudo chmod 0500 /opt/akastr-agent/tools/ipquality/ip.sh
sudo sha256sum /opt/akastr-agent/tools/ipquality/ip.sh
```

把输出的 64 位小写摘要写入 `script_sha256`，并为同一 commit 选择稳定小写 token，例如 `xykt-a1b2c3d4e5f6` 写入 `script_version`。采用前应通过受信渠道复核 commit 和摘要；仅对同一次网络下载自行计算摘要只能固定内容，不能单独证明来源可信。

运行时会固定调用：

```text
/bin/bash <script_path> -4 -n -x <Agent 创建的本地 SOCKS5 relay URL>
```

Agent 不允许 command payload 改变脚本路径、参数或 URL。脚本必须能够生成 `https://report.check.place/...` 报告 URL；隐私模式 `-p` 与 v0.1.0 的完成判定不兼容。

### root-only SOCKS5 profiles

每个 Runner 都要保存它可能执行的目标凭据。profile ID 是主控 payload 引用的稳定标识；推荐直接使用对应目标节点的 UUID，且不能含秘密：

```json
{
  "schema_version": 1,
  "profiles": {
    "11111111-1111-4111-8111-111111111111": {
      "username": "REPLACE_WITH_SOCKS5_USERNAME",
      "password": "REPLACE_WITH_SOCKS5_PASSWORD"
    }
  }
}
```

安装到配置中声明的路径：

```bash
sudo install -d -m 0700 /etc/akastr-agent
sudo install -m 0600 /dev/null /etc/akastr-agent/proxies.json
sudo nano /etc/akastr-agent/proxies.json
sudo chmod 0600 /etc/akastr-agent/proxies.json
sudo test "$(stat -c '%a' /etc/akastr-agent/proxies.json)" = 600
```

文件只接受 `schema_version` 和 `profiles`；profile 数量为 1–128，ID 必须匹配 `[a-z0-9][a-z0-9._-]{0,63}`，username/password 各为 1–255 字符。UUID 符合该格式，但主控派发的 `proxy_profile_id` 必须与这里的 key 完全相同。该文件不得授予 group 或 other 任何权限。

## 6. 配置字段速查

配置解析会拒绝未知字段、多段 JSON 和尾随内容。所有路径必须是规范化的绝对 Linux 路径，且至少启用一种 capability。

| 字段 | v0.1.0 约束 |
| --- | --- |
| `schema_version` | 必须为 `1` |
| `node.id` | 主控签发的非零、规范小写 UUID；必须与 enrollment 身份一致 |
| `node.name` | 1–64 字符，首尾不能有空白 |
| `control.endpoint` | 绝对 `wss` URL，path 必须为 `/internal/agents/ws`，不能含 user info、query 或 fragment |
| `control.credential_file` | enrollment 生成的 root-only Ed25519 identity 文件 |
| `control.enrollment_token_file` | 一次性 token 的预期安装路径 |
| `state_file` | operation active/recent 状态；未知或损坏 schema 会失败关闭 |
| `ip_state_file` | IPv4 baseline 与待确认变化；即使未启用 watcher 也要提供合法路径 |
| `recent_operation_limit` | `16`–`1024` |
| `ip_watch.interval_seconds` | 启用时 `10`–`3600` 秒 |
| `ip_watch.observe_ipv6` | v0.1.0 应设 `false`；当前自然变化 monitor 只上报 IPv4 |
| `change_ip.program` | 启用时必须是存在、regular、可执行的绝对路径 |
| `change_ip.args` | 固定 argv，最多 32 项；每项非空且不能含 NUL |
| `change_ip.timeout_seconds` | provider 执行超时，`1`–`300` 秒 |
| `change_ip.observe_timeout_seconds` | provider 完成后等待新 IPv4，`30`–`900` 秒；建议默认 `300` |
| `socks5.address_source` | `observed_ipv4` 或 `static` |
| `socks5.advertised_host` | `observed_ipv4` 时必须空；`static` 时必填 |
| `socks5.port` | `1`–`65535` |
| `ipquality_runner.script_path` | 固定、已审核官方脚本的绝对路径 |
| `ipquality_runner.proxy_profiles_file` | root-only profiles 的绝对路径 |
| `ipquality_runner.timeout_seconds` | `60`–`1800` 秒 |
| `ipquality_runner.max_concurrency` | v0.1.0 必须为 `1` |
| `ipquality_runner.script_version` | 1–64 字符稳定小写 token：`[a-z0-9][a-z0-9._-]{0,63}` |
| `ipquality_runner.script_sha256` | 恰好 64 位小写十六进制 SHA-256 |

## 7. 安装前检查 CLI

把已校验的二进制路径代入下列命令。`capabilities` 只输出不含秘密的描述；`check-config` 还会检查运行时文件、状态、ChangeIP program、IPQuality checksum、profile 权限和 Runner 依赖。

amd64 示例：

```bash
cd /root/akastr-agent-install
sudo ./akastr-agent-linux-amd64 version
sudo ./akastr-agent-linux-amd64 capabilities --config ./config.json
sudo ./akastr-agent-linux-amd64 check-config --config ./config.json
```

成功时最后一条输出 `configuration valid`。必须先通过该检查，再使用一次性 token enrollment。

## 8. 一键安装

`install.sh` 只接受完全未安装的新路径；以下任一对象已存在都会拒绝运行：

```text
/usr/local/lib/akastr-agent/current
/etc/systemd/system/akastr-agent.service
/etc/akastr-agent
/var/lib/akastr-agent
/usr/local/lib/akastr-agent
```

如果目标节点需要 ChangeIP curl 配置，或 Runner 需要脚本/profiles，应先按前文准备 `/etc/akastr-agent`。但这会触发 fresh-host 检查，所以这种节点应使用第 9 节的等价手动安装。当前 `install.sh` 更适合不需要额外 `/etc/akastr-agent` 文件的最小实例；这是 v0.1.0 的真实限制。

对于满足 fresh-host 条件的实例，从不可变 v0.1.0 tag 下载脚本并校验脚本本身：

```bash
cd /root/akastr-agent-install
sudo curl --fail --silent --show-error --location \
  https://raw.githubusercontent.com/akastrmix/akastr-agent/v0.1.0/scripts/install.sh \
  --output install.sh
echo '791c4b1578c69a255b06f8be8adfc6c5981d8a076fe8e91330ccd15d812d1a1c  install.sh' \
  | sudo sha256sum --check -
sudo chmod 0700 install.sh
```

确认 Gate 已开启、配置已通过检查且 token 已就绪后执行：

```bash
sudo ./install.sh \
  v0.1.0 \
  /root/akastr-agent-install/config.json \
  /root/akastr-agent-install/enrollment-token
```

完整接口为 `install.sh VERSION CONFIG_FILE ENROLLMENT_TOKEN_FILE [RELEASE_BASE_URL]`。可选 mirror 必须由操作者信任，并保持 `<base>/<version>/<asset>` 和 `<base>/<version>/<asset>.sha256` 布局；即使使用 mirror，checksum 也不会跳过。

脚本会再次下载并验证对应架构的 binary/checksum，创建 `0700` 配置与状态目录，将 config/token 安装为 `0600`，运行 `check-config` 和 `enroll`，删除安装位置的 token，创建加固 systemd unit，并在服务连续稳定约 5 秒后成功退出。

注意：脚本不会删除传入的 `/root/akastr-agent-install/enrollment-token` 源文件；成功后应将这个已经失效但仍属秘密的源文件安全移出工作区或删除。脚本也不是事务安装器：若 enrollment 或启动失败，它会保留已创建文件，之后不能直接重跑 fresh install；请保留证据、排障，并按手动步骤继续，不能用宽泛删除命令盲目清理。

## 9. 等价手动安装

目标节点和 Runner 通常需要提前准备 root-only provider 文件，因此推荐手动安装。下面以 amd64 为例；arm64 替换 asset 名称。

### 9.1 安装 immutable release 与配置

```bash
sudo install -d -m 0755 /usr/local/lib/akastr-agent/releases/v0.1.0
sudo install -d -m 0700 /etc/akastr-agent /var/lib/akastr-agent
sudo install -m 0755 \
  /root/akastr-agent-install/akastr-agent-linux-amd64 \
  /usr/local/lib/akastr-agent/releases/v0.1.0/akastr-agent
sudo install -m 0600 \
  /root/akastr-agent-install/config.json \
  /etc/akastr-agent/config.json
sudo install -m 0600 \
  /root/akastr-agent-install/enrollment-token \
  /etc/akastr-agent/enrollment-token
sudo ln -sfn \
  /usr/local/lib/akastr-agent/releases/v0.1.0 \
  /usr/local/lib/akastr-agent/current
```

安装后的 release 目录视为 immutable，不在原地覆盖 binary。

### 9.2 检查、查看能力并 enrollment

仅在 WSS Gate 已开启且 token 有效时执行 enrollment：

```bash
sudo /usr/local/lib/akastr-agent/current/akastr-agent version
sudo /usr/local/lib/akastr-agent/current/akastr-agent \
  check-config --config /etc/akastr-agent/config.json
sudo /usr/local/lib/akastr-agent/current/akastr-agent \
  capabilities --config /etc/akastr-agent/config.json
sudo /usr/local/lib/akastr-agent/current/akastr-agent \
  enroll --config /etc/akastr-agent/config.json
sudo rm -f -- /etc/akastr-agent/enrollment-token
```

成功输出 `enrolled agent <UUID>`。只有成功后才删除安装位置的 token。另行安全处理暂存目录中的 token 源文件。

### 9.3 创建 systemd unit

先创建 unit 文件：

```bash
sudo install -m 0644 /dev/null /etc/systemd/system/akastr-agent.service
sudo nano /etc/systemd/system/akastr-agent.service
```

填入下面的完整内容：

```ini
[Unit]
Description=Akastr Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/lib/akastr-agent/current/akastr-agent run --config /etc/akastr-agent/config.json
Restart=always
RestartSec=5s
TimeoutStopSec=30s
UMask=0077
NoNewPrivileges=true
PrivateTmp=true
PrivateDevices=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/akastr-agent
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
LockPersonality=true
MemoryDenyWriteExecute=true

[Install]
WantedBy=multi-user.target
```

然后启动并检查：

```bash
sudo chmod 0644 /etc/systemd/system/akastr-agent.service
sudo systemctl daemon-reload
sudo systemctl enable --now akastr-agent.service
sudo systemctl is-active akastr-agent.service
sudo systemctl show akastr-agent.service \
  --property=MainPID,ActiveState,SubState,NRestarts
```

`is-active` 必须输出 `active`，`MainPID` 不能为 `0`。systemd unit 以 root 运行以读取 root-only provider 文件，但 `ProtectSystem=strict` 令文件系统只读，只有 `/var/lib/akastr-agent` 可写。

## 10. 日常使用与日志

CLI 只有五个 command：

```text
akastr-agent version
akastr-agent check-config [--config PATH]
akastr-agent capabilities [--config PATH]
akastr-agent enroll [--config PATH]
akastr-agent run [--config PATH]
```

后四项默认 config 为 `/etc/akastr-agent/config.json`。`run` 是前台 daemon；systemd 已启动时不要再手工运行第二份。

Agent 没有本地 `change-ip` 或 `ipquality` CLI。用户仍从 AkastrCloud/Carpool 发起延迟立即更换（默认 5 分钟）、预设或自动更换；主控统一把它们转换为 `changeip.execute`。IPQuality 也只能由主控在每日缓存、目标互斥和 Runner 排队规则通过后派发，不能登录 Runner 手工绕过。

常用命令：

```bash
sudo systemctl status akastr-agent.service --no-pager
sudo journalctl -u akastr-agent.service -n 100 --no-pager
sudo journalctl -u akastr-agent.service --since '30 minutes ago' --no-pager
sudo systemctl restart akastr-agent.service
```

日志为 JSON，连接成功会出现 `control connection ready`。断线只记录稳定 code，例如 `connection_failed` 或 `websocket_<close-code>`；不会记录 WSS payload、SOCKS5 密码或 IPQuality 脚本完整输出。

### 自然 IPv4 上报

启用 `ip_watch` 后，首次观察只在 `ip_state_file` 建立 baseline，不通知用户。之后的自然 IPv4 变化先持久化，再通过 WSS 发送。主控确认持久化以前，Agent 会保留并在重连后重发。AkastrCloud 负责向所有仍满足订阅条件的用户私聊播报，并重置该节点当天 IPQuality 缓存；Agent 自己不连接 Telegram。

不要手工编辑 `state.json`、`ip-state.json` 或 `identity.json`。schema 不认识或文件损坏时，Agent 会失败关闭，防止重复执行或丢事件。

## 11. 更新和回滚

更新脚本把新版本安装到新的 immutable 目录，先用现有配置执行 `check-config`，再原子切换 `current` 并重启。若新服务无法稳定运行，它会自动把 symlink 恢复到旧 release 并再次验证服务。

下载当前已审核的 v0.1.0 updater：

```bash
cd /root/akastr-agent-install
sudo curl --fail --silent --show-error --location \
  https://raw.githubusercontent.com/akastrmix/akastr-agent/v0.1.0/scripts/update.sh \
  --output update.sh
echo '1d0ee5de3feb9586c6f08a1e80a957e35e1ca8b70c53fddbaddb1157ece3b7fa  update.sh' \
  | sudo sha256sum --check -
sudo chmod 0700 update.sh
```

目标 release 正式发布并经操作者批准后执行，其中 `vX.Y.Z` 必须换成真实版本：

```bash
sudo ./update.sh vX.Y.Z
```

完整接口为 `update.sh VERSION [RELEASE_BASE_URL]`，mirror 布局和 checksum 规则与安装器相同。不能更新到已存在的 release 目录，也不会修改 config。未来 release 若要求新版 updater，应从该 release 的不可变 tag 获取，并用可信发布记录中的摘要校验，不能盲目使用 main 分支脚本。

若程序保持 active 但业务验收失败，先停止新任务，再明确选择仍存在的旧 release；下面只是以 v0.1.0 为例：

```bash
sudo ln -sfn \
  /usr/local/lib/akastr-agent/releases/v0.1.0 \
  /usr/local/lib/akastr-agent/current
sudo systemctl restart akastr-agent.service
sudo systemctl is-active akastr-agent.service
sudo /usr/local/lib/akastr-agent/current/akastr-agent version
```

回滚 binary 不会自动回滚主控路由，也不能抹掉在途 operation。节点迁移回滚必须由 AkastrCloud 操作者协调，并保留 Agent identity、journal 和旧 IPChanger 证据。

## 12. 可恢复卸载

仓库没有 `uninstall.sh`。在主控先注销/隔离 installation、确认没有 active command，并确认旧节点路径可接管后，再执行可恢复卸载。以下命令把配置、状态和 release 移进 root-only 备份，而不是立即销毁：

```bash
sudo test ! -e /root/akastr-agent-uninstall-backup
sudo install -d -m 0700 /root/akastr-agent-uninstall-backup
sudo systemctl disable --now akastr-agent.service
sudo mv /etc/systemd/system/akastr-agent.service \
  /root/akastr-agent-uninstall-backup/akastr-agent.service
sudo mv /etc/akastr-agent \
  /root/akastr-agent-uninstall-backup/etc-akastr-agent
sudo mv /var/lib/akastr-agent \
  /root/akastr-agent-uninstall-backup/var-lib-akastr-agent
sudo mv /usr/local/lib/akastr-agent \
  /root/akastr-agent-uninstall-backup/usr-local-lib-akastr-agent
sudo systemctl daemon-reload
sudo systemctl reset-failed akastr-agent.service
```

备份含 private key、provider secret 和操作状态，必须持续保持 root-only。通过回滚观察期后才能按操作者的数据销毁流程处理；不要在教程中复制、输出或上传这些文件。

## 13. 常见故障

| 现象或 code | 含义与处理 |
| --- | --- |
| `control.endpoint must ...` | endpoint 必须是可信 `wss` 地址，path 精确为 `/internal/agents/ws`，且无 query/fragment |
| `enrollment token must be ...` | token 不是 root-only regular file，或不是 32-byte canonical unpadded base64url；不要自行生成，向主控重新申请 |
| enrollment 返回 HTTP 非 200 | Gate 未开启、token 无效/已使用或主控拒绝；保留日志并联系 AkastrCloud 操作者，不要关闭 TLS 校验 |
| `identity already exists` | 此实例已经 enrollment；不要覆盖 private key。核对主控 installation，再决定继续或走正式注销流程 |
| `configured node ID does not match enrolled identity` | `node.id` 与 `identity.json` 不同；恢复配套 config，不要编辑 identity |
| `configuration valid` 之前报 state/schema 错误 | 状态未知或损坏；保留原文件调查，不要删除来强行启动 |
| `IPQuality required command ... is unavailable` | 安装第 1 节列出的 Runner 依赖，并重新运行 `check-config` |
| `IPQuality script checksum mismatch` | 脚本内容与 `script_sha256` 不同；停止 Runner，核对固定 commit 和可信摘要，不能直接更新摘要迁就未知内容 |
| `proxy_profile_not_found` | 主控 payload 的 profile ID 不存在于 Runner 的 root-only profile 文件；修正映射并重启 |
| `proxy_preflight_failed` / `proxy_postflight_failed` | SOCKS5 地址、凭据、出站 TLS 或代理稳定性失败；不要在日志中打印密码 |
| `stale_expected_ipv4` / `proxy_ipv4_changed` | 目标 IPv4 代际已经变化；让主控重置缓存/任务，不要强行接受旧报告 |
| `runner_busy` | Runner 的唯一执行槽正被占用，主控应排队，不要启动第二个 Agent |
| `start_failed` / `exited_nonzero` / `timed_out` | ChangeIP 固定 provider 无法启动、非零退出或超时；在本机检查固定文件、权限和服务商接口 |
| `ipv4_unchanged` | provider 完成，但观察窗口内公网 IPv4 没有改变 |
| `ipv4_observe_timed_out` | provider 后公网 IPv4 一直不可观察，不等于“旧 IP 未变”；先恢复节点网络 |
| service 反复重启 | 用 `systemctl show` 和 `journalctl` 查看稳定 code，修复后重新运行 `check-config`；不要清空 state 逃避错误 |

## 14. 正式节点迁移与回滚边界

一次安全的逐节点迁移顺序是：

1. 主控批准并启用受限 WSS Gate，创建 installation 和一次性 token；
2. 保留旧 IPChanger，安装 Agent 并验证 enrollment、WSS ready、capability 和自然 IPv4 baseline；
3. 由 AkastrCloud 只把该节点的新 command 路由切到 Agent，避免两套执行端同时 ChangeIP；
4. 在批准的测试窗口验证 ChangeIP、自然 IPv4 私聊、重连幂等和必要的 IPQuality 排队/缓存；
5. 完成观察 Gate 后才停用旧实例；在全部六个节点迁移并超过回滚期前，不归档旧仓库和证据。

回滚时先阻止新 command，保留 Agent 的 identity/state/journal，恢复主控到旧 integration，再验证旧路径。不能仅停止一个 systemd service 就宣称业务回滚完成，也不能让旧 IPChanger 与 Agent 同时接受同一节点的 ChangeIP。

当前 Gate 关闭，因此以上步骤仍是操作边界，不是本教程授权执行的生产操作。
