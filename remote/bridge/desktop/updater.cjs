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
  if (platform === 'darwin') {
    const appDir = path.resolve(execPath, '../../..');
    if (!appDir.endsWith('.app')) return null;
    return { appDir, launch: appDir };
  }
  return { appDir: path.dirname(execPath), launch: execPath };
}

function psQuote(s) { return `'${String(s).replace(/'/g, "''")}'`; }
function shQuote(s) { return `'${String(s).replace(/'/g, "'\\''")}'`; }

/** The detached script that swaps the folders once the app has quit. */
function swapScript({ platform, pid, appDir, newDir, backupDir, launch, log }) {
  if (platform === 'win32') {
    return [
      "$ErrorActionPreference = 'Stop'",
      `$app = ${psQuote(appDir)}; $new = ${psQuote(newDir)}; $bak = ${psQuote(backupDir)}; $exe = ${psQuote(launch)}; $log = ${psQuote(log)}`,
      'function Log($m) { try { Add-Content -LiteralPath $log -Value ("{0:o} {1}" -f (Get-Date), $m) } catch {} }',
      `try { Wait-Process -Id ${Number(pid)} -Timeout 60 -ErrorAction SilentlyContinue } catch {}`,
      // The core and helper processes may still hold files for a moment: retry the rename for up to a minute.
      '$moved = $false',
      'for ($i = 0; $i -lt 120; $i++) { try { [IO.Directory]::Move($app, $bak); $moved = $true; break } catch { Start-Sleep -Milliseconds 500 } }',
      "if (-not $moved) { Log 'install folder still in use; update skipped'; Start-Process -FilePath $exe; exit 1 }",
      'try { [IO.Directory]::Move($new, $app) } catch { Log "swap failed: $_"; [IO.Directory]::Move($bak, $app); Start-Process -FilePath $exe; exit 1 }',
      "Log 'updated'",
      'Start-Process -FilePath $exe',
      'Start-Sleep -Seconds 5',
      'try { Remove-Item -LiteralPath $bak -Recurse -Force } catch { Log "old copy left at $bak" }',
      '',
    ].join('\r\n');
  }
  const start = platform === 'darwin' ? 'open "$app"' : '"$exe" >/dev/null 2>&1 &';
  return [
    '#!/bin/sh',
    `app=${shQuote(appDir)}; new=${shQuote(newDir)}; bak=${shQuote(backupDir)}; exe=${shQuote(launch)}; log=${shQuote(log)}`,
    `i=0; while kill -0 ${Number(pid)} 2>/dev/null && [ $i -lt 120 ]; do sleep 0.5; i=$((i+1)); done`,
    'if mv "$app" "$bak"; then',
    '  if mv "$new" "$app"; then echo "$(date) updated" >> "$log"; else echo "$(date) swap failed" >> "$log"; mv "$bak" "$app"; fi',
    'else echo "$(date) install folder busy; update skipped" >> "$log"; fi',
    start,
    'sleep 5; rm -rf "$bak"',
    '',
  ].join('\n');
}

/* ---------- network ---------- */
async function fetchSmall(url, signal) {
  const res = await fetch(url, { headers: { 'User-Agent': 'SalcaraBridge' }, signal, redirect: 'follow' });
  if (!res.ok) throw new Error(`HTTP ${res.status}`);
  const buf = Buffer.from(await res.arrayBuffer());
  if (buf.length > MAX_SMALL_ASSET) throw new UpdateError('更新清单过大');
  return buf;
}

async function downloadTo(url, dest, { size, sha256, signal, onProgress }) {
  const res = await fetch(url, { headers: { 'User-Agent': 'SalcaraBridge' }, signal, redirect: 'follow' });
  if (!res.ok || !res.body) throw new Error(`HTTP ${res.status}`);
  const hash = crypto.createHash('sha256');
  const out = fs.createWriteStream(dest);
  let received = 0;
  try {
    const reader = res.body.getReader();
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      received += value.length;
      if (received > size) throw new UpdateError('下载的文件大小和发布信息不一致');
      hash.update(value);
      if (!out.write(value)) await new Promise((r) => out.once('drain', r));
      onProgress(received);
    }
  } finally {
    await new Promise((r) => out.end(r));
  }
  if (received !== size) throw new UpdateError('下载没有完成，请重试');
  if (hash.digest('hex') !== sha256) throw new UpdateError('下载的文件校验不通过，已删除');
}

