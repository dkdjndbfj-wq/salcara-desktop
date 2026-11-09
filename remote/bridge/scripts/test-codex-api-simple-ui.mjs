import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';
import crypto from 'node:crypto';

const sources = await Promise.all(['launch-progress.js', 'codex-accounts.js', 'codex-api-images.js', 'codex-api-network.js', 'codex-api-readiness.js', 'codex-api-observation.js'].map((name) => readFile(new URL('../internal/console/web/' + name, import.meta.url), 'utf8')));
const shellSource = await readFile(new URL('../internal/console/web/shell.js', import.meta.url), 'utf8');
function deferred() {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}
async function settle() { for (let i = 0; i < 12; i++) await Promise.resolve(); }
const decode = (text) => text.replace(/&quot;/g, '"').replace(/&#39;/g, "'").replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&amp;/g, '&');
const escape = (text) => String(text).replace(/[&<>"']/g, (char) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[char]);

// A small DOM contract, not a rendering engine. Absent/detached IDs resolve to
// null, observers run in microtasks, and replacement creates new node objects.
// These details expose stale async writes that auto-creating mock nodes hide.
function harness(handler) {
  const observers = [], listeners = new Map(), requests = [], timers = new Map();
  let timerId = 0;
  let body;
  const changed = (target) => {
    for (const observer of observers) {
      const scope = observer.target;
      let inScope = target === scope;
      if (observer.options.subtree) for (let node = target.parent; node; node = node.parent) inScope ||= node === scope;
      if (!inScope || observer.queued) continue;
      observer.queued = true;
      queueMicrotask(() => { observer.queued = false; observer.callback(); });
    }
  };
  class TextNode {
    constructor(value) { this.nodeType = 3; this.nodeName = '#text'; this.nodeValue = value; this.parent = null; }
    get textContent() { return this.nodeValue; }
    remove() { const parent = this.parent; if (parent) { parent._nodes.splice(parent._nodes.indexOf(this), 1); this.parent = null; changed(parent); } }
    replaceWith(node) { const parent = this.parent; if (parent) { parent.insertBefore(node, this); this.remove(); } }
  }
  class Element {
    constructor(tag, attrs = {}) {
      this.tagName = tag.toUpperCase(); this.attrs = attrs; this.parent = null; this._nodes = [];
      this.value = attrs.value || ''; this._html = ''; this.disabled = 'disabled' in attrs; this.hidden = 'hidden' in attrs; this.checked = 'checked' in attrs;
      this.dataset = {};
      for (const [key, value] of Object.entries(attrs)) if (key.startsWith('data-')) this.dataset[key.slice(5).replace(/-([a-z])/g, (_, char) => char.toUpperCase())] = value;
      this.classList = { contains: (name) => (attrs.class || '').split(/\s+/).includes(name) };
    }
    get selectedOptions() { return this.children.filter((row) => row.tagName === 'OPTION' && 'selected' in row.attrs); }
    get options() { return this.children.filter(row => row.tagName === 'OPTION'); }
    get selected() { return 'selected' in this.attrs; }
    set selected(value) { if (value) this.attrs.selected = ''; else delete this.attrs.selected; }
    get nodeType() { return 1; }
    get nodeName() { return this.tagName; }
    get children() { return this._nodes.filter(node => node.nodeType === 1); }
    get childNodes() { return this._nodes; }
    get textContent() { return this._nodes.map(node => node.textContent).join(''); }
    set textContent(value) { for (const node of this._nodes) node.parent = null; this._nodes = []; if (value) { const node = new TextNode(value); node.parent = this; this._nodes.push(node); } }
    get firstElementChild() { return this.children[0] || null; }
    get lastChild() { return this._nodes.at(-1) || null; }
    get content() { return this; }
    get attributes() { return Object.entries(this.attrs).map(([name, value]) => ({ name, value })); }
    get id() { return this.attrs.id || ''; }
    get childElementCount() { return this.children.length; }
    get isConnected() { for (let node = this; node; node = node.parent) if (node === body) return true; return false; }
    get innerHTML() { return this._html; }
    set innerHTML(html) {
      for (const node of this._nodes) node.parent = null;
      this._nodes = []; this._html = html; parse(html, this); changed(this);
    }
    querySelector(selector) {
      if (selector.includes(' ')) { const [head, ...tail] = selector.split(' '); return this.querySelector(head)?.querySelector(tail.join(' ')) || null; }
      for (const node of this.children) {
        if (matches(node, selector)) return node;
        const found = node.querySelector(selector); if (found) return found;
      }
      return null;
    }
    querySelectorAll(selector) {
      if (selector.includes(',')) return [...new Set(selector.split(',').flatMap(part => this.querySelectorAll(part)))];
      const matchesAll = [];
      for (const child of this.children) { if (matches(child, selector)) matchesAll.push(child); matchesAll.push(...child.querySelectorAll(selector)); }
      return matchesAll;
    }
    closest(selector) { for (let node = this; node; node = node.parent) if (matches(node, selector)) return node; return null; }
    getAttribute(name) { return this.attrs[name] ?? null; }
    hasAttribute(name) { return name in this.attrs; }
    setAttribute(name, value) { this.attrs[name] = value; if (name.startsWith('data-')) this.dataset[name.slice(5).replace(/-([a-z])/g, (_, char) => char.toUpperCase())] = value; if (name === 'disabled') this.disabled = true; }
    removeAttribute(name) { delete this.attrs[name]; if (name === 'disabled') this.disabled = false; }
    insertBefore(node, reference) {
      if (node === reference) return node;
      node.remove();
      const index = reference ? this._nodes.indexOf(reference) : this._nodes.length;
      this._nodes.splice(index, 0, node); node.parent = this; changed(this); return node;
    }
    appendChild(node) { return this.insertBefore(node, null); }
    replaceWith(node) { const parent = this.parent; if (parent) { parent.insertBefore(node, this); this.remove(); } }
    insertAdjacentHTML(position, html) {
      if (position === 'beforeend' || position === 'afterbegin') {
        const temporary = new Element('fragment'); parse(html, temporary);
        for (const node of temporary._nodes) node.parent = this;
        this._nodes = position === 'beforeend' ? this._nodes.concat(temporary._nodes) : temporary._nodes.concat(this._nodes);
        this._html = position === 'beforeend' ? this._html + html : html + this._html;
        changed(this); return;
      }
      assert.equal(position, 'beforebegin');
      const temporary = new Element('fragment'); parse(html, temporary);
      for (const node of temporary._nodes) node.parent = this.parent;
      this.parent._nodes.splice(this.parent._nodes.indexOf(this), 0, ...temporary._nodes); changed(this.parent);
    }
    remove() { const parent = this.parent; if (!parent) return; parent._nodes.splice(parent._nodes.indexOf(this), 1); this.parent = null; changed(parent); }
    cloneNode() { return new Element(this.tagName, { ...this.attrs }); }
    click() {}
    focus() { context.document.activeElement = this; }
    contains(target) { for (let node = target; node; node = node.parent) if (node === this) return true; return false; }
  }
  function matches(node, selector) {
    if (selector.startsWith('#')) return node.id === selector.slice(1);
    if (selector.startsWith('.')) return node.classList.contains(selector.slice(1));
    const attr = selector.match(/^\[([^=\]]+)(?:="([^"]*)")?\]$/);
    if (attr) return attr[2] == null ? attr[1] in node.attrs : node.attrs[attr[1]] === attr[2];
    return node.tagName === selector.toUpperCase();
  }
  function parse(html, parent) {
    const stack = [parent], tokens = html.match(/<[^>]*>|[^<]+/g) || [];
    for (const token of tokens) {
      if (token.startsWith('</')) { stack.pop(); continue; }
      if (!token.startsWith('<')) {
        const current = stack.at(-1), node = new TextNode(decode(token)); node.parent = current; current._nodes.push(node);
        if (current.tagName === 'TEXTAREA') current.value += decode(token);
        continue;
      }
      const tag = token.match(/^<([\w-]+)/)?.[1]; if (!tag) continue;
      const attrs = {};
      const attributes = token.slice(tag.length + 1, -1);
      for (const match of attributes.matchAll(/([\w-]+)(?:="([^"]*)")?/g)) attrs[match[1]] = decode(match[2] ?? '');
      const node = new Element(tag, attrs), owner = stack.at(-1); node.parent = owner; owner._nodes.push(node);
      if (!['input', 'br', 'hr', 'img', 'meta', 'link'].includes(tag)) stack.push(node);
    }
    const finalize = (node) => {
      for (const child of node.children) finalize(child);
      if (node.tagName === 'SELECT') node.value = (node.children.find((option) => 'selected' in option.attrs) || node.children[0])?.attrs.value || '';
    };
    finalize(parent);
  }
  body = new Element('body');
  const modalRoot = new Element('div', { id: 'modalRoot' }); modalRoot.parent = body; body._nodes.push(modalRoot);
  const context = vm.createContext({
    AbortController, crypto: crypto.webcrypto,
    api: async (path, payload, method, requestOptions) => {
      requests.push({ path, payload, requestOptions });
      return handler(path, payload, requestOptions);
    },
    $: (selector) => body.querySelector(selector), ACTIONS: {}, S: { route: 'other' }, icon: () => '', esc: escape,
    busy: async (button, action) => { const wasDisabled = button?.disabled; if (button) button.disabled = true; try { return await action(); } finally { if (button?.isConnected) button.disabled = wasDisabled; } },
    confirm: () => true, renderLocalCards() {}, toast() {}, setTimeout: fn => { const id = ++timerId; timers.set(id, fn); return id; }, clearTimeout: id => timers.delete(id), URL, URLSearchParams, TextEncoder,
    MutationObserver: class {
      constructor(callback) { this.callback = callback; this.queued = false; }
      observe(target, options) { this.target = target; this.options = options; observers.push(this); }
    },
    document: { body, createElement: tag => new Element(tag), addEventListener(event, fn) { if (!listeners.has(event)) listeners.set(event, []); listeners.get(event).push(fn); } },
  });
  for (const source of sources) vm.runInContext(source, context);
  context.launchAPI = (path, payload, progress, requestOptions) => context.api(path, payload, undefined, requestOptions);
  return {
    context, requests, timers, $: (selector) => body.querySelector(selector),
    run: (source) => vm.runInContext(source, context),
    close: async () => { modalRoot.innerHTML = ''; await settle(); },
    action: (name, applyOnly = false) => context.ACTIONS[name](body.querySelectorAll('[data-act="' + name + '"]').find(node => (node.dataset.applyOnly === 'true') === applyOnly)),
    emit: async (event, target) => { for (const fn of listeners.get(event) || []) await fn({ target }); await settle(); },
    key: async (key) => { const event = { key, preventDefault() {}, stopImmediatePropagation() { this.stopped = true; } }; for (const fn of listeners.get('keydown') || []) { await fn(event); if (event.stopped) break; } return event; },
    tick: async () => { const entry = timers.entries().next().value; if (entry) { timers.delete(entry[0]); await entry[1](); await settle(); } },
  };
}
const accounts = [
  { id: 'api-one', name: '日常开发' },
  { id: 'api-two', name: '备用 API' },
  { id: 'quota-only', name: '仅查额度', officialAccount: { mode: 'chatgpt_web_session' } },
  { id: 'library-only', name: '未关联', libraryOnly: true },
];
const client = { id: 'client-one', name: '我的工具', accountId: 'api-one', disabled: true, maxConcurrency: 7,
  modelAllowlist: ['model-a'], modelDenylist: ['model-b'], modelAllowPrefixes: ['allow-'], modelDenyPrefixes: ['deny-'],
  modelAliases: { friendly: 'model-a' }, modelPrefix: 'team/', accountIds: ['api-two'], imageAccountIds: ['api-one'],
  poolStrategy: 'single_account', poolAccountId: 'api-two' };
function fixture(clients = [], intercept = () => undefined) {
  let enabled = false;
  return async (path, payload) => {
    const value = intercept(path, payload);
    if (value !== undefined) return value;
    if (path === '/api/local/accounts') return { accounts: structuredClone(accounts) };
    if (path === '/api/local/codex-api') return { enabled, running: enabled, baseUrl: 'http://127.0.0.1:9999/codex-api/v1', clients: structuredClone(clients) };
    if (path === '/api/local/codex-api/enabled') { enabled = payload.enabled; return { ok: true }; }
    if (path === '/api/local/codex-api/images') return payload || { mode: 'disabled', maxConcurrentRequests: 1 };
    if (path === '/api/local/codex-api/network') return { revision: 'synthetic-revision', bindScope: 'loopback', ports: [], listeningPorts: 0, baseUrls: [], enabled, running: enabled };
    if (path === '/api/local/codex-api/clients') return payload?.suppressKey ? { ok: true, id: 'client-created' } : { id: 'client-created', key: 'synthetic-once-only-key' };
    if (path === '/api/local/codex-api/clients/rotate') return { id: payload.id, key: 'synthetic-rotated-key' };
    throw new Error('Unexpected synthetic request: ' + path);
  };
}
const plain = (value) => JSON.parse(JSON.stringify(value));
const clientWrites = (h) => h.requests.filter((row) => row.path.startsWith('/api/local/codex-api/clients') && row.payload);
async function open(h) { await h.context.ACTIONS.codexAPI(); await settle(); }
async function edit(h, id = 'client-one') { await h.context.ACTIONS.codexAPIEdit({ dataset: { id } }); await settle(); }

function installNavigation(h) {
  const rendered = [], closes = [], homeController = new AbortController();
  h.$('#modalRoot').insertAdjacentHTML('beforebegin', '<div id="view"></div><div id="headActions"></div><div id="pageIco"></div><div id="pageTitle"></div><div id="pageSub"></div>');
  Object.assign(h.context, {
    route() {}, location: { hash: '#accounts', pathname: '/', search: '' },
    history: { replaceState(_state, _title, url) { h.context.location.hash = url.includes('#') ? '#' + url.split('#')[1] : ''; } },
    $$: () => [], ROUTE_ICON: {}, PAGES: { accounts: {}, settings: {}, usage: {} },
    RENDER: Object.fromEntries(['accounts', 'settings', 'usage'].map(name => [name, async () => rendered.push(name)])),
    openPanel() {}, closePanel() {}, pageSkeleton: () => '', HOME: { controller: homeController },
    closeLocalEditor: options => { closes.push(options); return h.context.closeAPIManagement(); },
  });
  h.context.S.route = 'accounts';
  const start = shellSource.indexOf('route = async function () {');
  const end = shellSource.indexOf('/* ---------- loading:', start);
  h.run(shellSource.slice(shellSource.indexOf('function homeCancelLoad()'), shellSource.indexOf('async function homeLoadStats(')));
  h.run(shellSource.slice(start, end));
  return { rendered, closes, homeController };
}

test('back navigation protects client and settings drafts, cancel restores the current URL', async () => {
  for (const kind of ['client', 'settings']) {
    const h = harness(fixture([client]));
    if (kind === 'client') { await edit(h); h.$('#apiClientName').value = 'unsaved client'; }
    else { await open(h); h.$('#apiNetworkHost').value = '192.0.2.40'; }
    const nav = installNavigation(h), panel = h.$('#officialPanel');
    h.context.location.hash = '#settings';
    const navigation = h.context.route(); await settle();
    assert.ok(h.$('#apiUnsaved')); assert.equal(h.$('#officialPanel'), panel);
    await h.action('apiManagementStay'); await navigation;
    assert.equal(h.context.location.hash, '#accounts'); assert.deepEqual(nav.rendered, []);
    assert.equal(h.run('apiManagementHasDraft()'), true);
    assert.equal(nav.closes[0].returnToService, false);
    assert.equal(nav.homeController.signal.aborted, false);
    assert.equal(h.context.HOME.controller, nav.homeController);
  }
});

test('multiple navigation requests share one discard choice and only the latest destination renders', async () => {
  const h = harness(fixture([client])); await edit(h); h.$('#apiClientName').value = 'unsaved';
  const nav = installNavigation(h);
  h.context.location.hash = '#settings'; const first = h.context.route(); await settle();
  const notice = h.$('#apiUnsaved');
  h.context.location.hash = '#usage'; const latest = h.context.route(); await settle();
  assert.equal(h.$('#apiUnsaved'), notice);
  assert.equal(nav.homeController.signal.aborted, false);
  await h.action('apiManagementDiscard'); await Promise.all([first, latest]);
  assert.deepEqual(nav.rendered, ['usage']); assert.equal(h.context.S.route, 'usage');
  assert.equal(h.context.location.hash, '#usage'); assert.equal(h.$('#officialPanel'), null);
  assert.equal(nav.homeController.signal.aborted, true); assert.equal(h.context.HOME.controller, null);
});

test('service tabs retain settings drafts and closing offers inline stay or discard', async () => {
  const h = harness(fixture([client])); await open(h);
  h.run("selectAPIServiceView('settings')");
  const field = h.$('#apiNetworkHost'); field.value = '192.0.2.22';
  h.run("selectAPIServiceView('clients')");
  assert.equal(h.$('#apiService-settings').hidden, true);
  h.run("selectAPIServiceView('settings')");
  assert.equal(h.$('#apiNetworkHost'), field);
  assert.equal(field.value, '192.0.2.22');
  const staying = h.run('closeAPIManagement()'); await settle();
  assert.ok(h.$('#apiUnsaved')); assert.ok(h.$('#officialPanel'));
  await h.action('apiManagementStay'); assert.equal(await staying, false);
  assert.equal(h.run('apiManagementHasDraft()'), true);
  const leaving = h.run('closeAPIManagement()'); await settle();
  await h.action('apiManagementDiscard'); assert.equal(await leaving, true);
  assert.equal(h.$('#officialPanel'), null);
  assert.equal(clientWrites(h).length, 0);
});

test('rejected client saves keep the draft and allow a corrected retry without recreating a client', async () => {
  let writes = 0;
  const h = harness(fixture([client], (path, payload) => {
    if (path === '/api/local/codex-api/clients' && payload) {
      if (++writes === 1) throw Object.assign(new Error('invalid name'), { status:400 });
      return { ok:true, id:client.id };
    }
  }));
  await edit(h); const field = h.$('#apiClientName'); field.value = 'edited client';
  await h.action('codexAPISave');
  assert.equal(h.$('#apiClientName'), field); assert.equal(field.value, 'edited client');
  assert.match(h.$('#officialError').textContent, /输入已保留/);
  assert.equal(h.$('[data-act="codexAPISave"]').disabled, false);
  await h.action('codexAPISave');
  assert.equal(writes, 2);
  for (const write of clientWrites(h)) assert.equal(write.payload.id, client.id);
  assert.ok(h.$('#apiService-clients')); assert.equal(h.$('#apiService-clients').hidden, false);
});

test('lost client creation response preserves the draft but blocks blind resubmission', async () => {
  const h = harness(fixture([], (path, payload) => path === '/api/local/codex-api/clients' && payload ? Promise.reject(new Error('response lost')) : undefined));
  await h.context.ACTIONS.codexAPINew();
  h.$('#apiClientAccount').value = 'api-one'; h.$('#apiClientName').value = 'new client';
  await h.action('codexAPISave');
  assert.equal(h.$('#apiClientName').value, 'new client');
  assert.equal(h.$('[data-act="codexAPISave"]').disabled, true);
  await assert.rejects(h.action('codexAPISave'), /待确认/);
  assert.equal(clientWrites(h).length, 1);
});

test('pool checkboxes serialize explicit members in saved order and clear obsolete strategy', async () => {
  const h = harness(fixture([client])); await edit(h);
  const box = h.$('#apiClientPools').querySelector('input');
  // Existing text pool is api-two; adding api-one appends in the visible order.
  const choice = { dataset:{apiPool:'apiClientTextAccounts'}, value:'api-one', checked:true };
  await h.emit('change', choice);
  assert.deepEqual(plain(h.run('readAPIClientBinding().accountIds')), ['api-two','api-one']);
  choice.checked=false; await h.emit('change', choice); choice.value='api-two'; await h.emit('change', choice);
  const binding = plain(h.run('readAPIClientBinding()'));
  assert.deepEqual(binding.accountIds, []); assert.equal(binding.poolStrategy, ''); assert.equal(binding.poolAccountId, '');
  assert.ok(box);
});

test('a failed local state read cannot keep a green runtime or enabled start button', async () => {
  let fail=false;
  const h = harness(fixture([client], path => path === '/api/local/codex-api' && fail ? Promise.reject(new Error('local offline')) : undefined));
  await open(h); fail=true;
  await h.context.ACTIONS.codexAPIServiceRefresh({dataset:{}});
  assert.equal(h.$('#apiServiceRuntime .account-state').textContent, '状态待刷新');
  assert.equal(h.$('[data-act="codexAPIEnabled"]').disabled, true);
  assert.ok(h.$('#apiService-clients'));
});

test('request subview retains editor drafts in a single dialog and closes independently', async () => {
  const records = { checkedAt: Date.now(), accountNames: { anon: '实际账号' }, clientNames: { named: '客户端' }, records: [{ id: 'one', accountId: 'anon', clientId: 'named', model: 'model-a', state: 'completed', routingReason: 'failover', firstByteMs: 0, attempts: [{ accountId: 'anon', state: 'failed', reason: 'rate_limited', httpStatus: 429 }] }] };
  const h = harness(fixture([client], path => path.startsWith('/api/local/codex-api/requests?') ? records : undefined));
  await edit(h);
  const input = h.$('#apiClientName'), original = h.$('#officialPanel'); input.value = 'unsaved client name';
  await h.context.ACTIONS.codexAPIRequests({ dataset: { id: client.id } });
  assert.equal(h.$('#officialPanel'), original);
  assert.equal(input.value, 'unsaved client name');
  const summary = h.$('#apiRequestsList').querySelector('summary');
  assert.ok(summary);
  assert.equal(summary.closest('[data-act]').dataset.act, 'closeModal', 'only the normal modal backdrop is above the request details');
  const app = await readFile(new URL('../internal/console/web/app.js', import.meta.url), 'utf8');
  assert.match(app, /el\.dataset\.act === 'closeModal'.*e\.target !== el\) return/);
  assert.equal(h.$('#apiClientEditor').hidden, true);
  assert.equal(h.$('#apiRequestsOverlay').closest('[role="dialog"]'), h.$('#officialPanel').closest('[role="dialog"]'));
  assert.equal(h.$('#apiRequestsOverlay').getAttribute('data-act'), null);
  assert.match(h.$('#apiRequestsList').innerHTML, /实际账号|备用账号接替|上游限流|首个响应数据/);
  assert.doesNotMatch(h.$('#apiRequestsList').innerHTML, /failover|rate_limited/);
  await h.emit('click', summary);
  assert.ok(h.$('#apiRequestsOverlay'), 'click inside the dialog must not close it');
  await h.key('Escape');
  assert.equal(h.$('#apiRequestsOverlay'), null);
  assert.equal(h.$('#officialPanel'), original);
  assert.equal(input.value, 'unsaved client name');
  assert.equal(h.$('#apiClientEditor').hidden, false);
  assert.match(h.requests.find(row => row.path.includes('/requests?')).path, /clientId=client-one/);
});

test('late request reads cannot replace a newer scope and unknown diagnostic fields stay unknown', async () => {
  const old = deferred(); let reads = 0;
  const h = harness(fixture([client], path => path.startsWith('/api/local/codex-api/requests?') ? ++reads === 1 ? old.promise : { records: [{ id: 'new', model: 'new-model', routingReason: 'untrusted_enum', failureStage: 'another_enum' }], checkedAt: Date.now() } : undefined));
  const opening = h.context.ACTIONS.codexAPIRequests({ dataset: { id: 'old' } }); await settle();
  await h.context.ACTIONS.codexAPIRequests({ dataset: { id: 'new' } });
  const rendered = h.$('#apiRequestsList').innerHTML;
  old.resolve({ records: [{ model: 'stale-model' }] }); await opening;
  assert.equal(h.$('#apiRequestsList').innerHTML, rendered);
  assert.match(rendered, /new-model|未记录/);
  assert.doesNotMatch(rendered, /untrusted_enum|another_enum|stale-model/);
  await h.action('codexAPIRequestsClose');
  assert.equal(h.$('#apiRequestsOverlay'), null);
});

test('visible observation updates only status mounts and stops after close or visibility change', async () => {
  let reads = 0;
  const h = harness(fixture([client], path => path === '/api/local/codex-api' ? { enabled: true, running: true, clients: [client], statistics: [{ clientId: client.id, inflight: ++reads }], baseUrl: 'http://127.0.0.1/api' } : undefined));
  await open(h);
  const image = h.$('#apiImageConcurrency'), network = h.$('#apiNetworkHost'); image.value = '9'; network.value = '192.0.2.15';
  const form = h.$('#apiServiceAdvanced');
  await h.tick();
  assert.ok(reads >= 2);
  assert.equal(h.$('#apiServiceAdvanced'), form);
  assert.ok(h.$('#apiServiceSummary'), 'polling must retain the stable summary mount');
  assert.equal(image.value, '9'); assert.equal(network.value, '192.0.2.15');
  assert.match(h.$('#apiServiceUpdated').textContent, /更新于/);
  h.context.document.hidden = true; await h.emit('visibilitychange', {});
  const before = reads; await h.tick(); assert.equal(reads, before);
  h.context.document.hidden = false; await h.emit('visibilitychange', {});
  h.context.closeOfficialPanel(); await h.tick(); assert.equal(reads, before);
});

test('automatic observation preserves open client actions and focused controls while publishing updated usage', async () => {
  let reads = 0;
  const h = harness(fixture([client], path => path === '/api/local/codex-api' ? { enabled: true, running: true, clients: [client], statistics: [{ clientId: client.id, inflight: ++reads }] } : undefined));
  await open(h);
  const button = h.$('[data-act="codexAPIRequests"]'), more = button.closest('details');
  more.setAttribute('open', ''); button.focus();
  const runtime = h.$('#apiServiceRuntime');
  await h.tick();
  assert.ok(reads >= 2); assert.equal(h.$('[data-act="codexAPIRequests"]'), button);
  assert.equal(button.isConnected, true); assert.equal(h.context.document.activeElement, button);
  assert.equal(more.hasAttribute('open'), true); assert.equal(h.$('#apiServiceRuntime'), runtime);
  assert.match(h.$('#apiServiceClients').textContent, /2 在途/);
});

test('returning to a visible page while a status read is pending resumes observation', async () => {
  const pending = deferred(); let reads = 0;
  const h = harness(fixture([client], path => path === '/api/local/codex-api' && ++reads === 2 ? pending.promise : undefined));
  await open(h);
  const refreshing = h.tick(); await settle();
  h.context.document.hidden = true; await h.emit('visibilitychange', {});
  h.context.document.hidden = false; await h.emit('visibilitychange', {});
  pending.resolve({ clients: [client], running: true, enabled: true });
  await refreshing;
  assert.ok(h.timers.size > 0, 'visibility return must retain a follow-up poll');
  const before = reads; await h.tick();
  assert.ok(reads > before);
});

test('client quick toggle posts only identity, intent and revision, preserving edited pool policies', async () => {
  let row = { ...client, revision: 'client-rev-1' };
  const h = harness(fixture([client], (path, payload) => {
    if (path === '/api/local/codex-api') return { clients: [row], running: true, enabled: true };
    if (path === '/api/local/codex-api/clients/enabled') { assert.deepEqual(plain(payload), { id: row.id, enabled: true, expectedRevision: row.revision }); row = { ...row, disabled: false }; return { ok: true, id: row.id, enabled: true }; }
  }));
  await open(h); await h.action('codexAPIClientEnabled');
  assert.equal(h.requests.filter(row => row.path === '/api/local/codex-api/clients').length, 0);
  assert.equal(h.run('apiClientPanel.clients[0].disabled'), false);
  assert.deepEqual(plain(h.run('apiClientPanel.clients[0].accountIds')), ['api-two']);
});

test('client actions preserve their nodes and stay locked until network and image reconciliation finishes', async () => {
  const network = deferred(), images = deferred();
  let networkReads = 0, imageReads = 0, writes = 0, row = { ...client, disabled: false, revision: 'rev-0' };
  const h = harness(fixture([row], (path, payload) => {
    if (path === '/api/local/codex-api') return { clients: [{ ...row }], running: true, enabled: true };
    if (path === '/api/local/codex-api/clients/enabled') {
      assert.equal(payload.expectedRevision, row.revision);
      writes++; row = { ...row, disabled: !payload.enabled, revision: `rev-${writes}` };
      return { ok: true, id: row.id, enabled: payload.enabled };
    }
    if (path === '/api/local/codex-api/network' && ++networkReads === 2) return network.promise;
    if (path === '/api/local/codex-api/images' && ++imageReads === 2) return images.promise;
  }));
  await open(h);
  const oldButton = h.$('[data-act="codexAPIClientEnabled"]');
  const disabling = h.action('codexAPIClientEnabled'); await settle();
  assert.equal(h.run('apiClientPanel.clients[0].disabled'), true, 'new service state arrives before auxiliary reads finish');
  const rebuilt = h.$('[data-act="codexAPIClientEnabled"]');
  assert.equal(rebuilt, oldButton);
  assert.equal(rebuilt.dataset.enabled, 'true');
  const modifying = ['codexAPIClientEnabled', 'codexAPIEdit', 'codexAPIRotate', 'codexAPIRevoke', 'codexAPINew'];
  for (const action of modifying) assert.equal(h.$(`[data-act="${action}"]`).disabled, true, `${action} must remain visibly locked`);
  assert.equal(h.$('[data-act="codexAPIRequests"]').disabled, false);
  network.resolve({ bindScope: 'loopback', listeningPorts: 0 }); await settle();
  assert.equal(rebuilt.disabled, true, 'one completed auxiliary read must not unlock writes');
  images.resolve({ mode: 'disabled', maxConcurrentRequests: 1 }); await disabling;
  for (const action of modifying) assert.equal(h.$(`[data-act="${action}"]`).disabled, false, `${action} must recover after full reconciliation`);
  await h.action('codexAPIClientEnabled');
  assert.equal(writes, 2); assert.equal(h.run('apiClientPanel.clients[0].disabled'), false);
  assert.equal(h.$('[data-act="codexAPIClientEnabled"]').disabled, false);
});

test('readiness renders every member and repairs the real credential without losing rule-check inputs', async () => {
  const readiness = { serving: true, readyClients: 0, routes: [], clients: [{ id: client.id, name: client.name, state: 'pool_members_unavailable', members: [
    { id: 'local-one', name: 'Needs refresh', credentialRef: 'official-one', state: 'credential_refresh_required' },
    { id: 'local-two', name: 'Needs login', credentialRef: 'official-two', state: 'credential_login_required' },
    { id: 'local-api', name: 'API only', state: 'account_missing' },
  ] }] };
  const h = harness(fixture([client], path => path === '/api/local/codex-api/readiness' ? readiness : undefined));
  const repairs = []; h.context.ACTIONS.officialRefresh = async button => repairs.push(button.dataset.id);
  await h.context.ACTIONS.codexAPIReadiness();
  assert.match(h.$('#apiReadinessRows').innerHTML, /Needs refresh/);
  assert.match(h.$('#apiReadinessRows').innerHTML, /Needs login/);
  assert.match(h.$('#apiReadinessRows').innerHTML, /API only/);
  const key = h.$('#apiReadinessKey'); key.value = 'typed-key';
  await h.context.ACTIONS.codexAPIReadinessRepair({ dataset: { key: 'client-one:local-one', kind: 'refresh' } });
  assert.deepEqual(repairs, ['official-one']);
  assert.equal(h.$('#apiReadinessKey'), key); assert.equal(key.value, 'typed-key');
  assert.equal(h.run('apiReadinessPanel.repairs.has("client-one:local-api")'), false);
  await h.context.ACTIONS.codexAPIReadinessRepair({ dataset: { key: 'client-one:local-two', kind: 'login' } });
  assert.ok(h.$('[data-act="officialLogin"]'));
  assert.equal(h.$('[data-act="officialLogin"]').dataset.id, 'official-two');
});

test('opening and enabling never create a key; settings have a separate tab and diagnosis remains accessible', async () => {
  const h = harness(fixture()); await open(h);
  assert.equal(h.$('#apiClientName'), null);
  assert.equal(h.$('#apiClientModelPrefix'), null);
  assert.equal(h.$('#apiClientAccount').value, '');
  const advanced = h.$('#apiServiceAdvanced');
  assert.equal(advanced.closest('#apiService-settings').hidden, true);
  for (const selector of ['#apiNetworkSettings', '#apiImageSettings']) assert.ok(advanced.querySelector(selector), selector);
  assert.ok(h.$('[data-act="codexAPIReadiness"]'));
  assert.equal(h.$('#apiClientAccount').children.length, 3, 'only callable accounts plus the placeholder');
  await h.action('codexAPIEnabled'); await settle();
  assert.equal(clientWrites(h).length, 0);
  assert.equal(h.$('#apiServiceRuntime .account-state').textContent, '运行中');
});

test('first setup requires an explicit account and never exposes a manual key', async () => {
  const h = harness(fixture()); await open(h);
  await assert.rejects(h.action('codexAPICreate'), /请选择/); assert.equal(clientWrites(h).length, 0);
  h.$('#apiClientAccount').value = 'api-two';
  await h.action('codexAPICreate'); await settle();
  assert.deepEqual(plain(clientWrites(h)[0].payload), { name: '本机客户端', accountId: 'api-two', onlyIfEmpty: true, suppressKey: true });
  assert.equal(h.$('#apiCreatedKey'), null);
  assert.doesNotMatch(h.$('#modalRoot').innerHTML, /synthetic-once-only-key/);
});

test('existing clients are shown once with their pools and direct management without exposing keys', async () => {
  const h = harness(fixture([client])); await open(h);
  assert.equal(h.$('[data-act="codexAPICreate"]'), null);
  assert.equal(h.$('#apiClientAccount'), null);
  assert.equal(h.$('#apiServiceContent').querySelector('code').textContent.startsWith('http'), true);
  assert.ok(h.$('#apiServiceClients').querySelector('[data-act="codexAPIEdit"]'));
  assert.ok(h.$('[data-act="codexAPINew"]'));
  assert.equal(h.$('[data-act="copyVal"]').closest('#apiServiceAdvanced'), null);
  assert.equal(h.$('[data-act="codexAPIRotate"]').closest('#apiServiceClients'), h.$('#apiServiceClients'));
  assert.equal(h.$('#apiServiceClients').querySelectorAll('.api-client-row').length, 1);
  assert.match(h.$('#apiServiceContent').innerHTML, /文字池 · 1 个 · 固定成员/);
  assert.match(h.$('#apiServiceContent').innerHTML, /图片池 · 1 个/);
  assert.equal(h.$('#apiCreatedKey'), null);
  assert.equal(clientWrites(h).length, 0);
  await assert.rejects(h.context.ACTIONS.codexAPICreate({ dataset: {} }), /已有密钥/);
});

test('collapsed editor preserves every configured policy without opening advanced', async () => {
  const h = harness(fixture([client], (path, payload) => path.endsWith('/clients') && payload ? { id: 'client-one' } : undefined));
  await edit(h);
  assert.equal(h.$('#apiClientAllowlist').closest('details').getAttribute('open'), null);
  assert.equal(h.$('#apiServiceContent'), null);
  assert.equal(h.$('#apiNetworkSettings'), null);
  assert.equal(h.$('#apiImageSettings'), null);
  assert.equal(h.requests.some((row) => /\/images$|\/network$/.test(row.path)), false);
  h.$('#apiClientName').value = '已改名称';
  await h.action('codexAPISave'); await settle();
  assert.deepEqual(plain(clientWrites(h)[0].payload), { ...client, name: '已改名称' });
});

test('missing binding does not silently pick the first account and missing fixed pool remains visible', async () => {
  const h = harness(fixture([{ ...client, accountId: 'removed', accountIds: ['removed-pool'], poolAccountId: 'removed-pool' }]));
  await edit(h);
  assert.equal(h.$('#apiClientAccount').value, '');
  assert.equal(h.$('#apiClientPoolAccount').value, 'removed-pool');
  assert.deepEqual(h.$('#apiClientTextAccounts').selectedOptions.map((row) => row.value), ['removed-pool']);
  await assert.rejects(h.action('codexAPISave'), /请选择/);
  assert.equal(clientWrites(h).length, 0);
});

test('collapsed save preserves explicit pool order independently of vault ordering', async () => {
  const ordered = { ...client, accountIds: ['api-two', 'api-one'], imageAccountIds: ['api-two', 'api-one'], poolStrategy: 'custom', poolAccountId: '' };
  const h = harness(fixture([ordered], (path, payload) => path.endsWith('/clients') && payload ? { id: client.id } : undefined));
  await edit(h); await h.action('codexAPISave');
  const body = plain(clientWrites(h)[0].payload);
  assert.deepEqual(body.accountIds, ordered.accountIds);
  assert.deepEqual(body.imageAccountIds, ordered.imageAccountIds);
  assert.equal(body.poolStrategy, 'custom');
});

test('empty account catalog disables creation and rejects fabricated selection', async () => {
  const h = harness(fixture([], (path) => path === '/api/local/accounts' ? { accounts: [] } : undefined)); await open(h);
  assert.equal(h.$('[data-act="codexAPICreate"]').disabled, true);
  h.$('#apiClientAccount').value = 'not-real';
  await assert.rejects(h.action('codexAPICreate'), /请选择/);
  assert.equal(clientWrites(h).length, 0);
});

test('default creation rechecks deleted accounts and newly existing clients before POST', async () => {
  for (const scenario of ['deleted', 'existing']) {
    let accountsRead = 0, statusRead = 0;
    const h = harness(fixture([], (path) => {
      if (path === '/api/local/accounts' && ++accountsRead > 1 && scenario === 'deleted') return { accounts: [] };
      if (path === '/api/local/codex-api' && ++statusRead > 1 && scenario === 'existing') return { clients: [client] };
    }));
    await open(h); h.$('#apiClientAccount').value = 'api-one';
    await assert.rejects(h.action('codexAPICreate'), /不可用|已有密钥/);
    assert.equal(clientWrites(h).length, 0);
  }
});

test('duplicate creation and other writes share one gate while POST is pending', async () => {
  const pending = deferred();
  const h = harness(fixture([], (path, payload) => path.endsWith('/clients') && payload ? pending.promise : undefined));
  await open(h); h.$('#apiClientAccount').value = 'api-one';
  const creating = h.action('codexAPICreate'); await settle();
  await h.action('codexAPICreate');
  await h.context.ACTIONS.codexAPIRotate({ dataset: { id: 'client-one' } });
  assert.equal(clientWrites(h).length, 1);
  pending.resolve({ ok: true, id: 'synthetic-created' }); await creating;
  assert.equal(h.$('#apiCreatedKey'), null);
});

test('atomic rotate posts only id and does not save an unexpanded or changed editor', async () => {
  const h = harness(fixture([client])); await edit(h);
  h.$('#apiClientName').value = 'unsaved'; h.$('#apiClientConcurrency').value = 'invalid';
  await h.context.ACTIONS.codexAPIRotate({ dataset: { id: client.id } });
  assert.equal(clientWrites(h)[0].path, '/api/local/codex-api/clients/rotate');
  assert.deepEqual(plain(clientWrites(h)[0].payload), { id: 'client-one' });
  assert.equal(h.$('#apiCreatedKey').value, 'synthetic-rotated-key');
});

test('declining rotation performs no request and leaves the panel editable', async () => {
  const h = harness(fixture([client])); await open(h); h.context.confirm = () => false;
  await h.action('codexAPIRotate'); assert.equal(clientWrites(h).length, 0);
  h.context.confirm = () => true; await h.action('codexAPIRotate'); assert.equal(clientWrites(h).length, 1);
});

test('unknown create or rotate result cannot be blindly repeated in the same panel', async () => {
  for (const rotating of [false, true]) {
    const h = harness(fixture(rotating ? [client] : [], (path, payload) => path.startsWith('/api/local/codex-api/clients') && payload ? Promise.reject(new Error('response lost')) : undefined));
    await open(h); if (!rotating) h.$('#apiClientAccount').value = 'api-one';
    const action = rotating ? 'codexAPIRotate' : 'codexAPICreate';
    await assert.rejects(h.action(action), /response lost/);
    await assert.rejects(h.action(action), /结果待确认/);
    assert.equal(clientWrites(h).length, 1);
  }
});

test('late rotate key after closure or modal replacement is never exposed or reopened', async () => {
  for (const replace of [false, true]) {
    const wait = deferred(), h = harness(fixture([client], (path) => path.endsWith('/rotate') ? wait.promise : undefined));
    await open(h); const rotating = h.action('codexAPIRotate'); await settle();
    await h.close(); if (replace) h.run("officialPanel('另一个窗口', '<p>保持当前内容</p>')");
    wait.resolve({ key: 'synthetic-late-secret' }); await rotating; await settle();
    assert.equal(h.$('#apiCreatedKey'), null);
    assert.doesNotMatch(h.$('#modalRoot').innerHTML, /synthetic-late-secret|客户端密钥/);
  }
});

test('closing during default preflight prevents creation and late reads do not repaint replacement', async () => {
  const wait = deferred(); let reads = 0;
  const h = harness(fixture([], (path) => path === '/api/local/codex-api' && ++reads > 1 ? wait.promise : undefined));
  await open(h); h.$('#apiClientAccount').value = 'api-one'; const creating = h.action('codexAPICreate'); await settle();
  await h.close(); h.run("officialPanel('替换窗口', '<p>不改变</p>')");
  wait.resolve({ clients: [] }); await creating; await settle();
  assert.equal(clientWrites(h).length, 0); assert.equal(h.$('#apiCreatedKey'), null);
});

test('explicit advanced additional key creation preserves policies and omits first-only restriction', async () => {
  const h = harness(fixture([client])); await open(h);
  await h.action('codexAPINew'); await settle();
  assert.equal(h.$('#apiClientName').value, '本机客户端');
  h.$('#apiClientAccount').value = 'api-two';
  h.$('#apiClientModelPrefix').value = 'tools/';
  await h.action('codexAPISave');
  const body = clientWrites(h)[0].payload;
  assert.equal(body.accountId, 'api-two'); assert.equal(body.modelPrefix, 'tools/');
  assert.equal(body.onlyIfEmpty, undefined); assert.equal(body.rotate, undefined);
});

test('network save refreshes the main service status while its form remains in advanced', async () => {
  const h = harness(fixture([], (path, payload) => path.endsWith('/network') && payload ? { revision: 'next', enabled: true, running: true, listeningPorts: 1, baseUrls: ['http://127.0.0.1:1234/v1'] } : undefined));
  await open(h); h.$('#apiNetworkPorts').value = '1234';
  await h.action('codexAPINetworkSave');
  assert.equal(h.$('#apiServiceRuntime .account-state').textContent, '运行中');
  assert.equal(h.$('[data-act="codexAPIEnabled"]').dataset.enabled, 'false');
  assert.equal(h.$('#apiNetworkSettings').closest('#apiService-settings').hidden, true);
});

const launchRevision = 'mla-v1-' + 'a'.repeat(64);
function launchFixture(intercept = () => undefined) {
  return fixture([{ ...client, disabled: false }], (path, payload) => {
    const custom = intercept(path, payload); if (custom !== undefined) return custom;
    if (path === '/api/local/codex-api') return { running: true, enabled: true, clients: [{ ...client, disabled: false }] };
    if (path.endsWith('/launch/preview')) return { ready: true, clientId: payload.clientId, target: payload.target, revision: launchRevision, clientName: client.name, models: ['gpt-fixture'], catalogOverride: true, requiresModeChange: false };
    if (path.endsWith('/launch/safe-v1')) return { ok: true, protocol: payload.protocol, operationId: payload.operationId };
  });
}
test('managed launch keeps automatic credentials out of UI and consumes one confirmed preview', async () => {
  const pending = deferred(), h = harness(launchFixture((path) => path.endsWith('/launch/safe-v1') ? pending.promise : undefined));
  await h.context.ACTIONS.codexAPIServiceOpen({ dataset: { target: 'codex-desktop' } });
  assert.equal(h.$('#apiServiceLaunchReady'), null);
  h.$('#apiServiceLaunchOverride').checked = true;
  const launching = h.action('codexAPIServiceLaunch'); await settle();
  await h.action('codexAPIServiceLaunch');
  const calls = h.requests.filter((r) => r.path.endsWith('/launch/safe-v1'));
  assert.equal(calls.length, 1);
  assert.deepEqual(plain(calls[0].payload), { protocol: 'safe-agent-change.v1', operationId: calls[0].payload.operationId, clientId: client.id, target: 'codex-desktop', revision: launchRevision, catalogOverride: true, allowModeChange: true, applyOnly: false });
  assert.match(calls[0].payload.operationId, /^[a-f0-9-]{36}$/);
  assert.equal(h.$('#apiCreatedKey'), null);
  pending.resolve({ ok: true, protocol: calls[0].payload.protocol, operationId: calls[0].payload.operationId }); await launching;
});
test('late managed launch preview cannot populate a replacement dialog', async () => {
  const wait = deferred(), h = harness(launchFixture((path) => path.endsWith('/preview') ? wait.promise : undefined));
  const opening = h.context.ACTIONS.codexAPIServiceOpen({ dataset: { target: 'codex' } }); await settle();
  await h.close(); h.run("officialPanel('保留窗口', '<p>保留内容</p>')");
  wait.resolve({ ready: true, clientId: client.id, target: 'codex', revision: launchRevision, clientName: client.name, models: ['gpt-fixture'], catalogOverride: true, requiresModeChange: false });
  await opening;
  assert.equal(h.$('#apiServiceLaunchReady'), null);
  assert.match(h.$('#modalRoot').innerHTML, /保留内容/);
});
test('ambiguous native launch response cannot blindly restart the tool again', async () => {
  const h = harness(launchFixture((path) => path.endsWith('/launch/safe-v1') ? Promise.reject(Error('response lost')) : undefined));
  await h.context.ACTIONS.codexAPIServiceOpen({ dataset: { target: 'codex' } });
  await h.action('codexAPIServiceLaunch'); await h.action('codexAPIServiceLaunch');
  assert.equal(h.requests.filter((r) => r.path.endsWith('/launch/safe-v1')).length, 1);
  assert.match(h.$('#apiServiceLaunchContent').innerHTML, /response lost/);
});

test('official local API service only advertises and admits Codex launch targets', async () => {
  const h = harness(launchFixture());
  const html = h.run("localAPIServiceLaunchHTML({running:true,clients:[{id:'synthetic-client',disabled:false}]})");
  assert.match(html, /data-target="codex-desktop"/);
  assert.match(html, /data-target="codex"/);
  assert.doesNotMatch(html, /claude|Claude/);
  for (const target of ['claude', 'claude-desktop', 'unknown']) {
    await assert.rejects(h.context.ACTIONS.codexAPIServiceOpen({ dataset: { target } }), /不支持/);
  }
  assert.equal(h.requests.length, 0);
  await h.context.ACTIONS.codexAPIServiceOpen({ dataset: { target: 'codex' } });
  h.run("apiServiceLaunchPanel.target='claude'");
  await assert.rejects(h.action('codexAPIServiceLaunch'), /不支持/);
  assert.equal(h.requests.filter((r) => r.path.endsWith('/launch/safe-v1')).length, 0);
});

test('service apply-only uses the same safe transaction and never requests a launch', async () => {
  const h=harness(launchFixture());
  await h.context.ACTIONS.codexAPIServiceOpen({dataset:{target:'codex'}});
  assert.equal(h.$('#apiServiceLaunchReady'),null);assert.equal(h.$('#apiServiceLaunchMode'),null);
  await h.action('codexAPIServiceLaunch',true);
  const writes=h.requests.filter(row=>row.path.endsWith('/launch/safe-v1'));
  assert.equal(writes.length,1);assert.equal(writes[0].payload.applyOnly,true);
  assert.equal(writes[0].payload.protocol,'safe-agent-change.v1');assert.equal('confirmed' in writes[0].payload,false);
});

test('service stopping progress stays in its panel and both actions remain disabled', async () => {
  const h=harness(launchFixture()),wait=deferred();let body;
  await h.context.ACTIONS.codexAPIServiceOpen({dataset:{target:'codex'}});
  const original=h.$('#officialPanel');
  h.context.launchAPI=async(_path,payload,progress)=>{body=payload;progress.event({type:'phase',step:'stop'});await wait.promise;return {ok:true,protocol:payload.protocol,operationId:payload.operationId};};
  const pending=h.action('codexAPIServiceLaunch');await settle();
  assert.equal(h.$('#officialPanel'),original);assert.match(h.$('#apiServiceLaunchProgress').textContent,/正在停止/);
  const buttons=h.$('#apiServiceLaunchContent').querySelectorAll('[data-act="codexAPIServiceLaunch"]');
  assert.equal(buttons.length,2);assert.ok(buttons.every(button=>button.disabled));
  await h.action('codexAPIServiceLaunch',true);assert.equal(body.applyOnly,false);
  wait.resolve();await pending;
});

test('closing service selection aborts during stop and late errors cannot reopen it', async () => {
  const h=harness(launchFixture()),wait=deferred();let submitted=0,signal,applied=false;
  await h.context.ACTIONS.codexAPIServiceOpen({dataset:{target:'codex'}});
  h.context.launchAPI=async(_path,payload,_progress,options)=>{submitted++;signal=options.signal;await wait.promise;if(signal.aborted)throw Object.assign(Error('cancelled'),{cancelled:true});applied=true;return {ok:true,protocol:payload.protocol,operationId:payload.operationId};};
  const pending=h.action('codexAPIServiceLaunch');await settle();h.run('closeOfficialPanel()');wait.resolve();await pending;
  assert.equal(signal.aborted,true);assert.equal(applied,false);assert.equal(submitted,1);assert.equal(h.$('#officialPanel'),null);
});

test('service receipt mismatch blocks blind replay and retains chosen override', async () => {
  const h=harness(launchFixture((path)=>path.endsWith('/launch/safe-v1')?{ok:true,protocol:'older',operationId:'wrong'}:undefined));
  await h.context.ACTIONS.codexAPIServiceOpen({dataset:{target:'codex'}});
  h.$('#apiServiceLaunchOverride').checked=false;await h.action('codexAPIServiceLaunch');await h.action('codexAPIServiceLaunch');
  assert.equal(h.requests.filter(row=>row.path.endsWith('/launch/safe-v1')).length,1);
  assert.equal(h.$('#apiServiceLaunchOverride').checked,false);assert.match(h.$('#apiServiceLaunchContent').innerHTML,/操作结果未确认/);
  assert.ok(h.$('#apiServiceLaunchContent').querySelectorAll('[data-act="codexAPIServiceLaunch"]').every(button=>button.disabled));
});

test('service matching receipt with native submitted uncertainty remains locked', async () => {
  const h=harness(launchFixture((path,payload)=>path.endsWith('/launch/safe-v1')?{ok:true,protocol:payload.protocol,operationId:payload.operationId,result:{submitted:true,uncertain:true}}:undefined));
  await h.context.ACTIONS.codexAPIServiceOpen({dataset:{target:'codex'}});
  await h.action('codexAPIServiceLaunch');await h.action('codexAPIServiceLaunch');
  assert.equal(h.requests.filter(row=>row.path.endsWith('/launch/safe-v1')).length,1);
  assert.match(h.$('#apiServiceLaunchContent').innerHTML,/配置\/启动已提交/);
  assert.ok(h.$('#apiServiceLaunchContent').querySelectorAll('[data-act="codexAPIServiceLaunch"]').every(button=>button.disabled));
});

