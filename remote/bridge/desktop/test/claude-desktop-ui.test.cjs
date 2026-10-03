const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const code = fs.readFileSync(path.resolve(__dirname, '../../internal/console/web/claude-desktop.js'), 'utf8');

function fixture() {
  const elements = new Map();
  let modalContent = '';
  elements.set('#modalRoot', { get innerHTML() { return modalContent; }, set innerHTML(value) {
    modalContent = value;
    if (value.includes('id="cdTitle"')) elements.set('#cdTitle', {}); else elements.delete('#cdTitle');
    if (value.includes('id="cdModel"')) elements.set('#cdModel', { value: 'claude-sonnet-fixture' }); else elements.delete('#cdModel');
    if (value.includes('id="cdOverride"')) elements.set('#cdOverride', { checked: /id="cdOverride"[^>]*checked/.test(value) }); else elements.delete('#cdOverride');
  } });
  const calls = [], notices = [];
  const account = { id: 'shared', name: '<Shared API>', baseUrl: 'https://relay.invalid/v1', models: ['gpt-fixture', 'claude-sonnet-fixture', 'grok-fixture', 'deepseek-chat'], wire: 'auto' };
  const state = { local: { accounts: [account], bindings: { 'claude-desktop': { accountId: 'shared', model: 'gpt-fixture' } } } };
  let listener;
  let status = { supported: true, managed: false, mode: 'standard', requiresModeChange: true, message: '首次使用不同本地聊天库' };
  const $ = (selector) => elements.get(selector);
  const context = vm.createContext({
    S: state, $, ACTIONS: { wbOpen: async () => calls.push(['legacy-open']), localRestore: async () => calls.push(['legacy-restore']) },
    modelsOf: (a) => a?.models || [], bindingFor: () => state.local.bindings['claude-desktop'],
    accountFor: (id) => state.local.accounts.find((a) => a.id === id),
    esc: (s) => String(s ?? '').replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('"', '&quot;'),
    icon: () => '', wbHost: (url) => new URL(url).host, remoteLampHTML: (lamp) => `<span>${lamp.label}</span>`, desktopRemoteLamp: () => ({ state: 'off', label: '手机远程未连接' }),
    api: async (endpoint, input) => {
      calls.push([endpoint, input]);
      if (endpoint.endsWith('/status')) return status;
      if (endpoint === '/api/local/accounts') return state.local;
      if (endpoint.endsWith('/models')) return { models: account.models };
      if (endpoint.endsWith('/switch')) return { ok: true };
      if (endpoint.endsWith('/restore')) return { ok: true };
      throw new Error('Unexpected preview endpoint');
    },
    busy: async (_button, action) => action(), toast: (...args) => notices.push(args),
    renderAgentCards: () => {}, confirm: () => { throw new Error('Unexpected extra confirmation'); },
    wbBind: async (_target, patch) => { calls.push(['bind', patch]); if ('id' in patch) state.local.bindings['claude-desktop'].accountId = patch.id; if ('override' in patch) state.local.bindings['claude-desktop'].override = patch.override; },
    wbNewAccount: () => calls.push(['new-account']),
    document: { addEventListener: (_name, fn) => { listener = fn; } },
  });
  vm.runInContext(`${code}\nthis.testCD = { cdOpen, cdModels, wbClaudeDesktopCard, CD };`, context);
  return { context, calls, notices, account, elements, state, setStatus: (value) => { status = value; },
    html: () => modalContent, listener: async (value) => listener({ target: { value, hasAttribute: (name) => name === 'data-cd-key' } }),
    toggle: async (checked) => listener({ target: { checked, hasAttribute: (name) => name === 'data-cd-override' } }) };
}

test('Claude Desktop shares named API vault, distinguishes actual desktop remote capability and does not claim native chat control', () => {
  const f = fixture();
  const html = f.context.testCD.wbClaudeDesktopCard({ available: true });
  assert.match(html, /data-cd-key/); assert.match(html, /&lt;Shared API>/);
  assert.match(html, /手机远程未连接/); assert.match(html, /同一个第三方聊天库/);
  assert.doesNotMatch(html, /手机可继续|手动设置|data-act="wbOpenGo"/);
});

test('both the Agent card and advanced backup button use the dedicated Claude Desktop restore', async () => {
  const f = fixture(); f.context.confirm = () => true;
  await f.context.ACTIONS.localRestore({ dataset: { kind: 'claude-desktop' } });
  assert.equal(f.calls.some(([endpoint]) => endpoint === '/api/local/claude-desktop/restore'), true);
  assert.equal(f.calls.some(([endpoint]) => endpoint === 'legacy-restore'), false);
  await f.context.ACTIONS.localRestore({ dataset: { kind: 'codex' } });
  assert.equal(f.calls.at(-1)[0], 'legacy-restore');
});

test('selecting an API opens configuration only; no launch, extra confirmation or model picker', async () => {
  const f = fixture(); await f.listener('shared');
  assert.match(f.html(), /打开 Claude Desktop/); assert.match(f.html(), /配置并打开/);
  assert.match(f.html(), /刷新模型|覆盖 Claude 的模型菜单/);
  for (const model of f.account.models) assert.ok(f.html().includes(model));
  assert.doesNotMatch(f.html(), /id="cdModel"|默认模型|allowModeChange.*checkbox/);
  assert.equal(f.calls.filter(([endpoint]) => endpoint.endsWith('/switch')).length, 0);
  assert.equal(f.calls[0][0], 'bind');
});

