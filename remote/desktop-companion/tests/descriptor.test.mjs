import test from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdtemp, readFile, rmdir, stat, unlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { removeOwnedDescriptor, writePrivateDescriptor } from "../src/descriptor.mjs";

const descriptor = { version: 1, port: 12345, token: "a".repeat(64), expiresAt: 9999999999999,
  controllerThreadId: "11111111-2222-4333-8444-555555555555", sessionKeys: ["codex:22222222-2222-4333-8444-555555555555"] };

test("descriptor is private at birth, existing file is never overwritten, and only matching ownership is removed", { timeout: 15000 }, async () => {
  const directory = await mkdtemp(join(tmpdir(), "salcara-descriptor-test-")); let path;
  try {
    path = await writePrivateDescriptor(directory, descriptor);
    assert.deepEqual(JSON.parse(await readFile(path, "utf8")), descriptor);
    if (process.platform === "win32") {
      const encoded = Buffer.from(path).toString("base64");
      const script = `$ProgressPreference='SilentlyContinue'; $p=[Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('${encoded}')); $a=[IO.File]::GetAccessControl($p); [PSCustomObject]@{protected=$a.AreAccessRulesProtected;sids=@($a.GetAccessRules($true,$true,[Security.Principal.SecurityIdentifier])|ForEach-Object{$_.IdentityReference.Value});owner=$a.GetOwner([Security.Principal.SecurityIdentifier]).Value}|ConvertTo-Json -Compress`;
      const acl = JSON.parse(execFileSync(String.raw`C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`, ["-NoProfile", "-NonInteractive", "-EncodedCommand", Buffer.from(script, "utf16le").toString("base64")], { windowsHide: true, encoding: "utf8", timeout: 5000 }));
      assert.equal(acl.protected, true); assert.deepEqual(new Set(acl.sids), new Set([acl.owner, "S-1-5-18"]));
    } else assert.equal((await stat(path)).mode & 0o777, 0o600);
    await assert.rejects(writePrivateDescriptor(directory, { ...descriptor, token: "b".repeat(64) }), { code: "DESCRIPTOR_UNAVAILABLE" });
    assert.equal(JSON.parse(await readFile(path, "utf8")).token, descriptor.token);
    await removeOwnedDescriptor(path, { ...descriptor, token: "b".repeat(64) });
    assert.equal(JSON.parse(await readFile(path, "utf8")).token, descriptor.token);
    await removeOwnedDescriptor(path, descriptor);
    await assert.rejects(stat(path)); path = undefined;
  } finally { if (path) await unlink(path).catch(() => undefined); await rmdir(directory); }
});

test("foreign descriptor is retained even after an attempted owned cleanup", async () => {
  const directory = await mkdtemp(join(tmpdir(), "salcara-descriptor-test-")); const path = join(directory, "active-desktop.json");
  try {
    await writeFile(path, '{"foreign":true}', { mode: 0o600 });
    await removeOwnedDescriptor(path, descriptor);
    assert.equal(await readFile(path, "utf8"), '{"foreign":true}');
  } finally { await unlink(path); await rmdir(directory); }
});

test("Windows retires only expired private valid leases without PID or live socket checks", { skip: process.platform !== "win32", timeout: 20000 }, async () => {
  const directory = await mkdtemp(join(tmpdir(), "salcara-descriptor-test-")); let path;
  const previous = { ...descriptor, expiresAt: Date.now() - 1000 };
  try {
    path = await writePrivateDescriptor(directory, previous);
    const replacement = { ...descriptor, expiresAt: Date.now() + 60000, token: "b".repeat(64) };
    assert.equal(await writePrivateDescriptor(directory, replacement), path);
    assert.deepEqual(JSON.parse(await readFile(path, "utf8")), replacement);
    await assert.rejects(writePrivateDescriptor(directory, descriptor), { code: "DESCRIPTOR_UNAVAILABLE" });
    assert.deepEqual(JSON.parse(await readFile(path, "utf8")), replacement);
  } finally { if (path) await unlink(path).catch(() => undefined); await rmdir(directory); }
});
