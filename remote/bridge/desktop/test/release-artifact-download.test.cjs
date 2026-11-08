'use strict';
const { test } = require('node:test');
const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { spawn, spawnSync } = require('node:child_process');
const { downloadReleaseArtifact, MAX_ARTIFACT_BYTES, DOWNLOAD_TIMEOUT_MS } = require('../scripts/download-release-artifact.cjs');

// A complete, uncompressed one-file ZIP, generated entirely from synthetic data.
function syntheticZIP(body = Buffer.from('synthetic release fixture only')) {
  const name = Buffer.from('fixture.txt');
  let crc = 0xffffffff;
  for (const byte of body) {
    crc ^= byte;
    for (let bit = 0; bit < 8; bit++) crc = (crc >>> 1) ^ (crc & 1 ? 0xedb88320 : 0);
  }
  crc = (crc ^ 0xffffffff) >>> 0;
  const local = Buffer.alloc(30 + name.length);
  local.writeUInt32LE(0x04034b50, 0);
  local.writeUInt16LE(20, 4);
  local.writeUInt32LE(crc, 14);
  local.writeUInt32LE(body.length, 18);
  local.writeUInt32LE(body.length, 22);
  local.writeUInt16LE(name.length, 26);
  name.copy(local, 30);
  const central = Buffer.alloc(46 + name.length);
  central.writeUInt32LE(0x02014b50, 0);
  central.writeUInt16LE(20, 4);
  central.writeUInt16LE(20, 6);
  central.writeUInt32LE(crc, 16);
  central.writeUInt32LE(body.length, 20);
  central.writeUInt32LE(body.length, 24);
  central.writeUInt16LE(name.length, 28);
  name.copy(central, 46);
  const end = Buffer.alloc(22);
  end.writeUInt32LE(0x06054b50, 0);
  end.writeUInt16LE(1, 8);
  end.writeUInt16LE(1, 10);
  end.writeUInt32LE(central.length, 12);
  end.writeUInt32LE(local.length + body.length, 16);
  return Buffer.concat([local, body, central, end]);
}

function digest(data) { return `sha256:${crypto.createHash('sha256').update(data).digest('hex')}`; }

function fixture(t, data = syntheticZIP()) {
  const temporary = fs.mkdtempSync(path.join(os.tmpdir(), 'salcara-artifact-download-'));
  t.after(() => fs.rmSync(temporary, { recursive: true, force: true }));
  const input = {
    repository: 'dkdjndbfj-wq/salcara-desktop', buildRun: '1234567', sourceSHA: 'a'.repeat(40),
    artifactId: '9876543', digest: digest(data), outputDirectory: path.join(temporary, 'exclusive-download'),
  };
  const metadata = {
    id: Number(input.artifactId), name: 'salcara-desktop-windows-signed', expired: false,
    digest: input.digest, size_in_bytes: data.length,
    workflow_run: { id: Number(input.buildRun), head_sha: input.sourceSHA },
    archive_download_url: 'https://untrusted.invalid/must-not-be-used',
  };
  const calls = [];
  const downloads = [];
  const options = {
    readArtifact: async endpoint => { calls.push(endpoint); return metadata; },
    startDownload: endpoint => { downloads.push(endpoint); return syntheticChild(data); },
    timeoutMs: 5000,
  };
  return { input, metadata, options, calls, downloads, temporary, data };
}

function syntheticChild(data, extra = '', prefix = '') {
  return spawn(process.execPath, ['-e', `${prefix};process.stdout.write(Buffer.from(process.env.FIXTURE_ZIP,'base64'));${extra}`], {
    windowsHide: true, stdio: ['ignore', 'pipe', 'pipe'],
    env: { SystemRoot: process.env.SystemRoot || '', FIXTURE_ZIP: data.toString('base64') },
  });
}

function assertRetainedPartial(f) {
  assert.ok(fs.existsSync(path.join(f.input.outputDirectory, 'artifact.zip.partial')));
  assert.equal(fs.existsSync(path.join(f.input.outputDirectory, 'artifact.zip')), false);
}

