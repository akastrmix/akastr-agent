# Akastr Agent 安装与使用教程

本文面向手动迁移节点的操作者。v0.2.0 不再要求用户下载 binary、复制 JSON、创建 token 文件或手工验证 checksum；唯一推荐入口是 AkastrCloud 后台为 installation 生成的一行交互式安装命令。

> 当前不能执行：AkastrCloud 生产环境的 Agent WSS Gate 仍然关闭，正式节点尚未迁移。请等待主控操作者明确开放 Gate 和迁移窗口。现在执行安装无法完成正常 enrollment，也不得提前停用旧 IPChanger。

## 1. 支持范围与准备

v0.2.0 只支持：

- Debian 12 或 Debian 13；
- `x86_64` / `amd64`；
- 以 systemd 为 init；
- 可以使用 root 权限和交互式 TTY；
- 可以访问 AkastrCloud WSS/HTTPS、GitHub release；Runner 还需访问官方 IPQuality 脚本地址。

不再发布 ARM binary。Ubuntu、Alpine、OpenRC、容器和 Windows service 不是受支持路径。节点不需要 Git 或 Go。

先确认系统：

```bash
uname -m
ps -p 1 -o comm=
. /etc/os-release
printf '%s %s\n' "$ID" "$VERSION_ID"
```

预期分别看到 `x86_64`、`systemd` 和 `debian 12` 或 `debian 13`。后台生成的命令需要先下载生成版 `install.sh`，因此主机至少要有可信 CA 和 `curl`；缺少时先安装：

```bash
sudo apt-get update
sudo apt-get install --yes ca-certificates curl
```

安装器必须以 root 运行。普通管理员应使用后台命令中提供的 `sudo`；已经直接登录 root 时按后台提示执行。安装器会自行安装其余依赖：

- 所有模式：`ca-certificates`、`curl`、`jq`；
- Runner 额外安装：`bash`、`bc`、`netcat-openbsd`、`dnsutils`、`iproute2`。

不要关闭 TLS/host 校验，不要使用 `curl | sh`，也不要把非交互管道当作终端。安装器会直接从 `/dev/tty` 读取问题和秘密。

## 2. 在 AkastrCloud 后台创建 installation

WSS Gate 开放后，管理员在 AkastrCloud 后台进入 Agent 管理，创建以下一种 installation：

- `target`：服务目标节点，负责 IPv4 观察、可选 ChangeIP 和 SOCKS5 描述；
- `runner`：专用 IPQuality Runner，通过目标节点的 SOCKS5 排队执行检测。

后台会生成：

1. 稳定 installation UUID；
2. 一次性 enrollment token；
3. 预填 `AKASTR_AGENT_ID`、`AKASTR_AGENT_MODE` 和 `AKASTR_CONTROL_ENDPOINT` 的一行安装命令。

一次性 token **不会**嵌入命令。这样可避免它进入 shell history、进程参数或后台复制记录；稍后由安装器在终端静默询问。

不要自行生成 UUID/token，不要猜测 enrollment API，也不要手工拼装命令。后台生成的命令绑定 installation 类型和主控地址，是该节点的权威入口。

## 3. 执行一行安装命令

在待迁移节点打开一个稳定的交互式 SSH 终端：

1. 从 AkastrCloud 后台复制完整命令；
2. 原样粘贴为一行，不改变 URL、version、installation UUID、mode 或 WSS endpoint；
3. 执行后确认标题显示 `Akastr Agent v0.2.0 交互式安装器`；
4. 按向导回答问题；
5. 在 `一次性 enrollment token` 提示出现时粘贴后台 token；输入不会回显；
6. 阅读不含秘密的安装摘要；
7. 只有内容正确时，对最终确认输入 `y`。

直接按 Enter 等同采用方括号中的默认值。所有是/否问题默认 `N`，需要启用时明确输入 `y`。

生成版安装器内嵌 v0.2.0 binary 的 SHA-256。它会自动下载 `akastr-agent-linux-amd64`、计算摘要并与内嵌值比较；不一致立即停止。用户不需要复制、保存或手工比对 SHA，也不应另找 binary 替换。

