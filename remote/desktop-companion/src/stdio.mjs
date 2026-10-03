import { CONNECT_TOOL_NAME, rpcError } from "./server.mjs";

export const MAX_REQUEST_BYTES = 64 * 1024;

export async function runStdio({ input, output, server }) {
  let partial = [];
  let partialBytes = 0;
  let oversized = false;
  const pending = new Set();
  let outputFailure;

  const emit = (message) => new Promise((resolve, reject) => {
    if (message === undefined) { resolve(); return; }
    output.write(`${JSON.stringify(message)}\n`, (error) => error ? reject(new Error("OUTPUT_UNAVAILABLE")) : resolve());
  });
  const parseLine = async (line) => {
    if (line.length === 0 || (line.length === 1 && line[0] === 13)) return;
    let message;
    try {
      message = JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(line));
    } catch {
      await emit(rpcError(null, -32700, "Parse error"));
      return;
    }
    // The active authorization deliberately remains pending. Keep consuming
    // cancellation notifications without allowing unbounded background calls.
    if (message?.method === "tools/call" && message?.params?.name === CONNECT_TOOL_NAME && Object.hasOwn(message, "id")) {
      if (pending.size >= 4) { await emit(rpcError(message.id, -32600, "Too many active calls")); return; }
      const work = Promise.resolve(server.handle(message)).then(emit).catch((error) => { outputFailure = error; server.close?.(); });
      pending.add(work); void work.finally(() => pending.delete(work));
    } else await emit(await server.handle(message));
  };
  const append = async (segment) => {
    if (oversized) return;
    if (partialBytes + segment.length > MAX_REQUEST_BYTES) {
      oversized = true;
      partial = [];
      partialBytes = 0;
      await emit(rpcError(null, -32700, "Request too large"));
      return;
    }
    partial.push(segment);
    partialBytes += segment.length;
  };

  // Sequential handling and stream backpressure bound the number of requests in
  // flight. Overlong lines are discarded until the next newline, then resynced.
  try { for await (const value of input) {
    const chunk = Buffer.isBuffer(value) ? value : Buffer.from(value);
    let start = 0;
    let newline;
    while ((newline = chunk.indexOf(10, start)) !== -1) {
      await append(chunk.subarray(start, newline));
      if (!oversized) await parseLine(Buffer.concat(partial, partialBytes));
      partial = [];
      partialBytes = 0;
      oversized = false;
      start = newline + 1;
    }
    if (start < chunk.length) await append(chunk.subarray(start));
  }
  if (!oversized && partialBytes > 0) await parseLine(Buffer.concat(partial, partialBytes));
  } finally {
    // Closing or losing the host stream is revocation, never a detached daemon.
    server.close?.();
    await Promise.all(pending);
  }
  if (outputFailure) throw outputFailure;
}