test('fixed artifact ID is downloaded as bytes, hash verified and exclusively promoted to ZIP', async t => {
  const f = fixture(t);
  const result = await downloadReleaseArtifact(f.input, f.options);
  assert.deepEqual(f.calls, [`repos/${f.input.repository}/actions/artifacts/${f.input.artifactId}`]);
  assert.deepEqual(f.downloads, [`repos/${f.input.repository}/actions/artifacts/${f.input.artifactId}/zip`]);
  assert.deepEqual(result, { artifactId: f.input.artifactId, digest: f.input.digest, bytes: f.data.length,
    archivePath: path.join(f.input.outputDirectory, 'artifact.zip') });
  assert.deepEqual(fs.readFileSync(result.archivePath), f.data);
  assert.deepEqual(fs.readdirSync(f.input.outputDirectory), ['artifact.zip']);
});

for (const [name, patch] of [
  ['other repository', { repository: 'elsewhere/release' }],
  ['missing run', { buildRun: undefined }],
  ['unsafe run', { buildRun: '9007199254740992' }],
  ['missing artifact', { artifactId: undefined }],
  ['numeric artifact instead of canonical string', { artifactId: 9876543 }],
  ['zero artifact', { artifactId: '0' }],
  ['artifact injection', { artifactId: '../9876543?name=another' }],
  ['noncanonical artifact', { artifactId: '09876543' }],
  ['wrong source', { sourceSHA: '../main' }],
  ['missing digest', { digest: undefined }],
  ['unprefixed digest', { digest: 'a'.repeat(64) }],
  ['truncated digest', { digest: `sha256:${'a'.repeat(63)}` }],
  ['empty output', { outputDirectory: '' }],
]) {
  test(`artifact inputs reject ${name} before any API or child process`, async t => {
    const f = fixture(t);
    await assert.rejects(downloadReleaseArtifact({ ...f.input, ...patch }, f.options));
    assert.deepEqual(f.calls, []);
    assert.deepEqual(f.downloads, []);
    assert.equal(fs.existsSync(f.input.outputDirectory), false);
  });
}

test('filesystem root cannot be used as artifact output', async t => {
  const f = fixture(t);
  await assert.rejects(downloadReleaseArtifact({ ...f.input, outputDirectory: path.parse(f.temporary).root }, f.options));
  assert.deepEqual(f.calls, []);
});

for (const [name, patch] of [
  ['replaced artifact ID', { id: 8888888 }],
  ['wrong artifact name', { name: 'another-artifact' }],
  ['expired', { expired: true }],
  ['missing expiry state', { expired: undefined }],
  ['changed digest', { digest: `sha256:${'b'.repeat(64)}` }],
  ['wrong run', { workflow_run: { id: 1, head_sha: 'a'.repeat(40) } }],
  ['wrong source SHA', { workflow_run: { id: 1234567, head_sha: 'b'.repeat(40) } }],
  ['missing run', { workflow_run: undefined }],
  ['size beyond hard cap', { size_in_bytes: MAX_ARTIFACT_BYTES + 1 }],
  ['missing size', { size_in_bytes: undefined }],
  ['fractional size', { size_in_bytes: 123.5 }],
  ['empty size', { size_in_bytes: 0 }],
]) {
  test(`selected artifact rejects ${name} before downloading or creating output`, async t => {
    const f = fixture(t);
    Object.assign(f.metadata, patch);
    await assert.rejects(downloadReleaseArtifact(f.input, f.options), /Selected artifact metadata does not match/);
    assert.equal(f.calls.length, 1);
    assert.deepEqual(f.downloads, []);
    assert.equal(fs.existsSync(f.input.outputDirectory), false);
  });
}

for (const metadata of [null, '<html>private-error</html>', [], {}]) {
  test(`malformed artifact metadata fails closed: ${JSON.stringify(metadata)}`, async t => {
    const f = fixture(t);
    f.options.readArtifact = async () => metadata;
    await assert.rejects(downloadReleaseArtifact(f.input, f.options), /Selected artifact metadata does not match/);
    assert.deepEqual(f.downloads, []);
  });
}

