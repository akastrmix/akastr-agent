# Akastr Agent

Akastr Agent is the lightweight, outbound-controlled runtime for AkastrCloud service nodes. It replaces the design of the old `ip-changer` project with a single Go process, typed capabilities, bounded local state, and an agent-initiated WSS control channel.

This repository is currently in the pre-production foundation stage. It is not connected to AkastrCloud production and must not replace any of the six existing IPChanger installations yet.

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

The WSS daemon, enrollment, IPQuality Runner, and installation scripts are added only after the paired AkastrCloud protocol and rollout proposal is approved. Public IP observation and the fixed ChangeIP command provider already have isolated, unit-tested implementations but are not yet wired to a daemon.