test('explicit configure-and-open posts to dedicated 3P transaction and not CLI or legacy switch', async () => {
  const f = fixture(); await f.context.testCD.cdOpen();
  await f.context.ACTIONS.cdGo({ dataset: { id: 'shared' } });
  const mutations = f.calls.filter(([endpoint]) => endpoint.endsWith('/switch'));
  assert.equal(mutations.length, 1); assert.equal(mutations[0][0], '/api/local/claude-desktop/switch');
  assert.equal('model' in mutations[0][1], false);
  assert.equal(mutations[0][1].confirmed, true); assert.equal(mutations[0][1].allowModeChange, true);
  assert.equal(mutations[0][1].catalogOverride, true);
  assert.equal(f.html(), ''); assert.equal(f.calls.some(([name]) => name === 'legacy-open'), false);
});

test('unknown version or managed configuration remains disabled and cannot send switch', async () => {
  for (const status of [{ supported: false }, { supported: true, managed: true }]) {
    const f = fixture(); f.setStatus(status); await f.context.testCD.cdOpen();
    assert.match(f.html(), /data-act="cdGo"[^>]*disabled/);
    await f.context.ACTIONS.cdGo({ dataset: { id: 'shared' } });
    assert.equal(f.calls.some(([endpoint]) => endpoint.endsWith('/switch')), false);
  }
});

test('cancelled configuration cannot be resurrected by a late capability response', async () => {
  const f = fixture();
  let finish;
  f.context.api = (endpoint) => { assert.match(endpoint, /status$/); return new Promise((resolve) => { finish = resolve; }); };
  const pending = f.context.testCD.cdOpen();
  f.elements.get('#modalRoot').innerHTML = '';
  finish({ supported: true }); await pending;
  assert.equal(f.html(), '');
});

test('empty selection only saves selection, and new API path does not launch', async () => {
  const f = fixture(); await f.listener('');
  assert.equal(f.html(), ''); assert.equal(f.calls.length, 1);
  await f.listener('__new'); assert.equal(f.calls.at(-1)[0], 'new-account');
  assert.equal(f.calls.some(([endpoint]) => endpoint.endsWith('/switch')), false);
});

test('a non-Anthropic upstream can configure through the dedicated local translator', async () => {
  const f = fixture(); f.account.wire = 'responses'; await f.context.testCD.cdOpen();
  assert.doesNotMatch(f.html(), /data-act="cdGo"[^>]*disabled/);
  await f.context.ACTIONS.cdGo({ dataset: { id: 'shared' } });
  assert.equal(f.calls.some(([endpoint]) => endpoint.endsWith('/switch')), true);
});

test('saved model metadata is not exposed or overwritten by configuration-and-open', async () => {
  const f = fixture(); f.state.local.bindings['claude-desktop'].model = 'claude-private-fixture';
  await f.context.testCD.cdOpen();
  assert.doesNotMatch(f.html(), /claude-private-fixture|cdModel|默认模型/);
  await f.context.ACTIONS.cdGo({ dataset: { id: 'shared' } });
  assert.equal(f.state.local.bindings['claude-desktop'].model, 'claude-private-fixture');
});

test('refresh includes DeepSeek and every returned model, without launching or picking a default', async () => {
  const f = fixture(); await f.context.testCD.cdOpen();
  const api = f.context.api;
  f.context.api = async (endpoint, input) => {
    if (endpoint === '/api/local/models') f.account.models = ['deepseek-chat', 'grok-fresh', 'claude-sonnet-fixture'];
    return api(endpoint, input);
  };
  await f.context.ACTIONS.cdLoad({ dataset: { id: 'shared' } });
  assert.match(f.html(), /可用模型 · 3/);
  for (const model of f.account.models) assert.ok(f.html().includes(model));
  assert.doesNotMatch(f.html(), /gpt-fixture|默认模型|id="cdModel"/);
  assert.equal(f.calls.filter(([endpoint]) => endpoint === '/api/local/models').length, 1);
  assert.equal(f.calls.some(([endpoint]) => endpoint.endsWith('/switch')), false);
});

test('turning menu override off saves preference, and opening sends false without erasing models', async () => {
  const f = fixture(); await f.context.testCD.cdOpen();
  f.elements.get('#cdOverride').checked = false;
  await f.toggle(false);
  assert.equal(f.state.local.bindings['claude-desktop'].override, false);
  assert.equal(f.calls.some(([endpoint]) => endpoint.endsWith('/switch')), false);
  await f.context.testCD.cdOpen();
  assert.equal(f.elements.get('#cdOverride').checked, false);
  await f.context.ACTIONS.cdGo({ dataset: { id: 'shared' } });
  assert.equal(f.calls.find(([endpoint]) => endpoint.endsWith('/switch'))[1].catalogOverride, false);
  assert.ok(f.account.models.includes('deepseek-chat'));
});

test('cancelled model refresh cannot reopen the configuration dialog', async () => {
  const f = fixture(); await f.context.testCD.cdOpen();
  const api = f.context.api;
  let finish;
  f.context.api = (endpoint, input) => endpoint === '/api/local/models'
    ? new Promise((resolve) => { finish = resolve; }) : api(endpoint, input);
  const pending = f.context.ACTIONS.cdLoad({ dataset: { id: 'shared' } });
  f.elements.get('#modalRoot').innerHTML = '';
  finish({ models: f.account.models }); await pending;
  assert.equal(f.html(), '');
});
