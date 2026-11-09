import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';

const read = name => readFileSync(new URL('../internal/console/web/' + name, import.meta.url), 'utf8');
const daily = read('account-daily.js'), official = read('codex-accounts.js'), workbench = read('workbench.js'), app = read('app.js');
const section = (source, start, end) => {
  const from = source.indexOf(start), to = source.indexOf(end, from);
  assert.ok(from >= 0 && to > from, 'actual function range must exist');
  return source.slice(from, to);
};
const plain = value => JSON.parse(JSON.stringify(value));
const sourceRevision = value => 'src-v1-' + value.repeat(64);
const deferred = () => { let resolve, reject; const promise = new Promise((yes, no) => { resolve = yes; reject = no; }); return { promise, resolve, reject }; };
const account = id => ({ id, name: '同名账号', baseUrl: 'https://same.example.test/v1', sourceRevision: sourceRevision(id), poolState: { inflight: 0 }, apiService: { clients: [] } });

function fixture() {
  const badges = [], nodes = new Map(), requests = [], timers = [];
  const a = account('a'), b = account('b');
  const root = { isConnected: true, querySelectorAll: selector => selector === '[data-account-active]' ? badges : [] };
  nodes.set('#localAccountList', root); nodes.set('#accountDailyToolbar', { innerHTML: '' });
  const local = { accounts: [a, b], bindings: {}, tools: [{ id: 'codex', name: 'Codex CLI', available: true }] };
  const state = { renders: 0, backend: plain(local), service: { enabled: true, running: true, clients: [] }, intercept: null };
  let context;
  const globals = {
    console, URL, Map, Set, Date, Promise, ACTIONS: {}, OFFICIAL_PANEL: { mutation: 0, epoch: 0, refresh: 0 },
    S: { route: 'accounts', routeRevision: 1, localFilter: '', localAccountsRevision: 0, local, officialRows: [] },
    CODEX_API_OVERVIEW: { revision: 0, status: state.service, error: false, pending: false, activityLocal: local },
    apiClientWritePending: false, officialLibraryRevision: 0,
    $: selector => nodes.get(selector), document: { hidden: false, addEventListener() {} },
    esc: value => String(value).replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('"', '&quot;'),
    icon: name => `<svg data-icon="${name}"></svg>`,
    setTimeout: (fn, delay) => { timers.push({ fn, delay }); return timers.length; }, clearTimeout() {},
    apiServiceUsageFresh: () => false, officialLibraryAccounts: rows => rows,
    renderLocalCards() { state.renders++; context.accountDailyPrepare(context.S.local.accounts, context.S.local.accounts); context.accountDailySync(); },
    renderAgentCards() {}, updateOfficialAccountObservations() {}, scheduleOfficialCostExpiry() {}, syncOfficialCleanupAction() {},
    renderCodexAPIOverview() { context.accountActiveSync(); },
    officialMutationCurrent: () => true, loadOfficialLibrary: async () => true,
    api: async path => {
      requests.push(path);
      if (state.intercept) { const result = state.intercept(path); if (result !== undefined) return result; }
      if (path === '/api/local/accounts') return plain(state.backend);
      if (path === '/api/local/codex/accounts') return { accounts: [] };
      if (path === '/api/local/codex-api') return plain(state.service);
      throw new Error('unexpected request ' + path);
    },
  };
  context = vm.createContext(globals);
  vm.runInContext(daily + '\n' + section(official, 'async function loadCodexAPIOverview()', 'async function toggleCodexAPIService(') + '\n' +
    section(official, 'let officialAccountCacheOwner = null;', 'function apiServiceCostsHTML()') + '\n' +
    section(official, 'async function refreshOfficialCards(', 'function officialLoginChallenge(') + '\n' +
    section(workbench, 'async function wbRefreshLaunchCards()', 'async function wbOpenAccount('), context);
  const addBadge = row => {
    const classes = new Set(), attrs = new Map();
    const card = { dataset: {}, classList: { toggle: (name, on) => on ? classes.add(name) : classes.delete(name), contains: name => classes.has(name) } };
    const badge = { dataset: { accountActive: row.id, accountActiveIdentity: context.accountDailyIdentity(row) }, hidden: true, textContent: '', title: '',
      closest: () => card, setAttribute: (name, value) => attrs.set(name, value), removeAttribute: name => attrs.delete(name) };
    badges.push(badge); return { badge, card, attrs };
  };
  const cards = [addBadge(a), addBadge(b)];
  context.accountDailyPrepare(local.accounts, local.accounts);
  return { context, state, requests, timers, nodes, root, a, b, cards, addBadge,
    active: row => context.accountActiveState(row), sync: () => context.accountActiveSync(),
    service(client) {
      context.CODEX_API_OVERVIEW.status = { enabled: true, running: true, clients: [client] };
      context.CODEX_API_OVERVIEW.activityLocal = context.S.local;
      for (const row of context.S.local.accounts) row.apiService.clients = [{ clientId: client.id, disabled: false, text: true, image: true }];
    },
    poll() { vm.runInContext('officialAccountCacheOwner = { root: $("#localAccountList"), timer: 0, pending: false }', context); return vm.runInContext('refreshOfficialAccountCache(officialAccountCacheOwner)', context); },
  };
}

