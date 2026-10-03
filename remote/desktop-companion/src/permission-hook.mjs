import { request } from "node:http";
import { open, lstat } from "node:fs/promises";
import { constants } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { DESCRIPTOR_NAME, ensureOwnDirectory } from "./descriptor.mjs";
import { isRecord } from "./probe.mjs";

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const keys = (value, names) => Object.keys(value).every(key => names.includes(key));

export function permissionCommand(input) {
  if (!isRecord(input) || input.hook_event_name !== "PermissionRequest" || !UUID.test(input.session_id ?? "")
      || !UUID.test(input.turn_id ?? "") || typeof input.tool_name !== "string" || !/^[A-Za-z0-9_.:-]{1,160}$/.test(input.tool_name)
      || !isRecord(input.tool_input) || typeof input.cwd !== "string" || input.cwd.length > 4096
      || Buffer.byteLength(JSON.stringify(input.tool_input)) > 16000) return undefined;
  return { type: "hook.permission", sessionKey: `codex:${input.session_id}`, turnId: input.turn_id,
    toolName: input.tool_name, toolInput: input.tool_input, cwd: input.cwd };
}

export async function readHookDescriptor(directory, command, now = Date.now()) {
  const path = join(await ensureOwnDirectory(directory), DESCRIPTOR_NAME);
  const before = await lstat(path);
  if (!before.isFile() || before.isSymbolicLink() || before.size > 8192 || before.size <= 0
      || process.platform !== "win32" && before.mode & 0o077) throw new Error("unavailable");
  const file = await open(path, constants.O_RDONLY | (constants.O_NOFOLLOW ?? 0));
  let data;
  try {
    const actual = await file.stat();
    if (!actual.isFile() || actual.dev !== before.dev || actual.ino !== before.ino || actual.size > 8192) throw new Error("unavailable");
    data = JSON.parse(await file.readFile("utf8"));
  } finally { await file.close(); }
  if (!isRecord(data) || !keys(data, ["version", "port", "token", "expiresAt", "controllerThreadId", "sessionKeys"])
      || data.version !== 1 || !Number.isInteger(data.port) || data.port < 1 || data.port > 65535 || !/^[a-f0-9]{64}$/.test(data.token ?? "")
      || !Number.isSafeInteger(data.expiresAt) || data.expiresAt <= now || data.expiresAt - now > 2592000000
      || !UUID.test(data.controllerThreadId ?? "") || !Array.isArray(data.sessionKeys) || !data.sessionKeys.includes(command.sessionKey)
      || command.sessionKey === `codex:${data.controllerThreadId}`) throw new Error("unavailable");
  return data;
}

function safeDecision(value) {
  if (!isRecord(value) || !keys(value, ["hookSpecificOutput"]) || !isRecord(value.hookSpecificOutput)) return {};
  const output = value.hookSpecificOutput;
  if (!keys(output, ["hookEventName", "decision"]) || output.hookEventName !== "PermissionRequest" || !isRecord(output.decision)) return {};
  const decision = output.decision;
  if (!keys(decision, ["behavior", "message"]) || !["allow", "deny"].includes(decision.behavior)
      || decision.message !== undefined && (typeof decision.message !== "string" || decision.message.length > 2000)) return {};
  return { hookSpecificOutput: { hookEventName: "PermissionRequest", decision: { behavior: decision.behavior,
    ...(decision.behavior === "deny" && decision.message ? { message: decision.message } : {}) } } };
}

/** Private loopback only. No credentials/diagnostics go to stdout or the model. */
export async function runPermissionHook(input, { directory = dirname(dirname(fileURLToPath(import.meta.url))), readDescriptor = readHookDescriptor, timeoutMs = 115000 } = {}) {
  const command = permissionCommand(input);
  if (!command) return {};
  try {
    const descriptor = await readDescriptor(directory, command);
    return await new Promise(resolve => {
      let done = false;
      let timer;
      const finish = value => { if (done) return; done = true; clearTimeout(timer); resolve(value); };
      const req = request({ hostname: "127.0.0.1", port: descriptor.port, path: "/rpc", method: "POST", agent: false,
        headers: { "Content-Type": "application/json", Authorization: `Bearer ${descriptor.token}` } }, response => {
        const chunks = []; let length = 0;
        response.on("error", () => finish({}));
        response.on("aborted", () => finish({}));
        response.on("data", chunk => { length += chunk.length; if (length > 4096) { response.destroy(); finish({}); } else chunks.push(chunk); });
        response.on("end", () => {
          try {
            const reply = JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(Buffer.concat(chunks)));
            finish(response.statusCode === 200 && reply.ok === true ? safeDecision(reply.result) : {});
          } catch { finish({}); }
        });
      });
      req.on("error", () => finish({}));
      timer = setTimeout(() => { req.destroy(); finish({}); }, timeoutMs);
      req.end(JSON.stringify(command));
    });
  } catch { return {}; }
}

// Imported by tests, otherwise a fixed installer-owned command hook. A failed
// hook returns no decision, so Codex retains its ordinary approval prompt.
if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  let length = 0; const chunks = [];
  try {
    for await (const chunk of process.stdin) { length += chunk.length; if (length > 65536) throw new Error("large"); chunks.push(chunk); }
    const input = JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(Buffer.concat(chunks)));
    process.stdout.write(`${JSON.stringify(await runPermissionHook(input))}\n`);
  } catch { process.stdout.write("{}\n"); }
}
