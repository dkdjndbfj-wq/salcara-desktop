'use strict';
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { expectedSourceSHA, readAsarPackage, verifyPayloadVersions, verifyArchivePayload } = require('../scripts/verify-release-artifact.cjs');

const sourceSHA = 'a'.repeat(40);
const pkg = { name: 'salcara-bridge-desktop', version: '1.6.6', main: 'main.cjs',
  salcaraUpdateRepo: 'dkdjndbfj-wq/salcara-desktop', salcaraUpdatePublicKey: 'fixture-public-key' };

function buildInfo() {
  return { GoVersion: 'go1.27.1', Path: 'salcara/bridge', Main: { Path: 'salcara/bridge' },
    Settings: Object.entries({ GOOS: 'windows', GOARCH: 'amd64', CGO_ENABLED: '0', '-trimpath': 'true',
      vcs: 'git', 'vcs.revision': sourceSHA, 'vcs.modified': 'false' }).map(([Key, Value]) => ({ Key, Value })) };
}

function fixture(t) {
  const temporaryRoot = fs.realpathSync(os.tmpdir());
  const root = fs.mkdtempSync(path.join(temporaryRoot, 'salcara-release-payload-test-'));
  fs.mkdirSync(path.join(root, 'resources'));
  fs.writeFileSync(path.join(root, 'resources/app.asar'), 'synthetic-asar');
  fs.writeFileSync(path.join(root, 'resources/SalcaraBridge.exe'), 'synthetic-bridge');
  t.after(() => {
    assert.equal(path.dirname(path.resolve(root)), temporaryRoot);
    assert.ok(path.basename(root).startsWith('salcara-release-payload-test-'));
    assert.ok(!fs.lstatSync(root).isSymbolicLink());
    fs.rmSync(root, { recursive: true, force: true });
  });
  return root;
}

function inspection(options = {}) {
  const calls = [];
  return { calls, readPackage: async () => ({ ...pkg, ...options.package }), go: 'fixture-go',
    run(file, args, settings) {
      calls.push({ file, args, settings });
      assert.equal(settings.windowsHide, true);
      assert.equal(settings.timeout, 30000);
      assert.equal(settings.encoding, 'utf8');
      assert.ok(settings.maxBuffer > 0 && settings.maxBuffer <= 1024 * 1024);
      if (file === 'fixture-go') {
        assert.deepEqual(args.slice(0, 3), ['version', '-m', '-json']);
        if (options.metadataError) throw new Error('private metadata failure');
        const info = buildInfo();
        options.mutate?.(info);
        return options.metadataOutput ?? JSON.stringify(info);
      }
      assert.deepEqual(args, ['version']);
      assert.equal(settings.maxBuffer, 4096);
      if (options.versionError) throw new Error('private timed-out candidate output');
      return options.versionOutput ?? `Salcara Bridge ${pkg.version}\r\n`;
    } };
}

test('expected source uses explicit SHA before workflow SHA and fails closed without a full identity', () => {
  assert.equal(expectedSourceSHA({ SOURCE_SHA: sourceSHA, GITHUB_SHA: 'b'.repeat(40) }), sourceSHA);
  assert.equal(expectedSourceSHA({ GITHUB_SHA: sourceSHA }), sourceSHA);
  for (const env of [{}, { SOURCE_SHA: '' }, { SOURCE_SHA: '', GITHUB_SHA: sourceSHA },
    { SOURCE_SHA: 'a'.repeat(7) }, { SOURCE_SHA: `${sourceSHA}\n` }, { SOURCE_SHA: '../main' }]) {
    assert.throws(() => expectedSourceSHA(env), /exact expected source SHA required/);
  }
});

test('actual app identity, native version and pinned clean Go source must all agree', async t => {
  const root = fixture(t), io = inspection();
  assert.deepEqual(await verifyPayloadVersions(root, pkg, sourceSHA, io), {
    asarVersion: pkg.version, bridgeVersion: pkg.version, bridgeGoVersion: 'go1.27.1',
    sourceSHA, bridgeTarget: 'windows/amd64', bridgeVCSModified: false,
  });
  assert.equal(io.calls.length, 2);
  assert.equal(io.calls[1].file, path.join(root, 'resources/SalcaraBridge.exe'));
});

