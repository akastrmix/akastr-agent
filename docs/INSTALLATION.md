# Akastr Agent 安装与使用教程

本文面向节点操作者。所有参数均在 AkastrCloud 后台填写；后台返回一行版本化安装命令，VPS 执行时不会询问节点名称、模式、WSS、ChangeIP、SOCKS5 或 token。后台中的节点是持久对象：同一命令可用于空白 VPS、覆盖安装或修复，不必先卸载。

## 1. 支持范围

安装环境必须满足：

- Debian 12 或 Debian 13；
- `x86_64` / `amd64`；
- systemd；
- root 用户直接操作；安装和管理命令不依赖 `sudo`；
- 能访问 AkastrCloud HTTPS/WSS 与 GitHub release；Runner 还需访问 GitHub 上固定 commit 的官方 IPQuality 脚本。

不发布 ARM 版本，也不支持 Ubuntu、Alpine、OpenRC、容器或 Windows service。节点不需要 Git、Go、Node.js，也不用手写 JSON 或校验 SHA-256。

先检查主机：

```bash
uname -m
ps -p 1 -o comm=
. /etc/os-release
printf '%s %s\n' "$ID" "$VERSION_ID"
```

预期依次看到 `x86_64`、`systemd`、`debian 12` 或 `debian 13`。如果主机还没有 `curl` 或 `wget`：

```bash
apt-get update
apt-get install --yes ca-certificates curl wget
```

## 2. 在后台填写全部参数

进入 AkastrCloud 后台的“Agent 管理”，选择一种安装类型。

### 目标节点

必须填写节点名称并绑定对应服务器，然后设置：

- 公网 IPv4 检查间隔：10–300 秒；一般保持 60 秒；
- ChangeIP：不启用、粘贴服务商完整 `curl` 命令，或固定本机程序；
- SOCKS5 入口：不公布，或公布端口。

服务商接口方式直接粘贴完整命令，例如 `curl -X POST -H "Authorization: Bearer …" https://example.com/changeIP/`。后台只接受 HTTPS、POST、一个 Bearer header 和一个 URL，再解析成结构化配置；它不会执行这段文本，也不会把 token 拆成另一个输入框。该 secret 不进入安装命令，最终只存在于 root-only 的 `/etc/akastr-agent/changeip-curl.conf`。

“固定本机程序”填写可执行文件的绝对路径，例如 `/usr/local/bin/changeip`。没有参数就保持参数框为空；有参数时每行填写一个。它不接受 shell 命令串、`/bin/sh`、`/bin/bash`、`/usr/bin/env` 或 `eval`。主控以后只能触发这组固定 argv，不能远程换程序或参数。

公布 SOCKS5 只描述已有代理，Agent 不安装代理服务，也不保存该代理的用户名和密码。只需填写 1–65535 的监听端口；主控始终使用 Agent 最近一次观测到的公网 IPv4，不接受 DDNS、固定主机名或手填 IP。尚未建立公网 IPv4 baseline 时不会派发 IPQuality。

### IPQuality Runner

Runner 不绑定单一服务器。勾选需要检测的目标服务器，逐项填写 SOCKS5 用户名和密码。后台以稳定 server key 生成 1–128 个本地 profile；密码不会进入安装命令、列表、capability、Agent 日志或 command payload。

