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

预期依次看到 `x86_64`、`systemd`、`debian 12` 或 `debian 13`。如果主机还没有 `curl`：

```bash
apt-get update
apt-get install --yes ca-certificates curl
```

## 2. 在后台填写全部参数

进入 AkastrCloud 后台的“Agent 管理”，选择一种安装类型。

### 目标节点

必须填写节点名称并绑定对应服务器，然后设置：

- 公网 IP 检查间隔：10–300 秒；一般保持 60 秒。Target 同时尝试观察 IPv6，无公网 IPv6 时忽略；
- ChangeIP：不启用、粘贴服务商完整 `curl` 命令，或固定本机程序；
- SOCKS5 入口：不公布，或公布端口。

服务商接口方式直接粘贴完整命令，例如 `curl -X POST -H "Authorization: Bearer …" https://example.com/changeIP/`。后台只接受 HTTPS、POST、一个 Bearer header 和一个 URL，再解析成结构化配置；它不会执行这段文本，也不会把 token 拆成另一个输入框。该 secret 不进入安装命令，最终只存在于节点上 root-only 的配置文件。

“固定本机程序”填写节点上已有程序或脚本的干净绝对路径，例如 `/usr/local/bin/changeip`。没有参数就保持参数框为空；有参数时每行填写一个。程序必须是 Agent service 可读取的非 symlink regular file 并具有执行权限；脚本需要有效 shebang。它不接受相对路径、控制字符、shell/env/busybox 入口，以及 sandbox 隐藏的 home、runtime user 或临时目录。主控以后只能触发这组固定 argv，不能远程换程序或参数。

公布 SOCKS5 只描述已有代理，Agent 不安装代理服务，也不保存该代理的用户名和密码。只需填写 1–65535 的监听端口；主控始终使用 Agent 最近一次观测到的公网 IPv4，不接受 DDNS、固定主机名或手填 IP。尚未建立公网 IPv4 baseline 时不会派发 IPQuality。

### IPQuality Runner

Runner 不绑定单一服务器。勾选需要检测的目标服务器，逐项填写 SOCKS5 用户名和密码。后台以稳定 server key 生成 1–128 个本地 profile；密码不会进入安装命令、列表、capability、Agent 日志或 command payload。

