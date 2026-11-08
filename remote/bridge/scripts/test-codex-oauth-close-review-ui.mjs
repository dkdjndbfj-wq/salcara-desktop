import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { setImmediate as flush } from 'node:timers/promises';
import vm from 'node:vm';

const source = await readFile(new URL('../internal/console/web/codex-accounts.js', import.meta.url), 'utf8');
function deferred() {
  let resolve;
  const promise = new Promise(done => { resolve = done; });
  return { promise, resolve };
}
function harness(api) {
  const nodes = new Map(), timers = new Map(), calls = [];
  let timerID = 0;
  const node = id => {
    if (!nodes.has(id)) nodes.set(id, { value: '', innerHTML: '', textContent: '', dataset: {}, click() {}, hidden: true });
    return nodes.get(id);
  };
  const context = vm.createContext({
    URL, TextEncoder, ACTIONS: {}, S: { route: 'other' }, $: node, icon: () => '', esc: value => String(value),
    api: async (path, body) => { calls.push({ path, body }); return api(path, body); },
    busy: async (_button, action) => action(), copyText: async () => {}, renderLocalCards() {}, confirm: () => true,
    setTimeout: (action, delay) => { const id = ++timerID; timers.set(id, { action, delay }); return id; },
    clearTimeout: id => timers.delete(id),
    MutationObserver: class { observe() {} }, document: { addEventListener() {} },
  });
  vm.runInContext(source, context);
  const retry = async () => {
    assert.equal(timers.size, 1, 'expected one detached retry');
    const [id, timer] = timers.entries().next().value;
    timers.delete(id);
    timer.action();
    await flush();
  };
  return { context, node, calls, timers, retry, run: code => vm.runInContext(code, context) };
}
const complete = id => ({ id, status: 'complete', account: { id: 'saved-account', identityEpoch: 'identity-one' } });

test('closing after authorization completes still reconciles the saved account with the API list', async () => {
  let attached = false;
  const h = harness(async (path, body) => {
    if (path.endsWith('/cancel')) return { ok: true };
    if (path.endsWith('/status')) { attached = true; return complete(body.id); }
    if (path === '/api/local/accounts') return { accounts: attached ? [{ id: 'official-saved-account' }] : [] };
    throw Error(path);
  });
  await h.context.openOfficialAccounts();
  h.run("OFFICIAL_PANEL.session = 'completed-login'");
  h.context.closeOfficialPanel();
  await flush();
  assert.equal(h.node('#modalRoot').innerHTML, '');
  assert.equal(h.run('OFFICIAL_PANEL.session'), null);
  assert.equal(attached, true, 'completion that won just before close must still be attached');
  assert.equal(h.context.S.local?.accounts[0]?.id, 'official-saved-account');
});

test('closed authorization reconciliation cannot close or overwrite a newly opened panel', async () => {
  const cancellation = deferred();
  const h = harness(async (path, body) => {
    if (path.endsWith('/cancel')) return cancellation.promise;
    if (path.endsWith('/status')) return complete(body.id);
    if (path === '/api/local/accounts') return { accounts: [{ id: 'official-saved-account' }] };
    throw Error(path);
  });
  await h.context.openOfficialAccounts();
  h.run("OFFICIAL_PANEL.session = 'completed-login'");
  h.context.closeOfficialPanel();
  await h.context.openOfficialAccounts();
  const epoch = h.run('OFFICIAL_PANEL.epoch');
  h.run("OFFICIAL_PANEL.session = 'newer-login'; OFFICIAL_PANEL.challenge = { id: 'newer-login' }; OFFICIAL_PANEL.timer = setTimeout(() => {}, 1500)");
  h.node('#officialError').textContent = 'new panel marker';
  cancellation.resolve({ ok: true });
  await flush();
  assert.equal(h.context.S.local?.accounts[0]?.id, 'official-saved-account');
  assert.equal(h.run('OFFICIAL_PANEL.epoch'), epoch);
  assert.equal(h.run('OFFICIAL_PANEL.session'), 'newer-login');
  assert.equal(h.run('OFFICIAL_PANEL.challenge.id'), 'newer-login');
  assert.notEqual(h.node('#modalRoot').innerHTML, '');
  assert.equal(h.node('#officialError').textContent, 'new panel marker');
  assert.equal(h.timers.size, 1, 'the new login keeps its poll timer');
  assert.equal(h.calls.filter(call => call.path.endsWith('/cancel') && call.body.id === 'newer-login').length, 0);
});

test('a start response arriving after the panel closes reconciles a login that already completed', async () => {
  const started = deferred();
  let attached = false;
  const h = harness(async (path, body) => {
    if (path.endsWith('/login')) return started.promise;
    if (path.endsWith('/cancel')) return { ok: true };
    if (path.endsWith('/status')) { attached = true; return complete(body.id); }
    if (path === '/api/local/accounts') return { accounts: attached ? [{ id: 'official-saved-account' }] : [] };
    throw Error(path);
  });
  await h.context.openOfficialAccounts();
  const login = h.context.ACTIONS.officialLogin({ dataset: { mode: 'device' } });
  h.context.closeOfficialPanel();
  started.resolve({ id: 'late-login', mode: 'device', status: 'pending' });
  await login;
  await flush();
  assert.equal(attached, true, 'the delayed start response must not strand a completed login');
  assert.equal(h.context.S.local?.accounts[0]?.id, 'official-saved-account');
  assert.equal(h.node('#modalRoot').innerHTML, '');
});

