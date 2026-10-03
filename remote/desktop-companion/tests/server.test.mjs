import test from "node:test";
import assert from "node:assert/strict";
import { PassThrough, Readable, Writable } from "node:stream";
import { createMcpServer, CONNECT_TOOL_NAME, PROBE_TOOL_NAME } from "../src/server.mjs";
import { createProbe } from "../src/probe.mjs";
import { runStdio, MAX_REQUEST_BYTES } from "../src/stdio.mjs";

const initialize = { jsonrpc: "2.0", id: 1, method: "initialize", params: {
  protocolVersion: "2025-06-18", capabilities: {}, clientInfo: { name: "fake-test-client", version: "1" },
} };
const initialized = { jsonrpc: "2.0", method: "notifications/initialized" };
const call = { jsonrpc: "2.0", id: 3, method: "tools/call", params: { name: PROBE_TOOL_NAME, arguments: {} } };

async function ready(probe = createProbe()) {
  const server = createMcpServer({ probe });
  await server.handle(initialize);
  await server.handle(initialized);
  return server;
}

test("MCP lifecycle advertises one read-only tool and no session control", async () => {
  const server = createMcpServer({ probe: createProbe() });
  assert.deepEqual((await server.handle({ jsonrpc: "2.0", id: "ping", method: "ping" })).result, {});
  assert.equal((await server.handle(call)).error.code, -32600);
  const init = await server.handle(initialize);
  assert.equal(init.result.protocolVersion, "2025-06-18");
  assert.equal((await server.handle(call)).error.code, -32600);
  assert.equal(await server.handle(initialized), undefined);
  const list = await server.handle({ jsonrpc: "2.0", id: 2, method: "tools/list" });
  assert.deepEqual(list.result.tools.map((tool) => tool.name), [PROBE_TOOL_NAME]);
  assert.deepEqual(list.result.tools[0].annotations, { readOnlyHint: true, destructiveHint: false, openWorldHint: false, idempotentHint: true });
  const response = await server.handle(call);
  assert.equal(response.result.structuredContent.desktopControl, false);
  assert.equal(response.result.structuredContent.remoteSend, false);
  assert.equal(response.result.structuredContent.catalogAttempted, false);
  assert.deepEqual(JSON.parse(response.result.content[0].text), response.result.structuredContent);
});

test("only host params._meta is forwarded, never arguments metadata", async () => {
  const hostMeta = { "openai/threadId": "REAL_HOST_META" };
  let seen;
  const server = await ready(async (args, meta) => { seen = { args, meta }; return { desktopControl: false, remoteSend: false }; });
  await server.handle({ ...call, params: { ...call.params, _meta: hostMeta, arguments: { allowCatalogProbe: true } } });
  assert.deepEqual(seen, { args: { allowCatalogProbe: true }, meta: hostMeta });
  const response = await server.handle({ ...call, params: { ...call.params, arguments: { allowCatalogProbe: true, _meta: hostMeta } } });
  assert.equal(response.error.code, -32602);
});

test("unknown methods/tools, argument paths and caller identities never invoke probe", async () => {
  let calls = 0;
  const server = await ready(async () => { calls++; return {}; });
  for (const request of [
    { ...call, method: "list_threads" },
    { ...call, method: "resources/read" },
    { ...call, params: { name: "send_message_to_thread" } },
    { ...call, params: { ...call.params, arguments: { allowCatalogProbe: "true" } } },
    { ...call, params: { ...call.params, arguments: { pipePath: "SECRET" } } },
    { ...call, params: { ...call.params, arguments: { callerSource: "codex", threadId: "SECRET" } } },
    { ...call, params: { ...call.params, _meta: "SECRET" } },
  ]) assert.ok((await server.handle(request)).error);
  assert.equal(calls, 0);
});

test("notifications cannot initiate a tool call", async () => {
  let calls = 0;
  const server = await ready(async () => { calls++; return {}; });
  const notification = { ...call, params: { ...call.params, arguments: { allowCatalogProbe: true } } };
  delete notification.id;
  assert.equal(await server.handle(notification), undefined);
  assert.equal(calls, 0);
});

test("request and ID boundaries reject batches, oversized IDs and malformed shapes", async () => {
  const server = await ready();
  for (const request of [[], [call], null, { ...call, jsonrpc: "1.0" }, { ...call, id: null },
    { ...call, id: "x".repeat(129) }, { ...call, id: 1.2 }, { ...call, id: Number.MAX_SAFE_INTEGER + 1 },
    { ...call, params: [] }, { ...call, secret: "SECRET" }, { ...call, method: 1 }, { ...call, id: "bad\nID" }]) {
    const response = await server.handle(request);
    assert.equal(response.error.code, -32600);
    assert.ok(!JSON.stringify(response).includes("SECRET"));
  }
});

test("unknown protocol is negotiated and duplicate initialization rejected", async () => {
  const server = createMcpServer({ probe: createProbe() });
  assert.equal((await server.handle({ ...initialize, params: { ...initialize.params, protocolVersion: "9999-01-01" } })).result.protocolVersion, "2025-06-18");
  assert.equal((await server.handle(initialize)).error.code, -32600);
});

