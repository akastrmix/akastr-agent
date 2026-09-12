#!/bin/sh
set -eu

[ "${AKASTR_INSTALLER_CONTAINER_TEST:-}" = '1' ] && [ -e /.dockerenv ] || {
  echo 'this test may run only in an explicitly marked disposable Docker container' >&2
  exit 2
}
[ "$(id -u)" -eq 0 ] || {
  echo 'installer container test requires container root' >&2
  exit 2
}

case "$(. /etc/os-release; printf '%s:%s' "${ID:-}" "${VERSION_ID:-}")" in
  debian:12|debian:13) ;;
  *) echo 'installer container test requires Debian 12 or 13' >&2; exit 2 ;;
esac

source_installer=${1:-/source/scripts/install.sh}
[ -f "$source_installer" ] || {
  echo "installer template is missing: $source_installer" >&2
  exit 2
}

for path in /etc/akastr-agent /var/lib/akastr-agent /usr/local/lib/akastr-agent /etc/systemd/system/akastr-agent.service; do
  [ ! -e "$path" ] && [ ! -L "$path" ] || {
    echo "container is not clean: $path" >&2
    exit 2
  }
done

test_root=$(mktemp -d /tmp/akastr-installer-test.XXXXXX)
fake_bin="$test_root/bin"
mkdir -p "$fake_bin" /etc/systemd/system

cleanup() {
  trap - EXIT HUP INT TERM
  rm -rf -- /etc/akastr-agent /var/lib/akastr-agent /usr/local/lib/akastr-agent "$test_root"
  rm -f -- /etc/systemd/system/akastr-agent.service
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

export AKASTR_TEST_ROOT=$test_root
export PATH="$fake_bin:/usr/sbin:/usr/bin:/sbin:/bin"

cat > "$test_root/fake-agent" <<'AGENT'
#!/bin/sh
set -eu
case "${1:-}" in
  version)
    echo 'v9.9.9'
    ;;
  bootstrap)
    shift
    output=''
    agent_id=''
    endpoint=''
    token_file=''
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --output-dir) output=$2; shift 2 ;;
        --agent-id) agent_id=$2; shift 2 ;;
        --endpoint) endpoint=$2; shift 2 ;;
        --token-file) token_file=$2; shift 2 ;;
        --configuration-root) shift 2 ;;
        *) echo "unexpected bootstrap argument: $1" >&2; exit 2 ;;
      esac
    done
    [ -n "$output" ] || exit 2
    [ "$agent_id" = "$AKASTR_TEST_AGENT_ID" ] || exit 2
    [ "$endpoint" = "$AKASTR_TEST_BOOTSTRAP_ENDPOINT" ] || exit 2
    [ "$(cat "$token_file")" = "$AKASTR_TEST_MACHINE_TOKEN" ] || exit 2
    revision=2
    [ "${AKASTR_TEST_MODE:-target}" != runner ] || revision=3
    mkdir -p "$output"
    cat > "$output/config.json" <<EOF
{"schema_version":3,"configuration_revision":$revision,"node":{"id":"$agent_id","name":"fixture"},"control":{}}
EOF
    printf '%s\n' 'fixture-machine-token' > "$output/machine-token"
    printf '%064d\n' 0 > "$output/.bootstrap-sha256"
    if [ "${AKASTR_TEST_MODE:-target}" = runner ]; then
      printf '%s\n' '{}' > "$output/proxy-profiles.json"
      echo 'bootstrap_mode=runner'
    else
      echo 'bootstrap_mode=target'
    fi
    ;;
  check-idle|check-config)
    if [ "$1" = check-idle ] && [ -e "$AKASTR_TEST_ROOT/idle-fail-once" ]; then
      rm -- "$AKASTR_TEST_ROOT/idle-fail-once"
      exit 20
    fi
    if [ "$1" = check-idle ] && [ -e "$AKASTR_TEST_ROOT/idle-kill-once" ]; then
      rm -- "$AKASTR_TEST_ROOT/idle-kill-once"
      kill -KILL "$PPID"
      exit 20
    fi
    ;;
  enroll)
    if [ -e "$AKASTR_TEST_ROOT/enroll-fail-once" ]; then
      rm -f -- "$AKASTR_TEST_ROOT/enroll-fail-once"
      exit 20
    fi
    cat > /etc/akastr-agent/identity.json <<EOF
{
  "schema_version": 2,
  "enrollment_state": "confirmed",
  "agent_id": "$AKASTR_TEST_AGENT_ID",
  "public_key": "fixture-public",
  "private_key": "fixture-private"
}
EOF
    chmod 0600 /etc/akastr-agent/identity.json
    ;;
  *)
    echo "unexpected fake Agent operation: ${1:-}" >&2
    exit 2
    ;;
