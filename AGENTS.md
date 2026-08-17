# Akastr Agent — Agent Notes

## Product boundary

- Akastr Agent is a new project. Do not copy the old IPChanger runtime or preserve its HTTP contract as a compatibility layer.
- AkastrCloud remains the control plane and the only business source of truth. Telegram, eligibility, daily IPQuality policy, queues, cache metadata, and user delivery do not belong in this repository.
- The retired IPChanger repository is archived history. Never use it as a deployment, recovery, or compatibility source.
- The runtime is a single Go process managed by systemd. Prefer the Go standard library and add dependencies only when they have a concrete runtime purpose.
- The Agent only executes typed, preconfigured operations. Never add a remote shell, arbitrary command payload, terminal, generic host monitoring, generic offline alerting, or Telegram channel delivery.

## Current and reserved capabilities

First release capabilities:

- Observe public IPv4 and optional IPv6.
- Execute a preconfigured ChangeIP command provider.
- Describe a configured SOCKS5 endpoint without exposing credentials.
- Execute the pinned IPQuality script when this installation is configured as a Runner.

Reserved extension points, not first-release implementations:

- ChangeIP HTTP-flow provider.
- Xray traffic/log observation.
- Long-running bandwidth policy and rate limiting.

Add future features as sibling packages. Do not create empty implementations, placeholder protocol fields, or speculative interfaces.

## Security and contracts

- Secret material is stored only in root-only files outside Git. Never place credentials in capability manifests, operation journals, logs, tests, or example configuration.
- Network control is outbound WSS. Authentication and message fields must be approved together with the AkastrCloud side before implementation.
- Any change to a production schema, durable queue/payload, authentication, secret flow, or rollout boundary requires the AkastrCloud ADR 0024 proposal and explicit operator approval before implementation.
- Agent-local state contains only bounded operational metadata. A corrupt or unknown state schema fails closed; do not silently reset it.
- ChangeIP and IPQuality are logically mutually exclusive for the same target node. Runner concurrency is an additional independent resource constraint.

## Development

- Keep packages small and aligned with `docs/ARCHITECTURE.md`.
- Use fixed argument vectors; do not invoke `/bin/sh -c` for configured providers.
- Validate configuration strictly and reject unknown fields.
- Add tests for every state transition, conflict rule, and parsing change.
- Run `go test ./...`, `go vet ./...`, and `go build ./cmd/akastr-agent` before delivery.
- Update README and the authoritative design document when behavior or boundaries change.
