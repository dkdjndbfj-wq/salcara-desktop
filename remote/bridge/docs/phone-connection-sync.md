# One scan for a computer's saved relays

The initial QR pairs a physical computer and phone. The current Hub and Bridge
both advertise `pair.connections.v1`; older versions retain the existing
single-relay pairing and restoration flow.

After QR confirmation, and when an existing connection becomes available, the
phone sends the authenticated command `{ "type": "remote.connections.sync" }`.
It has no caller-supplied URL or destination. The Bridge enumerates at most 32
saved, non-Bot relay identities belonging to the current physical computer.

For each relay, the Bridge anonymously checks discovery, then authenticates
`POST /bridge/pair/sync` with that relay's saved device ID and device secret.
The body contains `deviceId`, `computerId`, `bindingId`, `phoneHash`, and
`pairHash`. The phone token is an HMAC scoped to the target relay, computer and
phone-binding generation. Only its SHA-256 hash is sent to the target Hub.
The initial QR uses an optional generation-scoped `pairSeed` supplied by the
Bridge in `/bridge/pair/start`; the Hub derives the same phone token from that
seed and the claimed phone hash. The seed is transient and never sent to the
phone or persisted. Older Hubs ignore this optional field.

Retries retain the same token, including after process restarts. An existing
binding to another phone, or a revoked instance of the same generation, is
rejected. Binding creation uses the Hub's existing durable issuance boundary.

The command response contains `computerId` and `connections`. A successful row
contains `hubUrl`, `deviceId`, `deviceName`, and `token`; an unsuccessful row
contains the same public metadata and `error`. The response never contains a
device secret or model API key. It cannot be stored as a Hub command receipt:
explicit `requestId` or `receiptOnly` is rejected. Redirects are not followed.

If a relay holds an older, non-reproducible QR token for the same phone, it
returns `retained: true` instead of replacing that token. The phone keeps its
saved credential, or reports missing local credentials if that record was lost.

The phone validates the complete response and expected physical computer before
importing credentials into SecureStore. Its public database contains metadata
only. Import rechecks the original source credential inside the local mutation
queue, and rolls back an import if the selected computer changes during the
write. The selected connection remains the QR's original connection.

An offline or outdated secondary relay does not undo the original pairing.
The phone exposes per-relay failure state and a manual retry, and retries when
the home connection is refreshed (at most once every 30 seconds). Reopening the
app uses persisted credentials without another QR. Unbinding retires the same
physical-phone generation across all saved relays.

`tools/single-scan-e2e.cjs` runs an isolated Bridge and two independent Hub
processes on dynamically allocated loopback ports. It decodes one actual QR
PNG, checks partial failure and retry, credential isolation, rejected target
injection, restart restoration, and cross-relay revocation. It never invokes a
model or accesses production configuration. `tools/device-pair-e2e.cjs` remains
as a compatibility entry point to that current acceptance test.
