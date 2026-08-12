#!/bin/sh
set -eu

[ "$#" -ge 1 ] && [ "$#" -le 2 ] || {
  echo "usage: update.sh VERSION [RELEASE_BASE_URL]" >&2
  exit 2
}
version=$1
release_base=${2:-"https://github.com/akastrmix/akastr-agent/releases/download"}
printf '%s\n' "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$' || {
  echo "invalid release version" >&2
  exit 2
}
[ "$(id -u)" -eq 0 ] || { echo "updater must run as root" >&2; exit 1; }
[ -f /etc/akastr-agent/config.json ] || { echo "Akastr Agent is not installed" >&2; exit 1; }

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
install -d -m 0755 "$release_dir"
install -m 0755 "$temporary/$asset" "$release_dir/akastr-agent"
"$release_dir/akastr-agent" check-config --config /etc/akastr-agent/config.json
previous=$(readlink -f /usr/local/lib/akastr-agent/current)
ln -sfn "$release_dir" /usr/local/lib/akastr-agent/current
if ! systemctl restart akastr-agent.service || ! systemctl is-active --quiet akastr-agent.service; then
  ln -sfn "$previous" /usr/local/lib/akastr-agent/current
  systemctl restart akastr-agent.service
  echo "update failed; previous release restored" >&2
  exit 1
fi
echo "Akastr Agent updated to $version"
