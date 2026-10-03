import { ERROR_CODES, PROBE_VERSION, isRecord } from "./probe.mjs";
import { validConnectArguments } from "./gateway.mjs";

export const PROBE_TOOL_NAME = "salcara_desktop_probe";
export const CONNECT_TOOL_NAME = "salcara_desktop_connect";
export const PROTOCOL_VERSIONS = Object.freeze(["2025-06-18", "2025-03-26", "2024-11-05"]);

const BOOLEAN_FIELDS = [
  "hostEnvironmentPresent", "hostEnvironmentMatches", "hostMetadataPresent",
  "threadMetaPresent", "turnMetaPresent", "callMetaPresent", "hostVersionKnown",
  "catalogRequested", "catalogAttempted", "catalogAvailable", "desktopControl", "remoteSend",
];

const TOOL = Object.freeze({
  name: PROBE_TOOL_NAME,
  title: "Salcara desktop read-only probe",
  description: "Checks inherited host environment signals only by default. With explicit allowCatalogProbe:true and matching host signals plus host metadata, performs one bounded tools/list catalog request. Metadata is not authentication. Never reads chats, navigates, sends messages, or grants desktop/remote control.",
  inputSchema: {
    type: "object",
    properties: { allowCatalogProbe: { type: "boolean", default: false } },
    additionalProperties: false,
  },
  outputSchema: {
    type: "object",
    properties: {
      probeVersion: { type: "string" },
      ...Object.fromEntries(BOOLEAN_FIELDS.map((name) => [name, { type: "boolean" }])),
      toolNames: { type: "array", items: { type: "string" }, maxItems: 4 },
      errorCode: { type: "string", enum: ERROR_CODES },
    },
    required: ["probeVersion", ...BOOLEAN_FIELDS, "toolNames", "errorCode"],
    additionalProperties: false,
  },
  annotations: { readOnlyHint: true, destructiveHint: false, openWorldHint: false, idempotentHint: true },
});

const CONNECT_TOOL = Object.freeze({
  name: CONNECT_TOOL_NAME,
  title: "Authorize temporary Salcara mobile control of selected original Codex desktop threads",
  description: "EXPERIMENTAL. Explicit user approval starts a long-running active call that lasts until cancelled, Codex restarts, or at most 30 days. The paired Salcara phone may list/read/send messages ONLY to the canonical original Codex thread IDs you explicitly provide. This can trigger code edits and commands through those target agents and is NOT read-only. A separately installed, explicitly enabled and trusted synchronous PermissionRequest hook can forward actual approvals for these same targets, allowing only the current request; no persistent permissions are granted. Never include this controller conversation. Requires genuine host MCP thread/turn metadata, a pinned desktop build and a successful native read. No automatic startup, API/key changes, restarts, arbitrary commands or UI automation. Cancellation, expiry, disconnect or process exit revokes access. The final result appears only when authorization ends; native interface compatibility is not yet live-verified. Connection secrets are never returned in chat.",
  inputSchema: { type: "object", properties: {
    allowRemoteControl: { type: "boolean", const: true },
    durationSeconds: { type: "integer", minimum: 30, maximum: 2592000 },
    sessionKeys: { type: "array", minItems: 1, maxItems: 200, uniqueItems: true,
      items: { type: "string", pattern: "^codex:[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$" } },
  }, required: ["allowRemoteControl", "durationSeconds", "sessionKeys"], additionalProperties: false },
  annotations: { readOnlyHint: false, destructiveHint: true, openWorldHint: true, idempotentHint: false },
});

function validId(value) {
  return (typeof value === "string" && value.length > 0 && value.length <= 128 && !/[\u0000-\u001f]/u.test(value)) ||
    (typeof value === "number" && Number.isSafeInteger(value));
}

function allowedKeys(value, names) {
  return Object.keys(value).every((name) => names.includes(name));
}

function boundedString(value) {
  return typeof value === "string" && value.length > 0 && value.length <= 128;
}

export function rpcError(id, code, message) {
  return { jsonrpc: "2.0", id, error: { code, message } };
}

