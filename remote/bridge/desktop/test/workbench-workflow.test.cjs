'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const { setup, deferred } = require('./helpers/workbench.cjs');
const close = f => f.ACTIONS.closeModal(f.button('closeModal'), { target: { closest: () => null } });

test('Agent and API-card entry points produce the same uncommitted confirmation', async () => {
  const fromCard = setup(), fromAPI = setup();
  await fromCard.change('codex', 'api-shared');
  await fromAPI.ACTIONS.accountOpen({ dataset: { target: 'codex', id: 'api-shared' } });
  assert.equal(fromCard.modal.innerHTML, fromAPI.modal.innerHTML);
  for (const f of [fromCard, fromAPI]) {
    assert.equal(f.document.getElementById('wbReady'), null);
    assert.equal(f.calls.filter(call => call.body).length, 0);
  }
});

test('API edit after verification invalidates the confirmation before binding or launching', async () => {
  const f = setup();
  await f.change('codex', 'api-shared');
  f.backend.accounts[0].sourceRevision = 'src-v1-' + 'c'.repeat(64);
  await f.ACTIONS.wbOpenGo(f.button('wbOpenGo'));
  assert.equal(f.calls.filter(call => call.url === '/api/local/bind').length, 0);
  assert.match(f.modal.innerHTML, /账号或工具已变化/);
  f.assertNoLaunch();
  await f.ACTIONS.wbRecheck();
  assert.equal(f.WB.accountOpen.sourceRevision, 'src-v1-' + 'c'.repeat(64));
  assert.equal(f.document.getElementById('wbReady'), null);
});

test('duplicate clicks submit exactly one safe change', async () => {
  const gate = deferred(), f = setup({ onChange: () => gate.promise });
  await f.change('codex', 'api-shared');
  const button = f.button('wbOpenGo');
  const first = f.ACTIONS.wbOpenGo(button);
  await Promise.resolve(); await Promise.resolve();
  const second = f.ACTIONS.wbOpenGo(button);
  gate.resolve(); await Promise.all([first, second]);
  assert.equal(f.calls.filter(c => c.url === '/api/local/bind').length, 0);
  assert.equal(f.calls.filter(c => c.url === '/api/local/agent-change/v1').length, 1);
});

test('closing during stop aborts before any binding changes', async () => {
  const gate = deferred(), entered = deferred(), f = setup({ onChange: () => { entered.resolve(); return gate.promise; } });
  await f.change('codex', 'api-shared');
  const request = f.ACTIONS.wbOpenGo(f.button('wbOpenGo'));
  await entered.promise; close(f); gate.resolve(); await request;
  assert.equal(f.backend.bindings.codex.accountId, '');
  assert.equal(f.backend.bindings.codex.pending, false);
  assert.equal(f.modal.innerHTML, '');
  assert.equal(f.calls.filter(c => c.url === '/api/local/agent-change/v1').length, 1);
});

test('leaving and re-entering the same route cannot revive a stale verification', async () => {
  const gate = deferred(), f = setup({ onAccounts: () => gate.promise });
  f.S.routeRevision = 1;
  const open = f.change('codex', 'api-shared');
  f.S.routeRevision = 3;
  f.modal.innerHTML = 'new page';
  gate.resolve(); await open;
  assert.equal(f.modal.innerHTML, 'new page');
  assert.equal(f.WB.accountOpen.verified, false);
  assert.equal(f.calls.filter(c => c.body).length, 0);
});

test('unconfirmed launch does not turn a read-only recheck into another launch', async () => {
  const f = setup({ onChange: () => { throw Object.assign(new Error('response lost'), { uncertain: true }); } });
  await f.change('codex', 'api-shared');
  await f.ACTIONS.wbOpenGo(f.button('wbOpenGo'));
  assert.match(f.modal.innerHTML, /请在原工具核对配置与任务状态/);
  assert.doesNotMatch(f.modal.innerHTML, /data-act="wbRecheck"/);
  await f.ACTIONS.wbRecheck();
  assert.equal(f.WB.accountOpen.executionUnknown, true);
  assert.equal(f.calls.filter(c => c.url === '/api/local/agent-change/v1').length, 1);
});

test('first discovery failure becomes a visible retry state instead of an endless skeleton', async () => {
  const f = setup({ onAccounts: () => { throw new Error('offline fixture'); } });
  f.S.local = null; f.context.clearTimeout = () => {};
  const view = { isConnected: true, innerHTML: 'skeleton' };
  f.RENDER_overview(view);
  await new Promise(resolve => setImmediate(resolve));
  assert.match(view.innerHTML, /读取 Agent 状态失败/);
  assert.match(view.innerHTML, /data-act="refresh"/);
  assert.doesNotMatch(view.innerHTML, /skeleton/);
});

test('web-only accounts never appear as callable keys in Agent dropdowns', () => {
  const f = setup({ claude: true });
  f.S.local.accounts.push({ id: 'browser-session-only', name: 'web only', libraryOnly: true });
  for (const tool of f.S.local.tools) assert.doesNotMatch(f.wbCard(tool), /browser-session-only/);
});
