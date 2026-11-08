import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';
import { test } from 'node:test';

const source = readFileSync(new URL('../internal/console/web/account-daily.js', import.meta.url), 'utf8');
const plain = value => JSON.parse(JSON.stringify(value));
const revision = value => 'src-v1-' + String(value).repeat(64);
const clientRevision = value => 'api-client-v1-' + String(value).repeat(64);
const tagsRevision = 'tags-v1-' + 'a'.repeat(64);
const deferred = () => { let resolve; const promise = new Promise(done => resolve = done); return { promise, resolve }; };
const flush = () => new Promise(setImmediate);

function fixture(count = 4) {
  const accounts = Array.from({ length: count }, (_, i) => ({ id: 'account_' + i, name: '账号 ' + i, sourceRevision: revision(i + 1), tagMetadata: { tags: i % 2 ? ['常用'] : [], revision: tagsRevision } }));
  const client = { id: 'client_a', name: '开发', accountId: accounts[0].id, accountIds: [accounts[0].id], imageAccountIds: [accounts[0].id], revision: clientRevision(1), poolStrategy: 'auto', disabled: true };
  const state = { accounts, clients: [client], intercept: null, clientSeq: 1, reloads: 0, calls: [], notices: [] };
  const nodes = new Map([['#localAccountList', {}], ['#accountDailyToolbar', { innerHTML: '' }]]), listeners = {};
  let context;
  const contextObject = {
    console, setTimeout: () => 1, clearTimeout() {}, Map, Set, Date, Promise,
    ACTIONS: {}, S: { route: 'accounts', localFilter: '', local: { accounts }, officialRows: [] }, OFFICIAL_PANEL: { epoch: 0 }, apiClientWritePending: false,
    API_CARD_USAGE: { cache: new Map() },
    esc: value => String(value).replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('"', '&quot;'),
    $: id => id === '#accountDailyClient' ? { value: vm.runInContext('ACCOUNT_DAILY.panel?.clientId', context) } : nodes.get(id),
    document: { addEventListener(event, handler) { listeners[event] = handler; } },
    apiServiceUsageFresh: usage => usage?.verified === true && usage.source === 'openai_codex_usage_api' && usage.status === 'fresh' && Date.parse(usage.freshUntil) > Date.now(),
    apiCardUsageData: (raw, job) => raw?.id === job.id && raw?.sourceRevision === job.revision && raw.freshUntil > Date.now() ? raw : null,
    officialPanel(_title, body) {
      context.accountDailyCancelPanel(); context.OFFICIAL_PANEL.epoch++;
      for (const id of ['#officialPanel', '#accountDailyContent', '#officialError']) nodes.set(id, { innerHTML: '', textContent: '' });
      if (body.includes('accountDailyTagsInput')) nodes.set('#accountDailyTagsInput', { value: '', disabled: false });
    },
    closeOfficialPanel() { context.accountDailyCancelPanel(); context.OFFICIAL_PANEL.epoch++; nodes.delete('#officialPanel'); },
    officialMutationToken: () => ++state.mutation,
    officialMutationCurrent: token => token === state.mutation,
    refreshOfficialCards: async () => { state.reloads++; context.S.local.accounts = plain(state.accounts); context.renderLocalCards(); },
    busy: async (_button, fn) => fn(), toast: message => state.notices.push(message),
    withAPIClientWrite: async fn => { if (context.apiClientWritePending) return; context.apiClientWritePending = true; try { await fn(); } finally { context.apiClientWritePending = false; } },
    renderLocalCards() { const library = context.S.local.accounts; context.accountDailyPrepare(library, context.accountDailyFilter(library)); context.accountDailySync(); },
    api: async (path, body) => {
      state.calls.push({ path, body: body && plain(body) });
      if (state.intercept) { const value = state.intercept(path, body); if (value !== undefined) return value; }
      if (path === '/api/local/accounts') return { accounts: plain(state.accounts) };
      if (path === '/api/local/codex-api') return { clients: plain(state.clients) };
      if (path === '/api/local/codex-api/members') {
        assert.equal(body.expectedClientRevision, client.revision); assert.equal(body.expectedSourceRevision, state.accounts.find(a => a.id === body.accountId).sourceRevision);
        const field = body.mode === 'text' ? 'accountIds' : 'imageAccountIds';
        client[field] = body.enabled ? [...new Set([...client[field], body.accountId])] : client[field].filter(id => id !== body.accountId);
        client.revision = clientRevision(++state.clientSeq);
        return { ok: true, clientId: client.id, accountId: body.accountId, mode: body.mode, enabled: body.enabled, clientRevision: client.revision };
      }
      if (path === '/api/local/accounts/daily-usage') { const account = state.accounts.find(a => a.id === body.id); assert.equal(body.expectedSourceRevision, account.sourceRevision); return { id: account.id, sourceRevision: account.sourceRevision, balance: { amount: 2, currency: 'USD' }, freshUntil: Date.now() + 10000 }; }
      if (path === '/api/local/accounts/daily-refresh') return { ok: true, id: body.id, identityEpoch: body.identityEpoch };
      if (path === '/api/local/accounts/tags') return { ok: true, id: body.id, kind: body.kind, metadata: { tags: body.tags, identityEpoch: body.identityEpoch || 0, revision: 'tags-v1-' + 'b'.repeat(64) } };
      throw new Error('unexpected path ' + path);
    },
  };
  state.mutation = 0; context = vm.createContext(contextObject); vm.runInContext(source, context);
  context.renderLocalCards();
  const choose = ids => { for (const id of ids) listeners.change({ target: { dataset: { dailySelect: id }, checked: true } }); };
  const all = () => listeners.change({ target: { id: 'accountDailyAll', checked: true } });
  const owner = () => vm.runInContext('ACCOUNT_DAILY.panel', context);
  return { context, state, accounts, client, nodes, listeners, choose, all, owner,
    selected: () => plain(context.accountDailySelected()), html: () => nodes.get('#accountDailyContent')?.innerHTML || '',
    refresh: () => context.ACTIONS.accountDailyRefresh({}), openMembers: () => context.ACTIONS.accountDailyMembers({}),
    apply: (mode = 'text', enabled = true) => context.ACTIONS.accountDailyApplyMembers({ dataset: { mode, enabled: String(enabled) } }),
    writes: () => state.calls.filter(call => call.path.endsWith('/members')),
  };
}

