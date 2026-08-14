#!/bin/sh
set -eu

VERSION='@AKASTR_AGENT_VERSION@'
BINARY_SHA256='@AKASTR_AGENT_BINARY_SHA256@'
ASSET='akastr-agent-linux-amd64'
RELEASE_BASE_URL=${AKASTR_RELEASE_BASE_URL:-https://github.com/akastrmix/akastr-agent/releases/download}
DEFAULT_CONTROL_ENDPOINT='wss://origin.akastrmix.com/internal/agents/ws'
IPQUALITY_COMMIT='0ee5f192fed70c04615852efba0e4b8bd43546c7'
IPQUALITY_VERSION='0ee5f192fed7'
IPQUALITY_SHA256='69cb11be5ff0853cb02a2ce038a6572f9792428601e2e74febe004fbd4391730'

CONFIG_DIR=/etc/akastr-agent
STATE_DIR=/var/lib/akastr-agent
RELEASE_ROOT=/usr/local/lib/akastr-agent
SERVICE_FILE=/etc/systemd/system/akastr-agent.service

say() { printf '%s\n' "$*"; }
fail() { printf '错误：%s\n' "$*" >&2; exit 1; }

prompt() {
  prompt_text=$1
  default_value=${2-}
  if [ -n "$default_value" ]; then
    printf '%s [%s]: ' "$prompt_text" "$default_value" >/dev/tty
  else
    printf '%s: ' "$prompt_text" >/dev/tty
  fi
  IFS= read -r prompt_value </dev/tty || fail '无法读取终端输入'
  [ -n "$prompt_value" ] || prompt_value=$default_value
  printf '%s' "$prompt_value"
}

prompt_secret() {
  secret_prompt=$1
  printf '%s: ' "$secret_prompt" >/dev/tty
  stty -echo </dev/tty
  IFS= read -r secret_value </dev/tty || {
    stty echo </dev/tty
    printf '\n' >/dev/tty
    fail '无法读取终端输入'
  }
  stty echo </dev/tty
  printf '\n' >/dev/tty
  printf '%s' "$secret_value"
}

confirm() {
  answer=$(prompt "$1" "N")
  case "$answer" in
    y|Y|yes|YES|Yes) return 0 ;;
    *) return 1 ;;
  esac
}

require_uuid() {
  printf '%s\n' "$1" | grep -Eq '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' \
    || fail 'installation UUID 格式不正确'
  [ "$1" != '00000000-0000-0000-0000-000000000000' ] || fail 'installation UUID 不能为零'
}

require_stable_id() {
  printf '%s\n' "$1" | grep -Eq '^[a-z0-9][a-z0-9._-]{0,63}$' \
    || fail '标识只能包含小写字母、数字、点、下划线和连字符，最长 64 字符'
}

require_number_between() {
  number_value=$1
  number_min=$2
  number_max=$3
  printf '%s\n' "$number_value" | grep -Eq '^[0-9]+$' || fail '请输入整数'
  [ "$number_value" -ge "$number_min" ] && [ "$number_value" -le "$number_max" ] \
    || fail "数值必须在 $number_min 到 $number_max 之间"
}

preflight() {
  [ "$(id -u)" -eq 0 ] || fail '请使用 sudo 运行安装器'
  [ -r /dev/tty ] || fail '安装器需要交互式终端，不能直接通过 curl | sh 运行'
  case "$(uname -m)" in
    x86_64|amd64) ;;
    *) fail '仅支持 x86_64 / amd64；本项目不再发布 ARM 版本' ;;
  esac
  [ -r /etc/os-release ] || fail '无法识别操作系统'
  # shellcheck disable=SC1091
  . /etc/os-release
  [ "${ID:-}" = 'debian' ] || fail '当前只支持 Debian 12/13'
  case "${VERSION_ID:-}" in
    12|13) ;;
    *) fail '当前只支持 Debian 12/13' ;;
  esac
}

make_temporary() {
  temporary=$(mktemp -d)
  chmod 0700 "$temporary"
  trap 'stty echo </dev/tty 2>/dev/null || true; rm -rf -- "$temporary"' EXIT HUP INT TERM
}