for (const [field, value] of Object.entries({ version: '1.6.5', name: 'another-app', main: 'another-main.cjs',
  salcaraUpdateRepo: 'another/repository', salcaraUpdatePublicKey: 'another-key' })) {
  test(`stale or foreign ASAR ${field} fails before any binary inspection`, async t => {
    const root = fixture(t), io = inspection({ package: { [field]: value } });
    await assert.rejects(verifyPayloadVersions(root, pkg, sourceSHA, io), new RegExp(`app.asar ${field}`));
    assert.equal(io.calls.length, 0);
  });
}

for (const [key, value] of Object.entries({ GOOS: 'linux', GOARCH: 'arm64', CGO_ENABLED: '1',
  '-trimpath': 'false', vcs: 'hg', 'vcs.revision': 'b'.repeat(40), 'vcs.modified': 'true' })) {
  test(`wrong Go ${key} fails before running the candidate`, async t => {
    const root = fixture(t), io = inspection({ mutate(info) { info.Settings.find(setting => setting.Key === key).Value = value; } });
    await assert.rejects(verifyPayloadVersions(root, pkg, sourceSHA, io), /Bridge build setting mismatch/);
    assert.equal(io.calls.length, 1);
  });
}

for (const [label, mutate] of [
  ['wrong Go toolchain', info => { info.GoVersion = 'go1.27.0'; }],
  ['wrong main package', info => { info.Path = 'another/main'; }],
  ['wrong module', info => { info.Main.Path = 'another/module'; }],
  ['missing settings', info => { delete info.Settings; }],
  ['missing VCS revision', info => { info.Settings = info.Settings.filter(setting => setting.Key !== 'vcs.revision'); }],
  ['duplicate settings', info => { info.Settings.push({ Key: 'vcs.modified', Value: 'false' }); }],
]) {
  test(`unverifiable build metadata rejects ${label}`, async t => {
    const root = fixture(t), io = inspection({ mutate });
    await assert.rejects(verifyPayloadVersions(root, pkg, sourceSHA, io));
    assert.equal(io.calls.length, 1);
  });
}

for (const options of [{ metadataError: true }, { metadataOutput: '<html>not build info</html>' }]) {
  test(`metadata read failure never runs the candidate: ${JSON.stringify(options)}`, async t => {
    const root = fixture(t), io = inspection(options);
    await assert.rejects(verifyPayloadVersions(root, pkg, sourceSHA, io), { message: 'Unable to inspect Bridge build metadata' });
    assert.equal(io.calls.length, 1);
  });
}

for (const [label, options] of [['no output', { versionOutput: '' }], ['old linked version', { versionOutput: 'Salcara Bridge 1.6.5\n' }],
  ['unexpected logs', { versionOutput: `starting Agent\nSalcara Bridge ${pkg.version}\n` }], ['timeout', { versionError: true }]]) {
  test(`native version rejects ${label} explicitly`, async t => {
    const root = fixture(t), io = inspection(options);
    await assert.rejects(verifyPayloadVersions(root, pkg, sourceSHA, io),
      label === 'timeout' ? { message: 'Bridge version command failed or timed out' } : /exact release version/);
    assert.equal(io.calls.length, 2);
  });
}

test('ASAR package reader rejects links, unpacked entries, oversized and incomplete JSON', async t => {
  const root = fixture(t), archive = path.join(root, 'resources/app.asar');
  const prefix = Buffer.alloc(16);
  prefix.writeUInt32LE(4, 0);
  prefix.writeUInt32LE(8, 4);
  fs.writeFileSync(archive, prefix);
  const bytes = Buffer.from(JSON.stringify(pkg));
  for (const entry of [{ size: bytes.length, link: 'another.json' }, { size: bytes.length, unpacked: true },
    { size: bytes.length, files: {} }, { size: bytes.length, link: '' },
    { size: 0 }, { size: 256 * 1024 + 1 }, { size: bytes.length + 1 }]) {
    await assert.rejects(readAsarPackage(archive, async () => ({
      statFile: () => entry, extractFile: () => bytes,
    })), /package.json required/);
  }
  assert.deepEqual(await readAsarPackage(archive, async () => ({
    statFile(file, name, followLinks) { assert.equal(name, 'package.json'); assert.equal(followLinks, false); return { size: bytes.length }; },
    extractFile: () => bytes,
  })), pkg);
});