test('search, verified-plan and custom-tag filters compose without selecting hidden accounts', () => {
  const f = fixture(); const a = f.accounts[1];
  a.officialAccount = { id: 'official_a', identityEpoch: 3, plan: 'Pro', usage: { verified: true, source: 'openai_codex_usage_api', status: 'fresh', freshUntil: new Date(Date.now() + 10000).toISOString(), plan: { verified: true, source: 'openai_codex_usage_api', family: 'plus' } } };
  f.context.S.local.officialAccountTags = { official_a: { tags: ['常用'], revision: tagsRevision, identityEpoch: 3 } };
  f.context.renderLocalCards(); f.all(); assert.equal(f.selected().length, 4);
  f.listeners.change({ target: { id: 'accountDailyPlan', value: 'plus' } }); assert.equal(f.selected().length, 1);
  f.listeners.change({ target: { id: 'accountDailyTag', value: '常用' } }); assert.equal(f.selected()[0].id, a.id);
  f.context.S.localFilter = 'does not match'; f.context.renderLocalCards(); assert.equal(f.selected().length, 0);
  f.context.S.localFilter = ''; f.context.renderLocalCards(); assert.equal(f.selected().length, 0);
  a.officialAccount.usage.freshUntil = new Date(Date.now() - 1).toISOString(); assert.equal(f.context.accountDailyPlan(a), 'unknown');
  delete a.officialAccount.usage; assert.equal(f.context.accountDailyPlan(a), 'unknown');
});

test('same-ID replacement removes the prior selection and metadata identity cannot cross', () => {
  const f = fixture(); f.all(); f.accounts[1].sourceRevision = revision('f'); f.context.renderLocalCards();
  assert.equal(f.selected().length, 3);
  f.accounts[2].officialAccount = { id: 'official_x', identityEpoch: 4 };
  f.context.S.local.officialAccountTags = { official_x: { tags: ['旧身份'], identityEpoch: 3, revision: tagsRevision } };
  assert.equal(f.context.accountDailyMetadata(f.accounts[2]), null);
});

