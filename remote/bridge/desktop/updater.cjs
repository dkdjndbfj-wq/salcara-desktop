'use strict';
/*
 * Automatic updates for the packaged desktop app.
 *
 * A release on GitHub carries three assets:
 *   latest.json      { version, notes, files: { "<platform>-<arch>": { name, sha256, size } } }
 *   latest.json.sig  Ed25519 signature of latest.json (base64), made by scripts/release.cjs
 *   <name>.tar.gz    the packaged app for one platform
 *
 * The app installs only what the publisher signed: the manifest must verify against the public key
 * in package.json (salcaraUpdatePublicKey), must be newer than the running version, and the archive
 * must match the signed size and SHA-256. Without a key, or when the install folder is not writable,
 * the window offers the release page instead of installing.
 *
 * Installing swaps whole folders next to the current install (same volume, so it is a rename), after
 * the app and its core have quit; a small detached script does the swap and starts the new version.
 * If the swap cannot happen the old version is put back and started again.
 *
 * Nothing here touches Electron at load time; main.cjs passes in what it needs.
 */
const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');
const os = require('node:os');
const { spawn, execFile } = require('node:child_process');
const { validateArchive, assertPrivateTree } = require('./update-archive.cjs');

const ED25519_SPKI_PREFIX = Buffer.from('302a300506032b6570032100', 'hex');
const MAX_SMALL_ASSET = 256 * 1024;
const MAX_ARCHIVE = 2 * 1024 * 1024 * 1024;

/* ---------- pure helpers (unit tested) ---------- */
function versionParts(v) { return String(v || '').replace(/^v/, '').split(/[.+-]/).slice(0, 3).map((x) => Number.parseInt(x, 10) || 0); }
function newer(a, b) {
  const x = versionParts(a), y = versionParts(b);
  for (let i = 0; i < 3; i++) if (x[i] !== y[i]) return x[i] > y[i];
  return false;
}
function publicKeyFrom(b64) {
  const raw = Buffer.from(String(b64 || '').trim(), 'base64');
  if (raw.length !== 32) return null;
  try { return crypto.createPublicKey({ key: Buffer.concat([ED25519_SPKI_PREFIX, raw]), format: 'der', type: 'spki' }); } catch { return null; }
}
class UpdateError extends Error {}

/** Checks a downloaded manifest and returns what to install for this computer. Throws UpdateError. */
function verifyManifest({ manifest, signature, publicKey, current, platform, arch, tag }) {
  const key = publicKeyFrom(publicKey);
  if (!key) throw new UpdateError('这个版本没有配置更新签名公钥，不能自动安装');
  const sig = Buffer.from(String(signature || '').trim(), 'base64');
  if (sig.length !== 64 || !crypto.verify(null, Buffer.from(manifest), key, sig)) throw new UpdateError('更新文件的签名不正确，已拒绝安装');
  let data;
  try { data = JSON.parse(Buffer.from(manifest).toString('utf8')); } catch { throw new UpdateError('更新清单格式不正确'); }
  if (!data || data.schema !== 1 || data.product !== 'salcara-desktop') throw new UpdateError('更新清单不属于桌面程序');
  const version = String(data && data.version || '');
  if (!/^\d+\.\d+\.\d+$/.test(version)) throw new UpdateError('更新清单里的版本号不正确');
  if (tag && String(tag).replace(/^v/, '') !== version) throw new UpdateError('更新清单和发布版本不一致');
  if (!newer(version, current)) throw new UpdateError('这不是更新的版本');
  const file = data.files && Object.prototype.hasOwnProperty.call(data.files, `${platform}-${arch}`) ? data.files[`${platform}-${arch}`] : null;
  if (!file) throw new UpdateError('这个新版本没有提供适合这台电脑的安装包');
  if (!/^[A-Za-z0-9._-]+\.tar\.gz$/.test(String(file.name || '')) || !/^[0-9a-f]{64}$/.test(String(file.sha256 || ''))
    || !Number.isInteger(file.size) || file.size <= 0 || file.size > MAX_ARCHIVE) throw new UpdateError('更新清单里的安装包信息不正确');
  const notes = typeof data.notes === 'string' ? data.notes.slice(0, 8000) : '';
  return { version, notes, file: { name: file.name, sha256: file.sha256, size: file.size } };
}

