#!/bin/sh
set -eu

usage() {
  echo "usage: install.sh VERSION CONFIG_FILE ENROLLMENT_TOKEN_FILE [RELEASE_BASE_URL]" >&2
  exit 2
}

[ "$#" -ge 3 ] && [ "$#" -le 4 ] || usage
version=$1
config_source=$2
token_source=$3
release_base=${4:-"https://github.com/akastrmix/akastr-agent/releases/download"}

printf '%s\n' "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$' || {
  echo "invalid release version" >&2
  exit 2
}
[ "$(id -u)" -eq 0 ] || { echo "installer must run as root" >&2; exit 1; }
[ -f "$config_source" ] || { echo "configuration file not found" >&2; exit 1; }
[ -f "$token_source" ] || { echo "enrollment token file not found" >&2; exit 1; }

machine=$(uname -m)
case "$machine" in
  x86_64|amd64) architecture=amd64 ;;
  aarch64|arm64) architecture=arm64 ;;
  *) echo "unsupported architecture: $machine" >&2; exit 1 ;;
esac

asset="akastr-agent-linux-$architecture"
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT HUP INT TERM
curl --fail --silent --show-error --location \
  "$release_base/$version/$asset" -o "$temporary/$asset"
curl --fail --silent --show-error --location \
  "$release_base/$version/$asset.sha256" -o "$temporary/$asset.sha256"
expected=$(awk 'NR == 1 {print $1}' "$temporary/$asset.sha256")
case "$expected" in
  *[!0-9a-f]*|'') echo "invalid release checksum" >&2; exit 1 ;;
esac
[ "${#expected}" -eq 64 ] || { echo "invalid release checksum length" >&2; exit 1; }
actual=$(sha256sum "$temporary/$asset" | awk '{print $1}')
[ "$actual" = "$expected" ] || { echo "release checksum mismatch" >&2; exit 1; }

release_dir="/usr/local/lib/akastr-agent/releases/$version"
install -d -m 0755 "$release_dir" /etc/akastr-agent /var/lib/akastr-agent
install -m 0755 "$temporary/$asset" "$release_dir/akastr-agent"
install -m 0600 "$config_source" /etc/akastr-agent/config.json
install -m 0600 "$token_source" /etc/akastr-agent/enrollment-token
ln -sfn "$release_dir" /usr/local/lib/akastr-agent/current

/usr/local/lib/akastr-agent/current/akastr-agent check-config --config /etc/akastr-agent/config.json
/usr/local/lib/akastr-agent/current/akastr-agent enroll --config /etc/akastr-agent/config.json
rm -f /etc/akastr-agent/enrollment-token

cat > /etc/systemd/system/akastr-agent.service <<'UNIT'
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
ProtectSystem=full
ProtectHome=read-only

[Install]
WantedBy=multi-user.target
UNIT
chmod 0644 /etc/systemd/system/akastr-agent.service
systemctl daemon-reload
systemctl enable --now akastr-agent.service
systemctl is-active --quiet akastr-agent.service
echo "Akastr Agent $version installed and running"
