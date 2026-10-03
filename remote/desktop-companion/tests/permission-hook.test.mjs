import test from "node:test";
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { spawn } from "node:child_process";
import { mkdtemp, rmdir } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { readHookDescriptor, permissionCommand, runPermissionHook } from "../src/permission-hook.mjs";
import { writePrivateDescriptor, removeOwnedDescriptor } from "../src/descriptor.mjs";

const id = "11111111-2222-4333-8444-555555555555";
const event = { hook_event_name: "PermissionRequest", session_id: id, turn_id: "22222222-2222-4333-8444-555555555555", tool_name: "Bash", tool_input: { command: "git status" }, cwd: "/fixture" };
const decision = { hookSpecificOutput: { hookEventName: "PermissionRequest", decision: { behavior: "allow" } } };

test("only bounded official permission inputs are forwarded; other lifecycle hooks do nothing", () => {
  assert.deepEqual(permissionCommand(event), { type: "hook.permission", sessionKey: `codex:${id}`, turnId: event.turn_id, toolName: "Bash", toolInput: event.tool_input, cwd: event.cwd });
  for (const input of [{ ...event, hook_event_name: "PreToolUse" }, { ...event, session_id: "thr_123" }, { ...event, turn_id: "not-id" },
    { ...event, tool_input: { command: "x".repeat(16001) } }, { ...event, tool_name: "tool\nname" }]) assert.equal(permissionCommand(input), undefined);
});

test("hook reads only the private exact descriptor and refuses controller/out-of-scope/expired leases", async () => {
  const directory = await mkdtemp(join(tmpdir(), "salcara-hook-test-"));
  const descriptor = { version: 1, token: "a".repeat(64), port: 1234, expiresAt: Date.now() + 10000,
    controllerThreadId: "33333333-2222-4333-8444-555555555555", sessionKeys: [`codex:${id}`] };
  let path;
  try {
    path = await writePrivateDescriptor(directory, descriptor);
    assert.equal((await readHookDescriptor(directory, permissionCommand(event))).token, descriptor.token);
    await assert.rejects(readHookDescriptor(directory, { sessionKey: `codex:${descriptor.controllerThreadId}` }));
    await assert.rejects(readHookDescriptor(directory, { sessionKey: "codex:outside" }));
    await assert.rejects(readHookDescriptor(directory, permissionCommand(event), descriptor.expiresAt));
  } finally { await removeOwnedDescriptor(path, descriptor); await rmdir(directory); }
});

test("loopback hook never accepts redirects, malformed decisions or transport errors as permission", async () => {
  let reply = { ok: true, result: decision }; let status = 200;
  const bodies = [];
  const server = createServer(async (req, res) => {
    let body = ""; for await (const chunk of req) body += chunk;
    bodies.push(JSON.parse(body));
    assert.equal(req.url, "/rpc"); assert.equal(req.headers.authorization, `Bearer ${"a".repeat(64)}`);
    res.writeHead(status, { "Content-Type": "application/json", Location: "https://invalid.fixture" }); res.end(JSON.stringify(reply));
  });
  await new Promise(resolve => server.listen(0, "127.0.0.1", resolve));
  try {
    const options = { readDescriptor: async () => ({ token: "a".repeat(64), port: server.address().port }), timeoutMs: 500 };
    assert.deepEqual(await runPermissionHook(event, options), decision);
    assert.deepEqual(bodies[0], permissionCommand(event));
    for (const bad of [{ ...decision, continue: true }, { hookSpecificOutput: { hookEventName: "PreToolUse", decision: { behavior: "allow" } } },
      { hookSpecificOutput: { hookEventName: "PermissionRequest", decision: { behavior: "allow", updatedPermissions: {} } } },
      { hookSpecificOutput: { hookEventName: "PermissionRequest", decision: { behavior: "allow_session" } } }]) {
      reply = { ok: true, result: bad }; assert.deepEqual(await runPermissionHook(event, options), {});
    }
    status = 302; reply = { ok: true, result: decision }; assert.deepEqual(await runPermissionHook(event, options), {});
    assert.deepEqual(await runPermissionHook(event, { readDescriptor: async () => { throw new Error("private secret"); } }), {});
  } finally { server.closeAllConnections(); await new Promise(resolve => server.close(resolve)); }
});

test("actual command entry emits only a no-decision JSON for malformed stdin, without raw input diagnostics", async () => {
  const child = spawn(process.execPath, [fileURLToPath(new URL("../src/permission-hook.mjs", import.meta.url))], { windowsHide: true });
  let stdout = "", stderr = "";
  child.stdout.on("data", chunk => { stdout += chunk; }); child.stderr.on("data", chunk => { stderr += chunk; });
  child.stdin.end("fixture-secret-invalid-json");
  const code = await new Promise(resolve => child.on("close", resolve));
  assert.equal(code, 0); assert.equal(stdout.trim(), "{}"); assert.equal(stderr, "");
});