function run(file, args) {
  return new Promise((resolve, reject) => execFile(file, args, { windowsHide: true, timeout: 10 * 60 * 1000 }, (error) => (error ? reject(error) : resolve())));
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

  function read() { try { return JSON.parse(fs.readFileSync(stateFile, 'utf8')) || {}; } catch { return {}; } }
  function write(patch) {
    try { fs.mkdirSync(path.dirname(stateFile), { recursive: true }); fs.writeFileSync(stateFile, JSON.stringify({ ...read(), ...patch })); } catch { /* best effort */ }
  }
  function set(patch) { st = { ...st, ...patch, current: deps.app.getVersion() }; deps.onChange(st); }

  function stagingRoot() { return layout ? path.join(path.dirname(layout.appDir), `.${path.basename(layout.appDir)}.update`) : null; }
  function canWrite() {
    if (!layout) return false;
    try { fs.accessSync(path.dirname(layout.appDir), fs.constants.W_OK); fs.accessSync(layout.appDir, fs.constants.W_OK); return true; } catch { return false; }
  }

  /** Leftovers of an earlier update (the staging folder and backups of old versions). */
  function cleanup() {
    if (!layout) return;
    const parent = path.dirname(layout.appDir), base = path.basename(layout.appDir);
    let names = [];
    try { names = fs.readdirSync(parent); } catch { return; }
    for (const name of names) {
      if (name === `.${base}.update` || (name.startsWith(`${base}.old-`) && /^\d+$/.test(name.slice(base.length + 5)))) {
        fs.rm(path.join(parent, name), { recursive: true, force: true }, () => {});
      }
    }
  }

  async function check(manual) {
    if (!deps.repo) { set({ phase: 'unconfigured' }); return st; }
    if (['downloading', 'verifying', 'ready', 'installing'].includes(st.phase)) return st;
    if (manual) set({ phase: 'checking', error: '' });
    try {
      const res = await fetch(`https://api.github.com/repos/${deps.repo}/releases/latest`, {
        headers: { Accept: 'application/vnd.github+json', 'User-Agent': 'SalcaraBridge' }, signal: AbortSignal.timeout(15000) });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const rel = await res.json();
      const tag = String(rel.tag_name || '');
      const page = `https://github.com/${deps.repo}/releases/tag/${encodeURIComponent(tag)}`;
      if (!tag || rel.draft || rel.prerelease || !newer(tag, st.current)) { release = null; set({ phase: 'latest', version: '', page: '' }); return st; }
      const prefix = `https://github.com/${deps.repo}/releases/download/`;
      const asset = (name) => (rel.assets || []).find((a) => a && a.name === name && String(a.browser_download_url || '').startsWith(prefix));
      const base = { version: tag.replace(/^v/, ''), notes: String(rel.body || '').slice(0, 8000), page, size: 0, canInstall: false, reason: '' };
      const m = asset('latest.json'), s = asset('latest.json.sig');
      if (!m || !s) { release = base; set({ phase: 'available', ...base, reason: '这个版本没有提供自动更新包，请到发布页下载' }); return st; }
      const signal = AbortSignal.timeout(20000);
      const [manifest, signature] = await Promise.all([fetchSmall(m.browser_download_url, signal), fetchSmall(s.browser_download_url, signal)]);
      const ok = verifyManifest({ manifest, signature: signature.toString('utf8'), publicKey: deps.publicKey, current: st.current, platform, arch, tag });
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
      if (error instanceof UpdateError) { set({ phase: 'error', error: error.message, canRetry: false, retry: '' }); return st; }
      if (manual) set({ phase: 'error', error: '检查更新失败，请检查网络后重试', canRetry: true, retry: 'check' });
      else if (st.phase === 'checking') set({ phase: 'idle' });
      return st;
    }
  }

  async function download() {
    if (!release || !release.file || !st.canInstall || ['downloading', 'verifying', 'ready', 'installing'].includes(st.phase)) return st;
    const root = stagingRoot();
    controller = new AbortController();
    const started = Date.now();
    let last = 0;
    set({ phase: 'downloading', received: 0, speed: 0, error: '' });
    try {
      await fs.promises.rm(root, { recursive: true, force: true });
      await fs.promises.mkdir(root, { recursive: true });
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
      const newDir = path.join(root, 'app');
      await fs.promises.mkdir(newDir);
      await run(tarPath(platform), ['-xzf', archive, '-C', newDir]);
      await fs.promises.rm(archive, { force: true });
      let target = newDir;
      if (platform === 'darwin') {
        const apps = (await fs.promises.readdir(newDir)).filter((n) => n.endsWith('.app'));
        if (apps.length !== 1) throw new UpdateError('安装包内容不正确');
        target = path.join(newDir, apps[0]);
      } else if (!fs.existsSync(path.join(newDir, path.basename(deps.execPath)))) {
        throw new UpdateError('安装包内容不正确');
      }
      staged = { newDir: target, version: release.version };
      set({ phase: 'ready' });
    } catch (error) {
      fs.rm(root, { recursive: true, force: true }, () => {});
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
    fs.writeFileSync(script, swapScript({ platform, pid: process.pid, appDir: layout.appDir, newDir: staged.newDir, backupDir, launch: layout.launch, log }), { mode: 0o700 });
    await deps.beforeInstall();
    const child = platform === 'win32'
      ? spawn(path.join(process.env.SystemRoot || 'C:\\Windows', 'System32', 'WindowsPowerShell', 'v1.0', 'powershell.exe'),
        ['-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass', '-WindowStyle', 'Hidden', '-File', script], { detached: true, stdio: 'ignore', windowsHide: true })
      : spawn('/bin/sh', [script], { detached: true, stdio: 'ignore' });
    child.unref();
    write({ installing: staged.version });
    deps.app.quit();
    return st;
  }

  function skip() { if (st.version) write({ skipped: st.version }); }
  function skipped(version) { return read().skipped === version; }
  function openPage() { if (st.page) deps.shell.openExternal(st.page); }
  /** After a restart: say whether the last update went in. */
  function afterRestart() {
    const pending = read().installing;
    if (!pending) return null;
    write({ installing: '' });
    return { version: pending, ok: !newer(pending, deps.app.getVersion()) };
  }

  return { check, download, cancel, install, skip, skipped, openPage, cleanup, afterRestart, state: () => st };
}

module.exports = { createUpdater, verifyManifest, newer, versionParts, publicKeyFrom, swapScript, installLayout, UpdateError };
