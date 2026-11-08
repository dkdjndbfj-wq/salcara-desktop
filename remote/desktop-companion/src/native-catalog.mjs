import { createConnection } from "node:net";
import { ProbeError, isRecord, sanitizeToolNames } from "./probe.mjs";
import { WINDOWS_PIPE_PATTERN } from "./host-policy.mjs";

export const CATALOG_TIMEOUT_MS = 3000;
export const MAX_CATALOG_FRAME_BYTES = 1024 * 1024;
export const MAX_CATALOG_TOOLS = 256;
export const MODEL_OVERRIDE_TRANSPORT = "codex-tool-schema-v1";
const THINKING = new Set(["none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"]);

// Retain only a bounded capability projection, never schema descriptions or
// arbitrary host fields. Complex/ref schemas are unknown, not guessed support.
export function nativeSendOptions(tool) {
  if (!isRecord(tool)) return undefined;
  const schema = tool.inputSchema ?? tool.parameters;
  if (tool.inputSchema && tool.parameters && JSON.stringify(tool.inputSchema) !== JSON.stringify(tool.parameters)) return undefined;
  if (!isRecord(schema) || schema.type !== "object" || !isRecord(schema.properties) ||
      Object.keys(schema).some(key => !["type", "properties", "required", "description", "title", "$schema", "additionalProperties"].includes(key)) ||
      (Object.hasOwn(schema, "additionalProperties") && typeof schema.additionalProperties !== "boolean") ||
      ["anyOf", "oneOf", "allOf", "$ref", "not", "if"].some(key => Object.hasOwn(schema, key)) ||
      !Array.isArray(schema.required) || !schema.required.includes("threadId") || !schema.required.includes("prompt") ||
      schema.required.some(key => !["threadId", "prompt"].includes(key))) return undefined;
  const simpleString = value => isRecord(value) && value.type === "string" &&
    Object.keys(value).every(key => ["type", "description", "title", "enum"].includes(key)) &&
    (!Object.hasOwn(value, "enum") || Array.isArray(value.enum) && value.enum.length > 0 && value.enum.length <= 256 &&
      value.enum.every(item => typeof item === "string" && item.length <= 200) && new Set(value.enum).size === value.enum.length);
  if (!simpleString(schema.properties.threadId) || !simpleString(schema.properties.prompt) || !simpleString(schema.properties.model)) return undefined;
  // A constrained model list needs a separate value contract. Do not advertise
  // arbitrary model IDs on an enum-only or constrained host implementation.
  if (["threadId", "prompt", "model"].some(key => Object.hasOwn(schema.properties[key], "enum"))) return undefined;
  const thinking = schema.properties.thinking;
  return { model: true, thinking: simpleString(thinking) && Array.isArray(thinking.enum) && thinking.enum.length > 0 && thinking.enum.length <= THINKING.size &&
    thinking.enum.every(value => typeof value === "string" && THINKING.has(value)) && new Set(thinking.enum).size === thinking.enum.length ? [...thinking.enum] : [] };
}

const REQUEST = Buffer.from(JSON.stringify({
  jsonrpc: "2.0",
  id: 1,
  method: "tools/list",
  params: { threadStartKind: "all" },
}), "utf8");
const HEADER = Buffer.alloc(4);
HEADER.writeUInt32LE(REQUEST.length);
const REQUEST_FRAME = Buffer.concat([HEADER, REQUEST]);

