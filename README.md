# Akastr Agent

Akastr Agent is the lightweight, outbound-controlled runtime for AkastrCloud service nodes. It replaces the design of the old `ip-changer` project with a single Go process, typed capabilities, bounded local state, and an agent-initiated WSS control channel.

This repository now contains the approved `2026-08-13.v1` pre-production runtime. It is not connected to AkastrCloud production and must not replace any of the six existing IPChanger installations until a staged node cutover is explicitly performed.

## Ownership boundary

Akastr Agent owns node-local execution and observation:

- public IPv4/IPv6 observation;
- a fixed ChangeIP command provider;
- non-secret SOCKS5 endpoint description;
- pinned IPQuality script execution on a dedicated Runner installation.

AkastrCloud owns orchestration and business policy:

- Telegram commands, private-message delivery, bindings, and eligibility;
- ChangeIP sessions, delayed/preset/automatic scheduling, and durable queues;
- target-node and Runner resource scheduling;
- IPQuality daily limits and cached reports.

IPQuality is limited to one real run per service node per Hong Kong calendar day. Further requests use that day's cache. The daily state resets at `00:00 Asia/Hong_Kong` or immediately after the node's observed IPv4 changes. This rule is enforced centrally so reinstalling an Agent cannot bypass it.

Telegram channel delivery, generic offline alerts, generic host monitoring, arbitrary remote commands, and HTTP-flow ChangeIP are not part of the first release.

## Repository map

```text
cmd/akastr-agent/       CLI entry
docs/                   architecture and approved behavior
internal/app/           application assembly
internal/capability/    non-secret capability registry
internal/config/        strict JSON configuration
internal/operation/     local operation mutex and bounded journal
internal/state/         atomic state file persistence
internal/features/      concrete node observations
internal/providers/     fixed local execution providers
internal/identity/      Ed25519 enrollment identity
internal/protocol/      approved WSS wire contract
internal/transport/ws/  authenticated reconnecting control client
scripts/                checksum-verified install/update entry points
```

Future features will be added as focused sibling packages under `internal/features/` and `internal/providers/`; empty placeholders are intentionally not created.

## Development commands

```bash
go test ./...
go vet ./...
go build ./cmd/akastr-agent
```

Validate an example configuration and print its non-secret capability manifest:

```bash
go run ./cmd/akastr-agent check-config --config ./config.example.json
go run ./cmd/akastr-agent capabilities --config ./config.example.json
```

Enroll once, then run the daemon:

```bash
akastr-agent enroll --config /etc/akastr-agent/config.json
akastr-agent run --config /etc/akastr-agent/config.json
```

`enroll` generates an Ed25519 key locally and consumes the root-only token file; only the public key is sent. `run` keeps one outbound WSS connection, rejects untyped commands, persists operation results before acknowledging them, and retries unconfirmed natural IPv4 events. Logs contain stable codes rather than payloads or proxy credentials.

For a release asset, `scripts/install.sh` installs a checksum-verified binary without Git, enrolls, creates the systemd unit, and starts it. `scripts/update.sh` validates the new binary/config and restores the previous symlink if restart fails. Release publication and production rollout remain separate operator actions.

Maintainers create deterministic Linux amd64/arm64 assets plus checksum files with `scripts/build-release.sh vX.Y.Z <new-output-directory>`; the installer expects those four files under the matching release tag.
