'use strict';
// Download one immutable GitHub artifact ID. Do not extract it or mutate a
// release here. Failed/uncertain downloads are retained and never retried.
const crypto = require('node:crypto');
const fsp = require('node:fs/promises');
const path = require('node:path');
const { execFileSync, spawn } = require('node:child_process');
const { Transform } = require('node:stream');
const { pipeline } = require('node:stream/promises');

const REPOSITORY = 'dkdjndbfj-wq/salcara-desktop';
const ARTIFACT_NAME = 'salcara-desktop-windows-signed';
const MAX_ARTIFACT_BYTES = 2 * 1024 * 1024 * 1024;
const DOWNLOAD_TIMEOUT_MS = 5 * 60 * 1000;
const FAILED = 'Artifact download failed; retain the output for review before retrying';

class ArtifactGateError extends Error {}
function gate(message) { return new ArtifactGateError(message); }
function safeID(value) {
  return typeof value === 'string' && /^[1-9][0-9]{0,15}$/.test(value) && Number.isSafeInteger(Number(value));
}

function validateInput(input) {
  if (input.repository !== REPOSITORY || !safeID(input.buildRun) || !safeID(input.artifactId) ||
      typeof input.sourceSHA !== 'string' || !/^[a-f0-9]{40}$/.test(input.sourceSHA) ||
      typeof input.digest !== 'string' || !/^sha256:[a-f0-9]{64}$/.test(input.digest)) {
    throw gate('Exact repository, source, run, artifact ID and SHA-256 digest are required');
  }
  if (typeof input.outputDirectory !== 'string' || !input.outputDirectory.trim() || input.outputDirectory.includes('\0')) {
    throw gate('A new exclusive artifact output directory is required');
  }
  const directory = path.resolve(input.outputDirectory);
  if (directory === path.parse(directory).root) throw gate('A new exclusive artifact output directory is required');
  return directory;
}

function ghEnvironment() {
  return { ...process.env, GH_HOST: 'github.com', GH_DEBUG: '', GH_PROMPT_DISABLED: '1' };
}

function readArtifact(endpoint) {
  try {
    return JSON.parse(execFileSync('gh', ['api', '--hostname', 'github.com', '--method', 'GET', endpoint], {
      encoding: 'utf8', timeout: 30000, maxBuffer: 1024 * 1024,
      windowsHide: true, env: ghEnvironment(), stdio: ['ignore', 'pipe', 'pipe'],
    }));
  } catch {
    throw gate('Unable to verify the selected artifact metadata');
  }
}

function startDownload(endpoint) {
  return spawn('gh', ['api', '--hostname', 'github.com', '--method', 'GET', endpoint], {
    windowsHide: true, env: ghEnvironment(), stdio: ['ignore', 'pipe', 'pipe'],
  });
}

function validateArtifact(artifact, input, maxBytes) {
  if (!artifact || !Number.isSafeInteger(artifact.id) || String(artifact.id) !== input.artifactId ||
      artifact.name !== ARTIFACT_NAME || artifact.expired !== false || artifact.digest !== input.digest ||
      !Number.isSafeInteger(artifact.size_in_bytes) || artifact.size_in_bytes < 22 || artifact.size_in_bytes > maxBytes ||
      !Number.isSafeInteger(artifact.workflow_run?.id) || String(artifact.workflow_run.id) !== input.buildRun ||
      artifact.workflow_run.head_sha !== input.sourceSHA) {
    throw gate('Selected artifact metadata does not match the exact tested run, source, digest or size');
  }
}

