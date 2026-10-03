import test from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";

const entry = fileURLToPath(new URL("../src/index.mjs", import.meta.url));

async function invoke(args, input) {
  // A fake empty environment prevents access to any real desktop host signals.
  const child = spawn(process.execPath, [entry, ...args], { env: {}, stdio: ["pipe", "pipe", "pipe"], windowsHide: true });
  let stdout = "";
  let stderr = "";
  child.stdout.setEncoding("utf8");
  child.stderr.setEncoding("utf8");
  child.stdout.on("data", (data) => { stdout += data; });
  child.stderr.on("data", (data) => { stderr += data; });
  const closed = new Promise((resolve, reject) => {
    child.once("error", reject);
    child.once("close", (code) => resolve(code));
  });
  child.stdin.on("error", () => { /* The override rejection may close stdin immediately. */ });
  child.stdin.end(input);
  return { code: await closed, stdout, stderr };
}

test("entry stdio handshake and default tool work in an isolated fake environment", { timeout: 5000 }, async () => {
  const messages = [
    { jsonrpc: "2.0", id: 1, method: "initialize", params: { protocolVersion: "2025-06-18", capabilities: {}, clientInfo: { name: "fake", version: "1" } } },
    { jsonrpc: "2.0", method: "notifications/initialized" },
    { jsonrpc: "2.0", id: 2, method: "tools/list" },
    { jsonrpc: "2.0", id: 3, method: "tools/call", params: { name: "salcara_desktop_probe", arguments: {} } },
  ];
  const result = await invoke([], `${messages.map((message) => JSON.stringify(message)).join("\n")}\n`);
  assert.equal(result.code, 0);
  assert.equal(result.stderr, "");
  const responses = result.stdout.trim().split("\n").map((line) => JSON.parse(line));
  assert.equal(responses.length, 3);
  assert.ok(responses.every((response) => response.jsonrpc === "2.0"));
  assert.deepEqual(responses[1].result.tools.map((tool) => tool.name), ["salcara_desktop_probe", "salcara_desktop_connect"]);
  const probe = responses[2].result.structuredContent;
  assert.equal(probe.hostEnvironmentPresent, false);
  assert.equal(probe.catalogAttempted, false);
  assert.equal(probe.desktopControl, false);
  assert.equal(probe.remoteSend, false);
});

test("entry rejects command-line pipe/caller overrides without output or connection", { timeout: 5000 }, async () => {
  const result = await invoke(["--pipe", "SECRET_ARBITRARY_PIPE"], "");
  assert.equal(result.code, 1);
  assert.equal(result.stdout, "");
  assert.equal(result.stderr, "");
});