test('closing a pending authorization that cancellation wins does not refresh or attach an account', async () => {
  const h = harness(async (path, body) => {
    if (path.endsWith('/cancel')) return { ok: true };
    if (path.endsWith('/status')) return { id: body.id, status: 'cancelled' };
    throw Error(path);
  });
  await h.context.openOfficialAccounts();
  h.run("OFFICIAL_PANEL.session = 'cancelled-login'");
  h.context.closeOfficialPanel();
  await flush();
  assert.equal(h.calls.filter(call => call.path.endsWith('/status')).length, 1);
  assert.equal(h.calls.filter(call => call.path === '/api/local/accounts').length, 0);
  assert.equal(h.timers.size, 0);
  assert.equal(h.node('#modalRoot').innerHTML, '');
});

for (const action of ['deleted', 'replaced']) {
  test(`closed completion cannot resurrect an account ${action} before the terminal status check`, async () => {
    const h = harness(async path => {
      if (path.endsWith('/cancel')) return { ok: true };
      if (path.endsWith('/status')) throw Error('账号已移除或变更，请刷新列表');
      throw Error(path);
    });
    const accounts = action === 'deleted' ? [] : [{ id: 'official-saved-account', identityEpoch: 'new-identity' }];
    h.context.S.local = { accounts };
    await h.context.openOfficialAccounts();
    h.run("OFFICIAL_PANEL.session = 'completed-login'");
    h.context.closeOfficialPanel();
    await h.context.openOfficialAccounts();
    h.node('#officialError').textContent = 'new panel marker';
    await flush();
    assert.deepEqual(h.context.S.local.accounts, accounts);
    assert.equal(h.calls.filter(call => call.path === '/api/local/accounts' || call.path.endsWith('/attach')).length, 0);
    assert.equal(h.timers.size, 0, 'an identity conflict is terminal');
    assert.equal(h.node('#officialError').textContent, 'new panel marker');
  });
}

test('a delayed list read from closed-login completion cannot restore a subsequently deleted card', async () => {
  const oldList = deferred(), listEntered = deferred();
  let listReads = 0;
  const h = harness(async (path, body) => {
    if (path.endsWith('/cancel')) return { ok: true };
    if (path.endsWith('/status')) return complete(body.id);
    if (path.endsWith('/delete')) return { ok: true };
    if (path === '/api/local/accounts') {
      if (++listReads === 1) { listEntered.resolve(); return oldList.promise; }
      return { accounts: [] };
    }
    throw Error(path);
  });
  await h.context.openOfficialAccounts();
  h.run("OFFICIAL_PANEL.session = 'completed-login'");
  h.context.closeOfficialPanel();
  await listEntered.promise;
  await h.context.ACTIONS.officialDelete({ dataset: { id: 'saved-account' } });
  assert.equal(h.context.S.local.accounts.length, 0);
  oldList.resolve({ accounts: [{ id: 'official-saved-account' }] });
  await flush();
  assert.equal(h.context.S.local.accounts.length, 0);
  assert.equal(h.timers.size, 0);
});

for (const failedPath of ['/api/local/codex/login/cancel', '/api/local/codex/login/status', '/api/local/accounts']) {
  test(`closed completion retries a temporary ${failedPath} failure without owning a new panel`, async () => {
    let failed = false;
    const h = harness(async (path, body) => {
      if (path === failedPath && !failed) { failed = true; throw Error('synthetic temporary failure'); }
      if (path.endsWith('/cancel')) return { ok: true };
      if (path.endsWith('/status')) return complete(body.id);
      if (path === '/api/local/accounts') return { accounts: [{ id: 'official-saved-account' }] };
      throw Error(path);
    });
    await h.context.openOfficialAccounts();
    h.run("OFFICIAL_PANEL.session = 'completed-login'");
    h.context.closeOfficialPanel();
    await h.context.openOfficialAccounts();
    const epoch = h.run('OFFICIAL_PANEL.epoch');
    h.node('#officialError').textContent = 'new panel marker';
    await flush();
    assert.equal(h.node('#officialError').textContent, 'new panel marker');
    await h.retry();
    assert.equal(h.context.S.local?.accounts[0]?.id, 'official-saved-account');
    assert.equal(h.run('OFFICIAL_PANEL.epoch'), epoch);
    assert.notEqual(h.node('#modalRoot').innerHTML, '');
    assert.equal(h.node('#officialError').textContent, 'new panel marker');
    assert.equal(h.timers.size, 0);
  });
}

test('detached reconciliation has a bounded retry count during a persistent transport outage', async () => {
  const h = harness(async () => { throw Error('synthetic persistent transport failure'); });
  await h.context.openOfficialAccounts();
  h.run("OFFICIAL_PANEL.session = 'closed-login'");
  h.context.closeOfficialPanel();
  await flush();
  await h.retry();
  await h.retry();
  assert.equal(h.timers.size, 0);
  assert.equal(h.calls.length, 3);
  assert.equal(h.node('#modalRoot').innerHTML, '');
});