esac
AGENT
chmod 0755 "$test_root/fake-agent"

cat > "$test_root/fake-ipquality" <<'IPQUALITY'
#!/bin/sh
exit 0
IPQUALITY
chmod 0755 "$test_root/fake-ipquality"

cat > "$fake_bin/curl" <<'CURL'
#!/bin/sh
set -eu
destination=''
url=''
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output|-o) destination=$2; shift 2 ;;
    https://*) url=$1; shift ;;
    *) shift ;;
  esac
done
[ -n "$destination" ] && [ -n "$url" ] || exit 2
printf '%s\n' "$url" >> "$AKASTR_TEST_ROOT/curl.log"
case "$url" in
  */akastr-agent-linux-amd64) cp "$AKASTR_TEST_ROOT/fake-agent" "$destination" ;;
  */ip.sh) cp "$AKASTR_TEST_ROOT/fake-ipquality" "$destination" ;;
  *) echo "unexpected fixture URL: $url" >&2; exit 2 ;;
esac
CURL
chmod 0755 "$fake_bin/curl"

cat > "$fake_bin/apt-get" <<'APT'
#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$AKASTR_TEST_ROOT/apt.log"
if [ "${1:-}" = install ]; then
  for command in bc dig ip jq nc; do
    cat > "$AKASTR_TEST_ROOT/bin/$command" <<'COMMAND'
#!/bin/sh
exit 0
COMMAND
    chmod 0755 "$AKASTR_TEST_ROOT/bin/$command"
  done
fi
APT
chmod 0755 "$fake_bin/apt-get"

cat > "$fake_bin/systemctl" <<'SYSTEMCTL'
#!/bin/sh
set -eu
operation=${1:-}
shift || true
state=unknown
[ ! -f "$AKASTR_TEST_ROOT/systemd-state" ] || state=$(cat "$AKASTR_TEST_ROOT/systemd-state")
case "$operation" in
  is-enabled)
    [ -e "$AKASTR_TEST_ROOT/systemd-enabled" ]
    ;;
  disable)
    rm -f -- "$AKASTR_TEST_ROOT/systemd-enabled"
    ;;
  is-active)
    printf '%s\n' "$state"
    [ "$state" = active ] && exit 0
    exit 3
    ;;
  stop)
    echo inactive > "$AKASTR_TEST_ROOT/systemd-state"
    ;;
  is-failed)
    [ "$state" = failed ]
    ;;
  reset-failed)
    echo inactive > "$AKASTR_TEST_ROOT/systemd-state"
    echo reset-failed >> "$AKASTR_TEST_ROOT/systemctl.log"
    ;;
  daemon-reload)
    ;;
  enable)
    : > "$AKASTR_TEST_ROOT/systemd-enabled"
    echo active > "$AKASTR_TEST_ROOT/systemd-state"
    ;;
  status)
    printf 'fixture service state: %s\n' "$state"
    ;;
  *)
    echo "unexpected systemctl operation: $operation" >&2
    exit 2
    ;;
esac
SYSTEMCTL
chmod 0755 "$fake_bin/systemctl"

binary_sha=$(sha256sum "$test_root/fake-agent" | awk '{print $1}')
ipquality_sha=$(sha256sum "$test_root/fake-ipquality" | awk '{print $1}')
installer="$test_root/install.sh"
sed \
  -e "s|@AKASTR_AGENT_VERSION@|v9.9.9|g" \
  -e "s|@AKASTR_AGENT_BINARY_SHA256@|$binary_sha|g" \
  -e "s|^IPQUALITY_SHA256='[^']*'|IPQUALITY_SHA256='$ipquality_sha'|" \
  "$source_installer" > "$installer"
chmod 0755 "$installer"

agent_id='123e4567-e89b-42d3-a456-426614174000'
machine_token='aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
[ "${#machine_token}" -eq 43 ] || exit 2
export AKASTR_TEST_AGENT_ID=$agent_id
export AKASTR_TEST_MACHINE_TOKEN=$machine_token
unset AKASTR_AGENT_ID AKASTR_AGENT_MACHINE_TOKEN AKASTR_AGENT_BOOTSTRAP_ENDPOINT

run_install() {
  if [ "${2:-code}" = legacy ]; then
    cat "$installer" | AKASTR_TEST_MODE=$1 \
      AKASTR_AGENT_ID=$agent_id AKASTR_AGENT_MACHINE_TOKEN=$machine_token \
      AKASTR_AGENT_BOOTSTRAP_ENDPOINT='https://control.invalid/bootstrap' \
      AKASTR_TEST_BOOTSTRAP_ENDPOINT='https://control.invalid/bootstrap' sh -s -- --install
  else
    cat "$installer" | AKASTR_TEST_MODE=$1 \
      AKASTR_TEST_BOOTSTRAP_ENDPOINT='https://origin.akastrmix.com/internal/agents/bootstrap' \
      sh -s -- "$agent_id.$machine_token"
  fi
}

