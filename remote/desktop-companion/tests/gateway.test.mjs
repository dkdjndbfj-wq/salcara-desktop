import test from "node:test";
import assert from "node:assert/strict";
import { request as httpRequest } from "node:http";
import { mkdtemp, rmdir } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { randomUUID } from "node:crypto";
import { createDesktopConnect, validConnectArguments, DESKTOP_CAPABILITIES } from "../src/gateway.mjs";
import { runPermissionHook } from "../src/permission-hook.mjs";

const caller = "11111111-2222-4333-8444-555555555555";
const target = "22222222-2222-4333-8444-555555555555";
const other = "33333333-2222-4333-8444-555555555555";
const key = `codex:${target}`;
const meta = { "openai/threadId": caller, "openai/turnId": "44444444-2222-4333-8444-555555555555" };
const args = { allowRemoteControl: true, durationSeconds: 30, sessionKeys: [key] };
const result = (value) => ({ success: true, contentItems: [{ type: "inputText", text: JSON.stringify(value) }] });

async function fixture(overrides = {}) {
  const directory = await mkdtemp(join(tmpdir(), "salcara-gateway-test-"));
  let descriptor; let activate; let hashes = 0; let removes = 0; let writes = 0; const calls = [];
  const ready = new Promise((resolve) => { activate = resolve; });
  const controller = new AbortController();
  const native = async (request) => {
    calls.push(request);
    if (overrides.nativeCall) return overrides.nativeCall(request);
    if (request.tool === "list_threads") return result({ schemaVersion: 4, pinnedThreads: [{ id: caller, title: "Controller private" }],
      threads: [{ id: target, title: "Original" }, { id: other, title: "Outside scope" }] });
    if (request.tool === "read_thread") return result({ schemaVersion: 1, thread: { id: request.args.threadId, kind: "codex", hostId: "local" }, turns: [] });
    return result({ threadId: request.args.threadId });
  };
  const connect = createDesktopConnect({ directory, inspectHost: async () => { hashes++; return { environmentMatches: true, metadataPresent: true, versionKnown: true,
    pipePath: "FIXTURE_ONLY" }; }, catalog: async () => ["list_threads", "read_thread", "send_message_to_thread"], nativeCall: native,
    writeDescriptor: async (_directory, value) => { writes++; descriptor = value; return "FIXTURE_ONLY_DESCRIPTOR"; },
    removeDescriptor: async (_path, _value) => { removes++; }, onActive: (value) => { descriptor = value; activate(value); }, ...overrides, nativeCall: native });
  const done = connect(overrides.connectArgs ?? args, meta, { signal: controller.signal }).finally(() => rmdir(directory));
  await ready;
  const rpc = async (body, options = {}) => {
    return new Promise((resolve, reject) => {
      const request = httpRequest(`http://127.0.0.1:${descriptor.port}/rpc`, { method: "POST", headers: {
        "Content-Type": "application/json", Authorization: `Bearer ${descriptor.token}`, ...options.headers,
      } }, (response) => {
        let data = ""; response.on("data", (chunk) => { data += chunk; });
        response.on("end", () => { try { resolve({ status: response.statusCode, body: JSON.parse(data) }); } catch (error) { reject(error); } });
      });
      request.on("error", reject); request.end(JSON.stringify(body));
    });
  };
  return { descriptor, done, controller, calls, rpc, connect, hashes: () => hashes, writes: () => writes, removes: () => removes };
}

test("connect arguments require explicit consent, bounded duration, canonical distinct original targets", () => {
  assert.equal(validConnectArguments(args), true);
  assert.equal(validConnectArguments({ ...args, durationSeconds: 2592000 }), true);
  for (const bad of [{ ...args, allowRemoteControl: false }, { ...args, durationSeconds: 2592001 }, { ...args, durationSeconds: 29 },
    { ...args, sessionKeys: [] }, { ...args, sessionKeys: [key, key] }, { ...args, sessionKeys: [key.toUpperCase()] },
    { ...args, sessionKeys: [key.replace('-4333-', '-0333-')] }, { ...args, sessionKeys: [key.replace('-8444-', '-1444-')] }, { ...args, threadId: caller }]) {
    assert.equal(validConnectArguments(bad), false);
  }
});

