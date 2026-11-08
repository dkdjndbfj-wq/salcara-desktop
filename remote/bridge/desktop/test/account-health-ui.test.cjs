'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.resolve(__dirname, '../../internal/console/web/account-health.js'), 'utf8');

function deferred() {
  let resolve;
  const promise = new Promise(done => { resolve = done; });
  return { promise, resolve };
}

class Element {
  constructor() { this.dataset = {}; this.children = []; this.isConnected = true; this.textContent = ''; this.innerHTML = ''; }
  appendChild(child) { this.children.push(child); return child; }
  querySelector(selector) {
    if (selector === '.api-provider-ident') return this.ident || null;
    if (selector === '.key-face') return null;
    if (selector === '[data-account-health]') return this.children.find(child => child.dataset?.accountHealth) || this.ident?.children.find(child => child.dataset?.accountHealth) || null;
    return null;
  }
  querySelectorAll(selector) { return selector === '[data-account-id]' ? this.cards || [] : []; }
  remove() { this.isConnected = false; }
}

function fixture() {
  const account = { id: 'acct-1', sourceRevision: 'source-a' };
  const ident = new Element();
  const card = new Element(); card.dataset.accountId = account.id; card.ident = ident;
  const root = new Element(); root.cards = [card];
  const requests = [];
  const window = { renderLocalCards() {} };
  const context = vm.createContext({
    window,
    document: { getElementById: () => root, createElement: () => new Element() },
    S: { route: 'accounts', routeRevision: 1, local: { accounts: [account] } },
    ACTIONS: {},
    $: selector => selector === '#localAccountList' ? root : null,
    api: () => { const request = deferred(); requests.push(request); return request.promise; },
    busy: (_button, fn) => fn(),
    toast() {},
    esc: value => String(value ?? '').replace(/[&<>"']/g, ch => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[ch])),
  });
  vm.runInContext(source, context, { filename: 'account-health.js' });
  return { context, account, card, requests, window };
}

test('older health snapshots cannot overwrite a newer response for the same account list', async () => {
  const f = fixture();
  f.window.renderLocalCards();
  f.window.renderLocalCards();

  f.requests[1].resolve([{ accountId: 'acct-1', score: 0.2, available: false, cooldownRemainingSec: 90 }]);
  await new Promise(resolve => setImmediate(resolve));
  f.requests[0].resolve([{ accountId: 'acct-1', score: 0, available: false, cooldownRemainingSec: 900 }]);
  await new Promise(resolve => setImmediate(resolve));

  const badge = f.card.querySelector('[data-account-health]');
  assert.match(badge.textContent, /冷却 2 分钟/);
  assert.match(badge.innerHTML, /accountHealthReset/);
});

test('a response for an account identity that was replaced cannot be applied', async () => {
  const f = fixture();
  f.window.renderLocalCards();
  f.context.S.local.accounts[0].sourceRevision = 'source-b';
  f.window.renderLocalCards();

  f.requests[0].resolve([{ accountId: 'acct-1', score: 0, available: false, cooldownRemainingSec: 900 }]);
  await new Promise(resolve => setImmediate(resolve));
  f.requests[1].resolve([{ accountId: 'acct-1', score: 0.9, available: true }]);
  await new Promise(resolve => setImmediate(resolve));

  assert.equal(f.card.querySelector('[data-account-health]'), null, 'stale cooldown data is not shown for a replaced credential');
});