assert_count() {
  expected=$1
  pattern=$2
  file=$3
  actual=0
  if [ -f "$file" ]; then
    actual=$(grep -c "$pattern" "$file" || true)
  fi
  [ "$actual" -eq "$expected" ] || {
    echo "expected $expected matches for $pattern in $file, got $actual" >&2
    exit 1
  }
}

for invalid_code in '' "$agent_id" ".$machine_token" "$agent_id." "$agent_id.short" "$agent_id.$machine_token.extra"; do
  if invalid_output=$(cat "$installer" | sh -s -- "$invalid_code" 2>&1); then
    echo 'invalid install code unexpectedly succeeded' >&2
    exit 1
  fi
  if printf '%s\n' "$invalid_output" | grep -Fq "$machine_token"; then
    echo 'installer error exposed the machine token' >&2
    exit 1
  fi
done
if cat "$installer" | sh -s -- "$agent_id.$machine_token" extra >/dev/null 2>&1; then
  echo 'extra install argument unexpectedly succeeded' >&2
  exit 1
fi
[ ! -e /usr/local/lib/akastr-agent ]
[ ! -e "$test_root/curl.log" ]

: > "$test_root/idle-fail-once"
if run_install target >/dev/null 2>&1; then
  echo 'injected first-install failure unexpectedly succeeded' >&2
  exit 1
fi
[ -d /var/lib/akastr-agent/configurations ]
[ ! -e /etc/akastr-agent/identity.json ]
: > "$test_root/idle-kill-once"
if run_install target >/dev/null 2>&1; then
  echo 'interrupted first install unexpectedly succeeded' >&2
  exit 1
fi
# Empty roots and interrupted staging are retryable; execution state is not.
printf '%s\n' 'unowned-operation-state' > /var/lib/akastr-agent/state.json
if unowned_output=$(run_install target 2>&1); then
  echo 'installation with unowned execution state unexpectedly succeeded' >&2
  exit 1
fi
printf '%s\n' "$unowned_output" | grep -Fq 'do not prove node ownership'
grep -Fxq 'unowned-operation-state' /var/lib/akastr-agent/state.json
rm -- /var/lib/akastr-agent/state.json

run_install target >/dev/null
[ "$(cat "$test_root/systemd-state")" = active ]
[ -f /etc/akastr-agent/identity.json ]
[ -f /var/lib/akastr-agent/configurations/2/config.json ]
[ -L /usr/local/lib/akastr-agent/current ]
[ "$(readlink /usr/local/lib/akastr-agent/current/config)" = /var/lib/akastr-agent/configurations/2 ]
assert_count 3 'akastr-agent-linux-amd64' "$test_root/curl.log"

mkdir -p /usr/local/lib/akastr-agent/.install-abandoned /usr/local/lib/akastr-agent/releases/.update-123 /var/lib/akastr-agent/configurations/.configuration.456
cp /etc/akastr-agent/identity.json "$test_root/identity.before-legacy"
run_install target legacy >/dev/null
cmp -s "$test_root/identity.before-legacy" /etc/akastr-agent/identity.json
[ ! -e /usr/local/lib/akastr-agent/.install-abandoned ]
[ ! -e /usr/local/lib/akastr-agent/releases/.update-123 ]
[ ! -e /var/lib/akastr-agent/configurations/.configuration.456 ]
assert_count 3 'akastr-agent-linux-amd64' "$test_root/curl.log"
[ -f /var/lib/akastr-agent/configurations/2/config.json ]

mkdir -p \
  /usr/local/lib/akastr-agent/releases/v9.9.7 \
  /usr/local/lib/akastr-agent/releases/manual \
  /usr/local/lib/akastr-agent/deployments/v9.9.7-r1 \
  /var/lib/akastr-agent/configurations/1
cp "$test_root/fake-agent" /usr/local/lib/akastr-agent/releases/v9.9.7/akastr-agent
cat > /var/lib/akastr-agent/configurations/1/config.json <<EOF
{"schema_version":3,"configuration_revision":1,"node":{"id":"$agent_id","name":"stale"},"control":{}}
EOF
ln -s /usr/local/lib/akastr-agent/releases/v9.9.7/akastr-agent \
  /usr/local/lib/akastr-agent/deployments/v9.9.7-r1/akastr-agent
ln -s /var/lib/akastr-agent/configurations/1 \
  /usr/local/lib/akastr-agent/deployments/v9.9.7-r1/config

