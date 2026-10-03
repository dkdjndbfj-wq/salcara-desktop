'use strict';

// UI-only contract tests. All API calls use synthetic fixtures: no local credentials,
// processes, Electron windows, remote sites, or real Codex / Claude tools are touched.
const test = require('node:test');
const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const web = path.resolve(__dirname, '../../internal/console/web');
const appSource = fs.readFileSync(path.join(web, 'app.js'), 'utf8').replace(/\r\n/g, '\n');
const workbenchSource = fs.readFileSync(path.join(web, 'workbench.js'), 'utf8').replace(/\r\n/g, '\n');
const clone = (value) => JSON.parse(JSON.stringify(value));

class Element {
  constructor(doc, owner, attrs = {}) {
    this.doc = doc;
    this.owner = owner;
    this.id = attrs.id || '';
    this.value = attrs.value || '';
    this.dataset = attrs.dataset || {};
    this.checked = !!attrs.checked;
    this.disabled = !!attrs.disabled;
    this.isConnected = true;
    this.textContent = '';
    this.className = attrs.className || '';
    this.classList = { contains: (name) => this.className.split(/\s+/).includes(name) };
    this.children = [];
    this._html = '';
  }
  set innerHTML(html) {
    for (const child of this.children) {
      child.isConnected = false;
      if (child.id && this.doc.elements.get(child.id) === child) this.doc.elements.delete(child.id);
    }
    this.children = [];
    this._html = String(html);
    for (const match of this._html.matchAll(/<([a-z][\w-]*)([^>]*?)>/gi)) {
      const attrs = match[2];
      const get = (name) => attrs.match(new RegExp('(?:^|\\s)' + name + '="([^"]*)"'))?.[1] || '';
      const dataset = {};
      for (const data of attrs.matchAll(/\bdata-([a-z-]+)="([^"]*)"/g)) {
        dataset[data[1].replace(/-([a-z])/g, (_, letter) => letter.toUpperCase())] = data[2];
      }
      const node = new Element(this.doc, this, {
        id: get('id'), value: get('value'), className: get('class'), dataset,
        checked: /(?:^|\s)checked(?:\s|$)/.test(attrs),
        disabled: /(?:^|\s)disabled(?:\s|$)/.test(attrs),
      });
      this.children.push(node);
      if (node.id) this.doc.elements.set(node.id, node);
    }
  }
  get innerHTML() { return this._html; }
  querySelector(selector) {
    const act = selector.match(/^\[data-act=["']?([^"'\]]+)["']?\]$/)?.[1];
    return act ? this.children.find((node) => node.dataset.act === act) || null : null;
  }
  focus() {}
}