Runner 固定使用官方 [xykt/IPQuality](https://github.com/xykt/IPQuality) commit `0ee5f192fed70c04615852efba0e4b8bd43546c7`，并发严格为 1。安装器会自动安装 `bash`、`jq`、`curl`、`bc`、`netcat-openbsd`、`dnsutils` 和 `iproute2`，并在改动本地 Agent 前确认 `/bin/bash`、`jq`、`curl`、`bc`、`nc`、`dig` 与 `ip` 均可执行。多个检测由 AkastrCloud 持久排队，不能同时运行。

“每个服务节点每天一次真实 IPQuality”由主控执行：香港时间同一天的后续请求读取缓存；到 `00:00` 或目标 IPv4 变化后开启新代际。重装 Runner 或新增 profile 不能绕过此限制。

## 3. 添加节点并执行一键命令

点击“添加节点”后，节点会立刻出现在下方列表中，状态为“待安装”，同时显示一键命令。复制完整命令到目标 VPS 执行。命令形态如下，实际 UUID、机器 token 和版本由后台填写：

```bash
( installer=$(mktemp /tmp/akastr-agent-install.XXXXXX.sh) && trap 'rm -f -- "$installer"' 0 && wget --no-hsts --https-only --tries=3 --timeout=30 -qO "$installer" 'https://github.com/akastrmix/akastr-agent/releases/download/<release-version>/install.sh' && env AKASTR_AGENT_ID='<uuid>' AKASTR_AGENT_MACHINE_TOKEN='<machine-token>' AKASTR_AGENT_BOOTSTRAP_ENDPOINT='https://origin.akastrmix.com/internal/agents/bootstrap' sh "$installer" --install )
```

上面只展示命令结构；实际安装必须完整复制后台生成的命令，不要手工替换占位符。

不要改写、拆分或公开这行命令，也不要把 wget/curl 的网络输出直接通过管道交给 shell。命令以 `mktemp` 创建唯一入口文件，并在子 shell 退出时自动删除；wget 使用 `--no-hsts`，不会创建或更新用户级 HSTS 数据库。机器 token 是该节点的长期安装凭据，可能进入本机 shell history；它不会用于 WSS 日常认证，但可重新下载密封配置并为重装后的主机注册新公钥。命令不包含 ChangeIP Bearer、SOCKS5 密码或其他 provider secret。

需要修复、覆盖或重装时，在列表中点击“安装命令”即可重新显示该节点的命令。binary、bootstrap、依赖和本地空闲状态全部验证成功后，安装器才停止 Agent，并暂存 Agent 自己的配置、状态、release 与 unit。每次安装都会注册全新的 identity，不保留既有 private key 或 operation state。主控有未完成 command 时拒绝重装，避免切断正在执行或等待确认的操作。怀疑命令泄露时点击“轮换密钥”，原命令立即失效；删除节点只用于永久移除。

安装过程完全非交互。它会：

1. 检查 root、Debian 12/13、amd64 和 systemd；
2. 安装所需 Debian 包；
3. 下载同版本 amd64 binary，并自动完成内部完整性校验；
4. 使用节点 UUID 与机器 token 通过 HTTPS 取得持久密封配置；
5. 在本机以 AES-256-GCM 验证并解密，生成 root-only 配置与 secret 文件；
6. Runner 下载并自动校验固定 commit 的 IPQuality 脚本；
7. 严格停止全部 Agent unit，任何 unit 未进入 inactive 都会中止，不移动 Agent 文件；
8. 停止后读取稳定的 operation journal；有 active 操作就保持原 unit 并中止；
9. 运行 `check-config`，生成新 identity 并完成注册，再删除本机机器 token 副本；
10. 把 Agent systemd 命名空间收敛为唯一的 `akastr-agent.service`；服务只有完成 WSS 认证和 hello 后才向 systemd 报告 ready。

成功时最后显示：

```text
Akastr Agent <release-version> installed successfully.
```

公钥注册前的失败会保持事务开始前的 Agent 文件、unit 与启停状态；已经由 apt 安装的通用依赖可能保留。主控公钥注册成功后，安装器会保留新 identity 和安装文件；此后的故障通过重跑同一条命令修复。

## 4. 文件与权限

主要路径：

```text
/etc/akastr-agent/config.json
/etc/akastr-agent/identity.json
/var/lib/akastr-agent/
/usr/local/lib/akastr-agent/releases/<release-version>/
/usr/local/lib/akastr-agent/current
/etc/systemd/system/akastr-agent.service
```

HTTP ChangeIP 另有 `/etc/akastr-agent/changeip-curl.conf`；Runner 另有 `/etc/akastr-agent/proxy-profiles.json` 与 `/usr/local/lib/akastr-agent/ipquality/ip.sh`。配置与 secret 文件权限为 `0600`，配置和状态目录为 root-only。唯一主进程可以写 `/var/lib/akastr-agent` 与自己的 release root，其他系统目录仍由 `ProtectSystem=strict` 保护。

## 5. 安装后验收

在 VPS 执行：

```bash
systemctl is-active akastr-agent.service
systemctl show akastr-agent.service --property=MainPID,ActiveState,SubState,NRestarts
/usr/local/lib/akastr-agent/current/akastr-agent version
/usr/local/lib/akastr-agent/current/akastr-agent check-config --config /etc/akastr-agent/config.json
/usr/local/lib/akastr-agent/current/akastr-agent capabilities --config /etc/akastr-agent/config.json
journalctl -u akastr-agent.service -n 100 --no-pager
```

正确结果是：系统中只有 `akastr-agent.service`，其状态为 `active`、`MainPID` 非 0、版本与后台批准的 release 一致、配置输出 `configuration valid`，日志出现 `control connection ready`。因为 service 使用 `Type=notify`，`active` 代表 WSS auth 与 hello 已完成，不只是进程存活。SOCKS5 capability 只应包含端口；任何 capability 都不应包含 token、密码、主机名或 provider secret。

再回到后台确认节点为“在线”，版本和类型正确；只有在线且 capability 完整的节点才能接收业务操作。

## 6. 日常使用

Agent 没有供操作者绕过主控的本地换 IP 或 IPQuality 命令。用户从 AkastrCloud/Carpool 发起延迟立即更换、预设更换、自动定时更换和 IPQuality 查询。

自然 IPv4 首次观察通过 WSS `ip.snapshot` 持久建立 Cloud baseline，之后的变化再以 `ip.observed` 上报。AkastrCloud 重置变化后的 IPQuality 缓存代际，并私聊所有仍满足订阅条件的用户；没有 Telegram channel 播报。

常用只读或服务操作：

```bash
systemctl status akastr-agent.service --no-pager
journalctl -u akastr-agent.service -f
systemctl restart akastr-agent.service
```

不要启动第二个 `akastr-agent run`，不要手工重复注册，也不要修改 `identity.json`、operation state 或 IP observation state。

## 7. 更新、状态与卸载

正常情况下不需要手动更新：唯一的 Agent 主进程每六小时检查一次 AkastrCloud 已批准版本。主控有未完成命令时返回 `busy`；Agent 看到可用版本后，必须先取得与 command 共用的进程级更新 lease。执行中的 command 会阻止更新，更新持锁期间不会接受新 command。下载、版本、内部完整性或当前配置任一验证失败都不会切换。通过验证后 Agent 原子切换 `current`、删除旧 release，并以同一 PID 重执行新 binary。可在主 service 日志中查看稳定更新错误码：

```bash
journalctl -u akastr-agent.service -n 100 --no-pager
```

Agent 只维护 `current` release，不提供 `--update` 或本地回退 CLI。自动更新失败时当前进程继续运行；如果 `current` 已切换但原位执行失败，systemd 会从唯一的 current 重启。需要人工修复时，维护者让 AkastrCloud 批准修复版本，或由操作者从后台复制该节点的一键命令并重新运行 `--install`。安装器会重新下载完整配置、确认双端空闲并重新注册 identity。

安装器的 `--status` 只读取 systemd 状态，不需要机器 token。把下方 `<release-version>` 替换为节点正在使用的完整版本标签：

```bash
( installer=$(mktemp /tmp/akastr-agent-install.XXXXXX.sh) && trap 'rm -f -- "$installer"' 0 && wget --no-hsts --https-only --tries=3 --timeout=30 -qO "$installer" 'https://github.com/akastrmix/akastr-agent/releases/download/<release-version>/install.sh' && sh "$installer" --status )
```

永久卸载必须使用显式销毁参数，并使用节点正在运行的完整版本标签：

```bash
( installer=$(mktemp /tmp/akastr-agent-install.XXXXXX.sh) && trap 'rm -f -- "$installer"' 0 && wget --no-hsts --https-only --tries=3 --timeout=30 -qO "$installer" 'https://github.com/akastrmix/akastr-agent/releases/download/<release-version>/install.sh' && sh "$installer" --uninstall --confirm-destroy-local-agent )
```

卸载会停止服务并永久删除 `/etc/akastr-agent`、`/var/lib/akastr-agent`、`/usr/local/lib/akastr-agent`、private key 和本地执行证据；它不会自动删除后台节点。确认不再需要现场证据后才能执行；需要永久移除时，再在后台删除节点。

## 8. 常见故障

| 现象 | 处理 |
| --- | --- |
| 拒绝系统或架构 | 只支持 Debian 12/13 amd64；不要绕过检测或使用 ARM asset |
| 下载失败并显示 `wget exit code` | `4` 通常表示网络/TLS，`8` 表示服务器返回错误；不要反复消耗 token，先核对版本化 Release 地址 |
| 缺少 `AKASTR_AGENT_*` | 命令被截断或手工改写；回后台重新生成，不要自行拼装 |
| bootstrap 返回 403 | 机器 token 已轮换、节点已删除或 UUID 不匹配；回后台重新取得有效命令 |
| bootstrap authentication failed | 密文、token 或 UUID 不匹配；停止操作，不要尝试绕过认证 |
| `check-config` 报 ChangeIP program | 程序不存在、不是 regular file 或不可执行；先修复固定 provider |
| `change_triggered` | HTTP provider 收到 `200`，或固定程序退出 `0`；只确认触发，主控继续等待公网 IP 事实 |
| `change_trigger_unknown` | 换 IP 可能让响应、进程或 WSS 提前断开；Agent 不重发 provider，恢复后由常驻 IPv4 monitor 收敛 |
| `http_status_not_200` / `exited_nonzero` | 服务商返回非 `200`，或固定程序非零退出；先修复 provider，Agent 不把它当成已触发 |
| IPQuality 脚本校验失败 | 停止安装；不要更改 checksum 或使用浮动在线脚本 |
| `proxy_profile_not_found` | Runner 未配置该目标 server key；删除后按完整 profile 重新添加 Runner 节点 |
| `proxy_preflight_failed` / `proxy_postflight_failed` | 检查目标 SOCKS5 host、端口、凭据和代理稳定性，不要打印密码 |
| `runner_busy` | Runner 单执行槽正忙，应由主控排队 |
| enrollment 返回 `agent_node_busy` / HTTP 409 | 主控仍有 pending、offered 或 accepted command；等待其终结后重新运行同一安装命令 |
| service 启动超时 | WSS auth 或 hello 未完成；查看唯一主 service 日志，修复主控、网络或 identity 问题后重跑同一安装命令 |
| service 反复重启 | 查看 `systemctl show`、`journalctl` 并运行 `check-config`；不要删除 state 逃避错误 |
| 日志出现 `update_check_failed` | 主控或网络暂时不可用；运行版本继续工作，下一个六小时周期重试 |
| 日志出现 `update_apply_failed` | 下载、版本、配置或内部完整性验证失败；运行版本不受影响，不要改地址或跳过校验 |
| 日志出现 `update_reexec_failed` | current 已切换但进程替换失败；保留日志，等待 systemd 从 current 重启；仍失败则重跑后台一键安装命令 |

## 9. 维护者发布版本

普通 `main` 提交和 Pull Request 会自动执行测试、静态检查、构建与安装脚本语法检查，但不会发布文件。正式发布从 AkastrCloud 仓库执行唯一同步入口：

```powershell
.\release.cmd agent -AgentVersion vX.Y.Z -Execute
```

两个仓库都必须位于 `main`、已经提交且工作树干净。同步发布器验证双方协议与 Agent 源码，直接推送 Agent `main` 和语义化标签；GitHub Actions 从标签重新验证，只构建 `akastr-agent-linux-amd64` 和版本专用 `install.sh`。发布器验真两个不可变资产后，自动提交 Cloud 的精确更新目标并执行正常 backend 发布。流程不创建 PR；同一 tag 与 commit 可在中断后直接重跑，已存在的 Release 不允许覆盖或替换。

每个版本使用独立的 `releases/download/vX.Y.Z/...` 地址。只有同步流程中的 Cloud backend 激活成功，主进程的六小时循环才会收到 `update_available`；系统不跟随 GitHub `latest`。发布动作不会创建节点或触发 ChangeIP/IPQuality。
