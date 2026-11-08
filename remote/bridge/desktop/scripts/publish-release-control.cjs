'use strict';
const fs = require('node:fs'), path = require('node:path'), os = require('node:os'), crypto = require('node:crypto');
const { execFileSync } = require('node:child_process');
const { signControl } = require('./release-control.cjs');
const { verifyControl } = require('../update-control.cjs');
const pkg = require('../package.json');
async function main() {
  const revision = Number(process.env.CONTROL_REVISION), percent = Number(process.env.CONTROL_PERCENT);
  const version = process.env.CONTROL_VERSION || '', repo = pkg.salcaraUpdateRepo;
  if (!/^[1-9]\d*$/.test(process.env.CONTROL_REVISION || '') || !Number.isSafeInteger(revision)) throw new Error('A positive integer control revision is required');
  if (!process.env.DESKTOP_UPDATE_KEY) throw new Error('Desktop signing secret is required');
  const run = args => execFileSync('gh', args, { encoding: 'utf8', windowsHide: true, maxBuffer: 1024 * 1024 });
  // List releases instead of treating arbitrary GET failures as a missing tag.
  const releases = JSON.parse(run(['api', `repos/${repo}/releases?per_page=100`]));
  const existing = releases.find(item => item.tag_name === 'update-control');
  let previousRevision = 0;
  if (existing) {
    if (!existing.prerelease || existing.draft) throw new Error('Control must be a public prerelease, separate from application releases');
    const assets = existing.assets || [];
    const bodyAsset = assets.find(a => a.name === 'control.json'), sigAsset = assets.find(a => a.name === 'control.json.sig');
    if (!bodyAsset || !sigAsset) throw new Error('Incomplete existing control assets; inspect the control release before retrying');
    const body = run(['api', `repos/${repo}/releases/assets/${bodyAsset.id}`, '-H', 'Accept: application/octet-stream']);
    const signature = run(['api', `repos/${repo}/releases/assets/${sigAsset.id}`, '-H', 'Accept: application/octet-stream']);
    // Expired controls still establish the revision floor for the publisher.
    const expiresAt = JSON.parse(body).expiresAt;
    previousRevision = verifyControl({ body, signature, publicKey: pkg.salcaraUpdatePublicKey, repo, now: Math.min(Date.now(), expiresAt - 1) }).revision;
    if (revision <= previousRevision) throw new Error('Control revision must increase');
  }
  const now = Date.now();
  const input = { revision, issuedAt: now, expiresAt: now + 7 * 86400000, paused: process.env.CONTROL_PAUSED === 'true',
    blockedVersions: (process.env.CONTROL_BLOCKED || '').split(',').map(v => v.trim()).filter(Boolean), reason: process.env.CONTROL_REASON || '',
    ...(version ? { rollout: { version, percent, salt: `salcara-${version}` } } : {}) };
  const result = signControl(input, crypto.createPrivateKey(process.env.DESKTOP_UPDATE_KEY), pkg.salcaraUpdatePublicKey, repo, previousRevision);
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'salcara-control-publish-'));
  try {
    const bodyFile = path.join(directory, 'control.json'), sigFile = `${bodyFile}.sig`, notes = path.join(directory, 'notes.md');
    fs.writeFileSync(bodyFile, result.body); fs.writeFileSync(sigFile, result.signature + '\n');
    fs.writeFileSync(notes, 'Signed desktop distribution control. This is not an application version.\n');
    if (!existing) run(['release', 'create', 'update-control', bodyFile, sigFile, '--repo', repo, '--title', 'Desktop update distribution control', '--prerelease', '--latest=false', '--notes-file', notes]);
    else run(['release', 'upload', 'update-control', bodyFile, sigFile, '--repo', repo, '--clobber']);
    console.log(`Published signed control revision ${revision}; valid until ${new Date(input.expiresAt).toISOString()}.`);
  } finally { fs.rmSync(directory, { recursive: true, force: true }); }
}
main().catch(error => { console.error(error.message); process.exitCode = 1; });