install_packages() {
  packages='ca-certificates curl jq'
  if [ "$install_mode" = 'runner' ]; then
    packages="$packages bash bc netcat-openbsd dnsutils iproute2"
  fi
  say '正在安装 Debian 依赖……'
  export DEBIAN_FRONTEND=noninteractive
  apt-get update
  # shellcheck disable=SC2086
  apt-get install -y --no-install-recommends $packages
}

download_binary() {
  binary_path="$temporary/$ASSET"
  curl --fail --silent --show-error --location \
    "$RELEASE_BASE_URL/$VERSION/$ASSET" -o "$binary_path"
  actual_sha256=$(sha256sum "$binary_path" | awk '{print $1}')
  [ "$actual_sha256" = "$BINARY_SHA256" ] || fail '安装包完整性校验失败'
  chmod 0755 "$binary_path"
}

prepare_ipquality() {
  [ "$install_mode" = 'runner' ] || return 0
  ipquality_path="$temporary/ip.sh"
  curl --fail --silent --show-error --location \
    "https://raw.githubusercontent.com/xykt/IPQuality/$IPQUALITY_COMMIT/ip.sh" \
    -o "$ipquality_path"
  ipquality_actual=$(sha256sum "$ipquality_path" | awk '{print $1}')
  [ "$ipquality_actual" = "$IPQUALITY_SHA256" ] || fail 'IPQuality 官方脚本完整性校验失败'
  chmod 0755 "$ipquality_path"
}

service_is_stable() {
  attempt=0
  while [ "$attempt" -lt 5 ]; do
    sleep 1
    [ "$(systemctl is-active akastr-agent.service 2>/dev/null || true)" = 'active' ] || return 1
    [ "$(systemctl show akastr-agent.service --property=MainPID --value)" != '0' ] || return 1
    attempt=$((attempt + 1))
  done
}

update_existing() {
  [ -f "$CONFIG_DIR/config.json" ] || fail '现有安装缺少配置文件'
  [ -x "$RELEASE_ROOT/current/akastr-agent" ] || fail '现有安装缺少当前版本'
  current_version=$($RELEASE_ROOT/current/akastr-agent version)
  [ "$current_version" != "$VERSION" ] || fail "当前已经是 $VERSION"
  download_binary
  release_dir="$RELEASE_ROOT/releases/$VERSION"
  [ ! -e "$release_dir" ] || fail "$VERSION 的不可变目录已存在"
  install -d -m 0755 "$release_dir"
  install -m 0755 "$binary_path" "$release_dir/akastr-agent"
  "$release_dir/akastr-agent" check-config --config "$CONFIG_DIR/config.json"
  previous=$(readlink -f "$RELEASE_ROOT/current")
  ln -sfn "$release_dir" "$RELEASE_ROOT/current"
  if ! systemctl restart akastr-agent.service || ! service_is_stable; then
    ln -sfn "$previous" "$RELEASE_ROOT/current"
    if ! systemctl restart akastr-agent.service || ! service_is_stable; then
      fail '更新和自动回退都失败，请立即检查 systemd 日志'
    fi
    fail '更新失败，已经恢复上一版本'
  fi
  say "Akastr Agent 已更新到 $VERSION"
}

uninstall_existing() {
  say '卸载会停止 Agent，并删除程序、配置、私钥与本地状态。'
  confirm '确认永久卸载 Akastr Agent？' || { say '已取消'; return; }
  systemctl disable --now akastr-agent.service >/dev/null 2>&1 || true
  rm -f -- "$SERVICE_FILE"
  systemctl daemon-reload
  rm -rf -- "$CONFIG_DIR" "$STATE_DIR" "$RELEASE_ROOT"
  say 'Akastr Agent 已卸载；主控中的 installation 仍需由管理员吊销。'
}

existing_menu() {
  say '检测到 Akastr Agent 已安装。'
  say "1) 更新到 $VERSION"
  say '2) 查看服务状态'
  say '3) 卸载'
  say '4) 退出'
  choice=$(prompt '请选择操作' '1')
  case "$choice" in
    1) update_existing ;;
    2) systemctl --no-pager --full status akastr-agent.service || true ;;
    3) uninstall_existing ;;
    4) say '已退出' ;;
    *) fail '无效选项' ;;
  esac
}

