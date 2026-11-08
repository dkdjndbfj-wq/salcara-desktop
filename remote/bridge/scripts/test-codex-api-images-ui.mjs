import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

const sources = await Promise.all(['codex-accounts.js', 'codex-api-images.js'].map((name) => readFile(new URL('../internal/console/web/' + name, import.meta.url), 'utf8')));
const css = await readFile(new URL('../internal/console/web/app.css', import.meta.url), 'utf8');
const index = await readFile(new URL('../internal/console/web/index.html', import.meta.url), 'utf8');
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
  const observers = [], listeners = new Map(), requests = [];
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
  class Element {
    constructor(tag, attrs = {}) {
      this.tagName = tag.toUpperCase(); this.attrs = attrs; this.parent = null; this.children = [];
      this.value = attrs.value || ''; this.textContent = ''; this._html = ''; this.disabled = 'disabled' in attrs; this.hidden = 'hidden' in attrs;
      this.dataset = {};
      for (const [key, value] of Object.entries(attrs)) if (key.startsWith('data-')) this.dataset[key.slice(5).replace(/-([a-z])/g, (_, char) => char.toUpperCase())] = value;
      this.classList = { contains: (name) => (attrs.class || '').split(/\s+/).includes(name) };
    }
    get id() { return this.attrs.id || ''; }
    get childElementCount() { return this.children.length; }
    get isConnected() { for (let node = this; node; node = node.parent) if (node === body) return true; return false; }
    get innerHTML() { return this._html; }
    set innerHTML(html) {
      for (const node of this.children) node.parent = null;
      this.children = []; this._html = html; parse(html, this); changed(this);
    }
    querySelector(selector) {
      for (const node of this.children) {
        if (matches(node, selector)) return node;
        const found = node.querySelector(selector); if (found) return found;
      }
      return null;
    }
    closest(selector) { for (let node = this; node; node = node.parent) if (matches(node, selector)) return node; return null; }
    getAttribute(name) { return this.attrs[name] ?? null; }
    insertAdjacentHTML(position, html) {
      if (position === 'beforeend' || position === 'afterbegin') {
        const temporary = new Element('fragment'); parse(html, temporary);
        for (const node of temporary.children) node.parent = this;
        this.children = position === 'beforeend' ? this.children.concat(temporary.children) : temporary.children.concat(this.children);
        this._html = position === 'beforeend' ? this._html + html : html + this._html;
        changed(this); return;
      }
      assert.equal(position, 'beforebegin');
      const temporary = new Element('fragment'); parse(html, temporary);
      for (const node of temporary.children) node.parent = this.parent;
      this.parent.children.splice(this.parent.children.indexOf(this), 0, ...temporary.children); changed(this.parent);
    }
    remove() { const parent = this.parent; if (!parent) return; parent.children.splice(parent.children.indexOf(this), 1); this.parent = null; changed(parent); }
    click() {}
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
        const current = stack.at(-1); current.textContent += decode(token);
        if (current.tagName === 'TEXTAREA' || current.tagName === 'OPTION') current.value += decode(token);
        continue;
      }
      const tag = token.match(/^<([\w-]+)/)?.[1]; if (!tag) continue;
      const attrs = {};
      const attributes = token.slice(tag.length + 1, -1);
      for (const match of attributes.matchAll(/([\w-]+)(?:="([^"]*)")?/g)) attrs[match[1]] = decode(match[2] ?? '');
      const node = new Element(tag, attrs), owner = stack.at(-1); node.parent = owner; owner.children.push(node);
      if (!['input', 'br', 'hr', 'img', 'meta', 'link'].includes(tag)) stack.push(node);
    }
    const finalize = (node) => {
      for (const child of node.children) finalize(child);
      if (node.tagName === 'SELECT') node.value = (node.children.find((option) => 'selected' in option.attrs) || node.children[0])?.attrs.value || '';
    };
    finalize(parent);
  }
  body = new Element('body');
  const modalRoot = new Element('div', { id: 'modalRoot' }); modalRoot.parent = body; body.children.push(modalRoot);
  const context = vm.createContext({
    api: async (path, payload) => {
      requests.push({ path, payload });
      return handler(path, payload);
    },
    $: (selector) => body.querySelector(selector), ACTIONS: {}, S: { route: 'other' }, icon: () => '', esc: escape,
    busy: async (button, action) => { if (button) button.disabled = true; try { return await action(); } finally { if (button?.isConnected) button.disabled = false; } },
    confirm: () => true, renderLocalCards() {}, toast() {}, setTimeout: () => 1, clearTimeout() {}, URL, TextEncoder,
    MutationObserver: class {
      constructor(callback) { this.callback = callback; this.queued = false; }
      observe(target, options) { this.target = target; this.options = options; observers.push(this); }
    },
    document: { body, addEventListener(event, fn) { if (!listeners.has(event)) listeners.set(event, []); listeners.get(event).push(fn); } },
  });
  for (const source of sources) vm.runInContext(source, context);
  return {
    context, requests, $: (selector) => body.querySelector(selector),
    run: (source) => vm.runInContext(source, context),
    close: async () => { modalRoot.innerHTML = ''; await settle(); },
    action: (name) => context.ACTIONS[name](body.querySelector('[data-act="' + name + '"]')),
    emit: async (event, target) => { for (const fn of listeners.get(event) || []) await fn({ target }); await settle(); },
  };
}
const apiState = { enabled: true, running: true, baseUrl: 'http://127.0.0.1:9999/codex-api', clients: [{ id: 'client-fixture', name: 'fixture', accountId: 'account-fixture', modelPrefix: 'team/', maxConcurrency: 2, modelAliases: { friendly: 'gpt-fixture' } }] };
const accounts = { accounts: [{ id: 'account-fixture', name: 'fixture account' }] };
function fixtureAPI(images) {
  return async (path, payload) => {
    if (path === '/api/local/codex-api') return structuredClone(apiState);
    if (path === '/api/local/accounts') return structuredClone(accounts);
    if (path === '/api/local/codex-api/images') return images(payload);
    throw new Error('unexpected synthetic control endpoint: ' + path);
  };
}
async function open(h) { await h.context.ACTIONS.codexAPI(); await settle(); }