function fakeDocument() {
  const doc = {
    readyState: 'loading', elements: new Map(), listeners: new Map(),
    addEventListener(type, listener) {
      if (!this.listeners.has(type)) this.listeners.set(type, []);
      this.listeners.get(type).push(listener);
    },
    getElementById(id) { return this.elements.get(id) || null; },
    querySelector(selector) {
      const nested = selector.match(/^#([^\s]+)\s+(.+)$/);
      if (nested) return this.getElementById(nested[1])?.querySelector(nested[2]) || null;
      return selector.startsWith('#') ? this.getElementById(selector.slice(1)) : null;
    },
    querySelectorAll() { return []; },
  };
  for (const id of ['modalRoot', 'agentToolList', 'headActions', 'toastRoot']) {
    doc.elements.set(id, new Element(doc, null, { id }));
  }
  return doc;
}

function fixture() {
  return {
    accounts: [
      { id: 'api-shared', name: 'Shared test API', kind: 'api', baseUrl: 'https://example.test/v1', wire: 'auto', models: ['grok-test', 'claude-test', 'gpt-test'], model: '', keyMasked: 'synthetic-mask' },
      { id: 'api-empty', name: 'Empty catalog', kind: 'api', baseUrl: 'https://other.example.test/v1', wire: 'auto', models: [], model: '', keyMasked: 'synthetic-mask' },
    ],
    tools: [
      { id: 'codex-desktop', kind: 'codex', name: 'Codex Desktop', available: true, version: 'test' },
      { id: 'codex', kind: 'codex', name: 'Codex CLI', available: true, version: 'test' },
      { id: 'claude', kind: 'claude', name: 'Claude Code', available: true, version: 'test' },
      { id: 'claude-desktop', kind: 'claude', name: 'Claude Desktop', available: true, version: 'test' },
    ],
    bindings: Object.fromEntries(['codex-desktop', 'codex', 'claude', 'claude-desktop'].map((id) => [id, {
      accountId: '', model: '', protocol: id.startsWith('codex') ? 'responses' : 'anthropic',
      pending: false, remote: { state: 'off', label: '手机远程未连接', detail: 'Synthetic status' },
    }])),
    toolPaths: {},
  };
}

function setup(options = {}) {
  const document = fakeDocument(), backend = fixture(), calls = [], toasts = [];
  const context = vm.createContext({
    document, URL, console, location: { hash: '#overview' },
    window: { addEventListener() {} },
    setTimeout() { throw new Error('Unexpected startup timer'); },
    fetch() { throw new Error('Unexpected real network'); },
  });
  vm.runInContext(appSource, context, { filename: 'app.js' });
  vm.runInContext(workbenchSource, context, { filename: 'workbench.js' });
  context.api = async (url, body) => {
    calls.push({ url, body: body === undefined ? undefined : clone(body) });
    if (url === '/api/local/accounts' && body === undefined) return clone(backend);
    if (url === '/api/local/accounts' && body) {
      if (options.onSave) await options.onSave();
      const account = { ...clone(body), id: body.id || 'api-new', keyMasked: 'synthetic-mask' };
      delete account.key;
      backend.accounts.push(account);
      return { ok: true, account: clone(account) };
    }
    if (url === '/api/local/bind') {
      if (options.onBind) await options.onBind();
      const binding = backend.bindings[body.target];
      Object.assign(binding, { accountId: body.id, model: body.model, protocol: body.protocol, pending: !!body.id });
      if ('override' in body) binding.override = body.override;
      return { ok: true };
    }
    if (url === '/api/local/models') {
      if (options.onModels) await options.onModels();
      const account = backend.accounts.find((item) => item.id === body.id);
      account.models = ['grok-test', 'claude-test', 'gpt-test'];
      return { models: [...account.models] };
    }
    if (url === '/api/local/switch') return { ok: true };
    throw new Error('Unexpected test API route: ' + url);
  };
  context.toast = (message, kind) => toasts.push({ message, kind });
  context.localFixture = clone(backend);
  const exposed = vm.runInContext('S.local = localFixture; ({ S, ACTIONS, wbCard, wbOpenModal, wbRefreshCardStatus, renderAgentCards, inferProtocol, defaultModel })', context);
  exposed.renderAgentCards();
  return {
    ...exposed, document, backend, calls, toasts, context,
    modal: document.getElementById('modalRoot'),
    async change(target, value) {
      const element = { id: 'wbKey-' + target, dataset: { wbKey: target }, value };
      await Promise.all((document.listeners.get('change') || []).map((listener) => listener({ target: element })));
      return element;
    },
    button(act) { return document.getElementById('modalRoot').querySelector(`[data-act="${act}"]`); },
    assertNoLaunch() { assert.equal(calls.filter((call) => call.url === '/api/local/switch').length, 0); },
  };
}

function deferred() {
  let resolve;
  const promise = new Promise((done) => { resolve = done; });
  return { promise, resolve };
}

test('modal/editor layout and model rules remain stable after requested picker removal and shorter copy', () => {
  // Normalized-LF snapshots; the modal includes the requested compact copy.
  const snapshots = [
    [workbenchSource, 'wbOpenModal', '68fe245121cfa4b9ae4a41e4eeb7b4fc0d28ebe11724482d7fb9d71fb6adb8ab'],
    [workbenchSource, 'inferProtocol', 'f86fd4e83d8fd50613b471fefba9cc3598268196a9636e23b446d2438ad965dd'],
    [workbenchSource, 'defaultModel', 'e5f40804bc2b4e88ba62973d876bfc7e69abf468dd50b1f17b70f5a0c198520c'],
    [workbenchSource, 'wbLoadModels', '7ca02c5345baf84d4ce46d495a055dbc58b4cf22e62e3de7944269b9d10e2762'],
    [workbenchSource, 'wbBind', '05f0e3c29eb0e40d5d2bfd4c39684e27a140e40262ba0f71d307aa2773a280f8'],
    [appSource, 'localEditor', '73728443f6b8e33f51adb2f2aad22f92d2c33d2ee84e17353693ef24dc7fa79d'],
    [appSource, 'desktopRemoteLamp', 'a858dc477860a8e4de441aedfa08ac4a342fe4015914ac04507b9853f51c12da'],
  ];
  for (const [source, name, expected] of snapshots) {
    const start = source.indexOf('function ' + name + '(');
    assert.notEqual(start, -1, name);
    let body = source.slice(start, source.indexOf('\n}', start) + 2);
    if (name === 'wbOpenModal') body = body.replace('data-target="${esc(t.id)}">${icon(\'external\')}打开', 'data-target="${esc(t.id)}" ${models.length || bd.model ? \'\' : \'disabled\'}>${icon(\'external\')}打开');
    if (name === 'localEditor') body = body.replace('模型在原工具内选择。', '模型和接口在工具卡片里选。');
    assert.equal(crypto.createHash('sha256').update(body).digest('hex'), expected, name);
  }
});

test('all ordinary Agent dialogs have no default-model input and an empty catalog does not prevent opening', async () => {
  for (const target of ['codex', 'codex-desktop', 'claude']) {
    const ui = setup({ onModels: () => { throw new Error('Catalog unavailable fixture'); } });
    await ui.change(target, 'api-empty');
    assert.doesNotMatch(ui.modal.innerHTML, /默认模型|id="(?:toolModel|wbModel)|<select[^>]*(?:model|Model)|<input[^>]*(?:model|Model)/);
    assert.equal(ui.button('wbOpenGo').disabled, false);
    await ui.ACTIONS.wbOpenGo(ui.button('wbOpenGo'));
    const request = ui.calls.find((call) => call.url === '/api/local/switch');
    assert.equal(request.body.keepModel, true); assert.equal('model' in request.body, false);
  }
});

test('saved API selection waits for successful bind, then opens the original dialog without launching', async () => {
  const gate = deferred();
  const ui = setup({ onBind: () => gate.promise });
  const selecting = ui.change('codex-desktop', 'api-shared');
  assert.equal(ui.modal.innerHTML, '');
  assert.equal(ui.calls[0].url, '/api/local/bind');
  gate.resolve();
  await selecting;
  assert.match(ui.modal.innerHTML, /aria-labelledby="wbOpenTitle"/);
  assert.match(ui.modal.innerHTML, /Shared test API/);
  assert.equal(ui.document.getElementById('wbReady').checked, true);
  assert.equal(ui.calls[0].body.model, 'gpt-test');
  assert.equal(ui.calls[0].body.protocol, 'responses');
  ui.assertNoLaunch();
  ui.ACTIONS.closeModal(ui.button('closeModal'), { target: { closest: () => null } });
  assert.equal(ui.modal.innerHTML, '');
  ui.assertNoLaunch();
});

test('no API selection saves native-login binding but does not open a dialog or launch', async () => {
  const ui = setup();
  await ui.change('codex', '');
  assert.equal(ui.modal.innerHTML, '');
  assert.deepEqual(ui.calls[0].body, { target: 'codex', id: '', model: '', protocol: '' });
  ui.assertNoLaunch();
});

test('bind failure reports the error and never opens or launches the tool', async () => {
  const ui = setup({ onBind: () => { throw new Error('Synthetic bind failure'); } });
  await ui.change('claude', 'api-shared');
  assert.equal(ui.modal.innerHTML, '');
  assert.equal(ui.toasts.at(-1).message, 'Synthetic bind failure');
  ui.assertNoLaunch();
});

test('Claude default stays native and all named API keys remain shared between tool cards', async () => {
  const ui = setup();
  await ui.change('claude', 'api-shared');
  assert.equal(ui.calls[0].body.model, 'claude-test');
  assert.equal(ui.calls[0].body.protocol, 'anthropic');
  for (const target of ['codex-desktop', 'codex', 'claude']) {
    const html = ui.wbCard(ui.S.local.tools.find((item) => item.id === target));
    assert.match(html, /value="api-shared"/);
    assert.match(html, /value="api-empty"/);
    assert.match(html, /data-act="toolRestore"/);
    assert.match(html, /<details class="wb-card-more">/);
  }
  ui.assertNoLaunch();
});

test('empty model catalog shows the dialog before model loading and keeps existing Codex override behavior', async () => {
  let sawOpenBeforeLoad = false, ui;
  ui = setup({ onModels: () => { sawOpenBeforeLoad = !!ui.document.getElementById('wbOpenTitle'); } });
  await ui.change('codex-desktop', 'api-empty');
  assert.equal(sawOpenBeforeLoad, true);
  assert.equal(ui.calls.filter((call) => call.url === '/api/local/models').length, 1);
  assert.equal(ui.S.local.bindings['codex-desktop'].override, true);
  assert.match(ui.modal.innerHTML, /data-wb-override="codex-desktop" checked/);
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
  assert.deepEqual(switches[0].body, { id: 'api-shared', target: 'codex-desktop', confirmed: true, keepModel: true });
});

test('add API from a tool card saves the shared key then returns to the original open dialog without launch', async () => {
  const ui = setup();
  await ui.change('claude', '__new');
  const save = ui.button('localSave');
  assert.equal(save.dataset.wbTarget, 'claude');
  ui.document.getElementById('laName').value = 'New shared API';
  ui.document.getElementById('laKey').value = 'synthetic-contract-key';
  ui.document.getElementById('laAuth').value = 'bearer';
  ui.document.getElementById('laWire').value = 'auto';
  ui.S.localDraftModels = ['claude-test'];
  await ui.ACTIONS.localSave(save);
  assert.equal(ui.S.local.bindings.claude.accountId, 'api-new');
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
  assert.match(html, /data-act="toolRestore" data-target="claude"/);
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
