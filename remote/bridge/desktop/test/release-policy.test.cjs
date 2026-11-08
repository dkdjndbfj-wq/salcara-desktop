'use strict';

const { test } = require('node:test');
const assert = require('node:assert/strict');
const { parseReleaseArgs, validateReleasePolicy, createReleaseManifest } = require('../scripts/release-policy.cjs');

const base = { version: '1.8.0', repo: 'example/desktop', date: '2026-11-08T00:00:00.000Z' };
const policy = { minimumSupportedVersion: '1.7.0', updateReason: '远程协议已升级，请先完成当前任务，再更新桌面端。' };

test('release CLI reads explicit policy and rejects missing, duplicate or unknown arguments', () => {
  assert.deepEqual(parseReleaseArgs(['--notes', 'notes with spaces.md', '--minimum-supported-version', '1.7.0', '--update-reason', policy.updateReason]), { notesFile: 'notes with spaces.md', ...policy });
  assert.deepEqual(parseReleaseArgs([]), {});
  assert.throws(() => parseReleaseArgs(['--minimum-supported-version']), /缺少值/);
  assert.throws(() => parseReleaseArgs(['--update-reason', '--notes', 'notes.md']), /缺少值/);
  assert.throws(() => parseReleaseArgs(['--notes', 'a', '--notes', 'b']), /不能重复/);
  assert.throws(() => parseReleaseArgs(['--minimum-version', '1.7.0']), /未知发布参数/);
});

test('policy validates numeric version boundaries and reason length', () => {
  assert.deepEqual(validateReleasePolicy({}, '1.8.0'), {});
  assert.deepEqual(validateReleasePolicy(policy, '1.8.0'), policy);
  assert.deepEqual(validateReleasePolicy({ minimumSupportedVersion: '1.8.0' }, '1.8.0'), { minimumSupportedVersion: '1.8.0' });
  assert.deepEqual(validateReleasePolicy({ minimumSupportedVersion: '1.9.0' }, '1.10.0'), { minimumSupportedVersion: '1.9.0' });
  for (const minimumSupportedVersion of ['1.8.1', '1.9.0', 'v1.7.0', '1.7', '1.7.0-beta', '1.7.0\n', '', 17, null]) {
    assert.throws(() => validateReleasePolicy({ minimumSupportedVersion }, '1.8.0'), /最低支持版本/);
  }
  assert.throws(() => validateReleasePolicy({ updateReason: '升级' }, '1.8.0'), /必须指定最低支持版本/);
  for (const updateReason of ['', '  ', 'x'.repeat(1001), false, null]) {
    assert.throws(() => validateReleasePolicy({ minimumSupportedVersion: '1.7.0', updateReason }, '1.8.0'), /更新原因/);
  }
  assert.equal(validateReleasePolicy({ minimumSupportedVersion: '1.7.0', updateReason: 'x'.repeat(1000) }, '1.8.0').updateReason.length, 1000);
});

test('new manifest defaults to optional and includes explicit signed compatibility fields', () => {
  const optional = createReleaseManifest(base);
  assert.equal(Object.hasOwn(optional, 'minimumSupportedVersion'), false);
  assert.equal(Object.hasOwn(optional, 'updateReason'), false);
  const required = createReleaseManifest({ ...base, options: policy });
  assert.equal(required.minimumSupportedVersion, policy.minimumSupportedVersion);
  assert.equal(required.updateReason, policy.updateReason);
  assert.equal(required.schema, 1);
});

test('adding a platform preserves the existing policy when options are omitted or identical', () => {
  const previous = createReleaseManifest({ ...base, options: policy, notes: '- 功能更新' });
  previous.files['win32-x64'] = { name: 'windows.tar.gz' };
  for (const options of [{}, policy, { minimumSupportedVersion: policy.minimumSupportedVersion }]) {
    const merged = createReleaseManifest({ ...base, options, previous });
    assert.equal(merged.minimumSupportedVersion, policy.minimumSupportedVersion);
    assert.equal(merged.updateReason, policy.updateReason);
    assert.equal(merged.notes, previous.notes);
    assert.deepEqual(merged.files, previous.files);
    assert.notEqual(merged.files, previous.files);
  }
});

test('same-release platform merges cannot change an existing or absent policy', () => {
  const previous = createReleaseManifest({ ...base, options: policy });
  for (const options of [{ minimumSupportedVersion: '1.8.0' }, { updateReason: 'another reason' }, { minimumSupportedVersion: '' }]) {
    assert.throws(() => createReleaseManifest({ ...base, options, previous }), /更新策略不一致/);
  }
  assert.throws(() => createReleaseManifest({ ...base, options: policy, previous: createReleaseManifest(base) }), /更新策略不一致/);
  const noReason = createReleaseManifest({ ...base, options: { minimumSupportedVersion: '1.7.0' } });
  assert.throws(() => createReleaseManifest({ ...base, options: policy, previous: noReason }), /更新策略不一致/);
});

test('invalid or foreign existing manifests fail instead of silently discarding policy', () => {
  const previous = createReleaseManifest({ ...base, options: policy });
  for (const changed of [{ repo: 'foreign/project' }, { schema: 2 }, { product: 'foreign' }, { files: [] }, { version: 'bad' }]) {
    assert.throws(() => createReleaseManifest({ ...base, previous: { ...previous, ...changed } }), /已有更新清单/);
  }
  assert.throws(() => createReleaseManifest({ ...base, previous: { ...previous, minimumSupportedVersion: '99.0.0' } }), /最低支持版本/);
});

test('a newer release carries compatibility and control settings without carrying old payloads', () => {
  const controlURL = 'https://github.com/example/desktop/releases/download/update-control/control.json';
  const previous = createReleaseManifest({ ...base, options: { ...policy, controlURL } });
  previous.files['win32-x64'] = { name: 'old-payload.tar.gz' };
  const next = createReleaseManifest({ ...base, version: '1.8.1', previous });
  assert.equal(next.minimumSupportedVersion, policy.minimumSupportedVersion);
  assert.equal(next.updateReason, policy.updateReason);
  assert.equal(next.controlURL, controlURL);
  assert.deepEqual(next.files, {});
});