test('pending image read cannot fill a closed or newer API panel', async () => {
  const older = deferred(), newer = deferred(); let reads = 0;
  const h = harness(fixtureAPI(() => (++reads === 1 ? older : newer).promise));
  await open(h); const detached = h.$('#apiServiceContent');
  await h.close(); assert.equal(h.$('#officialPanel'), null);
  await open(h);
  newer.resolve({ mode: 'enabled', maxConcurrentRequests: 2 }); await settle();
  older.resolve({ mode: 'disabled', maxConcurrentRequests: 16 }); await settle();
  assert.equal(detached.isConnected, false);
  assert.equal(detached.querySelector('#apiImageSettings'), null);
  assert.equal(h.$('#apiImageMode').value, 'enabled'); assert.equal(h.$('#apiImageConcurrency').value, '2');
  assert.equal(reads, 2);
});

test('read failure offers retry without writing settings or inference', async () => {
  let reads = 0;
  const h = harness(fixtureAPI((payload) => { assert.equal(payload, undefined); if (++reads === 1) throw new Error('synthetic denied read'); return { mode: 'images_only', maxConcurrentRequests: 3 }; }));
  await open(h);
  assert.ok(h.$('[data-act="codexAPIImagesRetry"]')); assert.equal(h.$('#apiImageMode'), null);
  await h.action('codexAPIImagesRetry'); await settle();
  assert.equal(reads, 2); assert.equal(h.$('#apiImageMode').value, 'images_only');
  assert.equal(h.$('#apiImageConcurrency').value, '3');
  assert.ok(h.requests.every((request) => request.path.startsWith('/api/local/')));
});

test('closing during save never reopens panel or marks a new panel saved', async () => {
  const saved = deferred(); let writes = 0;
  const h = harness(fixtureAPI((payload) => { if (payload) { writes++; return saved.promise; } return { mode: 'disabled', maxConcurrentRequests: 1 }; }));
  await open(h); h.$('#apiImageMode').value = 'enabled';
  const saving = h.action('codexAPIImagesSave'); await settle(); await h.close();
  const epoch = h.run('OFFICIAL_PANEL.epoch'); saved.resolve({ mode: 'enabled', maxConcurrentRequests: 1 }); await saving; await settle();
  assert.equal(h.$('#officialPanel'), null); assert.equal(h.run('OFFICIAL_PANEL.epoch'), epoch); assert.equal(writes, 1);
  await open(h);
  assert.equal(h.$('#apiImageStatus').textContent, '');
});

test('invalid image mode and concurrency never cross the control API boundary', async () => {
  let writes = 0;
  const h = harness(fixtureAPI((payload) => { if (payload) writes++; return { mode: 'disabled', maxConcurrentRequests: 1 }; }));
  await open(h);
  for (const value of ['', '0', '-1', '17', '1.5', 'NaN', 'Infinity']) {
    h.$('#apiImageConcurrency').value = value;
    await assert.rejects(h.action('codexAPIImagesSave'), /1–16/);
  }
  h.$('#apiImageConcurrency').value = '2'; h.$('#apiImageMode').value = 'other';
  await assert.rejects(h.action('codexAPIImagesSave'));
  assert.equal(writes, 0);
});