/** Where the running app lives and what to start after the swap. */
function installLayout(execPath, platform) {
  const paths = platform === 'win32' ? path.win32 : path.posix;
  if (platform === 'darwin') {
    const appDir = paths.resolve(execPath, '../../..');
    if (!appDir.endsWith('.app')) return null;
    return { appDir, launch: appDir };
  }
  return { appDir: paths.dirname(execPath), launch: execPath };
}

function psQuote(s) { return `'${String(s).replace(/'/g, "''")}'`; }
function shQuote(s) { return `'${String(s).replace(/'/g, "'\\''")}'`; }

/** The detached script that swaps the folders once the app has quit. */
function swapScript({ platform, pid, appDir, newDir, backupDir, launch, log, permit, receipt, token }) {
  if (platform === 'win32') {
    return [
      "$ErrorActionPreference = 'Stop'",
      `$app = ${psQuote(appDir)}; $new = ${psQuote(newDir)}; $bak = ${psQuote(backupDir)}; $exe = ${psQuote(launch)}; $log = ${psQuote(log)}; $permit = ${psQuote(permit || '')}; $receipt = ${psQuote(receipt || '')}; $token = ${psQuote(token || '')}`,
      'function Log($m) { try { Add-Content -LiteralPath $log -Value ("{0:o} {1}" -f (Get-Date), $m) } catch {} }',
      `try { Wait-Process -Id ${Number(pid)} -Timeout 60 -ErrorAction SilentlyContinue } catch {}`,
      `if (Get-Process -Id ${Number(pid)} -ErrorAction SilentlyContinue) { Log 'original app did not exit; update skipped'; exit 1 }`,
      "if (-not $permit -or -not (Test-Path -LiteralPath $permit) -or [IO.File]::ReadAllText($permit) -ne $token) { Log 'update not permitted'; exit 1 }",
      '$app = [IO.Path]::GetFullPath($app); $new = [IO.Path]::GetFullPath($new); $bak = [IO.Path]::GetFullPath($bak)',
      '$parent = [IO.Path]::GetDirectoryName($app); $stage = [IO.Path]::Combine($parent, "." + [IO.Path]::GetFileName($app) + ".update")',
      'if (-not $parent -or -not $bak.StartsWith($app + ".old-", [StringComparison]::OrdinalIgnoreCase) -or -not $new.StartsWith($stage + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) { Log "unsafe update scope"; exit 1 }',
      // The core and helper processes may still hold files for a moment: retry the rename for up to a minute.
      '$moved = $false',
      'for ($i = 0; $i -lt 120; $i++) { try { [IO.Directory]::Move($app, $bak); $moved = $true; break } catch { Start-Sleep -Milliseconds 500 } }',
      "if (-not $moved) { Log 'install folder still in use; update skipped'; Start-Process -FilePath $exe -WindowStyle Hidden; exit 1 }",
      'try { [IO.Directory]::Move($new, $app) } catch { Log "swap failed: $_"; [IO.Directory]::Move($bak, $app); Start-Process -FilePath $exe -WindowStyle Hidden; exit 1 }',
      '$started = $null; try { $started = Start-Process -FilePath $exe -WindowStyle Hidden -PassThru } catch { Log "new app launch failed: $_" }',
      '$ready = $false; for ($i = 0; $i -lt 120; $i++) { if ((Test-Path -LiteralPath $receipt) -and [IO.File]::ReadAllText($receipt) -eq $token) { $ready = $true; break }; if (-not $started -or $started.HasExited) { break }; Start-Sleep -Milliseconds 500 }',
      'if ($ready) { Log "updated and healthy"; try { Remove-Item -LiteralPath $bak -Recurse -Force } catch { Log "old copy retained" }; exit 0 }',
      'if ($started -and -not $started.HasExited) { Log "new app did not acknowledge healthy startup; backup retained without killing it"; exit 1 }',
      'try { [IO.Directory]::Move($app, $new); [IO.Directory]::Move($bak, $app); Start-Process -FilePath $exe -WindowStyle Hidden; Log "rolled back" } catch { Log "rollback requires recovery: $_" }; exit 1',
      '',
    ].join('\r\n');
  }
  const start = platform === 'darwin' ? 'open -W "$app" >/dev/null 2>&1 &' : '"$exe" >/dev/null 2>&1 &';
  return [
    '#!/bin/sh',
    `app=${shQuote(appDir)}; new=${shQuote(newDir)}; bak=${shQuote(backupDir)}; exe=${shQuote(launch)}; log=${shQuote(log)}; permit=${shQuote(permit || '')}; receipt=${shQuote(receipt || '')}; token=${shQuote(token || '')}`,
    `i=0; while kill -0 ${Number(pid)} 2>/dev/null && [ $i -lt 120 ]; do sleep 0.5; i=$((i+1)); done`,
    `if kill -0 ${Number(pid)} 2>/dev/null; then echo 'original app did not exit' >> "$log"; exit 1; fi`,
    '[ -n "$permit" ] && [ "$(cat "$permit" 2>/dev/null)" = "$token" ] || exit 1',
    'parent=$(dirname "$app"); base=$(basename "$app"); [ "$parent" != "$app" ] && [ "$parent" != "/" ] || exit 1',
    'case "$bak" in "$app".old-[0-9]*) ;; *) exit 1 ;; esac',
    'case "$new" in "$parent/.$base.update/"*) ;; *) exit 1 ;; esac',
    'if mv "$app" "$bak"; then',
    '  if ! mv "$new" "$app"; then mv "$bak" "$app"; ' + start + ' exit 1; fi',
    'else echo "$(date) install folder busy; update skipped" >> "$log"; ' + start + ' exit 1; fi',
    start,
    'child=$!; i=0; while [ $i -lt 120 ]; do',
    '  if [ "$(cat "$receipt" 2>/dev/null)" = "$token" ]; then echo "updated and healthy" >> "$log"; rm -rf "$bak"; exit 0; fi',
    '  kill -0 "$child" 2>/dev/null || break; sleep 0.5; i=$((i+1)); done',
    'if kill -0 "$child" 2>/dev/null; then echo "startup unacknowledged; backup retained" >> "$log"; exit 1; fi',
    'if mv "$app" "$new" && mv "$bak" "$app"; then echo "rolled back" >> "$log"; ' + start + ' else echo "rollback requires recovery" >> "$log"; fi',
    '',
  ].join('\n');
}

