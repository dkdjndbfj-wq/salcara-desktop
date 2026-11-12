import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';

const source = fs.readFileSync(new URL('../internal/console/web/remote-support.js', import.meta.url), 'utf8');
const remoteSource = fs.readFileSync(new URL('../internal/console/web/remote.js', import.meta.url), 'utf8');
const connectionHelpers = remoteSource.slice(remoteSource.indexOf('function rmBeginConnectionAction('), remoteSource.indexOf('function sessionCapabilities('));
const SRC = 'src-v1-' + 'a'.repeat(64), NEXT = 'src-v1-' + 'b'.repeat(64);
const plain = value => JSON.parse(JSON.stringify(value));
function gate() { let resolve, reject; return { promise: new Promise((r, j) => { resolve = r; reject = j; }), resolve, reject }; }
const settle = () => new Promise(setImmediate);
function fixture() {
  const nodes = new Map(), calls = [], notices = [], timers = new Map();
  const state = { now: 1800000000000, timer: 0, renders: 0, intercept: null };
  class Clock extends Date { static now() { return state.now; } }
  const supported = (api = 'supported', account = 'supported') => ({ apiRemote: api, accountRemote: account, checkedAt: state.now, freshUntil: state.now + 300000 });
  const account = { id: 'api-one', name: 'Fixture API', sourceType: 'api_key', sourceRevision: SRC, baseUrl: 'https://relay.fixture/a', remoteSupport: supported() };
  const official = { id: 'official-one', name: 'Official fixture', sourceType: 'official_account', sourceRevision: SRC, officialAccount: { id: 'vault-one', mode: 'oauth' } };
  const station = { hubUrl: 'https://relay.fixture/salcara-hub', name: 'relay.fixture', support: supported() };
  const local = { accounts: [account, official], remoteStations: [station] };
  function node(id, extra = {}) { const item = { innerHTML: '', textContent: '', dataset: {}, isConnected: true, ...extra }; nodes.set(id, item); return item; }
  const context = vm.createContext({
    S: { route: 'accounts', local, state: { config: { remoteConnections: [station] } } }, ACTIONS: {}, OFFICIAL_PANEL: { epoch: 0 },
    RM: { remoteMode: 'station', connectionRevision: 0, loginEpoch: 0 },
    loadState: async () => context.S.state, rmUpdateRemoteStatus() {},
    URL, Date: Clock, icon: () => '', esc: value => String(value).replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('"', '&quot;'),
    $: key => nodes.get(key), document: { getElementById: id => nodes.get('#' + id), addEventListener() {} },
    setTimeout: (fn, ms) => { const id = ++state.timer; timers.set(id, { fn, at: state.now + ms }); return id; },
    clearTimeout: id => timers.delete(id),
    officialPanel() { context.OFFICIAL_PANEL.epoch++; for (const id of ['#officialPanel', '#officialError', '#officialStationChoices']) node(id); },
    closeOfficialPanel() { context.OFFICIAL_PANEL.epoch++; for (const id of ['#officialPanel', '#officialError', '#officialStationChoices']) nodes.delete(id); },
    renderLocalCards: () => { state.renders++; }, toast: message => notices.push(message), location: { hash: 'accounts' }, route: async () => {},
    api: async (path, body) => {
      calls.push({ path, ...(body ? { body: plain(body) } : {}) });
      if (state.intercept) { const result = state.intercept(path, body); if (result !== undefined) return result; }
      if (path === '/api/local/accounts') return plain(context.S.local);
      if (path === '/api/local/accounts/remote-support') {
        const row = context.S.local.accounts.find(a => a.id === body.id);
        return { ok: true, id: body.id, sourceRevision: body.expectedSourceRevision, baseUrl: row.baseUrl, remoteSupport: supported() };
      }
      if (path === '/api/remote/discover') return { ok: true, connected: body.connect, plugin: { root: new URL(body.hubUrl).origin, hubUrl: body.hubUrl } };
      if (path === '/api/remote/mode') return { ok: true, activeMode: body.mode };
      throw new Error('unexpected API path');
    },
  });
  vm.runInContext(connectionHelpers, context);
  vm.runInContext(source, context);
  const card = a => node('#accountRemote-' + a.id, { dataset: { source: a.sourceRevision, base: a.baseUrl } });
  const button = (hub = station.hubUrl, target = '#officialStationChoices') => ({
    dataset: { hub }, isConnected: true, disabled: false, closest: selector => selector === target ? nodes.get(target) : null,
  });
  return { context, nodes, calls, notices, state, local, account, official, station, supported, node, card, button,
    writes: () => calls.filter(call => call.body), probeWrites: () => calls.filter(call => call.path.endsWith('/remote-support')),
    queue: a => context.queueAccountRemoteProbe(a || account),
    probe: a => context.ACTIONS.accountRemoteProbe({ dataset: { id: (a || account).id, source: (a || account).sourceRevision, base: (a || account).baseUrl } }),
    officialOpen: () => context.ACTIONS.officialRemoteStations({ dataset: { id: official.id } }),
    officialHTML: () => nodes.get('#officialStationChoices')?.innerHTML,
    advance: ms => { state.now += ms; for (const [id, timer] of [...timers]) if (timer.at <= state.now) { timers.delete(id); timer.fn(); } },
  };
}