Runner 固定使用官方 [xykt/IPQuality](https://github.com/xykt/IPQuality)，具体 commit 与 SHA-256 见 [`pin.go`](../internal/providers/ipquality/script/pin.go)。并发严格为 1，多个检测由 AkastrCloud 持久排队。除作为安装前置的 `curl` 外，安装器只在缺少 Runner 命令时安装 `bash`、`jq`、`bc`、`netcat-openbsd`、`dnsutils` 和 `iproute2`，并在改动本地 Agent 前确认 `/bin/bash`、`jq`、`curl`、`bc`、`nc`、`dig` 与 `ip` 均可执行。

检测次数与缓存由 [Cloud Carpool 契约](https://github.com/akastrmix/AkastrCloud/blob/main/docs/CARPOOL.md#5-changeip-与-ipquality)管理；重装 Runner 或新增 profile 不能绕过限制。

## 3. 添加节点并执行一键命令

点击“添加节点”后，节点会立刻出现在下方列表中，状态为“待安装”，同时显示一键命令。复制完整命令到目标 VPS 执行。命令形态如下，实际安装码由后台填写：

```text
curl -fsSL https://origin.akastrmix.com/agent.sh | sh -s -- '安装码'
```

上面只展示命令结构；实际安装必须完整复制后台生成的命令，不要手工替换占位符。

不要改写、拆分或公开这行命令。`/agent.sh` 不接收安装码，只以不缓存的 302 跳转到 Cloud 当前批准版本的 GitHub Release installer；同一命令重跑会取得当时批准的版本，而非 GitHub latest。命令信任官方 HTTPS 入口及其固定版本 Release 跳转，发布流程验真 installer，installer 内部仍校验 Agent binary 的 SHA-256。

安装码只是 `节点UUID.机器token` 的组合，不是新增凭据或短码兑换服务；脚本拆开后通过环境变量交给 Agent 程序，使用 HTTPS bootstrap `https://origin.akastrmix.com/internal/agents/bootstrap`。机器 token 是长期安装凭据，可能进入 shell history 和安装脚本参数，但不会写入节点磁盘，也不会用于 WSS 日常认证。命令不包含 ChangeIP Bearer、SOCKS5 密码或其他 provider secret。

需要修改配置时点击“修改配置”。后台回填完整配置，包括 ChangeIP curl 原文与 Runner 凭据，密码可按需显示；在同一表单修改后保存。内容没有变化时不会更新版本或断开连接；真实修改会保留节点 ID、角色、服务器绑定、机器 token 与 identity，递增 configuration revision，并在新配置应用完成前暂停业务派发。保存时会核对你打开表单时的配置版本；若另一页面已修改，保留当前草稿并提示读取最新配置后重新确认。具体编辑契约见 [Cloud API](https://github.com/akastrmix/AkastrCloud/blob/main/docs/API.md#agent-控制通道)。在线 Agent 会立即取回新配置自动应用；离线 Agent 在恢复连接后自动同步，不需要重新执行安装命令。只有人工修复或重装才再次获取同一条一键命令。安装器拒绝覆盖另一个节点的机器，也拒绝接手没有 identity 的执行状态；同节点重装会生成新密钥，但保留执行日志与 IP 状态，残缺状态通过重跑同一命令 fix-forward 收敛。怀疑命令泄露时点击“轮换密钥”，原命令立即失效。

安装过程完全非交互，自动校验程序与脚本、取得密封配置并注册节点。它只管理唯一的 `akastr-agent.service`，完成后仍须按第 5 节验收业务连接。内部安装与更新流程见[架构说明](ARCHITECTURE.md#8-安装与自动更新)。

成功时最后显示：

```text
Akastr Agent <release-version> installed successfully.
```

下载、配置校验、依赖准备和注册都在停止现有 service 前完成；注册失败时旧 Agent 照常运行。注册成功后安装器才停止旧 service、切换到新部署并启动。中途失败不回滚，按报错修复后重跑同一命令。新节点、人工重装和残缺安装使用相同路径；普通配置变更使用第 7 节的自动更新。

## 4. 文件与权限

主要路径：

```text
/etc/akastr-agent/identity.json
/var/lib/akastr-agent/state.json
/var/lib/akastr-agent/ip-state.json
/usr/local/lib/akastr-agent/current -> slots/a 或 slots/b
/usr/local/lib/akastr-agent/slots/{a,b}/{akastr-agent,config.json}
/usr/local/lib/akastr-agent/ipquality/<sha256>.sh
/etc/systemd/system/akastr-agent.service
```

`current` 指向正在运行的 slot，另一个 slot 是上一版。`config.json` 就是 Cloud 下发的完整配置，含 ChangeIP Bearer 或 Runner 代理密码，权限为 `0600`；identity、配置和状态目录都是 root-only。固定程序由操作者在节点上管理，安装器不会写入或卸载。唯一主进程可以写 `/var/lib/akastr-agent` 与 `/usr/local/lib/akastr-agent`；固定程序所在的其他系统目录在 `ProtectSystem=strict` 下只读，home 目录不对 service 开放。

## 5. 安装后验收

在 VPS 执行：

```bash
systemctl is-active akastr-agent.service
systemctl show akastr-agent.service --property=MainPID,ActiveState,SubState,NRestarts
/usr/local/lib/akastr-agent/current/akastr-agent version
/usr/local/lib/akastr-agent/current/akastr-agent prepare --config /usr/local/lib/akastr-agent/current/config.json
journalctl -u akastr-agent.service -n 100 --no-pager
```

正确结果是：系统中只有 `akastr-agent.service`，其状态为 `active`、`MainPID` 非 0、版本与后台批准的 release 一致、`prepare` 输出 capability 列表，日志出现 `control connection ready`。service 的 `active` 表示进程运行，不能单独证明业务连接可用。SOCKS5 capability 只应包含端口；任何 capability 都不应包含 token、密码、主机名或 provider secret。

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

普通配置修改不需要重新运行安装命令。在 Cloud 的添加/编辑表单管理同一套完整参数，curl 原文、脚本路径/参数和 Runner 凭据会回填；Cloud 加密保存，管理员读取时解密，密码可显示。节点上的自定义程序由操作者准备。新增 Agent 功能由维护者发布新版本。两种变更都由主控保存为节点的目标版本与配置，再由 Agent 自动应用：

```mermaid
flowchart TD
    A{变更类型}
    A -->|修改节点配置| B[后台保存配置，主控断开该节点]
    A -->|新增 Agent 功能| C[发布新版，Cloud 后端重启]
    B --> D[节点重连时立即检查更新]
    C --> D
    D -->|节点忙碌| E[保持当前版本，稍后重试]
    E --> D
    D -->|发现变化| F[准备新版本并自行验证]
    F --> G[以新版本重启并连接主控]
    G -->|45 秒内被接受| H[切换为当前部署，恢复业务]
    G -->|失败| I[systemd 自动回到旧版本]
```

节点每 60 秒也会检查一次，离线节点恢复连接后自动同步；正在执行任务时等待空闲。同一版本/配置最多连续试两次；两次都失败时后台节点详情显示失败原因，节点保留旧版本，六小时后自动再试一次。修复后发布新版本或保存新配置会立即重新尝试，也可以在节点上重跑后台一键命令。协议与提交顺序见 [PROTOCOL.md](PROTOCOL.md#更新检查)。

```bash
journalctl -u akastr-agent.service -n 100 --no-pager
```

Agent 不提供 `--update` 或本地回退 CLI。自动更新不重写 systemd unit，也不安装 Debian 依赖包；需要改变这些内容的版本，由维护者在发布说明中写明已有节点的处理方式（通常是重跑后台一键命令）。节点上由操作者自行提供的 ChangeIP 程序仍由操作者维护。

日常状态直接从 systemd 读取，不需要再次下载安装器或使用机器 token：

```bash
systemctl --no-pager --full status akastr-agent.service
```

永久卸载必须使用版本化 installer 和显式销毁参数：

```bash
curl -fsSL 'https://github.com/akastrmix/akastr-agent/releases/download/<release-version>/install.sh' | sh -s -- --uninstall --confirm-destroy-local-agent
```

卸载会停止唯一 service，并永久删除该 unit、`/etc/akastr-agent`、`/var/lib/akastr-agent`、`/usr/local/lib/akastr-agent`、private key 和本地执行记录；中途失败直接重跑同一卸载命令。执行记录删除后，若主控仍有该节点未完成的任务，重装会被主控拒绝，直到管理员处理这些任务。操作者自行管理的固定 ChangeIP 程序不受影响。它不会自动删除后台节点。

## 8. 常见故障

| 现象 | 处理 |
| --- | --- |
| 拒绝系统或架构 | 只支持 Debian 12/13 amd64；不要绕过检测或使用 ARM asset |
| 下载失败并显示 `curl exit code` | 先核对网络/TLS 与版本化 Release 地址；不要改用浮动或 raw 地址 |
| 提示 `pass exactly one install code` 或 usage | 命令被截断或手工改写；回后台重新生成，不要自行拼装 |
| bootstrap 返回 403 | 机器 token 已轮换、节点已删除或 UUID 不匹配；回后台重新取得有效命令 |
| bootstrap authentication failed | 密文、token 或 UUID 不匹配；停止操作，不要尝试绕过认证 |
| `prepare` 或安装报 ChangeIP program | 程序不存在、不是 regular file 或不可执行；先修复固定 provider |
| 安装提示 `another Agent node` / `has no identity` | 这台机器装着别的节点，或留有来源不明的执行状态；确认后用卸载命令清理再安装 |
| `change_triggered` | HTTP provider 收到 `200`，或固定程序退出 `0`；只确认触发，主控继续等待公网 IP 事实 |
| `change_trigger_unknown` | 换 IP 可能让响应、进程或 WSS 提前断开；Agent 不重发 provider，恢复后由常驻 IPv4 monitor 收敛 |
| `http_status_not_200` / `exited_nonzero` | 服务商返回非 `200`，或固定程序非零退出；先修复 provider，Agent 不把它当成已触发 |
| IPQuality 脚本校验失败 | 停止安装；不要更改 checksum 或使用浮动在线脚本 |
| `proxy_profile_not_found` | Runner 未配置该目标 server key；删除后按完整 profile 重新添加 Runner 节点 |
| `proxy_preflight_failed` / `proxy_postflight_failed` | 检查目标 SOCKS5 host、端口、凭据和代理稳定性，不要打印密码 |
| `runner_busy` | Runner 单执行槽正忙，应由主控排队 |
| enrollment 返回 `agent_node_busy` / HTTP 409 | 主控仍有 pending、offered、accepted command 或 active ChangeIP session；等待其终结后重新运行同一安装命令 |
| enrollment 返回 `agent_release_required` | 安装命令引用的 binary 不是主控当前批准 release；回后台重新获取命令 |
| enrollment 返回 `agent_configuration_stale` | 配置在本次注册期间又被修改；重新运行同一条后台一键命令以取得最新 revision |
| service 启动超时 | WSS auth 或 hello 未完成；查看唯一主 service 日志，修复主控、网络或 identity 问题后重跑同一安装命令 |
| service 反复重启 | 查看 `systemctl show`、`journalctl` 并运行 `prepare`；不要删除 state 逃避错误 |
| 后台显示 `update_download_failed` | 节点无法从 GitHub 下载批准版本；检查节点网络，恢复后自动重试 |
| 后台显示 `update_candidate_invalid` | 新版本不接受当前配置或本机缺少依赖；查看日志中 `prepare` 的报错并修复配置或依赖 |
| 后台显示 `candidate_not_ready` / `candidate_runtime_invalid` | 新版本启动后未能在 45 秒内被主控接受，或本机依赖不满足；节点已自动回到旧版本，修复后发布新版本、保存新配置或重跑安装命令 |
| 后台显示 `update_state_invalid` | 本地更新记录或文件写入异常；检查磁盘与权限后重跑后台一键命令 |

## 9. 维护者发布版本

正式发布的命令、前置条件、CI 验真与重跑流程统一见 [Cloud 更新指南](https://github.com/akastrmix/AkastrCloud/blob/main/docs/UPDATE_GUIDE.md#5-发布范围与操作者配置)。节点在同协议或破坏性业务协议发布后都通过更新检查自动更新，不要求逐节点重装。若更新检查、下载目标或候选程序 CLI 本身不兼容，须另行批准迁移方案。不要绕过同步发布器手工打标签或修改 Cloud pin。

每个版本使用独立的 `releases/download/vX.Y.Z/...` 地址。只有同步流程中的 Cloud backend 激活成功，节点的更新检查才会收到 `update_available`；系统不跟随 GitHub `latest`。发布动作不会创建节点或触发 ChangeIP/IPQuality。
