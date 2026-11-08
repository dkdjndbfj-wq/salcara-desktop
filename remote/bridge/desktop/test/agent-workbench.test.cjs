'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const { setup, deferred, appSource, workbenchSource, web } = require('./helpers/workbench.cjs');
const clone = value => JSON.parse(JSON.stringify(value));

test('unchanged discovery snapshots preserve Agent card DOM; real changes repaint it', () => {
  const f = setup(), list = f.document.getElementById('agentToolList'), initialWrites = list.writeCount;
  f.S.local = clone(f.S.local);
  f.renderAgentCards();
  assert.equal(list.writeCount, initialWrites, 'an identical discovery result must not replace selects, menus or focusable controls');
  f.S.local.tools[0].available = false;
  f.renderAgentCards();
  assert.equal(list.writeCount, initialWrites + 1, 'a changed tool status must update its visible card');
});

test('tool discovery refresh backs off while still scheduling its first poll promptly', () => {
  const f = setup(), delays = [];
  f.context.setTimeout = (_callback, delay) => { delays.push(delay); return delays.length; };
  f.context.clearTimeout = () => {};
  f.wbPollDiscovery({ isConnected: true });
  f.wbPollDiscovery({ isConnected: true });
  f.wbPollDiscovery({ isConnected: true });
  assert.deepEqual(delays, [500, 1000, 2000]);
  assert.equal(f.WB.discoveryDelay, 4000);
});

test('Agent and API modal close buttons delegate immediately without launching or waiting for a backend', async () => {
  for (const editor of [false, true]) {
    const f = setup();
    if (editor) vm.runInContext('localEditor(null)', f.context);
    else {
      f.S.local.bindings.codex.accountId = 'api-shared';
      f.wbOpenModal('codex');
    }
    const button = f.modal.children.find(node => node.dataset.act === 'closeModal' && node.classList.contains('icon-btn'));
    assert.ok(button, 'the actual modal template must expose its close control');
    let prevented = false;
    // Include a child target (SVG/text node wrapper), not only the button box.
    const event = { target: { closest: selector => selector === '[data-act]' ? button : null }, preventDefault() { prevented = true; } };
    for (const listener of f.document.listeners.get('click')) listener(event);
    await Promise.resolve();
    assert.equal(prevented, true);
    assert.equal(f.modal.innerHTML, '');
    assert.equal(f.calls.length, 0);
    f.assertNoLaunch();
  }
});

test('a late model response cannot restore an Agent modal after its close button is clicked', async () => {
  const gate = deferred(), f = setup({ onModels: () => gate.promise });
  f.S.local.bindings.codex.accountId = 'api-shared';
  f.backend.bindings.codex.accountId = 'api-shared';
  await f.wbOpenAccount('codex', 'api-shared');
  const load = f.wbAccountModels(f.WB.accountOpen, true);
  const button = f.modal.children.find(node => node.dataset.act === 'closeModal' && node.classList.contains('icon-btn'));
  const event = { target: { closest: selector => selector === '[data-act]' ? button : null }, preventDefault() {} };
  for (const listener of f.document.listeners.get('click')) listener(event);
  assert.equal(f.modal.innerHTML, '');
  gate.resolve();
  await load;
  assert.equal(f.modal.innerHTML, '');
  f.assertNoLaunch();
});

test('modal/editor layout and model rules remain stable after requested picker removal and shorter copy', () => {
  const ui = setup();
  vm.runInContext('localEditor(null)', ui.context);
  assert.match(ui.modal.innerHTML, /class="modal account-modal[^"]*"[^>]*role="dialog"[^>]*aria-modal="true"/);
  assert.match(ui.modal.innerHTML, /id="laKey" type="password" autocomplete="off"/);
  assert.match(ui.modal.innerHTML, /<input type="hidden" id="laModel"/);
  assert.match(ui.modal.innerHTML, /<(?:textarea|input)[^>]*id="laModels"/);
  assert.match(ui.modal.innerHTML, /data-act="closeModal" aria-label="关闭"/);
  assert.match(ui.modal.innerHTML, /class="btn primary" data-act="localSave"/);
  assert.doesNotMatch(ui.modal.innerHTML, /<select[^>]*id="laModel"/);
});