test('saved-station API scope read failure becomes an error with a working local retry', async () => {
  const f = fixture(); f.context.S.route = 'login';
  const root = f.node('#savedRemoteStations'), status = f.node('#remoteSupportScopeStatus');
  f.state.intercept = path => { if (path === '/api/local/accounts') throw Error('fixture unavailable'); };
  f.context.remoteSavedStationsHTML({ remoteConnections: [f.station] }); await settle();
  assert.match(root.innerHTML, /API 范围暂不可读/); assert.doesNotMatch(root.innerHTML, /正在读取 API 范围/); assert.match(status.innerHTML, /remoteScopeRetry/);
  f.state.intercept = null; await f.context.ACTIONS.remoteScopeRetry();
  assert.match(root.innerHTML, /电脑允许/); assert.equal(status.innerHTML, '');
  assert.equal(f.calls.some(call => call.path === '/api/remote/discover'), false);
});

test('a late saved-scope response cannot paint a replacement page with the same element id', async () => {
  const f = fixture(), pending = gate(); f.context.S.route = 'login';
  f.node('#savedRemoteStations'); f.node('#remoteSupportScopeStatus');
  f.state.intercept = path => path === '/api/local/accounts' ? pending.promise : undefined;
  f.context.remoteSavedStationsHTML({ remoteConnections: [f.station] }); await settle();
  const replacement = f.node('#savedRemoteStations', { innerHTML: 'new owner' });
  pending.resolve(f.local); await settle(); assert.equal(replacement.innerHTML, 'new owner');
});

test('API cards render two independent concise states without starting a probe', () => {
  const f = fixture();
  f.account.remoteSupport = f.supported('unsupported', 'supported');
  const html = f.context.accountRemoteSupportHTML(f.account);
  assert.match(html, /aria-label="API 远程 · 暂不支持"/);
  assert.match(html, /aria-label="账号远程 · 可用"/);
  for (let i = 0; i < 8; i++) { f.context.accountRemoteSupportHTML(f.account); f.context.syncRemoteSupport(); }
  assert.equal(f.calls.length, 0);
});

