import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

const source = await readFile(new URL('../internal/console/web/codex-accounts.js', import.meta.url), 'utf8');
function deferred() {
  let resolve, reject;
  const promise = new Promise((done, fail) => { resolve = done; reject = fail; });
  return { promise, resolve, reject };
}
function harness(api) {
  const nodes = new Map(), timers = new Map(), calls = [];
  let timerID = 0;
  const node = id => {
    if (!nodes.has(id)) {
      const element = { value: '', textContent: '', dataset: {}, click() {}, focus() { this.focused = true; }, hidden: true, innerHTMLWrites: 0, _innerHTML: '' };
      Object.defineProperty(element, 'innerHTML', { get() { return this._innerHTML; }, set(value) { this._innerHTML = String(value); this.innerHTMLWrites++; } });
      if (id === '#modalRoot') element.querySelector = selector => {
        const shell = selector.match(/data-official-shell="(add|standard)"/)?.[1];
        return shell && element.dataset.officialShell === shell ? {} : null;
      };
      nodes.set(id, element);
    }
    return nodes.get(id);
  };
  const context = vm.createContext({
    URL, TextEncoder, ACTIONS: {}, S: { route: 'other' }, $: node, icon: () => '', esc: value => String(value),
    api: async (path, body) => { calls.push({ path, body }); return api(path, body); },
    busy: async (_button, action) => action(), copyText: async () => {}, renderLocalCards() {},
    setTimeout: (action, delay) => { const id = ++timerID; timers.set(id, { action, delay }); return id; },
    clearTimeout: id => timers.delete(id),
    MutationObserver: class { observe() {} }, document: { addEventListener() {} },
  });
  vm.runInContext(source, context);
  const run = code => vm.runInContext(code, context);
  const retry = async () => {
    assert.equal(timers.size, 1, 'expected one current retry or status poll');
    const [id, timer] = timers.entries().next().value;
    timers.delete(id);
    await timer.action();
  };
  return { context, node, calls, timers, run, retry };
}
function pending(id, mode = 'browser') {
  return mode === 'device'
    ? { id, mode, status: 'pending', verificationURL: 'https://auth.openai.com/codex/device', userCode: 'SYNTHETIC-CODE' }
    : { id, mode, status: 'pending', authorizationURL: 'https://auth.openai.com/oauth/authorize?state=synthetic' };
}
async function openOldLogin(h) {
  await h.context.openOfficialAccounts();
  h.run("OFFICIAL_PANEL.session = 'old-login'");
  return h.context.pollOfficialLogin('old-login', h.run('OFFICIAL_PANEL.epoch'));
}

for (const mode of ['browser', 'device']) {
  test(`old completed refresh cannot close or cancel a newer ${mode} authorization`, async () => {
    const refresh = deferred(), entered = deferred();
    const h = harness(async (path, body) => {
      if (path === '/api/local/accounts') { entered.resolve(); return refresh.promise; }
      if (path.endsWith('/login')) return pending('new-login', mode);
      if (path.endsWith('/status')) return body.id === 'old-login' ? { id: body.id, status: 'complete', account: { id: 'saved' } } : pending(body.id, mode);
      if (path.endsWith('/cancel')) return { ok: true };
      throw Error(path);
    });
    const old = openOldLogin(h);
    await entered.promise;
    await h.context.ACTIONS.officialLogin({ dataset: { mode } });
    assert.equal(h.run('OFFICIAL_PANEL.session'), 'new-login');
    refresh.resolve({ accounts: [{ id: 'saved' }] });
    await old;
    assert.equal(h.run('OFFICIAL_PANEL.session'), 'new-login');
    assert.notEqual(h.node('#modalRoot').innerHTML, '');
    assert.equal(h.calls.filter(call => call.path.endsWith('/cancel')).length, 0);
  });
}

test('new authorization is fenced as soon as its start request begins', async () => {
  const refresh = deferred(), entered = deferred(), started = deferred(), startEntered = deferred();
  const h = harness(async (path, body) => {
    if (path === '/api/local/accounts') { entered.resolve(); return refresh.promise; }
    if (path.endsWith('/login')) { startEntered.resolve(); return started.promise; }
    if (path.endsWith('/status')) return body.id === 'old-login' ? { id: body.id, status: 'complete', account: { id: 'saved' } } : pending(body.id, 'device');
    if (path.endsWith('/cancel')) return { ok: true };
    throw Error(path);
  });
  const old = openOldLogin(h);
  await entered.promise;
  const next = h.context.ACTIONS.officialLogin({ dataset: { mode: 'device' } });
  await startEntered.promise;
  refresh.resolve({ accounts: [{ id: 'saved' }] });
  await old;
  started.resolve(pending('new-login', 'device'));
  await next;
  assert.equal(h.run('OFFICIAL_PANEL.session'), 'new-login');
  assert.notEqual(h.node('#modalRoot').innerHTML, '');
  assert.equal(h.calls.filter(call => call.path.endsWith('/cancel')).length, 0);
});

