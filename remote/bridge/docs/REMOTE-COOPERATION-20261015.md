# Explicit user cross-task messages

The Mobile “发送到另一任务” panel and own-message “回复” action send a **user message from one task view to another**. This is not a native Codex/Claude agent-management API and is never triggered by model prose or an incoming delegation event.

## Supported contract

- Same built-in Codex or Claude CLI adapter, same selected managed source/account, same paired device; two different root sessions. Both must currently open with exact namespace/ID, be controllable and unarchived. The target must be idle or failed. Native desktop, read-only, child, cross-agent and unknown-state targets are refused.
- `session.cooperation.prepare` is a Hub source-protected transient read. It opens and checks both sessions without submitting a turn, then returns an opaque random 256-bit ticket, source/target snapshots and five-minute expiry. No ticket is persisted.
- Explicit `session.send` includes `sourceSessionKey`, `cooperationTicket` and a UUID operation ID. The ordinary account/model/permission/source validations remain in place. The Bridge reopens the pair, consumes the one-use ticket, and constructs an internal `TurnOptions.CrossTaskOrigin`.
- The ticket binds the Bridge process/agent instance, device, pairing/owner, exact source and target, account/provider/settings and a monotonic source epoch. API A→B→A, policy revision changes, re-pairing, reconnect and manager replacement cannot revive an old ticket. Ordinary unchanged policy heartbeats do not expire it.
- Before native admission the source is opened again and identity/scope/expiry are checked again. Codex and Claude also reject a target that became busy before their native send boundary.

## Display and persistence

`MessageOrigin.kind=user_cross_task` with `verification=bridge_receipt` means a **Bridge-owned receipt for an explicit phone user action**. It contains source session/title, target session and operation ID. It does not identify a model as the sender and grants no subsequent send authority. Raw phone `messageOrigin` is rejected.

Codex persists the exact client UUID and, after observation, its exact native item ID. Claude sends a native user UUID and persists the exact UUID receipt. Neither adapter matches by text/time. Own origins survive Bridge restart and history cache loss; native peer messages, other sessions, child events and assistant messages cannot acquire the receipt. Replies obtain a new preparation.

Mobile saves each attempted submission before transport. Its durable owner includes station/credential scope **and device ID**, plus the exact source session; the saved record contains no ticket. An unknown transport result remains `unconfirmed`, with “打开目标任务” and explicit user acknowledgement before another submission. A known local rejection before invoking transport is `not_sent`. There is no automatic replay, including after restart. Successful records remain available locally, with at most 100 display records per source; unconfirmed records are never evicted. Adapter receipts remain the authoritative exact-ID history.

## Native upstream limits

- Claude SDK `origin.kind=peer` / `fromSession` is documented as sender-asserted navigation, not authentication. A bounded root-session hint is exposed only as `verification=native_reported`, labelled “来源未核验”. It can open a read-only navigation route and never provides a Reply grant. Addresses, PID, arbitrary sender fields and body claims are not emitted as identity. Nested child hints are omitted.
- Codex's observed `codexDelegation` extension is not a public authenticated sender contract. Those events retain only the delegation category. Public `collabAgentToolCall` records and parent relationships are not promoted into a native send-management RPC.
- No `nativeSendMessage` support is claimed. No remote paid model, deployment, physical-device installation or credential change was used in the fixture acceptance.

## Acceptance evidence

Workspace `work/cooperation/` contains the handoff, source manifest and actual RNWeb component browser evidence. Targeted Go tests cover forged provenance, source/target swaps, cross-agent scope, expiry, restart, manager/pair/device/source changes, A→B→A, final admission races and exact-ID receipt recovery. Mobile tests cover transport, source fences, durable uncertain/not-sent receipts, device isolation and explicit UI actions.
