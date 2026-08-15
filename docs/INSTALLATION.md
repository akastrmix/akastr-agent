# Akastr Agent 安装与使用教程

本文面向手动迁移节点的操作者。v0.4.0 的新装参数全部在 AkastrCloud 后台填写；后台只返回一行命令，VPS 执行后不会再询问节点名称、模式、WSS、ChangeIP、SOCKS5 或 token。后台中的节点是持久对象：VPS 重装后可以再次取得原安装命令，不必删除后重建。

## 1. 支持范围

新装只支持：

- Debian 12 或 Debian 13；
- `x86_64` / `amd64`；
- systemd；
- root 或可用的 `sudo`；
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
sudo apt-get update
sudo apt-get install --yes ca-certificates curl wget
```

## 2. 在后台填写全部参数

进入 AkastrCloud 后台的“Agent 管理”，选择一种安装类型。

### 目标节点

必须填写节点名称并绑定对应服务器，然后设置：

- 公网 IPv4 检查间隔：10–3600 秒；一般保持 60 秒；
- ChangeIP：不启用、粘贴服务商完整 `curl` 命令，或固定本机程序；
- SOCKS5 入口：不公布，或公布地址来源与端口。

服务商接口方式直接粘贴完整命令，例如 `curl -X POST -H "Authorization: Bearer …" https://example.com/changeIP/`。后台只接受 HTTPS、POST、一个 Bearer header 和一个 URL，再解析成结构化配置；它不会执行这段文本，也不会把 token 拆成另一个输入框。该 secret 不进入安装命令，最终只存在于 root-only 的 `/etc/akastr-agent/changeip-curl.conf`。

“固定本机程序”填写可执行文件的绝对路径，例如 `/usr/local/bin/changeip`。没有参数就保持参数框为空；有参数时每行填写一个。它不接受 shell 命令串、`/bin/sh`、`/bin/bash`、`/usr/bin/env` 或 `eval`。主控以后只能触发这组固定 argv，不能远程换程序或参数。

公布 SOCKS5 只描述已有代理，Agent 不安装代理服务，也不保存该代理的用户名和密码：

- 地址随公网 IPv4 变化时选“跟随观测到的公网 IPv4”；
- 使用 DDNS 或固定入口时选“固定主机名或 IP”；
- 端口必须为 1–65535。

### IPQuality Runner

Runner 不绑定单一服务器。勾选需要检测的目标服务器，逐项填写 SOCKS5 用户名和密码。后台以稳定 server key 生成 1–128 个本地 profile；密码不会进入安装命令、列表、capability、Agent 日志或 command payload。