collect_common() {
  change_provider=''
  agent_id=${AKASTR_AGENT_ID:-}
  [ -n "$agent_id" ] || agent_id=$(prompt 'installation UUID')
  require_uuid "$agent_id"

  install_mode=${AKASTR_AGENT_MODE:-}
  if [ -z "$install_mode" ]; then
    say '1) 目标节点：监测 IP，可执行 ChangeIP，可公布 SOCKS5 描述'
    say '2) IPQuality Runner：通过目标节点 SOCKS5 排队运行检测'
    mode_choice=$(prompt '请选择安装类型' '1')
    case "$mode_choice" in 1) install_mode=target ;; 2) install_mode=runner ;; *) fail '无效安装类型' ;; esac
  fi
  case "$install_mode" in target|runner) ;; *) fail '安装类型必须是 target 或 runner' ;; esac

  default_name=${AKASTR_AGENT_NAME:-$(hostname -s 2>/dev/null || hostname)}
  node_name=$(prompt '节点名称' "$default_name")
  [ -n "$node_name" ] && [ "${#node_name}" -le 64 ] || fail '节点名称必须为 1-64 字符'
  control_endpoint=$(prompt '主控 WSS 地址' "${AKASTR_CONTROL_ENDPOINT:-$DEFAULT_CONTROL_ENDPOINT}")
  case "$control_endpoint" in wss://*/internal/agents/ws) ;; *) fail 'WSS 地址必须使用 wss:// 且路径为 /internal/agents/ws' ;; esac

  enrollment_token=$(prompt_secret '一次性 enrollment token（输入不会回显）')
  printf '%s\n' "$enrollment_token" | grep -Eq '^[A-Za-z0-9_-]{43}$' \
    || fail 'enrollment token 格式不正确'
}

