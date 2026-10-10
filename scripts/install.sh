#!/bin/sh
# Akastr Agent installer. Downloads the approved release binary, verifies it
# and hands over to `akastr-agent install`; all install logic lives in Go.
set -eu

AGENT_RELEASE_VERSION='@AKASTR_AGENT_VERSION@'
BINARY_SHA256='@AKASTR_AGENT_BINARY_SHA256@'
RELEASE_BASE_URL=${AKASTR_RELEASE_BASE_URL:-https://github.com/akastrmix/akastr-agent/releases/download}
SERVICE_FILE=/etc/systemd/system/akastr-agent.service

fail() { printf 'Error: %s\n' "$*" >&2; exit 1; }

require_root() {
  [ "$(id -u)" -eq 0 ] || fail 'run the installer as root'
  command -v systemctl >/dev/null 2>&1 || fail 'systemd is required'
}

install_agent() {
  require_root
  case "$(uname -m)" in
    x86_64|amd64) ;;
    *) fail 'only x86_64 / amd64 is supported' ;;
  esac
  [ -r /etc/os-release ] || fail 'cannot identify the operating system'
  os_identity=$(
    # shellcheck disable=SC1091
    . /etc/os-release
    printf '%s:%s' "${ID:-}" "${VERSION_ID:-}"
  )
  case "$os_identity" in
    debian:12|debian:13) ;;
    *) fail 'only Debian 12 and Debian 13 are supported' ;;
  esac
  command -v curl >/dev/null 2>&1 || fail 'curl is required'
  command -v sha256sum >/dev/null 2>&1 || fail 'sha256sum is required'

  temporary=$(mktemp -d)
  trap 'rm -rf -- "$temporary"' EXIT
  trap 'exit 1' HUP INT TERM
  binary="$temporary/akastr-agent"
  # A slow link may take as long as it needs; only a minute without data ends
  # an attempt, and each attempt continues from the bytes already received
  # (curl's own --retry starts over instead).
  attempt=1
  until curl --fail --show-error --silent --location \
    --proto '=https' --proto-redir '=https' \
    --connect-timeout 30 --speed-limit 1 --speed-time 60 --continue-at - \
    --output "$binary" "$RELEASE_BASE_URL/$AGENT_RELEASE_VERSION/akastr-agent-linux-amd64"; do
    [ "$attempt" -lt 10 ] || fail 'Agent binary download failed'
    attempt=$((attempt + 1))
    sleep 5
  done
  [ "$(sha256sum "$binary" | awk '{print $1}')" = "$BINARY_SHA256" ] \
    || fail 'Agent binary integrity check failed'
  chmod 0755 "$binary"
  "$binary" install || fail 'installation is incomplete; fix the reported error and rerun the same command'
}

uninstall_agent() {
  [ "${1:-}" = '--confirm-destroy-local-agent' ] && [ "$#" -eq 1 ] \
    || fail 'uninstall requires --confirm-destroy-local-agent'
  require_root
  if [ -e "$SERVICE_FILE" ]; then
    systemctl disable --now akastr-agent.service >/dev/null \
      || fail 'could not stop akastr-agent.service; nothing was removed'
  fi
  # A running Agent must never lose its identity and execution records.
  ! systemctl is-active --quiet akastr-agent.service \
    || fail 'akastr-agent.service is still running; nothing was removed'
  if [ -e "$SERVICE_FILE" ]; then
    rm -f -- "$SERVICE_FILE"
    systemctl daemon-reload
  fi
  rm -rf -- /etc/akastr-agent /var/lib/akastr-agent /usr/local/lib/akastr-agent
  printf 'Akastr Agent uninstalled successfully.\n'
}

case "${1:-}" in
  --status)
    [ "$#" -eq 1 ] || fail '--status accepts no additional arguments'
    systemctl --no-pager --full status akastr-agent.service
    ;;
  --uninstall)
    shift
    uninstall_agent "$@"
    ;;
  *.*)
    [ "$#" -eq 1 ] || fail 'pass exactly one install code'
    AKASTR_AGENT_ID=${1%%.*}
    AKASTR_AGENT_MACHINE_TOKEN=${1#*.}
    export AKASTR_AGENT_ID AKASTR_AGENT_MACHINE_TOKEN
    set --
    install_agent
    ;;
  *)
    fail 'usage: install.sh <node-id.machine-token> | --status | --uninstall --confirm-destroy-local-agent'
    ;;
esac
