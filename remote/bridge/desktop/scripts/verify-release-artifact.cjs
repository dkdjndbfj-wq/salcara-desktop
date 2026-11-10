'use strict';
// Read-only public artifact verification; never reads a signing private key.
const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { execFileSync } = require('node:child_process');
const { verifyManifest } = require('../updater.cjs');
const { validateArchive, assertPrivateTree } = require('../update-archive.cjs');

const PAYLOAD_FILES = ['resources/app.asar', 'resources/SalcaraBridge.exe'];

function expectedSourceSHA(env = process.env) {
  const sha = env.SOURCE_SHA ?? env.GITHUB_SHA;
  assert.match(sha || '', /^[a-f0-9]{40}$/, 'exact expected source SHA required');
  return sha;
}

async function digestFile(file) {
  const hash = crypto.createHash('sha256');
  const info = fs.lstatSync(file);
  assert.ok(info.isFile() && !info.isSymbolicLink() && info.size > 0, 'nonempty regular payload file required');
  for await (const chunk of fs.createReadStream(file)) hash.update(chunk);
  return hash.digest('hex');
}

async function readAsarPackage(file, loadAsar = () => import('@electron/asar')) {
  const info = fs.lstatSync(file);
  assert.ok(info.isFile() && !info.isSymbolicLink() && info.size >= 16 && info.size <= 512 * 1024 * 1024,
    'bounded regular app.asar required');
  const fd = fs.openSync(file, 'r');
  try {
    // Bound the Chromium Pickle header before the ASAR library allocates it.
    const prefix = Buffer.alloc(8);
    assert.equal(fs.readSync(fd, prefix, 0, prefix.length, 0), prefix.length);
    const headerSize = prefix.readUInt32LE(4);
    assert.ok(prefix.readUInt32LE(0) === 4 && headerSize >= 8 && headerSize <= 16 * 1024 * 1024
      && 8 + headerSize <= info.size, 'bounded complete app.asar header required');
  } finally { fs.closeSync(fd); }
  const asar = await loadAsar();
  const entry = asar.statFile(file, 'package.json', false);
  assert.ok(entry && !Object.hasOwn(entry, 'link') && !Object.hasOwn(entry, 'files') && !entry.unpacked && Number.isSafeInteger(entry.size)
    && entry.size > 0 && entry.size <= 256 * 1024, 'packed bounded app.asar package.json required');
  const bytes = asar.extractFile(file, 'package.json', false);
  assert.ok(Buffer.isBuffer(bytes) && bytes.length === entry.size, 'complete app.asar package.json required');
  return JSON.parse(bytes.toString('utf8'));
}

async function verifyPayloadVersions(root, pkg, sourceSHA, { readPackage = readAsarPackage, run = execFileSync, go = process.env.GO_BIN || 'go' } = {}) {
  assert.match(sourceSHA || '', /^[a-f0-9]{40}$/, 'exact expected source SHA required');
  assertPrivateTree(path.join(root, 'resources'));
  for (const name of PAYLOAD_FILES) {
    const info = fs.lstatSync(path.join(root, name));
    assert.ok(info.isFile() && !info.isSymbolicLink() && info.size > 0, `regular payload required: ${name}`);
  }
  const embedded = await readPackage(path.join(root, PAYLOAD_FILES[0]));
  for (const name of ['version', 'name', 'main', 'salcaraUpdateRepo', 'salcaraUpdatePublicKey']) {
    assert.equal(embedded?.[name], pkg[name], `app.asar ${name} must match tested source`);
  }
  const bridge = path.join(root, PAYLOAD_FILES[1]);
  const options = { encoding: 'utf8', timeout: 30000, windowsHide: true, maxBuffer: 1024 * 1024 };
  let build;
  try { build = JSON.parse(run(go, ['version', '-m', '-json', bridge], options)); }
  catch { throw new Error('Unable to inspect Bridge build metadata'); }
  assert.equal(build?.GoVersion, 'go1.27.1', 'Bridge must use the pinned CI Go version');
  assert.equal(build.Path, 'salcara/bridge', 'Bridge main package mismatch');
  assert.equal(build.Main?.Path, 'salcara/bridge', 'Bridge module mismatch');
  assert.ok(Array.isArray(build.Settings), 'Bridge build settings required');
  const settings = new Map();
  for (const setting of build.Settings) {
    assert.ok(typeof setting?.Key === 'string' && typeof setting.Value === 'string'
      && !settings.has(setting.Key), 'unique Bridge build settings required');
    settings.set(setting.Key, setting.Value);
  }
  for (const [key, value] of Object.entries({ GOOS: 'windows', GOARCH: 'amd64', CGO_ENABLED: '0',
    '-trimpath': 'true', vcs: 'git', 'vcs.revision': sourceSHA, 'vcs.modified': 'false' })) {
    assert.equal(settings.get(key), value, `Bridge build setting mismatch: ${key}`);
  }
  // -trimpath deliberately omits -ldflags from Go build info. The pure version
  // command returns before Bridge startup and proves the actual linked value.
  let output;
  try { output = run(bridge, ['version'], { ...options, maxBuffer: 4096 }); }
  catch { throw new Error('Bridge version command failed or timed out'); }
  assert.equal(output.trim(), `Salcara Bridge ${pkg.version}`, 'Bridge version command must report the exact release version');
  return { asarVersion: embedded.version, bridgeVersion: pkg.version, bridgeGoVersion: build.GoVersion,
    sourceSHA, bridgeTarget: `${settings.get('GOOS')}/${settings.get('GOARCH')}`, bridgeVCSModified: false };
}