test('refresh concurrency is bounded and each selected account has its own result', async () => {
  const f = fixture(7); let active = 0, maximum = 0;
  f.state.intercept = (path, body) => {
    if (!path.endsWith('/daily-usage')) return;
    const account = f.accounts.find(a => a.id === body.id); active++; maximum = Math.max(maximum, active);
    return flush().then(() => { active--; return { id: account.id, sourceRevision: account.sourceRevision, balance: { amount: 2, currency: 'USD' }, freshUntil: Date.now() + 10000 }; });
  };
  f.all(); await f.refresh();
  assert.equal(maximum, 3); assert.equal(f.owner().jobs.length, 7); assert.ok(f.owner().jobs.every(job => job.state === 'done')); assert.equal(f.state.reloads, 1);
});

test('stop cancels queued refreshes and old work shares the concurrency cap with a new batch', async () => {
  const f = fixture(6), gate = deferred(); let oldCalls = 0;
  f.state.intercept = path => { if (path.endsWith('/daily-usage')) { oldCalls++; return gate.promise; } };
  f.all(); const first = f.refresh(); await flush(); assert.equal(oldCalls, 3);
  f.context.ACTIONS.accountDailyStop(); assert.ok(f.owner().jobs.every(job => job.state === 'cancelled'));
  const second = f.refresh(); await flush(); assert.equal(oldCalls, 3);
  f.state.intercept = null; gate.resolve({}); await Promise.all([first, second]);
  assert.equal(f.owner().jobs.length, 6); assert.ok(f.owner().jobs.every(job => job.state === 'done')); assert.equal(f.state.reloads, 1);
});

test('official refresh carries the protected identity and replacement cannot get a success result', async () => {
  const f = fixture(1), gate = deferred(); const a = f.accounts[0]; a.officialAccount = { id: 'vault_a', identityEpoch: 7 };
  f.context.renderLocalCards(); f.all();
  f.state.intercept = path => path.endsWith('/daily-refresh') ? gate.promise : undefined;
  const work = f.refresh(); await flush(); assert.deepEqual(f.state.calls[0].body, { id: 'vault_a', identityEpoch: 7 });
  a.officialAccount.identityEpoch++; gate.resolve({ ok: true, id: 'vault_a', identityEpoch: 7 }); await work;
  assert.equal(f.owner().jobs[0].state, 'error'); assert.match(f.owner().jobs[0].message, /变化/);
});

test('batch membership is sequential, chains committed CAS and never enables a disabled client', async () => {
  const f = fixture(); f.choose([f.accounts[1].id, f.accounts[2].id]); await f.openMembers(); await f.apply();
  assert.equal(f.writes().length, 2);
  assert.equal(f.writes()[0].body.expectedClientRevision, clientRevision(1)); assert.equal(f.writes()[1].body.expectedClientRevision, clientRevision(2));
  assert.equal(f.client.disabled, true); assert.deepEqual(f.client.imageAccountIds, [f.accounts[0].id]); assert.ok(f.owner().jobs.every(job => job.state === 'done'));
});

test('primary, last explicit member, implicit fallback and single-account anchor stay in the editor', async () => {
  for (const setup of [
    f => ({ id: f.accounts[0].id, enabled: false }),
    f => { f.client.accountIds = [f.accounts[1].id]; return { id: f.accounts[1].id, enabled: false }; },
    f => { f.client.accountIds = []; return { id: f.accounts[1].id, enabled: true }; },
    f => { f.client.accountIds.push(f.accounts[1].id); f.client.poolStrategy = 'single_account'; f.client.poolAccountId = f.accounts[1].id; return { id: f.accounts[1].id, enabled: false }; },
  ]) {
    const f = fixture(); const job = setup(f); f.choose([job.id]); await f.openMembers(); await f.apply('text', job.enabled);
    assert.equal(f.writes().length, 0); assert.match(f.owner().jobs[0].message, /客户端设置/);
  }
});

test('image removal stops at the last member and text routing is preserved', async () => {
  const f = fixture(); f.client.imageAccountIds = [f.accounts[1].id, f.accounts[2].id];
  f.choose([f.accounts[1].id, f.accounts[2].id]); await f.openMembers(); await f.apply('image', false);
  assert.equal(f.writes().length, 1); assert.deepEqual(f.client.imageAccountIds, [f.accounts[2].id]); assert.deepEqual(f.client.accountIds, [f.accounts[0].id]);
  assert.match(f.owner().jobs[1].message, /客户端设置/);
});