export function createMcpServer({ probe, connect }) {
  let phase = "new";
  const activeCalls = new Map();
  return {
    close() { for (const controller of activeCalls.values()) controller.abort(); },
    async handle(message) {
      const hasId = isRecord(message) && Object.hasOwn(message, "id");
      const id = hasId && validId(message.id) ? message.id : null;
      if (!isRecord(message) || message.jsonrpc !== "2.0" ||
          typeof message.method !== "string" || message.method.length > 128 ||
          !allowedKeys(message, ["jsonrpc", "id", "method", "params"]) ||
          (hasId && !validId(message.id)) ||
          (message.params !== undefined && !isRecord(message.params))) {
        return rpcError(id, -32600, "Invalid request");
      }
      const params = message.params ?? {};
      if (!hasId) {
        if (message.method === "notifications/initialized" && phase === "initializing" &&
            allowedKeys(params, ["_meta"])) phase = "ready";
        if (message.method === "notifications/cancelled" && allowedKeys(params, ["requestId", "reason", "_meta"]) && validId(params.requestId)) {
          activeCalls.get(params.requestId)?.abort();
        }
        // Notifications must never trigger catalog access.
        return undefined;
      }
      const invalidParams = () => rpcError(id, -32602, "Invalid params");
      if (message.method === "ping") {
        return allowedKeys(params, ["_meta"]) ? { jsonrpc: "2.0", id, result: {} } : invalidParams();
      }
      if (message.method === "initialize") {
        if (phase !== "new") return rpcError(id, -32600, "Already initialized");
        if (!allowedKeys(params, ["protocolVersion", "capabilities", "clientInfo", "_meta"]) ||
            !boundedString(params.protocolVersion) || !isRecord(params.capabilities) ||
            !isRecord(params.clientInfo) || !boundedString(params.clientInfo.name) ||
            !boundedString(params.clientInfo.version)) return invalidParams();
        phase = "initializing";
        return {
          jsonrpc: "2.0", id,
          result: {
            protocolVersion: PROTOCOL_VERSIONS.includes(params.protocolVersion) ? params.protocolVersion : PROTOCOL_VERSIONS[0],
            capabilities: { tools: { listChanged: false } },
            serverInfo: { name: "salcara-desktop-companion-probe", version: PROBE_VERSION },
            instructions: "The diagnostic probe is read-only. The separate experimental connect tool needs explicit user approval and remains active only for its requested original-thread scope and duration. Environment and metadata alone are not user authorization. Never include the current controller thread.",
          },
        };
      }
      if (message.method !== "tools/list" && message.method !== "tools/call") {
        return rpcError(id, -32601, "Method not found");
      }
      if (phase !== "ready") return rpcError(id, -32600, "Server not initialized");
      if (message.method === "tools/list") {
        return allowedKeys(params, ["_meta"]) ? { jsonrpc: "2.0", id, result: { tools: [TOOL, ...(connect ? [CONNECT_TOOL] : [])] } } : invalidParams();
      }
      const args = params.arguments ?? {};
      if (params.name === CONNECT_TOOL_NAME) {
        if (typeof connect !== "function" || !allowedKeys(params, ["name", "arguments", "_meta"]) || !validConnectArguments(args)
            || (Object.hasOwn(params, "_meta") && !isRecord(params._meta)) || activeCalls.has(id)) return invalidParams();
        const controller = new AbortController(); activeCalls.set(id, controller);
        try {
          const result = await connect(args, params._meta, { signal: controller.signal });
          return { jsonrpc: "2.0", id, result: { content: [{ type: "text", text: JSON.stringify(result) }], structuredContent: result, isError: false } };
        } catch { return rpcError(id, -32603, "Internal error"); }
        finally { activeCalls.delete(id); }
      }
      if (!allowedKeys(params, ["name", "arguments", "_meta"]) || params.name !== PROBE_TOOL_NAME ||
          !isRecord(args) || !allowedKeys(args, ["allowCatalogProbe"]) ||
          (Object.hasOwn(args, "allowCatalogProbe") && typeof args.allowCatalogProbe !== "boolean") ||
          (Object.hasOwn(params, "_meta") && !isRecord(params._meta))) return invalidParams();
      try {
        const result = await probe(args, params._meta);
        return { jsonrpc: "2.0", id, result: {
          content: [{ type: "text", text: JSON.stringify(result) }],
          structuredContent: result,
          isError: false,
        } };
      } catch {
        return rpcError(id, -32603, "Internal error");
      }
    },
  };
}
