import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';

const source = fs.readFileSync(new URL('../internal/console/web/account-model-rules.js', import.meta.url), 'utf8');
const SOURCE = 'src-v1-' + 'a'.repeat(64), NEXT = 'src-v1-' + 'b'.repeat(64);
const plain = value => JSON.parse(JSON.stringify(value));
const decode = value => value.replaceAll('&quot;', '"').replaceAll('&lt;', '<').replaceAll('&gt;', '>').replaceAll('&amp;', '&');
const esc = value => String(value).replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;').replaceAll('"', '&quot;');
function gate() { let resolve, reject; return { promise: new Promise((r, j) => { resolve = r; reject = j; }), resolve, reject }; }
function fixture() {
  const nodes = new Map(), calls = [], notices = [], listeners = {};
  const account = { id: 'fixture-api', name: 'Fixture API', sourceRevision: SOURCE, modelAllowlist: ['model-a'], modelDenylist: ['model-b'] };
  const other = { id: 'fixture-official', name: 'Official account', sourceType: 'official_account', officialAccount: { id: 'vault-one', mode: 'oauth' }, sourceRevision: SOURCE, modelAllowlist: [], modelDenylist: [] };
  const state = { accounts: [account, other], intercept: null, refreshes: 0, sequence: 0 };
  function node(id) {
    const element = { value: '', textContent: '', disabled: false, isConnected: true, focus() {}, _html: '' };
    Object.defineProperty(element, 'innerHTML', {
      get() { return this._html; },
      set(html) {
        this._html = html;
        for (const match of html.matchAll(/<textarea[^>]+id="([^"]+)"[^>]*>([\s\S]*?)<\/textarea>/g)) {
          const child = node('#' + match[1]); child.value = decode(match[2]);
        }
        for (const match of html.matchAll(/<button[^>]+id="([^"]+)"/g)) node('#' + match[1]);
      },
    });
    nodes.set(id, element); return element;
  }
  const context = vm.createContext({
    ACTIONS: {}, OFFICIAL_PANEL: { epoch: 0 }, S: { route: 'accounts', local: { accounts: state.accounts } },
    $: id => nodes.get(id), esc, TextEncoder,
    document: { addEventListener(name, fn) { listeners[name] = fn; } },
    officialPanel(_title, html, footer) {
      context.OFFICIAL_PANEL.epoch++; nodes.clear();
      node('#officialPanel').innerHTML = footer || ''; node('#officialError'); node('#accountModelRulesContent').innerHTML = html;
      if (nodes.has('#accountModelRulesReload')) nodes.get('#accountModelRulesReload').hidden = true;
    },
    closeOfficialPanel() { context.OFFICIAL_PANEL.epoch++; nodes.clear(); },
    toast: message => notices.push(message),
    officialMutationToken: () => ({ epoch: context.OFFICIAL_PANEL.epoch, sequence: ++state.sequence }),
    officialMutationCurrent: token => token.epoch === context.OFFICIAL_PANEL.epoch && token.sequence === state.sequence,
    refreshOfficialCards: async () => { state.refreshes++; return true; },
    api: async (path, body) => {
      calls.push({ path, ...(body ? { body: plain(body) } : {}) });
      if (state.intercept) { const value = state.intercept(path, body); if (value !== undefined) return value; }
      if (path === '/api/local/accounts') return plain({ accounts: state.accounts });
      if (path === '/api/local/accounts/model-rules') {
        const current = state.accounts.find(row => row.id === body.id);
        if (current.sourceRevision !== body.expectedSourceRevision) throw new Error('账号来源已变化，请重新打开并确认');
        Object.assign(current, plain(body), { sourceRevision: NEXT });
        return { ok: true, id: body.id, sourceRevision: NEXT, modelAllowlist: body.modelAllowlist, modelDenylist: body.modelDenylist, modelsInvalidated: true };
      }
      throw new Error('unexpected request');
    },
  });
  vm.runInContext(source, context);
  return { context, nodes, calls, notices, listeners, state, account, other,
    open: (id = account.id) => context.ACTIONS.accountModelRules({ dataset: { id } }),
    save: (button = nodes.get('#accountModelRulesSave')) => context.ACTIONS.accountModelRulesSave(button),
    reload: () => context.ACTIONS.accountModelRulesReload(nodes.get('#accountModelRulesReload')),
    writes: () => calls.filter(call => call.body),
    html: () => nodes.get('#accountModelRulesContent')?.innerHTML,
    error: () => nodes.get('#officialError')?.textContent,
    values: (allow, deny) => { nodes.get('#accountModelAllow').value = allow; nodes.get('#accountModelDeny').value = deny; },
  };
}

test('opening either account type loads its saved policy without mutation', async () => {
  const f = fixture();
  for (const account of [f.account, f.other]) {
    await f.open(account.id);
    assert.match(f.html(), new RegExp(account.name));
    assert.equal(f.nodes.get('#accountModelAllow').value, account.modelAllowlist.join('\n'));
    assert.equal(f.nodes.get('#accountModelDeny').value, account.modelDenylist.join('\n'));
    assert.equal(f.writes().length, 0);
  }
});