test('late successful save must not claim subsequently edited draft is saved', async () => {
  const saved = deferred();
  const h = harness(fixtureAPI((payload) => payload ? saved.promise : { mode: 'disabled', maxConcurrentRequests: 1 }));
  await open(h); h.$('#apiImageMode').value = 'enabled'; h.$('#apiImageConcurrency').value = '2';
  const saving = h.action('codexAPIImagesSave'); await settle();
  h.$('#apiImageConcurrency').value = '5'; await h.emit('input', h.$('#apiImageConcurrency'));
  saved.resolve({ mode: 'enabled', maxConcurrentRequests: 2 }); await saving; await settle();
  assert.equal(h.$('#apiImageConcurrency').value, '5');
  assert.notEqual(h.$('#apiImageStatus').textContent, '图片设置已保存', 'the displayed value 5 has not been accepted by the server');
});

test('new API controls inherit existing field, input and button styles', async () => {
  const h = harness(fixtureAPI(() => ({ mode: 'disabled', maxConcurrentRequests: 1 })));
  await open(h);
  for (const [selector, className] of [['#apiImageMode', 'input'], ['#apiImageConcurrency', 'input'], ['[data-act="codexAPIImagesSave"]', 'btn']]) {
    assert.ok(h.$(selector)?.classList.contains(className)); assert.ok(css.includes('.' + className));
  }
  await h.context.ACTIONS.codexAPIEdit({ dataset: { id: 'client-fixture' } }); await settle();
  assert.ok(h.$('#apiClientModelPrefix')?.classList.contains('input'));
  assert.ok(index.indexOf('codex-accounts.js') < index.indexOf('codex-api-images.js'));
});

test('old API client read cannot overwrite a newer client editor', async () => {
  const older = deferred(); let statusReads = 0;
  const current = structuredClone(apiState);
  current.clients = [{ ...current.clients[0], id: 'newer-client', name: 'newer draft', modelPrefix: 'newer/' }];
  const h = harness(async (path, payload) => {
    if (path === '/api/local/codex-api') return ++statusReads === 1 ? older.promise : current;
    if (path === '/api/local/accounts') return accounts;
    if (path === '/api/local/codex-api/images') { assert.equal(payload, undefined); return { mode: 'disabled', maxConcurrentRequests: 1 }; }
    throw new Error('unexpected synthetic endpoint');
  });
  const loading = h.context.ACTIONS.codexAPI(); await settle();
  await h.context.ACTIONS.codexAPIEdit({ dataset: { id: 'newer-client' } }); await settle();
  older.resolve(apiState); await loading; await settle();
  assert.equal(h.$('#apiClientName').value, 'newer draft');
  assert.equal(h.$('#apiClientModelPrefix').value, 'newer/');
  assert.ok(h.$('[data-act="codexAPISave"]'));
  assert.equal(h.$('[data-act="codexAPICreate"]'), null);
});

test('removed account must not silently rebind a client to the first available account', async () => {
  const removed = structuredClone(apiState); removed.clients[0].accountId = 'deleted-account';
  let writes = 0;
  const h = harness(async (path, payload) => {
    if (path === '/api/local/codex-api') return removed;
    if (path === '/api/local/accounts') return accounts;
    if (path === '/api/local/codex-api/images') return { mode: 'disabled', maxConcurrentRequests: 1 };
    if (path === '/api/local/codex-api/clients') { writes++; throw new Error('client mutation reached control API'); }
    throw new Error('unexpected synthetic endpoint');
  });
  await h.context.ACTIONS.codexAPIEdit({ dataset: { id: 'client-fixture' } }); await settle();
  assert.equal(h.$('#apiClientAccount').value, '', 'missing binding needs an explicit new selection');
  await assert.rejects(h.action('codexAPISave'));
  assert.equal(writes, 0);
});

test('invalid namespace is rejected before creating or saving a client', async () => {
  let writes = 0;
  const h = harness(async (path, payload) => {
    if (path === '/api/local/codex-api') return apiState;
    if (path === '/api/local/accounts') return accounts;
    if (path === '/api/local/codex-api/images') return { mode: 'disabled', maxConcurrentRequests: 1 };
    if (path === '/api/local/codex-api/clients') { writes++; throw new Error('client mutation reached control API'); }
    throw new Error('unexpected synthetic endpoint');
  });
  for (const id of ['', 'client-fixture']) {
    await h.context.ACTIONS.codexAPIEdit({ dataset: { id } }); await settle();
    if (!id) h.$('#apiClientAccount').value = 'account-fixture';
    h.$('#apiClientName').value = 'synthetic';
    for (const prefix of ['team', '/team/', 'team/nested/', 'team space/', 'https://example/']) {
      h.$('#apiClientModelPrefix').value = prefix;
      await assert.rejects(h.action('codexAPISave'));
    }
  }
  assert.equal(writes, 0);
});

