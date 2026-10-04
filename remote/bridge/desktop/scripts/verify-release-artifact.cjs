'use strict';
// Read-only public artifact verification; never reads a signing private key.
const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');
const { execFileSync } = require('node:child_process');
const { verifyManifest } = require('../updater.cjs');
const { validateArchive } = require('../update-archive.cjs');

async function verifyRelease(directory, portableDirectory) {
  const pkg = require('../package.json');
  assert.match(pkg.version, /^\d+\.\d+\.\d+$/);
  assert.equal(pkg.salcaraUpdateRepo, 'dkdjndbfj-wq/salcara-desktop');
  const archiveName = `Salcara-Bridge-${pkg.version}-win32-x64.tar.gz`;
  const zipName = `Salcara-Bridge-${pkg.version}-win32-x64.zip`;
  const setupName = `Salcara-Desktop-${pkg.version}-win32-x64-setup.exe`;
  const expected = [archiveName, zipName, setupName, 'latest.json', 'latest.json.sig', 'SHA256SUMS.txt'].sort();
  assert.deepEqual(fs.readdirSync(directory).sort(), expected, 'exact complete artifact required');
  const sums = fs.readFileSync(path.join(directory, 'SHA256SUMS.txt'), 'utf8').replace(/^\uFEFF/, '').trim().split(/\r?\n/);
  const seen = new Set();
  async function digest(file) {
    const hash = crypto.createHash('sha256');
    const info = fs.lstatSync(file);
    assert.ok(info.isFile() && !info.isSymbolicLink() && info.size > 0);
    for await (const chunk of fs.createReadStream(file)) hash.update(chunk);
    return hash.digest('hex');
  }
  for (const line of sums) {
    const match = /^([a-f0-9]{64})  ([A-Za-z0-9._-]+)$/.exec(line);
    assert.ok(match && expected.includes(match[2]) && match[2] !== 'SHA256SUMS.txt' && !seen.has(match[2]), 'invalid checksum inventory');
    seen.add(match[2]);
    assert.equal(await digest(path.join(directory, match[2])), match[1], `hash mismatch: ${match[2]}`);
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
  assert.equal(await digest(path.join(directory, archiveName)), verified.file.sha256);
  await validateArchive(path.join(directory, archiveName));
  const tar = process.platform === 'win32' ? path.join(process.env.SystemRoot || 'C:\\Windows', 'System32', 'tar.exe') : 'tar';
  const marker = JSON.parse(execFileSync(tar, ['-xOf', path.join(directory, archiveName), './.salcara-install.json'], {encoding: 'utf8'}));
  assert.deepEqual(marker, {product: 'salcara-desktop', version: pkg.version});
  if (portableDirectory) {
    const root = path.join(portableDirectory, 'Salcara Bridge-win32-x64');
    assert.deepEqual(JSON.parse(fs.readFileSync(path.join(root, '.salcara-install.json'), 'utf8')), marker);
    for (const name of ['Salcara Bridge.exe', 'resources/app.asar', 'resources/SalcaraBridge.exe', 'resources/SalcaraProbeNode.exe', 'resources/desktop-companion/src/stdio.mjs', 'resources/NODE-LICENSE.txt', 'resources/GO-THIRD-PARTY-NOTICES.txt']) {
      assert.ok(fs.statSync(path.join(root, name)).isFile(), `incomplete portable package: ${name}`);
    }
  }
  console.log(JSON.stringify({version:pkg.version, product:data.product, repo:data.repo, signatureVerified:true, archiveVerified:true, archiveBytes:verified.file.size, archiveSHA256:verified.file.sha256, setupName, setupSHA256:await digest(path.join(directory, setupName)), portableVerified:Boolean(portableDirectory)}));
}
if (require.main === module) verifyRelease(path.resolve(process.argv[2]), process.argv[3] && path.resolve(process.argv[3])).catch(error => {console.error(error.message);process.exitCode=1;});
module.exports = { verifyRelease };
