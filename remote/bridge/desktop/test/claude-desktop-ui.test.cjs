'use strict';
const { test } = require('node:test');
const assert = require('node:assert/strict');
const vm = require('node:vm');
const { setup, deferred } = require('./helpers/workbench.cjs');
const fixture = options => setup({ claude: true, ...options });
const ready = f => { f.document.getElementById('cdReady').checked = true; };

test('Claude Desktop card keeps original styling and dedicated restore route', async () => {
  const f = fixture();
  const html = f.wbCard(f.S.local.tools.find(t => t.id === 'claude-desktop'));
  assert.match(html, /data-cd-key/); assert.match(html, /同一个第三方聊天库/);
  assert.doesNotMatch(html, /data-act="wbOpenGo"/);
  f.context.route = async () => {};
  await f.ACTIONS.localRestore({ dataset: { kind: 'claude-desktop' } });
  assert.equal(f.calls.some(c => c.url === '/api/local/claude-desktop/restore'), true);
});

test('Claude selection checks capability without saving or launching; cancellation leaves binding intact', async () => {
  const f = fixture();
  await f.change('claude-desktop', 'api-shared');
  assert.match(f.modal.innerHTML, /打开 Claude Desktop/);
  assert.match(f.modal.innerHTML, /配置并打开/);
  assert.equal(f.document.getElementById('cdReady').checked, false);
  assert.equal(f.button('cdGo').disabled, true);
  assert.equal(f.calls.filter(c => c.body).length, 0);
  assert.equal(f.backend.bindings['claude-desktop'].accountId, '');
  f.ACTIONS.closeModal(f.button('closeModal'), { target: { closest: () => null } });
  assert.equal(f.modal.innerHTML, '');
});

test('unbound and non-callable Claude choices show the selected placeholder', () => {
  const f = fixture(), tool = f.S.local.tools.find(t => t.id === 'claude-desktop');
  assert.match(f.wbCard(tool), /<option value="" disabled selected>选择 API 密钥/);
  f.S.local.accounts.push({ id: 'web-only', libraryOnly: true, name: 'Website login' });
  f.S.local.bindings['claude-desktop'].accountId = 'web-only';
  const html = f.wbCard(tool);
  assert.match(html, /<option value="" disabled selected>选择 API 密钥/);
  assert.match(html, /data-act="cdOpen" disabled/);
  assert.doesNotMatch(html, /value="web-only"/);
});

test('explicit Claude confirmation uses the dedicated source-bound transaction once', async () => {
  const gate = deferred(), f = fixture({ onSwitch: () => gate.promise });
  await f.change('claude-desktop', 'api-shared'); ready(f);
  const button = f.button('cdGo');
  const first = f.ACTIONS.cdGo(button);
  await Promise.resolve(); await Promise.resolve();
  const second = f.ACTIONS.cdGo(button);
  gate.resolve(); await Promise.all([first, second]);
  const writes = f.calls.filter(c => c.url.endsWith('/switch'));
  assert.equal(writes.length, 1);
  assert.equal(writes[0].url, '/api/local/claude-desktop/switch');
  assert.deepEqual(writes[0].body, { id: 'api-shared', confirmed: true, allowModeChange: true, catalogOverride: true, expectedSourceRevision: 'src-v1-' + 'a'.repeat(64) });
  assert.equal(f.calls.some(c => c.url === '/api/local/bind'), false);
  assert.equal(f.modal.innerHTML, '');
});

test('unsupported or managed Claude configuration cannot launch', async () => {
  for (const status of [{ supported: false }, { supported: true, managed: true }]) {
    const f = fixture({ onStatus: () => status });
    await f.change('claude-desktop', 'api-shared'); ready(f);
    await f.ACTIONS.cdGo(f.button('cdGo'));
    assert.equal(f.calls.some(c => c.url.endsWith('/switch')), false);
  }
});

test('late Claude status and catalog reads cannot reopen a closed dialog', async () => {
  for (const kind of ['status', 'models']) {
    const gate = deferred(), f = fixture(kind === 'status' ? { onStatus: () => gate.promise } : { onModels: () => gate.promise });
    let request = f.change('claude-desktop', 'api-shared');
    if (kind === 'models') { await request; request = f.ACTIONS.cdLoad(f.button('cdLoad')); }
    else { await Promise.resolve(); await Promise.resolve(); }
    f.ACTIONS.closeModal(f.button('closeModal'), { target: { closest: () => null } });
    gate.resolve({ supported: true }); await request;
    assert.equal(f.modal.innerHTML, '');
    assert.equal(f.calls.some(c => c.url.endsWith('/switch')), false);
  }
});

test('Claude menu preference is a draft until explicit confirmation and preserves model metadata', async () => {
  const f = fixture();
  f.backend.accounts[0].model = 'grok-test';
  await f.change('claude-desktop', 'api-shared');
  const draft = f.WB.accountOpen;
  for (const listener of f.document.listeners.get('change')) await listener({ target: { id: 'cdOverride', dataset: {}, checked: false, hasAttribute: name => name === 'data-cd-override' } });
  assert.equal(draft.override, false);
  assert.equal(f.calls.filter(c => c.body).length, 0);
  await f.ACTIONS.cdLoad(f.button('cdLoad'));
  assert.equal(f.WB.accountOpen.override, false);
  assert.match(f.modal.innerHTML, /grok-test/);
  ready(f); await f.ACTIONS.cdGo(f.button('cdGo'));
  const mutation = f.calls.find(c => c.url === '/api/local/claude-desktop/switch');
  assert.equal(mutation.body.catalogOverride, false);
  assert.equal('model' in mutation.body, false);
  assert.equal(f.backend.accounts[0].model, 'grok-test');
});

test('Claude capability read failure has a recoverable recheck action', async () => {
  let fail = true;
  const f = fixture({ onStatus: () => { if (fail) throw new Error('status unavailable'); return { supported: true }; } });
  await f.change('claude-desktop', 'api-shared');
  assert.match(f.modal.innerHTML, /status unavailable/);
  assert.match(f.modal.innerHTML, /重新检查/);
  fail = false; await f.ACTIONS.wbRecheck();
  assert.equal(f.WB.accountOpen.verified, true);
  assert.equal(f.WB.accountOpen.status.supported, true);
  assert.equal(f.document.getElementById('cdReady').checked, false);
});
