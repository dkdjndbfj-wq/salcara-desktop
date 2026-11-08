import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
const source = fs.readFileSync(new URL('../internal/console/web/api-card-membership.js', import.meta.url), 'utf8');
const plain = value => JSON.parse(JSON.stringify(value));
const SOURCE = 'src-v1-' + 'b'.repeat(64);
const CLIENT = 'api-client-v1-' + 'c'.repeat(64);
function gate() { let resolve; return { promise: new Promise(r => resolve = r), resolve }; }
function fixture() {
  const nodes = new Map(), calls = [], notices = [], editors = [], listeners = {};
  const account = { id: 'account_b', name: 'Fixture API', sourceRevision: SOURCE };
  const primary = { id: 'account_a', name: 'Primary API', sourceRevision: 'src-v1-' + 'a'.repeat(64) };
  const client = { id: 'client_a', revision: CLIENT, name: 'Fixture client', accountId: primary.id, accountIds: [primary.id, account.id], imageAccountIds: [primary.id], poolStrategy: 'auto' };
  const state = { clients: [client], accounts: [primary, account], intercept: null, sequence: 0 };
  const context = vm.createContext({
    ACTIONS: {}, OFFICIAL_PANEL: { epoch: 0 }, S: { route: 'accounts', local: { accounts: state.accounts } },
    document: { addEventListener(name, fn) { listeners[name] = fn; } },
    $: id => nodes.get(id), esc: value => String(value).replaceAll('<', '&lt;').replaceAll('"', '&quot;'),
    apiClientAccounts: local => (local.accounts || []).filter(a => !a.libraryOnly && a.officialAccount?.mode !== 'chatgpt_web_session'),
    officialPanel() { context.OFFICIAL_PANEL.epoch++; nodes.clear(); for (const id of ['#officialPanel', '#apiCardMembers', '#officialError']) nodes.set(id, { innerHTML: '', textContent: '' }); },
    closeOfficialPanel() { context.OFFICIAL_PANEL.epoch++; nodes.clear(); },
    openCodexAPIClient: async id => editors.push(id),
    busy: async (_button, fn) => fn(), toast: value => notices.push(value),
    officialMutationToken: () => ++state.sequence, officialMutationCurrent: token => token === state.sequence,
    refreshOfficialCards: async () => {},
    api: async (path, body) => {
      calls.push({ path, body: body && plain(body) });
      if (state.intercept) { const result = state.intercept(path, body); if (result !== undefined) return result; }
      if (path === '/api/local/accounts') return plain({ accounts: state.accounts });
      if (path === '/api/local/codex-api') return plain({ clients: state.clients });
      if (path === '/api/local/codex-api/members') return { ok: true, ...body };
      throw new Error('unexpected request');
    },
  });
  vm.runInContext(source, context);
  const button = (mode = 'text', enabled = false, id = client.id) => ({ dataset: { id, mode, enabled: String(enabled) } });
  return { context, nodes, calls, notices, editors, state, account, primary, client, listeners,
    open: id => context.ACTIONS.apiCardMembership({ dataset: { id: id || account.id } }),
    toggle: (mode, enabled, id) => context.ACTIONS.apiCardMemberToggle(button(mode, enabled, id)),
    edit: id => context.ACTIONS.apiCardMemberEdit(button('text', false, id)),
    writes: () => calls.filter(call => call.body), html: () => nodes.get('#apiCardMembers')?.innerHTML,
  };
}

test('card opens exact-account membership without any mutation or auto-enable', async () => {
  const f = fixture(); await f.open();
  assert.match(f.html(), /Fixture API/); assert.match(f.html(), /移出文字/); assert.match(f.html(), /加入图片/);
  assert.equal(f.writes().length, 0); assert.equal(f.editors.length, 0);
});

test('explicit text removal and image addition never use both or touch other clients', async () => {
  const f = fixture(); await f.open(); await f.toggle('text', false);
  assert.deepEqual(f.writes(), [{ path: '/api/local/codex-api/members', body: { clientId: 'client_a', accountId: 'account_b', mode: 'text', enabled: false, expectedSourceRevision: SOURCE, expectedClientRevision: CLIENT } }]);
  assert.deepEqual(f.notices, ['已移出文字成员']);
  await f.open(); await f.toggle('image', true);
  assert.equal(f.writes()[1].body.mode, 'image'); assert.equal(f.writes()[1].body.enabled, true);
});

test('primary membership routes to explicit editor, never false removal', async () => {
  const f = fixture(); await f.open(f.primary.id);
  assert.match(f.html(), /更换主账号/); assert.match(f.html(), /选择替代账号/);
  await f.toggle('text', false); assert.equal(f.writes().length, 0);
  await f.edit(f.client.id); assert.deepEqual(f.editors, ['client_a']);
});

test('fallback, last explicit member, and single-account route require editor', async () => {
  for (const change of [
    c => { c.accountIds = []; c.poolStrategy = ''; },
    c => { c.accountIds = ['account_b']; },
    c => { c.poolStrategy = 'single_account'; c.poolAccountId = 'account_b'; },
  ]) {
    const f = fixture(); change(f.client); await f.open(); assert.match(f.html(), /文字设置/);
    await f.toggle('text', false); await f.toggle('text', true); assert.equal(f.writes().length, 0);
    await f.edit(f.client.id); assert.deepEqual(f.editors, ['client_a']);
  }
});

