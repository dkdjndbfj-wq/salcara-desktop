'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const app = fs.readFileSync(path.resolve(__dirname, '../../internal/console/web/app.js'), 'utf8');
const busy = app.slice(app.indexOf('const BUTTON_OPERATIONS ='), app.indexOf('let localConversationSyncPending ='));
const start = app.indexOf('  quit: async '), end = app.indexOf('\n};', start);
assert.ok(start >= 0 && end > start, 'the production quit action exists');
const action = 'const ACTIONS = {\n' + app.slice(start, end) + '\n}; globalThis.quit = ACTIONS.quit;';
function deferred() {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}
function fixture({ confirm = async () => true, request = async () => ({ ok: true }) } = {}) {
  const attributes = new Map(), calls = [], notices = [];
  const requestStarted = deferred();
  let confirmations = 0, view = { isConnected: true }, html = 'settings page';
  const button = { innerHTML: '退出 Salcara Bridge', textContent: '退出 Salcara Bridge', disabled: false, isConnected: true,
    setAttribute: (name, value) => attributes.set(name, value), getAttribute: name => attributes.get(name) ?? null,
    removeAttribute: name => attributes.delete(name) };
  const body = { writes: 0, get innerHTML() { return html; }, set innerHTML(value) {
    html = value; this.writes++; button.isConnected = false; view.isConnected = false;
  } };
  const state = { route: 'settings', routeRevision: 4 };
  const context = vm.createContext({ S: state, $: selector => selector === '#view' ? view : null, document: { body },
    confirm: (...args) => { confirmations++; return confirm(...args); },
    api: (...args) => { calls.push(args); requestStarted.resolve(); return request(...args); }, toast: (...args) => notices.push(args) });
  vm.runInContext(busy + '\n' + action, context, { filename: 'quit-production-action.js' });
  return { state, button, body, calls, notices, context, requestStarted: requestStarted.promise, confirmations: () => confirmations,
    quit: () => context.quit(button), replaceView() { view = { isConnected: true }; } };
}

test('cancelling quit sends no request and restores the settings button', async () => {
  const f = fixture({ confirm: async () => false });
  await f.quit();
  assert.equal(f.calls.length, 0); assert.equal(f.body.writes, 0); assert.deepEqual(f.notices, []);
  assert.equal(f.button.disabled, false); assert.equal(f.button.innerHTML, '退出 Salcara Bridge');
  assert.equal(f.button.getAttribute('aria-busy'), null);
});

test('failed and unconfirmed quit replies preserve the page and permit an explicit retry', async () => {
  for (const outcome of [new Error('network unavailable'), {}, { ok: false }, { ok: 'true' }, null]) {
    let attempt = 0;
    const f = fixture({ request: async () => {
      if (++attempt > 1) return { ok: true };
      if (outcome instanceof Error) throw outcome;
      return outcome;
    } });
    await f.quit();
    assert.equal(f.body.innerHTML, 'settings page'); assert.equal(f.body.writes, 0);
    assert.match(f.notices[0][0], /退出未确认，请重试/); assert.equal(f.notices[0][1], 'bad');
    assert.equal(f.button.disabled, false); assert.equal(f.button.getAttribute('aria-busy'), null);
    await f.quit();
    assert.equal(f.calls.length, 2); assert.equal(f.confirmations(), 2);
    assert.match(f.body.innerHTML, /Salcara Bridge 正在退出/); assert.doesNotMatch(f.body.innerHTML, /已退出|可以关闭这个页面/);
  }
});

test('repeated quit clicks share the confirmation and request and await an acknowledged reply', async () => {
  const confirmation = deferred(), response = deferred();
  const f = fixture({ confirm: () => confirmation.promise, request: () => response.promise });
  const first = f.quit(), second = f.quit();
  assert.equal(f.confirmations(), 1); assert.equal(f.button.disabled, true); assert.equal(f.calls.length, 0);
  confirmation.resolve(true); await f.requestStarted;
  const third = f.quit();
  assert.equal(f.calls.length, 1); assert.equal(f.calls[0][0], '/api/quit');
  assert.equal(JSON.stringify(f.calls[0][1]), '{}'); assert.equal(f.body.writes, 0);
  response.resolve({ ok: true }); await Promise.all([first, second, third]);
  assert.equal(f.body.writes, 1); assert.match(f.body.innerHTML, /Salcara Bridge 正在退出/);
  assert.doesNotMatch(f.body.innerHTML, /已退出/); assert.deepEqual(f.notices, []);
});

test('confirmation cannot quit after navigation, page replacement or detachment', async () => {
  for (const leave of [f => { f.state.route = 'accounts'; }, f => { f.state.routeRevision++; }, f => f.replaceView(), f => { f.button.isConnected = false; }]) {
    const confirmation = deferred(), f = fixture({ confirm: () => confirmation.promise });
    const pending = f.quit(); leave(f); confirmation.resolve(true); await pending;
    assert.equal(f.calls.length, 0); assert.equal(f.body.writes, 0); assert.deepEqual(f.notices, []);
  }
});

test('late quit replies cannot replace another page or toast onto it', async () => {
  for (const failed of [false, true]) {
    const response = deferred(), f = fixture({ request: () => response.promise });
    const pending = f.quit(); await f.requestStarted; assert.equal(f.calls.length, 1);
    f.replaceView(); f.state.routeRevision++;
    if (failed) response.reject(new Error('late failure')); else response.resolve({ ok: true });
    await pending;
    assert.equal(f.body.writes, 0); assert.deepEqual(f.notices, []);
  }
});
