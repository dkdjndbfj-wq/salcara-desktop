'use strict';

// UI-only race contracts. These fixtures never connect to a Hub or a local
// Bridge; they only hold synthetic responses until the page has been replaced.
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.resolve(__dirname, '../../internal/console/web/remote.js'), 'utf8');
const connectionSource = fs.readFileSync(path.resolve(__dirname, '../../internal/console/web/remote-connect.js'), 'utf8');
const appSource = fs.readFileSync(path.resolve(__dirname, '../../internal/console/web/app.js'), 'utf8');
const apiSource = appSource.slice(appSource.indexOf('async function api('), appSource.indexOf('/* 系统相关的文案'));
const clockDiagnostic = { direction: 'local_behind', offsetMs: 60000, localTime: 1700000000000, signedTime: 1700000060000, source: 'signed_manifest' };

function deferred() { let resolve, reject; const promise = new Promise((done, fail) => { resolve = done; reject = fail; }); return { promise, resolve, reject }; }
function fixture() {
  const calls = [], pending = [], notices = [], routed = [], listeners = new Map();
  const nodes = new Map();
  const node = id => { if (!nodes.has(id)) nodes.set(id, { id, innerHTML: '', textContent: '', isConnected: true, classList: { toggle() {}, remove() {}, contains() { return false; } } }); return nodes.get(id); };
  const context = vm.createContext({
    AbortController,
    PAGES: { login: {}, sessions: {}, projects: {} },
    ICONS: {}, ACTIONS: {}, S: { route: 'sessions', approvals: [], sessions: [], openKey: null, tl: [] },
    document: { addEventListener(type, fn) { const handlers = listeners.get(type) || []; handlers.push(fn); listeners.set(type, handlers); }, getElementById: id => node(id), querySelector() { return null; }, querySelectorAll() { return []; } },
    window: { addEventListener() {} },
    setInterval: () => 1, clearInterval() {}, setTimeout: () => 1, clearTimeout() {}, URL,
    $: selector => node(String(selector).replace(/^#/, '')),
    $$: () => [],
    icon: () => '', esc: value => String(value ?? ''),
    api: (pathName, body) => { calls.push({ path: pathName, body }); const gate = deferred(); pending.push({ path: pathName, body, ...gate }); return gate.promise; },
    route() { routed.push(true); }, loadState: async () => ({ config: { loggedIn: false, relayRoot: '', hubUrl: '', remoteConnections: [] }, hub: { state: 'not_logged_in' } }),
    toast(message, kind) { notices.push({ message, kind }); }, confirm: async () => true, busy: async (_button, fn) => fn(), copyText() {}, OSX: () => 'win', relTime: () => 'now', location: { hash: '' },
    renderText: value => String(value ?? ''), renderDiff: () => '', tlApply() {}, KIND_ICON: {}, canOpenNativeSession: () => false,
  });
  vm.runInContext(connectionSource, context, { filename: 'remote-connect.js' });
  vm.runInContext(source, context, { filename: 'remote.js' });
  return { context, calls, pending, node, notices, routed, emit(type, target) { for (const fn of listeners.get(type) || []) fn({ target }); } };
}

function connectionFixture(mode = 'station') {
  const f = fixture();
  f.context.S.route = 'login';
  f.context.S.routeRevision = 1;
  f.context.S.state = { config: { deviceId: 'test-device', loggedIn: false, effectiveHubUrl: 'https://old.test/salcara-hub', remoteAgentSettingsScope: 'scope-original', remoteConnections: [] }, hub: { state: 'not_logged_in' } };
  vm.runInContext(`RM.remoteMode = ${JSON.stringify(mode)}`, f.context);
  f.node('rmStation').value = 'https://one.test';
  f.node('rmHub').value = '';
  f.node('rmEnrollmentClaim').value = 'a'.repeat(64);
  return f;
}

function registeredConnection(f, hubUrl = 'https://one.test/salcara-hub') {
  const registration = { deviceId: f.context.S.state.config.deviceId, hubUrl };
  f.context.loadState = async () => {
    Object.assign(f.context.S.state.config, { ...registration, loggedIn: true, effectiveRemoteMode: 'hub', effectiveHubUrl: hubUrl, remoteAgentSettingsScope: 'scope-registered' });
    f.context.S.state.hub = { ...registration, state: 'connecting' };
    return f.context.S.state;
  };
  return { ok: true, registered: true, stage: 'waiting_online', registration };
}

function apiConnectionFixture() {
  const f = connectionFixture();
  const account = { id: 'api-a', name: 'A', sourceRevision: 'src-v1-' + 'a'.repeat(64), baseUrl: 'https://api-a.test/tenant/v1', sourceType: 'api_key', key: 'synthetic-key-never-forward' };
  f.context.S.local = { accounts: [account] };
  f.node('rmSourceAPI').value = account.id;
  f.emit('change', f.node('rmSourceAPI'));
  f.node('rmEnrollmentFallback').hidden = true;
  return { ...f, account };
}

function switchFixture(mode = 'station') {
  const f = connectionFixture(mode);
  Object.assign(f.context.S.state.config, { loggedIn: true, effectiveRemoteMode: 'hub', effectiveHubUrl: 'https://shared.test/hub',
    remoteModes: { bot: { groupId: 'alpha' } }, remoteConnections: [
      { mode: 'hub', hubUrl: 'https://shared.test/hub' }, { mode: 'hub', hubUrl: 'https://second.test/hub' },
      { mode: 'bot', hubUrl: 'https://shared.test/hub', groupId: 'alpha' }, { mode: 'bot', hubUrl: 'https://shared.test/hub', groupId: 'beta' },
    ] });
  f.context.S.state.hub = { state: 'connected' };
  f.context.loadState = async () => f.context.S.state;
  const state = f.context.rmSwitchState();
  f.node('rmSwitchMode').value = state.mode;
  f.node('rmSwitchTarget').value = state.draft.key;
  f.choose = (modeName, hub = '', group = '') => {
    const key = f.context.rmConnectionTarget(modeName, hub, group).key;
    f.node('rmSwitchTarget').value = key;
    f.emit('change', f.node('rmSwitchTarget'));
    return key;
  };
  f.apply = () => f.context.ACTIONS.rmActivateMode({ isConnected: true, dataset: { target: 'switch' } });
  return f;
}

test('right-side mode and target selection only browse and preserve separate form drafts', async () => {
  const f = switchFixture();
  f.node('rmStation').value = 'https://unfinished.test/v1'; f.node('rmHub').value = 'https://private.test/hub';
  f.node('rmSwitchMode').value = 'bot'; f.emit('change', f.node('rmSwitchMode'));
  assert.equal(vm.runInContext('RM.remoteMode', f.context), 'bot');
  assert.equal(vm.runInContext('RM.stationDraft.url', f.context), 'https://unfinished.test/v1');
  f.context.rmSwitchState();
  const bot = f.choose('bot', 'https://shared.test/hub', 'beta');
  f.node('rmBotBinding').value = 'unfinished invite';
  f.node('rmSwitchMode').value = 'hub'; f.emit('change', f.node('rmSwitchMode'));
  f.context.rmSwitchState();
  assert.equal(vm.runInContext('RM.botDraft', f.context), 'unfinished invite');
  f.node('rmSwitchMode').value = 'bot'; f.emit('change', f.node('rmSwitchMode'));
  assert.equal(f.context.rmSwitchState().draft.key, bot);
  assert.equal(f.context.S.state.config.effectiveRemoteMode, 'hub');
  assert.match(f.node('rmSwitchNote').textContent, /仅选择了待切换目标/);
  assert.equal(f.calls.length, 0);
});

test('right-side switch freezes exact Hub and group and waits for acknowledged effective identity', async () => {
  for (const row of [{ mode: 'hub', hub: 'https://second.test/hub', group: '' }, { mode: 'bot', hub: 'https://shared.test/hub', group: 'beta' }, { mode: 'direct', hub: '', group: '' }]) {
    const f = switchFixture(row.mode === 'hub' ? 'station' : row.mode);
    f.choose(row.mode, row.hub, row.group);
    const switching = f.apply();
    const expected = { mode: row.mode };
    if (row.hub) expected.hubUrl = row.hub;
    if (row.group) expected.groupId = row.group;
    assert.deepEqual(JSON.parse(JSON.stringify(f.calls[0].body)), expected);
    assert.equal(f.context.S.state.config.effectiveRemoteMode, 'hub');
    assert.equal(f.notices.length, 0);
    await f.apply();
    assert.equal(f.calls.length, 1, 'all connection mutations share one lease');
    f.context.loadState = async () => { Object.assign(f.context.S.state.config, { effectiveRemoteMode: row.mode, effectiveHubUrl: row.hub, remoteModes: { bot: { groupId: row.group } } }); return f.context.S.state; };
    f.pending[0].resolve({ ok: true, activeMode: row.mode });
    await switching;
    assert.match(f.notices.at(-1).message, /已启用/);
    assert.equal(f.context.rmSwitchState().current, true);
    assert.equal(f.routed.length, 0, 'successful switch updates status without replacing drafts');
    assert.equal(f.calls.some(call => /session|discover|pair|history/.test(call.path)), false);
  }
});

test('switch failure and both selector nodes survive status refresh and explicit mode round trips', async () => {
  const f = switchFixture('bot'), key = f.choose('bot', 'https://shared.test/hub', 'beta');
  const modeNode = f.node('rmSwitchMode'), targetNode = f.node('rmSwitchTarget');
  modeNode.innerHTML = 'unchanged mode options'; targetNode.innerHTML = 'unchanged exact targets';
  const switching = f.apply();
  f.pending[0].reject(Object.assign(new Error('saved target denied'), { status: 409 }));
  await switching;
  assert.match(f.node('rmSwitchError').innerHTML, /saved target denied/);
  f.context.rmUpdateRemoteStatus();
  assert.equal(f.node('rmSwitchMode'), modeNode); assert.equal(f.node('rmSwitchTarget'), targetNode);
  assert.equal(targetNode.value, key); assert.equal(targetNode.innerHTML, 'unchanged exact targets');
  assert.equal(modeNode.innerHTML, 'unchanged mode options');
  modeNode.value = 'hub'; f.emit('change', modeNode); f.context.rmSwitchState();
  modeNode.value = 'bot'; f.emit('change', modeNode);
  assert.equal(f.context.rmSwitchState().draft.key, key);
  assert.match(f.context.rmConnectionSwitchMarkup(f.context.S.state.config), /saved target denied/);
  assert.equal(f.context.S.state.config.effectiveRemoteMode, 'hub');
});

test('removed target stays visible as unavailable and cannot silently switch to another group', async () => {
  const f = switchFixture('bot'), key = f.choose('bot', 'https://shared.test/hub', 'beta');
  f.context.S.state.config.remoteConnections = f.context.S.state.config.remoteConnections.filter(row => row.groupId !== 'beta');
  f.context.rmUpdateRemoteStatus();
  assert.equal(f.node('rmSwitchTarget').value, key);
  assert.equal(f.node('rmSwitchApply').disabled, true);
  assert.match(f.node('rmSwitchNote').textContent, /已不在已保存连接/);
  assert.match(f.context.rmConnectionSwitchMarkup(f.context.S.state.config), /beta.*已不可用/);
  await f.apply(); assert.equal(f.calls.length, 0);
});

test('a mode-only acknowledgement cannot claim the selected Bot group was enabled', async () => {
  const f = switchFixture('bot');
  f.choose('bot', 'https://shared.test/hub', 'beta');
  const switching = f.apply();
  f.context.S.state.config.effectiveRemoteMode = 'bot'; // still group alpha
  f.pending[0].resolve({ ok: true, activeMode: 'bot' });
  await switching;
  assert.equal(f.notices.length, 0);
  assert.match(f.node('rmSwitchError').innerHTML, /目标状态尚未核对/);
  assert.equal(f.context.rmSwitchState().current, false);
});

test('late submitted switch reconciles without erasing newer selection or navigating back', async () => {
  for (const leave of [false, true]) {
    const f = switchFixture('bot');
    f.choose('bot', 'https://shared.test/hub', 'beta');
    const switching = f.apply();
    const newer = f.choose('bot', 'https://shared.test/hub', 'alpha');
    if (leave) { f.context.S.route = 'accounts'; f.context.S.routeRevision++; }
    let refreshes = 0;
    f.context.loadState = async () => { refreshes++; f.context.S.state.config.effectiveRemoteMode = 'bot'; f.context.S.state.config.remoteModes.bot.groupId = 'beta'; return f.context.S.state; };
    f.pending[0].resolve({ ok: true, activeMode: 'bot' });
    await switching;
    assert.equal(refreshes, 1);
    assert.equal(vm.runInContext('RM.switchDraft.key', f.context), newer);
    assert.equal(f.notices.length, 0); assert.equal(f.routed.length, 0);
    assert.equal(f.node('rmSwitchError').innerHTML, '');
    assert.equal(vm.runInContext('RM.connectionAction', f.context), null);
  }
});

test('changed switch draft or removed saved identity cancels delayed pre-submit work', async () => {
  for (const change of ['selection', 'removed', 'route']) {
    const f = switchFixture('bot');
    f.choose('bot', 'https://shared.test/hub', 'beta');
    const gate = deferred();
    f.context.busy = async (_button, run) => { await gate.promise; return run(); };
    const switching = f.apply();
    if (change === 'selection') f.choose('bot', 'https://shared.test/hub', 'alpha');
    if (change === 'removed') f.context.S.state.config.remoteConnections = [];
    if (change === 'route') f.context.S.routeRevision++;
    gate.resolve(); await switching;
    assert.equal(f.calls.length, 0, change);
    assert.equal(f.notices.length, 0, change);
  }
});

test('abandoned switch preflight cannot block or clear a newer lease', async () => {
  const f = switchFixture('bot');
  f.choose('bot', 'https://shared.test/hub', 'beta');
  const gate = deferred(); let starts = 0;
  f.context.busy = async (_button, run) => { if (++starts === 1) await gate.promise; return run(); };
  const obsolete = f.apply();
  f.choose('bot', 'https://shared.test/hub', 'alpha');
  assert.equal(f.node('rmSwitchApply').disabled, false);
  const current = f.apply();
  assert.equal(f.calls.length, 1); assert.equal(f.calls[0].body.groupId, 'alpha');
  gate.resolve(); await obsolete;
  assert.equal(vm.runInContext('RM.connectionAction.submitted', f.context), true);
  f.context.loadState = async () => { f.context.S.state.config.effectiveRemoteMode = 'bot'; return f.context.S.state; };
  f.pending[0].resolve({ ok: true, activeMode: 'bot' }); await current;
  assert.equal(vm.runInContext('RM.connectionAction', f.context), null);
});

test('a late failed switch cannot attach its error to a reselected same logical target', async () => {
  const f = switchFixture('bot');
  f.choose('bot', 'https://shared.test/hub', 'beta');
  const old = f.apply();
  f.choose('bot', 'https://shared.test/hub', 'alpha');
  assert.match(f.node('rmSwitchNote').textContent, /此前目标.*beta.*当前选择尚未提交/);
  f.choose('bot', 'https://shared.test/hub', 'beta');
  f.pending[0].reject(Object.assign(new Error('obsolete denial'), { status: 409 }));
  await old;
  assert.equal(f.context.rmSwitchState().draft.error, '');
  assert.equal(f.node('rmSwitchError').innerHTML, '');
  assert.equal(f.notices.length, 0);
});

test('an opened remote API section reads only original agent configuration and renders all four save controls', async () => {
  const f = switchFixture(), view = f.node('view');
  f.context.renderRemoteLoginPage(view, f.context.S.state);
  assert.equal(f.calls.some(call => call.path === '/api/remote/agents'), false);
  assert.equal(f.calls.some(call => /session|history/.test(call.path)), false);
  const section = f.node('rmAgentSettings'); section.open = true;
  f.emit('toggle', section);
  const read = f.pending.find(call => call.path === '/api/remote/agents');
  assert.ok(read);
  read.resolve({ settingsScope: 'scope-original', agents: [{ id: 'codex', available: true, api: { source: 'phone', accountId: 'remote-api', model: 'm' } }], apis: [{ id: 'remote-api', name: 'Remote', models: ['m'] }, { id: 'denied', name: 'Denied', remoteEligibility: { supported: false, reason: 'external_api_not_allowed' } }] });
  await new Promise(setImmediate);
  const html = f.node('rmAgents').innerHTML;
  for (const id of ['codex', 'claude', 'deepseek-harness', 'hermes']) {
    assert.match(html, new RegExp(`data-rm-api="${id}"`));
    assert.match(html, new RegExp(`data-rm-model="${id}"`));
    assert.match(html, new RegExp(`data-act="rmSaveApi" data-agent="${id}"`));
  }
  assert.match(html, /跟随电脑设置/); assert.match(html, /本站未授权此 API 地址/);
  assert.equal(f.calls.some(call => /session|history/.test(call.path)), false);
});

test('late remote API section read cannot replace a newer page catalog or visible controls', async () => {
  const f = switchFixture(), newer = { agents: [{ id: 'newer' }] };
  const read = f.context.rmLoadAgents();
  f.context.S.route = 'accounts';
  vm.runInContext(`RM.agents = ${JSON.stringify(newer)}`, f.context);
  f.node('rmAgents').innerHTML = 'newer page';
  f.pending[0].resolve({ agents: [{ id: 'old' }], apis: [] });
  await read;
  assert.equal(f.node('rmAgents').innerHTML, 'newer page');
  assert.equal(vm.runInContext('RM.agents.agents[0].id', f.context), 'newer');
});

test('remote API controls stay unavailable when either side has no settings scope', async () => {
  for (const missing of ['state', 'catalog']) {
    const f = switchFixture();
    if (missing === 'state') delete f.context.S.state.config.remoteAgentSettingsScope;
    const reading = f.context.rmLoadAgents();
    f.pending[0].resolve({ settingsScope: missing === 'catalog' ? undefined : 'scope-original', agents: [], apis: [] });
    await reading;
    assert.match(f.node('rmAgents').innerHTML, /设置范围未确认/);
    assert.doesNotMatch(f.node('rmAgents').innerHTML, /data-act="rmSaveApi"/);
    await f.context.ACTIONS.rmSaveApi({ dataset: { agent: 'codex' } });
    assert.equal(f.calls.length, 1);
    assert.equal(f.notices.length, 0);
  }
});

test('active Hub pairing actions still work while browsing Bot or direct without switching connection', async () => {
  for (const browse of ['bot', 'direct']) {
    const f = switchFixture(browse);
    const showing = f.context.ACTIONS.rmShowQR({});
    assert.equal(f.calls[0].path, '/api/pair/start');
    f.pending[0].resolve({ code: 'fixture', qrUrl: 'data:active-hub', expiresAt: Date.now() + 60000 });
    await showing;
    assert.match(f.context.rmPhoneBindingMarkup(), /data:active-hub/);
    const revoking = f.context.ACTIONS.pairRevoke({});
    await new Promise(setImmediate);
    assert.equal(f.calls[1].path, '/api/pair/revoke');
    f.pending[1].resolve({ ok: true }); await revoking;
    assert.equal(f.context.S.state.config.effectiveRemoteMode, 'hub');
    assert.equal(f.calls.some(call => call.path === '/api/remote/mode'), false);
  }
});

test('Hub pairing confirmation cannot cross a changed active identity', async () => {
  const f = switchFixture('bot'), gate = deferred();
  f.context.confirm = () => gate.promise;
  const revoking = f.context.ACTIONS.pairRevoke({});
  f.context.S.state.config.effectiveHubUrl = 'https://different.test/hub';
  gate.resolve(true); await revoking;
  assert.equal(f.calls.length, 0);
});

test('active direct status and revocation remain reachable while browsing station', async () => {
  const f = switchFixture();
  f.context.S.state.config.effectiveRemoteMode = 'direct';
  f.context.renderRemoteLoginPage(f.node('view'), f.context.S.state);
  const read = f.pending.find(call => call.path === '/api/local/direct/status');
  assert.ok(read, 'render reads the active direct connection even with station form shown');
  read.resolve({ enabled: true, running: true, connected: true, paired: true });
  await new Promise(setImmediate);
  assert.match(f.node('rmPhoneBinding').innerHTML, /手机已连接/);
  assert.match(f.node('rmPhoneBinding').innerHTML, /rmDirectRevoke/);
  const refresh = f.context.ACTIONS.rmRecoveryRefresh({});
  await new Promise(setImmediate);
  f.pending.filter(call => call.path === '/api/local/direct/status').at(-1).resolve({ enabled: true, running: true, paired: true });
  await refresh;
  const revoking = f.context.ACTIONS.rmDirectRevoke({});
  await new Promise(setImmediate);
  f.pending.find(call => call.path === '/api/local/direct/revoke').resolve({ ok: true });
  await new Promise(setImmediate);
  f.pending.filter(call => call.path === '/api/local/direct/status').at(-1).resolve({ enabled: false, running: false, paired: false });
  await revoking;
  assert.equal(f.calls.some(call => /session|history/.test(call.path)), false);
});

test('switch acknowledgement changes phone binding to the enabled target, not the form draft', async () => {
  const f = switchFixture('bot');
  f.choose('bot', 'https://shared.test/hub', 'beta');
  f.context.rmUpdateRemoteStatus();
  assert.match(f.node('rmPhoneBinding').innerHTML, /rmShowQR/);
  const switching = f.apply();
  assert.match(f.node('rmPhoneBinding').innerHTML, /rmShowQR/);
  f.context.loadState = async () => { f.context.S.state.config.effectiveRemoteMode = 'bot'; f.context.S.state.config.remoteModes.bot.groupId = 'beta'; return f.context.S.state; };
  f.pending[0].resolve({ ok: true, activeMode: 'bot' }); await switching;
  assert.match(f.node('rmPhoneBinding').innerHTML, /同一份 Bot/);
  assert.doesNotMatch(f.node('rmPhoneBinding').innerHTML, /rmShowQR|pairRevoke/);
});

test('remote Agent API reads cannot carry the former effective Hub policy into a newly active target', async () => {
  const f = switchFixture(), read = f.context.rmLoadAgents();
  f.node('rmAgents').innerHTML = 'new target controls';
  f.context.S.state.config.effectiveHubUrl = 'https://second.test/hub';
  f.pending[0].resolve({ agents: [], apis: [{ id: 'former-policy' }] });
  await read;
  assert.equal(f.node('rmAgents').innerHTML, 'new target controls');
  assert.equal(vm.runInContext('RM.agents', f.context), null);
});

async function agentApiFixture() {
  const f = switchFixture();
  vm.runInContext('RM.agentSettingsOpen = true', f.context);
  const catalog = { settingsScope: 'scope-original', agents: [{ id: 'codex', available: true, api: { source: 'phone', accountId: 'allowed', model: 'old' } }], apis: [{ id: 'allowed', name: 'Allowed', models: ['old', 'new'] }] };
  const reading = f.context.rmLoadAgents(); f.pending[0].resolve(catalog); await reading;
  f.node('rmAgentApi-codex').value = 'allowed'; f.node('rmAgentApi-codex').dataset = { rmApi: 'codex' };
  f.node('rmAgentModel-codex').value = 'new'; f.node('rmAgentModel-codex').dataset = { rmModel: 'codex' };
  f.save = () => f.context.ACTIONS.rmSaveApi({ dataset: { agent: 'codex' } });
  f.ack = (model = 'new') => ({ ...catalog, agents: [{ id: 'codex', available: true, api: { source: 'phone', accountId: 'allowed', model, name: 'Allowed' } }] });
  return f;
}

test('remote API save uses the connection lease, freezes values and preserves same-identity drafts', async () => {
  const f = await agentApiFixture(), box = f.node('rmAgents'), markup = box.innerHTML;
  f.node('rmAgentModel-claude').value = 'another unsaved draft';
  const saving = f.save();
  await f.save();
  f.choose('hub', 'https://second.test/hub'); await f.apply();
  assert.equal(f.calls.length, 2); // original GET, one API save; no mode mutation
  assert.deepEqual(JSON.parse(JSON.stringify(f.calls[1].body)), { agent: 'codex', accountId: 'allowed', model: 'new', expectedSettingsScope: 'scope-original' });
  f.pending[1].resolve(f.ack()); await saving;
  assert.equal(box.innerHTML, markup);
  assert.equal(f.node('rmAgentModel-codex').value, 'new');
  assert.equal(f.node('rmAgentModel-claude').value, 'another unsaved draft');
  await f.context.rmLoadAgents(); f.context.rmUpdateRemoteStatus();
  assert.equal(box.innerHTML, markup, 'same-identity refresh never rebuilds user inputs');
  assert.equal(f.calls.length, 2);
});

test('acknowledged remote API save updates only its summary and follow-computer clears the submitted model', async () => {
  const f = await agentApiFixture();
  const saving = f.save(); f.pending[1].resolve(f.ack()); await saving;
  assert.match(f.node('rmAgentSummary-codex').textContent, /Allowed · new/);
  assert.match(f.notices.at(-1).message, /已保存/);
  f.node('rmAgentApi-codex').value = ''; f.emit('change', f.node('rmAgentApi-codex'));
  const following = f.save();
  assert.equal(f.calls[2].body.model, '');
  f.pending[2].resolve({ settingsScope: 'scope-original', agents: [{ id: 'codex', api: { source: 'tool' } }], apis: [] }); await following;
  assert.equal(f.node('rmAgentSummary-codex').textContent, '跟随电脑设置');
});

test('changed active identity removes old Agent inputs immediately and blocks saving during replacement read', async () => {
  const f = await agentApiFixture();
  f.context.S.state.config.effectiveHubUrl = 'https://second.test/hub';
  f.context.S.state.config.remoteAgentSettingsScope = 'scope-replaced';
  f.context.S.state.hub.hubUrl = 'https://shared.test/hub'; // stale status cannot bypass form invalidation
  f.context.rmUpdateRemoteStatus();
  assert.doesNotMatch(f.node('rmAgents').innerHTML, /data-act="rmSaveApi"/);
  assert.match(f.node('rmAgents').innerHTML, /正在读取当前连接/);
  await f.save();
  assert.equal(f.calls.length, 2); assert.equal(f.calls[1].path, '/api/remote/agents');
  f.pending[1].resolve({ settingsScope: 'scope-replaced', agents: [], apis: [] }); await new Promise(setImmediate);
  assert.equal(vm.runInContext('RM.agentForm.ready', f.context), true);
});

test('a late API save cannot repaint or notify the replacement Hub form', async () => {
  for (const fail of [false, true]) {
    const f = await agentApiFixture(), saving = f.save();
    f.context.S.state.config.effectiveHubUrl = 'https://second.test/hub';
    f.context.S.state.config.remoteAgentSettingsScope = 'scope-replaced';
    f.context.rmUpdateRemoteStatus();
    f.pending[2].resolve({ settingsScope: 'scope-replaced', agents: [], apis: [] }); await new Promise(setImmediate);
    const replacement = f.node('rmAgents').innerHTML;
    if (fail) f.pending[1].reject(Object.assign(new Error('old hub denial'), { status: 409 }));
    else f.pending[1].resolve(f.ack());
    await saving;
    assert.equal(f.node('rmAgents').innerHTML, replacement);
    assert.equal(f.notices.length, 0);
    assert.equal(f.node('rmAgentResult-codex').innerHTML, '');
    assert.equal(vm.runInContext('RM.agentForm.catalog.apis.length', f.context), 0);
  }
});

test('model edits cancel API preflight and late save acknowledgements do not overwrite newer model drafts', async () => {
  for (const beforeSubmit of [false, true]) {
    const f = await agentApiFixture(), gate = deferred();
    if (beforeSubmit) f.context.busy = async (_button, run) => { await gate.promise; return run(); };
    const saving = f.save();
    f.node('rmAgentModel-codex').value = 'newer draft'; f.emit('input', f.node('rmAgentModel-codex'));
    if (beforeSubmit) gate.resolve(); else f.pending[1].resolve(f.ack());
    await saving;
    assert.equal(f.node('rmAgentModel-codex').value, 'newer draft');
    assert.equal(f.notices.length, 0);
    assert.equal(f.calls.length, beforeSubmit ? 1 : 2);
  }
});

test('remote API failure or missing acknowledgement preserves input and exposes no optimistic success', async () => {
  for (const rejected of [false, true]) {
    const f = await agentApiFixture(), saving = f.save();
    if (rejected) f.pending[1].reject(Object.assign(new Error('pending approval blocks change'), { status: 409 }));
    else f.pending[1].resolve({ settingsScope: 'scope-original', ok: true });
    await saving;
    assert.match(f.node('rmAgentResult-codex').innerHTML, rejected ? /pending approval/ : /服务器未确认/);
    assert.equal(f.node('rmAgentModel-codex').value, 'new'); assert.equal(f.notices.length, 0);
  }
});

test('remote API save rejects missing or different acknowledgement scope without changing the draft', async () => {
  for (const settingsScope of [undefined, 'scope-other']) {
    const f = await agentApiFixture(), saving = f.save();
    f.pending[1].resolve({ ...f.ack(), settingsScope });
    await saving;
    assert.match(f.node('rmAgentResult-codex').innerHTML, /设置范围已变化或保存结果未确认/);
    assert.equal(vm.runInContext('RM.agentForm.ready', f.context), false);
    assert.equal(f.node('rmAgentModel-codex').value, 'new');
    assert.equal(f.notices.length, 0);
    await f.save();
    assert.equal(f.calls.length, 2, 'an unconfirmed scope cannot be saved again without rereading');
  }
});

test('current direct QR generation works across browsed forms and remains identity-owned', async () => {
  const f = switchFixture(); f.context.S.state.config.effectiveRemoteMode = 'direct';
  const generating = f.context.ACTIONS.rmDirectQR({});
  assert.equal(f.calls[0].path, '/api/local/direct/qr');
  f.pending[0].resolve({ enabled: true, qrDataURL: 'data:current-direct', expiresAt: Date.now() + 60000 });
  await new Promise(setImmediate);
  f.pending[1].resolve({ enabled: true, running: true, paired: false }); await generating;
  assert.match(f.node('rmPhoneBinding').innerHTML, /data:current-direct/);
  assert.equal(vm.runInContext('RM.remoteMode', f.context), 'station');
  assert.equal(f.calls.some(call => /session|history/.test(call.path)), false);
  const stale = switchFixture(); stale.context.S.state.config.effectiveRemoteMode = 'direct';
  const gate = deferred(); stale.context.busy = async (_button, run) => { await gate.promise; return run(); };
  const abandoned = stale.context.ACTIONS.rmDirectQR({});
  stale.context.S.state.config.effectiveRemoteMode = 'hub'; gate.resolve(); await abandoned;
  assert.equal(stale.calls.length, 0);
});

test('API enrollment freezes local identity and revision through probe and registration without exposing key', async () => {
  const f = apiConnectionFixture();
  const connecting = f.context.ACTIONS.rmConnect({});
  assert.deepEqual(JSON.parse(JSON.stringify(f.calls[0].body)), { url: f.account.baseUrl, hubUrl: '', accountId: f.account.id, expectedSourceRevision: f.account.sourceRevision });
  f.pending[0].resolve({ ok: true, stage: 'discovered', registered: false, eligibility: { supported: true, method: 'api' } });
  await new Promise(setImmediate);
  assert.deepEqual(JSON.parse(JSON.stringify(f.calls[1].body)), { connect: true, ...JSON.parse(JSON.stringify(f.calls[0].body)) });
  assert.equal(JSON.stringify(f.calls).includes(f.account.key), false);
  assert.equal(f.calls[1].body.enrollmentClaim, undefined);
  f.pending[1].resolve(registeredConnection(f, 'https://api-a.test/salcara-hub'));
  await connecting;
  assert.match(f.notices.at(-1).message, /登记已确认.*等待连接上线/);
  assert.equal(f.calls.some(call => /session|history/.test(call.path)), false);
});

test('an advertised API capability never marks actual enrollment eligibility confirmed', async () => {
  for (const oldOnline of [false, true]) {
    const f = apiConnectionFixture();
    if (oldOnline) { f.context.S.state.config.effectiveRemoteMode = 'hub'; f.context.S.state.config.loggedIn = true; f.context.S.state.hub.state = 'connected'; }
    const connecting = f.context.ACTIONS.rmConnect({});
    f.pending[0].resolve({ ok: true, stage: 'discovered', registered: false, eligibility: { supported: true, method: 'api' } });
    await new Promise(setImmediate);
    assert.match(f.node('rmConnectionSteps').innerHTML, /远程服务已发现/);
    assert.doesNotMatch(f.node('rmConnectionSteps').innerHTML, /服务器已确认授权|服务器已确认电脑登记/, String(oldOnline));
    f.pending[1].reject(Object.assign(new Error('invalid key'), { status: 401, code: 'api_credentials_invalid' }));
    await connecting;
    assert.equal(f.notices.length, 0);
    assert.match(f.node('rmConnectError').innerHTML, /API 凭据无效或已过期/);
  }
});

test('changing a selected API revision or its URL during discovery prevents registration', async () => {
  for (const change of ['revision', 'base', 'selection']) {
    const f = apiConnectionFixture();
    const connecting = f.context.ACTIONS.rmConnect({});
    if (change === 'revision') f.account.sourceRevision = 'src-v1-' + 'b'.repeat(64);
    if (change === 'base') f.account.baseUrl = 'https://other.test/v1';
    if (change === 'selection') f.node('rmSourceAPI').value = 'other-api';
    f.pending[0].resolve({ ok: true, stage: 'discovered', eligibility: { supported: true, method: 'api' } });
    await connecting;
    assert.equal(f.calls.length, 1, change);
    assert.equal(f.notices.length, 0, change);
  }
});

test('a delayed page account read cannot restore an older API source revision', async () => {
  const f = connectionFixture();
  f.context.loadState = async () => f.context.S.state;
  f.context.S.localAccountsRevision = 1;
  f.context.RENDER_login(f.node('view'));
  const read = pendingFor(f, '/api/local/accounts');
  const current = { accounts: [{ id: 'a', sourceType: 'api_key', sourceRevision: 'new', baseUrl: 'https://new.test/v1' }] };
  f.context.S.localAccountsRevision++;
  f.context.S.local = current;
  read.resolve({ accounts: [{ id: 'a', sourceType: 'api_key', sourceRevision: 'old', baseUrl: 'https://old.test/v1' }] });
  await new Promise(setImmediate);
  assert.equal(f.context.S.local, current);
});

test('editing address B detaches selected API A and never submits its identity to B', async () => {
  const f = apiConnectionFixture();
  f.node('rmStation').value = 'https://api-b.test/v1';
  f.node('rmEnrollmentClaim').value = 'b'.repeat(64);
  f.emit('input', f.node('rmStation'));
  assert.equal(f.node('rmSourceAPI').value, '');
  assert.equal(f.node('rmEnrollmentClaim').value, '');
  assert.equal(f.node('rmEnrollmentFallback').hidden, true);
  const connecting = f.context.ACTIONS.rmConnect({});
  assert.deepEqual(JSON.parse(JSON.stringify(f.calls[0].body)), { url: 'https://api-b.test/v1', hubUrl: '' });
  f.pending[0].reject(Object.assign(new Error('standalone device enrollment required'), { status: 403, code: 'device_enrollment_required' }));
  await connecting;
  assert.equal(f.node('rmEnrollmentFallback').hidden, false);
  assert.equal(f.calls.length, 1);
});

test('a mismatched source or missing revision fails before any network request', async () => {
  for (const change of ['url', 'revision']) {
    const f = apiConnectionFixture();
    if (change === 'url') f.node('rmStation').value = 'https://other.test/v1';
    else delete f.account.sourceRevision;
    await f.context.ACTIONS.rmConnect({});
    assert.equal(f.calls.length, 0);
    assert.match(f.node('rmConnectError').innerHTML, /API 已变化/);
  }
});

test('an unsupported API shows administrator recovery without exposing a registration code', async () => {
  const f = apiConnectionFixture();
  const connecting = f.context.ACTIONS.rmConnect({});
  f.pending[0].reject(Object.assign(new Error('capability missing'), { status: 409, code: 'api_enrollment_unsupported' }));
  await connecting;
  assert.equal(f.node('rmEnrollmentFallback').hidden, true);
  assert.match(f.node('rmConnectError').innerHTML, /暂不支持 API 自动登记.*站点管理员/);
  assert.equal(f.notices.length, 0);
});

test('saving a connection without a registration acknowledgement never becomes success or retries', async () => {
  const f = apiConnectionFixture();
  const connecting = f.context.ACTIONS.rmConnect({});
  f.pending[0].resolve({ ok: true, stage: 'discovered', eligibility: { supported: true, method: 'api' } });
  await new Promise(setImmediate);
  f.pending[1].resolve({ ok: true, connected: true, registered: false, stage: 'waiting_online' });
  await connecting;
  assert.equal(f.calls.length, 2);
  assert.equal(f.notices.length, 0);
  assert.match(f.node('rmConnectError').innerHTML, /暂未确认.*尚未确认电脑登记/);
});

test('an unknown registration offers explicit checks and only the retry button sets retryEnrollment', async () => {
  const f = apiConnectionFixture();
  const first = f.context.ACTIONS.rmConnect({});
  f.pending[0].resolve({ ok: true, stage: 'discovered', eligibility: { supported: true, method: 'api' } });
  await new Promise(setImmediate);
  f.pending[1].reject(Object.assign(new Error('电脑登记结果未知'), { status: 409, code: 'enrollment_result_unknown' }));
  await first;
  assert.equal(f.calls.length, 2);
  assert.match(f.node('rmConnectError').innerHTML, /暂未确认/);
  assert.match(f.node('rmConnectError').innerHTML, /检查登记结果/);
  assert.match(f.node('rmConnectError').innerHTML, /data-act="rmRetryEnrollment"/);
  assert.equal(f.calls.some(call => call.body?.retryEnrollment), false);
  const retry = f.context.ACTIONS.rmRetryEnrollment({});
  assert.equal(f.calls[2].body.retryEnrollment, undefined, 'the retry preflight remains read-only');
  f.pending[2].resolve({ ok: true, stage: 'discovered', eligibility: { supported: true, method: 'api' } });
  await new Promise(setImmediate);
  assert.equal(f.calls[3].body.retryEnrollment, true);
  assert.equal(f.calls[3].body.accountId, f.account.id);
  assert.equal(f.calls[3].body.expectedSourceRevision, f.account.sourceRevision);
  f.pending[3].resolve(registeredConnection(f, 'https://api-a.test/salcara-hub'));
  await retry;
  assert.equal(f.calls.length, 4);
  assert.equal(vm.runInContext('RM.pendingEnrollment', f.context), null);
});

test('registration retry cannot cross an API edit or run without a prior unknown result', async () => {
  const f = apiConnectionFixture();
  await f.context.ACTIONS.rmRetryEnrollment({});
  assert.equal(f.calls.length, 0);
  vm.runInContext(`RM.pendingEnrollment = { key: rmCurrentEnrollmentTargetKey() }`, f.context);
  f.account.sourceRevision = 'src-v1-' + 'b'.repeat(64);
  await f.context.ACTIONS.rmRetryEnrollment({});
  assert.equal(f.calls.length, 0);
  f.emit('input', f.node('rmStation'));
  assert.equal(vm.runInContext('RM.pendingEnrollment', f.context), null);
});

test('same-Hub Bot groups remain separate and activation sends the selected group', async () => {
  const f = connectionFixture('bot');
  const cfg = f.context.S.state.config;
  Object.assign(cfg, { effectiveRemoteMode: 'bot', loggedIn: true, remoteModes: { bot: { groupId: 'group-a' } }, remoteConnections: [
    { mode: 'bot', groupId: 'group-a', hubUrl: cfg.effectiveHubUrl, name: 'same name' },
    { mode: 'bot', groupId: 'group-b', hubUrl: cfg.effectiveHubUrl, name: 'same name' },
  ] });
  f.context.S.state.hub.state = 'connected';
  const markup = f.context.rmSavedBotsMarkup(cfg);
  assert.ok(f.context.renderRemoteConnectionLayout(f.node('view'), f.context.S.state).includes(markup), 'saved Bot states remain in the shared connection list');
  assert.equal(f.context.rmSavedConnections(cfg, 'bot').length, 2);
  assert.equal((markup.match(/已连接<\/span>/g) || []).length, 1);
  assert.doesNotMatch(markup, /data-group="group-a"/);
  assert.match(markup, /data-group="group-b" >启用 Bot/);
  const action = f.context.ACTIONS.rmActivateMode({ dataset: { mode: 'bot', hub: cfg.effectiveHubUrl, group: 'group-b' } });
  assert.equal(f.calls[0].body.groupId, 'group-b');
  f.pending[0].resolve({ ok: true, activeMode: 'bot' });
  await action;
});

function botIngressFixture() {
  const f = connectionFixture('bot'), hubUrl = 'https://old-bot.test/salcara-hub';
  f.context.S.state.config.remoteConnections = [{ mode: 'bot', groupId: 'group-a', hubUrl }, { mode: 'bot', groupId: 'group-b', hubUrl }];
  f.node('rmBotIngress-1').value = 'https://new-bot.test/salcara-hub';
  const button = { dataset: { field: 'rmBotIngress-1', hub: hubUrl, group: 'group-b' }, isConnected: true };
  return { ...f, hubUrl, button };
}

test('Bot address recovery targets the saved group without first binding or activating it', async () => {
  const f = botIngressFixture();
  const changing = f.context.ACTIONS.rmBotIngress(f.button);
  assert.deepEqual(JSON.parse(JSON.stringify(f.calls[0])), { path: '/api/remote/ingress', body: { hubUrl: f.hubUrl, groupId: 'group-b', url: 'https://new-bot.test/salcara-hub' } });
  await f.context.ACTIONS.rmBotIngress(f.button);
  assert.equal(f.calls.length, 1, 'same shared lease prevents duplicate recovery');
  f.pending[0].resolve({ ok: true, hubUrl: f.hubUrl, groupId: 'group-b', ingressUrl: 'https://new-bot.test/salcara-hub/v1' });
  await changing;
  assert.match(f.node('rmBotIngressError-1').innerHTML, /原连接身份验证并保存/);
  assert.equal(f.calls.some(call => /bot-bind|remote\/mode|session|history/.test(call.path)), false);
  assert.equal(f.routed.length, 0);
});

test('Bot address recovery preserves the old logical binding when identity verification fails', async () => {
  const f = botIngressFixture(), before = JSON.stringify(f.context.S.state.config.remoteConnections);
  const changing = f.context.ACTIONS.rmBotIngress(f.button);
  f.pending[0].reject(Object.assign(new Error('服务身份已变化'), { status: 409, code: 'hub_identity_changed' }));
  await changing;
  assert.equal(JSON.stringify(f.context.S.state.config.remoteConnections), before);
  assert.match(f.node('rmBotIngressError-1').innerHTML, /身份已变化/);
  assert.equal(f.notices.length, 0);
  assert.equal(f.calls.length, 1);
});

test('a late Bot address recovery cannot repaint a different mode or a newer address draft', async () => {
  for (const change of ['mode', 'draft']) {
    const f = botIngressFixture();
    const changing = f.context.ACTIONS.rmBotIngress(f.button);
    if (change === 'mode') await f.context.ACTIONS.rmMode({ dataset: { mode: 'direct' } });
    else { f.node('rmBotIngress-1').value = 'https://newer.test'; f.emit('input', f.node('rmBotIngress-1')); }
    f.pending[0].resolve({ ok: true, hubUrl: f.hubUrl, groupId: 'group-b', ingressUrl: 'https://new-bot.test/salcara-hub/v1' });
    await changing;
    assert.equal(f.node('rmBotIngressError-1').innerHTML, '', change);
    assert.equal(f.notices.length, 0, change);
  }
});

test('Bot address recovery rejects unsafe URLs and unconfirmed target acknowledgements', async () => {
  for (const url of ['http://new.test', 'https://name:secret@new.test', 'https://new.test/?token=x', 'https://new.test/#secret']) {
    const f = botIngressFixture();
    f.node('rmBotIngress-1').value = url;
    await f.context.ACTIONS.rmBotIngress(f.button);
    assert.equal(f.calls.length, 0, url);
    assert.match(f.node('rmBotIngressError-1').innerHTML, /HTTPS/);
  }
  const f = botIngressFixture();
  const changing = f.context.ACTIONS.rmBotIngress(f.button);
  f.pending[0].resolve({ ok: true, hubUrl: f.hubUrl, groupId: 'group-a', ingressUrl: 'https://new-bot.test/salcara-hub/v1' });
  await changing;
  assert.match(f.node('rmBotIngressError-1').innerHTML, /暂未确认/);
  assert.equal(f.notices.length, 0);
});

test('saved API selection fills its full address locally and clears another station claim', () => {
  const f = connectionFixture();
  f.context.S.local = { accounts: [{ id: 'a', baseUrl: 'https://api.test/tenant/v1', key: 'never-forward-this-key' }] };
  f.node('rmSourceAPI').value = 'a';
  f.node('rmHub').value = 'https://old.test/custom/v1';
  f.node('rmSourceAPI').classList.contains = () => false;
  f.emit('change', f.node('rmSourceAPI'));
  assert.equal(f.node('rmStation').value, 'https://api.test/tenant/v1');
  assert.equal(f.node('rmHub').value, '');
  assert.equal(f.node('rmEnrollmentClaim').value, '');
  assert.equal(f.calls.length, 0);
});

test('explicit reenrollment rotates only remote binding without loading histories', async () => {
  const f = connectionFixture();
  Object.assign(f.context.S.state.config, { loggedIn: true, effectiveRemoteMode: 'hub' });
  f.node('rmRecoveryClaim').value = 'c'.repeat(64);
  const recovering = f.context.ACTIONS.rmReenroll({ isConnected: true });
  await new Promise(setImmediate);
  assert.equal(f.calls.length, 1);
  assert.equal(f.calls[0].path, '/api/remote/discover');
  assert.deepEqual(JSON.parse(JSON.stringify(f.calls[0].body)), { url: 'https://old.test/salcara-hub', hubUrl: 'https://old.test/salcara-hub', enrollmentClaim: 'c'.repeat(64), connect: true, reenroll: true });
  f.pending[0].resolve({ ok: true });
  await recovering;
  assert.equal(f.node('rmRecoveryClaim').value, '');
  assert.equal(f.calls.some(call => /session|history/.test(call.path)), false);
});
const tick = () => new Promise(resolve => setImmediate(resolve));

test('remote API picker explains separate API and reverse proxy restrictions', async () => {
  const f = connectionFixture();
  f.context.AGENT_INFO = { codex: { name: 'Codex', badge: 'C' }, claude: { name: 'Claude', badge: 'A' } };
  const loading = f.context.rmLoadAgents();
  f.pending[0].resolve({
    settingsScope: 'scope-original',
    agents: [{ id: 'codex', available: true, api: { source: 'phone', name: 'Proxy', accountId: 'proxy' } }],
    apis: [
      { id: 'external', name: 'External', remoteEligibility: { supported: false, reason: 'external_api_not_allowed' } },
      { id: 'proxy', name: 'Proxy', remoteEligibility: { supported: false, reason: 'reverse_proxy_not_allowed' } },
      { id: 'allowed', name: 'Allowed', remoteEligibility: { supported: true, reason: 'supported' } },
    ],
  });
  await loading;
  const markup = f.node('rmAgents').innerHTML;
  assert.match(markup, /value="external"[^>]*disabled[^>]*>External · 本站未授权此 API 地址/);
  assert.match(markup, /value="proxy"[^>]*selected[^>]*disabled[^>]*>Proxy · 本站未授权此逆向代理地址/);
  assert.match(markup, /本站未授权此逆向代理地址；请选择已授权的来源/);
  assert.match(markup, /value="allowed"\s*>Allowed/);
});

test('recovery status distinguishes queued messages, storage errors and unknown task outcome', () => {
  const f = fixture();
  const normal = f.context.rmRecoveryMarkup({ queued: 3, recovery: { state: 'ready' } });
  assert.match(normal, /3 条进度消息/);
  assert.match(normal, /待同步到中转站/);
  assert.match(normal, /中转站 \/ Bot 通道/);
  assert.match(normal, /消息同步状态不代表任务是否完成/);
  assert.match(normal, /查看电脑对话/);
  const broken = f.context.rmRecoveryMarkup({ recovery: { state: 'blocked' } });
  assert.match(broken, /恢复不可用/);
  assert.match(broken, /不要删除恢复目录/);
  assert.doesNotMatch(broken, /已就绪/);
  assert.match(f.context.rmRecoveryMarkup({ recovery: { state: 'degraded' } }), /保持电脑端运行/);
  assert.match(f.context.rmRecoveryMarkup({ recovery: { state: 'ready', historyRequired: true } }), /原会话补齐/);
  assert.match(f.context.rmRecoveryMarkup({}), /待检查/);
});

test('live connection and recovery updates preserve drafts and restore QR after reconnect', () => {
  const f = connectionFixture();
  f.context.S.state.config.loggedIn = true;
  f.context.S.pairQR = 'data:image/png;base64,fixture';
  f.context.S.pairExpires = Date.now() + 60000;
  const draft = f.node('rmEnrollmentClaim').value;
  f.context.rmUpdateRemoteStatus({ deviceId: 'test-device', state: 'connecting', error: 'offline', queued: 4, recovery: { state: 'ready' } });
  assert.match(f.node('rmHubConnectionMeta').textContent, /连接中断，自动重连中/);
  assert.match(f.node('pairQR').innerHTML, /电脑连接成功后可扫码/);
  assert.match(f.node('pairQR').innerHTML, /data-act="rmShowQR" disabled/);
  assert.doesNotMatch(f.node('pairQR').innerHTML, /<img/);
  f.context.rmUpdateRemoteStatus({ deviceId: 'test-device', state: 'connected', queued: 0, recovery: { state: 'ready' } });
  assert.match(f.node('rmHubConnectionMeta').textContent, /已连接/);
  assert.match(f.node('pairQR').innerHTML, /data:image\/png/);
  assert.equal(f.node('rmEnrollmentClaim').value, draft);
  assert.equal(f.routed.length, 0, 'a status update must not rebuild editable connection forms');
  const previous = f.node('rmRecovery').innerHTML;
  f.context.rmUpdateRemoteStatus({ deviceId: 'different-device', state: 'connected', queued: 999 });
  assert.equal(f.node('rmRecovery').innerHTML, previous, 'an old device must not repaint the current one');
});

test('remote errors distinguish enrollment, phone pairing, source policy and availability', () => {
  const f = connectionFixture();
  for (const [status, label] of [
    [{ state: 'device_auth_required', errorCode: 'enrollment_claim_invalid' }, '需要新的设备登记码'],
    [{ state: 'device_auth_required', errorCode: 'device_enrollment_required' }, '设备尚未登记'],
    [{ state: 'device_auth_required', errorCode: 'device_credentials_invalid' }, '设备登记凭据失效'],
    [{ state: 'source_unauthorized' }, '本站未授权此 API 来源远程使用'],
    [{ state: 'pair_required' }, '手机需重新绑定'],
    [{ state: 'remote_blocked', errorCode: 'user_disabled' }, '远程用户已停用'],
    [{ state: 'connecting', errorCode: 'remote_unavailable' }, '远程服务暂不可用，自动重连中'],
    [{ state: 'connecting', errorCode: 'clock_skew' }, '时间不一致，请先校时'],
  ]) assert.equal(f.context.rmConnectionLabel(status), label);
});

test('clock skew connection rejection preserves drafts and offers only read-only refresh', async () => {
  const f = connectionFixture();
  const connecting = f.context.ACTIONS.rmConnect({});
  f.pending[0].reject(Object.assign(new Error('untrusted display text'), { status: 409, code: 'clock_skew' }));
  await connecting;
  const markup = f.node('rmConnectError').innerHTML;
  assert.match(markup, /电脑与中转站时间不一致/);
  assert.match(markup, /立即同步/);
  assert.match(markup, /data-act="rmRecoveryRefresh"/);
  assert.match(markup, /再次点击原连接按钮.*新连接不会自动重试/);
  assert.doesNotMatch(markup, /untrusted display text|data-act="rmRetryEnrollment"|data-act="rmReenroll"|href="#accounts"|操作结果暂未确认/);
  assert.equal(f.node('rmStation').value, 'https://one.test');
  assert.equal(f.node('rmEnrollmentClaim').value, 'a'.repeat(64));
  assert.equal(f.calls.length, 1);
  let reads = 0;
  f.context.loadState = async () => { reads++; return f.context.S.state; };
  await f.context.ACTIONS.rmRecoveryRefresh({});
  assert.equal(reads, 1);
  assert.equal(f.calls.length, 1, 'clock repair refresh must not resend enrollment, mutate APIs or scan sessions');
});

test('local API retains only bounded clock diagnostics without changing the delivery code', async () => {
  const f = fixture();
  vm.runInContext(apiSource, f.context);
  const ahead = { ...clockDiagnostic, source: 'fresh_proof', direction: 'local_ahead', offsetMs: -60000, signedTime: clockDiagnostic.localTime - 60000 };
  for (const diagnostic of [clockDiagnostic, ahead]) {
    f.context.fetch = async () => ({ ok: false, status: 409, json: async () => ({ code: 'enrollment_not_sent', clockSkew: { ...diagnostic, key: 'never-copy', html: '<script>bad</script>' } }) });
    for (const endpoint of ['/api/remote/discover', '/api/remote/ingress']) {
      await assert.rejects(f.context.api(endpoint, {}), error => {
        assert.equal(error.code, 'enrollment_not_sent');
        assert.equal(error.status, 409);
        assert.deepEqual(JSON.parse(JSON.stringify(error.clockSkew)), diagnostic);
        return true;
      });
    }
  }
  for (const clockSkew of [null, {}, { ...clockDiagnostic, source: 'http_date' }, { ...clockDiagnostic, offsetMs: '60000' }, { ...clockDiagnostic, direction: 'local_ahead' }, { ...clockDiagnostic, signedTime: 0 }, { ...clockDiagnostic, offsetMs: 1 }, { ...clockDiagnostic, localTime: 253402300800000 }, { ...ahead, source: 'signed_manifest' }]) {
    f.context.fetch = async () => ({ ok: false, status: 409, json: async () => ({ code: 'enrollment_not_sent', clockSkew }) });
    await assert.rejects(f.context.api('/api/remote/discover', {}), error => error.code === 'enrollment_not_sent' && error.clockSkew === undefined);
  }
  for (const [endpoint, code] of [['https://external.test/api/remote/discover', 'clock_skew'], ['/api/session/control', 'clock_skew'], ['/api/remote/discover', 'enrollment_result_unknown'], ['/api/remote/discover', 'hub_identity_changed']]) {
    f.context.fetch = async () => ({ ok: false, status: 409, json: async () => ({ code, clockSkew: clockDiagnostic }) });
    await assert.rejects(f.context.api(endpoint, {}), error => error.code === code && error.clockSkew === undefined);
  }
});

test('real API submit errors preserve not-sent and unknown semantics alongside clock guidance', async () => {
  for (const row of [
    { code: 'enrollment_not_sent', status: 409, clock: true, clear: true },
    { code: 'clock_skew', status: 409, clock: true },
    { code: 'clock_skew', status: 503, clock: true, unknown: true },
    { code: 'enrollment_result_unknown', status: 409, unknown: true },
  ]) {
    const f = apiConnectionFixture();
    vm.runInContext(apiSource, f.context);
    vm.runInContext('RM.pendingEnrollment = { key: rmCurrentEnrollmentTargetKey() }', f.context);
    const originalPending = vm.runInContext('RM.pendingEnrollment', f.context);
    const draft = f.node('rmStation').value;
    f.context.fetch = async (endpoint, options) => {
      const body = JSON.parse(options.body);
      f.calls.push({ path: endpoint, body });
      return body.connect ? { ok: false, status: row.status, json: async () => ({ code: row.code, error: 'server display text', clockSkew: clockDiagnostic }) }
        : { ok: true, status: 200, json: async () => ({ ok: true, stage: 'discovered', eligibility: { supported: true, method: 'api' } }) };
    };
    await f.context.ACTIONS.rmConnect({});
    const markup = f.node('rmConnectError').innerHTML;
    assert.equal(f.calls.length, 2, row.code);
    assert.equal(f.calls[1].body.connect, true);
    assert.equal(f.calls.some(call => call.body.retryEnrollment), false);
    assert.equal(f.node('rmStation').value, draft);
    assert.equal(f.notices.length, 0);
    assert.equal(/操作结果暂未确认/.test(markup), !!row.unknown);
    assert.equal(/立即同步/.test(markup), !!row.clock);
    if (row.clear) {
      assert.match(markup, /登记尚未发送/);
      assert.equal(vm.runInContext('RM.pendingEnrollment', f.context), null);
    } else assert.equal(vm.runInContext('RM.pendingEnrollment.key', f.context), originalPending.key);
    if (row.clock) {
      assert.doesNotMatch(markup, /server display text|data-act="rmRetryEnrollment"|data-act="rmReenroll"|href="#accounts"/);
      assert.match(markup, row.unknown ? /先刷新状态核对原操作，不要重复提交/ : /再次点击原连接按钮/);
    } else assert.match(markup, /data-act="rmRetryEnrollment"/);
    let reads = 0;
    f.context.loadState = async () => { reads++; return f.context.S.state; };
    await f.context.ACTIONS.rmRecoveryRefresh({});
    assert.equal(reads, 1);
    assert.equal(f.calls.length, 2, 'refresh never submits again or scans sessions');
  }
});

test('a delayed status from another Hub cannot repaint the current remote page', () => {
  const f = connectionFixture();
  f.context.rmUpdateRemoteStatus({ deviceId: 'test-device', hubUrl: 'https://other.test/salcara-hub', state: 'device_auth_required', error: 'old error' });
  assert.equal(f.node('rmHubConnectionError').innerHTML, '');
});

test('read-only recovery refresh cannot repaint or notify after leaving the page', async () => {
  const f = connectionFixture();
  const state = deferred();
  f.context.loadState = () => state.promise;
  const refreshing = f.context.ACTIONS.rmRecoveryRefresh({});
  f.context.S.route = 'sessions';
  state.resolve({});
  await refreshing;
  assert.equal(f.node('rmRecovery').innerHTML, '');
  assert.equal(f.notices.length, 0);
  assert.equal(f.calls.length, 0, 'refresh must not submit or replay a remote command');
});

test('direct recovery never presents retained Hub counters as direct delivery state', () => {
  const f = connectionFixture('direct');
  f.context.S.state.config.effectiveRemoteMode = 'direct';
  const hub = { queued: 99, lastEventSent: 1700000000000, recovery: { state: 'degraded', historyRequired: true } };
  const direct = f.context.rmRecoveryMarkup(hub);
  assert.match(direct, /直连消息与恢复/);
  assert.match(direct, /电脑与手机之间直接传输/);
  assert.match(direct, /原会话核对任务进度与结果/);
  assert.doesNotMatch(direct, /99|待同步|送达|保存失败|Hub/);
  f.context.S.state.config.effectiveRemoteMode = 'bot';
  const relay = f.context.rmRecoveryMarkup(hub);
  assert.match(relay, /中转站 \/ Bot 通道/);
  assert.match(relay, /99 条进度消息/);
  assert.match(relay, /保存失败/);
});

function pendingFor(f, pathName, bodyType) {
  const item = f.pending.find(x => !x.used && x.path === pathName && (!bodyType || x.body?.type === bodyType));
  assert.ok(item, `expected a pending request for ${pathName}${bodyType ? ` (${bodyType})` : ''}`);
  item.used = true;
  return item;
}

function actionKeysInObject(source, openBrace) {
  const keys = [];
  let depth = 1, paren = 0, bracket = 0, expectKey = true;
  for (let i = openBrace + 1; i < source.length;) {
    const c = source[i];
    if (c === '/' && source[i + 1] === '/') {
      i += 2; while (i < source.length && source[i] !== '\n') i++;
      continue;
    }
    if (c === '/' && source[i + 1] === '*') {
      i += 2; while (i < source.length && !(source[i] === '*' && source[i + 1] === '/')) i++;
      i = Math.min(source.length, i + 2); continue;
    }
    if (c === '"' || c === "'" || c === '`') {
      const quote = c; i++;
      while (i < source.length) {
        if (source[i] === '\\') { i += 2; continue; }
        if (source[i++] === quote) break;
      }
      continue;
    }
    if (depth === 1 && paren === 0 && bracket === 0) {
      if (/\s/.test(c)) { i++; continue; }
      if (expectKey) {
        const key = source.slice(i).match(/^([A-Za-z_$][\w$]*)/);
        if (key) {
          let end = i + key[0].length;
          while (/\s/.test(source[end] || '')) end++;
          if (source[end] === ':' || source[end] === '(' || source[end] === ',' || source[end] === '}') {
            keys.push(key[1]); i += key[0].length; expectKey = false; continue;
          }
        }
        if (c === '}') { depth--; if (!depth) break; }
        else if (c === '{') depth++;
        i++;
        continue;
      }
      if (c === ',') { expectKey = true; i++; continue; }
    }
    if (depth === 1) {
      if (c === '(') paren++;
      else if (c === ')') paren = Math.max(0, paren - 1);
      else if (c === '[') bracket++;
      else if (c === ']') bracket = Math.max(0, bracket - 1);
    }
    if (c === '{') depth++;
    else if (c === '}') { depth--; if (depth === 0) break; }
    i++;
  }
  return keys;
}

test('a stale session list cannot repaint or replace a newer page', async () => {
  const f = fixture();
  const view = { isConnected: true, innerHTML: '' };
  const first = f.context.RENDER_sessions(view);
  f.context.S.route = 'overview';
  pendingFor(f, '/api/sessions').resolve({ sessions: [{ sessionKey: 'stale', tool: 'codex', updatedAt: 1 }] });
  await first;
  assert.deepEqual(f.context.S.sessions, [], 'stale response must not update shared session state');
  assert.equal(f.context.S.openKey, null);
});

test('a stale session detail cannot overwrite a newly selected conversation', async () => {
  const f = fixture();
  f.context.S.route = 'sessions';
  const first = f.context.openSession('old', false);
  f.context.S.openKey = 'new';
  pendingFor(f, '/api/session?key=old').resolve({ session: { sessionKey: 'old', title: 'old' }, events: [] });
  await first;
  assert.equal(f.context.S.openInfo, null, 'late detail must not replace the selected conversation');
});

test('the conversations page renders before capability discovery finishes', async () => {
  const f = fixture();
  const view = { isConnected: true, innerHTML: '' };
  const loading = f.context.RENDER_sessions(view);
  assert.match(view.innerHTML, /id="cxSessionNav"/, 'navigation shell should be visible immediately');
  assert.match(view.innerHTML, /id="sessList"/, 'session list should be visible immediately');
  pendingFor(f, '/api/sessions').resolve({ sessions: [] });
  await loading;
  const capabilities = pendingFor(f, '/api/remote/agents');
  assert.ok(f.node('cxSessionNav').innerHTML.length >= 0);
  capabilities.resolve({ agents: [{ id: 'codex', capabilities: { archivedSessions: true } }] });
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(vm.runInContext('RM.agents.agents[0].capabilities.archivedSessions', f.context), true);
  assert.match(f.node('cxSessionNav').innerHTML, /cxArchived/, `capability-gated navigation should appear when capabilities arrive; nav=${f.node('cxSessionNav').innerHTML}`);
});

test('opening a conversation does not wait for optional controls to load', async () => {
  const f = fixture();
  f.context.S.route = 'sessions';
  f.context.S.openKey = 'codex:thread';
  const opening = f.context.openSession('codex:thread', false);
  pendingFor(f, '/api/session?key=codex%3Athread').resolve({ session: { sessionKey: 'codex:thread', tool: 'codex', title: 'thread' }, events: [] });
  await opening;
  assert.equal(f.context.S.openInfo.title, 'thread', 'core conversation should be usable before optional capability discovery');
  assert.doesNotMatch(f.node('sessDetail').innerHTML, /class="rm-error"/);
  assert.ok(f.pending.some(item => item.path === '/api/remote/agents'), 'optional capability discovery remains in the background');
});

test('a late capability response cannot turn a history failure into an empty list', async () => {
  const f = fixture();
  const read = f.context.RENDER_sessions({ isConnected: true, innerHTML: '' });
  pendingFor(f, '/api/sessions').reject(new Error('history denied'));
  await read;
  pendingFor(f, '/api/remote/agents').resolve({ agents: [] }); await tick();
  assert.match(f.node('sessList').innerHTML, /history denied/);
  assert.match(f.node('sessList').innerHTML, /data-act="reloadSessions"/);
  assert.doesNotMatch(f.node('sessList').innerHTML, /还没有 .* 对话/);
});

test('same-page refreshes preserve rows, cancel older reads, and merge newer events', async () => {
  const f = fixture();
  const initial = f.context.loadSessions();
  pendingFor(f, '/api/sessions').resolve({ sessions: [{ sessionKey: 'codex:one', tool: 'codex', title: 'Initial row', updatedAt: 1 }] }); await initial;
  const old = f.context.loadSessions(), oldResponse = pendingFor(f, '/api/sessions');
  assert.match(f.node('sessList').innerHTML, /Initial row/);
  const latest = f.context.loadSessions(), newResponse = pendingFor(f, '/api/sessions');
  f.context.sessionReadUpdated({ sessionKey: 'codex:one', tool: 'codex', title: 'Latest event', updatedAt: 3 });
  newResponse.resolve({ sessions: [{ sessionKey: 'codex:one', tool: 'codex', title: 'Older snapshot', updatedAt: 2 }] }); await latest;
  oldResponse.resolve({ sessions: [] }); await old;
  assert.match(f.node('sessList').innerHTML, /Latest event/);
  const failed = f.context.loadSessions(); pendingFor(f, '/api/sessions').reject(new Error('refresh unavailable')); await failed;
  assert.match(f.node('sessList').innerHTML, /Latest event/);
  assert.match(f.node('sessList').innerHTML, /refresh unavailable/);
});

test('reopening the same key cannot accept an older detail response and read cancellation aborts transport', async () => {
  const f = fixture(), signals = [], original = f.context.api;
  f.context.api = (path, body, method, options) => { if (options?.signal) signals.push(options.signal); return original(path, body); };
  const first = f.context.openSession('codex:one'), old = pendingFor(f, '/api/session?key=codex%3Aone');
  const second = f.context.openSession('codex:one'), fresh = pendingFor(f, '/api/session?key=codex%3Aone');
  assert.equal(signals[0].aborted, true);
  fresh.resolve({ session: { sessionKey: 'codex:one', tool: 'codex', title: 'Fresh detail' }, events: [] }); await second;
  old.resolve({ session: { sessionKey: 'codex:one', tool: 'codex', title: 'Stale detail' }, events: [] }); await first;
  assert.equal(f.context.S.openInfo.title, 'Fresh detail');
  f.context.cancelSessionReads(); assert.equal(signals.at(-1).aborted, true);
});

test('partial list failures preserve only failed sources and mark their rows stale', async () => {
  const f = fixture();
  const initial = f.context.loadSessions(); pendingFor(f, '/api/sessions').resolve({ sessions: [
    { sessionKey: 'codex:old', tool: 'codex' }, { sessionKey: 'claude:kept', tool: 'claude' },
  ] }); await initial;
  const partial = f.context.loadSessions(); pendingFor(f, '/api/sessions').resolve({ sessions: [{ sessionKey: 'codex:new', tool: 'codex' }], errors: ['Claude failed'], failedTools: ['claude'] }); await partial;
  assert.deepEqual(Array.from(f.context.S.sessions, s => s.sessionKey).sort(), ['claude:kept', 'codex:new']);
  assert.equal(f.context.S.sessions.find(s => s.tool === 'claude').historyStale, true);
});

test('opening snapshot merge keeps a final message newer than the buffered stream', async () => {
  const f = fixture();
  const app = fs.readFileSync(path.resolve(__dirname, '../../internal/console/web/app.js'), 'utf8');
  vm.runInContext(app.slice(app.indexOf('function tlKey('), app.indexOf('const KIND_ICON =')), f.context);
  const read = f.context.openSession('codex:one');
  vm.runInContext("SESSION_READS.detailChanges.events.push({type:'message',id:'same',text:'Hel',final:false,ts:10})", f.context);
  pendingFor(f, '/api/session?key=codex%3Aone').resolve({ session: { sessionKey: 'codex:one', tool: 'codex' }, events: [{ type: 'message', id: 'same', text: 'Hello', final: true, ts: 20 }] });
  await read;
  assert.equal(f.context.S.tl[0].text, 'Hello'); assert.equal(f.context.S.tl[0].final, true);
});

test('new-thread feature controls appear when the background catalog arrives', async () => {
  const f = fixture();
  vm.runInContext("RM.newThread = true; S.route = 'sessions'", f.context);
  f.node('cxFeatureMount');
  const loading = f.context.loadFeaturesForCwd('C:/fixture', 'codex');
  pendingFor(f, '/api/remote/agents').resolve({ agents: [{ id: 'codex', capabilities: { features: true } }] });
  await new Promise(resolve => setImmediate(resolve));
  const request = f.pending.find(item => !item.used && item.path === '/api/session/control' && item.body?.type === 'features.list');
  assert.ok(request, 'feature catalog should load after capability discovery');
  request.used = true;
  request.resolve({ collaborationModes: ['plan'], skillsSupported: true, skills: [{ name: 'review', displayName: 'Review', path: 'C:/fixture/.agents/skills/review' }] });
  await loading;
  assert.match(f.node('cxFeatureMount').innerHTML, /review/);
  assert.match(f.node('cxFeatureMount').innerHTML, /cxCollaborationMode/);
});

test('standalone Hub enrollment code is sent only on connect and retained after an asynchronous save', async () => {
  const f = fixture();
  f.context.S.route = 'login';
  f.context.S.state = { config: { loggedIn: false, relayRoot: '', hubUrl: '', remoteConnections: [] } };
  const view = { isConnected: true, innerHTML: '' };
  f.context.RENDER_login(view);
  assert.match(view.innerHTML, /id="rmEnrollmentClaim"/);
  assert.match(view.innerHTML, /id="rmEnrollmentFallback" hidden/);
  assert.doesNotMatch(view.innerHTML, /首次登记必填/);

  f.node('rmStation').value = 'https://hub.example.com/salcara-hub';
  f.node('rmHub').value = '';
  const claim = 'd'.repeat(64);
  f.node('rmEnrollmentClaim').value = claim;
  const connecting = f.context.ACTIONS.rmConnect({});
  await new Promise(resolve => setImmediate(resolve));
  const probe = pendingFor(f, '/api/remote/discover');
  assert.equal(probe.body.enrollmentClaim, undefined, 'read-only discovery must not persist or transmit the claim');
  probe.resolve({ ok: true });
  await new Promise(resolve => setImmediate(resolve));
  const connect = f.pending.find(item => !item.used && item.path === '/api/remote/discover' && item.body?.connect === true);
  assert.ok(connect, 'explicit connection should submit after discovery');
  assert.equal(connect.body.enrollmentClaim, claim);
  connect.used = true;
  connect.resolve({ ok: true, registered: false, stage: 'saved', eligibility: { supported: true, method: 'manual' } });
  await connecting;
  assert.equal(f.node('rmEnrollmentClaim').value, claim);
  assert.match(f.notices.at(-1).message, /设置已保存.*等待电脑登记和上线确认/);
  assert.equal(f.routed.length, 0, 'retain the current form while manual registration is not confirmed');
});

test('a standalone discovery requirement exposes its code field before any mutation', async () => {
  const f = connectionFixture();
  f.node('rmEnrollmentClaim').value = '';
  f.node('rmEnrollmentFallback').hidden = true;
  const connecting = f.context.ACTIONS.rmConnect({});
  f.pending[0].resolve({ ok: true, stage: 'discovered', errorCode: 'device_credentials_required', deviceError: '需要管理员提供登记码' });
  await connecting;
  assert.equal(f.calls.length, 1);
  assert.equal(f.node('rmEnrollmentFallback').hidden, false);
  assert.match(f.node('rmConnectError').innerHTML, /登记码/);
});

test('every rendered console data-act has a registered action', () => {
  const f = fixture();
  const webRoot = path.resolve(__dirname, '../../internal/console/web');
  const sources = fs.readdirSync(webRoot).filter(name => name.endsWith('.js')).map(name => fs.readFileSync(path.join(webRoot, name), 'utf8'));
  const allSource = sources.join('\n');
  const names = new Set([...allSource.matchAll(/data-act=["']([A-Za-z0-9_-]+)["']/g)].map(match => match[1]));
  const actions = new Set([...allSource.matchAll(/ACTIONS\.([A-Za-z][A-Za-z0-9_]*)\s*=/g)].map(match => match[1]));
  for (const match of allSource.matchAll(/(?:const ACTIONS\s*=\s*|Object\.assign\(ACTIONS,\s*)\{/g)) {
    for (const key of actionKeysInObject(allSource, match.index + match[0].length - 1)) actions.add(key);
  }
  const missing = [...names].filter(name => !actions.has(name));
  assert.deepEqual(missing, [], `unhandled data-act values: ${missing.join(', ')}`);
});

test('connection discovery freezes the entire draft and cancelling or editing prevents commit', async () => {
  for (const change of ['url', 'claim', 'mode', 'page', 'return', 'edit-back']) {
    const f = connectionFixture();
    const connecting = f.context.ACTIONS.rmConnect({});
    const probe = pendingFor(f, '/api/remote/discover');
    if (change === 'url') f.node('rmStation').value = 'https://two.test';
    if (change === 'claim') f.node('rmEnrollmentClaim').value = 'b'.repeat(64);
    if (change === 'mode') await f.context.ACTIONS.rmMode({ dataset: { mode: 'direct' } });
    if (change === 'page') f.context.S.route = 'accounts';
    if (change === 'return') f.context.S.routeRevision++;
    if (change === 'edit-back') { f.node('rmStation').value = 'https://two.test'; f.emit('input', f.node('rmStation')); f.node('rmStation').value = 'https://one.test'; f.emit('input', f.node('rmStation')); }
    probe.resolve({ ok: true });
    await connecting;
    assert.equal(f.calls.filter(call => call.body?.connect === true).length, 0, change);
    assert.equal(vm.runInContext('RM.connectionAction', f.context), null, change);
  }
});

test('double connect is serialized and sends the frozen enrollment only once', async () => {
  const f = connectionFixture();
  const connecting = f.context.ACTIONS.rmConnect({});
  await f.context.ACTIONS.rmConnect({});
  assert.equal(f.calls.length, 1);
  pendingFor(f, '/api/remote/discover').resolve({ ok: true });
  await tick();
  const commit = pendingFor(f, '/api/remote/discover');
  assert.equal(commit.body.enrollmentClaim, 'a'.repeat(64));
  assert.equal(commit.body.connect, true);
  await f.context.ACTIONS.rmConnect({});
  assert.equal(f.calls.length, 2);
  commit.resolve(registeredConnection(f));
  await connecting;
  assert.equal(f.node('rmEnrollmentClaim').value, '');
  assert.match(f.notices.at(-1).message, /登记已确认.*等待连接上线/);
  assert.doesNotMatch(f.notices.at(-1).message, /^已连接/);
});

test('missing or mismatched registration receipts cannot clear the current claim or QR', async () => {
  for (const mismatch of ['missing', 'device', 'hub']) {
    const f = connectionFixture();
    f.context.S.pairQR = 'data:previous-qr';
    f.context.S.pairExpires = Date.now() + 60000;
    const connecting = f.context.ACTIONS.rmConnect({});
    f.pending[0].resolve({ ok: true });
    await tick();
    const result = registeredConnection(f);
    if (mismatch === 'missing') delete result.registration;
    else result.registration = { ...result.registration, [mismatch === 'device' ? 'deviceId' : 'hubUrl']: mismatch === 'device' ? 'other-device' : 'https://other.test/hub' };
    f.pending[1].resolve(result);
    await connecting;
    assert.equal(f.node('rmEnrollmentClaim').value, 'a'.repeat(64));
    assert.equal(f.context.S.pairQR, 'data:previous-qr');
    assert.equal(vm.runInContext('RM.confirmedRegistration', f.context), undefined);
    assert.match(f.notices.at(-1).message, /刷新状态核对当前连接/);
    assert.equal(f.routed.length, 0);
  }
});

test('accepted connection never clears newer input or navigates a replacement page', async () => {
  const f = connectionFixture();
  let reads = 0;
  f.context.loadState = async () => { reads++; return { config: { loggedIn: true }, hub: { state: 'connecting' } }; };
  const connecting = f.context.ACTIONS.rmConnect({});
  pendingFor(f, '/api/remote/discover').resolve({ ok: true });
  await tick();
  const commit = pendingFor(f, '/api/remote/discover');
  f.node('rmEnrollmentClaim').value = 'b'.repeat(64);
  f.emit('input', f.node('rmEnrollmentClaim'));
  f.context.S.route = 'accounts';
  commit.resolve({ ok: true, registered: true, stage: 'waiting_online' });
  await connecting;
  assert.equal(reads, 1, 'accepted changes still reconcile current state');
  assert.equal(f.node('rmEnrollmentClaim').value, 'b'.repeat(64));
  assert.equal(f.routed.length, 0);
  assert.equal(f.notices.length, 0, 'a replaced page must not receive the previous connection success');
});

test('accepted connection followed by refresh failure remains saved and never retries', async () => {
  const f = connectionFixture();
  f.context.loadState = async () => { throw new Error('display refresh failed'); };
  const connecting = f.context.ACTIONS.rmConnect({});
  pendingFor(f, '/api/remote/discover').resolve({ ok: true, stage: 'discovered', eligibility: { supported: true, method: 'manual' } });
  await tick();
  pendingFor(f, '/api/remote/discover').resolve({ ok: true, registered: true, stage: 'waiting_online' });
  await connecting;
  assert.equal(f.calls.length, 2);
  assert.equal(f.notices.at(-1).kind, 'ok');
  assert.equal(f.node('rmConnectError').innerHTML, '');
});

test('lost commit response keeps enrollment and distinguishes uncertainty from explicit rejection', async () => {
  for (const status of [undefined, 403, 409, 503]) {
    const f = connectionFixture();
    const connecting = f.context.ACTIONS.rmConnect({});
    pendingFor(f, '/api/remote/discover').resolve({ ok: true });
    await tick();
    const error = new Error('fixture rejection'); if (status) error.status = status;
    pendingFor(f, '/api/remote/discover').reject(error);
    await connecting;
    assert.equal(f.node('rmEnrollmentClaim').value, 'a'.repeat(64));
    assert.equal(f.calls.length, 2, 'no automatic mutation retry');
    assert.equal(f.node('rmConnectError').innerHTML.includes('暂未确认'), !status || status >= 500, String(status));
  }
});

test('connection drafts survive mode changes and initial state refresh cannot erase typed input', async () => {
  const f = connectionFixture();
  const state = deferred(); f.context.loadState = () => state.promise;
  const view = f.node('view');
  f.context.RENDER_login(view);
  f.node('rmStation').value = 'https://draft.test'; f.emit('input', f.node('rmStation'));
  const before = view.innerHTML;
  state.resolve({ config: { loggedIn: false, relayRoot: 'https://previous.test' } });
  await tick();
  assert.equal(view.innerHTML, before);
  await f.context.ACTIONS.rmMode({ dataset: { mode: 'bot' } });
  await f.context.ACTIONS.rmMode({ dataset: { mode: 'station' } });
  assert.equal(vm.runInContext('RM.stationDraft.url', f.context), 'https://draft.test');
  assert.equal(vm.runInContext('RM.stationDraft.claim', f.context), '', 'a claim for an old address is cleared');
});

test('remote mode controls stay in the connection section while a first state request is pending', async () => {
  const f = connectionFixture();
  f.context.S.state = null;
  const first = deferred(), current = deferred();
  let reads = 0;
  f.context.loadState = () => ++reads === 1 ? first.promise : current.promise;
  const view = f.node('view');
  f.context.route = () => { f.context.S.routeRevision++; f.context.RENDER_login(view); };
  f.context.RENDER_login(view);
  assert.equal(f.node('headActions').innerHTML, '');
  assert.match(view.innerHTML, /id="rmSwitchMode"/);
  assert.match(view.innerHTML, /value="hub" selected/);
  await f.context.ACTIONS.rmMode({ dataset: { mode: 'bot' } });
  assert.match(view.innerHTML, /value="bot" selected/);
  current.resolve({ config: { loggedIn: false, remoteConnections: [] } });
  await tick();
  assert.match(view.innerHTML, /id="rmBotPanel"/);
  assert.doesNotMatch(view.innerHTML, /id="rmModeTabs"|class="rm-tabs/);
  const rendered = view.innerHTML;
  first.reject(new Error('obsolete load failure'));
  await tick();
  assert.equal(view.innerHTML, rendered, 'an older load must not replace the newly selected mode');
});

test('remote state responses cannot overwrite a later visit to the same shared view', async () => {
  for (const fail of [false, true]) {
    const f = connectionFixture();
    f.context.S.state = null;
    const first = deferred(), current = deferred();
    let reads = 0;
    f.context.loadState = () => ++reads === 1 ? first.promise : current.promise;
    const view = f.node('view');
    f.context.RENDER_login(view);
    f.context.S.route = 'sessions';
    f.context.S.routeRevision++;
    f.node('headActions').innerHTML = 'sessions toolbar';
    f.context.S.route = 'login';
    f.context.S.routeRevision++;
    f.context.RENDER_login(view);
    current.resolve({ config: { loggedIn: false, relayRoot: 'https://new.test', remoteConnections: [] } });
    await tick();
    const rendered = view.innerHTML, toolbar = f.node('headActions').innerHTML;
    if (fail) first.reject(new Error('obsolete load failure'));
    else first.resolve({ config: { loggedIn: false, relayRoot: 'https://old.test', remoteConnections: [] } });
    await tick();
    assert.equal(view.innerHTML, rendered, `outdated ${fail ? 'failure' : 'success'} must be ignored`);
    assert.equal(f.node('headActions').innerHTML, toolbar);
  }
});

test('initial status refresh cannot erase a device name or project path draft', async () => {
  for (const id of ['devInput', 'projPath']) {
    const f = connectionFixture();
    f.context.S.state.config.loggedIn = true;
    const state = deferred(); f.context.loadState = () => state.promise;
    const view = f.node('view');
    f.context.RENDER_login(view);
    f.node(id).value = 'unsaved value'; f.emit('input', f.node(id));
    const before = view.innerHTML;
    state.resolve({ config: { loggedIn: true, deviceName: 'old value' }, hub: { state: 'connected' } });
    await tick();
    assert.equal(view.innerHTML, before, id);
  }
});

test('shared direct status request refreshes the newest page and keeps a live QR until paired', async () => {
  const f = connectionFixture('direct');
  vm.runInContext('RM.loginEpoch = 1; RM.direct = { qrDataURL: "data:fixture", expiresAt: Date.now() + 60000 }', f.context);
  const first = f.context.rmLoadDirect(1);
  vm.runInContext('RM.loginEpoch = 2', f.context);
  const second = f.context.rmLoadDirect(2);
  assert.equal(f.calls.length, 1);
  pendingFor(f, '/api/local/direct/status').resolve({ enabled: true, running: true, paired: false });
  await Promise.all([first, second]);
  assert.match(f.node('rmPhoneBinding').innerHTML, /data:fixture/);
  const paired = f.context.rmLoadDirect(2);
  pendingFor(f, '/api/local/direct/status').resolve({ enabled: true, running: true, paired: true });
  await paired;
  assert.doesNotMatch(f.node('rmPhoneBinding').innerHTML, /data:fixture/);
  assert.match(f.node('rmPhoneBinding').innerHTML, /已配对/);
});

test('direct revoke invalidates old reads and cannot run beside QR generation', async () => {
  const f = connectionFixture('direct');
  const oldRead = f.context.rmLoadDirect();
  const revoking = f.context.ACTIONS.rmDirectRevoke({});
  await tick();
  await f.context.ACTIONS.rmDirectQR({});
  assert.equal(f.calls.some(call => call.path === '/api/local/direct/qr'), false);
  pendingFor(f, '/api/local/direct/revoke').resolve({ ok: true });
  await tick();
  const reads = f.pending.filter(item => item.path === '/api/local/direct/status');
  assert.equal(reads.length, 2);
  reads[1].resolve({ enabled: false, running: false, paired: false });
  await revoking;
  reads[0].resolve({ enabled: true, running: true, paired: true });
  await oldRead;
  assert.equal(vm.runInContext('RM.direct.enabled', f.context), false);
  assert.equal(vm.runInContext('RM.direct.paired', f.context), false);
});

test('leaving a confirmation cancels direct revoke before dispatch', async () => {
  const f = connectionFixture('direct'), answer = deferred();
  f.context.confirm = () => answer.promise;
  const revoke = f.context.ACTIONS.rmDirectRevoke({});
  f.context.S.route = 'accounts'; answer.resolve(true);
  await revoke;
  assert.equal(f.calls.length, 0);
});

test('Bot registration status belongs to the active station and waits for its connection', () => {
  const f = connectionFixture('bot');
  const config = { loggedIn: true, effectiveHubUrl: 'https://current.test/salcara-hub', remoteModes: { bot: { groupId: 'current' } }, remoteConnections: [{ hubUrl: 'https://saved.test/salcara-hub', botGroupId: 'old' }] };
  assert.match(f.context.rmSavedBotsMarkup(config), /已保存，未启用/);
  config.remoteConnections.push({ hubUrl: config.effectiveHubUrl, botGroupId: 'current' });
  assert.match(f.context.rmSavedBotsMarkup(config), /正在连接/);
  f.context.S.state.hub.state = 'connected';
  assert.match(f.context.rmSavedBotsMarkup(config), /已连接/);
});

test('saved station and typed connection use the same mutation lease', async () => {
  const f = connectionFixture();
  f.context.S.state.config.remoteConnections = [{ hubUrl: 'https://saved.test/salcara-hub', name: 'saved' }];
  const remoteSupport = fs.readFileSync(path.resolve(__dirname, '../../internal/console/web/remote-support.js'), 'utf8');
  vm.runInContext(remoteSupport, f.context, { filename: 'remote-support.js' });
  const button = { dataset: { hub: 'https://saved.test/salcara-hub' }, closest: () => f.node('savedRemoteStations'), isConnected: true };
  const connecting = f.context.ACTIONS.rmConnect({});
  await f.context.ACTIONS.remoteStationSelect(button);
  assert.equal(f.calls.length, 1);
  pendingFor(f, '/api/remote/discover').resolve({ ok: true }); await tick();
  pendingFor(f, '/api/remote/discover').resolve({ ok: true, registered: true, stage: 'waiting_online' }); await connecting;
  const saved = f.context.ACTIONS.remoteStationSelect(button);
  const request = pendingFor(f, '/api/remote/mode');
  assert.equal(request.body.hubUrl, button.dataset.hub);
  assert.equal(request.body.mode, 'hub');
  await f.context.ACTIONS.rmConnect({});
  assert.equal(f.calls.filter(call => call.path === '/api/remote/discover').length, 2);
  assert.equal(f.calls.filter(call => call.path === '/api/remote/mode').length, 1);
  request.resolve({ ok: true, activeMode: 'hub' }); await saved;
  assert.match(f.notices.at(-1).message, /设置已保存/);
});

test('an abandoned preflight cannot block or clear a newer connection lease', async () => {
  const f = connectionFixture();
  const old = f.context.ACTIONS.rmConnect({});
  const probe = pendingFor(f, '/api/remote/discover');
  f.context.S.routeRevision++;
  f.node('rmStation').value = 'https://new.test';
  const current = f.context.ACTIONS.rmConnect({});
  const newerProbe = pendingFor(f, '/api/remote/discover');
  probe.resolve({ ok: true }); await old;
  assert.notEqual(vm.runInContext('RM.connectionAction', f.context), null);
  newerProbe.resolve({ ok: true }); await tick();
  const commit = pendingFor(f, '/api/remote/discover');
  assert.equal(commit.body.url, 'https://new.test');
  commit.resolve({ ok: true, registered: true, stage: 'waiting_online' }); await current;
  assert.equal(f.calls.filter(call => call.body?.connect).length, 1);
});

test('direct refresh only replaces its own panel and cannot duplicate the self-host controls', async () => {
  const f = connectionFixture('direct');
  f.context.renderSelfhostHubPanel = () => '<section id="selfhost-fixture">saved hub draft</section>';
  assert.match(f.context.renderRemoteDirectPanel(), /selfhost-fixture/);
  const reading = f.context.rmLoadDirect();
  pendingFor(f, '/api/local/direct/status').resolve({ enabled: false, paired: false });
  await reading;
  assert.doesNotMatch(f.node('rmDirectPanel').outerHTML, /selfhost-fixture/);
});

test('an old official station picker read cannot replace a newer applied binding', async () => {
  const f = connectionFixture();
  f.context.S.route = 'accounts';
  const account = { id: 'official-fixture', sourceRevision: 'src-v1-' + 'a'.repeat(64), officialAccount: { mode: 'oauth' } };
  f.context.S.local = { accounts: [account], bindings: { codex: { accountId: 'old' } } };
  f.context.OFFICIAL_PANEL = { epoch: 1 };
  f.context.officialPanel = () => { f.context.OFFICIAL_PANEL.epoch++; };
  vm.runInContext(fs.readFileSync(path.resolve(__dirname, '../../internal/console/web/remote-support.js'), 'utf8'), f.context);
  const opening = f.context.ACTIONS.officialRemoteStations({ dataset: { id: account.id } });
  const read = pendingFor(f, '/api/local/accounts');
  const applied = { accounts: [account], bindings: { codex: { accountId: 'new', appliedId: 'new' } } };
  f.context.S.localAccountsRevision++;
  f.context.S.local = applied;
  read.resolve({ accounts: [account], bindings: { codex: { accountId: 'old' } } });
  await opening;
  assert.equal(f.context.S.local, applied);
  assert.match(f.node('officialError').textContent, /状态已更新/);
});

test('a delayed saved-station refresh cannot overwrite newer account bindings or station metadata', async () => {
  const f = connectionFixture();
  const station = { hubUrl: 'https://saved.test/salcara-hub', name: 'saved' };
  f.context.S.state.config.remoteConnections = [station];
  vm.runInContext(fs.readFileSync(path.resolve(__dirname, '../../internal/console/web/remote-support.js'), 'utf8'), f.context);
  const button = { dataset: { hub: station.hubUrl }, closest: () => f.node('savedRemoteStations'), isConnected: true };
  const probe = f.context.ACTIONS.remoteStationProbe(button);
  pendingFor(f, '/api/remote/discover').resolve({ ok: true }); await tick();
  const read = pendingFor(f, '/api/local/accounts');
  const applied = { accounts: [], bindings: { codex: { accountId: 'new', appliedId: 'new' } }, remoteStations: [{ ...station, name: 'fresh' }] };
  f.context.S.localAccountsRevision++;
  f.context.S.local = applied;
  f.context.S.state.config.remoteConnections = applied.remoteStations;
  read.resolve({ accounts: [], bindings: { codex: { accountId: 'old' } }, remoteStations: [{ ...station, name: 'stale' }] });
  await probe;
  assert.equal(f.context.S.local, applied);
  assert.equal(f.context.S.state.config.remoteConnections[0].name, 'fresh');
});

test('restoring the active mode chooses its tab once and later status refreshes preserve browsing', async () => {
  for (const [active, tab] of [['direct', 'direct'], ['bot', 'bot'], ['hub', 'station'], ['none', 'station']]) {
    const f = connectionFixture();
    f.context.S.state.config.effectiveRemoteMode = active;
    f.context.loadState = async () => f.context.S.state;
    f.context.RENDER_login(f.node('view'));
    assert.equal(vm.runInContext('RM.remoteMode', f.context), tab);
    assert.match(f.node('view').innerHTML, new RegExp(`当前启用：${{ direct: '直连', bot: 'Bot', hub: '中转站', none: '未启用' }[active]}`));
    const browsing = tab === 'direct' ? 'bot' : 'direct';
    await f.context.ACTIONS.rmMode({ dataset: { mode: browsing } });
    f.context.rmUpdateRemoteStatus();
    f.context.RENDER_login(f.node('view'));
    assert.equal(vm.runInContext('RM.remoteMode', f.context), browsing);
    assert.equal(f.calls.some(call => call.body !== undefined), false, 'browsing must not mutate the active connection');
  }
});

test('effective mode overrides saved credentials and stale connected status in every panel', () => {
  const f = connectionFixture('direct');
  const config = f.context.S.state.config;
  Object.assign(config, { effectiveRemoteMode: 'direct', loggedIn: true, effectiveHubUrl: 'https://saved.test/salcara-hub', remoteConnections: [{ mode: 'bot', hubUrl: 'https://saved.test/salcara-hub', name: 'saved-bot' }] });
  f.context.S.state.hub.state = 'connected';
  assert.match(f.context.rmSavedBotsMarkup(config), /已保存，未启用/);
  assert.doesNotMatch(f.context.rmSavedBotsMarkup(config), /已连接<\/span>/);
  vm.runInContext('RM.direct = { paired: true, enabled: true, running: true, connected: true, qrDataURL: "data:stale", expiresAt: Date.now() + 60000 }', f.context);
  config.effectiveRemoteMode = 'hub';
  assert.match(f.context.renderRemoteDirectPanel(false), /已配对，未启用/);
  assert.match(f.context.renderRemoteDirectPanel(false), /data-act="rmActivateMode" data-mode="direct"/);
  assert.doesNotMatch(f.context.renderRemoteDirectPanel(false), /data:stale|手机已连接/);
  config.effectiveRemoteMode = 'none';
  assert.equal(f.context.rmEffectiveMode(config), 'none');
});

test('stored direct hosts without a phone do not offer paired recovery', () => {
  const f = connectionFixture('direct');
  f.context.S.state.config.effectiveRemoteMode = 'hub';
  f.context.S.state.config.remoteModes = { direct: { configured: true, paired: false } };
  assert.match(f.context.renderRemoteDirectPanel(false), /启用并配对/);
  assert.doesNotMatch(f.context.renderRemoteDirectPanel(false), /无需重新扫码/);
  f.context.S.state.config.remoteModes.direct.paired = true;
  assert.match(f.context.renderRemoteDirectPanel(false), /无需重新扫码/);
});

test('saved mode activation is explicit, serialized and resumes without registration or QR', async () => {
  for (const mode of ['direct', 'bot', 'none']) {
    const f = connectionFixture(mode === 'none' ? 'station' : mode);
    f.context.S.state.config.effectiveRemoteMode = 'hub';
    f.context.loadState = async () => f.context.S.state;
    const button = { dataset: { mode, ...(mode === 'bot' ? { hub: 'https://saved-bot.test/salcara-hub' } : {}) } };
    const action = f.context.ACTIONS.rmActivateMode(button);
    const request = pendingFor(f, '/api/remote/mode');
    assert.equal(request.body.mode, mode);
    assert.equal(request.body.hubUrl, button.dataset.hub);
    await f.context.ACTIONS.rmActivateMode(button);
    assert.equal(f.calls.length, 1);
    assert.equal(f.context.S.state.config.effectiveRemoteMode, 'hub', 'do not report success before server acknowledgement');
    f.context.S.state.config.effectiveRemoteMode = mode;
    request.resolve({ ok: true, activeMode: mode });
    await action;
    assert.equal(f.calls.length, 1);
    assert.equal(f.notices.at(-1).kind, 'ok');
    assert.equal(f.routed.length, 1);
    assert.equal(vm.runInContext('RM.connectionAction', f.context), null);
  }
});

test('mode activation rejects unconfirmed or failed writes without replacing the prior mode', async () => {
  for (const result of ['reject', 'unconfirmed']) {
    const f = connectionFixture('direct');
    f.context.S.state.config.effectiveRemoteMode = 'hub';
    f.context.loadState = async () => f.context.S.state;
    const action = f.context.ACTIONS.rmActivateMode({ dataset: { mode: 'direct' } });
    const request = pendingFor(f, '/api/remote/mode');
    if (result === 'reject') { const error = new Error('port unavailable'); error.status = 409; request.reject(error); }
    else request.resolve({ ok: true, activeMode: 'hub' });
    await action;
    assert.equal(f.context.S.state.config.effectiveRemoteMode, 'hub');
    assert.equal(f.routed.length, 0);
    assert.equal(f.calls.length, 1);
    assert.match(f.node('rmModeError').innerHTML, result === 'reject' ? /port unavailable/ : /暂未确认/);
  }
});

test('a submitted mode change reconciles after tab navigation without navigating back', async () => {
  const f = connectionFixture('direct');
  let reads = 0;
  f.context.loadState = async () => { reads++; return f.context.S.state; };
  const action = f.context.ACTIONS.rmActivateMode({ dataset: { mode: 'direct' } });
  const request = pendingFor(f, '/api/remote/mode');
  await f.context.ACTIONS.rmMode({ dataset: { mode: 'bot' } });
  request.resolve({ ok: true, activeMode: 'direct' });
  await action;
  assert.equal(reads, 1);
  assert.equal(vm.runInContext('RM.remoteMode', f.context), 'bot');
  assert.equal(f.routed.length, 1, 'only explicit tab navigation should route');
});

test('saved Hub list excludes Bots and marks all inactive while direct mode is enabled', () => {
  const f = connectionFixture();
  Object.assign(f.context.S.state.config, { effectiveRemoteMode: 'direct', loggedIn: true, effectiveHubUrl: 'https://hub.test/salcara-hub', remoteConnections: [{ hubUrl: 'https://hub.test/salcara-hub', name: 'saved-hub' }, { mode: 'bot', hubUrl: 'https://bot.test/salcara-hub', name: 'saved-bot' }] });
  vm.runInContext(fs.readFileSync(path.resolve(__dirname, '../../internal/console/web/remote-support.js'), 'utf8'), f.context);
  const html = f.context.remoteSavedStationsHTML(f.context.S.state.config);
  assert.match(html, /saved-hub/);
  assert.match(html, /已保存，未启用/);
  assert.match(html, /启用中转站/);
  assert.doesNotMatch(html, /saved-bot|当前启用/);
});

test('Hub and Bot at the same URL remain separate across initial render, refresh and activation', async () => {
  for (const botFirst of [true, false]) {
    const f = connectionFixture();
    const hubUrl = 'https://shared.test/salcara-hub';
    const hub = { hubUrl, name: 'saved-hub', support: { checkedAt: 1 } };
    const bot = { hubUrl, name: 'saved-bot', mode: 'bot', groupId: 'group-a', support: { checkedAt: 3 } };
    const config = f.context.S.state.config;
    Object.assign(config, { loggedIn: true, effectiveRemoteMode: 'bot', effectiveHubUrl: hubUrl, remoteModes: { bot: { groupId: 'group-a' } }, remoteConnections: botFirst ? [bot, hub, { ...hub }] : [hub, { ...hub }, bot] });
    f.context.S.local = { accounts: [], remoteStations: [bot] };
    f.context.S.state.hub.state = 'connected';
    f.context.loadState = async () => f.context.S.state;
    vm.runInContext(fs.readFileSync(path.resolve(__dirname, '../../internal/console/web/remote-support.js'), 'utf8'), f.context);

    assert.deepEqual(Array.from(f.context.rmSavedConnections(config, 'hub'), row => row.name), ['saved-hub']);
    assert.deepEqual(Array.from(f.context.rmSavedConnections(config, 'bot'), row => row.name), ['saved-bot']);
    const initialHub = f.context.remoteSavedStationsHTML(config);
    assert.match(initialHub, /saved-hub/);
    assert.match(initialHub, /已保存，未启用/);
    assert.doesNotMatch(initialHub, /saved-bot|当前启用/);
    assert.equal((initialHub.match(/class="remote-station-row"/g) || []).length, 1);
    await tick();
    pendingFor(f, '/api/local/accounts').resolve({ accounts: [], remoteStations: [bot, hub] });
    await tick();
    const refreshedHub = f.node('savedRemoteStations').innerHTML;
    assert.match(refreshedHub, /saved-hub/);
    assert.doesNotMatch(refreshedHub, /saved-bot|当前启用/);
    assert.match(f.context.rmSavedBotsMarkup(config), /saved-bot/);
    assert.match(f.context.rmSavedBotsMarkup(config), />已连接</);
    assert.doesNotMatch(f.context.rmSavedBotsMarkup(config), /saved-hub|已保存，未启用/);

    const hubButton = { dataset: { hub: hubUrl }, closest: () => f.node('savedRemoteStations'), isConnected: true };
    const enableHub = f.context.ACTIONS.remoteStationSelect(hubButton);
    const hubRequest = pendingFor(f, '/api/remote/mode');
    assert.deepEqual({ ...hubRequest.body }, { mode: 'hub', hubUrl });
    config.effectiveRemoteMode = 'hub';
    hubRequest.resolve({ ok: true, activeMode: 'hub' });
    await enableHub;
    assert.match(f.node('savedRemoteStations').innerHTML, /当前启用/);
    assert.match(f.context.rmSavedBotsMarkup(config), /已保存，未启用/);
    assert.doesNotMatch(f.context.rmSavedBotsMarkup(config), />已连接</);

    await f.context.ACTIONS.rmMode({ dataset: { mode: 'bot' } });
    const enableBot = f.context.ACTIONS.rmActivateMode({ dataset: { mode: 'bot', hub: hubUrl, group: bot.groupId } });
    const botRequest = pendingFor(f, '/api/remote/mode');
    assert.deepEqual({ ...botRequest.body }, { mode: 'bot', hubUrl, groupId: bot.groupId });
    config.effectiveRemoteMode = 'bot';
    botRequest.resolve({ ok: true, activeMode: 'bot' });
    await enableBot;
    assert.match(f.node('savedRemoteStations').innerHTML, /已保存，未启用/);
    assert.doesNotMatch(f.node('savedRemoteStations').innerHTML, /当前启用/);
    assert.match(f.context.rmSavedBotsMarkup(config), />已连接</);
    assert.equal(f.calls.filter(call => call.path === '/api/remote/mode').length, 2);
    assert.equal(f.calls.some(call => call.path === '/api/remote/discover' || call.path === '/api/remote/bot-bind'), false);
  }
});