test("no host context, self-target, unpinned host or native rejection cannot activate or write descriptor", async () => {
  let disk = 0; let nativeCalls = 0;
  const options = { inspectHost: async () => ({ environmentMatches: true, metadataPresent: true, versionKnown: true }),
    catalog: async () => ["list_threads", "read_thread", "send_message_to_thread"], nativeCall: async () => { nativeCalls++; return { success: false }; },
    writeDescriptor: async () => { disk++; }, removeDescriptor: async () => undefined };
  assert.equal((await createDesktopConnect(options)(args, undefined)).errorCode, "HOST_CONTEXT_MISSING");
  assert.equal((await createDesktopConnect(options)({ ...args, sessionKeys: [`codex:${caller}`] }, meta)).errorCode, "SESSION_NOT_ALLOWED");
  assert.equal((await createDesktopConnect({ ...options, inspectHost: async () => ({ environmentMatches: true, metadataPresent: true, versionKnown: false }) })(args, meta)).errorCode, "HOST_VERSION_UNAVAILABLE");
  assert.equal((await createDesktopConnect(options)(args, meta)).errorCode, "NATIVE_CALL_REJECTED");
  assert.equal(disk, 0); assert.equal(nativeCalls, 1);
});

test("only an active authenticated lease reports control and list/read/send remain in the original explicit scope", async () => {
  const f = await fixture();
  try {
    const status = await f.rpc({ type: "status" });
    assert.equal(status.body.result.desktopControl, true); assert.equal(status.body.result.remoteSend, true);
    assert.equal(status.body.result.controllerThreadId, caller); assert.deepEqual(status.body.result.sessionKeys, [key]);
    assert.deepEqual(status.body.result.capabilities, DESKTOP_CAPABILITIES);
    assert.ok(!JSON.stringify(status.body).includes(f.descriptor.token));
    const listed = await f.rpc({ type: "list" });
    const list = JSON.parse(listed.body.result.contentItems[0].text);
    assert.equal(list.schemaVersion, 4, "the Go native list parser must receive the pinned schema version");
    assert.deepEqual(list.pinnedThreads, []); assert.equal(list.threads.length, 1); assert.equal(list.threads[0].id, target);
    const read = await f.rpc({ type: "read", sessionKey: key });
    assert.equal(JSON.parse(read.body.result.contentItems[0].text).thread.id, target);
    const send = await f.rpc({ type: "send", sessionKey: key, text: "Continue original task", operationId: "55555555-2222-4333-8444-555555555555" });
    assert.equal(JSON.parse(send.body.result.contentItems[0].text).threadId, target);
    assert.ok(f.calls.every((call) => call.context.threadId === caller && call.context.turnId === meta["openai/turnId"]));
    assert.equal(f.calls.find((call) => call.tool === "send_message_to_thread").args.threadId, target);
  } finally { f.controller.abort(); }
  const ended = await f.done;
  assert.equal(ended.desktopControl, false); assert.equal(ended.remoteSend, false); assert.equal(f.removes(), 1);
  assert.ok(!JSON.stringify(ended).includes(f.descriptor.token));
});

test("unauthenticated browsers, out-of-scope targets, unknown commands and overridden caller fields fail before native dispatch", async () => {
  const f = await fixture(); const before = f.calls.length;
  try {
    assert.equal((await f.rpc({ type: "status" }, { headers: { Authorization: "Bearer bad" } })).status, 401);
    assert.equal((await f.rpc({ type: "status" }, { headers: { Origin: "https://evil.example" } })).status, 401);
    assert.equal((await f.rpc({ type: "status" }, { headers: { Host: "evil.example" } })).status, 401);
    for (const body of [{ type: "read", sessionKey: `codex:${caller}` }, { type: "send", sessionKey: `codex:${other}`, text: "No" },
      { type: "restart" }, { type: "read", sessionKey: key, threadId: caller }, { type: "send", sessionKey: key, text: "No", operationId: "not-an-id" }]) {
      assert.equal((await f.rpc(body)).body.ok, false);
    }
    assert.equal(f.calls.length, before);
  } finally { f.controller.abort(); await f.done; }
});

