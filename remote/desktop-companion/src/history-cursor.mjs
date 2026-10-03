import { createHmac, randomBytes, timingSafeEqual } from "node:crypto";
import { DesktopError } from "./native-control.mjs";

// Original native cursors stay opaque. A lease-local MAC binds them to the
// exact approved session and expires naturally when this gateway is closed.
export function createHistoryCursors() {
  const secret = randomBytes(32);
  const sign = value => createHmac("sha256", secret).update(value).digest("base64url");
  const valid = value => typeof value === "string" && value.length > 0 && Buffer.byteLength(value) <= 2048 && !/[\x00-\x1f\x7f]/.test(value);
  return {
    encode(sessionKey, cursor) {
      if (cursor == null) return null;
      if (!valid(cursor)) throw new DesktopError("NATIVE_RESPONSE_INVALID");
      const payload = Buffer.from(JSON.stringify({ version: 1, sessionKey, cursor })).toString("base64url");
      if (payload.length + 44 > 4096) throw new DesktopError("NATIVE_RESPONSE_INVALID");
      return `${payload}.${sign(payload)}`;
    },
    decode(sessionKey, value) {
      if (value === undefined) return undefined;
      if (typeof value !== "string" || value.length > 4096 || !/^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]{43}$/.test(value)) throw new DesktopError("REQUEST_INVALID");
      const [payload, signature] = value.split(".");
      if (!timingSafeEqual(Buffer.from(signature), Buffer.from(sign(payload)))) throw new DesktopError("REQUEST_INVALID");
      let data;
      try { data = JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(Buffer.from(payload, "base64url"))); }
      catch { throw new DesktopError("REQUEST_INVALID"); }
      if (data?.version !== 1 || data.sessionKey !== sessionKey || !valid(data.cursor) || Object.keys(data).length !== 3) throw new DesktopError("REQUEST_INVALID");
      return data.cursor;
    },
  };
}
