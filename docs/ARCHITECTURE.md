# Akastr Agent architecture

## 1. Process model

Each installation runs one `akastr-agent` process under systemd. It initiates the WSS connection to AkastrCloud and never exposes a general-purpose management HTTP server. An installation advertises only the capabilities enabled by its local, root-owned configuration.

The same binary can serve two deployment shapes:

- a target node advertises IP observation, ChangeIP, and optionally a SOCKS5 descriptor;
- a dedicated Runner advertises IPQuality execution with `max_concurrency=1`.

Capabilities are composable; `target` and `runner` are not separate binaries or permanent protocol roles.

## 2. Control-plane boundary

AkastrCloud owns all durable business decisions. The Agent does not know Telegram users, subscriptions, daily limits, notification preferences, presets, or automatic ChangeIP rules.

For IPQuality, the control plane must obtain two resources before dispatch:

1. the target node must have no conflicting ChangeIP operation;
2. a compatible Runner must have an available execution slot.

Requests for the same target/day/IP generation coalesce into one real run. AkastrCloud returns the cached report after the first real run. A Hong Kong calendar-day rollover or observed IPv4 change starts a new cache generation.

The paired ADR 0024 proposal is approved as `2026-08-13/akastr-agent-wss-v1`. Protocol `2026-08-13.v1` uses a 15-second server nonce and Ed25519 signature over a newline-delimited, context-bound challenge. Enrollment tokens are one-time; only public identity and non-secret capabilities enter AkastrCloud. Offers, accepts, terminal results, result acknowledgements, and natural IPv4 observations all carry stable UUIDs. Delivery is at least once, while local journals and database uniqueness make execution/results idempotent.

## 3. Package boundaries

- `internal/config`: strict operator configuration and validation. Unknown fields are errors.
- `internal/capability`: deterministic, non-secret capability descriptions.
- `internal/state`: atomic JSON file persistence with a schema marker.
- `internal/operation`: local exclusive-group enforcement and a bounded operation journal. It stores no command payload or credentials.
- `internal/features/ipwatch`: bounded HTTPS public-IP observation with explicit address-family dialing and durable unconfirmed IPv4 events.
- `internal/providers/changeip/command`: fixed argv execution without a shell, with timeout and process-tree termination.
- `internal/providers/ipquality/script`: checksum-pinned Bash execution through a secret SOCKS5 profile, pre/post proxy IPv4 verification and bounded output parsing.
- `internal/identity`, `internal/protocol`, `internal/transport/ws`: local Ed25519 identity and the reconnecting approved control channel.
- `internal/app`: assembles configuration, executors and runtime entry points.

Reserved features become sibling packages only when their behavior is approved:

```text
internal/providers/changeip/httpflow/
internal/features/xraytraffic/
internal/features/ratelimit/
```

## 4. Local operation state

Every executable operation names an exclusive group. The engine persists an active record before execution and moves it to bounded recent history after a terminal result. A second operation in the same group is rejected locally even if the control plane scheduler misbehaves.

The journal deliberately stores only operation ID, kind, exclusive group, timestamps, state, and a stable terminal code. It does not store task payloads, SOCKS5 credentials, script output, customer information, or Telegram identifiers.

Terminal records include only the bounded safe protocol result needed for retransmission. After restart, an active record remains blocked until the same command is offered; it then becomes `interrupted_unknown` without re-executing and releases the group. The Agent never guesses that an interrupted operation succeeded and never silently deletes an unknown or corrupt state file.

## 5. SOCKS5 and IPQuality

Target-node capability metadata may expose an address source and port, but never a username or password. Runner credentials live in a separate root-only profile file indexed by stable target UUID.

The Runner executes a pinned and checksummed IPQuality script through the selected target SOCKS5 endpoint. The run records the expected target IPv4 generation. If that generation changes before completion, AkastrCloud discards the result even when the script exits successfully.

Proxy-mode reports are not treated as identical to execution on the target host: script checks that use the Runner's resolver or direct local network must be identified during acceptance testing and labelled or omitted.

## 6. Migration boundary

The old IPChanger HTTP endpoints and event callbacks remain in service until all six production nodes have moved to WSS and passed rollback/observation gates. Akastr Agent does not emulate the old endpoints. AkastrCloud temporarily owning both integrations during a staged rollout is a deployment concern, not an Agent compatibility layer.