async function verifyArchivePayload(archive, portableRoot, pkg, sourceSHA, tar, { run = execFileSync, inspect = verifyPayloadVersions } = {}) {
  const temporaryRoot = fs.realpathSync(os.tmpdir());
  const scratch = fs.mkdtempSync(path.join(temporaryRoot, 'salcara-release-payload-'));
  try {
    run(tar, ['-xf', archive, '-C', scratch, ...PAYLOAD_FILES.map(name => `./${name}`)],
      { windowsHide: true, timeout: 30000, maxBuffer: 64 * 1024 });
    const result = await inspect(scratch, pkg, sourceSHA);
    if (portableRoot) {
      assertPrivateTree(path.join(portableRoot, 'resources'));
      for (const name of PAYLOAD_FILES) {
        assert.equal(await digestFile(path.join(portableRoot, name)), await digestFile(path.join(scratch, name)),
          `portable payload differs from signed archive: ${name}`);
      }
    }
    return result;
  } finally {
    const relative = path.relative(temporaryRoot, path.resolve(scratch));
    assert.ok(relative && !relative.startsWith('..') && !path.isAbsolute(relative) && !relative.includes(path.sep)
      && path.basename(scratch).startsWith('salcara-release-payload-'), 'cleanup must stay in owned temporary directory');
    const info = fs.lstatSync(scratch);
    assert.ok(info.isDirectory() && !info.isSymbolicLink(), 'cleanup temporary directory must not be linked');
    fs.rmSync(scratch, { recursive: true, force: true });
  }
}

