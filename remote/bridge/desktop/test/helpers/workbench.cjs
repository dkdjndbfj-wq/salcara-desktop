'use strict';

// UI-only contract tests. All API calls use synthetic fixtures: no local credentials,
// processes, Electron windows, remote sites, or real Codex / Claude tools are touched.
const test = require('node:test');
const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const web = path.resolve(__dirname, '../../../internal/console/web');
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
    this.writeCount = (this.writeCount || 0) + 1;
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
  hasAttribute(name) { return name === 'data-cd-key' && Object.hasOwn(this.dataset, 'cdKey') || name === 'data-cd-override' && Object.hasOwn(this.dataset, 'cdOverride'); }
  setAttribute() {}
  removeAttribute() {}
  getAttribute() { return null; }
}

function fakeDocument() {
  const doc = {
    readyState: 'loading', hidden: false, elements: new Map(), listeners: new Map(),
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
      { id: 'api-shared', sourceRevision: 'src-v1-' + 'a'.repeat(64), name: 'Shared test API', kind: 'api', baseUrl: 'https://example.test/v1', wire: 'auto', models: ['grok-test', 'claude-test', 'gpt-test'], model: '', keyMasked: 'synthetic-mask' },
      { id: 'api-empty', sourceRevision: 'src-v1-' + 'b'.repeat(64), name: 'Empty catalog', kind: 'api', baseUrl: 'https://other.example.test/v1', wire: 'auto', models: [], model: '', keyMasked: 'synthetic-mask' },
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
    AbortController, crypto: crypto.webcrypto,
    document, URL, console, location: { hash: '#overview' },
    window: { addEventListener() {} },
    setTimeout() { throw new Error('Unexpected startup timer'); },
    fetch() { throw new Error('Unexpected real network'); },
  });
  vm.runInContext(appSource, context, { filename: 'app.js' });
  vm.runInContext(fs.readFileSync(path.join(web, 'launch-progress.js'), 'utf8'), context, { filename: 'launch-progress.js' });
  vm.runInContext(workbenchSource, context, { filename: 'workbench.js' });
  context.api = async (url, body, method, requestOptions = {}) => {
    calls.push({ url, body: body === undefined ? undefined : clone(body) });
    if (url === '/api/local/accounts' && body === undefined) { if (options.onAccounts) await options.onAccounts(); return clone(backend); }
    if (url === '/api/local/accounts' && body) {
      if (options.onSave) await options.onSave();
      const account = { sourceRevision: 'src-v1-' + 'b'.repeat(64), ...clone(body), id: body.id || 'api-new', keyMasked: 'synthetic-mask' };
      delete account.key;
      backend.accounts.push(account);
      return { ok: true, account: clone(account) };
    }
    if (url === '/api/local/agent-change/v1') {
      if (options.onChange) await options.onChange(body, requestOptions);
      if (requestOptions.signal?.aborted) throw Object.assign(new Error('cancelled'), { cancelled: true });
      const binding = backend.bindings[body.target];
      Object.assign(binding, { accountId: body.id, model: body.model, protocol: body.wireProtocol, override: body.override, pending: false });
      return { ok: true, protocol: body.protocol, operationId: body.operationId };
    }
    if (url === '/api/local/models') {
      if (options.onModels) await options.onModels();
      const account = backend.accounts.find((item) => item.id === body.id);
      account.models = ['grok-test', 'claude-test', 'gpt-test'];
      return { models: [...account.models] };
    }
    if (url === '/api/local/claude-desktop/status') { if (options.onStatus) return await options.onStatus(); return { supported: true, managed: false, requiresModeChange: true }; }
    if (url === '/api/local/claude-desktop/restore' || url === '/api/local/default') return { ok: true };
    throw new Error('Unexpected test API route: ' + url);
  };
  context.launchAPI = (url, body, progress, requestOptions) => context.api(url, body, undefined, requestOptions);
  if (options.claude) vm.runInContext(fs.readFileSync(path.join(web, 'claude-desktop.js'), 'utf8'), context, { filename: 'claude-desktop.js' });
  context.confirm = async () => true;
  context.toast = (message, kind) => toasts.push({ message, kind });
  context.localFixture = clone(backend);
  const exposed = vm.runInContext('S.local = localFixture; ({ S, WB, ACTIONS, wbCard, wbOpenModal, wbRefreshCardStatus, renderAgentCards, wbPollDiscovery, inferProtocol, defaultModel, wbOpenAccount, wbAccountModels, wbAccountCurrent, RENDER_overview })', context);
  exposed.renderAgentCards();
  return {
    ...exposed, document, backend, calls, toasts, context,
    modal: document.getElementById('modalRoot'),
    async change(target, value) {
      const element = target === 'claude-desktop' && options.claude ? { id: 'cdKey', dataset: {}, value, hasAttribute: name => name === 'data-cd-key' } : { id: 'wbKey-' + target, dataset: { wbKey: target }, value };
      await Promise.all((document.listeners.get('change') || []).map((listener) => listener({ target: element })));
      return element;
    },
    button(act) { return document.getElementById('modalRoot').querySelector(`[data-act="${act}"]`); },
    assertNoLaunch() { assert.equal(calls.filter((call) => call.url === '/api/local/agent-change/v1').length, 0); },
  };
}


function deferred() { let resolve, reject; const promise = new Promise((yes, no) => { resolve = yes; reject = no; }); return { promise, resolve, reject }; }
module.exports = { setup, deferred, appSource, workbenchSource, web };
