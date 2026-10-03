import test from "node:test";
import assert from "node:assert/strict";
import { win32 } from "node:path";
import { KNOWN_RESOURCES_PATH, KNOWN_ASAR_SHA256, CURRENT_RESOURCES_PATH, CURRENT_ASAR_SHA256, createResourceVerifier } from "../src/host-policy.mjs";

function fixture(overrides = {}) {
  const inspected = [];
  const resolved = [];
  const hashed = [];
  const archive = win32.join(overrides.resources ?? KNOWN_RESOURCES_PATH, "app.asar");
  const verifier = createResourceVerifier({
    platform: overrides.platform ?? "win32",
    inspectPath: async (path) => {
      inspected.push(path);
      if (overrides.inspectError) throw new Error("SECRET filesystem error");
      return {
        isSymbolicLink: () => path === overrides.symlink,
        isDirectory: () => path !== archive,
        isFile: () => path === archive && overrides.regularArchive !== false,
        size: overrides.archiveSize ?? 123,
      };
    },
    resolvePath: async (path) => {
      resolved.push(path);
      return path === overrides.redirect ? overrides.redirectTo ?? "C:\\SECRET\\redirected" : path;
    },
    hashArchive: async (path) => {
      hashed.push(path);
      if (overrides.hashError) throw new Error("SECRET hash error");
      return overrides.digest ?? KNOWN_ASAR_SHA256;
    },
  });
  return { verifier, archive, inspected, resolved, hashed };
}

test("version verifier reads only pinned archive after directory and canonical path checks", async () => {
  const f = fixture();
  assert.equal(await f.verifier(KNOWN_RESOURCES_PATH), true);
  assert.deepEqual(f.hashed, [f.archive]);
  assert.deepEqual(f.resolved, [KNOWN_RESOURCES_PATH, f.archive]);
  assert.ok(f.inspected.every((path) => KNOWN_RESOURCES_PATH.startsWith(path) || path === f.archive));
  assert.ok(!f.inspected.some((path) => /config|chat|token/i.test(path)));
});

test("researched updated desktop accepts only its own pinned digest; old build identity cannot redirect into new build", async () => {
  const current = fixture({ resources: CURRENT_RESOURCES_PATH, digest: CURRENT_ASAR_SHA256 });
  assert.equal(await current.verifier(CURRENT_RESOURCES_PATH), true);
  assert.deepEqual(current.hashed, [win32.join(CURRENT_RESOURCES_PATH, "app.asar")]);
  assert.equal(await fixture({ resources: CURRENT_RESOURCES_PATH }).verifier(CURRENT_RESOURCES_PATH), false);
  const redirected = fixture({ redirect: KNOWN_RESOURCES_PATH, redirectTo: CURRENT_RESOURCES_PATH });
  assert.equal(await redirected.verifier(KNOWN_RESOURCES_PATH), false);
  assert.deepEqual(redirected.hashed, []);
});

test("unknown paths and platforms never inspect disk", async () => {
  for (const [path, platform] of [["C:\\SECRET\\resources", "win32"], [`${KNOWN_RESOURCES_PATH}\\..\\resources`, "win32"], [KNOWN_RESOURCES_PATH, "linux"]]) {
    const f = fixture({ platform });
    assert.equal(await f.verifier(path), false);
    assert.deepEqual(f.inspected, []);
    assert.deepEqual(f.hashed, []);
  }
});

test("symlink resources, ancestor and archive reject before hashing", async () => {
  for (const symlink of [KNOWN_RESOURCES_PATH, win32.dirname(KNOWN_RESOURCES_PATH), win32.join(KNOWN_RESOURCES_PATH, "app.asar")]) {
    const f = fixture({ symlink });
    assert.equal(await f.verifier(KNOWN_RESOURCES_PATH), false);
    assert.deepEqual(f.hashed, []);
  }
});

test("canonical resource or archive redirection rejects before hashing", async () => {
  for (const redirect of [KNOWN_RESOURCES_PATH, win32.join(KNOWN_RESOURCES_PATH, "app.asar")]) {
    const f = fixture({ redirect });
    assert.equal(await f.verifier(KNOWN_RESOURCES_PATH), false);
    assert.deepEqual(f.hashed, []);
  }
});

test("invalid archive kind and size reject before hashing", async () => {
  for (const options of [{ regularArchive: false }, { archiveSize: 0 }, { archiveSize: 512 * 1024 * 1024 + 1 }, { archiveSize: NaN }, { archiveSize: 1.5 }]) {
    const f = fixture(options);
    assert.equal(await f.verifier(KNOWN_RESOURCES_PATH), false);
    assert.deepEqual(f.hashed, []);
  }
});

test("version mismatch and file/hash errors return false without raw exceptions", async () => {
  for (const options of [{ digest: "UNKNOWN" }, { inspectError: true }, { hashError: true }]) {
    const f = fixture(options);
    assert.equal(await f.verifier(KNOWN_RESOURCES_PATH), false);
  }
});
