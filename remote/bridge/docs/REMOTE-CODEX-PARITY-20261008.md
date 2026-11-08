# Bridge media and Codex parity audit — 2026-10-08

This document records the Bridge-side changes made for the phone remote
programming path. It does not change the desktop application's UI.

## Changes

- Codex `imageView` and `imageGeneration` items stay folded as `image_view` and
  `image_generation` work events (older Bridges used `read`). A local image path is copied into the private attachment store and
  the Hub receives only an opaque `att_` reference with MIME, size, and SHA-256.
  App-server versions that return a bounded Base64 image in
  `imageGeneration.result` are decoded, magic-checked, and imported through
  the same store.
- The same conversion is applied to live events and paged native history, so
  opening an older turn does not expose a Windows path.
- Absolute image paths are cleared even when the source file has already been
  removed. Non-image tool details are not interpreted as files.
- Codex user image inputs are recovered from the Bridge-owned `att_` paths and
  remain attached to the historical user message when possible.
- Codex history carries the actual turn id and reported `durationMs` so the
  phone can associate a completion summary with the correct turn.

## Bounds and security

- Only JPEG, PNG, and WebP are accepted; each file is at most 8 MiB.
- Attachment reads require a binding for the exact session key. Paths are never
  accepted by the `attachment.read` command.
- Temporary files and binding metadata use private permissions and expire by
  the existing attachment retention policy.
- Provider URLs and arbitrary text are not downloaded by the Bridge. Data URLs
  are accepted only when they contain a bounded Base64 image whose magic bytes
  match PNG, JPEG, or WebP.

## Agent adapter boundary

The Bridge manager aggregates independent Agent adapters behind one event and
session protocol. Codex and Claude Code are the shipped adapters; Claude
Desktop history is a separately gated read-only/native surface. A future Agent
(for example Hermes) must implement detection, paged sessions/history, native
event mapping, send/interrupt/approval behavior and an explicit capability
descriptor before it can be exposed as remotely controllable. Unknown Agent
metadata is not silently routed through Codex or Claude.

The manager exposes this as an explicit registry: extension adapters are
constructed independently and appended after the reserved built-ins. The
adapter descriptor is sanitized before it reaches `agents.status`; it includes
only public identity, control surface, and normalized capabilities. Missing
capabilities never inherit Codex/Claude behavior, and the registry does not
share provider credentials or local paths.

API choices use a second shared aggregation boundary instead of being embedded
in each adapter. `AgentDescriptor.apiFamily` maps a control surface to one
stable API-account namespace (Codex Desktop and Codex CLI share `codex`; Claude
Desktop Code and Claude Code share `claude`; an extension defaults to its own
family). The API directory is the only place that joins an Agent, an opaque
`api_...` account handle, a selected model and a station. A family mutation
checks every adapter sharing that family before committing, so a sibling Agent
cannot keep a live turn on the old key. Provider addresses and keys remain in
the local vault, and model membership is accepted only after a bounded,
fingerprint-checked catalog read.

This aggregation is deliberately not a generic “guess by model name” router:
an unknown Agent is rejected, an extension without a descriptor is
read-only/unknown, and the current Bridge does not claim Hermes execution until
its adapter supplies the native worker settings and event semantics.

## Verification

Run from this Bridge directory:

```text
gofmt -w internal/agents/codex_map.go internal/agents/map_test.go internal/hubclient/client.go internal/hubclient/api_directory.go internal/hubclient/attachments_test.go
go test ./...
```

The phone-side behavior and remaining protocol limitations are documented in
`salcara-mobile/docs/maintainers/REMOTE-CODEX-PARITY-AUDIT-20261008.md`.
