import test from "node:test";
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { KNOWN_RESOURCES_PATH, createHostPolicy } from "../src/host-policy.mjs";
import { createProbe, ProbeError, PROBE_VERSION } from "../src/probe.mjs";

test("packaged runtime and probe advertise the same version", async () => {
  const manifest = JSON.parse(await readFile(new URL("../package.json", import.meta.url), "utf8"));
  assert.equal(PROBE_VERSION, manifest.version);
});

const pipe = String.raw`\\.\pipe\codex-browser-use-a1a1a1a1-2222-4333-8444-555555555555`;
const thread = "01a0ae56-e9f6-7933-9ad4-5e08cd7874e5";
const turn = "11111111-2222-4333-8444-555555555555";
const meta = { "openai/threadId": thread, "openai/turnId": turn };
const env = { CODEX_ELECTRON_RESOURCES_PATH: KNOWN_RESOURCES_PATH, CODEX_APP_TOOLS_PIPE_PATH: pipe };

function fixture(overrides = {}) {
  let hashes = 0;
  let catalogs = 0;
  const inspectHost = createHostPolicy({
    env: overrides.env ?? env,
    platform: overrides.platform ?? "win32",
    verifyVersion: async (path) => {
      hashes++;
      assert.equal(path, KNOWN_RESOURCES_PATH);
      if (overrides.hashError) throw new Error("secret raw filesystem error");
      return overrides.versionKnown ?? true;
    },
  });
  const probe = createProbe({ inspectHost, catalog: async ({ pipePath }) => {
    catalogs++;
    assert.equal(pipePath, pipe);
    if (overrides.catalogError) throw overrides.catalogError;
    return ["send_message_to_thread", "read_thread", "list_threads", "list_threads", "secret_unknown_tool", "navigate_to_codex_page"];
  } });
  return { probe, counts: () => ({ hashes, catalogs }) };
}

test("default and false requests are environment-only, with no hash or socket access", async () => {
  const f = fixture();
  for (const args of [{}, { allowCatalogProbe: false }, { allowCatalogProbe: "true" }]) {
    const r = await f.probe(args, meta);
    assert.equal(r.hostEnvironmentMatches, true);
    assert.equal(r.hostMetadataPresent, true);
    assert.equal(r.hostVersionKnown, false);
    assert.equal(r.catalogRequested, false);
    assert.equal(r.catalogAttempted, false);
    assert.equal(r.desktopControl, false);
    assert.equal(r.remoteSend, false);
  }
  assert.deepEqual(f.counts(), { hashes: 0, catalogs: 0 });
});

test("explicit catalog requires matching environment, metadata and known version", async () => {
  const f = fixture();
  const r = await f.probe({ allowCatalogProbe: true }, meta);
  assert.equal(r.hostVersionKnown, true);
  assert.equal(r.catalogAvailable, true);
  assert.equal(r.callMetaPresent, false);
  assert.equal(r.desktopControl, false);
  assert.equal(r.remoteSend, false);
  assert.deepEqual(r.toolNames, ["list_threads", "read_thread", "navigate_to_codex_page", "send_message_to_thread"]);
  assert.deepEqual(f.counts(), { hashes: 1, catalogs: 1 });
  const json = JSON.stringify(r);
  for (const privateValue of [pipe, KNOWN_RESOURCES_PATH, thread, turn, "secret_unknown_tool"]) assert.ok(!json.includes(privateValue));
});

test("missing metadata never hashes or connects, even if args contain caller identities", async () => {
  const f = fixture();
  const r = await f.probe({ allowCatalogProbe: true, _meta: meta, threadId: thread, turnId: turn });
  assert.equal(r.errorCode, "HOST_CONTEXT_MISSING");
  assert.equal(r.catalogAttempted, false);
  assert.deepEqual(f.counts(), { hashes: 0, catalogs: 0 });
});

test("encoded host metadata works without a tool call ID", async () => {
  const f = fixture();
  const r = await f.probe({ allowCatalogProbe: true }, {
    "x-codex-turn-metadata": JSON.stringify({ thread_id: thread, turn_id: turn }),
  });
  assert.equal(r.hostMetadataPresent, true);
  assert.equal(r.threadMetaPresent, true);
  assert.equal(r.turnMetaPresent, true);
  assert.equal(r.callMetaPresent, false);
  assert.equal(r.catalogAvailable, true);
});

test("call metadata is only an observed signal and is never echoed", async () => {
  const f = fixture();
  const call = "call_examplePrivateCallId123";
  const r = await f.probe({}, { ...meta, "openai/toolCallId": call });
  assert.equal(r.callMetaPresent, true);
  assert.ok(!JSON.stringify(r).includes(call));
  assert.deepEqual(f.counts(), { hashes: 0, catalogs: 0 });
});

