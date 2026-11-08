import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';

const source = fs.readFileSync(new URL('../internal/console/web/codex-accounts.js', import.meta.url), 'utf8');
const plain = value => JSON.parse(JSON.stringify(value));
function deferred() { let resolve, reject; const promise = new Promise((a, b) => { resolve = a; reject = b; }); return { promise, resolve, reject }; }
function fixture() {
  const nodes = new Map(), listeners = new Map(), timers = new Map(), calls = [];
  let now = Date.parse('2026-10-14T12:00:00Z'), timerId = 0, renders = 0;
  const root = () => ({ isConnected: true, querySelectorAll: () => [] });
  nodes.set('#localAccountList', root()); nodes.set('#modalRoot', root()); nodes.set('#view', root());
  const fresh = { source: 'openai_codex_usage_api', verified: true, status: 'fresh', freshUntil: new Date(now + 1000).toISOString(),
    plan: { source: 'openai_codex_usage_api', verified: true, family: 'pro' }, rateLimit: { primary: { remainingPercent: 73, windowSeconds: 18000 } },
    codeReviewRateLimit: { primary: { remainingPercent: 45 } } };
  const account = { id: 'official-local', name: 'Synthetic', sourceRevision: 'src-v1-' + 'a'.repeat(64),
    officialAccount: { id: 'vault-account', identityEpoch: 7, mode: 'legacy_codex_cli', status: 'ready', usage: fresh } };
  const state = { local: { accounts: [account] }, rows: [account.officialAccount], service: { running: true, clients: [] }, intercept: null };
  const document = { hidden: false, addEventListener(name, fn) { const handlers = listeners.get(name) || []; handlers.push(fn); listeners.set(name, handlers); } };
  class Clock extends Date { static now() { return now; } }
  const context = vm.createContext({
    URL, TextEncoder, Date: Clock, S: { route: 'accounts', localAccountsRevision: 0, local: plain(state.local), officialRows: plain(state.rows) }, ACTIONS: {},
    document, MutationObserver: class { observe() {} }, $: id => nodes.get(id), icon: () => '', esc: String,
    busy: async (_, fn) => fn(), confirm: () => true, toast() {}, copyText: async () => {},
    setTimeout(fn, delay = 0) { const id = ++timerId; timers.set(id, { fn, at: now + delay }); return id; }, clearTimeout(id) { timers.delete(id); },
    // The production full renderer starts ordinary provider-usage reads; it
    // must never be reached by a cache poll or by display expiry.
    renderLocalCards() { renders++; throw Error('cache polling must not render the full account list'); },
    api: async (path, body) => {
      calls.push({ path, body }); if (body !== undefined) throw Error('cache refresh must never mutate or contact a provider');
      if (state.intercept) { const out = state.intercept(path); if (out !== undefined) return out; }
      if (path === '/api/local/accounts') return plain(state.local);
      if (path === '/api/local/codex/accounts') return { accounts: plain(state.rows) };
      if (path === '/api/local/codex-api') return plain(state.service);
      throw Error('unexpected endpoint: ' + path);
    },
  });
  vm.runInContext(source, context);
  const settle = async () => { for (let i = 0; i < 16; i++) await Promise.resolve(); };
  async function advance(ms) {
    now += ms;
    for (let limit = 0; limit < 30; limit++) {
      const due = [...timers].filter(([, row]) => row.at <= now);
      if (!due.length) break;
      for (const [id, row] of due) { if (timers.delete(id)) row.fn(); }
      await settle();
    }
  }
  return { context, nodes, calls, state, account, fresh, document, timers, settle, advance, renders: () => renders,
    run: code => vm.runInContext(code, context), start: () => context.scheduleOfficialCostExpiry(context.S.local.accounts),
    emit: name => { for (const fn of listeners.get(name) || []) fn({}); }, replaceRoot: () => nodes.set('#localAccountList', root()) };
}

test('official quota display expires with freshUntil even when the DTO status has not been reread', async () => {
  const f = fixture(); f.context.info = f.account.officialAccount;
  assert.match(f.run('officialQuotaHTML(info)'), /剩余 73%/);
  await f.advance(1001);
  assert.match(f.run('officialQuotaHTML(info)'), /过期数据 73%/);
  assert.match(f.run('officialUsageSummary(info)'), /过期/);
  assert.match(f.run('officialUsageDetails(info)'), /过期/);
  assert.match(f.run('officialPlanLabel(info)'), /待刷新/);
  assert.equal(f.calls.length, 0, 'display expiry never calls upstream or local APIs');
});

test('missing or invalid freshness is never displayed as a current official quota', () => {
  for (const until of [undefined, 'bad-date']) {
    const f = fixture(); f.account.officialAccount.usage.freshUntil = until; f.context.info = f.account.officialAccount;
    assert.match(f.run('officialQuotaHTML(info)'), /过期数据/);
  }
});

