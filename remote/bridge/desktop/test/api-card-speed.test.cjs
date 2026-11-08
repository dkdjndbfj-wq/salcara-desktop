'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs'), path = require('node:path'), vm = require('node:vm');
const source = fs.readFileSync(path.join(__dirname, '../../internal/console/web/api-card-speed.js'), 'utf8');
const rev = char => 'src-v1-' + char.repeat(64);
function fixture() {
  const notices = [], requests = [], listeners = {}, account = { id: 'example', name: 'API <example>', wire: 'responses', sourceRevision: rev('a'), speedMode: 'standard' };
  let renders = 0;
  const state = { route: 'accounts', routeRevision: 1, local: { accounts: [account], bindings: { codex: { accountId: account.id, pending: false } } } };
  const context = vm.createContext({ S: state, Object, Map,
    esc: value => String(value ?? '').replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('"', '&quot;'),
    document: { addEventListener: (type, fn) => { listeners[type] = fn; } },
    api: (url, body) => new Promise((resolve, reject) => requests.push({ url, body, resolve, reject })),
    toast: (...args) => notices.push(args), renderLocalCards: () => { renders++; },
  });
  vm.runInContext(source, context);
  const control = { dataset: { accountSpeed: account.id, sourceRevision: rev('a') }, value: 'fast', disabled: false, isConnected: true, setAttribute() {}, removeAttribute() {} };
  return { context, state, account, notices, requests, control, renders: () => renders, save: () => context.saveAccountSpeed(control) };
}
test('card speed saves one scoped revision and only adopts an acknowledged result', async () => {
  const f = fixture(), pending = f.save();
  assert.equal(f.control.disabled, true);
  assert.deepEqual(JSON.parse(JSON.stringify(f.requests[0].body)), { id: 'example', sourceRevision: rev('a'), speedMode: 'fast' });
  assert.equal(f.state.local.accounts[0].speedMode, 'standard');
  await f.save(); assert.equal(f.requests.length, 1);
  f.requests[0].resolve({ ok: true, id: 'example', speedMode: 'fast', sourceRevision: rev('b') }); await pending;
  assert.equal(f.state.local.accounts[0].speedMode, 'fast');
  assert.equal(f.state.local.accounts[0].sourceRevision, rev('b'));
  assert.equal(f.state.local.bindings.codex.pending, true);
  assert.equal(f.control.disabled, false); assert.equal(f.renders(), 1);
  assert.match(f.notices[0][0], /重新应用/);
});
test('failed or malformed replies preserve the saved value and never retry a write', async () => {
  for (const value of [null, {}, { ok: true, id: 'other', speedMode: 'fast', sourceRevision: rev('b') }, new Error('版本冲突')]) {
    const f = fixture(), pending = f.save();
    if (value instanceof Error) f.requests[0].reject(value); else f.requests[0].resolve(value);
    await pending;
    assert.equal(f.state.local.accounts[0].speedMode, 'standard'); assert.equal(f.control.value, 'standard');
    assert.equal(f.control.disabled, false); assert.equal(f.requests.length, 1); assert.equal(f.notices.at(-1)[1], 'bad');
  }
});
test('navigation and changed account revision fence late successes and failures', async () => {
  for (const stale of [f => { f.state.route = 'settings'; }, f => { f.state.routeRevision++; }, f => { f.account.sourceRevision = rev('c'); }]) {
    for (const fail of [false, true]) {
      const f = fixture(), pending = f.save(); stale(f);
      if (fail) f.requests[0].reject(new Error('late')); else f.requests[0].resolve({ ok: true, id: 'example', speedMode: 'fast', sourceRevision: rev('b') });
      await pending; assert.equal(f.state.local.accounts[0].speedMode, 'standard'); assert.equal(f.notices.length, 0);
    }
  }
});
test('rerender during saving can still adopt the same source acknowledgement and unlock the new card', async () => {
  const f = fixture(), pending = f.save(); f.control.isConnected = false;
  f.requests[0].resolve({ ok: true, id: 'example', speedMode: 'fast', sourceRevision: rev('b') }); await pending;
  assert.equal(f.state.local.accounts[0].speedMode, 'fast'); assert.equal(f.renders(), 1);
  assert.doesNotMatch(f.context.accountSpeedControlHTML(f.state.local.accounts[0]), / disabled/);
});
test('unsupported protocol, stale dropdown and unknown mode cannot submit changes', async () => {
  for (const change of [f => { f.account.wire = 'anthropic'; }, f => { f.control.dataset.sourceRevision = rev('b'); }, f => { f.control.value = 'invented'; }]) {
    const f = fixture(); change(f); await f.save(); assert.equal(f.requests.length, 0);
  }
});
test('three named levels escape account metadata and disclose upstream cost support', () => {
  const f = fixture(); let html = f.context.accountSpeedControlHTML(f.account);
  assert.match(html, /标准/); assert.match(html, /快速/); assert.match(html, /超高速/); assert.match(html, /可能增加费用/);
  assert.match(html, /API &lt;example>/); assert.doesNotMatch(html, /API <example>/);
  html = f.context.accountSpeedControlHTML({ ...f.account, wire: 'anthropic' }); assert.match(html, / disabled/);
});