test('switching Codex import tabs reuses the open dialog shell', async () => {
  const h = harness(async () => ({}));
  await h.context.openOfficialAccounts();
  const root = h.node('#modalRoot'), writes = root.innerHTMLWrites;
  assert.equal(root.dataset.officialShell, 'add');
  await h.context.ACTIONS.officialTab({ dataset: { tab: 'token' } });
  assert.equal(root.innerHTMLWrites, writes, 'token view must not recreate the overlay or dialog');
  assert.match(h.node('#officialPanel').innerHTML, /officialPasteText/);
  await h.context.ACTIONS.officialTab({ dataset: { tab: 'file' } });
  assert.equal(root.innerHTMLWrites, writes, 'file view must keep the same dialog shell');
  assert.match(h.node('#officialPanel').innerHTML, /officialFile/);
});

test('failed authorization start stays visible in the dialog and restores both choices', async () => {
  const h = harness(async path => {
    if (path.endsWith('/login')) throw Error('synthetic network connection diagnostic');
    throw Error(path);
  });
  await h.context.openOfficialAccounts();
  await h.context.ACTIONS.officialLogin({ dataset: { mode: 'browser' } });
  assert.match(h.node('#officialLoginState').innerHTML, /授权启动失败/);
  assert.match(h.node('#officialLoginState').innerHTML, /无法连接授权服务/);
  assert.doesNotMatch(h.node('#officialLoginState').innerHTML, /synthetic network/);
  assert.equal(h.run('OFFICIAL_PANEL.session'), null);
  assert.equal(h.run('OFFICIAL_PANEL.loginPending'), false);
  assert.equal(h.node('#officialBrowserLogin').disabled, false);
  assert.equal(h.node('#officialDeviceLogin').disabled, false);
});

test('completed authorization retries a failed account-list refresh without authorizing again', async () => {
  let reads = 0;
  const h = harness(async path => {
    if (path.endsWith('/status')) return { id: 'old-login', status: 'complete', account: { id: 'saved' } };
    if (path === '/api/local/accounts') {
      if (++reads === 1) throw Error('synthetic temporary list failure');
      return { accounts: [{ id: 'saved' }] };
    }
    throw Error(path);
  });
  await openOldLogin(h);
  assert.match(h.node('#officialError').textContent, /已登录.*刷新.*重试/);
  assert.equal(h.run('OFFICIAL_PANEL.session'), null, 'a new authorization may still be started');
  await h.retry();
  assert.equal(reads, 2);
  assert.equal(h.node('#modalRoot').innerHTML, '');
  assert.equal(h.calls.filter(call => call.path.endsWith('/login')).length, 0);
});

test('an already queued obsolete status poll cannot invalidate completion refresh', async () => {
  const refresh = deferred(), entered = deferred();
  let statuses = 0;
  const h = harness(async path => {
    if (path.endsWith('/status')) { statuses++; return { id: 'old-login', status: 'complete', account: { id: 'saved' } }; }
    if (path === '/api/local/accounts') { entered.resolve(); return refresh.promise; }
    throw Error(path);
  });
  const old = openOldLogin(h);
  await entered.promise;
  await h.context.pollOfficialLogin('old-login', h.run('OFFICIAL_PANEL.epoch'));
  refresh.resolve({ accounts: [{ id: 'saved' }] });
  await old;
  assert.equal(statuses, 1);
  assert.equal(h.node('#modalRoot').innerHTML, '');
});

test('a late completed-refresh failure cannot write an error or retry over a new authorization', async () => {
  const refresh = deferred(), entered = deferred();
  const h = harness(async (path, body) => {
    if (path === '/api/local/accounts') { entered.resolve(); return refresh.promise; }
    if (path.endsWith('/login')) return pending('new-login', 'device');
    if (path.endsWith('/status')) return body.id === 'old-login' ? { id: body.id, status: 'complete', account: { id: 'saved' } } : pending(body.id, 'device');
    throw Error(path);
  });
  const old = openOldLogin(h);
  await entered.promise;
  await h.context.ACTIONS.officialLogin({ dataset: { mode: 'device' } });
  refresh.reject(Error('synthetic old list failure'));
  await old;
  assert.equal(h.run('OFFICIAL_PANEL.session'), 'new-login');
  assert.equal(h.node('#officialError').textContent, '');
  assert.equal(h.timers.size, 1);
  assert.equal([...h.timers.values()][0].delay, 1500, 'only the new session status poll remains');
});