确认前，安装器只在权限 `0700` 的临时目录处理输入，并显示以下不含秘密的摘要：

- release version 和 `linux/amd64`；
- installation UUID、节点名称、类型和主控 endpoint；
- target 的检查间隔、ChangeIP/SOCKS5 开关；
- Runner 的 profile 数量和固定并发。

在最终确认处选择取消会显示 `已取消，未修改系统`。

## 4. 目标节点向导

后台命令已经把模式固定为 `target`。操作者仍需确认名称并配置该节点本地能力。

### 4.1 通用信息

- `节点名称`：默认取主机短名，可改为后台容易辨认的名称；长度 1–64 字符，首尾不能有空白。
- `主控 WSS 地址`：后台命令已经预填；应以 `wss://` 开头，path 为 `/internal/agents/ws`。不要改成 IP、测试域名或关闭 TLS。
- `一次性 enrollment token`：从后台复制，终端不回显。

### 4.2 公网 IPv4 观察

目标节点一定启用 IPv4 watch。向导询问：

```text
公网 IPv4 检查间隔（秒） [60]:
```

允许范围为 10–3600 秒，推荐保留 60 秒。首次成功观察只建立 baseline，不通知用户；之后的自然 IPv4 变化会持久化并上报 AkastrCloud。主控确认前，Agent 会在重连后重发。

v0.2.0 的向导固定 `observe_ipv6=false`，不会产生自然 IPv6 变化事件。

### 4.3 ChangeIP

`启用 ChangeIP？ [N]` 选择 `y` 后有两种 provider。

#### 方式一：HTTP POST + Bearer token（推荐）

依次输入：

- `ChangeIP HTTPS URL`：必须以 `https://` 开头，例如 `https://panel.example.com/api/v1/changeIP/`；
- `Bearer token`：静默输入，只接受字母、数字、点、下划线、波浪线和连字符。

安装器生成权限 `0600` 的本地 curl 配置，运行时固定执行：

```text
/usr/bin/curl --config /etc/akastr-agent/changeip-curl.conf
```

URL 和 bearer token 不进入 capability、operation journal 或 Agent 日志。

#### 方式二：本机可执行程序 + 固定参数

仅在节点已经有经过审核的专用 provider 时选择。依次输入：

- 可执行程序的规范绝对路径；
- 参数数量，范围 0–32；
- 每个固定参数，每项单独输入且不能为空。

安装器不会把一整行命令交给 shell 解析；Agent 使用固定 `program` + argv 执行。不要选择 `/bin/sh -c`、`/bin/bash -c`、通用任务执行器或能形成远程 shell 的程序。该可执行文件必须在安装前已经存在、是 regular file 且具有执行权限，否则 `check-config` 会失败。

两种方式都使用 60 秒 provider timeout；provider 完成后最多等待 300 秒观察新公网 IPv4。退出码为零不代表换 IP 成功，只有看到不同地址才返回 `ipv4_changed`。

### 4.4 SOCKS5 描述

`向主控公布该节点的 SOCKS5 入口？ [N]` 只描述已经存在的 SOCKS5 服务；Agent 不安装或启动代理，也不在目标 capability 中保存用户名/密码。

启用后输入端口 1–65535，再选择地址来源：

- 公网入口始终随节点 IPv4 变化：对“始终跟随观测到的公网 IPv4”输入 `y`；
- 使用 DDNS、固定域名或固定地址：保留默认 `N`，再输入静态 host，例如 `node.example.com`。

Runner 中对应 server key 的凭据必须能连接这里公布的 host/port。

## 5. IPQuality Runner 向导

后台命令已经把模式固定为 `runner`。Runner 不启用目标 IPv4 watch、ChangeIP 或 SOCKS5 描述。

### 5.1 配置 profile

先输入要配置的 SOCKS5 credential 数量，范围 1–128。每个 profile 依次输入：