test('official cards use the shared membership dialog with the local account ID', () => {
  const f = fixture(); f.context.card = { ...f.account, apiService: { clients: [{ clientId: 'fixture', image: true, primary: false }] } };
  const html = f.run('officialAccountCard(card, [], body => body)');
  assert.match(html, /data-act="apiCardMembership" data-id="official-local"/);
  assert.doesNotMatch(html, /data-act="officialAPIMember"/);
});

test('visible account cards periodically read only local caches and publish all current observations', async () => {
  const f = fixture(); f.start(); f.state.local.accounts[0].officialAccount.usage.rateLimit.primary.remainingPercent = 52;
  await f.advance(15000);
  assert.deepEqual(f.calls.map(row => row.path).sort(), ['/api/local/accounts', '/api/local/codex-api', '/api/local/codex/accounts'].sort());
  assert.equal(f.context.S.local.accounts[0].officialAccount.usage.rateLimit.primary.remainingPercent, 52);
  assert.equal(f.renders(), 0); assert.equal(f.run('CODEX_API_OVERVIEW.status.running'), true);
});

test('hidden pages stop polling and becoming visible rereads the current local cache', async () => {
  const f = fixture(); f.document.hidden = true; f.start(); await f.advance(30000); assert.equal(f.calls.length, 0);
  f.document.hidden = false; f.emit('visibilitychange'); await f.advance(0); assert.equal(f.calls.length, 3);
  f.document.hidden = true; f.emit('visibilitychange'); await f.advance(30000); assert.equal(f.calls.length, 3);
});

for (const change of ['route', 'view', 'hidden', 'newer read', 'mutation', 'opened editor', 'closed editor', 'API write']) {
  test('late cache read cannot publish after ' + change, async () => {
    const f = fixture(), pending = deferred(); f.start();
    f.state.intercept = path => path === '/api/local/accounts' ? pending.promise : undefined;
    await f.advance(15000);
    assert.equal(f.calls.length, 3);
    if (change === 'route') f.context.S.route = 'overview';
    if (change === 'view') f.replaceRoot();
    if (change === 'hidden') f.document.hidden = true;
    if (change === 'newer read') f.context.S.localAccountsRevision++;
    if (change === 'mutation') f.run('officialMutationToken()');
    if (change === 'opened editor') f.nodes.set('#officialPanel', {});
    if (change === 'closed editor') f.run('OFFICIAL_PANEL.epoch++');
    if (change === 'API write') f.run('apiClientWritePending = true');
    pending.resolve({ accounts: [] }); await f.settle();
    assert.equal(f.context.S.local.accounts.length, 1); assert.equal(f.renders(), 0);
  });
}

test('slow cache reads coalesce and a failed read preserves existing cards before retry', async () => {
  const f = fixture(), pending = deferred(); f.start();
  f.state.intercept = path => path === '/api/local/accounts' ? pending.promise : undefined;
  await f.advance(15000); await f.advance(45000); assert.equal(f.calls.length, 3);
  pending.reject(Error('fixture offline')); await f.settle();
  assert.equal(f.context.S.local.accounts.length, 1); assert.equal(f.renders(), 0);
  f.state.intercept = null; await f.advance(15000); assert.equal(f.calls.length, 6); assert.equal(f.renders(), 0);
});

test('a visible editor suspends cache reads until it closes', async () => {
  const f = fixture(); f.nodes.set('#officialPanel', {}); f.start(); await f.advance(15000);
  assert.equal(f.calls.length, 0);
  f.nodes.delete('#officialPanel'); await f.advance(15000); assert.equal(f.calls.length, 3);
});