test("updated native schema uses ten-turn pages and verifies lease-bound opaque history cursors before dispatch", async () => {
  const f = await fixture({ connectArgs: { ...args, sessionKeys: [key, `codex:${other}`] }, nativeCall: async ({ tool, args: a }) => {
    if (tool === "list_threads") return result({ schemaVersion: 4, threads: [], pinnedThreads: [] });
    assert.equal(tool, "read_thread"); assert.equal(a.turnLimit, 10);
    assert.ok(a.cursor === undefined || a.cursor === 'native-history-anchor');
    return result({ schemaVersion: 1, thread: { id: a.threadId, kind: "codex", hostId: "local" },
      page: { hasMore: a.cursor === undefined, nextCursor: a.cursor === undefined ? 'native-history-anchor' : null }, turns: [] });
  } });
  try {
    const first = JSON.parse((await f.rpc({ type: "read", sessionKey: key })).body.result.contentItems[0].text);
    const cursor = first.page.nextCursor;
    assert.notEqual(cursor, 'native-history-anchor');
    const before = f.calls.length;
    for (const body of [{ type: "read", sessionKey: `codex:${other}`, cursor }, { type: "read", sessionKey: key, cursor: 'native-history-anchor' }, { type: "read", sessionKey: key, cursor: null }]) {
      assert.equal((await f.rpc(body)).body.error, "REQUEST_INVALID");
    }
    assert.equal(f.calls.length, before);
    const older = JSON.parse((await f.rpc({ type: "read", sessionKey: key, cursor })).body.result.contentItems[0].text);
    assert.equal(older.page.nextCursor, null); assert.deepEqual(older.approvalEvents, []);
  } finally { f.controller.abort(); await f.done; }
});

test("small first history page passes its turn limit and invalid limits never reach native tools", async () => {
  const f = await fixture();
  try {
    const initial = await f.rpc({ type: "read", sessionKey: key, limit: 2 });
    assert.equal(initial.status, 200);
    assert.equal(f.calls.at(-1).args.turnLimit, 2);
    for (const limit of [0, 11, 1.5, "2", null]) {
      const before = f.calls.length;
      assert.equal((await f.rpc({ type: "read", sessionKey: key, limit })).status, 400);
      assert.equal(f.calls.length, before);
    }
  } finally { f.controller.abort(); await f.done; }
});

test("operation IDs prevent duplicate native sends and reject changed retry bodies", async () => {
  const f = await fixture();
  try {
    const request = { type: "send", sessionKey: key, text: "Same original task", operationId: "55555555-2222-4333-8444-555555555555" };
    assert.equal((await f.rpc(request)).body.ok, true);
    assert.equal((await f.rpc(request)).body.ok, true);
    assert.equal((await f.rpc({ ...request, text: "Changed" })).body.error, "OPERATION_CONFLICT");
    assert.equal(f.calls.filter((call) => call.tool === "send_message_to_thread").length, 1);
  } finally { f.controller.abort(); await f.done; }
});

test("more than 128 completed sends work, and an old compact receipt still prevents replay", async () => {
  const f = await fixture();
  try {
    const first = { type: "send", sessionKey: key, text: "First task", operationId: randomUUID() };
    assert.equal((await f.rpc(first)).body.ok, true);
    for (let i = 0; i < 140; i++) {
      assert.equal((await f.rpc({ ...first, text: `Task ${i}`, operationId: randomUUID() })).body.ok, true);
    }
    assert.equal((await f.rpc(first)).body.ok, true);
    assert.equal((await f.rpc({ ...first, text: "Changed task" })).body.error, "OPERATION_CONFLICT");
    assert.equal(f.calls.filter(call => call.tool === "send_message_to_thread").length, 141);
  } finally { f.controller.abort(); await f.done; }
});

test("concurrent identical operation shares one dispatch; ambiguous failure becomes a non-replayable tombstone", async () => {
  let release; const blocked = new Promise(resolve => { release = resolve; });
  const f = await fixture({ nativeCall: async ({ tool, args: nativeArgs }) => {
    if (tool === "list_threads") return result({ schemaVersion: 4, threads: [], pinnedThreads: [] });
    if (tool === "read_thread") return result({ schemaVersion: 1, thread: { id: target, kind: "codex", hostId: "local" }, turns: [] });
    await blocked; throw new Error("private raw host failure");
  } });
  try {
    const request = { type: "send", sessionKey: key, text: "Task", operationId: randomUUID() };
    const pending = [f.rpc(request), f.rpc(request)];
    // Let both local requests enter the gateway before the fixture settles.
    await new Promise(resolve => setTimeout(resolve, 20)); release();
    const replies = await Promise.all(pending);
    assert.ok(replies.every(reply => reply.body.ok === false));
    assert.equal((await f.rpc(request)).body.error, "OPERATION_UNCERTAIN");
    assert.equal(f.calls.filter(call => call.tool === "send_message_to_thread").length, 1);
  } finally { release(); f.controller.abort(); await f.done; }
});

test("unsupported approval and interrupt are explicit capabilities, never native calls", async () => {
  const f = await fixture();
  try {
    const count = f.calls.length;
    for (const type of ["interrupt", "approval.respond"]) assert.equal((await f.rpc({ type, sessionKey: key })).body.error, "CAPABILITY_UNAVAILABLE");
    assert.equal(f.calls.length, count);
  } finally { f.controller.abort(); await f.done; }
});