collect_target() {
  ip_interval=$(prompt '公网 IPv4 检查间隔（秒）' '60')
  require_number_between "$ip_interval" 10 3600

  change_enabled=false
  change_program=''
  change_args='[]'
  change_provider=''
  change_url=''
  change_bearer=''
  if confirm '启用 ChangeIP？'; then
    change_enabled=true
    say '1) HTTP POST + Bearer token（推荐）'
    say '2) 本机可执行程序 + 参数'
    provider_choice=$(prompt '请选择 ChangeIP provider' '1')
    case "$provider_choice" in
      1)
        change_provider=curl
        change_url=$(prompt 'ChangeIP HTTPS URL')
        case "$change_url" in https://*) ;; *) fail 'ChangeIP URL 必须使用 https://' ;; esac
        change_bearer=$(prompt_secret 'Bearer token（输入不会回显）')
        printf '%s\n' "$change_bearer" | grep -Eq '^[A-Za-z0-9._~-]+$' || fail 'Bearer token 包含不支持的字符'
        change_program=/usr/bin/curl
        change_args='["--config","/etc/akastr-agent/changeip-curl.conf"]'
        ;;
      2)
        change_provider=command
        change_program=$(prompt '可执行程序绝对路径')
        case "$change_program" in /*) ;; *) fail '程序必须使用绝对路径' ;; esac
        arg_count=$(prompt '参数数量' '0')
        require_number_between "$arg_count" 0 32
        : > "$temporary/changeip-args"
        arg_index=1
        while [ "$arg_index" -le "$arg_count" ]; do
          argument=$(prompt "参数 $arg_index")
          [ -n "$argument" ] || fail '参数不能为空'
          printf '%s\n' "$argument" >> "$temporary/changeip-args"
          arg_index=$((arg_index + 1))
        done
        change_args='[]'
        ;;
      *) fail '无效 provider' ;;
    esac
  fi

  socks_enabled=false
  socks_source=observed_ipv4
  socks_host=''
  socks_port=0
  if confirm '向主控公布该节点的 SOCKS5 入口？'; then
    socks_enabled=true
    socks_port=$(prompt 'SOCKS5 端口')
    require_number_between "$socks_port" 1 65535
    if confirm 'SOCKS5 主机地址始终跟随观测到的公网 IPv4？'; then
      socks_source=observed_ipv4
    else
      socks_source=static
      socks_host=$(prompt '静态 SOCKS5 主机名或 IP')
      [ -n "$socks_host" ] || fail '静态 SOCKS5 主机不能为空'
    fi
  fi
}

collect_runner() {
  profile_count=$(prompt '要配置的 SOCKS5 凭据数量' '1')
  require_number_between "$profile_count" 1 128
  : > "$temporary/profiles.tsv"
  profile_index=1
  while [ "$profile_index" -le "$profile_count" ]; do
    profile_id=$(prompt "第 $profile_index 个 server key（小写，例如 hkt）")
    require_stable_id "$profile_id"
    if cut -f1 "$temporary/profiles.tsv" | grep -Fxq "$profile_id"; then
      fail "重复的 server key：$profile_id"
    fi
    profile_username=$(prompt "[$profile_id] SOCKS5 用户名")
    profile_password=$(prompt_secret "[$profile_id] SOCKS5 密码（输入不会回显）")
    [ -n "$profile_username" ] && [ -n "$profile_password" ] || fail '用户名和密码不能为空'
    case "$profile_username$profile_password" in *"	"*) fail '用户名和密码不能包含制表符' ;; esac
    printf '%s\t%s\t%s\n' "$profile_id" "$profile_username" "$profile_password" >> "$temporary/profiles.tsv"
    profile_index=$((profile_index + 1))
  done
}

show_summary() {
  say ''
  say '安装摘要（不显示任何秘密）：'
  say "  版本：$VERSION"
  say '  架构：linux/amd64'
  say "  installation：$agent_id"
  say "  名称：$node_name"
  say "  类型：$install_mode"
  say "  主控：$control_endpoint"
  if [ "$install_mode" = 'target' ]; then
    say "  IPv4 检查：每 $ip_interval 秒"
    say "  ChangeIP：$change_enabled"
    say "  SOCKS5 描述：$socks_enabled"
  else
    say "  IPQuality profiles：$profile_count"
    say '  Runner 并发：1（严格排队）'
  fi
  say ''
}

write_config() {
  config_file="$temporary/config.json"
  if [ "$install_mode" = 'target' ]; then
    if [ "$change_provider" = 'command' ]; then
      change_args=$(jq -Rsc 'split("\n") | map(select(length > 0))' "$temporary/changeip-args")
    fi
    jq -n \
      --arg id "$agent_id" --arg name "$node_name" --arg endpoint "$control_endpoint" \
      --argjson interval "$ip_interval" --argjson changeEnabled "$change_enabled" \
      --arg changeProgram "$change_program" --argjson changeArgs "$change_args" \
      --argjson socksEnabled "$socks_enabled" --arg socksSource "$socks_source" \
      --arg socksHost "$socks_host" --argjson socksPort "$socks_port" \
      '{schema_version:1,node:{id:$id,name:$name},control:{endpoint:$endpoint,credential_file:"/etc/akastr-agent/identity.json",enrollment_token_file:"/etc/akastr-agent/enrollment-token"},state_file:"/var/lib/akastr-agent/state.json",ip_state_file:"/var/lib/akastr-agent/ip-state.json",recent_operation_limit:64,capabilities:{ip_watch:{enabled:true,interval_seconds:$interval,observe_ipv6:false},change_ip:(if $changeEnabled then {enabled:true,program:$changeProgram,args:$changeArgs,timeout_seconds:60,observe_timeout_seconds:300} else {enabled:false} end),socks5:(if $socksEnabled then {enabled:true,address_source:$socksSource,advertised_host:$socksHost,port:$socksPort} else {enabled:false} end),ipquality_runner:{enabled:false}}}' \
      > "$config_file"
  else
    jq -n \
      --arg id "$agent_id" --arg name "$node_name" --arg endpoint "$control_endpoint" \
      --arg version "$IPQUALITY_VERSION" --arg checksum "$IPQUALITY_SHA256" \
      '{schema_version:1,node:{id:$id,name:$name},control:{endpoint:$endpoint,credential_file:"/etc/akastr-agent/identity.json",enrollment_token_file:"/etc/akastr-agent/enrollment-token"},state_file:"/var/lib/akastr-agent/state.json",ip_state_file:"/var/lib/akastr-agent/ip-state.json",recent_operation_limit:64,capabilities:{ip_watch:{enabled:false},change_ip:{enabled:false},socks5:{enabled:false},ipquality_runner:{enabled:true,script_path:"/usr/local/lib/akastr-agent/ipquality/ip.sh",proxy_profiles_file:"/etc/akastr-agent/proxy-profiles.json",timeout_seconds:900,max_concurrency:1,script_version:$version,script_sha256:$checksum}}}' \
      > "$config_file"
  fi
}

write_runtime_files() {
  release_dir="$RELEASE_ROOT/releases/$VERSION"
  [ ! -e "$CONFIG_DIR" ] || fail "$CONFIG_DIR 已存在，请先核对旧文件"
  [ ! -e "$STATE_DIR" ] || fail "$STATE_DIR 已存在，请先核对旧文件"
  [ ! -e "$RELEASE_ROOT" ] || fail "$RELEASE_ROOT 已存在，请先核对旧文件"
  [ ! -e "$SERVICE_FILE" ] || fail 'systemd service 已存在'

  install -d -m 0700 "$CONFIG_DIR" "$STATE_DIR"
  install -d -m 0755 "$release_dir"
  install -m 0755 "$binary_path" "$release_dir/akastr-agent"
  install -m 0600 "$config_file" "$CONFIG_DIR/config.json"
  printf '%s\n' "$enrollment_token" > "$CONFIG_DIR/enrollment-token"
  chmod 0600 "$CONFIG_DIR/enrollment-token"

  if [ "$change_provider" = 'curl' ]; then
    {
      printf 'url = "%s"\n' "$change_url"
      printf 'request = "POST"\n'
      printf 'header = "Authorization: Bearer %s"\n' "$change_bearer"
      printf 'fail\nsilent\nshow-error\n'
    } > "$CONFIG_DIR/changeip-curl.conf"
    chmod 0600 "$CONFIG_DIR/changeip-curl.conf"
  fi

  if [ "$install_mode" = 'runner' ]; then
    install -d -m 0755 "$RELEASE_ROOT/ipquality"
    install -m 0755 "$ipquality_path" "$RELEASE_ROOT/ipquality/ip.sh"
    jq -Rn '
      reduce inputs as $line ({schema_version:1,profiles:{}};
        ($line | split("\t")) as $parts |
        .profiles[$parts[0]] = {username:$parts[1],password:$parts[2]})
    ' < "$temporary/profiles.tsv" > "$CONFIG_DIR/proxy-profiles.json"
    chmod 0600 "$CONFIG_DIR/proxy-profiles.json"
  fi

  ln -sfn "$release_dir" "$RELEASE_ROOT/current"
}

write_service() {
  cat > "$SERVICE_FILE" <<'UNIT'
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
UNIT
  chmod 0644 "$SERVICE_FILE"
  systemctl daemon-reload
}

fresh_install() {
  collect_common
  if [ "$install_mode" = 'target' ]; then collect_target; else collect_runner; fi
  show_summary
  confirm '确认写入配置、完成 enrollment 并启动服务？' || { say '已取消，未修改系统'; return; }

  install_packages
  download_binary
  prepare_ipquality
  write_config
  write_runtime_files
  "$RELEASE_ROOT/current/akastr-agent" check-config --config "$CONFIG_DIR/config.json"
  if ! "$RELEASE_ROOT/current/akastr-agent" enroll --config "$CONFIG_DIR/config.json"; then
    fail 'enrollment 失败；旧 IPChanger 未被修改，请保留现场排查'
  fi
  rm -f -- "$CONFIG_DIR/enrollment-token"
  enrollment_token=''
  change_bearer=''
  write_service
  systemctl enable --now akastr-agent.service
  if ! service_is_stable; then
    systemctl stop akastr-agent.service >/dev/null 2>&1 || true
    fail '服务未能稳定运行；旧 IPChanger 未被修改'
  fi
  say "Akastr Agent $VERSION 已安装并运行。"
  say '请回到 AkastrCloud 确认 installation 已变为 active，再进行迁移验收。'
}

preflight
make_temporary
say "Akastr Agent $VERSION 交互式安装器"
if [ -e "$RELEASE_ROOT/current" ] || [ -e "$SERVICE_FILE" ]; then
  install_mode=target
  existing_menu
else
  fresh_install
fi