test('save sends only normalized rules and the exact source precondition', async () => {
  const f = fixture(); await f.open(f.other.id);
  f.values(' model-a \r\nmodel-b\nmodel-a\n\n', 'model-b');
  await f.save();
  assert.deepEqual(f.writes(), [{ path: '/api/local/accounts/model-rules', body: {
    id: f.other.id, expectedSourceRevision: SOURCE, modelAllowlist: ['model-a', 'model-b'], modelDenylist: ['model-b'],
  } }]);
  assert.equal(f.other.sourceType, 'official_account');
  assert.deepEqual(f.notices, ['模型规则已保存']); assert.equal(f.nodes.size, 0); assert.equal(f.state.refreshes, 1);
});

test('empty input explicitly clears both lists', async () => {
  const f = fixture(); await f.open(); f.values(' \n ', '');
  await f.save();
  assert.deepEqual(f.writes()[0].body.modelAllowlist, []);
  assert.deepEqual(f.writes()[0].body.modelDenylist, []);
});

test('invalid UTF-8 length, too many rules and NUL remain editable with no write', async () => {
  for (const value of ['a'.repeat(201), '模'.repeat(67), Array(1001).fill('model').join('\n'), 'model\u0000x']) {
    const f = fixture(); await f.open(); f.values(value, '');
    await f.save();
    assert.match(f.error(), /1000|200/); assert.equal(f.writes().length, 0);
    assert.equal(f.nodes.get('#accountModelAllow').disabled, false);
    f.values('valid', ''); await f.save(); assert.equal(f.writes().length, 1);
  }
});

test('same-ID replacement before the read cannot inherit the old card confirmation', async () => {
  const f = fixture();
  f.state.intercept = path => path === '/api/local/accounts' ? { accounts: [{ ...f.account, sourceRevision: NEXT }] } : undefined;
  await f.open(); await f.save();
  assert.match(f.error(), /账号已更改/); assert.equal(f.writes().length, 0);
});

test('unsupported rows and missing source revision cannot open an editor', async () => {
  for (const patch of [{ libraryOnly: true }, { officialAccount: { mode: 'chatgpt_web_session' } }, { sourceRevision: '' }]) {
    const f = fixture(); Object.assign(f.account, patch);
    await assert.rejects(f.open(), /刷新/); assert.equal(f.calls.length, 0);
  }
});

test('late read after changing account cannot replace the newer editor', async () => {
  const f = fixture(), pending = gate(); let first = true;
  f.state.intercept = path => path === '/api/local/accounts' && first ? (first = false, pending.promise) : undefined;
  const opening = f.open(); await f.open(f.other.id); const latest = f.html();
  pending.resolve({ accounts: [f.account] }); await opening;
  assert.equal(f.html(), latest); assert.match(f.html(), /Official account/);
});

test('late read after closing the modal cannot create controls or write', async () => {
  const f = fixture(), pending = gate();
  f.state.intercept = () => pending.promise;
  const opening = f.open(); f.context.closeOfficialPanel();
  pending.resolve({ accounts: [f.account] }); await opening; await f.save();
  assert.equal(f.nodes.size, 0); assert.equal(f.writes().length, 0);
});

test('duplicate saves and ambiguous responses cannot resend the same snapshot', async () => {
  const f = fixture(), pending = gate(); await f.open();
  f.state.intercept = path => path.endsWith('/model-rules') ? pending.promise : undefined;
  const saving = f.save(); await f.save();
  assert.equal(f.writes().length, 1); assert.equal(f.nodes.get('#accountModelAllow').disabled, true);
  pending.resolve({ ok: true }); await saving; await f.save();
  assert.equal(f.writes().length, 1); assert.match(f.error(), /结果待确认/);
  assert.equal(f.state.refreshes, 0); assert.equal(f.notices.length, 0);
});

test('stale source conflict cannot retry or report success', async () => {
  const f = fixture(); await f.open(); f.account.sourceRevision = NEXT;
  await f.save(); await f.save();
  assert.equal(f.writes().length, 1); assert.match(f.error(), /来源已变化/);
  assert.equal(f.notices.length, 0);
});

test('failed or ambiguous save offers a fresh read and never automatically retries the write', async () => {
  const f = fixture(); await f.open();
  f.state.intercept = path => path.endsWith('/model-rules') ? { ok: true } : undefined;
  await f.save();
  assert.equal(f.nodes.get('#accountModelRulesReload').hidden, false);
  f.account.sourceRevision = NEXT; f.account.modelAllowlist = ['server-current'];
  f.state.intercept = null;
  await f.reload();
  assert.equal(f.writes().length, 1);
  assert.equal(f.nodes.get('#accountModelAllow').value, 'server-current');
  assert.equal(f.nodes.get('#accountModelAllow').disabled, false);
  f.values('new-model', ''); await f.save();
  assert.equal(f.writes().length, 2);
  assert.equal(f.writes()[1].body.expectedSourceRevision, NEXT);
});

