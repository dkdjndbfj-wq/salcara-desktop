'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const webDir = path.resolve(__dirname, '../../internal/console/web');
const source = fs.readFileSync(path.join(webDir, 'selfhost.js'), 'utf8');

function deferred() {
  let resolve;
  const promise = new Promise(done => { resolve = done; });
  return { promise, resolve };
}

function fixture() {
  const requests = [];
  const apiCalls = [];
  const nodes = new Map();
  const node = id => {
    if (!nodes.has(id)) nodes.set(id, { id, value: '', tagName: 'INPUT', isConnected: true });
    return nodes.get(id);
  };
  let panelMarkup = '<section id="shHubPanel"></section>';
  const details = { open: false };
  let activeElement = null;
  let holdStatus = true;
  let confirmResult = true;
  const confirmations = [];
  const panel = {
    id: 'shHubPanel',
    isConnected: true,
    contains: element => element === activeElement,
    querySelector: () => details,
    get outerHTML() { return panelMarkup; },
    set outerHTML(value) { panelMarkup = value; },
  };
  const context = vm.createContext({
    PAGES: {}, RENDER: {}, ACTIONS: {},
    S: { route: 'login' },
    RM: { remoteMode: 'station', loginEpoch: 1 },
    document: {
      get activeElement() { return activeElement; },
      getElementById: id => id === 'shHubPanel' ? panel : null,
    },
    $: selector => node(String(selector).replace(/^#/, '')),
    api: (endpoint, body) => {
      apiCalls.push({ endpoint, body });
      if (endpoint !== '/api/local/hub/status') return Promise.resolve({ ok: true });
      if (!holdStatus) return Promise.resolve({ running: false, addr: '', devices: 0, phones: 0 });
      const gate = deferred();
      requests.push({ endpoint, ...gate });
      return gate.promise;
    },
    icon: () => '', ico: () => '', esc: value => String(value ?? ''),
    toast() {}, busy: async (_button, fn) => fn(),
    confirm: async message => { confirmations.push(message); return confirmResult; },
    setInterval: () => 1, clearInterval() {},
  });
  vm.runInContext(source, context, { filename: 'selfhost.js' });
  return {
    context, requests, apiCalls, confirmations, panel,
    node,
    setActive: value => { activeElement = value; },
    setConfirm: value => { confirmResult = value; },
    holdStatus: value => { holdStatus = value; },
    panelMarkup: () => panelMarkup,
  };
}

test('the former local-connection route is merged into the direct tab', () => {
  const index = fs.readFileSync(path.join(webDir, 'index.html'), 'utf8');
  const remote = fs.readFileSync(path.join(webDir, 'remote-connect.js'), 'utf8');
  const shell = fs.readFileSync(path.join(webDir, 'shell.js'), 'utf8');
  assert.doesNotMatch(index, /data-route="selfhost"/);
  assert.match(index, /data-route="login"/);
  for (const [mode, label] of [['station', '中转站'], ['bot', 'Bot'], ['direct', '直连']]) assert.ok(remote.includes(`['${mode}', '${label}'`));
  assert.match(remote, /renderSelfhostHubPanel\(\)/);
  assert.match(shell, /r === 'selfhost'[\s\S]*RM\.remoteMode = 'direct'[\s\S]*'#login'/);
});

test('self-host status is polled only while the direct tab is active', () => {
  const f = fixture();
  const view = { isConnected: true };
  f.context.selfhostAttach(view, 1);
  assert.equal(f.requests.length, 0);

  f.context.RM.remoteMode = 'direct';
  f.context.selfhostAttach(view, 2);
  assert.equal(f.requests.length, 1);
  assert.equal(f.requests[0].endpoint, '/api/local/hub/status');
});

test('a status response after leaving the direct tab cannot update shared state', async () => {
  const f = fixture();
  const view = { isConnected: true };
  f.context.RM.remoteMode = 'direct';
  f.context.selfhostAttach(view, 1);
  f.context.RM.remoteMode = 'station';
  f.requests[0].resolve({ running: true, addr: ':9000' });
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(vm.runInContext('SELFHOST.hub', f.context), null);
});

test('a prior status request cannot overwrite the latest Hub state', async () => {
  const f = fixture();
  const view = { isConnected: true };
  f.context.RM.remoteMode = 'direct';
  f.context.selfhostAttach(view, 1);
  const seq = vm.runInContext('SELFHOST.seq', f.context);
  const newerLoad = f.context.selfhostLoad(view, 1, seq);

  f.requests[1].resolve({ running: true, addr: ':9100', mode: 'open', revision: 'new', bases: [] });
  await newerLoad;
  f.requests[0].resolve({ running: true, addr: ':9000', mode: 'restricted', revision: 'old', bases: ['https://old.example'] });
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(vm.runInContext('SELFHOST.hub.addr', f.context), ':9100');
});

test('status polling preserves an open editor and its current values', async () => {
  const f = fixture();
  const input = { tagName: 'INPUT' };
  const view = { isConnected: true };
  f.context.RM.remoteMode = 'direct';
  f.context.selfhostAttach(view, 1);
  f.node('shHubAddr').value = ':9100';
  f.setActive(input);
  const original = f.panelMarkup();

  f.requests[0].resolve({ running: false, addr: '', devices: 0, phones: 0 });
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(f.panelMarkup(), original);
  assert.equal(vm.runInContext('SELFHOST.draft.addr', f.context), ':9100');
});

test('restricted Hub start requires an allowed API base', async () => {
  const f = fixture();
  f.node('shHubAddr').value = ':8765';
  f.node('shHubMode').value = 'restricted';
  f.node('shHubRevision').value = 'policy-r2';
  f.node('shHubBases').value = '';
  await assert.rejects(f.context.ACTIONS.selfhostHubStart({}), /至少要填写一个允许的 API 地址/);
  assert.deepEqual(f.apiCalls, []);
});

test('open Hub start sends explicit confirmation only after the warning is accepted', async () => {
  const f = fixture();
  f.holdStatus(false);
  f.node('shHubAddr').value = ':8765';
  f.node('shHubMode').value = 'open';
  f.node('shHubRevision').value = 'policy-r2';

  f.setConfirm(false);
  await f.context.ACTIONS.selfhostHubStart({});
  assert.deepEqual(f.apiCalls, []);

  f.setConfirm(true);
  await f.context.ACTIONS.selfhostHubStart({});
  const start = f.apiCalls.find(call => call.endpoint === '/api/local/hub/start');
  assert.ok(start);
  assert.equal(start.body.confirmOpen, true);
  assert.deepEqual(Array.from(start.body.bases), []);
  assert.equal(f.confirmations.length, 2);
});

test('Hub start and stop mutations are single-flight', async () => {
  const f = fixture();
  f.holdStatus(false);
  f.node('shHubAddr').value = ':8765';
  f.node('shHubMode').value = 'open';
  f.node('shHubRevision').value = 'policy-r2';

  await Promise.all([
    f.context.ACTIONS.selfhostHubStart({}),
    f.context.ACTIONS.selfhostHubStart({}),
  ]);
  assert.equal(f.apiCalls.filter(call => call.endpoint === '/api/local/hub/start').length, 1);

  await Promise.all([
    f.context.ACTIONS.selfhostHubStop({}),
    f.context.ACTIONS.selfhostHubStop({}),
  ]);
  assert.equal(f.apiCalls.filter(call => call.endpoint === '/api/local/hub/stop').length, 1);
});
