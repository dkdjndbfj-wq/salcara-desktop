'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const { setup, deferred, web } = require('./helpers/workbench.cjs');
const copy = value => JSON.parse(JSON.stringify(value));
const flush = () => new Promise(resolve => setImmediate(resolve));

function delayNextSnapshot(f) {
  const gate = deferred(), snapshot = copy(f.backend), previous = f.context.api;
  let intercepted = false;
  f.context.api = (url, body) => {
    if (!intercepted && url === '/api/local/accounts' && body === undefined) {
      intercepted = true;
      return gate.promise;
    }
    return previous(url, body);
  };
  return { resolve: () => gate.resolve(snapshot), reject: error => gate.reject(error) };
}

async function selectAndLaunch(f) {
  await f.change('codex', 'api-shared');
  f.document.getElementById('wbReady').checked = true;
  await f.ACTIONS.wbOpenGo(f.button('wbOpenGo'));
  assert.equal(f.backend.bindings.codex.accountId, 'api-shared');
  assert.equal(f.S.local.bindings.codex.accountId, 'api-shared');
}

test('a late initial overview snapshot cannot erase a newly committed Agent choice', async () => {
  const f = setup(), old = delayNextSnapshot(f);
  f.context.clearTimeout = () => {};
  const view = { isConnected: true, innerHTML: '' };
  f.RENDER_overview(view);
  await selectAndLaunch(f);
  old.resolve(); await flush();
  assert.equal(f.S.local.bindings.codex.accountId, 'api-shared');
  assert.match(f.document.getElementById('agentToolList').innerHTML, /value="api-shared" selected/);
});

test('a failed initial read cannot append an obsolete error after a successful newer refresh', async () => {
  const f = setup(), old = delayNextSnapshot(f), notices = [];
  f.context.clearTimeout = () => {};
  const view = { isConnected: true, innerHTML: '', insertAdjacentHTML: (_where, html) => notices.push(html) };
  f.RENDER_overview(view);
  await selectAndLaunch(f);
  old.reject(new Error('obsolete request failure')); await flush();
  assert.deepEqual(notices, []);
});

test('a background status response cannot undo a choice committed while it was in flight', async () => {
  const f = setup(), old = delayNextSnapshot(f);
  const refreshing = f.context.refreshWorkbenchStatus();
  await selectAndLaunch(f);
  old.resolve(); await refreshing;
  assert.equal(f.S.local.bindings.codex.accountId, 'api-shared');
  assert.equal(f.S.bindingLoading, false);
});

test('status pushes do not steal the snapshot while an Agent confirmation is being verified', async () => {
  const f = setup(), old = delayNextSnapshot(f);
  const opening = f.change('codex', 'api-shared');
  const revision = f.S.localAccountsRevision;
  await f.context.refreshWorkbenchStatus();
  assert.equal(f.calls.length, 0, 'the background request must not start during foreground verification');
  assert.equal(f.S.localAccountsRevision, revision);
  old.resolve(); await opening;
  assert.equal(f.WB.accountOpen.verified, true);
  const reads = f.calls.length;
  await f.context.refreshWorkbenchStatus();
  assert.equal(f.calls.length, reads, 'a verified confirmation keeps its selected account snapshot');
});

test('status pushes do not supersede the first page read or a launch awaiting acknowledgement', async () => {
  const f = setup(), cold = f.S.local;
  f.S.local = null;
  await f.context.refreshWorkbenchStatus();
  assert.equal(f.calls.length, 0);
  f.S.local = cold;
  f.WB.launchPending = true;
  await f.context.refreshWorkbenchStatus();
  assert.equal(f.calls.length, 0);
});

test('a background response from a previous visit cannot replace the current route snapshot', async () => {
  const f = setup(), old = delayNextSnapshot(f);
  f.S.routeRevision = 1;
  const refreshing = f.context.refreshWorkbenchStatus();
  f.S.routeRevision = 3;
  f.S.local.bindings.codex.accountId = 'api-empty';
  old.resolve(); await refreshing;
  assert.equal(f.S.local.bindings.codex.accountId, 'api-empty');
});