async function verifyRelease(directory, portableDirectory) {
  const pkg = require('../package.json');
  const sourceSHA = expectedSourceSHA();
  assert.match(pkg.version, /^\d+\.\d+\.\d+$/);
  assert.equal(pkg.salcaraUpdateRepo, 'dkdjndbfj-wq/salcara-desktop');
  const archiveName = `Salcara-Bridge-${pkg.version}-win32-x64.tar.gz`;
  const zipName = `Salcara-Bridge-${pkg.version}-win32-x64.zip`;
  const setupName = `Salcara-Desktop-${pkg.version}-win32-x64-setup.exe`;
  const expected = [archiveName, zipName, setupName, 'latest.json', 'latest.json.sig', 'SHA256SUMS.txt'].sort();
  assert.deepEqual(fs.readdirSync(directory).sort(), expected, 'exact complete artifact required');
  const sums = fs.readFileSync(path.join(directory, 'SHA256SUMS.txt'), 'utf8').replace(/^\uFEFF/, '').trim().split(/\r?\n/);
  const seen = new Set();
  for (const line of sums) {
    const match = /^([a-f0-9]{64})  ([A-Za-z0-9._-]+)$/.exec(line);
    assert.ok(match && expected.includes(match[2]) && match[2] !== 'SHA256SUMS.txt' && !seen.has(match[2]), 'invalid checksum inventory');
    seen.add(match[2]);
    assert.equal(await digestFile(path.join(directory, match[2])), match[1], `hash mismatch: ${match[2]}`);
  }
  assert.deepEqual([...seen].sort(), expected.filter(name => name !== 'SHA256SUMS.txt'));
  const setup = fs.openSync(path.join(directory, setupName), 'r');
  try {
    const header = Buffer.alloc(64);
    assert.equal(fs.readSync(setup, header, 0, header.length, 0), header.length);
    assert.equal(header.subarray(0, 2).toString('ascii'), 'MZ', 'setup must be a Windows executable');
    const pe = Buffer.alloc(4);
    assert.equal(fs.readSync(setup, pe, 0, 4, header.readUInt32LE(60)), 4);
    assert.deepEqual(pe, Buffer.from([0x50, 0x45, 0, 0]), 'setup PE header required');
  } finally { fs.closeSync(setup); }
  const manifest = fs.readFileSync(path.join(directory, 'latest.json'));
  assert.ok(manifest.length <= 256 * 1024);
  const data = JSON.parse(manifest);
  assert.equal(data.repo, pkg.salcaraUpdateRepo);
  assert.deepEqual(Object.keys(data.files), ['win32-x64']);
  const verified = verifyManifest({ manifest, signature: fs.readFileSync(path.join(directory, 'latest.json.sig'), 'utf8'), publicKey: pkg.salcaraUpdatePublicKey, current: '0.0.0', platform: 'win32', arch: 'x64', tag: `v${pkg.version}` });
  assert.equal(verified.file.name, archiveName);
  assert.equal(fs.statSync(path.join(directory, archiveName)).size, verified.file.size);
  assert.equal(await digestFile(path.join(directory, archiveName)), verified.file.sha256);
  await validateArchive(path.join(directory, archiveName));
  const tar = process.platform === 'win32' ? path.join(process.env.SystemRoot || 'C:\\Windows', 'System32', 'tar.exe') : 'tar';
  const marker = JSON.parse(execFileSync(tar, ['-xOf', path.join(directory, archiveName), './.salcara-install.json'], {encoding: 'utf8'}));
  assert.deepEqual(marker, {product: 'salcara-desktop', version: pkg.version});
  if (portableDirectory) {
    const root = path.join(portableDirectory, 'Salcara Bridge-win32-x64');
    assert.deepEqual(JSON.parse(fs.readFileSync(path.join(root, '.salcara-install.json'), 'utf8')), marker);
    for (const name of ['Salcara Bridge.exe', 'resources/app.asar', 'resources/SalcaraBridge.exe', 'resources/SalcaraProbeNode.exe', 'resources/desktop-companion/src/stdio.mjs', 'resources/NODE-LICENSE.txt', 'resources/GO-THIRD-PARTY-NOTICES.txt', 'resources/README.md', 'resources/docs/USER-GUIDE.zh.md']) {
      assert.ok(fs.statSync(path.join(root, name)).isFile(), `incomplete portable package: ${name}`);
    }
  }
  const payload = await verifyArchivePayload(path.join(directory, archiveName), portableDirectory && path.join(portableDirectory, 'Salcara Bridge-win32-x64'), pkg, sourceSHA, tar);
  console.log(JSON.stringify({version:pkg.version, ...payload, product:data.product, repo:data.repo, signatureVerified:true, archiveVerified:true, archiveBytes:verified.file.size, archiveSHA256:verified.file.sha256, setupName, setupSHA256:await digestFile(path.join(directory, setupName)), portableVerified:Boolean(portableDirectory)}));
}
if (require.main === module) verifyRelease(path.resolve(process.argv[2]), process.argv[3] && path.resolve(process.argv[3])).catch(error => {console.error(error.message);process.exitCode=1;});
module.exports = { verifyRelease, expectedSourceSHA, readAsarPackage, verifyPayloadVersions, verifyArchivePayload };
