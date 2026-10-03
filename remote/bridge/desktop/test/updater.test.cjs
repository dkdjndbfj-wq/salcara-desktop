const { test } = require('node:test');
const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { execFileSync, spawnSync } = require('node:child_process');
const { createUpdater, verifyManifest, newer, swapScript, installLayout } = require('../updater.cjs');

function keys() {
  const { publicKey, privateKey } = crypto.generateKeyPairSync('ed25519');
  return { pub: publicKey.export({ type: 'spki', format: 'der' }).subarray(-32).toString('base64'), privateKey };
}
function manifestFor(version, files, privateKey) {
  const body = Buffer.from(JSON.stringify({ version, notes: '- 新功能', files }));
  return { body, sig: crypto.sign(null, body, privateKey).toString('base64') };
}
const file = { name: 'Salcara-Bridge-1.6.0-linux-x64.tar.gz', sha256: 'a'.repeat(64), size: 10 };

test('versions compare numerically', () => {
  assert.equal(newer('v1.10.0', '1.9.9'), true);
  assert.equal(newer('1.5.1', '1.5.1-test.2'), false);
  assert.equal(newer('1.5.0', '1.5.1'), false);
});

test('only a correctly signed, newer manifest for this platform is accepted', () => {
  const k = keys(), other = keys();
  const m = manifestFor('1.6.0', { 'linux-x64': file }, k.privateKey);
  const ok = verifyManifest({ manifest: m.body, signature: m.sig, publicKey: k.pub, current: '1.5.1', platform: 'linux', arch: 'x64', tag: 'v1.6.0' });
  assert.equal(ok.version, '1.6.0'); assert.equal(ok.file.name, file.name);
  const bad = (args, re) => assert.throws(() => verifyManifest({ manifest: m.body, signature: m.sig, publicKey: k.pub, current: '1.5.1', platform: 'linux', arch: 'x64', tag: 'v1.6.0', ...args }), re);
  bad({ publicKey: '' }, /公钥/);
  bad({ publicKey: other.pub }, /签名不正确/);
  bad({ manifest: Buffer.concat([m.body, Buffer.from(' ')]) }, /签名不正确/);
  bad({ current: '1.6.0' }, /不是更新/);          // no reinstall / downgrade
  bad({ tag: 'v1.7.0' }, /不一致/);
  bad({ platform: 'win32' }, /适合这台电脑/);
  const evil = manifestFor('1.6.0', { 'linux-x64': { ...file, name: '../../x.tar.gz' } }, k.privateKey);
  assert.throws(() => verifyManifest({ manifest: evil.body, signature: evil.sig, publicKey: k.pub, current: '1.5.1', platform: 'linux', arch: 'x64' }), /安装包信息/);
});