test('normal status refreshes still update the current snapshot and release their single-flight flag', async () => {
  const f = setup();
  f.backend.bindings.codex.accountId = 'api-shared';
  await f.context.refreshWorkbenchStatus();
  assert.equal(f.S.local.bindings.codex.accountId, 'api-shared');
  assert.equal(f.S.bindingLoading, false);
});

test('a superseded saved-key continuation cannot reopen a dialog from its old account snapshot', async () => {
  const f = setup(), old = delayNextSnapshot(f);
  const continuing = f.context.wbContinueWithSaved('codex', 'api-shared');
  f.backend.bindings.codex.accountId = 'api-empty';
  await f.context.refreshWorkbenchStatus();
  old.resolve(); await continuing;
  assert.equal(f.S.local.bindings.codex.accountId, 'api-empty');
  assert.equal(f.WB.accountOpen, null);
  assert.equal(f.modal.innerHTML, '');
});

test('a cold home read cannot overwrite accounts loaded after navigating to Agent', async () => {
  const f = setup(), old = delayNextSnapshot(f);
  const shell = fs.readFileSync(path.join(web, 'shell.js'), 'utf8');
  vm.runInContext(shell.slice(shell.indexOf('const HOME =')), f.context, { filename: 'shell-home.js' });
  f.S.route = 'home'; f.S.routeRevision = 1; f.S.local = null;
  const home = f.context.renderHome();
  f.S.route = 'overview'; f.S.routeRevision = 2;
  f.S.local = copy(f.backend);
  await selectAndLaunch(f);
  old.resolve(); await home;
  assert.equal(f.S.local.bindings.codex.accountId, 'api-shared');
});

test('an unbound or unavailable Agent selection shows its placeholder and disables Open', () => {
  const f = setup();
  const tool = f.S.local.tools.find(item => item.id === 'codex');
  f.S.local.accounts.push({ id: 'web-only', name: 'Website login', libraryOnly: true });
  for (const id of ['', 'removed-account', 'web-only']) {
    f.S.local.bindings.codex.accountId = id;
    const html = f.wbCard(tool);
    assert.match(html, /<option value="" disabled selected>选择 API 密钥<\/option>/);
    assert.match(html, /data-act="wbOpen" data-target="codex" disabled/);
    assert.doesNotMatch(html, /value="web-only"/);
  }
  f.S.local.bindings.codex.accountId = 'api-shared';
  const html = f.wbCard(tool);
  assert.doesNotMatch(html, /<option value="" disabled selected>/);
  assert.match(html, /<option value="api-shared" selected>/);
  assert.doesNotMatch(html, /data-act="wbOpen" data-target="codex" disabled/);
});

test('cold-start add-key and restore reads cannot replace a newer page or reopen its dialog', async () => {
  for (const action of ['localNew', 'toolRestore']) {
    const f = setup(), old = delayNextSnapshot(f);
    f.S.local = null; f.S.routeRevision = 1;
    const opening = f.ACTIONS[action]({ isConnected: true, dataset: { target: 'codex' } });
    f.S.route = 'settings'; f.S.routeRevision = 2;
    const latest = copy(f.backend);
    latest.bindings.codex.accountId = 'api-empty';
    f.S.local = latest;
    f.modal.innerHTML = '<div>newer dialog</div>';
    old.resolve(); await opening;
    assert.equal(f.S.local, latest);
    assert.equal(f.modal.innerHTML, '<div>newer dialog</div>');
  }
});

test('a current cold-start add-key request still opens its editor once the account library loads', async () => {
  const f = setup();
  f.S.local = null;
  await f.ACTIONS.localNew({ isConnected: true, dataset: {} });
  assert.ok(f.S.local.accounts.length);
  assert.match(f.modal.innerHTML, /id="laID"/);
});
