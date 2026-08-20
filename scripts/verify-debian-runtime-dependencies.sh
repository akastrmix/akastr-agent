#!/bin/sh
set -eu

installer=${1:-scripts/install.sh}
[ -f "$installer" ] || {
  echo "installer not found: $installer" >&2
  exit 1
}

runner_packages=$(sed -n "s/^RUNNER_PACKAGES='\([^']*\)'$/\1/p" "$installer")
runner_commands=$(sed -n "s/^RUNNER_COMMANDS='\([^']*\)'$/\1/p" "$installer")
[ -n "$runner_packages" ] && [ -n "$runner_commands" ] || {
  echo 'installer runtime dependency contract is missing or invalid' >&2
  exit 1
}

# shellcheck disable=SC1091
. /etc/os-release
case "${ID:-}:${VERSION_ID:-}" in
  debian:12|debian:13) ;;
  *)
    echo 'runtime dependency verification requires Debian 12 or Debian 13' >&2
    exit 1
    ;;
esac

export DEBIAN_FRONTEND=noninteractive
apt-get update
# shellcheck disable=SC2086
apt-get install -y --no-install-recommends ca-certificates curl $runner_packages
for command in $runner_commands; do
  command -v "$command" >/dev/null 2>&1 || {
    echo "runtime dependency command is unavailable: $command" >&2
    exit 1
  }
done

echo "debian_runtime_dependencies_ok version=$VERSION_ID commands=$(printf '%s' "$runner_commands" | tr ' ' ',')"