test('same-name and same-host accounts light only the exact successfully applied ID', () => {
  const f = fixture(); f.context.S.local.bindings.codex = { accountId: 'b', appliedId: 'b', pending: false };
  f.sync(); assert.equal(f.cards[0].badge.hidden, true); assert.equal(f.cards[1].badge.textContent, '当前应用');
  assert.equal(f.cards[1].card.classList.contains('account-is-active'), true);
  assert.equal(f.cards[1].attrs.get('aria-label'), '当前应用：当前应用于 Codex CLI');
  f.context.S.local.bindings.codex = { accountId: 'a', appliedId: 'a', pending: false };
  f.sync(); assert.equal(f.cards[0].badge.hidden, false); assert.equal(f.cards[1].badge.hidden, true);
  assert.equal(f.cards[1].card.classList.contains('account-is-active'), false);
});

test('selection, pending application and invalid or unavailable bindings never claim usage', () => {
  const f = fixture();
  for (const binding of [{ accountId: 'a' }, { accountId: 'a', appliedId: 'a', pending: true }, { accountId: 'a', appliedId: 'b', pending: false }, { accountId: 'a', appliedId: 'a' }]) {
    f.context.S.local.bindings.codex = binding; assert.equal(f.active(f.a), null); assert.equal(f.active(f.b), null);
  }
  f.context.S.local.bindings.codex = { accountId: 'a', appliedId: 'a', pending: false };
  f.context.S.local.tools[0].available = false; assert.equal(f.active(f.a), null);
});

test('only per-account live requests light a pool member; client counts and old use do not', () => {
  const f = fixture();
  f.service({ id: 'client', disabled: false, accountId: 'a', accountIds: ['a', 'b'], imageAccountIds: ['a', 'b'], poolStrategy: 'auto' });
  f.context.CODEX_API_OVERVIEW.status.statistics = [{ clientId: 'client', inflight: 2 }];
  for (const row of [f.a, f.b]) { row.apiService.active = true; row.apiService.activeClientCount = 10; row.poolEnabled = true; row.poolState.lastUsedAt = new Date().toISOString(); }
  assert.equal(f.active(f.a), null); assert.equal(f.active(f.b), null);
  f.b.poolState.inflight = 2; f.sync();
  assert.equal(f.cards[0].badge.hidden, true); assert.equal(f.cards[1].badge.textContent, '请求中');
  assert.equal(f.cards[1].badge.title, '2 个在途请求');
  f.b.poolState.inflight = 0; f.sync(); assert.equal(f.cards[1].badge.hidden, true);
  for (const value of ['1', NaN, Infinity, -1, 0.5]) { f.a.poolState.inflight = value; assert.equal(f.active(f.a), null); }
});

test('running services highlight only uniquely fixed current routes, including fixed-member selection', () => {
  const f = fixture();
  f.service({ id: 'client', name: '本地服务', disabled: false, accountId: 'a', accountIds: ['a', 'b'], poolStrategy: 'single_account', poolAccountId: 'b', imageAccountIds: ['b'] });
  assert.equal(f.active(f.a), null); assert.equal(f.active(f.b).label, '服务已启用');
  f.context.CODEX_API_OVERVIEW.status.clients[0].poolAccountId = 'removed';
  f.context.CODEX_API_OVERVIEW.status.clients[0].imageAccountIds = ['a', 'b'];
  assert.equal(f.active(f.a), null); assert.equal(f.active(f.b), null);
});