function decodeCatalog(body, includeSendOptions) {
  let message;
  try {
    message = JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(body));
  } catch {
    throw new ProbeError("CATALOG_RESPONSE_INVALID");
  }
  if (!isRecord(message) || message.jsonrpc !== "2.0" || message.id !== 1 ||
      Object.hasOwn(message, "error") || !isRecord(message.result) ||
      !Array.isArray(message.result.tools) || message.result.tools.length > MAX_CATALOG_TOOLS) {
    throw new ProbeError("CATALOG_RESPONSE_INVALID");
  }
  const names = [];
  let sendTool; let sendCount = 0;
  for (const tool of message.result.tools) {
    if (!isRecord(tool) || typeof tool.name !== "string" || tool.name.length > 128 ||
        typeof tool.namespace !== "string" || tool.namespace.length > 128) {
      throw new ProbeError("CATALOG_RESPONSE_INVALID");
    }
    if (tool.namespace === "codex_app") {
      names.push(tool.name);
      if (tool.name === "send_message_to_thread") { sendTool = tool; sendCount++; }
    }
  }
  const safeNames = sanitizeToolNames(names);
  return includeSendOptions ? { names: safeNames, sendOptions: sendCount === 1 ? nativeSendOptions(sendTool) : undefined } : safeNames;
}

// This client can send exactly one catalog request. It has no initialization,
// tools/call, cancellation, HTTP, or session-control implementation.
export function createNativeCatalog({
  connect = createConnection,
  setTimer = setTimeout,
  clearTimer = clearTimeout,
} = {}) {
  return ({ pipePath, includeSendOptions = false }) => new Promise((resolve, reject) => {
    let socket;
    let timer;
    let finished = false;
    let connected = false;
    let frameLength;
    let buffer = Buffer.alloc(0);

    const finish = (error, names) => {
      if (finished) return;
      finished = true;
      if (timer !== undefined) clearTimer(timer);
      // Keep the generic error listener attached during destroy; late socket
      // errors must not become uncaught errors containing a local pipe path.
      try { socket?.destroy(); } catch { /* No host error text leaves the probe. */ }
      if (error) reject(error);
      else resolve(names);
    };
    try {
      if (typeof pipePath !== "string" || pipePath.length > 256 || !WINDOWS_PIPE_PATTERN.test(pipePath)) {
        finish(new ProbeError("CATALOG_UNAVAILABLE"));
        return;
      }
      socket = connect(pipePath);
      if (!socket || typeof socket.on !== "function" || typeof socket.write !== "function" ||
          typeof socket.destroy !== "function") {
        finish(new ProbeError("CATALOG_UNAVAILABLE"));
        return;
      }
      socket.on("error", () => finish(new ProbeError("CATALOG_FAILED")));
      socket.on("close", () => finish(new ProbeError("CATALOG_FAILED")));
      socket.on("end", () => finish(new ProbeError("CATALOG_FAILED")));
      socket.on("connect", () => {
        if (finished || connected) return;
        connected = true;
        try { socket.write(REQUEST_FRAME); }
        catch { finish(new ProbeError("CATALOG_FAILED")); }
      });
      socket.on("data", (chunk) => {
        if (finished) return;
        if (!connected || !Buffer.isBuffer(chunk) ||
            buffer.length + chunk.length > MAX_CATALOG_FRAME_BYTES + 4) {
          finish(new ProbeError("CATALOG_FRAME_INVALID"));
          return;
        }
        buffer = Buffer.concat([buffer, chunk]);
        if (frameLength === undefined && buffer.length >= 4) {
          frameLength = buffer.readUInt32LE(0);
          if (frameLength === 0 || frameLength > MAX_CATALOG_FRAME_BYTES) {
            finish(new ProbeError("CATALOG_FRAME_INVALID"));
            return;
          }
        }
        if (frameLength !== undefined && buffer.length >= frameLength + 4) {
          if (buffer.length !== frameLength + 4) {
            finish(new ProbeError("CATALOG_FRAME_INVALID"));
            return;
          }
          try { finish(undefined, decodeCatalog(buffer.subarray(4), includeSendOptions)); }
          catch (error) { finish(error instanceof ProbeError ? error : new ProbeError("CATALOG_FAILED")); }
        }
      });
      timer = setTimer(() => finish(new ProbeError("CATALOG_TIMEOUT")), CATALOG_TIMEOUT_MS);
    } catch {
      finish(new ProbeError("CATALOG_FAILED"));
    }
  });
}