/* ---------- network ---------- */
async function fetchSmall(url, signal) {
  const res = await fetch(url, { headers: { 'User-Agent': 'SalcaraBridge' }, signal, redirect: 'follow' });
  if (!res.ok) throw new Error(`HTTP ${res.status}`);
  if (!res.body) throw new Error('Empty update response');
  const chunks = []; let size = 0;
  const reader = res.body.getReader();
  try {
    for (;;) { const next = await reader.read(); if (next.done) break; size += next.value.length;
      if (size > MAX_SMALL_ASSET) throw new UpdateError('更新清单过大'); chunks.push(Buffer.from(next.value)); }
  } finally { await reader.cancel().catch(() => {}); }
  return Buffer.concat(chunks, size);
}

async function downloadTo(url, dest, { size, sha256, signal, onProgress }) {
  const res = await fetch(url, { headers: { 'User-Agent': 'SalcaraBridge' }, signal, redirect: 'follow' });
  if (!res.ok || !res.body) throw new Error(`HTTP ${res.status}`);
  const hash = crypto.createHash('sha256');
  const out = await fs.promises.open(dest, 'wx', 0o600);
  let received = 0;
  const reader = res.body.getReader();
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      received += value.length;
      if (received > size) throw new UpdateError('下载的文件大小和发布信息不一致');
      hash.update(value);
      let offset = 0;
      while (offset < value.length) { const written = await out.write(value, offset, value.length - offset); if (!written.bytesWritten) throw new Error('Update write stalled'); offset += written.bytesWritten; }
      onProgress(received);
    }
  } finally {
    await reader.cancel().catch(() => {});
    await out.close();
  }
  if (received !== size) throw new UpdateError('下载没有完成，请重试');
  if (hash.digest('hex') !== sha256) throw new UpdateError('下载的文件校验不通过，已删除');
}