test('scripts quote paths safely', () => {
  const ps = swapScript({ platform: 'win32', pid: 1, appDir: "C:\\it's", newDir: 'n', backupDir: 'b', launch: 'e', log: 'l' });
  assert.match(ps, /'C:\\it''s'/);
  const sh = swapScript({ platform: 'linux', pid: 1, appDir: "/it's", newDir: 'n', backupDir: 'b', launch: 'e', log: 'l' });
  assert.match(sh, /'\/it'\\''s'/);
  assert.equal(installLayout('/Applications/Salcara Bridge.app/Contents/MacOS/Salcara Bridge', 'darwin').appDir, '/Applications/Salcara Bridge.app');
  assert.equal(installLayout('/x/Salcara Bridge', 'darwin'), null);
});

test('end to end: check, download, verify, unpack, swap and relaunch (POSIX)', { skip: process.platform === 'win32' }, async () => {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'salcara-upd-'));
  // The installed app and the new build.
  const appDir = path.join(tmp, 'Salcara Bridge'); fs.mkdirSync(appDir);
  fs.writeFileSync(path.join(appDir, 'salcara'), '#!/bin/sh\necho launched > "$(dirname "$0")/../launched"\n', { mode: 0o755 });
  fs.writeFileSync(path.join(appDir, 'VERSION'), '1.5.1');
  const build = path.join(tmp, 'build'); fs.mkdirSync(build);
  fs.writeFileSync(path.join(build, 'salcara'), fs.readFileSync(path.join(appDir, 'salcara')), { mode: 0o755 });
  fs.writeFileSync(path.join(build, 'VERSION'), '1.6.0');
  const archive = path.join(tmp, 'a.tar.gz');
  execFileSync('tar', ['-czf', archive, '-C', build, '.']);
  const bytes = fs.readFileSync(archive);
  const k = keys();
  const name = 'Salcara-Bridge-1.6.0-linux-x64.tar.gz';
  const m = manifestFor('1.6.0', { [`${process.platform}-${process.arch}`]: { name, sha256: crypto.createHash('sha256').update(bytes).digest('hex'), size: bytes.length } }, k.privateKey);
  const dl = 'https://github.com/o/r/releases/download/v1.6.0/';
  const served = { 'latest.json': m.body, 'latest.json.sig': Buffer.from(m.sig), [name]: bytes };
  const realFetch = global.fetch;
  global.fetch = async (url) => {
    if (url.endsWith('/releases/latest')) return new Response(JSON.stringify({ tag_name: 'v1.6.0', body: 'notes', assets: Object.keys(served).map((n) => ({ name: n, browser_download_url: dl + n })) }));
    const n = url.slice(dl.length);
    return served[n] ? new Response(served[n]) : new Response('no', { status: 404 });
  };
  let quit = false, stopped = false;
  const states = [];
  try {
    const u = createUpdater({
      app: { getVersion: () => '1.5.1', quit: () => { quit = true; } }, shell: {}, platform: process.platform, arch: process.arch,
      execPath: path.join(appDir, 'salcara'), isPackaged: true, repo: 'o/r', publicKey: k.pub, userData: path.join(tmp, 'ud'),
      onChange: (s) => states.push(s.phase), beforeInstall: async () => { stopped = true; },
    });
    // Tampered archive is refused.
    served[name] = Buffer.concat([bytes.subarray(0, -1), Buffer.from([bytes.at(-1) ^ 1])]);
    const u2 = createUpdater({ app: { getVersion: () => '1.5.1', quit() {} }, shell: {}, platform: process.platform, arch: process.arch,
      execPath: path.join(appDir, 'salcara'), isPackaged: true, repo: 'o/r', publicKey: k.pub, userData: path.join(tmp, 'ud2'), onChange() {}, beforeInstall: async () => {} });
    await u2.check(true); const r2 = await u2.download();
    assert.equal(r2.phase, 'error'); assert.match(r2.error, /校验不通过/);
    served[name] = bytes;
    assert.equal((await u.check(true)).phase, 'available');
    assert.equal(u.state().canInstall, true);
    assert.equal((await u.download()).phase, 'ready');
    assert.ok(states.includes('downloading') && states.includes('verifying'));
    // Install: the script waits for this "app" (a short-lived process) and swaps.
    const fakeApp = require('node:child_process').spawn('sleep', ['0.3']);
    const pid = process.pid; Object.defineProperty(process, 'pid', { value: fakeApp.pid, configurable: true });
    try { await u.install(); } finally { Object.defineProperty(process, 'pid', { value: pid, configurable: true }); }
    assert.ok(stopped && quit);
    for (let i = 0; i < 60 && !fs.existsSync(path.join(tmp, 'launched')); i++) await new Promise((r) => setTimeout(r, 200));
    assert.equal(fs.readFileSync(path.join(appDir, 'VERSION'), 'utf8'), '1.6.0');
    assert.ok(fs.existsSync(path.join(tmp, 'launched')), 'new version was started');
    for (let i = 0; i < 50 && fs.readdirSync(tmp).some((n) => n.includes('.old-')); i++) await new Promise((r) => setTimeout(r, 200));
    assert.ok(!fs.readdirSync(tmp).some((n) => n.includes('.old-')), 'old copy removed');
  } finally {
    global.fetch = realFetch;
  }
});
