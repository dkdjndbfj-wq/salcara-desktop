import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

const source = await readFile(new URL('../internal/console/web/codex-accounts.js', import.meta.url), 'utf8');
function deferred() {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}
function harness(api) {
  const nodes = new Map();
  const node = id => {
    if (!nodes.has(id)) nodes.set(id, { value: '', innerHTML: '', textContent: '', dataset: {}, hidden: true });
    return nodes.get(id);
  };
  const context = vm.createContext({
    URL, TextEncoder, api, $: node, ACTIONS: {}, S: { route: 'other' }, icon: () => '',
    esc: value => String(value), busy: async (_button, fn) => fn(), toast() {},
    renderLocalCards() {}, clearTimeout() {}, setTimeout: () => 1, document: { addEventListener() {} },
    MutationObserver: class { observe() {} },
  });
  vm.runInContext(source, context);
  const run = code => vm.runInContext(code, context);
  const preview = label => {
    context.fixtureLabel = label;
    run(`OFFICIAL_PANEL.fileRevision++; OFFICIAL_PANEL.credentials = fixtureLabel; resetOfficialImportReview(); setOfficialImportPreview({accounts:[{name:fixtureLabel}]});`);
  };
  preview('synthetic-private-credential');
  return { node, context, run, preview };
}

test('a saved import with failed list refresh offers read-only recovery without resending credentials', async () => {
  let imports = 0, reads = 0;
  const h = harness(async path => {
    if (path.endsWith('/import')) { imports++; return { ok: true }; }
    if (path === '/api/local/accounts') { if (++reads === 1) throw Error('synthetic-private-upstream'); return { accounts: [] }; }
    throw Error(path);
  });
  await h.context.ACTIONS.officialImport({});
  assert.match(h.node('#officialImportPreview').innerHTML, /已导入|已保存/);
  assert.match(h.node('#officialImportPreview').innerHTML, /officialImportRefresh/);
  assert.doesNotMatch(h.node('#officialImportPreview').innerHTML, /synthetic-private/);
  assert.equal(h.run('OFFICIAL_PANEL.credentials'), null);
  await h.context.ACTIONS.officialImportRefresh({});
  assert.equal(imports, 1);
  assert.equal(reads, 2);
  assert.equal(h.node('#modalRoot').innerHTML, '');
});

test('partial durable import is still described as saved when list refresh also fails', async () => {
  const h = harness(async path => {
    if (path.endsWith('/import')) return { ok: false, saved: true, error: '账号已安全保存，API 列表同步失败，请重试关联' };
    throw Error('synthetic private refresh failure');
  });
  await h.context.ACTIONS.officialImport({});
  const html = h.node('#officialImportPreview').innerHTML;
  assert.match(html, /已保存/);
  assert.match(html, /刷新/);
  assert.doesNotMatch(html, /synthetic private/);
});

test('an uncertain import result clears secrets and offers a list check without automatic resubmission', async () => {
  let writes = 0, reads = 0;
  const h = harness(async path => {
    if (path.endsWith('/import')) { writes++; throw Error('synthetic lost reply'); }
    if (path === '/api/local/accounts') { reads++; return { accounts: [] }; }
    throw Error(path);
  });
  await h.context.ACTIONS.officialImport({});
  assert.match(h.node('#officialImportPreview').innerHTML, /未确认/);
  assert.match(h.node('#officialImportPreview').innerHTML, /officialImportRefresh/);
  assert.doesNotMatch(h.node('#officialImportPreview').innerHTML, /已导入|已保存|synthetic/);
  assert.equal(h.run('OFFICIAL_PANEL.credentials'), null);
  assert.equal(writes, 1);
  assert.equal(reads, 0);
  await h.context.ACTIONS.officialImportRefresh({});
  assert.equal(writes, 1);
  assert.equal(reads, 1);
});