test('ASAR header allocation is bounded before invoking the parser', async t => {
  const root = fixture(t), archive = path.join(root, 'resources/app.asar');
  let calls = 0;
  const load = async () => { calls++; throw new Error('parser must not run'); };
  for (const headerSize of [0, 7, 17 * 1024 * 1024, 0xffffffff]) {
    const prefix = Buffer.alloc(16);
    prefix.writeUInt32LE(4, 0);
    prefix.writeUInt32LE(headerSize, 4);
    fs.writeFileSync(archive, prefix);
    await assert.rejects(readAsarPackage(archive, load), /bounded complete app.asar header/);
  }
  assert.equal(calls, 0);
});

for (const changed of [null, 'app.asar', 'SalcaraBridge.exe']) {
  test(`signed tar payload and portable files must agree: ${changed || 'matching files'}`, async t => {
    const portable = fixture(t);
    let scratch;
    const options = {
      run(tar, args, settings) {
        assert.equal(tar, 'fixture-tar');
        assert.deepEqual(args.slice(0, 3), ['-xf', 'fixture.tar.gz', '-C']);
        assert.deepEqual(args.slice(4), ['./resources/app.asar', './resources/SalcaraBridge.exe']);
        assert.equal(settings.windowsHide, true);
        assert.equal(settings.timeout, 30000);
        assert.equal(settings.maxBuffer, 64 * 1024);
        scratch = args[3];
        fs.mkdirSync(path.join(scratch, 'resources'));
        for (const name of ['app.asar', 'SalcaraBridge.exe']) {
          fs.writeFileSync(path.join(scratch, 'resources', name), name === changed
            ? 'different signed payload' : fs.readFileSync(path.join(portable, 'resources', name)));
        }
      },
      async inspect(root, expectedPackage, expectedSHA) {
        assert.equal(root, scratch);
        assert.equal(expectedPackage, pkg);
        assert.equal(expectedSHA, sourceSHA);
        return { sourceSHA };
      },
    };
    const result = verifyArchivePayload('fixture.tar.gz', portable, pkg, sourceSHA, 'fixture-tar', options);
    if (changed) await assert.rejects(result, /portable payload differs from signed archive/);
    else assert.deepEqual(await result, { sourceSHA });
    assert.equal(fs.existsSync(scratch), false, 'owned temporary extraction must be removed after success or failure');
  });
}

test('failed tar extraction cleans only the newly owned scratch directory', async () => {
  let scratch;
  await assert.rejects(verifyArchivePayload('fixture.tar.gz', undefined, pkg, sourceSHA, 'fixture-tar', {
    run(tar, args) { scratch = args[3]; throw new Error('synthetic tar failure'); },
  }), /synthetic tar failure/);
  assert.ok(scratch);
  assert.equal(fs.existsSync(scratch), false);
});

test('locked real ASAR reader inspects current and stale embedded package.json', async t => {
  let asar;
  try { asar = await import('@electron/asar'); }
  catch (error) {
    if (process.env.SALCARA_REQUIRE_ASAR_TEST === '1') throw error;
    t.skip('packaging dependencies absent; Windows packaging CI requires this test after npm ci --ignore-scripts');
    return;
  }
  for (const version of [pkg.version, '1.6.5']) {
    const root = fixture(t), source = path.join(root, 'source');
    fs.mkdirSync(source);
    fs.writeFileSync(path.join(source, 'package.json'), JSON.stringify({ ...pkg, version }));
    const archive = path.join(root, 'resources/app.asar');
    await asar.createPackage(source, archive);
    assert.equal((await readAsarPackage(archive)).version, version);
    const io = inspection();
    delete io.readPackage;
    if (version === pkg.version) await verifyPayloadVersions(root, pkg, sourceSHA, io);
    else await assert.rejects(verifyPayloadVersions(root, pkg, sourceSHA, io), /app.asar version/);
  }
});
