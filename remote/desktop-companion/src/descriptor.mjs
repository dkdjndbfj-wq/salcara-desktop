import { spawn } from "node:child_process";
import { constants } from "node:fs";
import { lstat, open, readFile, realpath, unlink } from "node:fs/promises";
import { dirname, join, parse, resolve } from "node:path";
import { DesktopError } from "./native-control.mjs";

export const DESCRIPTOR_NAME = "active-desktop.json";
const PRIVATE_WRITE_SCRIPT = String.raw`
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$stream = $null
$created = $false
$mutex = $null
$locked = $false
try {
  $data = [Console]::In.ReadToEnd() | ConvertFrom-Json
  $sid = [Security.Principal.WindowsIdentity]::GetCurrent().User
  # Serialize legitimate writers across per-chat MCP processes. A process exit
  # releases the kernel mutex; no stale lock file or PID inference is involved.
  $hash = [Security.Cryptography.SHA256]::Create()
  try { $name = [BitConverter]::ToString($hash.ComputeHash([Text.Encoding]::UTF8.GetBytes(([string]$data.path).ToLowerInvariant()))).Replace('-', '') }
  finally { $hash.Dispose() }
  $mutex = New-Object Threading.Mutex($false, ('Local\SalcaraDesktopDescriptor-' + $name))
  try { $locked = $mutex.WaitOne(3000) } catch [Threading.AbandonedMutexException] { $locked = $true }
  if (-not $locked) { throw 'Descriptor writer busy' }
  if ([IO.File]::Exists([string]$data.path)) {
    $info = New-Object IO.FileInfo([string]$data.path)
    $fileAcl = [IO.File]::GetAccessControl([string]$data.path)
    $rules = @($fileAcl.GetAccessRules($true, $true, [Security.Principal.SecurityIdentifier]))
    $private = $fileAcl.AreAccessRulesProtected -and ($fileAcl.GetOwner([Security.Principal.SecurityIdentifier]).Value -eq $sid.Value) -and ($rules.Count -eq 2)
    $seenSids = @()
    foreach ($rule in $rules) {
      if (($rule.AccessControlType -ne [Security.AccessControl.AccessControlType]::Allow) -or (@($sid.Value, 'S-1-5-18') -notcontains $rule.IdentityReference.Value)) { $private = $false }
      $seenSids += $rule.IdentityReference.Value
    }
    if ((@($seenSids | Select-Object -Unique).Count -ne 2) -or -not $private -or ($info.Attributes -band [IO.FileAttributes]::ReparsePoint) -or ($info.Length -gt 8192)) { throw 'Existing descriptor is not owned' }
    $old = [IO.File]::ReadAllText([string]$data.path) | ConvertFrom-Json
    $keys = @($old.PSObject.Properties.Name)
    $uuid = '^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'
    if (($keys.Count -ne 6) -or @($keys | Where-Object { @('version','port','token','expiresAt','controllerThreadId','sessionKeys') -notcontains $_ }).Count -gt 0 -or
        (-not ($old.version -is [int] -or $old.version -is [long])) -or (-not ($old.port -is [int] -or $old.port -is [long])) -or
        (-not ($old.expiresAt -is [int] -or $old.expiresAt -is [long])) -or (-not ($old.token -is [string])) -or (-not ($old.controllerThreadId -is [string])) -or (-not ($old.sessionKeys -is [array])) -or
        ($old.version -ne 1) -or ($old.port -lt 1) -or ($old.port -gt 65535) -or ($old.token -cnotmatch '^[a-f0-9]{64}$') -or
        ($old.controllerThreadId -cnotmatch $uuid) -or ($old.expiresAt -le 0) -or ($old.expiresAt -gt [DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds())) { throw 'Existing descriptor is active or foreign' }
    $sessions = @($old.sessionKeys)
    if (($sessions.Count -lt 1) -or ($sessions.Count -gt 200) -or (@($sessions | Select-Object -Unique).Count -ne $sessions.Count)) { throw 'Existing descriptor scope invalid' }
    foreach ($session in $sessions) {
      if ((-not ($session -is [string])) -or ($session -cnotmatch ('^codex:' + $uuid.Substring(1))) -or ($session -eq ('codex:' + $old.controllerThreadId))) { throw 'Existing descriptor scope invalid' }
    }
    # Only an expired, structurally valid owner-and-SYSTEM-only lease is removed.
    # A still-active lease, foreign JSON or redirected path is always retained.
    [IO.File]::Delete([string]$data.path)
  }
  $acl = New-Object Security.AccessControl.FileSecurity
  $acl.SetAccessRuleProtection($true, $false)
  $acl.SetOwner($sid)
  $acl.AddAccessRule((New-Object Security.AccessControl.FileSystemAccessRule($sid, 'FullControl', 'Allow')))
  $acl.AddAccessRule((New-Object Security.AccessControl.FileSystemAccessRule((New-Object Security.Principal.SecurityIdentifier('S-1-5-18')), 'FullControl', 'Allow')))
  $stream = New-Object IO.FileStream([string]$data.path, [IO.FileMode]::CreateNew, [Security.AccessControl.FileSystemRights]::FullControl, [IO.FileShare]::None, 4096, [IO.FileOptions]::WriteThrough, $acl)
  $created = $true
  $bytes = [Text.Encoding]::UTF8.GetBytes([string]$data.content)
  $stream.Write($bytes, 0, $bytes.Length)
  $stream.Flush($true)
  $stream.Dispose()
  $stream = $null
  [Console]::Out.Write('OK')
} catch {
  if ($null -ne $stream) { $stream.Dispose() }
  if ($created) { try { [IO.File]::Delete([string]$data.path) } catch {} }
  exit 1
} finally {
  if ($locked -and $null -ne $mutex) { $mutex.ReleaseMutex() }
  if ($null -ne $mutex) { $mutex.Dispose() }
}
`;

