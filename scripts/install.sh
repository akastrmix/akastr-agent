#!/bin/sh
set -eu

AGENT_RELEASE_VERSION='@AKASTR_AGENT_VERSION@'
BINARY_SHA256='@AKASTR_AGENT_BINARY_SHA256@'
ASSET='akastr-agent-linux-amd64'
RELEASE_BASE_URL=${AKASTR_RELEASE_BASE_URL:-https://github.com/akastrmix/akastr-agent/releases/download}
IPQUALITY_COMMIT='0ee5f192fed70c04615852efba0e4b8bd43546c7'
IPQUALITY_SHA256='69cb11be5ff0853cb02a2ce038a6572f9792428601e2e74febe004fbd4391730'

CONFIG_DIR=/etc/akastr-agent
STATE_DIR=/var/lib/akastr-agent
RELEASE_ROOT=/usr/local/lib/akastr-agent
SERVICE_FILE=/etc/systemd/system/akastr-agent.service
UPDATE_SERVICE_FILE=/etc/systemd/system/akastr-agent-update.service
UPDATE_TIMER_FILE=/etc/systemd/system/akastr-agent-update.timer

temporary=''
transaction_started=false
backup_complete=false
transaction_complete=false
preserve_identity=false
old_agent_enabled=false
old_agent_active=false
old_update_timer_enabled=false
old_update_timer_active=false

CONFIG_BACKUP="/etc/akastr-agent.install-backup.$$"
STATE_BACKUP="/var/lib/akastr-agent.install-backup.$$"
RELEASE_BACKUP="/usr/local/lib/akastr-agent.install-backup.$$"
SERVICE_BACKUP="$SERVICE_FILE.install-backup.$$"
UPDATE_SERVICE_BACKUP="$UPDATE_SERVICE_FILE.install-backup.$$"
UPDATE_TIMER_BACKUP="$UPDATE_TIMER_FILE.install-backup.$$"

say() { printf '%s\n' "$*"; }
fail() { printf '错误：%s\n' "$*" >&2; exit 1; }

cleanup() {
  if [ "$transaction_started" = true ] && [ "$transaction_complete" != true ]; then
    systemctl disable --now akastr-agent.service >/dev/null 2>&1 || true
    systemctl disable --now akastr-agent-update.timer >/dev/null 2>&1 || true
    systemctl stop akastr-agent-update.service >/dev/null 2>&1 || true
    rollback_directory "$CONFIG_BACKUP" "$CONFIG_DIR"
    rollback_directory "$STATE_BACKUP" "$STATE_DIR"
    rollback_directory "$RELEASE_BACKUP" "$RELEASE_ROOT"
    rollback_file "$SERVICE_BACKUP" "$SERVICE_FILE"
    rollback_file "$UPDATE_SERVICE_BACKUP" "$UPDATE_SERVICE_FILE"
    rollback_file "$UPDATE_TIMER_BACKUP" "$UPDATE_TIMER_FILE"
    systemctl daemon-reload >/dev/null 2>&1 || true
    if [ "$old_agent_enabled" = true ]; then
      systemctl enable akastr-agent.service >/dev/null 2>&1 || true
    fi
    if [ "$old_agent_active" = true ]; then
      systemctl start akastr-agent.service >/dev/null 2>&1 || true
    fi
    if [ "$old_update_timer_enabled" = true ]; then
      systemctl enable akastr-agent-update.timer >/dev/null 2>&1 || true
    fi
    if [ "$old_update_timer_active" = true ]; then
      systemctl start akastr-agent-update.timer >/dev/null 2>&1 || true
    fi
    transaction_complete=true
  fi
  if [ -n "$temporary" ] && [ -d "$temporary" ]; then
    rm -rf -- "$temporary"
  fi
}
trap cleanup EXIT
trap 'cleanup; exit 1' HUP INT TERM

rollback_directory() {
  backup=$1
  destination=$2
  if [ -e "$backup" ] || [ -L "$backup" ]; then
    rm -rf -- "$destination"
    mv -- "$backup" "$destination"
  elif [ "$backup_complete" = true ]; then
    rm -rf -- "$destination"
  fi
}

rollback_file() {
  backup=$1
  destination=$2
  if [ -e "$backup" ] || [ -L "$backup" ]; then
    rm -f -- "$destination"
    mv -- "$backup" "$destination"
  elif [ "$backup_complete" = true ]; then
    rm -f -- "$destination"
  fi
}

backup_existing() {
  source=$1
  backup=$2
  [ ! -e "$backup" ] && [ ! -L "$backup" ] || fail "安装事务备份路径已存在：$backup"
  if [ -e "$source" ] || [ -L "$source" ]; then
    mv -- "$source" "$backup"
  fi
}

require_uuid() {
  printf '%s\n' "$1" | grep -Eq '^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$' \
    || fail '节点 UUID 格式不正确'
}

preflight() {
  [ "$(id -u)" -eq 0 ] || fail '请使用 sudo 运行安装器'
  case "$(uname -m)" in
    x86_64|amd64) ;;
    *) fail '仅支持 x86_64 / amd64；本项目不发布 ARM 版本' ;;
  esac
  [ -r /etc/os-release ] || fail '无法识别操作系统'
  os_identity=$(
    # Keep /etc/os-release variables such as VERSION out of installer state.
    # shellcheck disable=SC1091
    . /etc/os-release
    printf '%s:%s' "${ID:-}" "${VERSION_ID:-}"
  )
  case "$os_identity" in
    debian:12|debian:13) ;;
    *) fail '当前只支持 Debian 12/13' ;;
  esac
  command -v systemctl >/dev/null 2>&1 || fail '主机未使用 systemd'
  command -v wget >/dev/null 2>&1 || fail '请先安装 ca-certificates、curl 和 wget'
  command -v curl >/dev/null 2>&1 || fail '请先安装 ca-certificates、curl 和 wget'
}

