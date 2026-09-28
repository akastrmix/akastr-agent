#!/bin/sh
set -eu
# Keep the 2-core development VPS responsive; callers can explicitly override.
: "${GOMAXPROCS:=1}"
: "${GOMEMLIMIT:=256MiB}"
export GOMAXPROCS GOMEMLIMIT
repository=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
# Repeat the concurrency-heavy packages; timeouts expose leaked goroutines.
go -C "$repository" test -p "$GOMAXPROCS" -count=20 -timeout=2m ./internal/features/ipwatch ./internal/daemon
go -C "$repository" test -p "$GOMAXPROCS" -count=1 -timeout=2m ./...
go -C "$repository" vet -p "$GOMAXPROCS" ./...
go -C "$repository" build -p "$GOMAXPROCS" -o /dev/null ./cmd/akastr-agent