- `server key`：AkastrCloud 中目标节点使用的稳定小写标识，例如 `example-hk`；只能包含小写字母、数字、点、下划线和连字符，最长 64 字符，不能重复；
- SOCKS5 username；
- SOCKS5 password：静默输入，不回显。

server key 必须与主控派发的 `proxy_profile_id` 完全一致。多个目标可在同一 Runner 中各有一个 profile，但凭据不会进入 capability、command payload 或日志。

安装器把 profiles 写入：

```text
/etc/akastr-agent/proxy-profiles.json
```

文件权限固定为 `0600`。用户不需要也不应手工编辑 JSON；新增、删除或轮换 profile 应在维护窗口通过获批的配置流程处理。

### 5.2 内置官方 IPQuality

v0.2.0 生成版安装器固定：

- 官方项目：[xykt/IPQuality](https://github.com/xykt/IPQuality)；
- commit：`0ee5f192fed70c04615852efba0e4b8bd43546c7`；
- runtime version token：`0ee5f192fed7`；
- timeout：900 秒；
- `max_concurrency=1`。

安装器从该不可变 commit 下载 `ip.sh`，并使用内嵌 SHA-256 自动验证。用户不需要选择 commit、复制 checksum 或手工运行脚本；校验失败会停止安装。

Agent 固定通过目标 SOCKS5 执行：

```text
/bin/bash <pinned-ip.sh> -4 -n -x <local-socks5-relay>
```

本地 relay 只监听 `127.0.0.1` 的随机端口。Runner 在脚本前后都通过目标 SOCKS5 观察 IPv4；有效报告 URL 加成功 postflight 才是 `report_ready`。同一 Runner 永远只有一个执行槽，其余任务由 AkastrCloud 排队，不能另起第二个 Agent 绕过。

“每个服务节点每天一次真实 IPQuality”仍由主控执行：同一天后续请求读取缓存；香港时间 `00:00` 或目标 IPv4 变化后重置。Runner 重装或新增 profile 不能绕过限额。

## 6. 安装器实际完成的工作

最终确认后，安装器按顺序：

1. 用 apt 安装该模式所需的 Debian 依赖；
2. 下载 v0.2.0 amd64 binary 并用内嵌 SHA-256 校验；
3. 生成严格 Agent config 和必要的 root-only secret 文件；
4. Runner 下载并校验内置 IPQuality 脚本；
5. 将 binary 安装到 immutable release 目录；
6. 运行 `check-config`，同时验证程序、状态、profile、脚本和依赖；
7. 使用终端中的一次性 token 执行 HTTPS enrollment；
8. enrollment 成功后删除本地 token 文件并清空 shell 变量；
9. 创建加固的 systemd unit，启用并启动服务；
10. 连续检查约 5 秒，确认 service 保持 active。

主要路径：

```text
/etc/akastr-agent/config.json
/etc/akastr-agent/identity.json
/var/lib/akastr-agent/
/usr/local/lib/akastr-agent/releases/v0.2.0/
/usr/local/lib/akastr-agent/current
/etc/systemd/system/akastr-agent.service
```

ChangeIP HTTP provider 另有 `/etc/akastr-agent/changeip-curl.conf`；Runner 另有 `/etc/akastr-agent/proxy-profiles.json` 和 `/usr/local/lib/akastr-agent/ipquality/ip.sh`。

配置、identity 和 secret 目录为 root-only；systemd service 的文件系统默认只读，只有 `/var/lib/akastr-agent` 可写。

## 7. 安装后验收

安装器显示成功后，在节点执行：

```bash
sudo systemctl is-active akastr-agent.service
sudo systemctl show akastr-agent.service \
  --property=MainPID,ActiveState,SubState,NRestarts
sudo /usr/local/lib/akastr-agent/current/akastr-agent version
sudo /usr/local/lib/akastr-agent/current/akastr-agent \
  check-config --config /etc/akastr-agent/config.json
sudo /usr/local/lib/akastr-agent/current/akastr-agent \
  capabilities --config /etc/akastr-agent/config.json
```

预期：

- `is-active` 输出 `active`；
- `MainPID` 不为 0；
- version 输出 `v0.2.0`；
- config 检查输出 `configuration valid`；
- capabilities 只含向导中启用的能力，不含 token、password 或 provider secret。

查看日志：

```bash
sudo journalctl -u akastr-agent.service -n 100 --no-pager
sudo journalctl -u akastr-agent.service --since '30 minutes ago' --no-pager
```

WSS 认证和 hello 完成后会出现 `control connection ready`。然后回到 AkastrCloud 后台，确认同一 installation 显示 active、版本和 capability 正确。主控未确认前不要开始迁移验收。

## 8. 日常使用

Agent 没有本地 `change-ip` 或 `ipquality` CLI。用户仍从 AkastrCloud/Carpool 发起：

- 延迟立即更换，默认 5 分钟；
- 预设更换；
- 自动定时更换；
- IPQuality 查询。

主控把前三种统一转换为带类型的 `changeip.execute`；IPQuality 只有在每日缓存、目标互斥和 Runner 排队规则通过后才派发。不能登录节点手工绕过主控策略。

自然 IPv4 首次观察只建立 baseline。之后的变化会先写入本地状态，再通过 WSS 至少一次投递；AkastrCloud 负责重置 IPQuality 缓存并向所有仍满足订阅条件的用户私聊播报。Agent 不连接 Telegram，也没有 channel 播报。

常用服务命令：

```bash
sudo systemctl status akastr-agent.service --no-pager
sudo systemctl restart akastr-agent.service
sudo journalctl -u akastr-agent.service -f
```

不要启动第二份 `akastr-agent run`，不要手工修改 `identity.json`、operation state 或 IP observation state。

## 9. 更新、状态与卸载

v0.2.0 没有独立 `update.sh` 或 `uninstall.sh`。以后要维护现有安装时，从 AkastrCloud 后台复制目标 release 新生成的一行命令并再次执行；安装器检测到现有 `current` 或 systemd unit 后显示：

```text
1) 更新到 <该安装器版本>
2) 查看服务状态
3) 卸载
4) 退出
```

### 更新

选择 1 后，安装器：

- 下载目标 release binary 并以内嵌摘要校验；
- 将它写入新的 immutable release 目录；
- 用现有 config 运行 `check-config`；
- 原子切换 `current` 并重启服务；
- 新服务无法稳定运行时恢复旧 symlink，并尝试重启旧版本。

更新保留 installation identity、config、secret 和本地状态，不重新 enrollment。运行与当前相同版本的安装器选择更新会被拒绝。

v0.2.0 的更新路径只更新 Agent binary，不重建向导配置、Runner profiles 或已安装的 IPQuality 脚本。未来 release 如果改变配置 schema 或 IPQuality pin，必须在该 release 明确提供迁移流程，不能假设现有更新菜单会自动改写。

更新后必须重新检查 version、service、restart count、capabilities 和后台 active 状态。自动恢复旧 symlink 不等于业务路由已经回滚。

### 查看状态

选择 2 只调用 systemd status，不修改安装。

### 永久卸载

选择 3 会再次要求明确确认，然后：

- 停止并 disable systemd service；
- 删除 unit；
- 永久删除 `/etc/akastr-agent`、`/var/lib/akastr-agent` 和 `/usr/local/lib/akastr-agent`；
- 删除 private key、provider secret、profiles 和本地 operation/observation state。

此操作不可由安装器恢复，而且不会自动吊销 AkastrCloud 后台的 installation。只有主控管理员先阻止新 command、完成业务回滚并明确允许销毁本地证据后才能卸载；之后还要在后台吊销 installation。

## 10. 常见故障

| 现象 | 含义与处理 |
| --- | --- |
| 安装器拒绝系统或架构 | 只支持 Debian 12/13 amd64；不要绕过检测或使用 ARM asset |
| 提示需要交互式终端 | 通过正常 SSH TTY 运行后台命令；不要使用 `curl | sh`、cron 或无 TTY automation |
| `installation UUID 格式不正确` | 后台命令被截断或修改；重新从同一 installation 复制，不要自行填写 |
| token 输入后提示格式不正确 | 重新复制后台一次性 token，确认没有空格或换行；不要把 token 发到聊天或命令行 |
| enrollment 返回 HTTP 非 200 | Gate 未开放、token 已用/过期或 installation 不匹配；保留现场并联系主控管理员，不要关闭 TLS |
| `check-config` 报 ChangeIP program 错误 | 自定义程序不存在、不是 regular file 或不可执行；修复本地 provider，不能改为通用 shell |
| HTTP ChangeIP 安装通过但操作失败 | 核对 HTTPS URL、专用 bearer 权限和服务商返回码；不要在日志或命令行输出 bearer |
| `ipv4_unchanged` | provider 完成，但 300 秒观察窗口内公网 IPv4 未改变 |
| `ipv4_observe_timed_out` | provider 后公网 IPv4 一直不可观察；这不代表旧地址仍然可达 |
| Runner 官方脚本校验失败 | 停止安装并保留错误；不要改 checksum 或改用在线浮动脚本 |
| `proxy_profile_not_found` | Runner 的 server key 与主控 `proxy_profile_id` 不一致 |
| `proxy_preflight_failed` / `proxy_postflight_failed` | 检查目标 SOCKS5 host/port、凭据、出站 TLS 和代理稳定性，不要打印密码 |
| `runner_busy` | 单并发执行槽正在使用，主控应排队 |
| service 反复重启 | 用 `systemctl show` 和 `journalctl` 查看稳定 code，再运行 `check-config`；不要删除 state 逃避错误 |

## 11. 高级排障边界

以下 CLI 只用于只读诊断：

```text
akastr-agent version
akastr-agent check-config --config /etc/akastr-agent/config.json
akastr-agent capabilities --config /etc/akastr-agent/config.json
```

`enroll` 由交互安装器管理，`run` 由 systemd 管理。不要手工重复 enrollment，也不要在服务已运行时另开前台 daemon。

如果安装在写入 runtime files 后失败，旧 IPChanger 不会被修改，但本地可能保留 partial installation。不要宽泛删除目录或反复使用新的 token；保留 `journalctl`、稳定错误 code 和现有 root-only 文件，交给主控管理员按该 installation 恢复或吊销。秘密文件内容不得复制到工单、聊天或日志。

安装路径中的 JSON 是运行时严格配置，不是推荐用户界面。高级排障可检查文件是否存在、owner 和 mode，但不得用猜测字段的方式修补：

```bash
sudo stat -c '%U %G %a %n' \
  /etc/akastr-agent/config.json \
  /etc/akastr-agent/identity.json
sudo readlink -f /usr/local/lib/akastr-agent/current
```

未知或损坏的 state schema 会失败关闭。不要删除 state 来强行启动，否则可能重复执行 command 或丢失自然 IP 事件。

## 12. 正式迁移与回滚边界

Gate 开放后，每个节点逐台进行：

1. 在后台创建对应 installation，取得一次性 token 和一行命令；
2. 保留旧 IPChanger，运行交互安装器；
3. 验证 service、WSS ready、capabilities 和自然 IPv4 baseline；
4. 由主控只把该节点的新 command 路由切到 Agent，避免两套执行端同时 ChangeIP；
5. 在批准窗口验证 ChangeIP、自然 IPv4 私聊、重连幂等，以及需要时的 IPQuality 排队/缓存；
6. 通过观察和回滚 Gate 后才停用旧实例。

回滚时先阻止新 command，保留 Agent identity/state/journal，恢复 AkastrCloud 到旧 integration，再验证旧路径。不能仅停止 Agent service 就宣称业务回滚，也不能在观察期选择永久卸载。

六个节点全部迁移、通过回滚期之前，旧 IPChanger 仓库和运行证据继续保持只读，不归档。当前生产 Gate 仍关闭，因此本节只是迁移边界，不是执行授权。
