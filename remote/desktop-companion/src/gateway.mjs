import { createHash, randomBytes, timingSafeEqual } from "node:crypto";
import { createServer } from "node:http";
import { extractHostContext } from "./host-policy.mjs";
import { removeOwnedDescriptor, writePrivateDescriptor } from "./descriptor.mjs";
import { createOperationLedger } from "./operation-ledger.mjs";
import { APPROVAL_TRANSPORT, createPermissionBroker } from "./permission-broker.mjs";
import { createHistoryCursors } from "./history-cursor.mjs";
import { DesktopError } from "./native-control.mjs";
import { isRecord } from "./probe.mjs";

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const SESSION = /^codex:[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const ERRORS = new Set(["CONTROL_ARGUMENTS_INVALID", "HOST_CONTEXT_MISSING", "HOST_ENVIRONMENT_UNAVAILABLE", "HOST_VERSION_UNAVAILABLE",
  "NATIVE_CONTEXT_INVALID", "NATIVE_CALL_FAILED", "NATIVE_CALL_REJECTED", "NATIVE_CALL_TIMEOUT", "NATIVE_RESPONSE_INVALID",
  "CATALOG_UNAVAILABLE", "DESCRIPTOR_UNAVAILABLE", "CONTROL_ALREADY_ACTIVE", "CONTROL_REVOKED", "SESSION_NOT_ALLOWED",
  "CONTROL_EXPIRED", "ACTIVATION_TIMEOUT", "REQUEST_INVALID", "REQUEST_TOO_LARGE", "CONTROL_BUSY", "OPERATION_CONFLICT",
  "OPERATION_STORAGE_UNAVAILABLE", "OPERATION_UNCERTAIN", "APPROVAL_EXPIRED", "CAPABILITY_UNAVAILABLE", "UNAUTHORIZED"]);
/** One explicit approval stays valid until revoked, Codex restarts or 30 days pass. */
export const MAX_DURATION_SECONDS = 2592000;
export const MAX_SESSIONS = 200;
export const DESKTOP_CAPABILITIES = Object.freeze({ list: true, read: true, send: true,
  interrupt: false, approval: false, attachments: false, modelOverride: false });
const keysOnly = (object, allowed) => Object.keys(object).every((key) => allowed.includes(key));
const fixedError = (error) => error instanceof DesktopError && ERRORS.has(error.code) ? error.code : "NATIVE_CALL_FAILED";

export function validConnectArguments(args) {
  return isRecord(args) && keysOnly(args, ["allowRemoteControl", "durationSeconds", "sessionKeys"])
    && args.allowRemoteControl === true && Number.isInteger(args.durationSeconds) && args.durationSeconds >= 30 && args.durationSeconds <= MAX_DURATION_SECONDS
    && Array.isArray(args.sessionKeys) && args.sessionKeys.length > 0 && args.sessionKeys.length <= MAX_SESSIONS
    && args.sessionKeys.every((key) => typeof key === "string" && SESSION.test(key))
    && new Set(args.sessionKeys).size === args.sessionKeys.length;
}

function textJson(result) {
  if (!isRecord(result) || result.success !== true || !Array.isArray(result.contentItems)) throw new DesktopError("NATIVE_CALL_REJECTED");
  for (const item of result.contentItems) {
    if (item?.type !== "inputText" || typeof item.text !== "string") continue;
    try { return JSON.parse(item.text); } catch { /* Try the next bounded text item. */ }
  }
  throw new DesktopError("NATIVE_RESPONSE_INVALID");
}

/** Only scoped threads leave list; no unrestricted native directory is forwarded to the phone. */
function scopedList(result, allowed) {
  const data = textJson(result);
  if (!isRecord(data)) throw new DesktopError("NATIVE_RESPONSE_INVALID");
  const filter = (items) => Array.isArray(items) ? items.filter((item) => isRecord(item)
    && allowed.has(`codex:${item.threadId ?? item.id ?? item.thread_id}`)) : [];
  if (data.schemaVersion !== 4 || !Array.isArray(data.threads) || !Array.isArray(data.pinnedThreads)) throw new DesktopError("NATIVE_RESPONSE_INVALID");
  return { success: true, contentItems: [{ type: "inputText", text: JSON.stringify({ schemaVersion: data.schemaVersion,
    threads: filter(data.threads), pinnedThreads: filter(data.pinnedThreads) }) }] };
}

function scopedRead(result, key) {
  const data = textJson(result);
  if (!isRecord(data) || !isRecord(data.thread) || data.thread.id !== key.slice(6)
      || data.thread.kind !== "codex" || data.thread.hostId !== "local") throw new DesktopError("NATIVE_RESPONSE_INVALID");
  return result;
}

export function createDesktopConnect({ inspectHost, catalog, nativeCall, directory = process.cwd(),
  writeDescriptor = writePrivateDescriptor, removeDescriptor = removeOwnedDescriptor, now = Date.now,
  createLedger = createOperationLedger, createPermissions = createPermissionBroker, onActive = () => undefined, activationTimeoutMs = 15000 } = {}) {
  let active = false;
  return async (args, meta, { signal } = {}) => {
    if (!validConnectArguments(args)) return { desktopControl: false, remoteSend: false, errorCode: "CONTROL_ARGUMENTS_INVALID" };
    if (active) return { desktopControl: false, remoteSend: false, errorCode: "CONTROL_ALREADY_ACTIVE" };
    active = true;
    let server; let path; let descriptor; let ledger; let permissions; let expiryTimer; let activationTimer; let endReason = "CONTROL_REVOKED";
    const controller = new AbortController();
    const revoke = (reason = "CONTROL_REVOKED") => { endReason = reason; controller.abort(); };
    const externalAbort = () => revoke("CONTROL_REVOKED");
    signal?.addEventListener("abort", externalAbort, { once: true });
    if (signal?.aborted) externalAbort();
    activationTimer = setTimeout(() => revoke("ACTIVATION_TIMEOUT"), activationTimeoutMs);
    const checkActive = () => {
      if (controller.signal.aborted) throw new DesktopError(endReason);
      if (descriptor && now() >= descriptor.expiresAt) { revoke("CONTROL_EXPIRED"); throw new DesktopError("CONTROL_EXPIRED"); }
    };
    try {
      const context = extractHostContext(meta);
      if (!context) throw new DesktopError("HOST_CONTEXT_MISSING");
      const allowed = new Set(args.sessionKeys);
      if (allowed.has(`codex:${context.threadId}`)) throw new DesktopError("SESSION_NOT_ALLOWED");
      const host = await inspectHost(meta, { catalogRequested: true }); checkActive();
      if (!host?.environmentMatches) throw new DesktopError("HOST_ENVIRONMENT_UNAVAILABLE");
      if (!host.metadataPresent) throw new DesktopError("HOST_CONTEXT_MISSING");
      if (!host.versionKnown) throw new DesktopError("HOST_VERSION_UNAVAILABLE");
      const names = await catalog({ pipePath: host.pipePath }); checkActive();
      if (!["list_threads", "read_thread", "send_message_to_thread"].every((name) => names.includes(name))) throw new DesktopError("CATALOG_UNAVAILABLE");
      const invoke = (tool, argumentsValue) => nativeCall({ pipePath: host.pipePath, context, tool, args: argumentsValue, signal: controller.signal });
      scopedList(await invoke("list_threads", { limit: 50 }), allowed); checkActive();
      // Older allowed targets need not be in the first list page; the original
      // identity is proven by an actual successful local read, not title matching.
      scopedRead(await invoke("read_thread", { threadId: args.sessionKeys[0].slice(6), hostId: "local", turnLimit: 10, includeOutputs: true, maxOutputCharsPerItem: 10000 }), args.sessionKeys[0]);
      checkActive();
      let inFlight = 0;
      ledger = await createLedger(directory); checkActive();
      // Only in-flight operations live in memory (at most 8 HTTP requests).
      // Completed receipts stay compact on disk for the entire lease.
      const operations = new Map();
      const cursors = createHistoryCursors();
      permissions = createPermissions({ allowed, signal: controller.signal, now });
      const reply = (response, statusCode, body) => {
        if (response.destroyed) return;
        response.writeHead(statusCode, { "Content-Type": "application/json; charset=utf-8", "Cache-Control": "no-store", "X-Content-Type-Options": "nosniff" });
        response.end(JSON.stringify(body));
      };
      const status = () => ({ desktopControl: true, remoteSend: true, controllerThreadId: context.threadId,
        sessionKeys: [...allowed], expiresAt: descriptor.expiresAt, capabilities: { ...DESKTOP_CAPABILITIES, approval: permissions.observed() },
        ...(permissions.observed() ? { approvalTransport: APPROVAL_TRANSPORT } : {}) });
      const dispatch = async (command, requestSignal) => {
        checkActive();
        if (!isRecord(command) || typeof command.type !== "string") throw new DesktopError("REQUEST_INVALID");
        if (command.type === "interrupt") throw new DesktopError("CAPABILITY_UNAVAILABLE");
        if (command.type === "hook.permission") return permissions.request(command, requestSignal);
        if (command.type === "approval.respond") {
          if (!permissions.observed()) throw new DesktopError("CAPABILITY_UNAVAILABLE");
          return permissions.respond(command);
        }
        if (command.type === "status" || command.type === "disconnect" || command.type === "list") {
          if (!keysOnly(command, ["type"])) throw new DesktopError("REQUEST_INVALID");
          if (command.type === "status") return status();
          if (command.type === "disconnect") return { disconnected: true };
          return scopedList(await invoke("list_threads", { limit: 50 }), allowed);
        }
        if (!["read", "send"].includes(command.type) || !keysOnly(command, command.type === "read" ? ["type", "sessionKey", "cursor"] : ["type", "sessionKey", "text", "operationId"])) throw new DesktopError("REQUEST_INVALID");
        if (!allowed.has(command.sessionKey)) throw new DesktopError("SESSION_NOT_ALLOWED");
        if (command.type === "read") {
          const cursor = cursors.decode(command.sessionKey, command.cursor);
          const read = textJson(scopedRead(await invoke("read_thread", { threadId: command.sessionKey.slice(6), hostId: "local", turnLimit: 10,
            includeOutputs: true, maxOutputCharsPerItem: 10000, ...(cursor === undefined ? {} : { cursor }) }), command.sessionKey));
          if (read.schemaVersion !== 1 || !Array.isArray(read.turns) || read.turns.length > 10) throw new DesktopError("NATIVE_RESPONSE_INVALID");
          if (read.page !== undefined) {
            if (!isRecord(read.page) || read.page.hasMore && read.page.nextCursor == null || cursor !== undefined && read.page.nextCursor === cursor) throw new DesktopError("NATIVE_RESPONSE_INVALID");
            read.page.nextCursor = cursors.encode(command.sessionKey, read.page.hasMore ? read.page.nextCursor : null);
          }
          // Older history cannot reintroduce an approval that was already resolved.
          read.approvalEvents = cursor === undefined ? permissions.snapshot(command.sessionKey) : [];
          return { success: true, contentItems: [{ type: "inputText", text: JSON.stringify(read) }] };
        }
        if (typeof command.text !== "string" || !command.text.trim() || command.text.length > 32000 || !UUID.test(command.operationId ?? "")) throw new DesktopError("REQUEST_INVALID");
        const fingerprint = createHash("sha256").update(JSON.stringify([command.sessionKey, command.text])).digest("hex");
        const previous = operations.get(command.operationId);
        if (previous) {
          if (previous.fingerprint !== fingerprint) throw new DesktopError("OPERATION_CONFLICT");
          return previous.result;
        }
        const result = (async () => {
          const receipt = await ledger.get(command.operationId);
          if (receipt) {
            if (receipt.fingerprint !== fingerprint) throw new DesktopError("OPERATION_CONFLICT");
            if (receipt.state !== "complete") throw new DesktopError("OPERATION_UNCERTAIN");
            return { success: true, contentItems: [{ type: "inputText", text: JSON.stringify({ threadId: receipt.threadId }) }] };
          }
          await ledger.begin(command.operationId, fingerprint); checkActive();
          const value = await invoke("send_message_to_thread", { threadId: command.sessionKey.slice(6), prompt: command.text });
          const acknowledgement = textJson(value);
          if (!isRecord(acknowledgement) || acknowledgement.threadId !== command.sessionKey.slice(6)) throw new DesktopError("NATIVE_RESPONSE_INVALID");
          try { await ledger.complete(command.operationId, acknowledgement.threadId); }
          catch { throw new DesktopError("OPERATION_UNCERTAIN"); }
          return { success: true, contentItems: [{ type: "inputText", text: JSON.stringify({ threadId: acknowledgement.threadId }) }] };
        })();
        operations.set(command.operationId, { fingerprint, result });
        try { return await result; } finally { operations.delete(command.operationId); }
      };
      const token = randomBytes(32).toString("hex");
      server = createServer(async (request, response) => {
        try {
          const supplied = request.headers.authorization?.match(/^Bearer ([a-f0-9]{64})$/)?.[1];
          if (request.socket.remoteAddress !== "127.0.0.1" || request.headers.host !== `127.0.0.1:${descriptor?.port}` || request.headers.origin
              || !supplied || !timingSafeEqual(Buffer.from(supplied), Buffer.from(token))) { request.resume(); reply(response, 401, { ok: false, error: "UNAUTHORIZED" }); return; }
          if (request.method !== "POST" || request.url !== "/rpc" || request.headers["content-type"]?.split(";")[0].trim() !== "application/json" || request.headers["content-encoding"]) {
            request.resume(); reply(response, 400, { ok: false, error: "REQUEST_INVALID" }); return;
          }
          if (inFlight >= 8) { request.resume(); reply(response, 429, { ok: false, error: "CONTROL_BUSY" }); return; }
          inFlight++;
          try {
            let length = 0; const chunks = [];
            for await (const chunk of request) { length += chunk.length; if (length > 64 * 1024) throw new DesktopError("REQUEST_TOO_LARGE"); chunks.push(chunk); }
            let command;
            try { command = JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(Buffer.concat(chunks))); }
            catch { throw new DesktopError("REQUEST_INVALID"); }
            const requestController = new AbortController();
            const cancelRequest = () => requestController.abort();
            response.once("close", cancelRequest);
            let result;
            try { result = await dispatch(command, requestController.signal); checkActive(); }
            finally { response.removeListener("close", cancelRequest); }
            reply(response, 200, { ok: true, result });
            if (command.type === "disconnect") response.once("finish", () => revoke("CONTROL_REVOKED"));
          } finally { inFlight--; }
        } catch (error) { reply(response, 400, { ok: false, error: fixedError(error) }); }
      });
      server.maxConnections = 16; server.requestTimeout = 35000; server.headersTimeout = 10000; server.keepAliveTimeout = 1000;
      await new Promise((resolveListen, reject) => { server.once("error", reject); server.listen(0, "127.0.0.1", resolveListen); });
      descriptor = { version: 1, port: server.address().port, token, expiresAt: now() + args.durationSeconds * 1000,
        controllerThreadId: context.threadId, sessionKeys: [...allowed] };
      checkActive();
      path = await writeDescriptor(directory, descriptor); checkActive();
      clearTimeout(activationTimer);
      // Node timers cap at 2^31-1 ms. A 30-day lease must not overflow into a
      // one-millisecond timer; long leases are scheduled in safe chunks.
      const scheduleExpiry = () => {
        const remaining = descriptor.expiresAt - now();
        if (remaining <= 0) { revoke("CONTROL_EXPIRED"); return; }
        expiryTimer = setTimeout(scheduleExpiry, Math.min(remaining, 2147483647));
      };
      scheduleExpiry();
      // onActive is an injected test hook, never a network publication or MCP token output.
      onActive({ ...descriptor });
      await new Promise((resolveEnd) => { controller.signal.addEventListener("abort", resolveEnd, { once: true }); if (controller.signal.aborted) resolveEnd(); });
      return { ended: true, desktopControl: false, remoteSend: false, sessionCount: allowed.size, errorCode: endReason };
    } catch (error) { return { desktopControl: false, remoteSend: false, errorCode: controller.signal.aborted ? endReason : fixedError(error) }; }
    finally {
      clearTimeout(expiryTimer); clearTimeout(activationTimer); controller.abort(); signal?.removeEventListener("abort", externalAbort);
      permissions?.close();
      if (server) { server.closeAllConnections(); await new Promise((resolveClose) => server.close(resolveClose)); }
      await ledger?.close();
      await removeDescriptor(path, descriptor); active = false;
    }
  };
}