async function downloadReleaseArtifact(input, options = {}) {
  const directory = validateInput(input);
  const maxBytes = options.maxBytes ?? MAX_ARTIFACT_BYTES;
  const timeoutMs = options.timeoutMs ?? DOWNLOAD_TIMEOUT_MS;
  if (!Number.isSafeInteger(maxBytes) || maxBytes < 22 || maxBytes > MAX_ARTIFACT_BYTES ||
      !Number.isSafeInteger(timeoutMs) || timeoutMs <= 0 || timeoutMs > DOWNLOAD_TIMEOUT_MS) {
    throw gate('Finite artifact download limits are required');
  }
  const endpoint = `repos/${input.repository}/actions/artifacts/${input.artifactId}`;
  let artifact;
  try {
    artifact = await (options.readArtifact || readArtifact)(endpoint);
  } catch {
    throw gate('Unable to verify the selected artifact metadata');
  }
  validateArtifact(artifact, input, maxBytes);
  try {
    // Never merge with a prior run or write through an existing directory/link.
    await fsp.mkdir(directory, { mode: 0o700 });
  } catch {
    throw gate('Artifact output must be a new exclusive directory; review previous output before retrying');
  }

  const partial = path.join(directory, 'artifact.zip.partial');
  const archive = path.join(directory, 'artifact.zip');
  let file, output, child, childFinished, timer;
  const controller = new AbortController();
  let bytes = 0;
  const signature = Buffer.alloc(4);
  let signatureLength = 0;
  const hash = crypto.createHash('sha256');
  try {
    file = await fsp.open(partial, 'wx', 0o600);
    output = file.createWriteStream();
    child = (options.startDownload || startDownload)(`${endpoint}/zip`);
    if (!child?.stdout || !child.stderr || typeof child.kill !== 'function') throw gate(FAILED);
    childFinished = new Promise(resolve => {
      child.once('error', () => resolve(false));
      child.once('close', (code, signal) => resolve(code === 0 && signal === null));
    });
    // Neither gh diagnostics nor signed download URLs are safe log output.
    child.stderr.on('error', () => {});
    child.stderr.resume();
    const meter = new Transform({
      transform(chunk, _encoding, callback) {
        if (chunk.length > maxBytes - bytes || chunk.length > artifact.size_in_bytes - bytes) {
          callback(gate('Artifact download exceeded its declared or maximum size'));
          return;
        }
        bytes += chunk.length;
        if (signatureLength < signature.length) {
          const copied = chunk.copy(signature, signatureLength, 0, Math.min(chunk.length, signature.length - signatureLength));
          signatureLength += copied;
        }
        hash.update(chunk);
        callback(null, chunk);
      },
    });
    timer = setTimeout(() => {
      controller.abort();
      child.kill('SIGKILL');
    }, timeoutMs);
    const [, cleanExit] = await Promise.all([
      pipeline(child.stdout, meter, output, { signal: controller.signal }),
      childFinished,
    ]);
    if (!cleanExit) throw gate(FAILED);
    if (bytes !== artifact.size_in_bytes || `sha256:${hash.digest('hex')}` !== input.digest ||
        signatureLength !== 4 || !signature.equals(Buffer.from([0x50, 0x4b, 0x03, 0x04]))) {
      throw gate('Artifact archive failed exact size, SHA-256 or ZIP verification');
    }
    // link is no-clobber on both NTFS and POSIX. Even an unexpected preexisting
    // destination is never replaced; partial output survives a failed promote.
    await fsp.link(partial, archive);
    await fsp.unlink(partial);
    return { artifactId: input.artifactId, digest: input.digest, bytes, archivePath: archive };
  } catch (error) {
    controller.abort();
    if (typeof child?.kill === 'function') child.kill('SIGKILL');
    if (output) output.destroy();
    if (childFinished) await childFinished;
    if (file) await file.close().catch(() => {});
    throw error instanceof ArtifactGateError ? error : gate(FAILED);
  } finally {
    clearTimeout(timer);
  }
}

if (require.main === module) {
  const operation = process.argv.length === 2 ? downloadReleaseArtifact({
    repository: process.env.GITHUB_REPOSITORY,
    sourceSHA: process.env.SOURCE_SHA,
    buildRun: process.env.BUILD_RUN,
    artifactId: process.env.ARTIFACT_ID,
    digest: process.env.ARTIFACT_DIGEST,
    outputDirectory: process.env.ARTIFACT_OUTPUT_DIR || 'verified-artifact-download',
  }) : Promise.reject(gate('Unknown artifact download command'));
  operation.then(result => console.log(JSON.stringify({ artifactId: result.artifactId, digest: result.digest, bytes: result.bytes })))
    .catch(error => { console.error(error.message); process.exitCode = 1; });
}

module.exports = { downloadReleaseArtifact, MAX_ARTIFACT_BYTES, DOWNLOAD_TIMEOUT_MS };