make_temporary() {
  temporary=$(mktemp -d)
  chmod 0700 "$temporary"
}

download_https() {
  url=$1
  destination=$2
  label=$3
  case "$url" in
    https://*) ;;
    *) fail "$label 下载地址必须使用 HTTPS" ;;
  esac
  if wget --no-hsts --https-only --tries=3 --timeout=30 -qO "$destination" "$url"; then
    :
  else
    wget_code=$?
    rm -f -- "$destination"
    fail "$label 下载失败（wget 退出码 $wget_code）"
  fi
  [ -s "$destination" ] || {
    rm -f -- "$destination"
    fail "$label 下载结果为空"
  }
}

install_packages() {
  packages='ca-certificates curl wget'
  if [ "$1" = 'runner' ]; then
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
  download_https "$RELEASE_BASE_URL/$AGENT_RELEASE_VERSION/$ASSET" "$binary_path" 'Agent binary'
  actual_sha256=$(sha256sum "$binary_path" | awk '{print $1}')
  [ "$actual_sha256" = "$BINARY_SHA256" ] || fail '安装包自动完整性校验失败'
  chmod 0755 "$binary_path"
}

prepare_ipquality() {
  [ "$install_mode" = 'runner' ] || return 0
  ipquality_path="$temporary/ip.sh"
  download_https \
    "https://raw.githubusercontent.com/xykt/IPQuality/$IPQUALITY_COMMIT/ip.sh" \
    "$ipquality_path" \
    'IPQuality 脚本'
  ipquality_actual=$(sha256sum "$ipquality_path" | awk '{print $1}')
  [ "$ipquality_actual" = "$IPQUALITY_SHA256" ] || fail 'IPQuality 官方脚本自动完整性校验失败'
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

write_units() {
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
  cat > "$UPDATE_SERVICE_FILE" <<'UNIT'
[Unit]
Description=Check and apply an AkastrCloud-approved Akastr Agent update
After=network-online.target
Wants=network-online.target
ConditionPathIsExecutable=/usr/local/lib/akastr-agent/current/akastr-agent

[Service]
Type=oneshot
ExecStart=/usr/local/lib/akastr-agent/current/akastr-agent self-update --config /etc/akastr-agent/config.json --release-root /usr/local/lib/akastr-agent
TimeoutStartSec=10min
UMask=0077
NoNewPrivileges=true
PrivateTmp=true
PrivateDevices=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/usr/local/lib/akastr-agent
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
LockPersonality=true
MemoryDenyWriteExecute=true
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
UNIT
  chmod 0644 "$UPDATE_SERVICE_FILE"
  cat > "$UPDATE_TIMER_FILE" <<'UNIT'
[Unit]
Description=Check for an AkastrCloud-approved Akastr Agent update every six hours

[Timer]
OnCalendar=*-*-* 00/6:00:00
RandomizedDelaySec=10m
Persistent=true
AccuracySec=1m
Unit=akastr-agent-update.service

[Install]
WantedBy=timers.target
UNIT
  chmod 0644 "$UPDATE_TIMER_FILE"
  systemctl daemon-reload
}

fresh_install() {
  agent_id=${AKASTR_AGENT_ID:-}
  machine_token=${AKASTR_AGENT_MACHINE_TOKEN:-}
  bootstrap_endpoint=${AKASTR_AGENT_BOOTSTRAP_ENDPOINT:-}
  [ -n "$agent_id" ] || fail '安装命令缺少 AKASTR_AGENT_ID'
  [ -n "$machine_token" ] || fail '安装命令缺少机器 token'
  [ -n "$bootstrap_endpoint" ] || fail '安装命令缺少 HTTPS bootstrap endpoint'
  require_uuid "$agent_id"
  printf '%s\n' "$machine_token" | grep -Eq '^[A-Za-z0-9_-]{43}$' \
    || fail '机器 token 格式不正确'

  make_temporary
  token_file="$temporary/machine-token"
  printf '%s\n' "$machine_token" > "$token_file"
  chmod 0600 "$token_file"
  unset AKASTR_AGENT_MACHINE_TOKEN machine_token

  download_binary
  bootstrap_dir="$temporary/bootstrap"
  install -d -m 0700 "$bootstrap_dir"
  bootstrap_result=$(
    "$binary_path" bootstrap \
      --agent-id "$agent_id" \
      --endpoint "$bootstrap_endpoint" \
      --token-file "$token_file" \
      --output-dir "$bootstrap_dir"
  )
  case "$bootstrap_result" in
    bootstrap_mode=target) install_mode=target ;;
    bootstrap_mode=runner) install_mode=runner ;;
    *) fail '主控返回了不支持的安装模式' ;;
  esac

  install_packages "$install_mode"
  prepare_ipquality

  if [ -f "$CONFIG_DIR/identity.json" ] && [ ! -L "$CONFIG_DIR/identity.json" ] && \
    "$binary_path" check-identity \
      --identity "$CONFIG_DIR/identity.json" --agent-id "$agent_id" >/dev/null 2>&1; then
    preserve_identity=true
    if [ -e "$STATE_DIR" ] || [ -L "$STATE_DIR" ]; then
      [ -d "$STATE_DIR" ] && [ ! -L "$STATE_DIR" ] || fail '同一节点的本地状态目录不安全，拒绝覆盖'
      if find "$STATE_DIR" -type l -print -quit | grep -q .; then
        fail '同一节点的本地状态包含符号链接，拒绝覆盖'
      fi
    fi
  fi

  if systemctl is-enabled --quiet akastr-agent.service 2>/dev/null; then old_agent_enabled=true; fi
  if systemctl is-active --quiet akastr-agent.service 2>/dev/null; then old_agent_active=true; fi
  if systemctl is-enabled --quiet akastr-agent-update.timer 2>/dev/null; then old_update_timer_enabled=true; fi
  if systemctl is-active --quiet akastr-agent-update.timer 2>/dev/null; then old_update_timer_active=true; fi

  transaction_started=true
  systemctl disable --now akastr-agent-update.timer >/dev/null 2>&1 || true
  systemctl stop akastr-agent-update.service >/dev/null 2>&1 || true
  systemctl stop akastr-agent.service >/dev/null 2>&1 || true
  backup_existing "$CONFIG_DIR" "$CONFIG_BACKUP"
  backup_existing "$STATE_DIR" "$STATE_BACKUP"
  backup_existing "$RELEASE_ROOT" "$RELEASE_BACKUP"
  backup_existing "$SERVICE_FILE" "$SERVICE_BACKUP"
  backup_existing "$UPDATE_SERVICE_FILE" "$UPDATE_SERVICE_BACKUP"
  backup_existing "$UPDATE_TIMER_FILE" "$UPDATE_TIMER_BACKUP"
  backup_complete=true

  release_dir="$RELEASE_ROOT/releases/$AGENT_RELEASE_VERSION"
  install -d -m 0700 "$CONFIG_DIR" "$STATE_DIR"
  install -d -m 0755 "$release_dir"
  install -m 0755 "$binary_path" "$release_dir/akastr-agent"
  install -m 0600 "$bootstrap_dir/config.json" "$CONFIG_DIR/config.json"
  install -m 0600 "$bootstrap_dir/machine-token" "$CONFIG_DIR/machine-token"
  if [ -f "$bootstrap_dir/changeip-curl.conf" ]; then
    install -m 0600 "$bootstrap_dir/changeip-curl.conf" "$CONFIG_DIR/changeip-curl.conf"
  fi
  if [ "$install_mode" = 'runner' ]; then
    install -m 0600 "$bootstrap_dir/proxy-profiles.json" "$CONFIG_DIR/proxy-profiles.json"
    install -d -m 0755 "$RELEASE_ROOT/ipquality"
    install -m 0755 "$ipquality_path" "$RELEASE_ROOT/ipquality/ip.sh"
  fi
  if [ "$preserve_identity" = true ]; then
    install -m 0600 "$CONFIG_BACKUP/identity.json" "$CONFIG_DIR/identity.json"
    if [ -d "$STATE_BACKUP" ] && [ ! -L "$STATE_BACKUP" ]; then
      cp -a "$STATE_BACKUP/." "$STATE_DIR/"
    fi
  fi
  ln -sfn "$release_dir" "$RELEASE_ROOT/current"

  "$RELEASE_ROOT/current/akastr-agent" check-config --config "$CONFIG_DIR/config.json"
  write_units
  if [ "$preserve_identity" != true ]; then
    "$RELEASE_ROOT/current/akastr-agent" enroll --config "$CONFIG_DIR/config.json" \
      || fail 'enrollment 失败；旧 Agent 已自动恢复，旧 IPChanger 未被修改'
  fi
  rm -f -- "$CONFIG_DIR/machine-token" "$token_file"

  systemctl enable akastr-agent.service akastr-agent-update.timer
  systemctl start akastr-agent.service
  service_is_stable || fail '服务未能稳定运行；旧 Agent 已自动恢复，旧 IPChanger 未被修改'
  systemctl start akastr-agent-update.timer

  transaction_complete=true
  rm -rf -- "$CONFIG_BACKUP" "$STATE_BACKUP" "$RELEASE_BACKUP"
  rm -f -- "$SERVICE_BACKUP" "$UPDATE_SERVICE_BACKUP" "$UPDATE_TIMER_BACKUP"
  say "Akastr Agent $AGENT_RELEASE_VERSION 已安装并运行。"
  say '自动更新每六小时检查一次，只接受 AkastrCloud 批准的同协议版本。'
  say '请回到 AkastrCloud 确认节点已在线，再进行迁移验收。'
}

