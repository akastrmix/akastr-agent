#!/bin/sh
set -eu

commit=$(sed -n "s/^IPQUALITY_COMMIT='\([0-9a-f]\{40\}\)'$/\1/p" scripts/install.sh)
expected=$(sed -n "s/^IPQUALITY_SHA256='\([0-9a-f]\{64\}\)'$/\1/p" scripts/install.sh)
[ -n "$commit" ] && [ -n "$expected" ] || {
  echo 'IPQuality source pin is missing or invalid' >&2
  exit 1
}

temporary=$(mktemp)
trap 'rm -f -- "$temporary"' EXIT HUP INT TERM
wget --no-hsts --https-only --tries=3 --timeout=30 -qO "$temporary" \
  "https://raw.githubusercontent.com/xykt/IPQuality/$commit/ip.sh"
actual=$(sha256sum "$temporary" | awk '{print $1}')
[ "$actual" = "$expected" ] || {
  echo "IPQuality source checksum mismatch: expected $expected, got $actual" >&2
  exit 1
}

echo "ipquality_source_ok commit=$commit sha256=$actual"
