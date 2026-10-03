import { randomUUID } from "node:crypto";
import { createConnection } from "node:net";
import { WINDOWS_PIPE_PATTERN } from "./host-policy.mjs";
import { isRecord } from "./probe.mjs";

export const NATIVE_CALL_TIMEOUT_MS = 30000;
export const MAX_NATIVE_FRAME_BYTES = 8 * 1024 * 1024;
const TOOLS = new Set(["list_threads", "read_thread", "send_message_to_thread"]);
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

export class DesktopError extends Error {
  constructor(code = "NATIVE_CALL_FAILED") { super(code); this.name = "DesktopError"; this.code = code; }
}

/** Original, bounded client for the experimentally observed version-specific transport. */
export function createNativeControl({ connect = createConnection, nonce = randomUUID, timeoutMs = NATIVE_CALL_TIMEOUT_MS } = {}) {
  return ({ pipePath, context, tool, args, signal }) => new Promise((resolve, reject) => {
    if (!WINDOWS_PIPE_PATTERN.test(pipePath ?? "") || !context || !UUID.test(context.threadId) || !UUID.test(context.turnId)
        || !TOOLS.has(tool) || !isRecord(args)) { reject(new DesktopError("NATIVE_CONTEXT_INVALID")); return; }
    if (signal?.aborted) { reject(new DesktopError("CONTROL_REVOKED")); return; }
    const id = `salcara-${nonce()}`;
    const body = Buffer.from(JSON.stringify({ jsonrpc: "2.0", id, method: "tools/call", params: {
      namespace: "codex_app", tool, arguments: args, callerSource: "codex", hostId: "local",
      threadId: context.threadId, turnId: context.turnId, callId: `mcp-call-${nonce()}`,
    } }));
    if (body.length > 64 * 1024) { reject(new DesktopError("REQUEST_TOO_LARGE")); return; }
    const header = Buffer.alloc(4); header.writeUInt32LE(body.length);
    let socket; let timer; let finished = false; let connected = false; let buffer = Buffer.alloc(0); let length;
    const finish = (error, result) => {
      if (finished) return; finished = true;
      clearTimeout(timer); signal?.removeEventListener("abort", abort);
      try { socket?.destroy(); } catch { /* No transport diagnostics leave this process. */ }
      error ? reject(error) : resolve(result);
    };
    const abort = () => finish(new DesktopError("CONTROL_REVOKED"));
    try {
      socket = connect(pipePath);
      socket.on("error", () => finish(new DesktopError("NATIVE_CALL_FAILED")));
      socket.on("end", () => finish(new DesktopError("NATIVE_CALL_FAILED")));
      socket.on("close", () => finish(new DesktopError("NATIVE_CALL_FAILED")));
      socket.on("connect", () => {
        if (finished || connected) return; connected = true;
        try { socket.write(Buffer.concat([header, body])); } catch { finish(new DesktopError("NATIVE_CALL_FAILED")); }
      });
      socket.on("data", (chunk) => {
        if (finished) return;
        if (!connected || !Buffer.isBuffer(chunk) || buffer.length + chunk.length > MAX_NATIVE_FRAME_BYTES + 4) {
          finish(new DesktopError("NATIVE_RESPONSE_INVALID")); return;
        }
        buffer = Buffer.concat([buffer, chunk]);
        if (length === undefined && buffer.length >= 4) {
          length = buffer.readUInt32LE(0);
          if (!length || length > MAX_NATIVE_FRAME_BYTES) { finish(new DesktopError("NATIVE_RESPONSE_INVALID")); return; }
        }
        if (length !== undefined && buffer.length >= length + 4) {
          if (buffer.length !== length + 4) { finish(new DesktopError("NATIVE_RESPONSE_INVALID")); return; }
          let message;
          try { message = JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(buffer.subarray(4))); }
          catch { finish(new DesktopError("NATIVE_RESPONSE_INVALID")); return; }
          if (!isRecord(message) || message.jsonrpc !== "2.0" || message.id !== id || Object.hasOwn(message, "error")
              || !isRecord(message.result) || message.result.success !== true || !Array.isArray(message.result.contentItems)) {
            finish(new DesktopError("NATIVE_CALL_REJECTED")); return;
          }
          const contentItems = message.result.contentItems.filter((item) => isRecord(item) && item.type === "inputText" && typeof item.text === "string")
            .map((item) => ({ type: "inputText", text: item.text }));
          if (!contentItems.length) { finish(new DesktopError("NATIVE_RESPONSE_INVALID")); return; }
          finish(undefined, { success: true, contentItems });
        }
      });
      timer = setTimeout(() => finish(new DesktopError("NATIVE_CALL_TIMEOUT")), timeoutMs);
      signal?.addEventListener("abort", abort, { once: true });
      if (signal?.aborted) abort();
    } catch { finish(new DesktopError("NATIVE_CALL_FAILED")); }
  });
}