test('metadata API failure is sanitized and not retried', async t => {
  const f = fixture(t);
  let attempts = 0;
  f.options.readArtifact = async () => { attempts++; throw new Error('private-token-and-signed-url'); };
  await assert.rejects(downloadReleaseArtifact(f.input, f.options), error => error.message === 'Unable to verify the selected artifact metadata');
  assert.equal(attempts, 1);
  assert.deepEqual(f.downloads, []);
  assert.equal(fs.existsSync(f.input.outputDirectory), false);
});

for (const [name, options] of [
  ['unlimited byte count', { maxBytes: Infinity }],
  ['raised hard cap', { maxBytes: MAX_ARTIFACT_BYTES + 1 }],
  ['disabled timeout', { timeoutMs: 0 }],
  ['raised timeout', { timeoutMs: DOWNLOAD_TIMEOUT_MS + 1 }],
]) {
  test(`invalid download limits reject ${name} before contacting API`, async t => {
    const f = fixture(t);
    await assert.rejects(downloadReleaseArtifact(f.input, { ...f.options, ...options }), /Finite artifact download limits/);
    assert.deepEqual(f.calls, []);
  });
}

test('existing output contents are never merged, removed or overwritten', async t => {
  const f = fixture(t);
  fs.mkdirSync(f.input.outputDirectory);
  const existing = path.join(f.input.outputDirectory, 'artifact.zip');
  fs.writeFileSync(existing, 'keep previous evidence');
  await assert.rejects(downloadReleaseArtifact(f.input, f.options), /new exclusive directory/);
  assert.equal(fs.readFileSync(existing, 'utf8'), 'keep previous evidence');
  assert.deepEqual(f.downloads, []);
});

test('existing output directory link is refused without touching its destination', async t => {
  const f = fixture(t);
  const other = path.join(f.temporary, 'private-other-directory');
  fs.mkdirSync(other);
  fs.writeFileSync(path.join(other, 'keep'), 'untouched');
  fs.symlinkSync(other, f.input.outputDirectory, process.platform === 'win32' ? 'junction' : 'dir');
  await assert.rejects(downloadReleaseArtifact(f.input, f.options), /new exclusive directory/);
  assert.deepEqual(fs.readdirSync(other), ['keep']);
  assert.deepEqual(f.downloads, []);
});

test('different ZIP with identical size fails the pinned SHA-256 and retains evidence', async t => {
  const f = fixture(t, syntheticZIP(Buffer.from('fixture-one')));
  const changed = syntheticZIP(Buffer.from('fixture-two'));
  assert.equal(changed.length, f.data.length);
  f.options.startDownload = () => syntheticChild(changed);
  await assert.rejects(downloadReleaseArtifact(f.input, f.options), /exact size, SHA-256 or ZIP verification/);
  assertRetainedPartial(f);
});

test('a truncated stream cannot be promoted even after successful child exit', async t => {
  const f = fixture(t);
  f.options.startDownload = () => syntheticChild(f.data.subarray(0, -1));
  await assert.rejects(downloadReleaseArtifact(f.input, f.options), /exact size, SHA-256 or ZIP verification/);
  assertRetainedPartial(f);
});

test('a non-ZIP stream is rejected even if its length and digest match metadata', async t => {
  const f = fixture(t, Buffer.from('not a ZIP but a synthetic error page long enough'));
  await assert.rejects(downloadReleaseArtifact(f.input, f.options), /exact size, SHA-256 or ZIP verification/);
  assertRetainedPartial(f);
});

test('oversized byte streams are stopped before growing the partial beyond the declared budget', async t => {
  const f = fixture(t);
  f.options.startDownload = () => syntheticChild(Buffer.alloc(64 * 1024));
  await assert.rejects(downloadReleaseArtifact(f.input, f.options), /exceeded its declared or maximum size/);
  assertRetainedPartial(f);
  assert.ok(fs.statSync(path.join(f.input.outputDirectory, 'artifact.zip.partial')).size <= f.metadata.size_in_bytes);
});