test('both API capability icons independently refresh in place without a picker or navigation', async () => {
  const f = fixture(); f.card(f.account);
  const html = f.context.accountRemoteSupportHTML(f.account);
  const buttons = [...html.matchAll(/<button\b([^>]*)>/g)];
  assert.equal(buttons.length, 2);
  for (const [, attributes] of buttons) {
    assert.match(attributes, /data-act="accountRemoteProbe"/);
    const dataset = Object.fromEntries([...attributes.matchAll(/data-(id|source|base)="([^"]*)"/g)].map(([, key, value]) => [key, value]));
    await f.context.ACTIONS.accountRemoteProbe({ dataset, isConnected: true }); await settle();
    assert.match(f.nodes.get('#accountRemote-api-one').innerHTML, /API 远程 · 可用/);
    assert.match(f.nodes.get('#accountRemote-api-one').innerHTML, /账号远程 · 可用/);
    assert.equal(f.nodes.has('#officialPanel'), false);
    assert.equal(f.context.S.route, 'accounts');
    assert.equal(f.context.location.hash, 'accounts');
  }
  assert.equal(f.probeWrites().length, 2);
  assert.equal(f.writes().length, 2);
});

test('API detection swaps both glyphs for an inline spinner and restores them on completion', async () => {
  const f = fixture(), pending = gate(); const card = f.card(f.account);
  f.state.intercept = path => path.endsWith('/remote-support') ? pending.promise : undefined;
  const detection = f.probe(); await settle();
  assert.equal((card.innerHTML.match(/is-checking/g) || []).length, 2);
  assert.equal((card.innerHTML.match(/aria-busy="true" disabled/g) || []).length, 2);
  assert.match(card.innerHTML, /M12 4a8 8/);
  assert.doesNotMatch(card.innerHTML, /m8 8-4 4|cy="8" r="3"/);
  pending.resolve({ ok: true, id: f.account.id, sourceRevision: SRC, baseUrl: f.account.baseUrl, remoteSupport: f.supported() });
  await detection; await settle();
  assert.doesNotMatch(card.innerHTML, /is-checking|aria-busy="true"/);
  assert.match(card.innerHTML, /m8 8-4 4/);
  assert.match(card.innerHTML, /cy="8" r="3"/);
});

test('both saved-station icons directly detect in place without a dialog or connection', async () => {
  const f = fixture(); f.context.S.route = 'login'; f.context.location.hash = 'login';
  f.node('#savedRemoteStations');
  const html = f.context.remoteSavedStationsHTML({ remoteConnections: [f.station] });
  const icons = [...html.matchAll(/<button\b([^>]*class="remote-capability[^>]*)>/g)];
  assert.equal(icons.length, 2);
  for (const [, attributes] of icons) {
    assert.match(attributes, /data-act="remoteStationProbe"/);
    await f.context.ACTIONS.remoteStationProbe(f.button(f.station.hubUrl, '#savedRemoteStations'));
    assert.match(f.nodes.get('#savedRemoteStations').innerHTML, /API 远程 · 可用/);
    assert.match(f.nodes.get('#savedRemoteStations').innerHTML, /账号远程 · 可用/);
    assert.equal(f.nodes.has('#officialPanel'), false);
    assert.equal(f.context.S.route, 'login');
    assert.equal(f.context.location.hash, 'login');
  }
  assert.equal(f.writes().length, 2);
  assert.ok(f.writes().every(write => write.path === '/api/remote/discover' && write.body.connect === false));
});

test('the phone-remote page retains a separate explicit connect action', async () => {
  const f = fixture(); f.context.S.route = 'login'; f.context.location.hash = 'login';
  f.node('#savedRemoteStations');
  const html = f.context.remoteSavedStationsHTML({ remoteConnections: [f.station] });
  assert.match(html, /data-act="remoteStationSelect"[^>]*>启用中转站<\/button>/);
  await f.context.ACTIONS.remoteStationSelect(f.button(f.station.hubUrl, '#savedRemoteStations'));
  assert.equal(f.writes().length, 1);
  assert.deepEqual(f.writes()[0], { path: '/api/remote/mode', body: { mode: 'hub', hubUrl: f.station.hubUrl } });
  assert.equal(f.nodes.has('#officialPanel'), false);
});

test('unknown, malformed and expired observations never turn green', () => {
  for (const patch of [{}, { freshUntil: 1 }, { checkedAt: 1900000000000 }, { checkedAt: 1800000030000, freshUntil: 1800000020000 }, { freshUntil: Infinity }, { apiRemote: true }]) {
    const f = fixture(); f.account.remoteSupport = Object.keys(patch).length ? { ...f.supported(), ...patch } : {};
    const html = f.context.accountRemoteSupportHTML(f.account);
    assert.match(html, /aria-label="API 远程 · 待检测"/);
  }
});

test('save-triggered auto detection coalesces by complete source and sends no key or URL', async () => {
  const f = fixture(), pending = gate(); f.card(f.account);
  f.state.intercept = path => path.endsWith('/remote-support') ? pending.promise : undefined;
  const a = f.queue(), b = f.queue(); await settle();
  assert.equal(f.probeWrites().length, 1);
  assert.deepEqual(f.probeWrites()[0].body, { id: f.account.id, expectedSourceRevision: SRC });
  pending.resolve({ ok: true, id: f.account.id, sourceRevision: SRC, baseUrl: f.account.baseUrl, remoteSupport: f.supported() });
  await Promise.all([a, b]); await settle(); await f.queue();
  assert.equal(f.probeWrites().length, 1);
  assert.match(f.nodes.get('#accountRemote-api-one').innerHTML, /API 远程 · 可用/);
});

test('manual refresh may retry once after an automatic failure and clears old green state', async () => {
  const f = fixture(); f.card(f.account);
  f.state.intercept = path => path.endsWith('/remote-support') ? Promise.reject(new Error('offline')) : undefined;
  await f.queue(); await settle();
  assert.doesNotMatch(f.nodes.get('#accountRemote-api-one').innerHTML, /is-supported/);
  f.state.intercept = null;
  await f.probe(); await settle();
  assert.equal(f.probeWrites().length, 2);
  assert.match(f.nodes.get('#accountRemote-api-one').innerHTML, /is-supported/);
});

test('a saved-source probe arriving before the refreshed account list is retained for that exact source', async () => {
  const f = fixture(); f.account.remoteSupport = {};
  const updated = { ...f.account, sourceRevision: NEXT };
  await f.queue(updated); await settle();
  assert.doesNotMatch(f.context.accountRemoteSupportHTML(f.account), /is-supported/);
  f.local.accounts[0] = updated;
  assert.match(f.context.accountRemoteSupportHTML(updated), /is-supported/);
  await f.queue(updated);
  assert.equal(f.probeWrites().length, 1);
});

test('slow probe cannot write into a replacement source or detached card', async () => {
  const f = fixture(), pending = gate(); const old = f.card(f.account);
  f.state.intercept = () => pending.promise;
  const work = f.queue(); await settle();
  f.account.sourceRevision = NEXT; f.account.baseUrl = 'https://other.fixture/b'; f.account.remoteSupport = {};
  const replacement = f.card(f.account); const oldHTML = old.innerHTML;
  pending.resolve({ ok: true, id: f.account.id, sourceRevision: SRC, baseUrl: 'https://relay.fixture/a', remoteSupport: f.supported() });
  await work; await settle();
  assert.equal(old.innerHTML, oldHTML);
  assert.doesNotMatch(replacement.innerHTML, /is-supported/);
});

test('same-host account cards and wrong full-path responses stay isolated', async () => {
  const f = fixture(), second = { ...f.account, id: 'api-two', baseUrl: 'https://relay.fixture/b', remoteSupport: {} };
  f.local.accounts.push(second); f.card(f.account); f.card(second);
  f.state.intercept = (path, body) => path.endsWith('/remote-support') ? {
    ok: true, id: body.id, sourceRevision: SRC, baseUrl: 'https://relay.fixture/a', remoteSupport: f.supported(),
  } : undefined;
  await f.queue(second); await settle();
  assert.doesNotMatch(f.nodes.get('#accountRemote-api-two').innerHTML, /is-supported/);
  assert.match(f.nodes.get('#accountRemote-api-one').innerHTML, /is-supported/);
});

test('probe queue caps concurrent pings and does not perform calls during rerenders', async () => {
  const f = fixture(), gates = [];
  f.state.intercept = path => { if (!path.endsWith('/remote-support')) return; const pending = gate(); gates.push(pending); return pending.promise; };
  const promises = Array.from({ length: 5 }, (_, index) => f.queue({ ...f.account, id: 'queued-' + index }));
  await settle(); assert.equal(gates.length, 2);
  for (let i = 0; i < 5; i++) {
    gates[i].resolve({ ok: false, id: 'queued-' + i, sourceRevision: SRC, baseUrl: f.account.baseUrl });
    await settle();
  }
  await Promise.all(promises);
  assert.equal(f.probeWrites().length, 5);
});

test('a stale visible-button revision cannot probe a newer account behind it', async () => {
  const f = fixture(); f.account.sourceRevision = NEXT;
  await f.context.ACTIONS.accountRemoteProbe({ dataset: { id: f.account.id, source: SRC, base: f.account.baseUrl } });
  assert.equal(f.calls.length, 0);
});

test('API version suffix normalization matches the server and empty query/fragment markers are rejected', async () => {
  const f = fixture(); f.account.baseUrl = 'https://relay.fixture/a/V1/';
  f.state.intercept = path => path.endsWith('/remote-support') ? {
    ok: true, id: f.account.id, sourceRevision: SRC, baseUrl: 'https://relay.fixture/a', remoteSupport: f.supported(),
  } : undefined;
  await f.queue(); await settle();
  assert.match(f.context.accountRemoteSupportHTML(f.account), /is-supported/);
  for (const suffix of ['?', '#']) await f.queue({ ...f.account, id: 'invalid-' + suffix, baseUrl: 'https://relay.fixture/a' + suffix });
  assert.equal(f.probeWrites().length, 1);
});

test('expiry updates both API markers and official station count without polling', () => {
  const f = fixture(); const api = f.card(f.account);
  const official = f.node('#officialRemote-' + f.official.id, { dataset: { source: SRC } });
  f.context.syncRemoteSupport();
  assert.match(api.innerHTML, /is-supported/); assert.match(official.innerHTML, /1 站/);
  f.advance(300001);
  assert.doesNotMatch(api.innerHTML, /is-supported/); assert.match(official.innerHTML, /暂不支持/);
  assert.equal(f.calls.length, 0);
});

test('official card picker opens saved sites without connecting or authorizing a phone', async () => {
  const f = fixture(); await f.officialOpen();
  assert.match(f.officialHTML(), /relay.fixture/); assert.match(f.officialHTML(), /data-act="remoteStationSelect"/);
  assert.equal(f.writes().length, 0);
});

test('official picker shows an honest unsupported fallback and a detection action', async () => {
  const f = fixture(); f.station.support = {};
  await f.officialOpen();
  assert.match(f.officialHTML(), /暂不支持/);
  assert.match(f.officialHTML(), /data-act="remoteStationProbe"/);
  assert.doesNotMatch(f.officialHTML(), /data-act="remoteStationSelect"/);
  await f.context.ACTIONS.remoteStationSelect(f.button());
  assert.equal(f.writes().length, 0);
});

test('official selection uses fresh server-side requirement and never sends account credentials', async () => {
  const f = fixture(); await f.officialOpen();
  await f.context.ACTIONS.remoteStationSelect(f.button());
  assert.deepEqual(f.writes(), [{ path: '/api/remote/discover', body: {
    url: 'https://relay.fixture', hubUrl: f.station.hubUrl, connect: true, requireOfficial: true,
  } }]);
  assert.equal(f.context.location.hash, 'login'); assert.equal(f.context.S.pairQR, '');
  assert.match(f.context.officialRemoteSupportHTML(f.official), /1 站/);
});

test('station failure remains gray even when the follow-up list read also fails', async () => {
  const f = fixture(); await f.officialOpen();
  f.state.intercept = () => Promise.reject(new Error('fixture network error'));
  await f.context.ACTIONS.remoteStationProbe(f.button());
  assert.doesNotMatch(f.officialHTML(), /is-supported/);
  assert.match(f.nodes.get('#officialError').innerHTML, /network error/);
});

test('a successful station retry clears the preceding error without leaving a stale failure message', async () => {
  const f = fixture(); await f.officialOpen();
  f.state.intercept = () => Promise.reject(new Error('fixture network error'));
  await f.context.ACTIONS.remoteStationProbe(f.button());
  f.state.intercept = null;
  await f.context.ACTIONS.remoteStationProbe(f.button());
  assert.equal(f.nodes.get('#officialError').textContent, '');
  assert.match(f.officialHTML(), /remoteStationSelect/);
});

test('late station check cannot repaint a newer dialog or report unrelated errors', async () => {
  const f = fixture(), pending = gate(); await f.officialOpen();
  f.state.intercept = path => path === '/api/remote/discover' ? pending.promise : undefined;
  const checking = f.context.ACTIONS.remoteStationProbe(f.button());
  f.context.officialPanel(); const next = f.nodes.get('#officialPanel');
  pending.reject(new Error('old failure')); await checking;
  assert.equal(f.nodes.get('#officialPanel'), next);
  assert.equal(f.nodes.get('#officialError').textContent, '');
});

test('duplicate station selections issue only one connect', async () => {
  const f = fixture(), pending = gate(); await f.officialOpen();
  f.state.intercept = path => path === '/api/remote/discover' ? pending.promise : undefined;
  const one = f.context.ACTIONS.remoteStationSelect(f.button()); const two = f.context.ACTIONS.remoteStationSelect(f.button());
  assert.match(f.officialHTML(), /API 远程 · 检测中/);
  assert.match(f.officialHTML(), /disabled>处理中/);
  assert.equal(f.writes().length, 1); pending.resolve({ ok: true }); await Promise.all([one, two]);
});

test('replacement official identity during picker read never exposes selectable stations', async () => {
  const f = fixture();
  f.state.intercept = path => path === '/api/local/accounts' ? { ...f.local, accounts: [{ ...f.official, sourceRevision: NEXT }] } : undefined;
  await f.officialOpen();
  assert.match(f.nodes.get('#officialError').textContent, /账号已更改/);
  assert.doesNotMatch(f.officialHTML(), /remoteStationSelect/);
});

test('saved-site names and endpoints are escaped and declarations never infer support from a hostname', () => {
  const f = fixture(); f.station.name = '<img src=x>'; f.station.support = {};
  const html = f.context.remoteSavedStationsHTML({ remoteConnections: [f.station] });
  assert.doesNotMatch(html, /<img|is-supported/);
  assert.match(html, /&lt;img/); assert.match(html, /待检测/);
});