test('stopped, disabled, removed, failed, mismatched and stale service snapshots clear the border', () => {
  for (const change of [
    f => { f.context.CODEX_API_OVERVIEW.status.running = false; },
    f => { f.context.CODEX_API_OVERVIEW.status.enabled = false; },
    f => { f.context.CODEX_API_OVERVIEW.status.clients[0].disabled = true; },
    f => { f.context.CODEX_API_OVERVIEW.status.clients = []; },
    f => { f.context.CODEX_API_OVERVIEW.error = true; },
    f => { f.context.CODEX_API_OVERVIEW.pending = true; },
    f => { f.a.apiService.clients = []; },
    f => { f.context.S.local = { ...f.context.S.local }; },
  ]) {
    const f = fixture(); f.service({ id: 'client', disabled: false, accountId: 'a' }); f.sync();
    assert.equal(f.cards[0].badge.hidden, false); change(f); f.sync();
    assert.equal(f.cards[0].badge.hidden, true); assert.equal(f.cards[0].card.classList.contains('account-is-active'), false);
  }
});

test('replaced identities, unlinked accounts and quota-only web sessions never inherit a glow', () => {
  const f = fixture(); f.a.poolState.inflight = 1; f.sync();
  f.context.S.local.accounts[0] = { ...f.a, sourceRevision: sourceRevision('c') };
  f.sync(); assert.equal(f.cards[0].badge.hidden, true); assert.equal(f.active(f.a), null);
  for (const extra of [{ libraryOnly: true }, { officialAccount: { id: 'vault', identityEpoch: 2, mode: 'chatgpt_web_session' } }]) {
    const row = { ...f.b, poolState: { inflight: 1 }, ...extra }; f.context.S.local.accounts[1] = row; assert.equal(f.active(row), null);
  }
});

test('existing cache reconciliation updates status without replacing cards or focused inputs', () => {
  const f = fixture(); f.b.poolState.inflight = 1;
  f.context.accountDailyReconcile(); assert.equal(f.cards[1].badge.textContent, '请求中'); assert.equal(f.state.renders, 0);
  f.b.poolState.inflight = 0; f.context.accountDailyReconcile(); assert.equal(f.cards[1].badge.hidden, true); assert.equal(f.state.renders, 0);
});

test('existing local-cache poll publishes new state and a failed refresh removes stale usage', async () => {
  const f = fixture(); f.state.backend.accounts[1].poolState.inflight = 1;
  await f.poll(); assert.equal(f.cards[1].badge.textContent, '请求中');
  assert.deepEqual(f.requests.sort(), ['/api/local/accounts', '/api/local/codex/accounts', '/api/local/codex-api'].sort());
  f.state.intercept = path => path === '/api/local/accounts' ? Promise.reject(new Error('offline')) : undefined;
  await f.poll(); assert.equal(f.cards[1].badge.hidden, true);
  assert.equal(f.context.CODEX_API_OVERVIEW.error, true, 'the same failed poll must retire the aggregate service claim');
  f.state.intercept = null; await f.poll(); assert.equal(f.cards[1].badge.textContent, '请求中');
  assert.equal(f.context.CODEX_API_OVERVIEW.error, false);
  assert.ok(f.timers.every(timer => timer.delay === 15000), 'reuse only the existing bounded visible-page poll');
});

test('malformed cache snapshots retire both account activity and the aggregate service status', async () => {
  for (const path of ['/api/local/accounts','/api/local/codex/accounts','/api/local/codex-api']) {
    const f=fixture();f.service({id:'client',disabled:false,accountId:'a'});f.sync();assert.equal(f.cards[0].badge.hidden,false);
    f.state.intercept=request=>request===path?{}:undefined;
    await f.poll();assert.equal(f.cards[0].badge.hidden,true);assert.equal(f.context.CODEX_API_OVERVIEW.error,true);
  }
});

