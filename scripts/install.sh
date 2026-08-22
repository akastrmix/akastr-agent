#!/bin/sh
set -eu

AGENT_RELEASE_VERSION='@AKASTR_AGENT_VERSION@'
BINARY_SHA256='@AKASTR_AGENT_BINARY_SHA256@'
ASSET='akastr-agent-linux-amd64'
RELEASE_BASE_URL=${AKASTR_RELEASE_BASE_URL:-https://github.com/akastrmix/akastr-agent/releases/download}
IPQUALITY_COMMIT='0ee5f192fed70c04615852efba0e4b8bd43546c7'
IPQUALITY_SHA256='9823c560e0d19769eb627329a31cb47da655d087166d86e40d9b6c77bc7f32fb'
RUNNER_PACKAGES='bash bc dnsutils iproute2 jq netcat-openbsd'
RUNNER_COMMANDS='/bin/bash bc curl dig ip jq nc'

CONFIG_DIR=/etc/akastr-agent
STATE_DIR=/var/lib/akastr-agent
CONFIGURATION_ROOT=$STATE_DIR/configurations
RELEASE_ROOT=/usr/local/lib/akastr-agent
SERVICE_FILE=/etc/systemd/system/akastr-agent.service

temporary=''
preserved_identity=''
service_stopped=false
installation_complete=false
machine_token_installed=false
reuse_ipquality=false
configuration_staging=''

say() { printf '%s\n' "$*"; }
fail() { printf 'Error: %s\n' "$*" >&2; exit 1; }

cleanup() {
  cleanup_status=$?
  trap - EXIT HUP INT TERM
  if [ "$machine_token_installed" = true ]; then
    rm -f -- "$CONFIG_DIR/machine-token" || true
  fi
  if [ -n "$temporary" ] && [ -d "$temporary" ]; then
    rm -rf -- "$temporary" || true
  fi
  if [ -n "$configuration_staging" ] && [ -d "$configuration_staging" ]; then
    rm -rf -- "$configuration_staging" || true
  fi
  if [ "$cleanup_status" -ne 0 ] && [ "$service_stopped" = true ] && [ "$installation_complete" != true ]; then
    printf 'Error: Agent operation is incomplete; fix the reported error and rerun the same command.\n' >&2
  fi
  exit "$cleanup_status"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

stop_agent_service() {
  if [ -e "$SERVICE_FILE" ] || [ -L "$SERVICE_FILE" ]; then
    [ -f "$SERVICE_FILE" ] && [ ! -L "$SERVICE_FILE" ] \
      || fail "Agent systemd unit is not a regular file: $SERVICE_FILE"
  fi
  if systemctl is-enabled --quiet akastr-agent.service 2>/dev/null; then
    systemctl disable akastr-agent.service >/dev/null
  fi
  if systemctl is-active --quiet akastr-agent.service 2>/dev/null; then
    systemctl stop akastr-agent.service >/dev/null
  fi
  if systemctl is-failed --quiet akastr-agent.service 2>/dev/null; then
    systemctl reset-failed akastr-agent.service >/dev/null
  fi
  unit_state=$(systemctl is-active akastr-agent.service 2>/dev/null || true)
  case "$unit_state" in
    inactive|unknown) ;;
    *) fail "Agent systemd unit did not stop: akastr-agent.service ($unit_state)" ;;
  esac
  service_stopped=true
}

require_uuid() {
  printf '%s\n' "$1" | grep -Eq '^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$' \
    || fail 'invalid node UUID'
}

require_root() {
  [ "$(id -u)" -eq 0 ] || fail 'run the installer as root'
}

require_systemd() {
  command -v systemctl >/dev/null 2>&1 || fail 'systemd is required'
}

preflight_install() {
  require_root
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
  require_systemd
  command -v curl >/dev/null 2>&1 || fail 'curl is required'
  command -v sha256sum >/dev/null 2>&1 || fail 'sha256sum is required'
}