test('nonzero child exit refuses otherwise correct bytes and never relays stderr', async t => {
  const f = fixture(t);
  f.options.startDownload = () => syntheticChild(f.data, "process.stderr.write('private-token-and-signed-url');process.exitCode=2");
  await assert.rejects(downloadReleaseArtifact(f.input, f.options), error =>
    error.message === 'Artifact download failed; retain the output for review before retrying');
  assertRetainedPartial(f);
});

test('large diagnostic output is drained without buffering it or blocking a valid byte stream', async t => {
  const f = fixture(t);
  f.options.startDownload = () => syntheticChild(f.data, '', "process.stderr.write('private-diagnostic-'.repeat(100000))");
  const result = await downloadReleaseArtifact(f.input, f.options);
  assert.deepEqual(fs.readFileSync(result.archivePath), f.data);
});

test('download spawn failure is sanitized and retains the exclusive partial file', async t => {
  const f = fixture(t);
  f.options.startDownload = () => { throw new Error('private-local-path-and-token'); };
  await assert.rejects(downloadReleaseArtifact(f.input, f.options), /Artifact download failed; retain the output/);
  assertRetainedPartial(f);
});

test('missing download executable is handled without exposing subprocess diagnostics', async t => {
  const f = fixture(t);
  f.options.startDownload = () => spawn(path.join(f.temporary, 'nonexistent-executable'), [], { stdio: ['ignore', 'pipe', 'pipe'], windowsHide: true });
  await assert.rejects(downloadReleaseArtifact(f.input, f.options), error => !error.message.includes(f.temporary));
  assertRetainedPartial(f);
});

test('a stalled subprocess is killed at the total deadline, with no retry or promotion', async t => {
  const f = fixture(t);
  let attempts = 0;
  let child;
  f.options.timeoutMs = 200;
  f.options.startDownload = () => {
    attempts++;
    child = syntheticChild(f.data, 'setInterval(()=>{},10000)');
    return child;
  };
  await assert.rejects(downloadReleaseArtifact(f.input, f.options), /Artifact download failed/);
  assert.equal(attempts, 1);
  assert.equal(child.killed, true);
  assertRetainedPartial(f);
  await assert.rejects(downloadReleaseArtifact(f.input, f.options), /new exclusive directory/);
  assert.equal(attempts, 1);
});

test('closing stdout without exiting cannot bypass the child deadline', async t => {
  const f = fixture(t);
  f.options.timeoutMs = 200;
  f.options.startDownload = () => syntheticChild(f.data, 'process.stdout.end();setInterval(()=>{},10000)');
  await assert.rejects(downloadReleaseArtifact(f.input, f.options), /Artifact download failed/);
  assertRetainedPartial(f);
});

test('promotion never overwrites a destination created during download', async t => {
  const f = fixture(t);
  f.options.startDownload = () => {
    fs.writeFileSync(path.join(f.input.outputDirectory, 'artifact.zip'), 'existing destination');
    return syntheticChild(f.data);
  };
  await assert.rejects(downloadReleaseArtifact(f.input, f.options), /Artifact download failed/);
  assert.equal(fs.readFileSync(path.join(f.input.outputDirectory, 'artifact.zip'), 'utf8'), 'existing destination');
  assert.ok(fs.existsSync(path.join(f.input.outputDirectory, 'artifact.zip.partial')));
});

test('CLI rejects invalid configuration without contacting GitHub or printing a token', t => {
  const f = fixture(t);
  const script = path.resolve(__dirname, '../scripts/download-release-artifact.cjs');
  const result = spawnSync(process.execPath, [script], {
    encoding: 'utf8', timeout: 5000, windowsHide: true,
    env: { SystemRoot: process.env.SystemRoot || '', GITHUB_REPOSITORY: 'wrong/repository',
      GH_TOKEN: 'private-sentinel-token', ARTIFACT_OUTPUT_DIR: f.input.outputDirectory },
  });
  assert.equal(result.status, 1);
  assert.match(result.stderr, /Exact repository, source, run, artifact ID and SHA-256 digest are required/);
  assert.doesNotMatch(result.stderr + result.stdout, /private-sentinel-token/);
  assert.equal(fs.existsSync(f.input.outputDirectory), false);
});
