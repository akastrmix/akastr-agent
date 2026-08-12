# Akastr Agent protocol `2026-08-13.v1`

AkastrCloud exposes HTTPS enrollment and an outbound-only WSS control route. Every JSON envelope contains exactly `protocol`, `message_id`, `type`, `sent_at`, and `body`; text frames are at most 64 KiB. Unknown fields, message types, capability fields, binary frames, invalid UUIDs, and future protocol versions fail closed.

## Enrollment and authentication

An operator creates an installation in AkastrCloud and places the one-time 32-byte base64url token in a root-only file. `akastr-agent enroll` creates an Ed25519 keypair, sends the token, raw 32-byte public key, version, and non-secret capability list over HTTPS, then atomically stores the returned installation UUID with its private key. The token becomes unusable after that transaction.

WSS connects with `agent_id`. AkastrCloud sends `auth.challenge` containing a 32-byte nonce and 15-second validity. The Agent signs these UTF-8 lines without a final newline:

```text
akastr-agent-auth-v1
<agent_id>
<challenge_id>
<nonce>
<issued_at exactly as received>
<expires_at exactly as received>
```

After `auth.response` / `auth.accepted`, the Agent sends `agent.hello`; a connection is ready only after `hello.accepted`. A newer authenticated connection replaces an older one for the same installation.

## Operations and events

`operation.offer` contains `command_id`, `command_type`, `payload_version=1`, a typed payload, `not_before`, and `expires_at`. First-release types are `changeip.execute` and `ipquality.execute`. The Agent replies `operation.accepted`, persists local running state, executes only its configured provider, persists the bounded terminal result, and sends `operation.result`. AkastrCloud confirms only after its database transaction and downstream event admission succeed.

Offers and results may repeat after disconnect. `command_id` is therefore both the execution idempotency key and the local journal key. A terminal local record is replayed, never executed again. No payload may select a program, argv, shell fragment, file, credential, or arbitrary URL.

`ip.observed` carries one natural IPv4 transition with `observation_id`, previous/new address, and observation time. The Agent retains one pending transition until `ip.observed_ack`; the first observation establishes a baseline and is not announced. AkastrCloud applies the existing private-message subscription guards and IPQuality cache reset. Telegram channel delivery is absent.

IPQuality payloads contain only target UUID, expected IPv4, non-secret SOCKS5 host/port, local profile id, and script version. Username/password remain in the Runner's root-only profile file. The Runner permits one active command, verifies the pinned script SHA-256 before every execution, checks proxy IPv4 before and after, and rejects a stale/changed generation.
