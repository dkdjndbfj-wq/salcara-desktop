import { createHash } from "node:crypto";
import { createReadStream } from "node:fs";
import { lstat, realpath } from "node:fs/promises";
import { win32 } from "node:path";
import { isRecord } from "./probe.mjs";

// Private desktop transport is version-gated, not assumed stable across updates.
export const KNOWN_RESOURCES_PATH = String.raw`C:\Program Files\WindowsApps\OpenAI.Codex_26.928.2636.0_x64__2p2nqsd0c76g0\app\resources`;
export const KNOWN_ASAR_SHA256 = "fb7b2ee791bcbdb3c4a6375e9fec8fb404f3eaff995f0d293227354b298aff49";
export const CURRENT_RESOURCES_PATH = String.raw`C:\Program Files\WindowsApps\OpenAI.Codex_26.930.2377.0_x64__2p2nqsd0c76g0\app\resources`;
export const CURRENT_ASAR_SHA256 = "7a65bbbdf265aaa130a6670f1d310e7113646f9b86b2e9602e82fee400a92856";
const RESEARCHED_HOSTS = new Map([[KNOWN_RESOURCES_PATH.toLowerCase(), KNOWN_ASAR_SHA256],
  [CURRENT_RESOURCES_PATH.toLowerCase(), CURRENT_ASAR_SHA256]]);
export const WINDOWS_PIPE_PATTERN = /^\\\\\.\\pipe\\codex-browser-use-[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;

const UUID_PATTERN = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;
const TOOL_CALL_PATTERN = /^call_[a-zA-Z0-9_-]{8,120}$/;
const MAX_ARCHIVE_BYTES = 512 * 1024 * 1024;
const VERSION_CHECK_TIMEOUT_MS = 3000;

function matchesKnownResources(value) {
  return typeof value === "string" && RESEARCHED_HOSTS.has(value.toLowerCase());
}

function isUuid(value) {
  return typeof value === "string" && UUID_PATTERN.test(value);
}

function inspectMetadata(meta) {
  if (!isRecord(meta)) return { threadMetaPresent: false, turnMetaPresent: false, callMetaPresent: false, metadataPresent: false };
  const directThread = meta["openai/threadId"];
  const directTurn = meta["openai/turnId"];
  const call = meta["openai/toolCallId"];
  let encoded;
  let invalidEncoded = false;
  if (Object.hasOwn(meta, "x-codex-turn-metadata")) {
    const value = meta["x-codex-turn-metadata"];
    if (typeof value !== "string" || value.length > 2048) invalidEncoded = true;
    else {
      try {
        encoded = JSON.parse(value);
        if (!isRecord(encoded) || !isUuid(encoded.thread_id) || !isUuid(encoded.turn_id)) invalidEncoded = true;
      } catch { invalidEncoded = true; }
    }
  }
  const thread = directThread ?? encoded?.thread_id;
  const turn = directTurn ?? encoded?.turn_id;
  const threadMetaPresent = isUuid(thread);
  const turnMetaPresent = isUuid(turn);
  const conflict = encoded && ((directThread !== undefined && directThread !== encoded.thread_id) ||
    (directTurn !== undefined && directTurn !== encoded.turn_id));
  return {
    threadMetaPresent,
    turnMetaPresent,
    callMetaPresent: isUuid(call) || (typeof call === "string" && TOOL_CALL_PATTERN.test(call)),
    metadataPresent: threadMetaPresent && turnMetaPresent && !invalidEncoded && !conflict,
  };
}

/** Private execution context only from the current MCP request; never a tool argument. */
export function extractHostContext(meta) {
  if (!inspectMetadata(meta).metadataPresent) return undefined;
  const encoded = meta["x-codex-turn-metadata"] ? JSON.parse(meta["x-codex-turn-metadata"]) : undefined;
  return { threadId: (meta["openai/threadId"] ?? encoded.thread_id).toLowerCase(),
    turnId: (meta["openai/turnId"] ?? encoded.turn_id).toLowerCase() };
}

async function hashKnownArchive(archivePath) {
  let stream;
  let timer;
  try {
    const hash = createHash("sha256");
    stream = createReadStream(archivePath);
    timer = setTimeout(() => stream.destroy(new Error("VERSION_CHECK_TIMEOUT")), VERSION_CHECK_TIMEOUT_MS);
    let bytes = 0;
    for await (const chunk of stream) {
      bytes += chunk.length;
      if (bytes > MAX_ARCHIVE_BYTES) throw new Error("ARCHIVE_TOO_LARGE");
      hash.update(chunk);
    }
    return hash.digest("hex");
  } finally {
    if (timer !== undefined) clearTimeout(timer);
    stream?.destroy();
  }
}

export function createResourceVerifier({
  platform = process.platform,
  inspectPath = lstat,
  resolvePath = realpath,
  hashArchive = hashKnownArchive,
} = {}) {
  return async (resourcesPath) => {
    if (platform !== "win32" || !matchesKnownResources(resourcesPath)) return false;
    try {
      // Refuse redirected package/resource paths. The only file read is app.asar.
      let current = resourcesPath;
      while (current !== win32.parse(current).root) {
        const info = await inspectPath(current);
        if (info.isSymbolicLink() || !info.isDirectory()) return false;
        current = win32.dirname(current);
      }
      if ((await resolvePath(resourcesPath)).toLowerCase() !== resourcesPath.toLowerCase()) return false;
      const archivePath = win32.join(resourcesPath, "app.asar");
      const archiveInfo = await inspectPath(archivePath);
      if (!archiveInfo.isFile() || archiveInfo.isSymbolicLink() || !Number.isSafeInteger(archiveInfo.size) ||
          archiveInfo.size <= 0 || archiveInfo.size > MAX_ARCHIVE_BYTES ||
          (await resolvePath(archivePath)).toLowerCase() !== archivePath.toLowerCase()) return false;
      return await hashArchive(archivePath) === RESEARCHED_HOSTS.get(resourcesPath.toLowerCase());
    } catch {
      return false;
    }
  };
}

export const verifyKnownHostResources = createResourceVerifier();

export function createHostPolicy({ env = {}, platform = process.platform, verifyVersion = verifyKnownHostResources } = {}) {
  // No environment enumeration, process scanning, configuration reads, or
  // command-line path override. Values are retained privately and never echoed.
  const resourcesPath = env.CODEX_ELECTRON_RESOURCES_PATH;
  const pipePath = env.CODEX_APP_TOOLS_PIPE_PATH;
  const environmentPresent = typeof resourcesPath === "string" && resourcesPath.length > 0 &&
    typeof pipePath === "string" && pipePath.length > 0;
  const environmentMatches = platform === "win32" && matchesKnownResources(resourcesPath) &&
    typeof pipePath === "string" && WINDOWS_PIPE_PATTERN.test(pipePath);
  return async (meta, { catalogRequested = false } = {}) => {
    const metadata = inspectMetadata(meta);
    let versionKnown = false;
    if (catalogRequested === true && environmentMatches && metadata.metadataPresent) {
      try { versionKnown = await verifyVersion(resourcesPath) === true; }
      catch { /* Fail closed without exposing filesystem errors. */ }
    }
    return {
      environmentPresent,
      environmentMatches,
      ...metadata,
      versionKnown,
      ...(environmentMatches ? { pipePath } : {}),
    };
  };
}