update_existing() {
  [ -f "$CONFIG_DIR/config.json" ] || fail '现有安装缺少配置文件'
  [ -x "$RELEASE_ROOT/current/akastr-agent" ] || fail '现有安装缺少当前版本'
  current_version=$($RELEASE_ROOT/current/akastr-agent version)
  if [ "$current_version" = "$AGENT_RELEASE_VERSION" ]; then
    write_units
    systemctl enable --now akastr-agent-update.timer
    say "当前已经是 $AGENT_RELEASE_VERSION，自动更新 timer 已启用。"
    return
  fi
  make_temporary
  install_packages target
  download_binary
  release_dir="$RELEASE_ROOT/releases/$AGENT_RELEASE_VERSION"
  [ ! -e "$release_dir" ] || fail "$AGENT_RELEASE_VERSION 的不可变目录已存在"
  install -d -m 0755 "$release_dir"
  install -m 0755 "$binary_path" "$release_dir/akastr-agent"
  "$release_dir/akastr-agent" check-config --config "$CONFIG_DIR/config.json"
  previous=$(readlink -f "$RELEASE_ROOT/current")
  write_units
  ln -sfn "$release_dir" "$RELEASE_ROOT/current"
  if ! systemctl restart akastr-agent.service || ! service_is_stable; then
    ln -sfn "$previous" "$RELEASE_ROOT/current"
    if ! systemctl restart akastr-agent.service || ! service_is_stable; then
      fail '更新和自动回退都失败，请立即检查 systemd 日志'
    fi
    fail '更新失败，已经恢复上一版本'
  fi
  systemctl enable --now akastr-agent-update.timer
  say "Akastr Agent 已更新到 $AGENT_RELEASE_VERSION。"
}