test("unexpected internal errors are generic", async () => {
  const server = await ready(async () => { throw new Error("SECRET exception path and key"); });
  const response = await server.handle(call);
  assert.deepEqual(response.error, { code: -32603, message: "Internal error" });
  assert.ok(!JSON.stringify(response).includes("SECRET"));
});

async function transport(chunks, server = createMcpServer({ probe: createProbe() })) {
  let stdout = "";
  const output = new Writable({ write(chunk, encoding, done) { stdout += chunk.toString(); done(); } });
  await runStdio({ input: Readable.from(chunks), output, server });
  return stdout.split("\n").filter(Boolean).map((line) => JSON.parse(line));
}

test("stdio emits only JSON-RPC, handles split UTF-8/CRLF and never echoes client data", async () => {
  const payload = [initialize, initialized, { jsonrpc: "2.0", id: 2, method: "tools/list" }, call];
  payload[0] = { ...initialize, params: { ...initialize.params, clientInfo: { name: "假客户端 SECRET CLIENT", version: "1" } } };
  const buffer = Buffer.from(`${payload.map((m) => JSON.stringify(m)).join("\r\n")}\r\n`);
  const chunks = Array.from({ length: buffer.length }, (_, i) => buffer.subarray(i, i + 1));
  const responses = await transport(chunks);
  assert.equal(responses.length, 3);
  assert.ok(responses.every((m) => m.jsonrpc === "2.0"));
  assert.ok(!JSON.stringify(responses).includes("SECRET CLIENT"));
});

test("parse errors and overlong lines resync with bounded generic output", async () => {
  const responses = await transport([
    Buffer.from("SECRET invalid json\n"),
    Buffer.alloc(MAX_REQUEST_BYTES + 100, 120),
    Buffer.from("\n"),
    Buffer.from(`${JSON.stringify({ jsonrpc: "2.0", id: 5, method: "ping" })}\n`),
    Buffer.from([0xc3, 0x28, 0x0a]),
  ]);
  assert.equal(responses.length, 4);
  assert.equal(responses[0].error.code, -32700);
  assert.equal(responses[1].error.message, "Request too large");
  assert.deepEqual(responses[2], { jsonrpc: "2.0", id: 5, result: {} });
  assert.equal(responses[3].error.code, -32700);
  assert.ok(!JSON.stringify(responses).includes("SECRET"));
});

test("connect is explicitly mutable, uses only host metadata and is revoked by MCP cancellation", async () => {
  const hostMeta = { "openai/threadId": "11111111-2222-4333-8444-555555555555",
    "openai/turnId": "44444444-2222-4333-8444-555555555555" };
  const argumentsValue = { allowRemoteControl: true, durationSeconds: 30,
    sessionKeys: ["codex:22222222-2222-4333-8444-555555555555"] };
  let seen;
  const server = createMcpServer({ probe: createProbe(), connect: async (args, meta, { signal }) => {
    seen = { args, meta };
    await new Promise((resolve) => signal.addEventListener("abort", resolve, { once: true }));
    return { ended: true, desktopControl: false, remoteSend: false };
  } });
  await server.handle(initialize); await server.handle(initialized);
  const list = await server.handle({ jsonrpc: "2.0", id: 2, method: "tools/list" });
  assert.deepEqual(list.result.tools.map((tool) => tool.name), [PROBE_TOOL_NAME, CONNECT_TOOL_NAME]);
  assert.equal(list.result.tools[1].annotations.readOnlyHint, false);
  const pending = server.handle({ jsonrpc: "2.0", id: 4, method: "tools/call", params: {
    name: CONNECT_TOOL_NAME, arguments: argumentsValue, _meta: hostMeta,
  } });
  assert.deepEqual(seen, { args: argumentsValue, meta: hostMeta });
  assert.equal(await server.handle({ jsonrpc: "2.0", method: "notifications/cancelled", params: { requestId: 4 } }), undefined);
  assert.equal((await pending).result.structuredContent.desktopControl, false);
});

test("stdio consumes cancellation while connect is pending and EOF revokes the active call", async () => {
  for (const cancelled of [true, false]) {
    const input = new PassThrough(); let stdout = ""; let entered;
    const active = new Promise((resolve) => { entered = resolve; });
    const output = new Writable({ write(chunk, _encoding, done) { stdout += chunk.toString(); done(); } });
    const server = createMcpServer({ probe: createProbe(), connect: async (_args, _meta, { signal }) => {
      entered(); await new Promise((resolve) => signal.addEventListener("abort", resolve, { once: true }));
      return { ended: true, desktopControl: false, remoteSend: false };
    } });
    const transportDone = runStdio({ input, output, server });
    input.write(`${JSON.stringify(initialize)}\n${JSON.stringify(initialized)}\n${JSON.stringify({ jsonrpc: "2.0", id: 4,
      method: "tools/call", params: { name: CONNECT_TOOL_NAME, arguments: { allowRemoteControl: true,
        durationSeconds: 30, sessionKeys: ["codex:22222222-2222-4333-8444-555555555555"] } } })}\n`);
    await active;
    if (cancelled) input.write(`${JSON.stringify({ jsonrpc: "2.0", method: "notifications/cancelled", params: { requestId: 4 } })}\n`);
    input.end(); await transportDone;
    const replies = stdout.trim().split("\n").map((line) => JSON.parse(line));
    assert.equal(replies.find((reply) => reply.id === 4).result.structuredContent.ended, true);
  }
});