Runner 固定使用官方 [xykt/IPQuality](https://github.com/xykt/IPQuality) commit `0ee5f192fed70c04615852efba0e4b8bd43546c7`，并发严格为 1。多个检测由 AkastrCloud 持久排队，不能同时运行。

“每个服务节点每天一次真实 IPQuality”仍由主控执行：香港时间同一天的后续请求读取缓存；到 `00:00` 或目标 IPv4 变化后开启新代际。重装 Runner 或新增 profile 不能绕过此限制。

## 3. 添加节点并执行一键命令

点击“添加节点”后，节点会立刻出现在下方列表中，状态为“待安装”，同时显示一键命令。复制完整命令到目标 VPS 执行。命令形态如下，实际 UUID、机器 token 和版本由后台填写：

```bash
( installer=$(mktemp /tmp/akastr-agent-install.XXXXXX.sh) && trap 'rm -f -- "$installer"' 0 && wget --no-hsts --https-only --tries=3 --timeout=30 -qO "$installer" 'https://github.com/akastrmix/akastr-agent/releases/download/v0.4.0/install.sh' && sudo env AKASTR_AGENT_ID='<uuid>' AKASTR_AGENT_MACHINE_TOKEN='<machine-token>' AKASTR_AGENT_BOOTSTRAP_ENDPOINT='https://origin.akastrmix.com/internal/agents/bootstrap' sh "$installer" --install )
```

不要改写、拆分或公开这行命令，也不要把 wget/curl 的网络输出直接通过管道交给 shell。命令以 `mktemp` 创建唯一入口文件，并在子 shell 退出时自动删除；wget 使用 `--no-hsts`，不会创建或更新用户级 HSTS 数据库。机器 token 是该节点的长期安装凭据，可能进入本机 shell history；它不会用于 WSS 日常认证，但可重新下载密封配置并为重装后的主机注册新公钥。命令不包含 ChangeIP Bearer、SOCKS5 密码或其他 provider secret。

以后需要重装时，在列表中点击“安装命令”即可重新显示同一条命令。如果怀疑命令泄露，点击“轮换密钥”：后台会重新加密原配置并生成新命令，旧命令立即失效。删除节点会断开现有连接并永久删除后台身份和安装凭据；只有确定不再使用时才删除。

安装过程完全非交互。它会：

1. 检查 root、Debian 12/13、amd64 和 systemd；
2. 安装所需 Debian 包；
3. 下载同版本 amd64 binary，并自动完成内部完整性校验；
4. 使用节点 UUID 与机器 token 通过 HTTPS 取得持久密封配置；
5. 在本机以 AES-256-GCM 验证并解密，生成 root-only 配置与 secret 文件；
6. Runner 下载并自动校验固定 commit 的 IPQuality 脚本；
7. 运行 Agent 自身的 `check-config`；
8. 完成注册并删除安装过程中的本机机器 token 副本；主控继续保留加密配置，供日后重装；
9. 写入加固的 systemd unit，启动并连续检查服务稳定性。

成功时最后显示：

```text
Akastr Agent v0.4.0 已安装并运行。
```

注册以前失败会删除本次创建的 Agent 配置、状态、release 和 systemd unit；已经由 apt 安装的通用依赖可能保留。注册成功后若 service 启动失败，安装器会保留 identity 与现场。两种情况都不会停止、删除或修改旧 IPChanger。

## 4. 文件与权限

主要路径：

```text
/etc/akastr-agent/config.json
/etc/akastr-agent/identity.json
/var/lib/akastr-agent/
/usr/local/lib/akastr-agent/releases/v0.4.0/
/usr/local/lib/akastr-agent/current
/etc/systemd/system/akastr-agent.service
```

HTTP ChangeIP 另有 `/etc/akastr-agent/changeip-curl.conf`；Runner 另有 `/etc/akastr-agent/proxy-profiles.json` 与 `/usr/local/lib/akastr-agent/ipquality/ip.sh`。配置与 secret 文件权限为 `0600`，配置和状态目录为 root-only。systemd 默认只允许进程写 `/var/lib/akastr-agent`。

## 5. 安装后验收

在 VPS 执行：

```bash
sudo systemctl is-active akastr-agent.service
sudo systemctl show akastr-agent.service --property=MainPID,ActiveState,SubState,NRestarts
sudo /usr/local/lib/akastr-agent/current/akastr-agent version
sudo /usr/local/lib/akastr-agent/current/akastr-agent check-config --config /etc/akastr-agent/config.json
sudo /usr/local/lib/akastr-agent/current/akastr-agent capabilities --config /etc/akastr-agent/config.json
sudo journalctl -u akastr-agent.service -n 100 --no-pager
```

正确结果是：service 为 `active`、`MainPID` 非 0、版本为 `v0.4.0`、配置输出 `configuration valid`，日志出现 `control connection ready`。capability 只显示已启用能力，不应包含 token、密码或 provider secret。

再回到后台确认节点为“在线”，版本和类型正确。正式迁移前继续保留旧 IPChanger；仅仅安装成功不等于业务路由已经切换。

## 6. 日常使用

Agent 没有供操作者绕过主控的本地换 IP 或 IPQuality 命令。用户仍从 AkastrCloud/Carpool 发起延迟立即更换、预设更换、自动定时更换和 IPQuality 查询。

自然 IPv4 首次观察只建立 baseline，之后的变化通过 WSS 持久上报。AkastrCloud 重置该 IP 代际的 IPQuality 缓存，并私聊所有仍满足订阅条件的用户；没有 Telegram channel 播报。

常用只读或服务操作：

```bash
sudo systemctl status akastr-agent.service --no-pager
sudo journalctl -u akastr-agent.service -f
sudo systemctl restart akastr-agent.service
```

不要启动第二个 `akastr-agent run`，不要手工重复注册，也不要修改 `identity.json`、operation state 或 IP observation state。

## 7. 更新、状态与卸载

维护操作不进入菜单，也不询问问题。下载目标版本的 `install.sh` 后显式选择操作：

```bash
( installer=$(mktemp /tmp/akastr-agent-install.XXXXXX.sh) && trap 'rm -f -- "$installer"' 0 && wget --no-hsts --https-only --tries=3 --timeout=30 -qO "$installer" 'https://github.com/akastrmix/akastr-agent/releases/download/v0.4.0/install.sh' && sudo sh "$installer" --update )
( installer=$(mktemp /tmp/akastr-agent-install.XXXXXX.sh) && trap 'rm -f -- "$installer"' 0 && wget --no-hsts --https-only --tries=3 --timeout=30 -qO "$installer" 'https://github.com/akastrmix/akastr-agent/releases/download/v0.4.0/install.sh' && sudo sh "$installer" --status )
```

`--update` 只更新 binary，保留 identity、配置、secret 和状态。它先验证新 binary 能读取现有配置，再原子切换 `current` 并重启；新服务不能稳定运行时自动恢复旧 symlink。当前已是同版本时直接报告，不重复写入。

永久卸载必须使用显式销毁参数：

```bash
( installer=$(mktemp /tmp/akastr-agent-install.XXXXXX.sh) && trap 'rm -f -- "$installer"' 0 && wget --no-hsts --https-only --tries=3 --timeout=30 -qO "$installer" 'https://github.com/akastrmix/akastr-agent/releases/download/v0.4.0/install.sh' && sudo sh "$installer" --uninstall --confirm-destroy-local-agent )
```

卸载会停止服务并永久删除 `/etc/akastr-agent`、`/var/lib/akastr-agent`、`/usr/local/lib/akastr-agent`、private key 和本地执行证据；它不会自动删除后台节点。只有完成业务回滚并确认不再需要现场证据后才能执行；需要永久移除时，再在后台删除节点。

## 8. 常见故障

| 现象 | 处理 |
| --- | --- |
| 拒绝系统或架构 | 只支持 Debian 12/13 amd64；不要绕过检测或使用 ARM asset |
| 下载失败并显示 `wget 退出码` | `4` 通常表示网络/TLS，`8` 表示服务器返回错误；不要反复消耗 token，先核对版本化 Release 地址 |
| 缺少 `AKASTR_AGENT_*` | 命令被截断或手工改写；回后台重新生成，不要自行拼装 |
| bootstrap 返回 403 | 机器 token 已轮换、节点已删除或 UUID 不匹配；回后台重新取得当前命令 |
| bootstrap authentication failed | 密文、token 或 UUID 不匹配；停止操作，不要尝试绕过认证 |
| `check-config` 报 ChangeIP program | 程序不存在、不是 regular file 或不可执行；先修复固定 provider |
| `ipv4_unchanged` | provider 已完成，但 300 秒内公网 IPv4 没变 |
| `ipv4_observe_timed_out` | provider 后一直无法重新观察公网 IPv4，不代表旧地址仍可达 |
| IPQuality 脚本校验失败 | 停止安装；不要更改 checksum 或使用浮动在线脚本 |
| `proxy_profile_not_found` | Runner 未配置该目标 server key；删除后按完整 profile 重新添加 Runner 节点 |
| `proxy_preflight_failed` / `proxy_postflight_failed` | 检查目标 SOCKS5 host、端口、凭据和代理稳定性，不要打印密码 |
| `runner_busy` | Runner 单执行槽正忙，应由主控排队 |
| service 反复重启 | 查看 `systemctl show`、`journalctl` 并运行 `check-config`；不要删除 state 逃避错误 |

## 9. 正式迁移与回滚

每个节点逐台进行：

1. 保留旧 IPChanger，在后台填完整参数、添加持久节点并取得一键命令；
2. 安装并完成上面的 service、WSS、capability 与 baseline 验收；
3. 由主控只把该节点的新 command 路由切到 Agent，避免两套执行端同时换 IP；
4. 在批准窗口验证 ChangeIP、自然 IPv4 私聊、WSS 重连幂等，以及需要时的 IPQuality 排队与缓存；
5. 通过观察和回滚 Gate 后才停用旧实例。

回滚时先阻止新 command，保留 Agent identity/state/journal，再恢复 AkastrCloud 到旧 integration 并验证旧路径。不能仅停止 Agent 就宣称业务回滚，也不能在观察期直接卸载。

## 10. 维护者发布新版本

普通 `main` 提交和 Pull Request 会自动执行测试、静态检查、构建与安装脚本语法检查，但不会发布文件。确认某个 `main` 提交可以发布后，只需创建并推送一个新的语义化标签：

```bash
git tag -a vX.Y.Z -m "Akastr Agent vX.Y.Z"
git push origin vX.Y.Z
```

GitHub Actions 会从这个已存在的标签重新验证源码，只构建 `akastr-agent-linux-amd64` 和版本专用 `install.sh`，核对 binary 的 Linux amd64 架构、内嵌版本、资产集合和脚本语法，然后自动创建 GitHub Release。任一步失败都不会发布；已经存在的 Release 不允许由工作流覆盖或替换。

新版本会得到独立的 `releases/download/vX.Y.Z/...` 地址。AkastrCloud 应在批准的发布中显式固定准备迁移的 Agent 版本；不会悄悄把正式节点切换到 `latest`。Release 发布本身也不会创建节点、更新现有 Agent 或触发 ChangeIP/IPQuality。
