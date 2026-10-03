import { randomUUID } from "node:crypto";
import { DesktopError } from "./native-control.mjs";
import { isRecord } from "./probe.mjs";

export const APPROVAL_TRANSPORT = "codex-hook-v1";
export const PERMISSION_WAIT_MS = 110000;
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const only = (value, names) => Object.keys(value).every(key => names.includes(key));

/** A synchronous, trusted PermissionRequest hook is the actual decision owner.
 * Nothing is auto-approved. Timeouts/revocation return {} to the normal desktop
 * approval flow. No server polling, history writes or execution cancellation. */
export function createPermissionBroker({ allowed, signal, now = Date.now, waitMs = PERMISSION_WAIT_MS, nonce = randomUUID } = {}) {
  const pending = new Map();
  const finished = new Map(); // Compact, bounded acknowledgements; no tool inputs.
  const events = new Map();
  let observed = false;
  const remember = (record, decision, by, operationId, message = "") => {
    const event = { type: "approval.resolved", sessionKey: record.sessionKey, tool: "codex", ts: now(),
      approvalId: record.id, decision, by, approvalTransport: APPROVAL_TRANSPORT };
    events.set(record.id, event);
    while (events.size > 128) events.delete(events.keys().next().value);
    finished.set(record.id, { sessionKey: record.sessionKey, decision, operationId, message });
    while (finished.size > 256) finished.delete(finished.keys().next().value);
  };
  const settle = (record, output, decision, by, operationId, message) => {
    if (!pending.delete(record.id)) return;
    clearTimeout(record.timer);
    record.cancelSignal?.removeEventListener("abort", record.cancel);
    remember(record, decision, by, operationId, message);
    record.resolve(output);
  };
  const expire = () => {
    for (const record of pending.values()) if (now() >= record.expiresAt) settle(record, {}, "deny", "timeout");
  };
  const revoke = () => { for (const record of [...pending.values()]) settle(record, {}, "deny", "timeout"); };
  signal?.addEventListener("abort", revoke, { once: true });
  return {
    observed: () => observed && !signal?.aborted,
    async request(command, cancelSignal) {
      expire();
      if (signal?.aborted || cancelSignal?.aborted) return {};
      if (!isRecord(command) || !only(command, ["type", "sessionKey", "turnId", "toolName", "toolInput", "cwd"])
          || !allowed.has(command.sessionKey) || !UUID.test(command.turnId ?? "")
          || typeof command.toolName !== "string" || !/^[A-Za-z0-9_.:-]{1,160}$/.test(command.toolName)
          || !isRecord(command.toolInput) || typeof command.cwd !== "string" || command.cwd.length > 4096) {
        throw new DesktopError("REQUEST_INVALID");
      }
      const detail = JSON.stringify(command.toolInput);
      // A partial/truncated command is never remotely approvable.
      if (Buffer.byteLength(detail) > 16000) throw new DesktopError("REQUEST_TOO_LARGE");
      if (pending.size >= 4) throw new DesktopError("CONTROL_BUSY");
      observed = true;
      const id = nonce();
      if (!UUID.test(id) || pending.has(id) || finished.has(id)) throw new DesktopError("REQUEST_INVALID");
      const record = { id, sessionKey: command.sessionKey, turnId: command.turnId, expiresAt: now() + waitMs, cancelSignal };
      const kind = command.toolName === "Bash" ? "command" : command.toolName === "apply_patch" ? "file_change" : "permission";
      record.event = { type: "approval.request", sessionKey: record.sessionKey, tool: "codex", ts: now(), approvalId: id,
        kind, title: command.toolName, detail, cwd: command.cwd, turnId: record.turnId, expiresAt: record.expiresAt,
        approvalTransport: APPROVAL_TRANSPORT };
      return new Promise(resolve => {
        record.resolve = resolve;
        record.cancel = () => settle(record, {}, "deny", "timeout");
        record.timer = setTimeout(record.cancel, waitMs);
        pending.set(id, record);
        cancelSignal?.addEventListener("abort", record.cancel, { once: true });
        if (signal?.aborted || cancelSignal?.aborted) record.cancel();
      });
    },
    snapshot(sessionKey) {
      expire();
      return [...events.values(), ...[...pending.values()].map(record => record.event)].filter(event => event.sessionKey === sessionKey);
    },
    respond(command) {
      expire();
      if (signal?.aborted || !isRecord(command) || !only(command, ["type", "sessionKey", "approvalId", "decision", "message", "operationId"])
          || !allowed.has(command.sessionKey) || !UUID.test(command.approvalId ?? "") || !UUID.test(command.operationId ?? "")
          || !["allow", "deny"].includes(command.decision)
          || (command.message !== undefined && (typeof command.message !== "string" || command.message.length > 2000))) {
        throw new DesktopError("REQUEST_INVALID");
      }
      const message = command.message ?? "";
      const old = finished.get(command.approvalId);
      if (old) {
        if (old.sessionKey !== command.sessionKey) throw new DesktopError("SESSION_NOT_ALLOWED");
        if (old.operationId !== command.operationId || old.decision !== command.decision || old.message !== message) throw new DesktopError("APPROVAL_EXPIRED");
        return { accepted: true, approvalId: command.approvalId, sessionKey: command.sessionKey };
      }
      const record = pending.get(command.approvalId);
      if (!record) throw new DesktopError("APPROVAL_EXPIRED");
      if (record.sessionKey !== command.sessionKey) throw new DesktopError("SESSION_NOT_ALLOWED");
      const decision = { behavior: command.decision, ...(command.decision === "deny" && message ? { message } : {}) };
      settle(record, { hookSpecificOutput: { hookEventName: "PermissionRequest", decision } }, command.decision, "phone", command.operationId, message);
      return { accepted: true, approvalId: command.approvalId, sessionKey: command.sessionKey };
    },
    close() { revoke(); signal?.removeEventListener("abort", revoke); },
  };
}