test('late recovery read cannot reopen an editor over another dialog', async () => {
  const f = fixture(), pending = gate(); await f.open();
  f.state.intercept = path => path.endsWith('/model-rules') ? { ok: true } : undefined;
  await f.save();
  f.context.refreshOfficialCards = () => pending.promise;
  const reloading = f.reload(); await f.open(f.other.id); const node = f.nodes.get('#officialPanel');
  pending.resolve(true); await reloading;
  assert.equal(f.nodes.get('#officialPanel'), node); assert.match(f.html(), /Official account/);
  assert.equal(f.writes().length, 1);
});

test('late save success and error do not close or repaint a new account dialog', async () => {
  for (const fails of [false, true]) {
    const f = fixture(), pending = gate(); await f.open();
    f.state.intercept = path => path.endsWith('/model-rules') ? pending.promise : undefined;
    const saving = f.save(); await f.open(f.other.id); const element = f.nodes.get('#officialPanel'), html = f.html();
    if (fails) pending.reject(new Error('old failure'));
    else pending.resolve({ ok: true, id: f.account.id, sourceRevision: NEXT, modelAllowlist: ['model-a'], modelDenylist: ['model-b'] });
    await saving;
    assert.equal(f.nodes.get('#officialPanel'), element); assert.equal(f.html(), html);
    assert.equal(f.error(), ''); assert.equal(f.notices.length, 0); assert.equal(f.state.refreshes, 0);
  }
});

test('mismatched save identity, policy or revision never claims success', async () => {
  for (const patch of [{ id: 'other-id' }, { modelDenylist: [] }, { sourceRevision: '' }, { ok: false }]) {
    const f = fixture(); await f.open();
    f.state.intercept = path => path.endsWith('/model-rules') ? { ok: true, id: f.account.id, sourceRevision: NEXT, modelAllowlist: ['model-a'], modelDenylist: ['model-b'], ...patch } : undefined;
    await f.save(); assert.match(f.error(), /结果待确认/);
    assert.equal(f.notices.length, 0); assert.equal(f.state.refreshes, 0);
  }
});

test('old button, replaced fields and route change cannot dispatch a write', async () => {
  for (const change of [
    f => ({ ...f.nodes.get('#accountModelRulesSave'), isConnected: false }),
    f => ({ ...f.nodes.get('#accountModelRulesSave') }),
    f => { f.nodes.set('#accountModelAllow', { value: 'injected' }); },
    f => { f.context.S.route = 'other'; },
  ]) {
    const f = fixture(); await f.open(); const button = change(f);
    await f.save(button); assert.equal(f.writes().length, 0);
  }
});

test('names and rules are escaped in the compact existing dialog', async () => {
  const f = fixture(); f.account.name = '<img src=x>'; f.account.modelAllowlist = ['</textarea><script>alert(1)</script>'];
  await f.open(); assert.doesNotMatch(f.html(), /<script>|<img/);
  assert.match(f.html(), /&lt;img/); assert.equal(f.nodes.get('#accountModelAllow').value, f.account.modelAllowlist[0]);
});

test('Escape closes only the current nested dialog', async () => {
  const f = fixture(); let stops = 0; await f.open();
  f.listeners.keydown({ key: 'Escape', preventDefault() {}, stopImmediatePropagation() { stops++; } });
  assert.equal(stops, 1); assert.equal(f.nodes.size, 0); assert.equal(f.context.S.route, 'accounts');
});

test('official cards expose rules only for linked executable accounts', () => {
  const file = fs.readFileSync(new URL('../internal/console/web/codex-accounts.js', import.meta.url), 'utf8');
  const start = file.indexOf('function officialAccountCard('), end = file.indexOf('\nasync function reconcileClosedOfficialLogin', start);
  const context = vm.createContext({
    esc, icon: () => '', officialAPIServiceSummary: () => ({ action: '管理 API 服务' }), officialCardAction: () => '',
    officialStatusTone: () => '', officialPlanBadge: () => '', officialAccountStatusLabel: () => '',
    officialQuotaHTML: () => '', officialUsageDetails: () => '', officialSubscriptionSummary: () => '',
    officialUsageTime: () => '', officialResetCreditButton: () => '', officialSubscriptionCardHTML: () => '', officialUpdatedHTML: () => '',
  });
  vm.runInContext(file.slice(start, end), context);
  const account = { id: 'official-linked', name: 'Official', officialAccount: { id: 'vault-one' } };
  assert.match(context.officialAccountCard(account, [], body => body), /data-act="accountModelRules" data-id="official-linked"/);
  for (const row of [{ ...account, libraryOnly: true }, { ...account, officialAccount: { id: 'vault-one', mode: 'chatgpt_web_session' } }]) {
    assert.doesNotMatch(context.officialAccountCard(row, [], body => body), /accountModelRules/);
  }
});
