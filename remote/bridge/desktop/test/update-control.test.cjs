'use strict';
const { test } = require('node:test');
const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const { verifyControl, decision } = require('../update-control.cjs');
const { signControl } = require('../scripts/release-control.cjs');
const { signingOptions } = require('../scripts/authenticode.cjs');

test('release control requires the publisher signature, increasing revision and bounded validity', () => {
  const keys = crypto.generateKeyPairSync('ed25519'), now = Date.now();
  const publicKey = keys.publicKey.export({ format: 'der', type: 'spki' }).subarray(-32).toString('base64');
  const input = { revision: 8, issuedAt: now, expiresAt: now + 3600000, paused: false, reason: '', blockedVersions: [] };
  const signed = signControl(input, keys.privateKey, publicKey, 'o/r');
  const args = { ...signed, publicKey, repo: 'o/r', previousRevision: 7, now };
  assert.equal(verifyControl(args).revision, 8);
  assert.throws(() => verifyControl({ ...args, previousRevision: 9 }), /修订号/);
  assert.throws(() => verifyControl({ ...args, now: now + 3600001 }), /时效/);
  assert.throws(() => verifyControl({ ...args, repo: 'other/repo' }), /格式/);
  assert.throws(() => verifyControl({ ...args, body: Buffer.from(signed.body.toString().replace('false', 'true')) }), /签名/);
  assert.throws(() => signControl({ ...input, expiresAt: now + 8 * 86400000 }, keys.privateKey, publicKey, 'o/r'), /时效/);
});
test('withdrawal and pause override even required updates; rollout is stable and never defers a required update', () => {
  const control = { paused: false, reason: '', blockedVersions: [], rollout: { version: '1.7.0', salt: 'campaign', percent: 0 } };
  assert.match(decision(control, '1.7.0', 'same-device'), /分批/);
  assert.equal(decision(control, '1.7.0', 'same-device', true), '');
  control.rollout.percent = 50;
  assert.equal(decision(control, '1.7.0', 'same-device'), decision(control, '1.7.0', 'same-device'));
  control.blockedVersions = ['1.7.0']; assert.match(decision(control, '1.7.0', 'same-device', true), /暂停/);
  control.blockedVersions = []; control.paused = true; assert.match(decision(control, '1.7.1', 'same-device', true), /暂停/);
});
test('production Windows signing cannot silently fall back to unsigned artifacts', () => {
  assert.throws(() => signingOptions({}), /certificate/);
  assert.throws(() => signingOptions({ SALCARA_AUTHENTICODE_THUMBPRINT: 'a'.repeat(40), SALCARA_AUTHENTICODE_TIMESTAMP: 'http://unsafe.test' }), /HTTPS/);
});
