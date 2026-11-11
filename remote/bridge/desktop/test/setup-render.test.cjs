'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.resolve(__dirname, '../../internal/console/web/setup.js'), 'utf8');

function fixture() {
  const elements = new Map();
  const element = id => {
    const node = { id, _html: '', writes: 0, hidden: false, isConnected: true };
    Object.defineProperty(node, 'innerHTML', { get() { return this._html; }, set(value) { this._html = String(value); this.writes++; } });
    elements.set(id, node);
    return node;
  };
  element('headActions'); element('setupDot');
  const context = vm.createContext({
    S: { route: 'setup' }, ACTIONS: {}, RENDER: {},
    $: id => elements.get(id.replace(/^#/, '')) || null,
    icon: () => '', ico: () => '', esc: value => String(value),
    localToolPathsPanel: () => '<div>tools</div>',
    api: async () => { throw new Error('Unexpected API request'); },
    setTimeout: () => 1, clearTimeout() {},
    document: { hidden: false }, Date, JSON, Math,
  });
  vm.runInContext(source, context, { filename: 'setup.js' });
  const view = { isConnected: true, _html: '', writes: 0 };
  Object.defineProperty(view, 'innerHTML', { get() { return this._html; }, set(value) { this._html = String(value); this.writes++; } });
  return { context, elements, view };
}

const snapshot = () => ({ os: 'windows', discoveryPending: true, items: [
  { id: 'codex', name: 'Codex CLI', required: true, installed: false, source: '', installable: true, download: '', version: '', role: '远程会话', note: '' },
] });

test('unchanged setup status does not recreate fixed page content or action controls', () => {
  const f = fixture(), state = snapshot(), local = { tools: [{ id: 'codex', name: 'Codex CLI', available: false, path: '' }], toolPaths: {} };
  f.context.renderSetup(f.view, state, local);
  const viewWrites = f.view.writes, actionWrites = f.elements.get('headActions').writes;
  f.context.renderSetup(f.view, structuredClone(state), structuredClone(local));
  assert.equal(f.view.writes, viewWrites);
  assert.equal(f.elements.get('headActions').writes, actionWrites);
  state.items[0].installed = true;
  f.context.renderSetup(f.view, state, local);
  assert.equal(f.view.writes, viewWrites + 1, 'a real tool state change still refreshes visible content');
});

test('optional missing Agents never block pairing or falsely claim a ready remote connection', () => {
  for (const installed of [false, true]) {
    const f = fixture(), state = snapshot();
    state.discoveryPending = false;
    state.items[0].required = false;
    state.items[0].installed = installed;
    state.items.push({ id: 'claude', name: 'Claude Code', required: false, installed: false, installable: true });
    f.context.renderSetup(f.view, state, null);
    assert.equal(f.elements.get('setupDot').hidden, true);
    assert.match(f.view.innerHTML, /远程连接已内置/);
    assert.match(f.view.innerHTML, /使用哪个 Agent 就准备哪个/);
    assert.doesNotMatch(f.view.innerHTML, /装好后手机才能|环境已就绪，手机可以/);
    assert.match(f.view.innerHTML, /data-tool="claude"/);
  }
});

