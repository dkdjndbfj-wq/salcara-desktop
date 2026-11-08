'use strict';
const { test } = require('node:test');
const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { createUpdater, verifyManifest } = require('../updater.cjs');
const { corePolicy, summary, announcementKey, createPolicySync } = require('../update-policy.cjs');

function fixture(t) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'salcara-required-update-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const keys = crypto.generateKeyPairSync('ed25519');
  const publicKey = keys.publicKey.export({ type: 'spki', format: 'der' }).subarray(-32).toString('base64');
  const data = { schema: 1, product: 'salcara-desktop', repo: 'o/r', version: '1.7.0', notes: '- 更新远程协议',
    minimumSupportedVersion: '1.6.5', updateReason: '旧协议已停止支持',
    files: { 'win32-x64': { name: 'Salcara-Bridge-1.7.0-win32-x64.tar.gz', sha256: 'a'.repeat(64), size: 10 } } };
  let offline = false, missing = false, badSignature = false, tag = 'v1.7.0';
  const signed = () => { const manifest = Buffer.from(JSON.stringify(data)); return { manifest, signature: crypto.sign(null, manifest, keys.privateKey).toString('base64') }; };
  const previous = global.fetch;
  global.fetch = async url => {
    if (offline) throw new Error('offline');
    const prefix = `https://github.com/o/r/releases/download/${tag}/`;
    if (url.endsWith('/releases/latest')) return new Response(JSON.stringify({ tag_name: tag,
      assets: (missing ? [] : ['latest.json', 'latest.json.sig', data.files['win32-x64'].name]).map(name => ({ name, browser_download_url: prefix + name })) }));
    const value = signed();
    if (url.endsWith('/latest.json')) return new Response(value.manifest);
    if (url.endsWith('/latest.json.sig')) return new Response(badSignature ? 'invalid' : value.signature);
    throw new Error('unexpected download');
  };
  t.after(() => { global.fetch = previous; });
  const deps = { app: { getVersion: () => '1.6.4', quit() { throw new Error('must not quit'); } }, shell: { openExternal() {} },
    platform: 'win32', arch: 'x64', isPackaged: false, repo: 'o/r', publicKey, userData: dir, onChange() {} };
  return { dir, data, deps, signed, create: () => createUpdater(deps),
    verify: (extra = {}) => verifyManifest({ ...signed(), publicKey, current: '1.6.4', platform: 'win32', arch: 'x64', tag: 'v1.7.0', ...extra }),
    network: options => { ({ offline = offline, missing = missing, badSignature = badSignature, tag = tag } = options); } };
}

test('only signed policies below the supported floor require an update', t => {
  const f = fixture(t);
  assert.equal(f.verify().required, true);
  assert.equal(f.verify({ current: '1.6.5' }).required, false);
  assert.equal(f.verify({ current: '1.6.6' }).required, false);
  const signed = f.signed();
  f.data.updateReason = 'tampered';
  assert.throws(() => f.verify({ signature: signed.signature }), /签名/);
  delete f.data.minimumSupportedVersion; delete f.data.updateReason;
  assert.equal(f.verify().required, false, 'old manifests remain optional');
  for (const floor of ['invalid', '1.8.0', 123]) {
    f.data.minimumSupportedVersion = floor;
    assert.throws(() => f.verify(), /最低支持版本/);
  }
  f.data.minimumSupportedVersion = '1.6.5'; f.data.updateReason = 'x'.repeat(1001);
  assert.throws(() => f.verify(), /升级原因/);
});

test('required policy overrides an earlier skip and survives offline restart and failed refresh', async t => {
  const f = fixture(t);
  fs.writeFileSync(path.join(f.dir, 'update.json'), JSON.stringify({ skipped: '1.7.0' }));
  const updater = f.create(); await updater.check(false);
  assert.equal(updater.state().required, true);
  assert.equal(updater.skipped('1.7.0'), false);
  assert.equal(updater.skip(), false);
  f.network({ offline: true });
  const restored = f.create();
  assert.equal(restored.state().required, true);
  assert.equal(restored.state().version, '1.7.0');
  await restored.check(true);
  assert.equal(restored.state().phase, 'error');
  assert.equal(restored.state().required, true);
  assert.equal(summary(restored.state(), true).update.version, '1.7.0');
  assert.equal(corePolicy(restored.state()).required, true);
});

test('unsigned, missing and older release information cannot clear a verified requirement', async t => {
  const f = fixture(t), updater = f.create(); await updater.check(false);
  for (const options of [{ missing: true }, { missing: false, badSignature: true }, { badSignature: false, tag: 'v1.6.4' }]) {
    f.network(options); await updater.check(true);
    assert.equal(updater.state().required, true);
    assert.equal(updater.state().version, '1.7.0');
  }
  f.network({ tag: 'v1.6.9' }); f.data.version = '1.6.9'; delete f.data.minimumSupportedVersion;
  await updater.check(true);
  assert.equal(updater.state().required, true, 'signed older manifests cannot roll back the known policy');
  f.network({ tag: 'v1.7.0' }); f.data.version = '1.7.0';
  await updater.check(true);
  assert.equal(updater.state().required, false, 'publisher can withdraw a requirement in a valid current release');
  assert.equal(f.create().state().required, false);
});

test('tampered cached evidence and already supported installed versions cannot impose a restriction', async t => {
  const f = fixture(t), updater = f.create(); await updater.check(false);
  f.deps.app.getVersion = () => '1.6.5'; assert.equal(f.create().state().required, false);
  f.deps.app.getVersion = () => '1.7.0'; assert.equal(f.create().state().required, false);
  f.deps.app.getVersion = () => '1.6.4';
  const file = path.join(f.dir, 'update.json'), saved = JSON.parse(fs.readFileSync(file, 'utf8'));
  saved.policy.signature = 'bad'; fs.writeFileSync(file, JSON.stringify(saved));
  assert.equal(f.create().state().required, false);
});

test('a release becoming required is announced again even when its version was already seen', () => {
  assert.notEqual(announcementKey({ version: '1.7.0' }), announcementKey({ version: '1.7.0', required: true, minimumSupportedVersion: '1.6.5' }));
});

test('policy synchronisation serialises updates and retries failed application', async () => {
  let releaseFirst; const first = new Promise(resolve => { releaseFirst = resolve; });
  const calls = [], errors = []; let fail = true;
  const syncer = createPolicySync({ apply: async p => { calls.push(p.required); if (calls.length === 1) await first; else if (fail) throw new Error('core restarting'); }, onError: e => errors.push(e) });
  const pending = syncer.sync({ required: false }); await Promise.resolve();
  syncer.sync({ required: true }); releaseFirst(); await pending;
  assert.deepEqual(calls, [false, true]); assert.ok(errors.at(-1));
  fail = false; await syncer.sync({ required: true });
  assert.deepEqual(calls, [false, true, true]); assert.equal(errors.at(-1), '');
  await syncer.sync({ required: true }); assert.equal(calls.length, 3);
  await syncer.sync({ required: true }, true); assert.equal(calls.length, 4, 'core restarts can force a fresh sync');
});