function run(file, args, signal) {
  return new Promise((resolve, reject) => execFile(file, args, { windowsHide: true, timeout: 10 * 60 * 1000, signal }, (error) => (error ? reject(error) : resolve())));
}
function tarPath(platform) {
  // Windows 10 1803 and later ship bsdtar; call it by full path rather than trusting PATH.
  return platform === 'win32' ? path.join(process.env.SystemRoot || 'C:\\Windows', 'System32', 'tar.exe') : 'tar';
}

/* ---------- the updater ---------- */
/**
 * deps: { app, shell, platform, arch, execPath, isPackaged, publicKey, repo, userData, onChange(state),
 *         beforeInstall(): Promise<void> }
 */
function createUpdater(deps) {
  const platform = deps.platform, arch = deps.arch;
  const layout = deps.isPackaged ? installLayout(deps.execPath, platform) : null;
  const stateFile = path.join(deps.userData, 'update.json');
  let st = { phase: 'idle', current: deps.app.getVersion() };
  let release = null; // { version, notes, file, archiveUrl, page }
  let controller = null;
  let staged = null;  // { newDir, version }
  let checking = null;

  function read() { try { return JSON.parse(fs.readFileSync(stateFile, 'utf8')) || {}; } catch { return {}; } }
  function write(patch) {
    try { fs.mkdirSync(path.dirname(stateFile), { recursive: true }); fs.writeFileSync(stateFile, JSON.stringify({ ...read(), ...patch })); } catch { /* best effort */ }
  }
  function set(patch) { st = { ...st, ...patch, current: deps.app.getVersion() }; deps.onChange(st); }

  function stagingRoot() { return layout ? path.join(path.dirname(layout.appDir), `.${path.basename(layout.appDir)}.update`) : null; }
  function canWrite() {
    if (!layout) return false;
    try { assertPrivateTree(layout.appDir); const marker = JSON.parse(fs.readFileSync(path.join(layout.appDir, '.salcara-install.json'), 'utf8'));
      if (marker.product !== 'salcara-desktop' || marker.version !== deps.app.getVersion()) return false;
      fs.accessSync(path.dirname(layout.appDir), fs.constants.W_OK); fs.accessSync(layout.appDir, fs.constants.W_OK); return true;
    } catch { return false; }
  }

  /** Leftovers of an earlier update (the staging folder and backups of old versions). */
  function cleanup() {
    if (!layout) return;
    // Only the swap helper deletes its exact backup after healthy startup.
    // Never sweep sibling folders or delete recovery data before acknowledgement.
    if (read().installing || ['downloading', 'verifying', 'ready', 'installing'].includes(st.phase)) return;
    try { assertPrivateTree(layout.appDir); const root = stagingRoot(); if (fs.existsSync(root)) { assertPrivateTree(root); fs.rmSync(root, { recursive: true }); } } catch { /* leave unsafe paths alone */ }
  }

  async function checkOnce(manual) {
    if (!deps.repo) { set({ phase: 'unconfigured' }); return st; }
    if (!/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(deps.repo)) { set({ phase: 'unconfigured' }); return st; }
    if (['downloading', 'verifying', 'ready', 'installing'].includes(st.phase)) return st;
    if (manual) set({ phase: 'checking', error: '' });
    try {
      const res = await fetch(`https://api.github.com/repos/${deps.repo}/releases/latest`, {
        headers: { Accept: 'application/vnd.github+json', 'User-Agent': 'SalcaraBridge' }, signal: AbortSignal.timeout(15000) });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const rel = await res.json();
      const tag = String(rel.tag_name || '');
      const page = `https://github.com/${deps.repo}/releases/tag/${encodeURIComponent(tag)}`;
      if (!/^v\d+\.\d+\.\d+$/.test(tag) || rel.draft || rel.prerelease || !newer(tag, st.current)) { release = null; set({ phase: 'latest', version: '', page: '', canInstall: false }); return st; }
      const prefix = `https://github.com/${deps.repo}/releases/download/${tag}/`;
      const asset = (name) => (rel.assets || []).find((a) => a && a.name === name && a.browser_download_url === prefix + name);
      const base = { version: tag.replace(/^v/, ''), notes: String(rel.body || '').slice(0, 8000), page, size: 0, canInstall: false, reason: '' };
      const m = asset('latest.json'), s = asset('latest.json.sig');
      if (!m || !s) { release = base; set({ phase: 'available', ...base, reason: '这个版本没有提供自动更新包，请到发布页下载' }); return st; }
      const signal = AbortSignal.timeout(20000);
      const [manifest, signature] = await Promise.all([fetchSmall(m.browser_download_url, signal), fetchSmall(s.browser_download_url, signal)]);
      const ok = verifyManifest({ manifest, signature: signature.toString('utf8'), publicKey: deps.publicKey, current: st.current, platform, arch, tag });
      if (JSON.parse(manifest.toString('utf8')).repo !== deps.repo) throw new UpdateError('更新清单不属于这个仓库');
      const archive = asset(ok.file.name);
      if (!archive) throw new UpdateError('发布里缺少安装包');
      let reason = '';
      if (!deps.isPackaged) reason = '开发模式下不能自动安装';
      else if (!layout) reason = '无法确定安装位置';
      else if (!canWrite()) reason = '安装文件夹没有写入权限，请到发布页下载';
      release = { ...base, version: ok.version, notes: ok.notes || base.notes, file: ok.file, archiveUrl: archive.browser_download_url };
      set({ phase: 'available', version: release.version, notes: release.notes, page, size: ok.file.size, canInstall: !reason, reason, error: '' });
      return st;
    } catch (error) {
      release = null;
      st.canInstall = false;
      if (error instanceof UpdateError) { set({ phase: 'error', error: error.message, canRetry: false, retry: '' }); return st; }
      if (manual) set({ phase: 'error', error: '检查更新失败，请检查网络后重试', canRetry: true, retry: 'check' });
      else if (st.phase === 'checking') set({ phase: 'idle' });
      return st;
    }
  }

  function check(manual) {
    if (!checking) checking = checkOnce(manual).finally(() => { checking = null; });
    return checking;
  }

  async function download() {
    if (!release || !release.file || !st.canInstall || ['downloading', 'verifying', 'ready', 'installing'].includes(st.phase)) return st;
    const root = stagingRoot();
    controller = new AbortController();
    const started = Date.now();
    let last = 0;
    set({ phase: 'downloading', received: 0, speed: 0, error: '' });
    try {
      assertPrivateTree(layout.appDir);
      if (fs.existsSync(root)) assertPrivateTree(root);
      await fs.promises.rm(root, { recursive: true, force: true });
      await fs.promises.mkdir(root, { recursive: true, mode: 0o700 });
      const archive = path.join(root, release.file.name);
      await downloadTo(release.archiveUrl, archive, {
        size: release.file.size, sha256: release.file.sha256, signal: controller.signal,
        onProgress(received) {
          const now = Date.now();
          if (now - last < 120 && received < release.file.size) return;
          last = now;
          set({ received, speed: Math.round(received / Math.max(0.2, (now - started) / 1000)) });
        },
      });
      set({ phase: 'verifying', received: release.file.size });
      controller.signal.throwIfAborted();
      await validateArchive(archive, controller.signal);
      const newDir = path.join(root, 'app');
      await fs.promises.mkdir(newDir);
      await run(tarPath(platform), ['-xzf', archive, '-C', newDir], controller.signal);
      controller.signal.throwIfAborted();
      await fs.promises.rm(archive, { force: true });
      let target = newDir;
      if (platform === 'darwin') {
        const apps = (await fs.promises.readdir(newDir)).filter((n) => n.endsWith('.app'));
        if (apps.length !== 1) throw new UpdateError('安装包内容不正确');
        target = path.join(newDir, apps[0]);
      } else if (!fs.existsSync(path.join(newDir, path.basename(deps.execPath)))) {
        throw new UpdateError('安装包内容不正确');
      }
      const marker = JSON.parse(await fs.promises.readFile(path.join(target, '.salcara-install.json'), 'utf8'));
      if (marker.product !== 'salcara-desktop' || marker.version !== release.version) throw new UpdateError('安装包内容不正确');
      controller.signal.throwIfAborted();
      staged = { newDir: target, version: release.version };
      set({ phase: 'ready' });
    } catch (error) {
      try { assertPrivateTree(root); fs.rmSync(root, { recursive: true, force: true }); } catch { /* do not follow linked paths */ }
      if (controller && controller.signal.aborted) set({ phase: 'available', received: 0 });
      else set({ phase: 'error', error: error instanceof UpdateError ? error.message : '下载失败，请检查网络后重试', canRetry: true, retry: 'download' });
    } finally {
      controller = null;
    }
    return st;
  }

  function cancel() { if (controller) controller.abort(); }

  async function install() {
    if (st.phase !== 'ready' || !staged || !layout) return st;
    set({ phase: 'installing' });
    const backupDir = `${layout.appDir}.old-${Date.now()}`;
    const log = path.join(deps.userData, 'update.log');
    const script = path.join(os.tmpdir(), `salcara-update-${process.pid}-${Date.now()}${platform === 'win32' ? '.ps1' : '.sh'}`);
    const token = crypto.randomBytes(32).toString('hex');
    const permit = path.join(stagingRoot(), 'install-permit');
    const receipt = path.join(deps.userData, `update-ready-${token}`);
    try {
      assertPrivateTree(layout.appDir); assertPrivateTree(staged.newDir);
      fs.writeFileSync(script, (platform === 'win32' ? '\ufeff' : '') + swapScript({ platform, pid: process.pid, appDir: layout.appDir, newDir: staged.newDir, backupDir, launch: layout.launch, log, permit, receipt, token }), { mode: 0o700, flag: 'wx' });
      await deps.beforeInstall();
      const child = platform === 'win32'
      ? spawn(path.join(process.env.SystemRoot || 'C:\\Windows', 'System32', 'WindowsPowerShell', 'v1.0', 'powershell.exe'),
        ['-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass', '-WindowStyle', 'Hidden', '-File', script], { detached: true, stdio: 'ignore', windowsHide: true })
      : spawn('/bin/sh', [script], { detached: true, stdio: 'ignore' });
      await new Promise((resolve, reject) => { child.once('spawn', resolve); child.once('error', reject); });
      child.unref();
      fs.mkdirSync(deps.userData, { recursive: true, mode: 0o700 });
      fs.writeFileSync(stateFile, JSON.stringify({ ...read(), installing: staged.version, receipt, token }), { mode: 0o600 });
      fs.writeFileSync(permit, token, { mode: 0o600, flag: 'wx' });
      if (deps.commitInstall) await deps.commitInstall();
      deps.app.quit();
    } catch (error) {
      try { fs.unlinkSync(permit); } catch { /* never leave a permission after an aborted install */ }
      if (read().token === token) write({ installing: '', receipt: '', token: '' });
      if (deps.installAborted) await deps.installAborted();
      set({ phase: 'error', error: String(error.message || error), canRetry: true, retry: 'download' });
    }
    return st;
  }

  function skip() { if (st.version) write({ skipped: st.version }); }
  function skipped(version) { return read().skipped === version; }
  function openPage() { if (st.page) deps.shell.openExternal(st.page); }
  /** After a restart: say whether the last update went in. */
  function afterRestart() {
    const saved = read(); const pending = saved.installing;
    if (!pending) return null;
    const ok = pending === deps.app.getVersion();
    if (ok && /^[0-9a-f]{64}$/.test(saved.token || '') && saved.receipt === path.join(deps.userData, `update-ready-${saved.token}`)) {
      try { fs.writeFileSync(saved.receipt, saved.token, { mode: 0o600, flag: 'wx' }); } catch { return { version: pending, ok: false }; }
    }
    write({ installing: '', receipt: '', token: '' });
    return { version: pending, ok };
  }

  return { check, download, cancel, install, skip, skipped, openPage, cleanup, afterRestart, state: () => st };
}

module.exports = { createUpdater, verifyManifest, newer, versionParts, publicKeyFrom, swapScript, installLayout, UpdateError };
