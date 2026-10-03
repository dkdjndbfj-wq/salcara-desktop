import { constants } from "node:fs";
import { lstat, mkdtemp, open, rm } from "node:fs/promises";
import { dirname, join, resolve } from "node:path";
import { ensureOwnDirectory } from "./descriptor.mjs";
import { DesktopError } from "./native-control.mjs";

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const HASH = /^[a-f0-9]{64}$/;
const MAX_RECORD_BYTES = 1024;

/** One small receipt per operation, only for this active lease. No prompts,
 * tokens or native tool results are stored. Receipts are not evicted while the
 * lease is live: forgetting an old operation would permit duplicate execution.
 * Memory usage is constant; a retry reads only its UUID-named receipt. */
export async function createOperationLedger(directory) {
  let root;
  let parent;
  let identity;
  try {
    parent = await ensureOwnDirectory(directory);
    root = await mkdtemp(join(parent, "desktop-receipts-"));
    identity = await lstat(root, { bigint: true });
  } catch { throw new DesktopError("OPERATION_STORAGE_UNAVAILABLE"); }
  let closed = false;
  const ownedRoot = async () => {
    const info = await lstat(root, { bigint: true });
    return info.isDirectory() && !info.isSymbolicLink() && info.dev === identity.dev && info.ino === identity.ino;
  };
  const requireOwnedRoot = async () => {
    try { if (closed || !await ownedRoot()) throw new Error("foreign receipt directory"); }
    catch { throw new DesktopError("OPERATION_STORAGE_UNAVAILABLE"); }
  };
  const receiptPath = (id) => {
    if (closed || !UUID.test(id ?? "")) throw new DesktopError("OPERATION_STORAGE_UNAVAILABLE");
    return join(root, `${id}.jsonl`);
  };
  return {
    async get(id) {
      let file;
      try {
        await requireOwnedRoot();
        const path = receiptPath(id);
        const info = await lstat(path);
        if (!info.isFile() || info.isSymbolicLink() || info.size > MAX_RECORD_BYTES) throw new Error("invalid receipt");
        file = await open(path, constants.O_RDONLY | (constants.O_NOFOLLOW ?? 0));
        const st = await file.stat();
        if (!st.isFile() || st.size > MAX_RECORD_BYTES) throw new Error("invalid receipt");
        const lines = (await file.readFile("utf8")).trim().split("\n");
        const first = JSON.parse(lines[0]);
        if (!HASH.test(first.fingerprint ?? "") || first.state !== "pending" || Object.keys(first).length !== 2) throw new Error("invalid receipt");
        // A partial/unknown completion remains a tombstone, never permission to
        // invoke the desktop again. A compact acknowledgement is all we need.
        if (lines.length === 2) {
          try {
            const last = JSON.parse(lines[1]);
            if (last.state === "complete" && UUID.test(last.threadId ?? "") && Object.keys(last).length === 2) return { ...first, ...last };
          } catch { /* Retain pending receipt. */ }
        }
        return first;
      } catch (error) {
        if (error?.code === "ENOENT") return undefined;
        // A receipt may already describe an executed operation. An unreadable
        // or partial record cannot be reported as definitely unsent.
        throw new DesktopError("OPERATION_UNCERTAIN");
      } finally { await file?.close(); }
    },
    async begin(id, fingerprint) {
      let file;
      try {
        await requireOwnedRoot();
        if (!HASH.test(fingerprint ?? "")) throw new Error("invalid fingerprint");
        file = await open(receiptPath(id), constants.O_CREAT | constants.O_EXCL | constants.O_WRONLY, 0o600);
        await file.writeFile(`${JSON.stringify({ fingerprint, state: "pending" })}\n`);
        await file.sync(); // Tombstone is durable before the native side effect.
      } catch (error) { throw new DesktopError(error?.code === "EEXIST" ? "OPERATION_UNCERTAIN" : "OPERATION_STORAGE_UNAVAILABLE"); }
      finally { await file?.close(); }
    },
    async complete(id, threadId) {
      let file;
      try {
        await requireOwnedRoot();
        if (!UUID.test(threadId ?? "")) throw new Error("invalid acknowledgement");
        const path = receiptPath(id);
        const info = await lstat(path);
        if (!info.isFile() || info.isSymbolicLink() || info.size > MAX_RECORD_BYTES / 2) throw new Error("invalid receipt");
        file = await open(path, constants.O_WRONLY | constants.O_APPEND | (constants.O_NOFOLLOW ?? 0));
        await file.writeFile(`${JSON.stringify({ state: "complete", threadId })}\n`);
        await file.sync();
      } catch { throw new DesktopError("OPERATION_STORAGE_UNAVAILABLE"); }
      finally { await file?.close(); }
    },
    async close() {
      closed = true;
      // Only the exact freshly-created lease directory can be removed, not the
      // payload directory, its parent, a redirected path or another lease.
      try {
        if (dirname(resolve(root)) === parent && root.startsWith(join(parent, "desktop-receipts-")) && await ownedRoot()) {
          await rm(root, { recursive: true, force: true });
        }
      } catch { /* A failed cleanup leaves hashes/acknowledgements, not secrets. */ }
    },
  };
}