test('a late failed poll cannot clear a newer account snapshot or another view', async () => {
  for (const change of ['newer snapshot', 'new root', 'navigation']) {
    const f = fixture(), gate = deferred(); f.b.poolState.inflight = 1; f.sync();
    f.state.intercept = path => path === '/api/local/accounts' ? gate.promise : undefined;
    const pending = f.poll();
    if (change === 'newer snapshot') { f.context.S.local = plain(f.context.S.local); f.context.S.localAccountsRevision++; }
    if (change === 'new root') f.nodes.set('#localAccountList', { ...f.root });
    if (change === 'navigation') f.context.S.route = 'overview';
    gate.reject(new Error('late offline')); await pending;
    assert.equal(f.cards[1].badge.hidden, false);
    assert.equal(f.context.CODEX_API_OVERVIEW.error, false);
  }
});

test('service refresh failure removes only service claims; late service results cannot attach to new local state', async () => {
  const f = fixture(); f.service({ id: 'client', disabled: false, accountId: 'a' }); f.sync();
  f.state.intercept = () => Promise.reject(new Error('offline'));
  await f.context.loadCodexAPIOverview(); assert.equal(f.cards[0].badge.hidden, true);
  const gate = deferred(); f.state.intercept = () => gate.promise;
  const pending = f.context.loadCodexAPIOverview(); f.context.S.local = plain(f.context.S.local);
  gate.resolve({ enabled: true, running: true, clients: [{ id: 'client', disabled: false, accountId: 'a' }] });
  await pending; assert.equal(f.cards[0].badge.hidden, true);
});

test('failed launch-card rereads clear old active state without retrying the launch', async () => {
  const f = fixture(); f.a.poolState.inflight = 1; f.sync();
  f.state.intercept = () => Promise.reject(new Error('offline'));
  await assert.rejects(f.context.wbRefreshLaunchCards(), /offline/); assert.equal(f.cards[0].badge.hidden, true);
  assert.deepEqual(f.requests, ['/api/local/accounts']);
});

test('launch rereads and manual-refresh failures cannot change a later visit to the same route', async () => {
  for (const method of ['wbRefreshLaunchCards', 'refreshOfficialCards']) {
    const f = fixture(), gate = deferred(); f.a.poolState.inflight = 1; f.sync();
    f.state.intercept = () => gate.promise; const pending = f.context[method]();
    f.context.S.routeRevision += 2;
    gate.reject(new Error('old visit')); await assert.rejects(pending, /old visit/);
    assert.equal(f.cards[0].badge.hidden, false);
  }
});

test('ordinary and official cards share escaped identity markers and retain direct launch controls', () => {
  const f = fixture();
  Object.assign(f.context, { officialAPIServiceSummary: () => ({ action: '加入 API 服务' }), officialCardAction: () => '', officialStatusTone: () => 'ok', officialPlanBadge: () => '', officialAccountStatusLabel: () => '已登录', officialQuotaHTML: () => '', officialUsageDetails: () => '', officialSubscriptionCardHTML: () => '', officialUpdatedHTML: () => '', officialResetCreditButton: () => '' });
  vm.runInContext(section(app, 'function localHubKey(', 'function localForm('), f.context);
  vm.runInContext(section(app, 'function accountLaunchToolIcon(', '\nfunction renderLocalCards()') + '\n' + section(official, 'function officialAccountCard(', '\nasync function reconcileClosedOfficialLogin('), f.context);
  const normal = f.context.localAccountCard(f.a); assert.match(normal, /data-account-active="a"/); assert.match(normal, /data-act="accountOpen"/);
  const protectedRow = { ...f.b, officialAccount: { id: 'vault_b', identityEpoch: 3, status: 'ready' } };
  const protectedHTML = f.context.officialAccountCard(protectedRow, [], body => body);
  assert.match(protectedHTML, /data-account-id="vault_b"/); assert.match(protectedHTML, /data-account-active="b"/);
  assert.match(protectedHTML, /data-account-active-identity="\[&quot;b&quot;,&quot;vault_b&quot;,3,/);
  assert.doesNotMatch(f.context.accountDailyCardHTML({ ...f.a, id: '\"><img src=x>' }), /<img/);
});