test('external policy changes and ambiguous replies stop the batch without retry', async () => {
  for (const kind of ['external', 'ambiguous']) {
    const f = fixture(); f.choose([f.accounts[1].id, f.accounts[2].id]); await f.openMembers();
    f.state.intercept = (path, body) => {
      if (!path.endsWith('/members')) return;
      if (kind === 'ambiguous') return { ok: true };
      f.client.revision = clientRevision(9);
      return { ok: true, clientId: f.client.id, accountId: body.accountId, mode: body.mode, enabled: body.enabled, clientRevision: clientRevision(2) };
    };
    await f.apply(); await f.apply(); assert.equal(f.writes().length, 1); assert.notEqual(f.owner().jobs[1].state, 'done');
  }
});

test('closing or changing route during preflight cannot issue writes or replace another panel', async () => {
  for (const leave of [f => f.context.closeOfficialPanel(), f => { f.context.accountDailyDispose(); f.context.S.route = 'overview'; }]) {
    const f = fixture(), gate = deferred(); f.choose([f.accounts[1].id]); await f.openMembers();
    f.state.intercept = path => path === '/api/local/accounts' ? gate.promise : undefined;
    const work = f.apply(); await flush(); leave(f); gate.resolve({ accounts: f.accounts }); await work;
    assert.equal(f.writes().length, 0); assert.equal(f.state.reloads, 0);
  }
});

test('stale source cannot join while another selected account can proceed', async () => {
  const f = fixture(); f.choose([f.accounts[1].id, f.accounts[2].id]); await f.openMembers();
  f.accounts[1].sourceRevision = revision('e'); await f.apply();
  assert.equal(f.writes().length, 1); assert.equal(f.writes()[0].body.accountId, f.accounts[2].id); assert.equal(f.owner().jobs[0].state, 'changed');
});

test('tag saves use exact source and metadata revisions, escaped text stays inert, duplicate writes are fenced', async () => {
  const f = fixture(); const account = f.accounts[1]; account.name = '<img src=x>'; account.tagMetadata.tags = ['<script>'];
  assert.doesNotMatch(f.context.accountDailyCardHTML(account), /<script>|<img/);
  f.context.ACTIONS.accountDailyTags({ dataset: { id: account.id } }); f.nodes.get('#accountDailyTagsInput').value = ' 开发, 常用, 开发';
  await f.context.ACTIONS.accountDailyTagsSave({}); await f.context.ACTIONS.accountDailyTagsSave({});
  const calls = f.state.calls.filter(call => call.path.endsWith('/tags')); assert.equal(calls.length, 1);
  assert.deepEqual(calls[0].body, { id: account.id, kind: 'local', tags: ['开发', '常用'], expectedTagsRevision: tagsRevision, expectedSourceRevision: account.sourceRevision });
  assert.deepEqual(f.state.notices, ['标签已保存']);
});

test('late tag save cannot dismiss a new panel and Escape cancels current work', async () => {
  const f = fixture(), gate = deferred(); f.context.ACTIONS.accountDailyTags({ dataset: { id: f.accounts[1].id } });
  f.state.intercept = path => path.endsWith('/tags') ? gate.promise : undefined;
  const work = f.context.ACTIONS.accountDailyTagsSave({}); await flush();
  f.context.ACTIONS.accountDailyTags({ dataset: { id: f.accounts[2].id } }); const panel = f.nodes.get('#officialPanel');
  gate.resolve({ ok: true, id: f.accounts[1].id, kind: 'local', metadata: { revision: tagsRevision } }); await work;
  assert.equal(f.nodes.get('#officialPanel'), panel); assert.equal(f.state.notices.length, 0);
  let stopped = false; f.listeners.keydown({ key: 'Escape', preventDefault() {}, stopImmediatePropagation() { stopped = true; } });
  assert.equal(stopped, true); assert.equal(f.nodes.has('#officialPanel'), false);
});

test('static hooks wire filtering, metadata, route disposal and assets into existing renderers', () => {
  const app = readFileSync(new URL('../internal/console/web/app.js', import.meta.url), 'utf8');
  const official = readFileSync(new URL('../internal/console/web/codex-accounts.js', import.meta.url), 'utf8');
  const index = readFileSync(new URL('../internal/console/web/index.html', import.meta.url), 'utf8');
  assert.match(app, /accountDailyFilter\(library\)/); assert.match(app, /accountDailyDispose\(\)/); assert.match(app, /accountDailyCardHTML\(a\)/);
  assert.match(official, /accountDailyCardHTML\(a\)/); assert.match(official, /accountDailyCancelPanel\(\)/); assert.match(index, /account-daily.js/); assert.match(index, /account-daily.css/);
});
