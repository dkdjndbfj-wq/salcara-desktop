# Hermes Gateway adapter

Protocol checked against NousResearch/hermes-agent; real Gateway acceptance uses
commit `b52b8b00f0c2af6b6d6ede68d9071be8cf2d1b5a` on 2026-11-08. The
adapter uses the user's installed Hermes client; it does not vendor or modify
the private bootstrap/ticket implementation.

- [`hermes_cli/_launchers.py`](https://github.com/NousResearch/hermes-agent/blob/main/hermes_cli/_launchers.py): `--print-runtime-command --module …` returns the installation-bound Python `-I -c` command. Salcara retains its home and dependency-generation bootstrap and replaces only the requested terminal module entry with its embedded stdio viewer. Native Windows `.exe` and POSIX launchers are supported; a Windows `.cmd`/`.bat` fallback must be repaired to the native official launcher before remote control is enabled.
- [`hermes_cli/gateway_client.py`](https://github.com/NousResearch/hermes-agent/blob/main/hermes_cli/gateway_client.py): `connect_gateway` authenticates the same-user private control channel, obtains a session ticket, and verifies the canonical WebSocket protocol. Salcara removes an inherited `HERMES_TUI_GATEWAY_URL` from the sidecar environment so a local adapter cannot attach to an unrelated remote owner.
- [`gateway/session_controls.py`](https://github.com/NousResearch/hermes-agent/blob/main/gateway/session_controls.py): the sidecar requires durable admission, event replay, CLI creation, and the supported creation parameters from `runtime.describe`. New sessions use `session.create(request_id, source="cli", cwd, model, provider="custom", base_url, api_key, yolo)`. Messages use `prompt.submit(session_id, input_id, text)`. History uses `session.resume`; its explicit identity, cwd, model, generation and replay watermark are checked before sending.
- [`gateway/session_authority.py`](https://github.com/NousResearch/hermes-agent/blob/main/gateway/session_authority.py): stopping calls `session.interrupt(session_id, execution_generation)`. The phone's observed authority epoch/generation must still match a fresh snapshot. The response means stop **requested**, not stopped. Completion comes from the native terminal event.
- [`gateway/session_pending_controls.py`](https://github.com/NousResearch/hermes-agent/blob/main/gateway/session_pending_controls.py): approval replies preserve the exact prompt ID and generation. Only advertised `once`, `session`, and `deny` map to phone decisions. Permanent grants, password/sudo server requests, clarification forms, attachments and child-session controls are not advertised.

The directory contains only Bridge-created Hermes sessions. Their native IDs,
public history, and source fingerprints persist under the Bridge state directory;
API keys are passed through the local stdin protocol and are never persisted in
this index or placed on a process command line. An existing session's API and
model stay frozen. A new session may choose an explicit model even when the
account has no default; unsupported effort/permission options are refused.

An existing Hermes session cannot continue after its Bridge route credential
rotates (including a Bridge restart or authorization update). The official
[`rebind_launch_key`](https://github.com/NousResearch/hermes-agent/blob/main/gateway/session_policy.py)
accepts only the original key, even after the native owner restarts; it cannot
replace that key with a fresh Bridge lease. The adapter records stable API
identity separately from the frozen launch-credential digest so it can explain
whether the API changed or the lease expired. History and exact-generation stop
remain available; the phone shows a notice to start a new session. A changed
gateway address/port also requires a new session. No retired credential is
revived and no rejected key is sent to native resume.

Closing the Bridge detaches its viewer. It does not kill a Gateway process tree
or silently stop work accepted by other clients. Disconnects, replay gaps,
oversized recovery buffers, and uncertain send receipts block further writes
until a fresh authoritative read. A pending native `unknown` execution is never
treated as idle and no message is automatically replayed. Background work that
the native Gateway does not include in its current execution stop remains under
the native application's control.

Source switching is blocked while any Hermes session is running, awaiting
approval, recovering, or uncertain after a disconnect/restart. Opening the
session reads its authoritative snapshot and can clear the guard once idle is
confirmed.

History and live messages share the owner's committed row identities from
`message.complete.persisted_turn`. The final assistant row is bound to its
admission's stream, including when SQLite history becomes visible before the
terminal event. A detached viewer can read the stored `prompt.receipt` to recover
that binding. User echoes retain the Bridge's own submission/client IDs. The
native `display_metadata.submission_id` joins the Bridge's accepted admission
receipt; `gateway_input_owner` is a principal and must never be used as an
admission ID. This
does not compare message text: identical answers in different turns remain
separate. The persisted identity maps and retained event list are bounded.

Validation: Go contract fixtures cover creation, source pinning, generation CAS,
approval scope, late-event recovery, history/live identity, repeated identical
turns, input acknowledgements, tool preservation and launcher parsing. Python standard-library
tests exercise the actual embedded stdio viewer with a fake installed client,
including failure classification and capability gating. These tests do not call
a model provider. An opt-in test additionally runs the unmodified official core
Gateway in its isolated Python 3.14 environment with official locked dependencies,
the production stdio sidecar, and a local synthetic OpenAI-compatible upstream.
It verifies reply, persisted same-key resume, native rejection of a replacement
key, and exact-generation interruption. It does not verify the installed native
launcher package or a real provider account.
The final `-race` acceptance passed in 52.02 seconds: one then two identical-text
answers with distinct committed rows, exactly three synthetic provider calls,
native `admission_conflict` for the deliberately replaced key, and a final
cancelled/interrupted native outcome. No data race was reported.
`internal/agents/hermes_native_policy_test.py` additionally executes the actual
upstream `rebind_launch_key` function (parsed from the file named by
`HERMES_POLICY_SOURCE`) to prove renewed keys are refused both before and after
an owner restart. This source-contract check is separate from a live model run.
