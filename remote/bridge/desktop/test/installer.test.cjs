'use strict';
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { inspectPayload, buildInstaller, REQUIRED_FILES } = require('../scripts/installer.cjs');

function fixture() {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'salcara-installer-test-'));
  const payload = path.join(root, 'payload with spaces');
  fs.mkdirSync(payload);
  for (const file of REQUIRED_FILES) {
    const target = path.join(payload, file);
    fs.mkdirSync(path.dirname(target), { recursive: true });
    fs.writeFileSync(target, `synthetic ${file}`);
  }
  fs.writeFileSync(path.join(payload, '.salcara-install.json'), JSON.stringify({ product: 'salcara-desktop', version: '1.6.1' }) + '\n');
  return { root, payload, release: path.join(root, 'release with spaces') };
}

test('installer validates a complete production payload and preserves argument boundaries', () => {
  const f = fixture();
  try {
    let calls = 0;
    const result = buildInstaller({ version: '1.6.1', payloadDirectory: f.payload, releaseDirectory: f.release,
      compiler: 'synthetic ISCC.exe', run(compiler, args, options) {
        calls++;
        assert.equal(compiler, 'synthetic ISCC.exe');
        assert.ok(args.includes(`/DPayloadDir=${f.payload}`));
        assert.ok(args.includes(`/DReleaseDir=${f.release}`));
        assert.ok(args.includes('/DAppVersion=1.6.1'));
        assert.equal(options.windowsHide, true);
        assert.match(args.at(-1), /installer[/\\]windows\.iss$/);
        fs.writeFileSync(path.join(f.release, 'Salcara-Desktop-1.6.1-win32-x64-setup.exe'), 'synthetic-compiler-output');
      } });
    assert.equal(calls, 1);
    assert.equal(path.basename(result), 'Salcara-Desktop-1.6.1-win32-x64-setup.exe');
  } finally { fs.rmSync(f.root, { recursive: true, force: true }); }
});

test('incomplete, foreign, wrong-version and test payloads never reach the compiler', () => {
  const f = fixture();
  let calls = 0;
  const options = { version: '1.6.1', payloadDirectory: f.payload, releaseDirectory: f.release, run() { calls++; } };
  try {
    assert.throws(() => buildInstaller({ ...options, version: '1.6.1-test.1' }), /production/);
    fs.writeFileSync(path.join(f.payload, '.salcara-install.json'), JSON.stringify({ product: 'other', version: '1.6.1' }));
    assert.throws(() => buildInstaller(options), /marker/);
    fs.writeFileSync(path.join(f.payload, '.salcara-install.json'), JSON.stringify({ product: 'salcara-desktop', version: '1.6.0' }));
    assert.throws(() => buildInstaller(options), /marker/);
    fs.writeFileSync(path.join(f.payload, '.salcara-install.json'), JSON.stringify({ product: 'salcara-desktop', version: '1.6.1' }));
    fs.unlinkSync(path.join(f.payload, REQUIRED_FILES[0]));
    assert.throws(() => buildInstaller(options));
    assert.equal(calls, 0);
  } finally { fs.rmSync(f.root, { recursive: true, force: true }); }
});

test('linked payload descendants and ancestors are rejected before compiler execution', () => {
  const f = fixture(), outside = path.join(f.root, 'outside');
  fs.mkdirSync(outside);
  const link = path.join(f.payload, 'linked');
  try {
    fs.symlinkSync(outside, link, process.platform === 'win32' ? 'junction' : 'dir');
    assert.throws(() => inspectPayload(f.payload, '1.6.1'), /Linked/);
    fs.unlinkSync(link);
    const alias = path.join(f.root, 'alias');
    fs.symlinkSync(f.payload, alias, process.platform === 'win32' ? 'junction' : 'dir');
    assert.throws(() => inspectPayload(alias, '1.6.1'), /ancestors/);
    fs.unlinkSync(alias);
  } finally { fs.rmSync(f.root, { recursive: true, force: true }); }
});

test('installer compilation failure cannot be mistaken for a completed installer', () => {
  const f = fixture();
  try {
    assert.throws(() => buildInstaller({ version: '1.6.1', payloadDirectory: f.payload, releaseDirectory: f.release, run() {} }));
  } finally { fs.rmSync(f.root, { recursive: true, force: true }); }
});