export async function ensureOwnDirectory(directory) {
  const absolute = resolve(directory);
  let current = absolute;
  while (current !== parse(current).root) {
    const info = await lstat(current);
    if (!info.isDirectory() || info.isSymbolicLink()) throw new DesktopError("DESCRIPTOR_UNAVAILABLE");
    current = dirname(current);
  }
  const actual = await realpath(absolute);
  if ((process.platform === "win32" ? actual.toLowerCase() : actual) !== (process.platform === "win32" ? absolute.toLowerCase() : absolute)) {
    throw new DesktopError("DESCRIPTOR_UNAVAILABLE");
  }
  return absolute;
}

function privateWindowsWrite(path, content) {
  return new Promise((resolveWrite, reject) => {
    // Fixed system helper, fixed script; path and secret are stdin JSON, never command-line arguments.
    const helper = spawn(String.raw`C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`,
      ["-NoProfile", "-NonInteractive", "-EncodedCommand", Buffer.from(PRIVATE_WRITE_SCRIPT, "utf16le").toString("base64")],
      { stdio: ["pipe", "pipe", "pipe"], windowsHide: true });
    let output = ""; let done = false;
    const timer = setTimeout(() => { helper.kill(); finish(false); }, 5000);
    const finish = (ok) => { if (done) return; done = true; clearTimeout(timer); ok ? resolveWrite() : reject(new DesktopError("DESCRIPTOR_UNAVAILABLE")); };
    helper.on("error", () => finish(false));
    helper.stdout.on("data", (chunk) => { if (output.length < 32) output += chunk.toString(); });
    helper.stderr.on("data", () => { /* No system diagnostics or paths are returned. */ });
    helper.stdin.on("error", () => finish(false));
    helper.on("close", (code) => finish(code === 0 && output === "OK"));
    helper.stdin.end(JSON.stringify({ path, content }));
  });
}

/** CreateNew/O_EXCL refuses live/foreign descriptors; Windows may retire an
 * expired private owned lease under a process-shared kernel mutex. ACL exists
 * before the new token is written. */
export async function writePrivateDescriptor(directory, descriptor) {
  try {
    const path = join(await ensureOwnDirectory(directory), DESCRIPTOR_NAME);
    const content = `${JSON.stringify(descriptor)}\n`;
    if (process.platform === "win32") await privateWindowsWrite(path, content);
    else {
      const file = await open(path, constants.O_CREAT | constants.O_EXCL | constants.O_WRONLY, 0o600);
      try { await file.writeFile(content); await file.sync(); } finally { await file.close(); }
    }
    return path;
  } catch { throw new DesktopError("DESCRIPTOR_UNAVAILABLE"); }
}

/** Only remove the exact descriptor this active lease created, never a replacement or symlink. */
export async function removeOwnedDescriptor(path, descriptor) {
  if (!path) return;
  try {
    const info = await lstat(path);
    if (!info.isFile() || info.isSymbolicLink() || info.size > 8192) return;
    const current = JSON.parse(await readFile(path, "utf8"));
    if (current.token === descriptor.token && current.port === descriptor.port && current.expiresAt === descriptor.expiresAt) await unlink(path);
  } catch { /* A missing or foreign descriptor is retained, not overwritten. */ }
}
