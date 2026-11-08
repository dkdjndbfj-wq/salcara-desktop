# Remote conversation metadata: bounded Bridge changes

This note records only the 2026-10-11 Bridge additions. It is not a claim of complete Codex Desktop parity or a release/deployment record. All validation uses synthetic local fixtures; no real account files, credentials, paid model requests, or other users' tasks were accessed.

## Reported conversation identity

- `SessionInfo.agentNickname` and `agentRole` retain the corresponding reported Codex thread fields, bounded to 160 characters in the CLI adapter and 160 bytes in the existing native projector.
- `forkedFromSessionKey` retains an explicitly reported, validated source ID. It describes a fork relationship only. It does not grant permission to read/control the source thread or expand a native lease.
- The native adapter accepts these optional fields only if its host actually returns them. Older hosts omit them and retain their existing behavior. There is no inferred nickname, role, or invented per-agent image.

The public generated app-server v2 `Thread` schema reports `agentNickname`, `agentRole`, and `forkedFromId`. Reference: [Codex app-server documentation](https://developers.openai.com/codex/app-server). The inspected generated protocol schemas are the workspace's `work/conversation-protocol-schema/v2/Thread.ts` and `ThreadItem.ts`.

## Delegated messages

The existing native-history projector already reads `content[].codexDelegation.input`. It now additionally emits `messageOrigin: { "kind": "delegation" }` only for a non-null object containing a string `input`. Invalid extensions fall back to the ordinary text field. Unknown extension fields, including a claimed `sourceThreadId`, are not forwarded.

This native extension is **not established by the public app-server UserInput schema**. The host may reconstruct its object from a text wrapper: even a parsed object therefore establishes only an observed display category, not an authenticated origin. It is confined to the existing native projector. No CLI text parsing, guessed source identity, external ChatGPT ingress, or cross-task sending API was added. Neither `codexDelegation` nor `clientId` may authorize a source, prove that ChatGPT sent a message, infer a source task, or confirm a phone's pending message. Ordinary text saying "from ChatGPT" does not acquire a delegated category in this Bridge.

The phone must not replace an optimistic user message merely because a delegated message has equal text, and must not navigate to a source thread guessed from text or client metadata.

## Turn-start state and terminal errors

- Creating a thread no longer marks it running before `turn/start` is accepted. A synchronous capacity rejection therefore does not leave a permanently busy empty conversation.
- An explicit `inProgress` reply marks the accepted turn running when no newer lifecycle notification has already arrived.
- A per-thread lifecycle version prevents a delayed `turn/start` reply from reviving a completed turn. Whole-process unloads advance the version as well. Ambiguous failures never cancel or falsely fail actual accepted work.
- CLI live notifications, wrapped turn-start RPC errors, and CLI/native terminal history share an allowlisted typed error mapper. A typed condition takes precedence over capacity/Key wording in its human-readable message.
- Recognized conditions include `model_capacity`, `context_limit`, `session_budget`, `usage_limit`, explicit HTTP `upstream_rate_limit`/`upstream_unauthorized`/`upstream_forbidden`/`upstream_bad_request`, connection/internal errors, `response_stream_interrupted`, `response_retry_exhausted`, policy/sandbox errors, rollback failure, and `turn_not_steerable`. HTTP 429 alone is not model capacity. Context, policy, stream, and sandbox conditions must not become a change-Key prompt merely by matching prose.
- Live `error.willRetry` remains `retrying`; terminal history does not inherit a live retry indicator. The inspected public `TurnError`/`ErrorNotification` schema provides **no `retryAfter` field**, so this adapter does not invent a retry countdown from natural-language text. Only absent/JSON-null typed information permits the old capacity-text fallback. An explicitly present unknown/malformed typed condition becomes neutral `unknown_execution_error`, never guessed from its prose, and does not forward its raw properties.
- Native historical error display does not change delivery uncertainty, receipts, retries, authorization, or unsupported native capability checks.

## Unknown records and incremental history

- An unsupported completed CLI/native item becomes a bounded `unsupported_item` notice inside its real conversation/turn. Its raw arguments, private result, file paths, and unknown extension fields are not forwarded. This preserves the **fact that an unrendered record exists**, not lossless support of that future record type.
- Unknown explicit terminal turn states become `unsupported_turn_state` notices rather than a successful completion. A live unknown `turn/completed` payload does not clear an existing busy turn, advance its lifecycle, or drain queued work; the real turn remains interruptible. A later supported host status/read establishes the actual state. A missing legacy status is not guessed from duration.
- A recognized historical completed turn no longer requires `durationMs`. `inProgress` remains started even when a duration exists. CLI history turn boundaries have stable IDs tied to the reported turn ID.
- Negative or excessively large reasoning indices are rejected before allocation. This prevents a malformed compatibility runtime from panicking or allocating an unbounded sparse summary; only a neutral notice is shown, not its raw delta.
- Both the bounded official history watcher and the explicit old-runtime fallback compare local SHA-256 item snapshots using stable reported item IDs, not only array indices. Existing message text, final/commentary phase, and tool output/status updates now reach the phone. Insertions do not duplicate unchanged older items.
- Initial reads seed known historical states. Only new/changed explicit turn states are emitted afterward, avoiding repeated old completion events. A status-only transition triggers the final backfill even when the host's updated timestamp did not change.
- The official background read retains the existing maximum of 20 pages of 5 turns. Fingerprints are bounded to 100 turns, 4096 items, and 16 MiB of observed item bytes; only hashes are retained, not raw results. A missing/compacted or over-budget boundary emits the existing `history-gap` recovery signal instead of reading all history in the background or silently dropping the gap. The old unsupported-method fallback still uses its existing full `thread/read`, but retains/processes only its newest bounded fingerprint window.
- A per-thread history-read version rejects an older reply after a newer read, a fresh opening of the same baseline, or whole-process unload. The legacy `thread/read` path additionally rejects a returned thread ID different from the requested ID. None of these changes resume an external running thread or turn a native read into CLI control.

## Explicit unknown ThreadStatus admission

The CLI adapter previously mapped every unrecognized `Thread.status.type` and `thread/status/changed.status.type` to idle. The external-thread send preflight checked only known active/in-progress turns and recent rollout writes, so a future non-empty thread state could pass through to `thread/resume`.

The bounded fix is internal only:

- An explicit unrecognized thread status sets `statusUnconfirmed`, removes `Controllable`, and uses the existing non-ready state rather than inventing a new wire/UI enum. Already-known running/waiting state and the actual turn ID are retained; no completed/failed turn event is fabricated.
- External continuation still performs its existing fresh read, but an unconfirmed result cannot resume or start a turn. A loaded unconfirmed runtime also rejects send/direct turn admission and blocks its settings-driven restart. Interrupt remains available for the existing known turn ID.
- Supported `idle` and `notLoaded` retain their existing meaning and can clear the internal flag on a fresh observation. Supported active/system-error observations likewise establish their existing state. A partial old-runtime snapshot without a thread status does not clear an earlier explicit unknown state. A first legacy snapshot with no earlier unknown observation retains its compatibility behavior.
- Invalid/empty thread attribution on status notifications is ignored. Status updates are emitted when readiness changes, even if the visible busy state remains the same.

This does not expand native control or add a new native preflight RPC. The native station-handover guard already checks explicit idle states with `nativeThreadIdle` and rejects unknown states independently of the `nativeStatus` display projection. The existing native display projection's fallback remains a separate host-version compatibility limitation, not proof of an idle native task or permission to resume it through CLI.

## Files

- `internal/protocol/protocol.go`: optional metadata DTOs.
- `internal/protocol/execution_errors.go`, `execution_errors_test.go`: shared typed execution-condition normalization and exhaustive scalar/object fixtures.
- `internal/agents/codex_map.go`, `codex.go`: thread metadata and accepted-turn lifecycle fence.
- `internal/agents/codex_message_pages.go`, `codex_requests.go`, `history_pages.go`: bounded item/turn fingerprint backfill, status-only refresh, read-version fences, and fallback snapshot seeding.
- `internal/desktopcompanion/active.go`, `native_history.go`: optional reported native metadata, delegated category, and terminal error projection.
- `internal/agents/codex_conversation_metadata_test.go`: bounded metadata, authorization separation, capacity rejection, accepted-turn state, completion-before-reply and rejection-after-start fixtures.
- `internal/desktopcompanion/conversation_metadata_test.go`: structured-only delegation classification, ignored unknown source fields, related-thread metadata and terminal errors.
- `internal/agents/codex_protocol_conditions_test.go`: typed live/history/wrapped-RPC errors, unsupported-record payload safety, unknown-state interruption, malformed reasoning indices, and global-error task isolation.
- `internal/agents/codex_history_updates_test.go`: official/legacy updates, stable-ID insertion, old-reply/reopen races, status-only final backfill, budget/boundary recovery, and mismatched thread identity.
- `internal/agents/codex_unknown_thread_status_test.go`: future-status resume refusal, known idle/notLoaded recovery, partial legacy snapshots, loaded runtime restart/send refusal with preserved interrupt, direct admission and invalid task attribution.
- `internal/desktopcompanion/native_unknown_records_test.go`: safe future-record placeholders and unknown/missing state handling.

## Validation

- Focused metadata/lifecycle/terminal-error regression set passed: agents 0.337 seconds; desktop companion 0.311 seconds.
- Targeted race run for that set passed: agents 1.524 seconds; desktop companion 1.461 seconds.
- Wider package run passed: `go test ./internal/agents ./internal/desktopcompanion ./internal/protocol -count=1 -timeout=120s` (agents 102.696 seconds; desktop companion 40.803 seconds; protocol has no tests). This wider run preceded the final native terminal-error parser addition, which was separately verified by the focused/race set.
- Package vet passed. Tracked-file `git diff --check` reported no whitespace errors (only the workspace's existing CRLF conversion warnings).

Additional validation for the typed-error/unknown-record/history fixes:

- Final focused protocol/agents/native metadata + condition + watcher set passed: protocol 0.258 seconds, agents 0.500 seconds, desktop companion 0.688 seconds.
- Final focused race set, including typed-unknown neutrality, malformed/overflow reasoning indices, watcher updates and newer-read/reopen/process-unload races, passed: protocol 1.347 seconds, agents 2.672 seconds, desktop companion 1.987 seconds. Package vet passed in the same final verification command.
- An earlier three-package full run exposed outdated assertions expecting message-only history or inventing completion from absent legacy status. Those fixture updates are owned by the other integration task; the earlier full run was **not a pass** and does not substitute for final integrated validation.
- Package vet and tracked production-file `git diff --check` passed after the watcher changes (CRLF conversion warnings only).
- Focused explicit-unknown-thread-state tests plus existing resume/lifecycle/typed-error/watcher regressions passed: agents 0.445 seconds; focused race passed in 2.459 seconds. Agents package vet and tracked production-file diff whitespace check passed (CRLF conversion warning only).

The main feature audit records the later integrated validation for concurrent changes, including all updated historical fixtures. These synthetic test results are not a real-phone/native-host acceptance test.

Real host verification remains required: actual native host fields and availability vary by Codex version; this work does not authorize or validate an external ChatGPT account connection.