for (const failure of [false, true]) {
  test(`an old import refresh ${failure ? 'failure' : 'success'} cannot discard a newer preview`, async () => {
    const refresh = deferred(), entered = deferred();
    const h = harness(async path => {
      if (path.endsWith('/import')) return { ok: true };
      entered.resolve(); return refresh.promise;
    });
    const work = h.context.ACTIONS.officialImport({});
    await entered.promise;
    h.preview('new-file');
    const epoch = h.run('OFFICIAL_PANEL.epoch');
    if (failure) refresh.reject(Error('synthetic obsolete error')); else refresh.resolve({ accounts: [] });
    await work;
    assert.equal(h.run('OFFICIAL_PANEL.credentials'), 'new-file');
    assert.equal(h.run('OFFICIAL_PANEL.epoch'), epoch);
    assert.match(h.node('#officialImportPreview').innerHTML, /new-file/);
    assert.doesNotMatch(h.node('#officialImportPreview').innerHTML, /officialImportRefresh|obsolete/);
  });
}

test('two refresh clicks share one request and disposing wipes the recovery state', async () => {
  const pending = deferred(); let reads = 0;
  const h = harness(async path => {
    if (path.endsWith('/import')) throw Error('synthetic unknown');
    reads++; return pending.promise;
  });
  await h.context.ACTIONS.officialImport({});
  const first = h.context.ACTIONS.officialImportRefresh({});
  await h.context.ACTIONS.officialImportRefresh({});
  h.run('disposeOfficialPanel()');
  const epoch = h.run('OFFICIAL_PANEL.epoch');
  pending.resolve({ accounts: [] });
  await first;
  assert.equal(reads, 1);
  assert.equal(h.run('OFFICIAL_PANEL.importResult'), null);
  assert.equal(h.run('OFFICIAL_PANEL.epoch'), epoch);
});

for (const failure of ['unavailable', 'invalid', 'superseded']) {
  test(`a saved import waits for the vault list when its read is ${failure}`, async () => {
    let writes = 0, vaultReads = 0;
    const h = harness(async path => {
      if (path.endsWith('/import')) { writes++; return { ok: true }; }
      if (path === '/api/local/accounts') return { accounts: [] };
      if (path === '/api/local/codex/accounts') {
        if (++vaultReads === 1) {
          if (failure === 'unavailable') throw Error('synthetic private vault failure');
          if (failure === 'invalid') return {};
          h.run('officialLibraryRevision++');
          return { accounts: [] };
        }
        return { accounts: [{ id: 'session-only', mode: 'chatgpt_web_session' }] };
      }
      throw Error(path);
    });
    h.run("S.route = 'accounts'; loadCodexAPIOverview = async () => {}; S.officialRows = [{id:'existing'}]");
    const epoch = h.run('OFFICIAL_PANEL.epoch');
    await h.context.ACTIONS.officialImport({});
    assert.equal(h.run('OFFICIAL_PANEL.epoch'), epoch);
    assert.match(h.node('#officialImportPreview').innerHTML, /账号已导入，列表刷新失败/);
    assert.doesNotMatch(h.node('#officialImportPreview').innerHTML, /synthetic private/);
    assert.equal(h.run('S.officialRows[0]?.id'), 'existing');
    await h.context.ACTIONS.officialImportRefresh({});
    assert.equal(writes, 1);
    assert.equal(vaultReads, 2);
    assert.equal(h.run('S.officialRows[0].id'), 'session-only');
    assert.equal(h.node('#modalRoot').innerHTML, '');
  });
}

test('a superseded API list does not report a completed refresh or close the import', async () => {
  const h = harness(async path => {
    if (path.endsWith('/import')) return { ok: true };
    if (path === '/api/local/accounts') {
      h.run('S.localAccountsRevision++');
      return { accounts: [] };
    }
    throw Error(path);
  });
  const epoch = h.run('OFFICIAL_PANEL.epoch');
  await h.context.ACTIONS.officialImport({});
  assert.equal(h.run('OFFICIAL_PANEL.epoch'), epoch);
  assert.match(h.node('#officialImportPreview').innerHTML, /列表刷新失败/);
});
