export const PROBE_VERSION = "0.4.0";

export const CATALOG_TOOL_ALLOWLIST = Object.freeze([
  "list_threads",
  "read_thread",
  "navigate_to_codex_page",
  "send_message_to_thread",
]);

export const ERROR_CODES = Object.freeze([
  "NONE",
  "HOST_ENVIRONMENT_UNAVAILABLE",
  "HOST_CONTEXT_MISSING",
  "HOST_VERSION_UNAVAILABLE",
  "CATALOG_UNAVAILABLE",
  "CATALOG_TIMEOUT",
  "CATALOG_FRAME_INVALID",
  "CATALOG_RESPONSE_INVALID",
  "CATALOG_FAILED",
]);

export class ProbeError extends Error {
  constructor(code) {
    const safeCode = ERROR_CODES.includes(code) ? code : "CATALOG_FAILED";
    super(safeCode);
    this.name = "ProbeError";
    this.code = safeCode;
  }
}

export function isRecord(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

export function sanitizeToolNames(names) {
  if (!Array.isArray(names)) throw new ProbeError("CATALOG_RESPONSE_INVALID");
  const present = new Set(names.filter((name) => typeof name === "string"));
  return CATALOG_TOOL_ALLOWLIST.filter((name) => present.has(name));
}

// The supplied policy examines only this process's inherited environment. These
// signals and metadata are deliberately not described as authentication.
export function createProbe({ inspectHost, catalog } = {}) {
  return async function probe(args = {}, hostMeta) {
    const result = {
      probeVersion: PROBE_VERSION,
      hostEnvironmentPresent: false,
      hostEnvironmentMatches: false,
      hostMetadataPresent: false,
      threadMetaPresent: false,
      turnMetaPresent: false,
      callMetaPresent: false,
      hostVersionKnown: false,
      catalogRequested: args.allowCatalogProbe === true,
      catalogAttempted: false,
      catalogAvailable: false,
      toolNames: [],
      desktopControl: false,
      remoteSend: false,
      errorCode: "NONE",
    };

    let signals;
    try {
      signals = typeof inspectHost === "function" ? await inspectHost(hostMeta, {
        catalogRequested: result.catalogRequested,
      }) : undefined;
    } catch {
      result.errorCode = "HOST_ENVIRONMENT_UNAVAILABLE";
      return result;
    }
    result.hostEnvironmentPresent = signals?.environmentPresent === true;
    result.hostEnvironmentMatches = signals?.environmentMatches === true;
    result.hostMetadataPresent = signals?.metadataPresent === true;
    result.threadMetaPresent = signals?.threadMetaPresent === true;
    result.turnMetaPresent = signals?.turnMetaPresent === true;
    result.callMetaPresent = signals?.callMetaPresent === true;
    result.hostVersionKnown = signals?.versionKnown === true;

    if (!result.catalogRequested) return result;
    if (!result.hostEnvironmentMatches) {
      result.errorCode = "HOST_ENVIRONMENT_UNAVAILABLE";
      return result;
    }
    if (!result.hostMetadataPresent) {
      result.errorCode = "HOST_CONTEXT_MISSING";
      return result;
    }
    if (!result.hostVersionKnown) {
      result.errorCode = "HOST_VERSION_UNAVAILABLE";
      return result;
    }
    if (typeof catalog !== "function" || typeof signals?.pipePath !== "string") {
      result.errorCode = "CATALOG_UNAVAILABLE";
      return result;
    }

    result.catalogAttempted = true;
    try {
      const names = await catalog({ pipePath: signals.pipePath });
      result.toolNames = sanitizeToolNames(names);
      result.catalogAvailable = true;
    } catch (error) {
      // Never return host exception messages, stack traces, or response content.
      result.errorCode = error instanceof ProbeError && ERROR_CODES.includes(error.code) ? error.code : "CATALOG_FAILED";
    }
    return result;
  };
}