show_status() {
  [ -e "$SERVICE_FILE" ] || fail 'Akastr Agent 尚未安装'
  systemctl --no-pager --full status akastr-agent.service
}

uninstall_existing() {
  [ "${1:-}" = '--confirm-destroy-local-agent' ] \
    || fail '卸载必须显式追加 --confirm-destroy-local-agent'
  [ "$#" -eq 1 ] || fail '卸载参数不正确'
  systemctl disable --now akastr-agent.service >/dev/null 2>&1 || true
  systemctl disable --now akastr-agent-update.timer >/dev/null 2>&1 || true
  systemctl stop akastr-agent-update.service >/dev/null 2>&1 || true
  rm -f -- "$SERVICE_FILE" "$UPDATE_SERVICE_FILE" "$UPDATE_TIMER_FILE"
  systemctl daemon-reload
  rm -rf -- "$CONFIG_DIR" "$STATE_DIR" "$RELEASE_ROOT"
  say 'Akastr Agent 已卸载；如需永久移除，请在主控中删除节点。'
}

preflight
operation=${1:-}
case "$operation" in
  --install)
    [ "$#" -eq 1 ] || fail '--install 不接受额外参数'
    fresh_install
    ;;
  --update)
    [ "$#" -eq 1 ] || fail '--update 不接受额外参数'
    update_existing
    ;;
  --status)
    [ "$#" -eq 1 ] || fail '--status 不接受额外参数'
    show_status
    ;;
  --uninstall)
    shift
    uninstall_existing "$@"
    ;;
  *)
    fail '用法：install.sh --install | --update | --status | --uninstall --confirm-destroy-local-agent'
    ;;
esac
