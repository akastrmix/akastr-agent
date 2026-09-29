#!/bin/sh
set -eu

pins=internal/modules/ipqualityrunner/script/pin.go
runner_packages=$(sed -n 's/^[[:space:]]*RunnerPackages[[:space:]]*= "\([^"]*\)"$/\1/p' "$pins")
runner_commands=$(sed -n 's/^[[:space:]]*RunnerCommands[[:space:]]*= "\([^"]*\)"$/\1/p' "$pins")
[ -n "$runner_packages" ] && [ -n "$runner_commands" ] || {
  echo 'Runner dependency lists are missing or invalid' >&2
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