for (const failure of [false, true]) {
  test(`closed-and-reopened panel ignores old completed refresh ${failure ? 'failure' : 'success'}`, async () => {
    const refresh = deferred(), entered = deferred();
    const h = harness(async path => {
      if (path === '/api/local/accounts') { entered.resolve(); return refresh.promise; }
      if (path.endsWith('/status')) return { id: 'old-login', status: 'complete', account: { id: 'saved' } };
      if (path.endsWith('/cancel')) return { ok: true };
      throw Error(path);
    });
    const old = openOldLogin(h);
    await entered.promise;
    h.context.closeOfficialPanel();
    await h.context.openOfficialAccounts();
    const epoch = h.run('OFFICIAL_PANEL.epoch');
    h.node('#officialError').textContent = 'new panel marker';
    if (failure) refresh.reject(Error('synthetic old list failure'));
    else refresh.resolve({ accounts: [{ id: 'saved' }] });
    await old;
    assert.equal(h.run('OFFICIAL_PANEL.epoch'), epoch);
    assert.notEqual(h.node('#modalRoot').innerHTML, '');
    assert.equal(h.node('#officialError').textContent, 'new panel marker');
    assert.equal(h.timers.size, 0);
  });
}

for (const complete of [false, true]) {
  test(`cancel checks the real terminal state when ${complete ? 'completion' : 'cancellation'} wins`, async () => {
    const h = harness(async path => {
      if (path.endsWith('/cancel')) return { ok: true };
      if (path.endsWith('/status')) return complete ? { id: 'old-login', status: 'complete', account: { id: 'saved' } } : { id: 'old-login', status: 'cancelled' };
      if (path === '/api/local/accounts') return { accounts: [{ id: 'saved' }] };
      throw Error(path);
    });
    await h.context.openOfficialAccounts();
    h.run("OFFICIAL_PANEL.session = 'old-login'");
    await h.context.ACTIONS.officialLoginCancel({});
    assert.equal(h.calls.filter(call => call.path.endsWith('/status')).length, 1);
    assert.equal(h.run('OFFICIAL_PANEL.session'), null);
    if (complete) {
      assert.equal(h.context.S.local.accounts[0].id, 'saved');
      assert.equal(h.node('#modalRoot').innerHTML, '');
    } else {
      assert.match(h.node('#officialLoginState').innerHTML, /已取消/);
      assert.equal(h.calls.filter(call => call.path === '/api/local/accounts').length, 0);
    }
  });
}

test('cancel retries a temporary status failure on the same session', async () => {
  let statuses = 0;
  const h = harness(async path => {
    if (path.endsWith('/cancel')) return { ok: true };
    if (path.endsWith('/status')) {
      statuses++;
      if (statuses === 1) return pending('old-login', 'device');
      if (statuses === 2) throw Error('synthetic temporary status failure');
      return { id: 'old-login', status: 'cancelled' };
    }
    throw Error(path);
  });
  await h.context.openOfficialAccounts();
  h.run("OFFICIAL_PANEL.session = 'old-login'");
  await h.context.pollOfficialLogin('old-login', h.run('OFFICIAL_PANEL.epoch'));
  assert.match(h.node('#officialLoginState').innerHTML, /href=|SYNTHETIC-CODE/);
  await h.context.ACTIONS.officialLoginCancel({});
  assert.equal(h.run('OFFICIAL_PANEL.session'), 'old-login');
  assert.match(h.node('#officialLoginState').textContent, /正在确认取消结果/);
  assert.doesNotMatch(h.node('#officialLoginState').textContent, /href=|SYNTHETIC-CODE|officialAuthorizationURL/);
  assert.match(h.node('#officialError').textContent, /重试/);
  await h.retry();
  assert.equal(h.run('OFFICIAL_PANEL.session'), null);
  assert.match(h.node('#officialLoginState').innerHTML, /已取消/);
  assert.equal(h.calls.filter(call => call.path.endsWith('/cancel')).length, 1);
});

test('a pre-cancel status response cannot replace the confirmed cancelled state', async () => {
  const oldStatus = deferred();
  let statuses = 0;
  const h = harness(async path => {
    if (path.endsWith('/cancel')) return { ok: true };
    if (path.endsWith('/status')) return ++statuses === 1 ? oldStatus.promise : { id: 'old-login', status: 'cancelled' };
    throw Error(path);
  });
  await h.context.openOfficialAccounts();
  h.run("OFFICIAL_PANEL.session = 'old-login'");
  const old = h.context.pollOfficialLogin('old-login', h.run('OFFICIAL_PANEL.epoch'));
  await h.context.ACTIONS.officialLoginCancel({});
  oldStatus.resolve(pending('old-login'));
  await old;
  assert.equal(statuses, 2);
  assert.equal(h.run('OFFICIAL_PANEL.session'), null);
  assert.match(h.node('#officialLoginState').innerHTML, /已取消/);
  assert.equal(h.timers.size, 0);
});