test('all ordinary Agent dialogs have no default-model input and an empty catalog does not prevent opening', async () => {
  for (const target of ['codex', 'codex-desktop', 'claude']) {
    const ui = setup({ onModels: () => { throw new Error('Catalog unavailable fixture'); } });
    await ui.change(target, 'api-empty');
    assert.doesNotMatch(ui.modal.innerHTML, /默认模型|id="(?:toolModel|wbModel)|<select[^>]*(?:model|Model)|<input[^>]*(?:model|Model)/);
    assert.equal(ui.document.getElementById('wbReady').checked, false);
    ui.document.getElementById('wbReady').checked = true;
    await ui.ACTIONS.wbOpenGo(ui.button('wbOpenGo'));
    const request = ui.calls.find((call) => call.url === '/api/local/switch');
    assert.equal(request.body.keepModel, true); assert.equal('model' in request.body, false);
  }
});

test('selecting a saved API only verifies a draft; cancelling preserves the prior binding', async () => {
  const gate = deferred(), ui = setup({ onAccounts: () => gate.promise });
  ui.backend.bindings['codex-desktop'].accountId = 'api-empty';
  ui.S.local.bindings['codex-desktop'].accountId = 'api-empty';
  const selecting = ui.change('codex-desktop', 'api-shared');
  assert.match(ui.modal.innerHTML, /正在检查账号与工具/);
  assert.equal(ui.calls[0].url, '/api/local/accounts');
  assert.equal(ui.calls.filter(call => call.body).length, 0);
  gate.resolve(); await selecting;
  assert.match(ui.modal.innerHTML, /Shared test API/);
  assert.equal(ui.document.getElementById('wbReady').checked, false);
  assert.equal(ui.backend.bindings['codex-desktop'].accountId, 'api-empty');
  ui.ACTIONS.closeModal(ui.button('closeModal'), { target: { closest: () => null } });
  assert.equal(ui.modal.innerHTML, '');
  assert.equal(ui.calls.filter(call => call.url === '/api/local/bind').length, 0);
  ui.assertNoLaunch();
});

test('empty selection is a placeholder and never silently changes the tools login', async () => {
  const ui = setup();
  ui.S.local.bindings.codex.accountId = 'api-shared';
  const select = await ui.change('codex', '');
  assert.equal(select.value, 'api-shared');
  assert.equal(ui.calls.length, 0);
  ui.assertNoLaunch();
});

test('bind failure appears in the owned dialog and offers recheck without launching', async () => {
  const ui = setup({ onBind: () => { throw new Error('Synthetic bind failure'); } });
  await ui.change('claude', 'api-shared');
  ui.document.getElementById('wbReady').checked = true;
  await ui.ACTIONS.wbOpenGo(ui.button('wbOpenGo'));
  assert.match(ui.modal.innerHTML, /Synthetic bind failure/);
  assert.match(ui.modal.innerHTML, /重新检查/);
  assert.equal(ui.document.getElementById('wbReady').checked, false);
  ui.assertNoLaunch();
});

test('Claude default stays native and all named API keys remain shared between tool cards', async () => {
  const ui = setup();
  await ui.change('claude', 'api-shared');
  assert.equal(ui.defaultModel(ui.S.local.accounts[0], 'claude'), 'claude-test');
  assert.equal(ui.calls.filter(call => call.url === '/api/local/bind').length, 0);
  for (const target of ['codex-desktop', 'codex', 'claude']) {
    const html = ui.wbCard(ui.S.local.tools.find((item) => item.id === target));
    assert.match(html, /value="api-shared"/);
    assert.match(html, /value="api-empty"/);
    assert.doesNotMatch(html, /data-act="toolRestore"/);
    assert.match(html, /<details class="wb-card-more">/);
  }
  ui.assertNoLaunch();
});

test('empty model catalog shows the dialog before loading and preserves the explicit Codex override choice', async () => {
  let sawOpenBeforeLoad = false, ui;
  ui = setup({ onModels: () => { sawOpenBeforeLoad = !!ui.document.getElementById('wbOpenTitle'); } });
  ui.backend.bindings['codex-desktop'].override = false;
  ui.S.local.bindings['codex-desktop'].override = false;
  await ui.change('codex-desktop', 'api-empty');
  assert.equal(sawOpenBeforeLoad, true);
  assert.equal(ui.calls.filter((call) => call.url === '/api/local/models').length, 1);
  assert.equal(ui.S.local.bindings['codex-desktop'].override, false);
  assert.doesNotMatch(ui.modal.innerHTML, /data-wb-override="codex-desktop" checked/);
  assert.ok(ui.calls.filter(call => call.url === '/api/local/bind').every(call => !Object.hasOwn(call.body, 'override')));
  ui.assertNoLaunch();
});

test('only the original explicit Open confirmation can switch, and its task checkbox is still enforced', async () => {
  const ui = setup();
  await ui.change('codex-desktop', 'api-shared');
  const open = ui.button('wbOpenGo');
  ui.document.getElementById('wbReady').checked = false;
  await ui.ACTIONS.wbOpenGo(open);
  ui.assertNoLaunch();
  ui.document.getElementById('wbReady').checked = true;
  await ui.ACTIONS.wbOpenGo(open);
  const switches = ui.calls.filter((call) => call.url === '/api/local/switch');
  assert.equal(switches.length, 1);
  assert.equal(ui.calls.filter(call => /^\/api\/sessions?\b/.test(call.url)).length, 0, 'opening an Agent must not load history');
  assert.doesNotMatch(ui.modal.innerHTML, /data-act="toolRestore"|查看会话/);
  assert.deepEqual(switches[0].body, { id: 'api-shared', target: 'codex-desktop', confirmed: true, keepModel: true, expectedSourceRevision: 'src-v1-' + 'a'.repeat(64) });
});

test('add API from a tool card saves the shared key then returns to the original open dialog without launch', async () => {
  const ui = setup();
  await ui.change('claude', '__new');
  const save = ui.button('localSave');
  assert.equal(save.dataset.wbTarget, 'claude');
  ui.document.getElementById('laName').value = 'New shared API';
  ui.document.getElementById('laKey').value = 'synthetic-contract-key';
  assert.ok(ui.document.getElementById('laAuth'), 'custom providers can override authentication in advanced settings');
  assert.ok(ui.document.getElementById('laWire'), 'custom providers can override their wire protocol in advanced settings');
  ui.S.localDraftModels = ['claude-test'];
  await ui.ACTIONS.localSave(save);
  assert.equal(ui.S.local.bindings.claude.accountId, '');
  assert.equal(ui.calls.filter(call => call.url === '/api/local/bind').length, 0);
  assert.match(ui.modal.innerHTML, /aria-labelledby="wbOpenTitle"/);
  assert.match(ui.modal.innerHTML, /New shared API/);
  assert.equal(ui.context.location.hash, '#overview');
  ui.assertNoLaunch();
});

test('cancelled add-key dialog leaves no continuation that could affect a later vault save', async () => {
  const ui = setup();
  await ui.change('codex', '__new');
  const oldSave = ui.button('localSave');
  ui.ACTIONS.closeModal(ui.button('closeModal'), { target: { closest: () => null } });
  assert.equal(oldSave.isConnected, false);
  await ui.ACTIONS.localNew({ dataset: {} });
  const save = ui.button('localSave');
  assert.equal(save.dataset.wbTarget, undefined);
  ui.document.getElementById('laName').value = 'Standalone vault API';
  ui.document.getElementById('laKey').value = 'synthetic-contract-key';
  await ui.ACTIONS.localSave(save);
  assert.equal(ui.calls.filter((call) => call.url === '/api/local/bind').length, 0);
  assert.equal(ui.context.location.hash, 'accounts');
  ui.assertNoLaunch();
});

test('closing a pending new-key save does not resurrect its tool confirmation dialog', async () => {
  const gate = deferred();
  const ui = setup({ onSave: () => gate.promise });
  await ui.change('codex-desktop', '__new');
  ui.document.getElementById('laName').value = 'Pending synthetic API';
  ui.document.getElementById('laKey').value = 'synthetic-contract-key';
  const saving = ui.ACTIONS.localSave(ui.button('localSave'));
  ui.ACTIONS.closeModal(ui.button('closeModal'), { target: { closest: () => null } });
  gate.resolve();
  await saving;
  assert.equal(ui.modal.innerHTML, '');
  assert.equal(ui.calls.filter((call) => call.url === '/api/local/bind').length, 0);
  ui.assertNoLaunch();
});

test('without the dedicated Claude Desktop adapter, fallback stays manual and does not claim cloud chat remote control', () => {
  const ui = setup();
  const html = ui.wbCard(ui.S.local.tools.find((item) => item.id === 'claude-desktop'));
  assert.doesNotMatch(html, /data-wb-key|data-act="wbOpen"/);
  assert.match(html, /Bridge 不会自动改写或重启/);
  assert.match(html, /普通云端聊天不能由 Bridge 遥控/);
  assert.doesNotMatch(html, /data-act="toolRestore"/);
});

test('status refresh updates lamp/chip without replacing the selected API or original dialog', async () => {
  const ui = setup();
  await ui.change('codex', 'api-shared');
  const select = ui.document.getElementById('wbKey-codex'), modal = ui.modal.innerHTML;
  ui.S.local.bindings.codex.remote = { state: 'connected', label: '已连接', detail: 'Synthetic connected status' };
  ui.wbRefreshCardStatus('codex');
  assert.equal(ui.document.getElementById('wbKey-codex'), select);
  assert.equal(ui.modal.innerHTML, modal);
  assert.match(ui.document.getElementById('lamp-codex').innerHTML, /lamp-connected/);
  ui.assertNoLaunch();
});

test('shell navigation uses Agent and API key names, shows usage, and keeps conversations inside settings', () => {
  const html = fs.readFileSync(path.join(web, 'index.html'), 'utf8');
  for (const [route, label] of [['overview', 'Agent'], ['accounts', 'API 密钥'], ['setup', '环境'], ['login', '手机远程'], ['usage', '用量'], ['settings', '设置']]) {
    assert.match(html, new RegExp(`data-route="${route}"[^\\n]*<b>${label}</b>`));
  }
  assert.doesNotMatch(html, /data-route="sessions"/);
  assert.match(appSource, /class="card set-link" href="#sessions"/);
  assert.match(appSource, /sessions: \(v\) => RENDER_sessions\(v\)/);
});
