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

The network contract, authentication handshake, database schema, and durable job payload remain intentionally unspecified in this repository until the paired ADR 0024 proposal is approved.

## 3. Package boundaries

- `internal/config`: strict operator configuration and validation. Unknown fields are errors.
- `internal/capability`: deterministic, non-secret capability descriptions.
- `internal/state`: atomic JSON file persistence with a schema marker.
- `internal/operation`: local exclusive-group enforcement and a bounded operation journal. It stores no command payload or credentials.
- `internal/features/ipwatch`: bounded HTTPS public-IP observation with explicit address-family dialing.
- `internal/providers/changeip/command`: fixed argv execution without a shell, with timeout and process-tree termination.
- `internal/app`: assembles configuration and capabilities for CLI/runtime entry points.

Remaining first-release functionality belongs under:

```text
internal/features/changeip/
internal/features/socks5/
internal/features/ipqualityrunner/
internal/providers/ipquality/script/
internal/transport/ws/
```

Reserved features become sibling packages only when their behavior is approved:

```text
internal/providers/changeip/httpflow/
internal/features/xraytraffic/
internal/features/ratelimit/
```

## 4. Local operation state

Every executable operation names an exclusive group. The engine persists an active record before execution and moves it to bounded recent history after a terminal result. A second operation in the same group is rejected locally even if the control plane scheduler misbehaves.

The journal deliberately stores only operation ID, kind, exclusive group, timestamps, state, and a stable terminal code. It does not store task payloads, SOCKS5 credentials, script output, customer information, or Telegram identifiers.

After restart, an active record remains active for explicit protocol reconciliation. The Agent never guesses that an interrupted operation succeeded and never silently deletes an unknown or corrupt state file.

## 5. SOCKS5 and IPQuality

Target-node capability metadata may expose an address source and port, but never a username or password. Runner credentials live in a separate root-only profile file indexed by stable target UUID.

The Runner executes a pinned and checksummed IPQuality script through the selected target SOCKS5 endpoint. The run records the expected target IPv4 generation. If that generation changes before completion, AkastrCloud discards the result even when the script exits successfully.

Proxy-mode reports are not treated as identical to execution on the target host: script checks that use the Runner's resolver or direct local network must be identified during acceptance testing and labelled or omitted.

## 6. Migration boundary

The old IPChanger HTTP endpoints and event callbacks remain in service until all six production nodes have moved to WSS and passed rollback/observation gates. Akastr Agent does not emulate the old endpoints. AkastrCloud temporarily owning both integrations during a staged rollout is a deployment concern, not an Agent compatibility layer.
