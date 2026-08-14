#!/bin/sh
set -eu

VERSION='@AKASTR_AGENT_VERSION@'
BINARY_SHA256='@AKASTR_AGENT_BINARY_SHA256@'
ASSET='akastr-agent-linux-amd64'
RELEASE_BASE_URL=${AKASTR_RELEASE_BASE_URL:-https://github.com/akastrmix/akastr-agent/releases/download}
IPQUALITY_COMMIT='0ee5f192fed70c04615852efba0e4b8bd43546c7'
IPQUALITY_SHA256='69cb11be5ff0853cb02a2ce038a6572f9792428601e2e74febe004fbd4391730'

CONFIG_DIR=/etc/akastr-agent
STATE_DIR=/var/lib/akastr-agent
RELEASE_ROOT=/usr/local/lib/akastr-agent
SERVICE_FILE=/etc/systemd/system/akastr-agent.service

temporary=''
fresh_files_created=false
fresh_complete=false

say() { printf '%s\n' "$*"; }
fail() { printf '错误：%s\n' "$*" >&2; exit 1; }

cleanup() {
  if [ "$fresh_files_created" = true ] && [ "$fresh_complete" != true ]; then
    systemctl disable --now akastr-agent.service >/dev/null 2>&1 || true
    rm -f -- "$SERVICE_FILE"
    systemctl daemon-reload >/dev/null 2>&1 || true
    rm -rf -- "$CONFIG_DIR" "$STATE_DIR" "$RELEASE_ROOT"
  fi
  if [ -n "$temporary" ] && [ -d "$temporary" ]; then
    rm -rf -- "$temporary"
  fi
}
trap cleanup EXIT
trap 'cleanup; exit 1' HUP INT TERM

require_uuid() {
  printf '%s\n' "$1" | grep -Eq '^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$' \
    || fail 'installation UUID 格式不正确'
}

preflight() {
  [ "$(id -u)" -eq 0 ] || fail '请使用 sudo 运行安装器'
  case "$(uname -m)" in
    x86_64|amd64) ;;
    *) fail '仅支持 x86_64 / amd64；本项目不发布 ARM 版本' ;;
  esac
  [ -r /etc/os-release ] || fail '无法识别操作系统'
  # shellcheck disable=SC1091
  . /etc/os-release
  [ "${ID:-}" = 'debian' ] || fail '当前只支持 Debian 12/13'
  case "${VERSION_ID:-}" in
    12|13) ;;
    *) fail '当前只支持 Debian 12/13' ;;
  esac
  command -v systemctl >/dev/null 2>&1 || fail '主机未使用 systemd'
  command -v curl >/dev/null 2>&1 || fail '请先安装 ca-certificates 和 curl'
}

make_temporary() {
  temporary=$(mktemp -d)
  chmod 0700 "$temporary"
}

install_packages() {
  packages='ca-certificates curl'
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
  curl --fail --silent --show-error --location \
    "$RELEASE_BASE_URL/$VERSION/$ASSET" -o "$binary_path"
  actual_sha256=$(sha256sum "$binary_path" | awk '{print $1}')
  [ "$actual_sha256" = "$BINARY_SHA256" ] || fail '安装包自动完整性校验失败'
  chmod 0755 "$binary_path"
}