preflight_status() {
  require_systemd
}

preflight_uninstall() {
  require_root
  require_systemd
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
  if curl -fsSL --output "$destination" "$url"; then
    :
  else
    download_code=$?
    rm -f -- "$destination"
    fail "$label download failed (curl exit code $download_code)"
  fi
  [ -s "$destination" ] || {
    rm -f -- "$destination"
    fail "$label download is empty"
  }
}

read_identity_agent_id() {
  LC_ALL=C tr -d '[:space:]' < "$1" \
    | sed -n 's/^{"schema_version":[0-9][0-9]*,"enrollment_state":"[^"]*","agent_id":"\([^"]*\)",.*$/\1/p' \
    | sed -n '1p'
}

read_config_agent_id() {
  LC_ALL=C tr -d '[:space:]' < "$1" \
    | sed -n 's/^{"schema_version":[0-9][0-9]*,.*"node":{"id":"\([^"]*\)",.*$/\1/p' \
    | sed -n '1p'
}

read_config_revision() {
  LC_ALL=C tr -d '[:space:]' < "$1" \
    | sed -n 's/^.*"configuration_revision":\([1-9][0-9]*\),.*$/\1/p' \
    | sed -n '1p'
}

version_is_newer() {
  left=${1#v}
  right=${2#v}
  old_ifs=$IFS
  IFS=.
  set -- $left $right
  IFS=$old_ifs
  [ "$1" -gt "$4" ] || { [ "$1" -eq "$4" ] && [ "$2" -gt "$5" ]; } \
    || { [ "$1" -eq "$4" ] && [ "$2" -eq "$5" ] && [ "$3" -gt "$6" ]; }
}

inspect_existing_install() {
  requested_id=$1
  existing_identity="$CONFIG_DIR/identity.json"
  existing_config="$RELEASE_ROOT/current/config/config.json"
  if [ ! -f "$existing_config" ]; then
    existing_config="$CONFIG_DIR/config.json"
  fi
  existing_binary="$RELEASE_ROOT/current/akastr-agent"
  identity_id=''
  config_id=''

  if [ -e "$existing_identity" ] || [ -L "$existing_identity" ]; then
    [ -f "$existing_identity" ] && [ ! -L "$existing_identity" ] \
      || fail "existing identity is not a regular file: $existing_identity"
    identity_id=$(read_identity_agent_id "$existing_identity")
    require_uuid "$identity_id"
    grep -Eq '"schema_version"[[:space:]]*:[[:space:]]*2([,}])' "$existing_identity" \
      && grep -Eq '"enrollment_state"[[:space:]]*:[[:space:]]*"(pending|confirmed)"' "$existing_identity" \
      || fail 'existing identity schema or enrollment state is invalid'
  fi
  if [ -e "$existing_config" ] || [ -L "$existing_config" ]; then
    [ -f "$existing_config" ] && [ ! -L "$existing_config" ] \
      || fail "existing configuration is not a regular file: $existing_config"
    config_id=$(read_config_agent_id "$existing_config")
    require_uuid "$config_id"
  fi
  if [ -n "$identity_id" ] && [ -n "$config_id" ] && [ "$identity_id" != "$config_id" ]; then
    fail 'existing Agent identity and configuration refer to different nodes'
  fi
  existing_id=${identity_id:-$config_id}
  if [ -n "$existing_id" ] && [ "$existing_id" != "$requested_id" ]; then
    fail 'the install command belongs to a different Agent node'
  fi

  if [ -e "$existing_binary" ] || [ -L "$existing_binary" ]; then
    [ -f "$existing_binary" ] && [ -x "$existing_binary" ] \
      || fail "existing Agent binary is not a regular executable: $existing_binary"
    existing_version=$($existing_binary version)
    printf '%s\n' "$existing_version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$' \
      || fail 'existing Agent reported an invalid release version'
    if version_is_newer "$existing_version" "$AGENT_RELEASE_VERSION"; then
      fail "refusing to downgrade Agent from $existing_version to $AGENT_RELEASE_VERSION"
    fi
  fi

  if [ -x "$existing_binary" ] && [ -f "$existing_config" ]; then
    maintenance_safe_check "$existing_binary" "$existing_config"
  fi
  if [ -f "$existing_identity" ]; then
    preserved_identity=$existing_identity
  fi
}

install_packages() {
  [ "$1" = 'runner' ] || return 0
  missing_runner_command=false
  for command in $RUNNER_COMMANDS; do
    command -v "$command" >/dev/null 2>&1 || missing_runner_command=true
  done
  [ "$missing_runner_command" = true ] || return 0
  export DEBIAN_FRONTEND=noninteractive
  apt-get update
  # shellcheck disable=SC2086
  apt-get install -y --no-install-recommends $RUNNER_PACKAGES
  for command in $RUNNER_COMMANDS; do
    command -v "$command" >/dev/null 2>&1 \
      || fail "Runner dependency command is unavailable after package installation: $command"
  done
}

download_binary() {
  installed_binary="$RELEASE_ROOT/releases/$AGENT_RELEASE_VERSION/akastr-agent"
  if [ -f "$installed_binary" ] && [ ! -L "$installed_binary" ] && [ -x "$installed_binary" ]; then
    installed_binary_sha=$(sha256sum "$installed_binary" | awk '{print $1}')
    if [ "$installed_binary_sha" = "$BINARY_SHA256" ]; then
      binary_path=$installed_binary
      return 0
    fi
  fi
  binary_path="$temporary/$ASSET"
  download_https "$RELEASE_BASE_URL/$AGENT_RELEASE_VERSION/$ASSET" "$binary_path" 'Agent binary'
  actual_sha256=$(sha256sum "$binary_path" | awk '{print $1}')
  [ "$actual_sha256" = "$BINARY_SHA256" ] || fail 'Agent binary integrity check failed'
  chmod 0755 "$binary_path"
}

prepare_ipquality() {
  [ "$install_mode" = 'runner' ] || return 0
  installed_ipquality="$RELEASE_ROOT/ipquality/ip.sh"
  if [ -f "$installed_ipquality" ] && [ ! -L "$installed_ipquality" ]; then
    installed_ipquality_sha=$(sha256sum "$installed_ipquality" | awk '{print $1}')
    if [ "$installed_ipquality_sha" = "$IPQUALITY_SHA256" ]; then
      reuse_ipquality=true
      return 0
    fi
  fi
  ipquality_path="$temporary/ip.sh"
  download_https \
    "https://raw.githubusercontent.com/xykt/IPQuality/$IPQUALITY_COMMIT/ip.sh" \
    "$ipquality_path" \
    'IPQuality script'
  ipquality_actual=$(sha256sum "$ipquality_path" | awk '{print $1}')
  [ "$ipquality_actual" = "$IPQUALITY_SHA256" ] || fail 'IPQuality script integrity check failed'
  chmod 0755 "$ipquality_path"
}

maintenance_safe_check() {
  maintenance_binary=$1
  maintenance_config=$2
  [ -f "$maintenance_binary" ] && [ -x "$maintenance_binary" ] \
    || fail "maintenance check binary is not a regular executable: $maintenance_binary"
  [ -f "$maintenance_config" ] && [ ! -L "$maintenance_config" ] \
    || fail "maintenance check config is not a regular file: $maintenance_config"
  "$maintenance_binary" check-idle --config "$maintenance_config"
}

write_service_unit() {
  service_temporary="$temporary/akastr-agent.service"
  cat > "$service_temporary" <<'UNIT'
[Unit]
Description=Akastr Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=notify
NotifyAccess=main
ExecStart=/usr/local/lib/akastr-agent/current/akastr-agent run --config /usr/local/lib/akastr-agent/current/config/config.json
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
  install -m 0644 "$service_temporary" "$SERVICE_FILE"
  systemctl daemon-reload
}

prepare_revision_configuration() {
  source_dir=$1
  install_mode=$2
  configuration_revision=$(read_config_revision "$source_dir/config.json")
  [ -n "$configuration_revision" ] || fail 'bootstrap configuration revision is invalid'
  configuration_dir="$CONFIGURATION_ROOT/$configuration_revision"

  install -d -m 0700 "$STATE_DIR" "$CONFIGURATION_ROOT"
  [ -d "$CONFIGURATION_ROOT" ] && [ ! -L "$CONFIGURATION_ROOT" ] \
    || fail 'managed configuration root is unsafe'
  configuration_staging="$CONFIGURATION_ROOT/.configuration.$$"
  [ ! -e "$configuration_staging" ] && [ ! -L "$configuration_staging" ] \
    || fail 'managed configuration staging path already exists'
  install -d -m 0700 "$configuration_staging"
  install -m 0600 "$source_dir/config.json" "$configuration_staging/config.json"
  install -m 0600 "$source_dir/.bootstrap-sha256" "$configuration_staging/.bootstrap-sha256"
  expected_files=2
  if [ -f "$source_dir/changeip-curl.conf" ]; then
    install -m 0600 "$source_dir/changeip-curl.conf" "$configuration_staging/changeip-curl.conf"
    expected_files=$((expected_files + 1))
  fi
  if [ "$install_mode" = runner ]; then
    install -m 0600 "$source_dir/proxy-profiles.json" "$configuration_staging/proxy-profiles.json"
    expected_files=$((expected_files + 1))
  fi

  if [ -e "$configuration_dir" ] || [ -L "$configuration_dir" ]; then
    [ -d "$configuration_dir" ] && [ ! -L "$configuration_dir" ] \
      || fail 'existing managed configuration revision is unsafe'
    actual_files=$(find "$configuration_dir" -mindepth 1 -maxdepth 1 -print | wc -l | tr -d '[:space:]')
    [ "$actual_files" -eq "$expected_files" ] \
      || fail 'existing managed configuration revision has unexpected files'
    for name in config.json .bootstrap-sha256 changeip-curl.conf proxy-profiles.json; do
      if [ -f "$configuration_staging/$name" ]; then
        [ -f "$configuration_dir/$name" ] && [ ! -L "$configuration_dir/$name" ] \
          && cmp -s "$configuration_staging/$name" "$configuration_dir/$name" \
          || fail 'existing managed configuration revision differs from the desired bootstrap'
      fi
    done
    rm -rf -- "$configuration_staging"
  else
    sync
    mv -- "$configuration_staging" "$configuration_dir"
    sync
  fi
  configuration_staging=''
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
  inspect_existing_install "$agent_id"
  if [ -n "$preserved_identity" ]; then
    install -m 0600 "$preserved_identity" "$temporary/identity.json"
    preserved_identity="$temporary/identity.json"
  fi
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
      --configuration-root "$CONFIGURATION_ROOT" \
      --output-dir "$bootstrap_dir"
  )
  case "$bootstrap_result" in
    bootstrap_mode=target) install_mode=target ;;
    bootstrap_mode=runner) install_mode=runner ;;
    *) fail 'the control plane returned an unsupported installation mode' ;;
  esac

  install_packages "$install_mode"
  prepare_ipquality
  prepare_revision_configuration "$bootstrap_dir" "$install_mode"

  maintenance_safe_check "$binary_path" "$configuration_dir/config.json"
  stop_agent_service
  maintenance_safe_check "$binary_path" "$configuration_dir/config.json"

  release_dir="$RELEASE_ROOT/releases/$AGENT_RELEASE_VERSION"
  install -d -m 0700 "$CONFIG_DIR" "$STATE_DIR"
  install -d -m 0755 "$release_dir"
  install -m 0755 "$binary_path" "$release_dir/.akastr-agent.$$"
  mv -f -- "$release_dir/.akastr-agent.$$" "$release_dir/akastr-agent"
  install -m 0600 "$bootstrap_dir/machine-token" "$CONFIG_DIR/machine-token"
  machine_token_installed=true
  if [ -n "$preserved_identity" ]; then
    install -m 0600 "$preserved_identity" "$CONFIG_DIR/identity.json"
  fi
  if [ "$install_mode" = 'runner' ]; then
    if [ "$reuse_ipquality" != true ]; then
      install -d -m 0755 "$RELEASE_ROOT/ipquality"
      install -m 0755 "$ipquality_path" "$RELEASE_ROOT/ipquality/ip.sh"
    fi
  fi
  deployments_root="$RELEASE_ROOT/deployments"
  deployment_dir="$deployments_root/$AGENT_RELEASE_VERSION-r$configuration_revision"
  install -d -m 0755 "$deployments_root" "$deployment_dir"
  rm -f -- "$deployment_dir/akastr-agent" "$deployment_dir/config"
  ln -s "$release_dir/akastr-agent" "$deployment_dir/akastr-agent"
  ln -s "$configuration_dir" "$deployment_dir/config"
  current_temporary="$RELEASE_ROOT/.current.$$"
  rm -f -- "$current_temporary"
  ln -s "$deployment_dir" "$current_temporary"
  mv -Tf -- "$current_temporary" "$RELEASE_ROOT/current"

  "$RELEASE_ROOT/current/akastr-agent" check-config --config "$RELEASE_ROOT/current/config/config.json"
  write_service_unit
  if "$RELEASE_ROOT/current/akastr-agent" enroll --config "$RELEASE_ROOT/current/config/config.json"; then
    :
  else
    enrollment_code=$?
    case "$enrollment_code" in
      21)
        fail 'enrollment outcome is uncertain; rerun the one-click install command'
        ;;
      20)
        fail 'enrollment was rejected; fix the control-plane state and rerun the same install command'
        ;;
      *)
        fail 'enrollment failed; rerun the same install command'
        ;;
    esac
  fi
  rm -f -- "$CONFIG_DIR/machine-token" "$token_file"
  machine_token_installed=false

  systemctl enable --now akastr-agent.service
  systemctl is-active --quiet akastr-agent.service \
    || fail 'the Agent service did not reach control-plane readiness'

  rm -f -- \
    "$CONFIG_DIR/config.json" \
    "$CONFIG_DIR/changeip-curl.conf" \
    "$CONFIG_DIR/proxy-profiles.json"

  installation_complete=true
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

  maintenance_binary="$RELEASE_ROOT/current/akastr-agent"
  maintenance_config="$RELEASE_ROOT/current/config/config.json"
  complete_runtime=false
  if [ -f "$maintenance_binary" ] && [ -x "$maintenance_binary" ] \
      && [ -f "$maintenance_config" ]; then
    complete_runtime=true
    maintenance_safe_check "$maintenance_binary" "$maintenance_config"
  fi
  stop_agent_service
  if [ "$complete_runtime" = true ]; then
    maintenance_safe_check "$maintenance_binary" "$maintenance_config"
  fi
  rm -f -- "$SERVICE_FILE"
  systemctl daemon-reload
  rm -rf -- "$CONFIG_DIR" "$STATE_DIR" "$RELEASE_ROOT"
  installation_complete=true
  say 'Akastr Agent uninstalled successfully.'
}

operation=${1:-}
case "$operation" in
  --install)
    [ "$#" -eq 1 ] || fail '--install accepts no additional arguments'
    preflight_install
    fresh_install
    ;;
  --status)
    [ "$#" -eq 1 ] || fail '--status accepts no additional arguments'
    preflight_status
    show_status
    ;;
  --uninstall)
    shift
    preflight_uninstall
    uninstall_existing "$@"
    ;;
  *)
    fail 'usage: install.sh --install | --status | --uninstall --confirm-destroy-local-agent'
    ;;
esac
