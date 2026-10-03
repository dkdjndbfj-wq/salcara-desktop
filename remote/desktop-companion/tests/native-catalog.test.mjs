import test from "node:test";
import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { createNativeCatalog, MAX_CATALOG_FRAME_BYTES, MAX_CATALOG_TOOLS, CATALOG_TIMEOUT_MS } from "../src/native-catalog.mjs";

const pipe = String.raw`\\.\pipe\codex-browser-use-a1a1a1a1-2222-4333-8444-555555555555`;

class FakeSocket extends EventEmitter {
  writes = [];
  destroyCount = 0;
  write(frame) { this.writes.push(Buffer.from(frame)); }
  destroy() { this.destroyCount++; this.emit("close"); }
}

function fixture() {
  const socket = new FakeSocket();
  let timer;
  let timerCleared = false;
  let connections = 0;
  const catalog = createNativeCatalog({
    connect: (path) => { connections++; assert.equal(path, pipe); return socket; },
    setTimer: (callback, ms) => { assert.equal(ms, CATALOG_TIMEOUT_MS); timer = callback; return 123; },
    clearTimer: (id) => { assert.equal(id, 123); timerCleared = true; },
  });
  return { socket, catalog, timeout: () => timer(), state: () => ({ connections, timerCleared }) };
}

function frame(message) {
  const body = Buffer.from(JSON.stringify(message));
  const header = Buffer.alloc(4);
  header.writeUInt32LE(body.length);
  return Buffer.concat([header, body]);
}

const response = { jsonrpc: "2.0", id: 1, result: { tools: [
  { name: "list_threads", namespace: "codex_app", description: "SECRET DESCRIPTION", inputSchema: { secret: "SECRET SCHEMA" } },
  { name: "send_message_to_thread", namespace: "codex_app" },
  { name: "private_unknown_tool", namespace: "codex_app" },
  { name: "read_thread", namespace: "some_other_namespace" },
] } };

test("native probe writes one tools/list request only, and sanitizes catalog", async () => {
  const f = fixture();
  const result = f.catalog({ pipePath: pipe });
  assert.equal(f.socket.writes.length, 0);
  f.socket.emit("connect");
  f.socket.emit("connect");
  assert.equal(f.socket.writes.length, 1);
  const request = f.socket.writes[0];
  assert.equal(request.readUInt32LE(0), request.length - 4);
  assert.deepEqual(JSON.parse(request.subarray(4)), {
    jsonrpc: "2.0", id: 1, method: "tools/list", params: { threadStartKind: "all" },
  });
  const incoming = frame(response);
  for (const [start, end] of [[0, 2], [2, 4], [4, 11], [11, incoming.length]]) f.socket.emit("data", incoming.subarray(start, end));
  assert.deepEqual(await result, ["list_threads", "send_message_to_thread"]);
  assert.equal(f.socket.destroyCount, 1);
  assert.deepEqual(f.state(), { connections: 1, timerCleared: true });
  f.socket.emit("error", new Error("late SECRET path"));
  assert.equal(f.socket.writes.length, 1);
});

test("timeout destroys socket without cancellation or other requests", async () => {
  const f = fixture();
  const result = f.catalog({ pipePath: pipe });
  f.socket.emit("connect");
  f.timeout();
  await assert.rejects(result, (error) => error.code === "CATALOG_TIMEOUT" && error.message === "CATALOG_TIMEOUT");
  assert.equal(f.socket.destroyCount, 1);
  assert.equal(f.socket.writes.length, 1);
});

for (const [name, mutate] of [
  ["wrong id", (m) => ({ ...m, id: 2 })],
  ["host error", () => ({ jsonrpc: "2.0", id: 1, error: { message: "SECRET HOST ERROR", data: pipe } })],
  ["too many tools", () => ({ jsonrpc: "2.0", id: 1, result: { tools: Array.from({ length: MAX_CATALOG_TOOLS + 1 }, () => ({ name: "list_threads", namespace: "codex_app" })) } })],
  ["no namespace", () => ({ jsonrpc: "2.0", id: 1, result: { tools: [{ name: "list_threads" }] } })],
  ["invalid tool", () => ({ jsonrpc: "2.0", id: 1, result: { tools: [null] } })],
  ["wrong result", () => ({ jsonrpc: "2.0", id: 1, result: { chats: ["SECRET CHAT"] } })],
]) {
  test(`invalid catalog response: ${name}`, async () => {
    const f = fixture();
    const result = f.catalog({ pipePath: pipe });
    f.socket.emit("connect");
    f.socket.emit("data", frame(mutate(response)));
    await assert.rejects(result, (error) => error.code === "CATALOG_RESPONSE_INVALID" && !error.message.includes("SECRET"));
    assert.equal(f.socket.destroyCount, 1);
    assert.equal(f.socket.writes.length, 1);
  });
}

test("invalid UTF-8 and invalid JSON return fixed response error", async () => {
  for (const body of [Buffer.from([0xc3, 0x28]), Buffer.from("SECRET invalid json")]) {
    const f = fixture();
    const result = f.catalog({ pipePath: pipe });
    f.socket.emit("connect");
    const header = Buffer.alloc(4); header.writeUInt32LE(body.length);
    f.socket.emit("data", Buffer.concat([header, body]));
    await assert.rejects(result, { code: "CATALOG_RESPONSE_INVALID" });
  }
});

test("zero, excessive, trailing and unsolicited frames fail closed", async () => {
  const tooLarge = Buffer.alloc(4); tooLarge.writeUInt32LE(MAX_CATALOG_FRAME_BYTES + 1);
  for (const [connected, data] of [[true, Buffer.alloc(4)], [true, tooLarge], [true, Buffer.concat([frame(response), Buffer.from("SECRET")])], [false, frame(response)]]) {
    const f = fixture();
    const result = f.catalog({ pipePath: pipe });
    if (connected) f.socket.emit("connect");
    f.socket.emit("data", data);
    await assert.rejects(result, { code: "CATALOG_FRAME_INVALID" });
    assert.equal(f.socket.destroyCount, 1);
  }
});

test("raw connection and socket errors never escape", async () => {
  const f = fixture();
  const result = f.catalog({ pipePath: pipe });
  f.socket.emit("error", new Error("SECRET host error with a key"));
  await assert.rejects(result, (error) => error.code === "CATALOG_FAILED" && error.message === "CATALOG_FAILED");
  const throwing = createNativeCatalog({ connect: () => { throw new Error("SECRET connect error"); } });
  await assert.rejects(throwing({ pipePath: pipe }), { code: "CATALOG_FAILED", message: "CATALOG_FAILED" });
});

test("socket closes before complete response fail without raw content", async () => {
  const f = fixture();
  const result = f.catalog({ pipePath: pipe });
  f.socket.emit("connect");
  f.socket.emit("data", frame(response).subarray(0, 12));
  f.socket.emit("end");
  await assert.rejects(result, { code: "CATALOG_FAILED" });
  assert.equal(f.socket.writes.length, 1);
});

test("lower-level catalog refuses arbitrary paths without invoking connect", async () => {
  let connections = 0;
  const catalog = createNativeCatalog({ connect: () => { connections++; throw new Error("must not connect"); } });
  for (const pipePath of ["", "SECRET", String.raw`\\.\pipe\other`, `${pipe}-extra`, pipe.replace("4333", "7333")]) {
    await assert.rejects(catalog({ pipePath }), { code: "CATALOG_UNAVAILABLE" });
  }
  assert.equal(connections, 0);
});