test('empty image fallback and last image member never silently change effective grants', async () => {
  for (const ids of [[], ['account_b']]) {
    const f = fixture(); f.client.imageAccountIds = ids; await f.open(); assert.match(f.html(), /图片设置/);
    await f.toggle('image', !ids.length); assert.equal(f.writes().length, 0);
  }
});

test('no clients offers explicit setup, opening does not create a key or enable service', async () => {
  const f = fixture(); f.state.clients = []; await f.open(); assert.match(f.html(), /配置客户端/);
  assert.equal(f.writes().length, 0); await f.edit(''); assert.deepEqual(f.editors, ['']);
});

test('read completed after switching account cannot replace newer dialog', async () => {
  const f = fixture(), pending = gate(); let first = true;
  f.state.intercept = path => path === '/api/local/codex-api' && first ? (first = false, pending.promise) : undefined;
  const old = f.open(); await f.open(f.primary.id); const newest = f.html();
  pending.resolve({ clients: [] }); await old; assert.equal(f.html(), newest); assert.match(f.html(), /Primary API/);
});

test('mutation completed after opening another account cannot close it or toast success', async () => {
  const f = fixture(), pending = gate(); await f.open();
  f.state.intercept = path => path.endsWith('/members') ? pending.promise : undefined;
  const saving = f.toggle('text', false); await new Promise(setImmediate);
  await f.open(f.primary.id); const node = f.nodes.get('#officialPanel');
  pending.resolve({ ok: true, clientId: 'client_a', accountId: 'account_b', mode: 'text', enabled: false }); await saving;
  assert.equal(f.nodes.get('#officialPanel'), node); assert.equal(f.notices.length, 0);
});

test('source replacement or changed client after display blocks stale writes', async () => {
  for (const mutate of [f => f.account.sourceRevision = 'src-v1-' + 'd'.repeat(64), f => f.client.disabled = true, f => f.client.revision = 'api-client-v1-' + 'e'.repeat(64), f => f.state.accounts = [f.primary]]) {
    const f = fixture(); await f.open(); mutate(f); await f.toggle('text', false);
    assert.equal(f.writes().length, 0); assert.match(f.nodes.get('#officialError').textContent, /已更改/);
  }
});

test('new card refuses missing or malformed revision instead of legacy mutation', async () => {
  for (const value of [undefined, '', 'raw-auth-epoch']) {
    const f = fixture(); f.account.sourceRevision = value; await assert.rejects(f.open(), /刷新/);
    assert.equal(f.writes().length, 0);
    const g = fixture(); g.client.revision = value; await g.open(); await g.toggle('text', false);
    assert.equal(g.writes().length, 0); assert.match(g.nodes.get('#officialError').textContent, /更新电脑端/);
  }
});

test('official account membership uses the same opaque source precondition', async () => {
  const f = fixture(); f.account.officialAccount = { id: 'protected-fixture', identityEpoch: 7 }; await f.open(); await f.toggle('text', false);
  assert.equal(f.writes()[0].body.expectedSourceRevision, SOURCE);
  assert.equal(f.writes()[0].body.expectedClientRevision, CLIENT);
});

test('server-side conflict after preflight never shows success or retries', async () => {
  const f = fixture(); await f.open();
  f.state.intercept = (path, body) => {
    if (!path.endsWith('/members')) return;
    assert.equal(body.expectedClientRevision, CLIENT); assert.equal(body.expectedSourceRevision, SOURCE);
    throw new Error('API 服务成员已变化，请重新打开');
  };
  await f.toggle('text', false); await f.toggle('text', false);
  assert.equal(f.writes().length, 1); assert.equal(f.notices.length, 0); assert.match(f.nodes.get('#officialError').textContent, /已变化/);
});

test('late preflight after panel close cannot issue a write', async () => {
  const f = fixture(), pending = gate(); await f.open();
  f.state.intercept = path => path === '/api/local/codex-api' ? pending.promise : undefined;
  const saving = f.toggle('text', false); f.context.closeOfficialPanel(); pending.resolve({ clients: [f.client] }); await saving;
  assert.equal(f.writes().length, 0);
});

test('duplicate clicks and ambiguous reply cannot retry from same snapshot', async () => {
  const f = fixture(), pending = gate(); await f.open();
  f.state.intercept = path => path.endsWith('/members') ? pending.promise : undefined;
  const saving = f.toggle('text', false); await new Promise(setImmediate); await f.toggle('text', false);
  assert.equal(f.writes().length, 1); pending.resolve({ ok: true }); await saving;
  await f.toggle('text', false); assert.equal(f.writes().length, 1); assert.equal(f.notices.length, 0);
  assert.match(f.nodes.get('#officialError').textContent, /结果待确认/);
});

test('unavailable account types cannot open and escaped labels stay inert', async () => {
  const f = fixture(); f.account.libraryOnly = true; await assert.rejects(f.open(), /刷新/);
  delete f.account.libraryOnly; f.account.name = '<script>unsafe</script>'; await f.open();
  assert.doesNotMatch(f.html(), /<script>/); assert.match(f.html(), /&lt;script>/);
});

test('Escape closes only the current nested membership dialog', async () => {
  const f = fixture(); let stopped = 0; await f.open();
  f.listeners.keydown({ key: 'Escape', preventDefault() {}, stopImmediatePropagation() { stopped++; } });
  assert.equal(stopped, 1); assert.equal(f.context.S.route, 'accounts'); assert.equal(f.nodes.size, 0);
});