test('reset-credit footer requires current real credits, exact mode, status and reached limit', async () => {
  const f = fixture();
  const ready = { ...f.account.officialAccount, usage: { ...f.fresh, rateLimit: { limitReached: true }, resetCreditsAvailable: 2 } };
  f.context.resetInfo = ready;
  assert.match(f.run('officialResetCreditButton(resetInfo)'), /data-act="officialResetCredit".*消耗官方重置次数（2）/);
  for (const patch of [{ mode:'chatgpt_web_session' }, { mode:'codex' }, { status:'needs_login' }, { status:'unknown' }, { id:'' },
    { usage:{ ...ready.usage, verified:false } }, { usage:{ ...ready.usage, source:'import_hint' } },
    { usage:{ ...ready.usage, rateLimit:{ limitReached:false } } },
    ...['2', null, -1, 0, NaN, Infinity, 1.1].map(count => ({ usage:{ ...ready.usage, resetCreditsAvailable:count } }))]) {
    f.context.resetInfo = { ...ready, ...patch };
    assert.doesNotMatch(f.run('officialResetCreditButton(resetInfo)'), /officialResetCredit"/);
  }
  for (const until of [undefined, 'bad-date', '2000-01-01T00:00:00Z']) {
    f.context.resetInfo = { ...ready, usage: { ...ready.usage, freshUntil:until } };
    assert.match(f.run('officialResetCreditButton(resetInfo)'), /data-act="officialUsageRefresh".*刷新官方重置次数/);
  }
  f.context.resetInfo = ready; await f.advance(1001);
  assert.doesNotMatch(f.run('officialResetCreditButton(resetInfo)'), /消耗官方重置/);
});

test('expiry updates only observations including the reset affordance and preserves card controls', async () => {
  const f = fixture(), patches = [], selectors = [];
  f.account.officialAccount.usage.resetCreditsAvailable = 2;
  f.account.officialAccount.usage.rateLimit.limitReached = true;
  f.context.S.local = plain(f.state.local);
  const card = { dataset: { accountId:'vault-account', identityEpoch:'7' }, classList:{ toggle() {}, contains(){ return false; } },
    querySelector(selector) { selectors.push(selector); return { selector }; } };
  f.nodes.get('#localAccountList').querySelectorAll = () => [card];
  f.context.patchOfficialObservationHTML = (node, html) => { if (node) patches.push({ selector:node.selector, html }); };
  f.start(); await f.advance(1001);
  assert.match(patches.find(row => row.selector === '.official-card-quotas').html, /过期数据/);
  assert.match(patches.find(row => row.selector === '[data-official-reset-credit]').html, /officialUsageRefresh.*刷新官方重置次数/);
  assert.match(patches.find(row => row.selector === '.official-plan-badge').html, /待刷新/);
  assert.ok(selectors.every(selector => !/official-card-actions|apiCardMembership|remote|wb-card-more/.test(selector)));
  assert.equal(f.calls.length, 0); assert.equal(f.renders(), 0);
  patches.length = 0; card.dataset.identityEpoch = '6';
  f.run('updateOfficialAccountObservations($("#localAccountList"), S.local.accounts)');
  assert.equal(patches.length, 0, 'another identity must not update an existing card');
});

// Small DOM doubles test the production node reconciliation directly; the
// action/HTTP fixtures above deliberately never implement full-page rendering.
function element(name, attrs = {}, children = [], writes = { count:0 }) {
  const values = new Map(Object.entries(attrs));
  const node = { nodeType:1, nodeName:name.toUpperCase(), childNodes:children, parentNode:null, writes,
    get attributes() { return [...values].map(([name, value]) => ({ name, value })); },
    hasAttribute: name => values.has(name), getAttribute: name => values.get(name) ?? null,
    setAttribute(name, value) { writes.count++; values.set(name, String(value)); },
    removeAttribute(name) { writes.count++; values.delete(name); },
    get lastChild() { return this.childNodes.at(-1); },
    insertBefore(child, before) { writes.count++; if (child.parentNode) child.remove(); const index = before ? this.childNodes.indexOf(before) : this.childNodes.length; this.childNodes.splice(index, 0, child); child.parentNode = this; },
    appendChild(child) { this.insertBefore(child, null); },
    remove() { this.parentNode.writes.count++; this.parentNode.childNodes.splice(this.parentNode.childNodes.indexOf(this), 1); this.parentNode = null; },
    replaceWith(next) { const parent=this.parentNode, index=parent.childNodes.indexOf(this); parent.writes.count++; parent.childNodes[index]=next; next.parentNode=parent; this.parentNode=null; },
  };
  for (const child of children) child.parentNode = node;
  return node;
}
function textNode(value, writes) {
  let text = value;
  return { nodeType:3, nodeName:'#text', parentNode:null,
    get nodeValue() { return text; }, set nodeValue(value) { writes.count++; text=value; },
    remove() { this.parentNode.childNodes.splice(this.parentNode.childNodes.indexOf(this), 1); this.parentNode=null; },
  };
}

test('equal observations do not mutate live DOM and changed button labels preserve node identity', () => {
  const f=fixture(), writes={count:0}, label=textNode('Pro', writes), button=element('button', { 'data-act':'officialResetCredit' }, [label], writes);
  const existing=element('div', {}, [button], writes);
  f.context.current=existing; f.context.next=element('div', {}, [element('button', {'data-act':'officialResetCredit'}, [textNode('Pro', {count:0})])]);
  f.run('patchOfficialObservationNode(current, next)'); assert.equal(writes.count, 0);
  f.context.next=element('div', {}, [element('button', {'data-act':'officialUsageRefresh'}, [textNode('Pro 待刷新', {count:0})])]);
  f.run('patchOfficialObservationNode(current, next)');
  assert.equal(existing.childNodes[0], button); assert.equal(button.childNodes[0], label);
  assert.equal(button.getAttribute('data-act'), 'officialUsageRefresh'); assert.equal(label.nodeValue, 'Pro 待刷新');
});

test('service plan refresh keeps the same expanded detail when groups move', () => {
  const f=fixture(), writes={count:0};
  const a=element('details', {'data-observation-key':'pro', open:''}, [], writes), b=element('details', {'data-observation-key':'team'}, [], writes);
  const current=element('div', {}, [a,b], writes);
  f.context.current=current; f.context.next=element('div', {}, [element('details', {'data-observation-key':'team'}), element('details', {'data-observation-key':'pro'})]);
  f.run('patchOfficialObservationNode(current, next)');
  assert.equal(current.childNodes[0], b); assert.equal(current.childNodes[1], a); assert.equal(a.hasAttribute('open'), true);
  assert.equal(b.hasAttribute('open'), false);
});