test('permission errors keep unsaved image draft editable and retryable', async () => {
  let writes = 0;
  const h = harness(fixtureAPI((payload) => {
    if (!payload) return { mode: 'disabled', maxConcurrentRequests: 1 };
    if (++writes === 1) throw new Error('synthetic permission rejected');
    assert.equal(payload.mode, 'enabled'); assert.equal(payload.maxConcurrentRequests, 4);
    return { mode: 'enabled', maxConcurrentRequests: 4 };
  }));
  await open(h); h.$('#apiImageMode').value = 'enabled'; h.$('#apiImageConcurrency').value = '4';
  await h.action('codexAPIImagesSave');
  assert.match(h.$('#apiImageStatus').textContent, /permission rejected.*已核对当前生效状态/);
  assert.equal(h.$('#apiImageMode').value, 'enabled'); assert.equal(h.$('#apiImageConcurrency').value, '4');
  assert.equal(h.$('[data-act="codexAPIImagesSave"]').disabled, false);
  assert.notEqual(h.$('#apiImageStatus').textContent, '图片设置已保存');
  await h.action('codexAPIImagesSave');
  assert.equal(writes, 2); assert.equal(h.$('#apiImageStatus').textContent, '图片设置已保存');
});

test('failed image persistence reconciles runtime restriction and retains the newer draft', async () => {
  const failed = deferred(); let reads = 0, writes = 0;
  const h = harness(fixtureAPI(payload => {
    if (payload) { writes++; return failed.promise; }
    reads++;
    return { mode: 'enabled', maxConcurrentRequests: 4, effectiveMode: reads > 1 ? 'disabled' : 'enabled', effectiveMaxConcurrentRequests: reads > 1 ? 1 : 4, runtimeRestricted: reads > 1, officialExecutorAvailable: false };
  }));
  await open(h);
  h.$('#apiImageMode').value = 'disabled'; h.$('#apiImageConcurrency').value = '1';
  const saving = h.action('codexAPIImagesSave'); await settle();
  h.$('#apiImageMode').value = 'images_only'; h.$('#apiImageConcurrency').value = '6';
  await h.emit('input', h.$('#apiImageConcurrency'));
  failed.reject(new Error('synthetic disk write failed')); await saving;
  assert.equal(writes, 1); assert.equal(reads, 2);
  assert.equal(h.$('#apiImageMode').value, 'images_only'); assert.equal(h.$('#apiImageConcurrency').value, '6');
  const effective = h.$('#apiImageEffective').innerHTML;
  assert.match(effective, /当前生效：关闭 · 并发 1/);
  assert.match(effective, /已保存：文字和图片 · 并发 4/);
  assert.match(effective, /当前限制与已保存设置不同/);
  assert.match(effective, /官方图片执行器未就绪/);
  assert.match(h.$('#apiImageStatus').textContent, /disk write failed.*已核对当前生效状态/);
  assert.equal(h.$('[data-act="codexAPIImagesSave"]').disabled, false);
});

test('reselecting a removed binding is explicit and preserves valid namespace and alias rules', async () => {
  const state = structuredClone(apiState); state.clients[0].accountId = 'deleted-account';
  const writes = [];
  const h = harness(async (path, payload) => {
    if (path === '/api/local/codex-api') return state;
    if (path === '/api/local/accounts') return accounts;
    if (path === '/api/local/codex-api/images') return { mode: 'disabled', maxConcurrentRequests: 1 };
    if (path === '/api/local/codex-api/clients') { writes.push(JSON.parse(JSON.stringify(payload))); return { id: 'client-fixture' }; }
    throw new Error('unexpected synthetic endpoint');
  });
  await h.context.ACTIONS.codexAPIEdit({ dataset: { id: 'client-fixture' } }); await settle();
  h.$('#apiClientAccount').value = 'account-fixture'; h.$('#apiClientModelPrefix').value = 'team-v2.1_/';
  await h.action('codexAPISave');
  assert.equal(writes.length, 1); assert.equal(writes[0].accountId, 'account-fixture');
  assert.equal(writes[0].modelPrefix, 'team-v2.1_/'); assert.deepEqual(writes[0].modelAliases, { friendly: 'gpt-fixture' });
});

test('editing after successful image save clears its stale success indicator', async () => {
  const h = harness(fixtureAPI((payload) => payload || { mode: 'disabled', maxConcurrentRequests: 1 }));
  await open(h); h.$('#apiImageMode').value = 'enabled';
  await h.action('codexAPIImagesSave');
  assert.equal(h.$('#apiImageStatus').textContent, '图片设置已保存');
  h.$('#apiImageMode').value = 'images_only'; await h.emit('change', h.$('#apiImageMode'));
  assert.equal(h.$('#apiImageStatus').textContent, '');
});
