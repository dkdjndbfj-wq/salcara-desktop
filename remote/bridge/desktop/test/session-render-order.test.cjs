'use strict';
const test = require('node:test'), assert = require('node:assert/strict');
const fs = require('node:fs'), path = require('node:path'), vm = require('node:vm');
const source = fs.readFileSync(path.resolve(__dirname, '../../internal/console/web/app.js'), 'utf8');
function fixture() {
  const context = vm.createContext({ S: { tl: [], tlIndex: new Map(), openInfo: null } });
  vm.runInContext(source.slice(source.indexOf('function tlKey('), source.indexOf('const KIND_ICON =')), context);
  return context;
}
test('buffered partial text and terminal states cannot rewind a newer snapshot', () => {
  for (const [old, incoming] of [
    [{ type: 'message', text: 'Hello', final: true }, { type: 'message', text: 'Hel', final: false }],
    [{ type: 'message', text: 'Hello', ts: 20 }, { type: 'message', text: 'Hel', ts: 10 }],
    [{ type: 'message', text: 'Snapshot', seq: 20, deviceId: 'one' }, { type: 'message', text: 'Old event', seq: 19, deviceId: 'one' }],
    [{ type: 'tool', status: 'completed', output: 'done', outputFinal: true }, { type: 'tool', status: 'running', output: 'do' }],
    [{ type: 'tool', status: 'completed', output: 'done', outputFinal: true }, { type: 'tool', status: 'completed', output: 'do' }],
  ]) {
    const f = fixture(); f.tlApply({ id: 'same', ...old }); f.tlApply({ id: 'same', ...incoming }, { snapshotReplay: true });
    assert.deepEqual(JSON.parse(JSON.stringify(f.S.tl[0])), { id: 'same', ...old });
  }
});
test('a newer buffered event extends a partial snapshot and new IDs are retained', () => {
  const f = fixture(); f.tlApply({ type: 'message', id: 'one', text: 'Hel', ts: 10 });
  f.tlApply({ type: 'message', id: 'one', text: 'Hello', ts: 11, final: true }, { snapshotReplay: true });
  f.tlApply({ type: 'message', id: 'two', text: 'New event' }, { snapshotReplay: true });
  assert.equal(f.S.tl[0].text, 'Hello'); assert.equal(f.S.tl[0].final, true); assert.equal(f.S.tl.length, 2);
});
test('replayed session status keeps a newer snapshot and accepts a later update', () => {
  const f = fixture(); f.S.openInfo = { sessionKey: 'codex:one', status: 'idle', updatedAt: 20 };
  f.tlApply({ type: 'session.updated', session: { sessionKey: 'codex:one', status: 'running', updatedAt: 10 } }, { snapshotReplay: true });
  assert.equal(f.S.openInfo.status, 'idle');
  f.tlApply({ type: 'session.updated', session: { sessionKey: 'codex:one', status: 'running', updatedAt: 21 } }, { snapshotReplay: true });
  assert.equal(f.S.openInfo.status, 'running');
});
