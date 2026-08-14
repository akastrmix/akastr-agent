#!/bin/sh
set -eu

[ "$#" -eq 2 ] || {
  echo "usage: build-release.sh VERSION OUTPUT_DIRECTORY" >&2
  exit 2
}
version=$1
output=$2
printf '%s\n' "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$' || {
  echo "invalid release version" >&2
  exit 2
}
[ ! -e "$output" ] || { echo "output directory already exists" >&2; exit 1; }
mkdir -m 0755 "$output"

asset="akastr-agent-linux-amd64"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags "-s -w -X main.version=$version" \
  -o "$output/$asset" ./cmd/akastr-agent
[ -s "$output/$asset" ] || {
  echo "amd64 release binary was not created" >&2
  exit 1
}

binary_sha256=$(sha256sum "$output/$asset" | awk '{print $1}')
printf '%s\n' "$binary_sha256" | grep -Eq '^[0-9a-f]{64}$' || {
  echo "invalid amd64 release binary checksum" >&2
  exit 1
}
sed \
  -e "s|@AKASTR_AGENT_VERSION@|$version|g" \
  -e "s|@AKASTR_AGENT_BINARY_SHA256@|$binary_sha256|g" \
  scripts/install.sh > "$output/install.sh"
chmod 0755 "$output/install.sh"

echo "release assets created in $output"