for (const [name, invalidMeta] of [
  ["empty", {}],
  ["missing turn", { "openai/threadId": thread }],
  ["non UUID thread", { ...meta, "openai/threadId": "secret arbitrary identity" }],
  ["non UUID turn", { ...meta, "openai/turnId": "secret arbitrary identity" }],
  ["malformed encoded", { "x-codex-turn-metadata": "secret malformed JSON" }],
  ["encoded object rather than string", { "x-codex-turn-metadata": { thread_id: thread, turn_id: turn } }],
  ["conflicting sources", { ...meta, "x-codex-turn-metadata": JSON.stringify({ thread_id: turn, turn_id: turn }) }],
]) {
  test(`invalid metadata: ${name} fails closed`, async () => {
    const f = fixture();
    const r = await f.probe({ allowCatalogProbe: true }, invalidMeta);
    assert.equal(r.errorCode, "HOST_CONTEXT_MISSING");
    assert.deepEqual(f.counts(), { hashes: 0, catalogs: 0 });
  });
}

for (const [name, invalidEnv, platform] of [
  ["missing", {}, "win32"],
  ["different package", { ...env, CODEX_ELECTRON_RESOURCES_PATH: KNOWN_RESOURCES_PATH.replace("2636", "9999") }, "win32"],
  ["relative path", { ...env, CODEX_ELECTRON_RESOURCES_PATH: ".\\resources" }, "win32"],
  ["dot segment", { ...env, CODEX_ELECTRON_RESOURCES_PATH: `${KNOWN_RESOURCES_PATH}\\..\\resources` }, "win32"],
  ["arbitrary pipe", { ...env, CODEX_APP_TOOLS_PIPE_PATH: String.raw`\\.\pipe\secret-other-pipe` }, "win32"],
  ["non v4 pipe", { ...env, CODEX_APP_TOOLS_PIPE_PATH: pipe.replace("4333", "7333") }, "win32"],
  ["bad UUID variant", { ...env, CODEX_APP_TOOLS_PIPE_PATH: pipe.replace("8444", "7444") }, "win32"],
  ["pipe suffix", { ...env, CODEX_APP_TOOLS_PIPE_PATH: `${pipe}-extra` }, "win32"],
  ["unsupported platform", env, "linux"],
]) {
  test(`invalid environment: ${name} fails closed`, async () => {
    const f = fixture({ env: invalidEnv, platform });
    const r = await f.probe({ allowCatalogProbe: true }, meta);
    assert.equal(r.errorCode, "HOST_ENVIRONMENT_UNAVAILABLE");
    assert.deepEqual(f.counts(), { hashes: 0, catalogs: 0 });
  });
}

test("version mismatch and filesystem errors never connect or expose host errors", async () => {
  for (const options of [{ versionKnown: false }, { hashError: true }]) {
    const f = fixture(options);
    const r = await f.probe({ allowCatalogProbe: true }, meta);
    assert.equal(r.errorCode, "HOST_VERSION_UNAVAILABLE");
    assert.equal(r.hostVersionKnown, false);
    assert.deepEqual(f.counts(), { hashes: 1, catalogs: 0 });
    assert.ok(!JSON.stringify(r).includes("secret"));
  }
});

test("catalog exceptions return fixed codes without raw host errors", async () => {
  for (const [error, expected] of [[new Error("secret key pipe error"), "CATALOG_FAILED"], [new ProbeError("CATALOG_TIMEOUT"), "CATALOG_TIMEOUT"]]) {
    const f = fixture({ catalogError: error });
    const r = await f.probe({ allowCatalogProbe: true }, meta);
    assert.equal(r.errorCode, expected);
    assert.equal(r.catalogAvailable, false);
    assert.deepEqual(r.toolNames, []);
    assert.ok(!JSON.stringify(r).includes("secret"));
  }
});

test("policy reads only the two inherited signal names, never enumerating environment", async () => {
  const accessed = [];
  const fakeEnv = new Proxy(env, {
    get(target, key) { accessed.push(key); return target[key]; },
    ownKeys() { throw new Error("environment enumeration forbidden"); },
  });
  const policy = createHostPolicy({ env: fakeEnv, platform: "win32", verifyVersion: () => { throw new Error("default must not read disk"); } });
  const signals = await policy(meta, { catalogRequested: false });
  assert.equal(signals.environmentMatches, true);
  assert.deepEqual(accessed, ["CODEX_ELECTRON_RESOURCES_PATH", "CODEX_APP_TOOLS_PIPE_PATH"]);
});
