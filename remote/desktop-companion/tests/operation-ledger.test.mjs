import test from "node:test";
import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { mkdir, mkdtemp, readdir, readFile, rename, rm, rmdir, stat, symlink, unlink, writeFile } from "node:fs/promises";
import { execFileSync } from "node:child_process";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createOperationLedger } from "../src/operation-ledger.mjs";

test("receipts are bounded compact metadata, never prompts/results, and owned cleanup leaves its parent", async () => {
  const directory = await mkdtemp(join(tmpdir(), "salcara-ledger-test-"));
  const ledger = await createOperationLedger(directory);
  try {
    const id = randomUUID(), threadId = randomUUID(), fingerprint = "a".repeat(64);
    assert.equal(await ledger.get(id), undefined);
    await ledger.begin(id, fingerprint);
    assert.deepEqual(await ledger.get(id), { fingerprint, state: "pending" });
    await ledger.complete(id, threadId);
    assert.deepEqual(await ledger.get(id), { fingerprint, state: "complete", threadId });
    const [ownDirectory] = await readdir(directory);
    const contents = await readFile(join(directory, ownDirectory, `${id}.jsonl`), "utf8");
    assert.ok(contents.length < 256);
    assert.deepEqual(contents.trim().split("\n").map(JSON.parse), [{ fingerprint, state: "pending" }, { state: "complete", threadId }]);
    await assert.rejects(ledger.begin(id, fingerprint), { code: "OPERATION_UNCERTAIN" });
    await assert.rejects(ledger.get("../foreign"), { code: "OPERATION_UNCERTAIN" });
  } finally { await ledger.close(); assert.deepEqual(await readdir(directory), []); await rmdir(directory); }
});

test("partial completion and invalid/oversized receipts never grant replay permission", async () => {
  const directory = await mkdtemp(join(tmpdir(), "salcara-ledger-test-"));
  const ledger = await createOperationLedger(directory);
  try {
    const id = randomUUID(), fingerprint = "b".repeat(64);
    await ledger.begin(id, fingerprint);
    const [ownDirectory] = await readdir(directory), path = join(directory, ownDirectory, `${id}.jsonl`);
    await writeFile(path, `${JSON.stringify({ fingerprint, state: "pending" })}\n{"state":`);
    assert.deepEqual(await ledger.get(id), { fingerprint, state: "pending" });
    await writeFile(path, "x".repeat(1100));
    await assert.rejects(ledger.get(id), { code: "OPERATION_UNCERTAIN" });
  } finally { await ledger.close(); await rmdir(directory); }
});

test("replaced receipt directory cannot grant replay and cleanup preserves the foreign replacement", async () => {
  const directory = await mkdtemp(join(tmpdir(), "salcara-ledger-test-"));
  const ledger = await createOperationLedger(directory);
  const [name] = await readdir(directory), root = join(directory, name), moved = join(directory, "retained-original");
  try {
    const id = randomUUID(); await ledger.begin(id, "c".repeat(64));
    await rename(root, moved); await mkdir(root); await writeFile(join(root, "foreign.txt"), "retained");
    await assert.rejects(ledger.get(id), { code: "OPERATION_UNCERTAIN" });
    await assert.rejects(ledger.begin(randomUUID(), "d".repeat(64)), { code: "OPERATION_STORAGE_UNAVAILABLE" });
    await ledger.close();
    assert.equal(await readFile(join(root, "foreign.txt"), "utf8"), "retained");
    assert.ok((await readFile(join(moved, `${id}.jsonl`), "utf8")).includes("pending"));
  } finally { await ledger.close(); await rm(directory, { recursive: true, force: true }); }
});