cp /var/lib/akastr-agent/configurations/2/config.json "$test_root/config.json.clean"
printf ' ' >> /var/lib/akastr-agent/configurations/2/config.json
run_install target >/dev/null
cmp -s "$test_root/config.json.clean" /var/lib/akastr-agent/configurations/2/config.json
printf '%064d\n' 1 > /var/lib/akastr-agent/configurations/2/.bootstrap-sha256
if mismatch_output=$(run_install target 2>&1); then
  echo 'mismatched immutable configuration revision unexpectedly succeeded' >&2
  exit 1
fi
printf '%s\n' "$mismatch_output" | grep -Fq 'differs from the desired bootstrap'
printf '%064d\n' 0 > /var/lib/akastr-agent/configurations/2/.bootstrap-sha256
[ "$(cat "$test_root/systemd-state")" = active ]

echo failed > "$test_root/systemd-state"
run_install target >/dev/null
assert_count 1 'reset-failed' "$test_root/systemctl.log"

rm -f -- /etc/systemd/system/akastr-agent.service
echo unknown > "$test_root/systemd-state"
run_install target >/dev/null
assert_count 1 'reset-failed' "$test_root/systemctl.log"

: > "$test_root/enroll-fail-once"
if failure_output=$(run_install target 2>&1); then
  echo 'injected enrollment failure unexpectedly succeeded' >&2
  exit 1
fi
printf '%s\n' "$failure_output" | grep -Fq 'rerun the same command'
[ "$(cat "$test_root/systemd-state")" = inactive ]
run_install target >/dev/null
[ "$(cat "$test_root/systemd-state")" = active ]

run_install runner >/dev/null
[ -f /var/lib/akastr-agent/configurations/3/proxy-profiles.json ]
[ -f /var/lib/akastr-agent/configurations/2/config.json ]
[ ! -e /var/lib/akastr-agent/configurations/2/proxy-profiles.json ]
[ "$(readlink /usr/local/lib/akastr-agent/current/config)" = /var/lib/akastr-agent/configurations/3 ]
[ -x /usr/local/lib/akastr-agent/ipquality/ip.sh ]
assert_count 1 '^update$' "$test_root/apt.log"
assert_count 1 '^install ' "$test_root/apt.log"
assert_count 1 '/ip.sh$' "$test_root/curl.log"
[ ! -e /usr/local/lib/akastr-agent/deployments/v9.9.7-r1 ]
[ ! -e /usr/local/lib/akastr-agent/releases/v9.9.7 ]
[ ! -e /var/lib/akastr-agent/configurations/1 ]
[ -d /usr/local/lib/akastr-agent/releases/manual ]

run_install runner >/dev/null
[ "$(readlink /usr/local/lib/akastr-agent/current/previous)" = /usr/local/lib/akastr-agent/deployments/v9.9.9-r2 ]
[ -f /var/lib/akastr-agent/configurations/2/config.json ]
assert_count 1 '^update$' "$test_root/apt.log"
assert_count 1 '^install ' "$test_root/apt.log"
assert_count 1 '/ip.sh$' "$test_root/curl.log"

cp /etc/akastr-agent/identity.json "$test_root/identity.clean"
printf '%s\n' '{"schema_version":1,"active":{},"recent":[]}' > /var/lib/akastr-agent/state.json
rm -- /var/lib/akastr-agent/configurations/3/proxy-profiles.json
run_install runner >/dev/null
[ -f /var/lib/akastr-agent/configurations/3/proxy-profiles.json ]
cmp -s "$test_root/identity.clean" /etc/akastr-agent/identity.json
grep -Fxq '{"schema_version":1,"active":{},"recent":[]}' /var/lib/akastr-agent/state.json
rm -- /var/lib/akastr-agent/state.json

sh "$installer" --uninstall --confirm-destroy-local-agent >/dev/null
[ ! -e /etc/akastr-agent ]
[ ! -e /var/lib/akastr-agent ]
[ ! -e /usr/local/lib/akastr-agent ]
[ ! -e /etc/systemd/system/akastr-agent.service ]

mkdir -p /usr/local/lib/akastr-agent/releases/v9.9.8
printf '%s\n' '[Service]' > /etc/systemd/system/akastr-agent.service
if ownership_output=$(run_install target 2>&1); then
  echo 'installation with unowned Agent artifacts unexpectedly succeeded' >&2
  exit 1
fi
printf '%s\n' "$ownership_output" | grep -Fq 'do not prove node ownership'
rm -rf -- /usr/local/lib/akastr-agent
rm -f -- /etc/systemd/system/akastr-agent.service

printf 'installer_container_integration_ok version=%s\n' "$(. /etc/os-release; printf '%s' "$VERSION_ID")"
