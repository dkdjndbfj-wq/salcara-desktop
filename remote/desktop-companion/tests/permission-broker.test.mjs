import test from "node:test";
import assert from "node:assert/strict";
import { createPermissionBroker, APPROVAL_TRANSPORT } from "../src/permission-broker.mjs";

const id = "11111111-2222-4333-8444-555555555555";
const key = `codex:${id}`;
const turnId = "22222222-2222-4333-8444-555555555555";
const approvalId = "33333333-2222-4333-8444-555555555555";
const operationId = "44444444-2222-4333-8444-555555555555";
const hook = { type: "hook.permission", sessionKey: key, turnId, toolName: "Bash", toolInput: { command: "git status", description: "Read status" }, cwd: "/fixture" };
const response = { type: "approval.respond", sessionKey: key, approvalId, decision: "allow", operationId };
const broker = options => createPermissionBroker({ allowed: new Set([key]), nonce: () => approvalId, ...options });

test("capability only appears after a genuine scoped request; approval maps to official hook output", async () => {
  const b = broker();
  assert.equal(b.observed(), false);
  const wait = b.request(hook);
  assert.equal(b.observed(), true);
  assert.deepEqual(b.snapshot(key)[0], { type: "approval.request", sessionKey: key, tool: "codex", ts: b.snapshot(key)[0].ts,
    approvalId, kind: "command", title: "Bash", detail: JSON.stringify(hook.toolInput), cwd: "/fixture", turnId,
    expiresAt: b.snapshot(key)[0].expiresAt, approvalTransport: APPROVAL_TRANSPORT });
  assert.deepEqual(b.snapshot("codex:outside"), []);
  const ack = b.respond(response);
  assert.deepEqual(ack, { accepted: true, approvalId, sessionKey: key });
  assert.deepEqual(await wait, { hookSpecificOutput: { hookEventName: "PermissionRequest", decision: { behavior: "allow" } } });
  assert.deepEqual(b.respond(response), ack);
  assert.throws(() => b.respond({ ...response, decision: "deny" }), /APPROVAL_EXPIRED/);
  assert.throws(() => b.respond({ ...response, operationId: turnId }), /APPROVAL_EXPIRED/);
  assert.equal(b.snapshot(key)[0].by, "phone");
  assert.ok(!JSON.stringify(b.snapshot(key)).includes("git status"), "resolved receipts drop original tool inputs");
  b.close();
});

test("denial is for this request only and cannot introduce session rules or changed tool inputs", async () => {
  const b = broker(); const wait = b.request(hook);
  for (const invalid of [{ ...response, decision: "allow_session" }, { ...response, answers: {} }, { ...response, updatedInput: {} },
    { ...response, sessionKey: "codex:outside" }, { ...response, operationId: "invalid" }, { ...response, message: "x".repeat(2001) }]) {
    assert.throws(() => b.respond(invalid));
  }
  b.respond({ ...response, decision: "deny", message: "Do not run" });
  assert.deepEqual(await wait, { hookSpecificOutput: { hookEventName: "PermissionRequest", decision: { behavior: "deny", message: "Do not run" } } });
  b.close();
});

test("timeout, lease revocation, and hook client cancellation decline to decide, never allow or deny original host operation", async () => {
  const timed = broker({ waitMs: 5 }); const wait = timed.request(hook);
  assert.deepEqual(await wait, {});
  assert.throws(() => timed.respond(response), /APPROVAL_EXPIRED/); timed.close();
  for (const method of ["lease", "client", "close"]) {
    const lease = new AbortController(), client = new AbortController();
    const b = broker({ signal: lease.signal }); const pending = b.request(hook, client.signal);
    if (method === "lease") lease.abort(); else if (method === "client") client.abort(); else b.close();
    assert.deepEqual(await pending, {});
    assert.ok(b.snapshot(key).every(event => event.type !== "approval.request")); b.close();
  }
});

test("wall-clock expiry is checked again before an answer and oversize commands are never truncated for approval", async () => {
  let now = 1000000;
  const b = broker({ now: () => now }); const wait = b.request(hook);
  now += 110000;
  assert.throws(() => b.respond(response), /APPROVAL_EXPIRED/);
  assert.deepEqual(await wait, {}); b.close();
  const invalid = broker();
  for (const input of [{ ...hook, sessionKey: "codex:other" }, { ...hook, turnId: "invalid" }, { ...hook, toolInput: { command: "x".repeat(16001) } },
    { ...hook, caller: id }, { ...hook, toolInput: [] }]) await assert.rejects(invalid.request(input));
  assert.equal(invalid.observed(), false); invalid.close();
});