prepare_ipquality() {
  [ "$install_mode" = 'runner' ] || return 0
  ipquality_path="$temporary/ip.sh"
  curl --fail --silent --show-error --location \
    "https://raw.githubusercontent.com/xykt/IPQuality/$IPQUALITY_COMMIT/ip.sh" \
    -o "$ipquality_path"
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
  [ ! -e "$CONFIG_DIR" ] || fail "$CONFIG_DIR 已存在；请先核对现有安装"
  [ ! -e "$STATE_DIR" ] || fail "$STATE_DIR 已存在；请先核对现有安装"
  [ ! -e "$RELEASE_ROOT" ] || fail "$RELEASE_ROOT 已存在；请先核对现有安装"
  [ ! -e "$SERVICE_FILE" ] || fail 'systemd service 已存在；请使用 --update'

  agent_id=${AKASTR_AGENT_ID:-}
  enrollment_token=${AKASTR_AGENT_ENROLLMENT_TOKEN:-}
  bootstrap_endpoint=${AKASTR_AGENT_BOOTSTRAP_ENDPOINT:-}
  [ -n "$agent_id" ] || fail '安装命令缺少 AKASTR_AGENT_ID'
  [ -n "$enrollment_token" ] || fail '安装命令缺少一次性 token'
  [ -n "$bootstrap_endpoint" ] || fail '安装命令缺少 HTTPS bootstrap endpoint'
  require_uuid "$agent_id"
  printf '%s\n' "$enrollment_token" | grep -Eq '^[A-Za-z0-9_-]{43}$' \
    || fail '一次性 token 格式不正确'

  make_temporary
  token_file="$temporary/enrollment-token"
  printf '%s\n' "$enrollment_token" > "$token_file"
  chmod 0600 "$token_file"
  unset AKASTR_AGENT_ENROLLMENT_TOKEN enrollment_token

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

  release_dir="$RELEASE_ROOT/releases/$VERSION"
  fresh_files_created=true
  install -d -m 0700 "$CONFIG_DIR" "$STATE_DIR"
  install -d -m 0755 "$release_dir"
  install -m 0755 "$binary_path" "$release_dir/akastr-agent"
  install -m 0600 "$bootstrap_dir/config.json" "$CONFIG_DIR/config.json"
  install -m 0600 "$bootstrap_dir/enrollment-token" "$CONFIG_DIR/enrollment-token"
  if [ -f "$bootstrap_dir/changeip-curl.conf" ]; then
    install -m 0600 "$bootstrap_dir/changeip-curl.conf" "$CONFIG_DIR/changeip-curl.conf"
  fi
  if [ "$install_mode" = 'runner' ]; then
    install -m 0600 "$bootstrap_dir/proxy-profiles.json" "$CONFIG_DIR/proxy-profiles.json"
    install -d -m 0755 "$RELEASE_ROOT/ipquality"
    install -m 0755 "$ipquality_path" "$RELEASE_ROOT/ipquality/ip.sh"
  fi
  ln -sfn "$release_dir" "$RELEASE_ROOT/current"

  "$RELEASE_ROOT/current/akastr-agent" check-config --config "$CONFIG_DIR/config.json"
  write_service
  "$RELEASE_ROOT/current/akastr-agent" enroll --config "$CONFIG_DIR/config.json" \
    || fail 'enrollment 失败；旧 IPChanger 未被修改'
  fresh_complete=true
  rm -f -- "$CONFIG_DIR/enrollment-token" "$token_file"

  systemctl enable --now akastr-agent.service
  service_is_stable || fail '服务未能稳定运行；旧 IPChanger 未被修改'
  say "Akastr Agent $VERSION 已安装并运行。"
  say '请回到 AkastrCloud 确认 installation 已变为已注册，再进行迁移验收。'
}

update_existing() {
  [ -f "$CONFIG_DIR/config.json" ] || fail '现有安装缺少配置文件'
  [ -x "$RELEASE_ROOT/current/akastr-agent" ] || fail '现有安装缺少当前版本'
  current_version=$($RELEASE_ROOT/current/akastr-agent version)
  [ "$current_version" != "$VERSION" ] || { say "当前已经是 $VERSION。"; return; }
  make_temporary
  install_packages target
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
  say "Akastr Agent 已更新到 $VERSION。"
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
  rm -f -- "$SERVICE_FILE"
  systemctl daemon-reload
  rm -rf -- "$CONFIG_DIR" "$STATE_DIR" "$RELEASE_ROOT"
  say 'Akastr Agent 已卸载；主控中的 installation 仍需由管理员吊销。'
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
