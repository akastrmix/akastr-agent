#!/bin/sh
set -eu
# Keep the 2-core development VPS responsive; callers can explicitly override.
: "${GOMAXPROCS:=1}"
: "${GOMEMLIMIT:=256MiB}"
export GOMAXPROCS GOMEMLIMIT
repository=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
# Keep cancellation stress checks before the full suite. Timeouts expose leaks.
go -C "$repository" test -p "$GOMAXPROCS" -count=100 -timeout=2m ./internal/autoupdate
go -C "$repository" test -p "$GOMAXPROCS" -count=20 -timeout=2m ./internal/features/ipwatch
go -C "$repository" test -p "$GOMAXPROCS" -count=1 -timeout=2m ./...
go -C "$repository" vet -p "$GOMAXPROCS" ./...
go -C "$repository" build -p "$GOMAXPROCS" ./cmd/akastr-agent