test("real loopback command hook waits for mobile approval, uses original scope and never invokes a private approval tool", async () => {
  const f = await fixture();
  let hook;
  try {
    const before = f.calls.length;
    hook = runPermissionHook({ hook_event_name: "PermissionRequest", session_id: target, turn_id: other,
      tool_name: "Bash", tool_input: { command: "git status" }, cwd: "/fixture" }, { readDescriptor: async () => f.descriptor });
    let read;
    for (let i = 0; i < 20; i++) {
      read = JSON.parse((await f.rpc({ type: "read", sessionKey: key })).body.result.contentItems[0].text);
      if (read.approvalEvents.length) break;
      await new Promise(resolve => setTimeout(resolve, 5));
    }
    const approval = read.approvalEvents[0];
    assert.equal(approval.approvalTransport, "codex-hook-v1"); assert.equal(approval.sessionKey, key);
    const status = (await f.rpc({ type: "status" })).body.result;
    assert.equal(status.capabilities.approval, true); assert.equal(status.approvalTransport, "codex-hook-v1");
    assert.equal(status.capabilities.interrupt, false);
    const command = { type: "approval.respond", sessionKey: key, approvalId: approval.approvalId, decision: "allow", operationId: randomUUID() };
    assert.equal((await f.rpc({ ...command, decision: "allow_session" })).body.error, "REQUEST_INVALID");
    assert.equal((await f.rpc(command)).body.result.accepted, true);
    assert.deepEqual(await hook, { hookSpecificOutput: { hookEventName: "PermissionRequest", decision: { behavior: "allow" } } });
    assert.equal((await f.rpc(command)).body.result.accepted, true);
    assert.ok(f.calls.slice(before).every(call => call.tool === "read_thread"), "no fake native permission API or CLI is used");
    assert.equal(JSON.parse((await f.rpc({ type: "read", sessionKey: key })).body.result.contentItems[0].text).approvalEvents[0].type, "approval.resolved");
  } finally { f.controller.abort(); await f.done; if (hook) await hook; }
});

test("active authorization cannot be duplicated; authenticated disconnect closes gateway and removes only owned lease", async () => {
  const f = await fixture();
  assert.equal((await f.connect(args, meta)).errorCode, "CONTROL_ALREADY_ACTIVE");
  assert.equal((await f.rpc({ type: "disconnect" })).body.result.disconnected, true);
  assert.equal((await f.done).errorCode, "CONTROL_REVOKED");
  assert.equal(f.removes(), 1);
  await assert.rejects(f.rpc({ type: "status" }));
});

test("wrong target/host returned by native read cannot activate even if native success is true", async () => {
  let writes = 0;
  const connect = createDesktopConnect({ inspectHost: async () => ({ environmentMatches: true, metadataPresent: true, versionKnown: true }),
    catalog: async () => ["list_threads", "read_thread", "send_message_to_thread"],
    nativeCall: async ({ tool }) => tool === "list_threads" ? result({ schemaVersion: 4, threads: [], pinnedThreads: [] })
      : result({ thread: { id: target, kind: "codex", hostId: "durable" } }),
    writeDescriptor: async () => { writes++; }, removeDescriptor: async () => undefined });
  assert.equal((await connect(args, meta)).errorCode, "NATIVE_RESPONSE_INVALID"); assert.equal(writes, 0);
});

test("expiry is checked on demand and revokes only the gateway lease", async () => {
  let currentTime = Date.now();
  const f = await fixture({ now: () => currentTime });
  currentTime = f.descriptor.expiresAt;
  // Revocation may close the local socket before the fixed error response flushes;
  // either outcome is fail-closed, with no desktop request made.
  const expired = await f.rpc({ type: "status" }).catch(() => null);
  if (expired) { assert.equal(expired.body.ok, false); assert.equal(expired.body.error, "CONTROL_EXPIRED"); }
  assert.equal((await f.done).errorCode, "CONTROL_EXPIRED");
  assert.equal(f.calls.filter((call) => call.tool === "send_message_to_thread").length, 0);
  assert.equal(f.removes(), 1);
});

test("30-day lease does not overflow Node's maximum timer delay and revoke immediately", async () => {
  const f = await fixture({ connectArgs: { ...args, durationSeconds: 2592000 } });
  try {
    await new Promise(resolve => setTimeout(resolve, 10));
    assert.equal((await f.rpc({ type: "status" })).body.result.desktopControl, true);
  } finally { f.controller.abort(); await f.done; }
});
