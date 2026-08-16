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
SYSTEMD_ROOT=/etc/systemd/system
SERVICE_FILE=/etc/systemd/system/akastr-agent.service

temporary=''
transaction_started=false
backup_complete=false
transaction_complete=false
units_captured=false
enrollment_irreversible=false

CONFIG_BACKUP="/etc/akastr-agent.install-backup.$$"
STATE_BACKUP="/var/lib/akastr-agent.install-backup.$$"
RELEASE_BACKUP="/usr/local/lib/akastr-agent.install-backup.$$"
UNITS_BACKUP="/etc/akastr-agent-units.install-backup.$$"

say() { printf '%s\n' "$*"; }
fail() { printf 'Error: %s\n' "$*" >&2; exit 1; }

cleanup() {
  if [ "$transaction_started" = true ] && [ "$transaction_complete" != true ]; then
    if [ "$enrollment_irreversible" = true ]; then
      rm -f -- "$CONFIG_DIR/machine-token"
      rm -rf -- "$CONFIG_BACKUP" "$STATE_BACKUP" "$RELEASE_BACKUP" "$UNITS_BACKUP"
    else
      if [ "$units_captured" = true ]; then
        remove_agent_units
        rollback_directory "$CONFIG_BACKUP" "$CONFIG_DIR"
        rollback_directory "$STATE_BACKUP" "$STATE_DIR"
        rollback_directory "$RELEASE_BACKUP" "$RELEASE_ROOT"
      fi
      restore_agent_units
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

backup_existing() {
  source=$1
  backup=$2
  [ ! -e "$backup" ] && [ ! -L "$backup" ] || fail "transaction backup path already exists: $backup"
  if [ -e "$source" ] || [ -L "$source" ]; then
    mv -- "$source" "$backup"
  fi
}

remove_agent_units() {
  for unit_path in "$SYSTEMD_ROOT"/akastr-agent*.service "$SYSTEMD_ROOT"/akastr-agent*.timer; do
    if [ -e "$unit_path" ] || [ -L "$unit_path" ]; then
      unit_name=$(basename "$unit_path")
      systemctl disable --now "$unit_name" >/dev/null 2>&1 || true
      rm -f -- "$unit_path"
    fi
  done
  systemctl daemon-reload >/dev/null 2>&1 || true
}

capture_agent_units() {
  [ ! -e "$UNITS_BACKUP" ] && [ ! -L "$UNITS_BACKUP" ] \
    || fail "transaction backup path already exists: $UNITS_BACKUP"
  install -d -m 0700 "$UNITS_BACKUP/files"
  : > "$UNITS_BACKUP/enabled"
  : > "$UNITS_BACKUP/active"
  for unit_path in "$SYSTEMD_ROOT"/akastr-agent*.service "$SYSTEMD_ROOT"/akastr-agent*.timer; do
    if [ -e "$unit_path" ] || [ -L "$unit_path" ]; then
      [ -f "$unit_path" ] && [ ! -L "$unit_path" ] \
        || fail "Agent systemd unit is not a regular file: $unit_path"
      unit_name=$(basename "$unit_path")
      if systemctl is-enabled --quiet "$unit_name" 2>/dev/null; then
        printf '%s\n' "$unit_name" >> "$UNITS_BACKUP/enabled"
      fi
      if systemctl is-active --quiet "$unit_name" 2>/dev/null; then
        printf '%s\n' "$unit_name" >> "$UNITS_BACKUP/active"
      fi
      systemctl disable "$unit_name" >/dev/null
      systemctl stop "$unit_name" >/dev/null
      systemctl reset-failed "$unit_name" >/dev/null 2>&1 || true
      unit_state=$(systemctl is-active "$unit_name" 2>/dev/null || true)
      [ "$unit_state" = 'inactive' ] || fail "Agent systemd unit did not stop: $unit_name"
    fi
  done
  for unit_path in "$SYSTEMD_ROOT"/akastr-agent*.service "$SYSTEMD_ROOT"/akastr-agent*.timer; do
    if [ -e "$unit_path" ] || [ -L "$unit_path" ]; then
      unit_name=$(basename "$unit_path")
      mv -- "$unit_path" "$UNITS_BACKUP/files/$unit_name"
    fi
  done
  systemctl daemon-reload
  units_captured=true
}

restore_agent_units() {
  [ -d "$UNITS_BACKUP" ] || return 0
  for unit_path in "$UNITS_BACKUP"/files/*; do
    [ -f "$unit_path" ] || continue
    mv -- "$unit_path" "$SYSTEMD_ROOT/$(basename "$unit_path")"
  done
  systemctl daemon-reload >/dev/null 2>&1 || true
  while read -r unit_name; do
    [ -n "$unit_name" ] || continue
    systemctl enable "$unit_name" >/dev/null 2>&1 || true
  done < "$UNITS_BACKUP/enabled"
  while read -r unit_name; do
    [ -n "$unit_name" ] || continue
    systemctl start "$unit_name" >/dev/null 2>&1 || true
  done < "$UNITS_BACKUP/active"
  rm -rf -- "$UNITS_BACKUP"
}

require_uuid() {
  printf '%s\n' "$1" | grep -Eq '^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$' \
    || fail 'invalid node UUID'
}

preflight() {
  [ "$(id -u)" -eq 0 ] || fail 'run the installer as root'
  case "$(uname -m)" in
    x86_64|amd64) ;;
    *) fail 'only x86_64 / amd64 is supported' ;;
  esac
  [ -r /etc/os-release ] || fail 'cannot identify the operating system'
  os_identity=$(
    # Keep /etc/os-release variables such as VERSION out of installer state.
    # shellcheck disable=SC1091
    . /etc/os-release
    printf '%s:%s' "${ID:-}" "${VERSION_ID:-}"
  )
  case "$os_identity" in
    debian:12|debian:13) ;;
    *) fail 'only Debian 12 and Debian 13 are supported' ;;
  esac
  command -v systemctl >/dev/null 2>&1 || fail 'systemd is required'
  command -v wget >/dev/null 2>&1 || fail 'install ca-certificates, curl, and wget first'
  command -v curl >/dev/null 2>&1 || fail 'install ca-certificates, curl, and wget first'
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
    *) fail "$label download URL must use HTTPS" ;;
  esac
  if wget --no-hsts --https-only --tries=3 --timeout=30 -qO "$destination" "$url"; then
    :
  else
    wget_code=$?
    rm -f -- "$destination"
    fail "$label download failed (wget exit code $wget_code)"
  fi
  [ -s "$destination" ] || {
    rm -f -- "$destination"
    fail "$label download is empty"
  }
}

install_packages() {
  packages='ca-certificates curl wget'
  if [ "$1" = 'runner' ]; then
    packages="$packages bash bc netcat-openbsd dnsutils iproute2"
  fi
  export DEBIAN_FRONTEND=noninteractive
  apt-get update
  # shellcheck disable=SC2086
  apt-get install -y --no-install-recommends $packages
}

download_binary() {
  binary_path="$temporary/$ASSET"
  download_https "$RELEASE_BASE_URL/$AGENT_RELEASE_VERSION/$ASSET" "$binary_path" 'Agent binary'
  actual_sha256=$(sha256sum "$binary_path" | awk '{print $1}')
  [ "$actual_sha256" = "$BINARY_SHA256" ] || fail 'Agent binary integrity check failed'
  chmod 0755 "$binary_path"
}

prepare_ipquality() {
  [ "$install_mode" = 'runner' ] || return 0
  ipquality_path="$temporary/ip.sh"
  download_https \
    "https://raw.githubusercontent.com/xykt/IPQuality/$IPQUALITY_COMMIT/ip.sh" \
    "$ipquality_path" \
    'IPQuality script'
  ipquality_actual=$(sha256sum "$ipquality_path" | awk '{print $1}')
  [ "$ipquality_actual" = "$IPQUALITY_SHA256" ] || fail 'IPQuality script integrity check failed'
  chmod 0755 "$ipquality_path"
}

write_service_unit() {
  cat > "$SERVICE_FILE" <<'UNIT'
[Unit]
Description=Akastr Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=notify
NotifyAccess=main
ExecStart=/usr/local/lib/akastr-agent/current/akastr-agent run --config /etc/akastr-agent/config.json
Restart=always
RestartSec=5s
TimeoutStartSec=45s
TimeoutStopSec=30s
UMask=0077
NoNewPrivileges=true
PrivateTmp=true
PrivateDevices=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/akastr-agent /usr/local/lib/akastr-agent
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
  agent_id=${AKASTR_AGENT_ID:-}
  machine_token=${AKASTR_AGENT_MACHINE_TOKEN:-}
  bootstrap_endpoint=${AKASTR_AGENT_BOOTSTRAP_ENDPOINT:-}
  [ -n "$agent_id" ] || fail 'AKASTR_AGENT_ID is missing'
  [ -n "$machine_token" ] || fail 'AKASTR_AGENT_MACHINE_TOKEN is missing'
  [ -n "$bootstrap_endpoint" ] || fail 'AKASTR_AGENT_BOOTSTRAP_ENDPOINT is missing'
  require_uuid "$agent_id"
  printf '%s\n' "$machine_token" | grep -Eq '^[A-Za-z0-9_-]{43}$' \
    || fail 'invalid machine token'

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
    *) fail 'the control plane returned an unsupported installation mode' ;;
  esac

  install_packages "$install_mode"
  prepare_ipquality

  transaction_started=true
  capture_agent_units
  "$binary_path" check-idle --config "$bootstrap_dir/config.json"
  backup_existing "$CONFIG_DIR" "$CONFIG_BACKUP"
  backup_existing "$STATE_DIR" "$STATE_BACKUP"
  backup_existing "$RELEASE_ROOT" "$RELEASE_BACKUP"
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
  ln -sfn "$release_dir" "$RELEASE_ROOT/current"

  "$RELEASE_ROOT/current/akastr-agent" check-config --config "$CONFIG_DIR/config.json"
  write_service_unit
  if "$RELEASE_ROOT/current/akastr-agent" enroll --config "$CONFIG_DIR/config.json"; then
    enrollment_irreversible=true
  elif [ -f "$CONFIG_DIR/identity.json" ] && [ ! -L "$CONFIG_DIR/identity.json" ]; then
    enrollment_irreversible=true
    fail 'enrollment outcome is uncertain; rerun the one-click install command'
  else
    fail 'enrollment was rejected; the original Agent installation will be restored'
  fi
  rm -f -- "$CONFIG_DIR/machine-token" "$token_file"

  systemctl enable --now akastr-agent.service
  systemctl is-active --quiet akastr-agent.service \
    || fail 'the Agent service did not reach control-plane readiness'

  transaction_complete=true
  rm -rf -- "$CONFIG_BACKUP" "$STATE_BACKUP" "$RELEASE_BACKUP"
  rm -rf -- "$UNITS_BACKUP"
  say "Akastr Agent $AGENT_RELEASE_VERSION installed successfully."
}

show_status() {
  [ -e "$SERVICE_FILE" ] || fail 'Akastr Agent is not installed'
  systemctl --no-pager --full status akastr-agent.service
}

uninstall_existing() {
  [ "${1:-}" = '--confirm-destroy-local-agent' ] \
    || fail 'uninstall requires --confirm-destroy-local-agent'
  [ "$#" -eq 1 ] || fail 'invalid uninstall arguments'
  remove_agent_units
  rm -rf -- "$CONFIG_DIR" "$STATE_DIR" "$RELEASE_ROOT"
  say 'Akastr Agent uninstalled successfully.'
}

preflight
operation=${1:-}
case "$operation" in
  --install)
    [ "$#" -eq 1 ] || fail '--install accepts no additional arguments'
    fresh_install
    ;;
  --status)
    [ "$#" -eq 1 ] || fail '--status accepts no additional arguments'
    show_status
    ;;
  --uninstall)
    shift
    uninstall_existing "$@"
    ;;
  *)
    fail 'usage: install.sh --install | --status | --uninstall --confirm-destroy-local-agent'
    ;;
esac
