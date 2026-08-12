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

for architecture in amd64 arm64; do
  asset="akastr-agent-linux-$architecture"
  CGO_ENABLED=0 GOOS=linux GOARCH=$architecture \
    go build -trimpath -ldflags "-s -w -X main.version=$version" \
    -o "$output/$asset" ./cmd/akastr-agent
  (
    cd "$output"
    sha256sum "$asset" > "$asset.sha256"
  )
done
echo "release assets created in $output"
