import test from "node:test";
import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { createNativeControl, MAX_NATIVE_FRAME_BYTES } from "../src/native-control.mjs";

const pipe = String.raw`\\.\pipe\codex-browser-use-a1a1a1a1-2222-4333-8444-555555555555`;
const context = { threadId: "11111111-2222-4333-8444-555555555555", turnId: "22222222-2222-4333-8444-555555555555" };
function fixture(response = (request) => ({ jsonrpc: "2.0", id: request.id, result: { success: true,
  contentItems: [{ type: "inputText", text: '{"threadId":"target"}' }], internalSecret: "NEVER_FORWARD" } })) {
  const sent = []; let socket; let calls = 0; let sequence = 0;
  const invoke = createNativeControl({ nonce: () => `fixture-${++sequence}`, timeoutMs: 100,
    connect: (path) => {
      assert.equal(path, pipe); calls++;
      socket = new EventEmitter(); socket.destroy = () => undefined;
      socket.write = (frame) => {
        assert.equal(frame.readUInt32LE(0), frame.length - 4);
        const request = JSON.parse(frame.subarray(4)); sent.push(request);
        const answer = response(request);
        if (answer === null) return;
        const body = Buffer.from(JSON.stringify(answer)); const header = Buffer.alloc(4); header.writeUInt32LE(body.length);
        queueMicrotask(() => socket.emit("data", Buffer.concat([header, body])));
      };
      queueMicrotask(() => socket.emit("connect")); return socket;
    } });
  return { invoke, sent, calls: () => calls, socket: () => socket };
}

test("native call uses genuine supplied request context, fixed namespace/host/source and unique execution nonces", async () => {
  const f = fixture();
  const result = await f.invoke({ pipePath: pipe, context, tool: "read_thread", args: { threadId: "target" } });
  assert.deepEqual(f.sent[0].params, { namespace: "codex_app", tool: "read_thread", arguments: { threadId: "target" },
    callerSource: "codex", hostId: "local", threadId: context.threadId, turnId: context.turnId, callId: "mcp-call-fixture-2" });
  assert.deepEqual(result, { success: true, contentItems: [{ type: "inputText", text: '{"threadId":"target"}' }] });
  assert.ok(!JSON.stringify(result).includes("NEVER_FORWARD"));
  await f.invoke({ pipePath: pipe, context, tool: "list_threads", args: {} });
  assert.notEqual(f.sent[0].params.callId, f.sent[1].params.callId);
});

test("arbitrary pipe, missing context, unsupported action and revoked call never open a socket", async () => {
  const f = fixture(); const revoked = new AbortController(); revoked.abort();
  for (const options of [{ pipePath: "\\\\.\\pipe\\other" }, { context: undefined }, { context: { ...context, threadId: "fake" } },
    { tool: "navigate_to_codex_page" }, { signal: revoked.signal }]) {
    await assert.rejects(f.invoke({ pipePath: pipe, context, tool: "list_threads", args: {}, ...options }));
  }
  assert.equal(f.calls(), 0);
});

test("native permission failure, mismatched response and raw error messages fail closed", async () => {
  for (const response of [(request) => ({ jsonrpc: "2.0", id: request.id, result: { success: false, contentItems: [{ type: "inputText", text: "SECRET" }] } }),
    () => ({ jsonrpc: "2.0", id: "wrong", result: { success: true, contentItems: [] } }),
    (request) => ({ jsonrpc: "2.0", id: request.id, error: { message: "SECRET pipe path" } })]) {
    const f = fixture(response);
    await assert.rejects(f.invoke({ pipePath: pipe, context, tool: "list_threads", args: {} }), (error) => !error.message.includes("SECRET"));
  }
});

test("large frames, timeout and active cancellation have bounded fixed errors", async () => {
  const f = fixture(() => null);
  const pending = f.invoke({ pipePath: pipe, context, tool: "list_threads", args: {} });
  await new Promise((resolve) => setImmediate(resolve));
  const header = Buffer.alloc(4); header.writeUInt32LE(MAX_NATIVE_FRAME_BYTES + 1);
  f.socket().emit("data", header);
  await assert.rejects(pending, { code: "NATIVE_RESPONSE_INVALID" });
  const timed = fixture(() => null);
  await assert.rejects(timed.invoke({ pipePath: pipe, context, tool: "list_threads", args: {} }), { code: "NATIVE_CALL_TIMEOUT" });
  const cancelled = fixture(() => null); const controller = new AbortController();
  const running = cancelled.invoke({ pipePath: pipe, context, tool: "read_thread", args: {}, signal: controller.signal });
  controller.abort(); await assert.rejects(running, { code: "CONTROL_REVOKED" });
});
