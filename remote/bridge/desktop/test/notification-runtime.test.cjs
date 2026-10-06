'use strict';
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs'), path = require('node:path'), vm = require('node:vm');
const source = fs.readFileSync(path.join(__dirname, '../../internal/console/web/notify.js'), 'utf8');
function fixture(focused = false) {
  const calls = [], context = vm.createContext({ localStorage: { getItem: () => null }, document: { hidden: false },
    window: { salcaraWindow: { state: async () => ({ focused }), notify: (...args) => calls.push(args) } } });
  vm.runInContext(source, context);
  return { calls, send: ev => { context.ev = { sessionKey: 'codex:main', tool: 'codex', turnId: 'turn-1', ...ev }; vm.runInContext('NOTIFY.onEvent(ev)', context); } };
}
const tick = () => new Promise(resolve => setImmediate(resolve));
function start(f, parent = '') {
  f.send({ type: 'turn', status: 'started' });
  f.send({ type: 'session.updated', session: { status: 'running', parentSessionKey: parent } });
}
test('only completed Chinese final Codex answer notifies, once despite duplicate terminals', async () => {
  const f = fixture(); start(f);
  f.send({ type: 'message', role: 'assistant', final: true, text: '已完成修复，并通过测试。' });
  f.send({ type: 'session.updated', session: { status: 'idle' } }); await tick(); assert.equal(f.calls.length, 0);
  f.send({ type: 'turn', status: 'completed' }); f.send({ type: 'turn', status: 'completed' }); await tick();
  assert.equal(f.calls.length, 1); assert.equal(f.calls[0][0], 'Codex 已完成本轮任务');
});
test('tool, approval, reasoning, partial response, English, child and replay never notify', async () => {
  for (const change of ['partial', 'english', 'child', 'replay', 'failed', 'interrupted', 'focused', 'claude']) {
    const f = fixture(change === 'focused'); if (change !== 'replay') start(f, change === 'child' ? 'codex:parent' : '');
    f.send({ type: 'approval.request', title: '需要审批', approvalId: 'ap' });
    f.send({ type: 'reasoning', final: true, text: '正在思考' });
    f.send({ type: 'message', role: 'assistant', final: change !== 'partial', text: change === 'english' ? 'All done.' : '处理完了', ...(change === 'claude' ? { tool: 'claude' } : {}) });
    f.send({ type: 'turn', status: ['failed', 'interrupted'].includes(change) ? change : 'completed' }); await tick();
    assert.equal(f.calls.length, 0, change);
  }
});
test('notification module does not open another SSE connection', () => { assert.doesNotMatch(source, /new EventSource/); });