test("junction/symlink replacement is never traversed by operations or cleanup", async () => {
  const directory = await mkdtemp(join(tmpdir(), "salcara-ledger-test-"));
  const ledger = await createOperationLedger(directory);
  const [name] = await readdir(directory), root = join(directory, name), moved = join(directory, "retained-original"), foreign = join(directory, "foreign");
  try {
    await rename(root, moved); await mkdir(foreign); await writeFile(join(foreign, "keep.txt"), "keep");
    await symlink(foreign, root, process.platform === "win32" ? "junction" : "dir");
    await assert.rejects(ledger.get(randomUUID()), { code: "OPERATION_UNCERTAIN" });
    await ledger.close();
    assert.equal(await readFile(join(foreign, "keep.txt"), "utf8"), "keep");
    await unlink(root);
  } finally { await ledger.close(); await rm(directory, { recursive: true, force: true }); }
});

test("owned directory cleanup unlinks a nested junction without deleting its foreign target", async () => {
  const directory = await mkdtemp(join(tmpdir(), "salcara-ledger-test-"));
  const ledger = await createOperationLedger(directory);
  const [name] = await readdir(directory), root = join(directory, name), foreign = join(directory, "foreign");
  try {
    await mkdir(foreign); await writeFile(join(foreign, "keep.txt"), "keep");
    await symlink(foreign, join(root, "foreign-link"), process.platform === "win32" ? "junction" : "dir");
    await ledger.close();
    assert.equal(await readFile(join(foreign, "keep.txt"), "utf8"), "keep");
    assert.deepEqual(await readdir(directory), ["foreign"]);
  } finally { await ledger.close(); await rm(directory, { recursive: true, force: true }); }
});

test("receipt permissions inherit the installer's private Windows directory; POSIX uses 700/600", async () => {
  const directory = await mkdtemp(join(tmpdir(), "salcara-ledger-private-test-"));
  let ledger;
  const encoded = Buffer.from(directory).toString("base64");
  const run = script => execFileSync(String.raw`C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`, ["-NoProfile", "-NonInteractive", "-EncodedCommand", Buffer.from(`$ProgressPreference='SilentlyContinue';${script}`, "utf16le").toString("base64")], { windowsHide: true, encoding: "utf8", timeout: 5000, stdio: ["ignore", "pipe", "pipe"] });
  try {
    if (process.platform === "win32") run(`$ErrorActionPreference='Stop';$p=[Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('${encoded}'));$sid=[Security.Principal.WindowsIdentity]::GetCurrent().User;$a=New-Object Security.AccessControl.DirectorySecurity;$a.SetAccessRuleProtection($true,$false);$a.SetOwner($sid);foreach($s in @($sid,(New-Object Security.Principal.SecurityIdentifier('S-1-5-18')))){$a.AddAccessRule((New-Object Security.AccessControl.FileSystemAccessRule($s,'FullControl','ContainerInherit,ObjectInherit','None','Allow')))};[IO.Directory]::SetAccessControl($p,$a)`);
    ledger = await createOperationLedger(directory);
    const id = randomUUID(); await ledger.begin(id, "e".repeat(64));
    const [name] = await readdir(directory), root = join(directory, name), receipt = join(root, `${id}.jsonl`);
    if (process.platform === "win32") {
      const child = Buffer.from(receipt).toString("base64");
      const acl = JSON.parse(run(`$p=[Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('${child}'));$a=[IO.File]::GetAccessControl($p);$rules=@($a.GetAccessRules($true,$true,[Security.Principal.SecurityIdentifier]));[PSCustomObject]@{currentUser=[Security.Principal.WindowsIdentity]::GetCurrent().User.Value;owner=$a.GetOwner([Security.Principal.SecurityIdentifier]).Value;sids=@($rules|ForEach-Object{$_.IdentityReference.Value});allow=@($rules|ForEach-Object{$_.AccessControlType.ToString()})}|ConvertTo-Json -Compress`));
      // Elevated Windows builders can assign the Administrators group as the
      // child's owner. The inherited access grants must still be exclusively
      // the actual installer user and SYSTEM, never that group's membership.
      assert.deepEqual(new Set(acl.sids), new Set([acl.currentUser, "S-1-5-18"]));
      assert.ok(acl.allow.every(value => value === "Allow"));
    } else {
      assert.equal((await stat(root)).mode & 0o777, 0o700);
      assert.equal((await stat(receipt)).mode & 0o777, 0o600);
    }
  } finally { await ledger?.close(); await rmdir(directory); }
});
