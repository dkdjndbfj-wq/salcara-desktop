import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';

const web = new URL('../internal/console/web/', import.meta.url);
const remote = fs.readFileSync(new URL('remote.js', web), 'utf8');
const presentation = fs.readFileSync(new URL('remote-connect.js', web), 'utf8');
const gate = () => { let resolve; const promise = new Promise(r => { resolve = r; }); return { promise, resolve }; };
const esc = value => String(value ?? '').replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('"', '&quot;');
function fixture() {
  const nodes = new Map(), listeners = new Map(), calls = [];
  const node = (id, attrs = {}) => { const value = { innerHTML: '', textContent: '', value: '', isConnected: true }; Object.defineProperties(value, Object.getOwnPropertyDescriptors(attrs)); nodes.set(id, value); return value; };
  const c = vm.createContext({
    S: { route: 'login', routeRevision: 1, local: { accounts: [] }, state: { config: { effectiveRemoteMode: 'none', remoteConnections: [], remoteAgentSettingsScope: 'scope-fixture' }, hub: {} }, approvals: [], sessions: [] },
    PAGES: { login: {}, sessions: {}, projects: {} }, ICONS: {}, ACTIONS: {},
    RENDER_login: null, RENDER_projects: null, RENDER_sessions: null,
    document: { getElementById: id => nodes.get(id), addEventListener: (kind, fn) => { if (!listeners.has(kind)) listeners.set(kind, []); listeners.get(kind).push(fn); } },
    $: selector => nodes.get(selector.slice(1)), $$: () => [], URL, AbortController, Date, esc, icon: () => '',
    setTimeout() {}, clearTimeout() {}, setInterval() {}, clearInterval() {},
    busy: async (_button, fn) => fn(), toast() {}, route: async () => {}, confirm: async () => true,
    api: async (path, body) => { calls.push({ path, body }); return { agents: [], apis: [], settingsScope: 'scope-fixture' }; },
  });
  vm.runInContext(presentation, c);
  vm.runInContext(remote, c);
  const { RM } = vm.runInContext('({RM})', c);
  node('view');
  let modalHTML = ''; const modalIDs = new Set();
  node('modalRoot', { get innerHTML() { return modalHTML; }, set innerHTML(value) {
    for (const id of modalIDs) { nodes.get(id).isConnected = false; nodes.delete(id); } modalIDs.clear(); modalHTML = value;
    for (const match of value.matchAll(/<[a-z][^>]*\bid="([^"]+)"[^>]*>/g)) {
      const id = match[1]; node(id, { value: match[0].match(/\bvalue="([^"]*)"/)?.[1] || '', focus() {} }); modalIDs.add(id);
    }
  } });
  c.loadState = async () => c.S.state;
  return { c, RM, nodes, node, calls, listeners };
}

test('direct errors and repair links survive repeated status refresh, even with a stale Hub identity', () => {
  const f = fixture(); f.RM.remoteMode = 'direct';
  f.c.S.state.config = { effectiveRemoteMode: 'direct', deviceId: 'direct-device' };
  f.c.S.state.hub = { deviceId: 'old-hub-device', hubUrl: 'https://old.example.test', state: 'connected' };
  f.RM.direct = { enabled: true, running: false, error: 'fixture: address in use' };
  const error = f.node('rmHubConnectionError');
  f.node('rmConnectionSteps', { set innerHTML(value) { error.innerHTML = value.match(/<div id="rmHubConnectionError">([\s\S]*)<\/div>$/)?.[1] || ''; } });
  for (let i = 0; i < 3; i++) f.c.rmUpdateRemoteStatus();
  assert.match(error.innerHTML, /fixture: address in use/); assert.match(error.innerHTML, /rmDirectTutorial/);
});

test('first saved target fills an untouched empty choice but never replaces an explicit unavailable target', () => {
  const f = fixture(); f.RM.remoteMode = 'station';
  assert.equal(f.c.rmSwitchState().draft.key, '');
  const config = f.c.S.state.config = { effectiveRemoteMode: 'hub', effectiveHubUrl: 'https://one.example.test/hub', remoteConnections: [{ hubUrl: 'https://one.example.test/hub', name: '工作站' }] };
  const state = f.c.rmSwitchState();
  assert.equal(state.current, true); assert.match(state.draft.label, /工作站/);
  assert.doesNotMatch(f.c.rmConnectionSwitchMarkup(config), /请先在左侧添加连接/);
  f.RM.switchDraft = { mode: 'hub', key: 'removed-explicit-target', label: '旧目标', touched: true };
  assert.equal(f.c.rmSwitchState().target, undefined); assert.equal(f.c.rmSwitchState().draft.key, 'removed-explicit-target');
  assert.match(f.c.rmConnectionSwitchMarkup(config), /旧目标（已不可用）/);
});

test('Bot groups at one address keep distinct target identities and names', () => {
  const f = fixture(); f.RM.remoteMode = 'bot';
  f.c.S.state.config = { effectiveRemoteMode: 'bot', effectiveHubUrl: 'https://bot.example.test/hub', remoteModes: { bot: { groupId: 'b' } }, remoteConnections: [
    { mode: 'bot', hubUrl: 'https://bot.example.test/hub', groupId: 'a', name: '家里' },
    { mode: 'bot', hubUrl: 'https://bot.example.test/hub', groupId: 'b', name: '公司' },
  ] };
  const state = f.c.rmSwitchState(); assert.equal(state.targets.length, 2); assert.equal(state.target.groupId, 'b');
  assert.notEqual(state.targets[0].key, state.targets[1].key); assert.match(state.target.label, /公司/);
});

test('every mode exposes computer name and current API settings without switching configuration views', async () => {
  for (const mode of ['hub', 'bot', 'direct', 'none']) {
    const f = fixture(); f.RM.remoteMode = mode === 'hub' || mode === 'none' ? 'station' : mode;
    f.c.S.state.config = { effectiveRemoteMode: mode, deviceName: 'Fixture computer', remoteConnections: [], remoteAgentSettingsScope: 'scope-fixture' };
    const html = f.c.renderRemoteConnectionLayout({}, f.c.S.state);
    assert.equal((html.match(/id="devName"/g) || []).length, 1); assert.match(html, /id="rmAgentSettings"/);
    f.node('rmAgents'); await f.c.rmLoadAgents();
    assert.match(f.nodes.get('rmAgents').innerHTML, /rmSaveApi/); assert.equal(f.calls[0].path, '/api/remote/agents');
  }
});

test('late API catalog cannot populate a new connection, and a failed catalog has an explicit retry', async () => {
  const f = fixture(), pending = gate(); f.RM.remoteMode = 'bot';
  const box = f.node('rmAgents');
  f.c.api = () => pending.promise;
  const read = f.c.rmLoadAgents();
  f.c.S.state.config = { effectiveRemoteMode: 'direct', remoteConnections: [], remoteAgentSettingsScope: 'scope-fixture' };
  pending.resolve({ agents: [], apis: [], settingsScope: 'scope-fixture' }); await read;
  assert.doesNotMatch(box.innerHTML, /rmSaveApi/);
  f.c.api = async () => { throw Error('请先完成直连手机配对'); };
  await f.c.ACTIONS.rmReloadAgents(); assert.match(box.innerHTML, /请先完成直连手机配对/); assert.match(box.innerHTML, /rmReloadAgents/);
  f.c.api = async () => ({ agents: [], apis: [], settingsScope: 'scope-fixture' }); await f.c.ACTIONS.rmReloadAgents(); assert.match(box.innerHTML, /rmSaveApi/);
});

test('API forms require an observed scope and send that exact scope rather than a newer state value', async () => {
  const f = fixture(); f.node('rmAgents');
  f.c.api = async (path, body) => {
    f.calls.push({ path, body });
    return { settingsScope: 'scope-fixture', agents: [{ id: 'codex', api: { source: 'tool' } }], apis: [] };
  };
  await f.c.rmLoadAgents(); f.node('rmAgentApi-codex'); f.node('rmAgentModel-codex'); f.node('rmAgentResult-codex');
  await f.c.ACTIONS.rmSaveApi({ isConnected: true, dataset: { agent: 'codex' } });
  assert.equal(f.calls.find(call => call.body)?.body.expectedSettingsScope, 'scope-fixture');
  const writes = f.calls.filter(call => call.body).length;
  f.c.S.state.config.remoteAgentSettingsScope = 'replacement-pairing-scope';
  await f.c.ACTIONS.rmSaveApi({ isConnected: true, dataset: { agent: 'codex' } });
  assert.equal(f.calls.filter(call => call.body).length, writes); assert.match(f.nodes.get('rmAgents').innerHTML, /连接已改变/);
  for (const missing of ['state', 'catalog']) {
    const old = fixture(); old.node('rmAgents');
    if (missing === 'state') delete old.c.S.state.config.remoteAgentSettingsScope;
    else old.c.api = async () => ({ agents: [], apis: [] });
    await old.c.rmLoadAgents(); assert.notEqual(old.RM.agentForm.ready, true); assert.match(old.nodes.get('rmAgents').innerHTML, /设置范围未确认/);
  }
});

test('scope conflict when an old form reaches the server locks saves until a fresh read', async () => {
  const f = fixture(); f.node('rmAgents'); await f.c.rmLoadAgents();
  f.node('rmAgentApi-codex'); f.node('rmAgentModel-codex'); const output = f.node('rmAgentResult-codex');
  f.c.api = async (path, body) => { f.calls.push({ path, body }); throw Object.assign(Error('设置范围已变化'), { status: 409 }); };
  await f.c.ACTIONS.rmSaveApi({ isConnected: true, dataset: { agent: 'codex' } });
  assert.equal(f.calls.at(-1).body.expectedSettingsScope, 'scope-fixture'); assert.equal(f.RM.agentForm.ready, false); assert.match(output.innerHTML, /rmReloadAgents/);
  const count = f.calls.length; await f.c.ACTIONS.rmSaveApi({ isConnected: true, dataset: { agent: 'codex' } }); assert.equal(f.calls.length, count);
});

test('catalog and state scope mismatch triggers a bounded fresh read instead of presenting stale controls', async () => {
  const f = fixture(); const box = f.node('rmAgents'); let reads = 0;
  f.c.api = async () => { reads++; return { settingsScope: 'new-scope', agents: [], apis: [] }; };
  f.c.loadState = async () => { f.c.S.state.config.remoteAgentSettingsScope = 'new-scope'; return f.c.S.state; };
  await f.c.rmLoadAgents(); assert.equal(reads, 2); assert.equal(f.RM.agentForm.catalog.settingsScope, 'new-scope'); assert.match(box.innerHTML, /rmSaveApi/);
  const changing = fixture(); changing.node('rmAgents'); let repeated = 0;
  changing.c.api = async () => ({ settingsScope: 'changed-' + (++repeated), agents: [], apis: [] });
  await changing.c.rmLoadAgents(); assert.equal(repeated, 2); assert.match(changing.nodes.get('rmAgents').innerHTML, /仍在变化/);
});

test('repair shortcuts only reveal relevant controls and never issue a mutation', async () => {
  const f = fixture(); const advanced = f.node('rmAdvanced'); const enrollment = f.node('rmEnrollmentRecovery');
  let focused = false; f.node('rmRecoveryClaim', { focus() { focused = true; } });
  await f.c.ACTIONS.rmShowRecovery({ dataset: { section: 'enrollment' } });
  assert.equal(advanced.open, true); assert.equal(enrollment.open, true); assert.equal(focused, true); assert.equal(f.calls.length, 0);
});

test('structured enrollment failures expose recovery without error text and reject stale connection status', () => {
  const f = fixture(); f.RM.remoteMode = 'station';
  f.c.S.state.config = { effectiveRemoteMode: 'hub', loggedIn: true, deviceId: 'current-device', effectiveHubUrl: 'https://current.example.test', remoteAgentSettingsScope: 'scope-fixture' };
  f.RM.confirmedRegistration = { deviceId: 'current-device', hubUrl: 'https://current.example.test', settingsScope: 'scope-fixture', revision: f.RM.connectionRevision };
  assert.match(f.c.rmConnectionStepsMarkup(), /服务器已确认电脑登记/);
  for (const failure of [{ state: 'device_auth_required', errorCode: '' }, { state: 'connecting', errorCode: 'device_enrollment_required' }]) {
    f.c.S.state.hub = { ...failure, error: '', deviceId: 'current-device', hubUrl: 'https://current.example.test' };
    const html = f.c.rmConnectionStepsMarkup();
    assert.match(html, /data-act="rmShowRecovery" data-section="enrollment"/);
    assert.match(html, /class="issue"[^]*登记电脑/); assert.doesNotMatch(html, /服务器已确认电脑登记/);
  }
  for (const mismatch of [{ deviceId: 'old-device' }, { hubUrl: 'https://old.example.test' }]) {
    f.c.S.state.hub = { state: 'device_auth_required', errorCode: 'device_enrollment_required', ...mismatch };
    assert.doesNotMatch(f.c.rmConnectionStepsMarkup(), /data-act="rmShowRecovery"/);
  }
  f.c.S.state.hub = { state: 'connected' };
  assert.doesNotMatch(f.c.rmConnectionStepsMarkup(), /data-act="rmShowRecovery"/);
  assert.equal(f.calls.length, 0);
});

test('unpaired Hub status offers rescan after enrollment despite retained local pairing fields', () => {
  const f = fixture();
  f.c.S.state.config = { effectiveRemoteMode: 'hub', loggedIn: true, deviceId: 'current-device', effectiveHubUrl: 'https://current.example.test', phoneHash: 'synthetic-retained-pairing' };
  f.c.S.state.hub = { deviceId: 'current-device', hubUrl: 'https://current.example.test', state: 'device_auth_required', errorCode: 'device_enrollment_required', pairing: { deviceId: 'current-device', paired: false } };
  let html = f.c.rmPhoneBindingMarkup();
  assert.match(html, /先完成电脑登记并等待连接成功/); assert.match(html, /请重新扫码/);
  assert.match(html, /data-act="rmShowQR" disabled/); assert.doesNotMatch(html, /手机已绑定|data-act="pairRevoke"/);
  f.c.S.state.hub.state = 'connected'; f.c.S.state.hub.errorCode = '';
  html = f.c.rmPhoneBindingMarkup();
  assert.match(html, /data-act="rmShowQR"/); assert.doesNotMatch(html, /data-act="rmShowQR" disabled/);
  f.c.S.state.hub.pairing = { deviceId: 'old-device', paired: true };
  assert.doesNotMatch(f.c.rmPhoneBindingMarkup(), /手机已绑定|data-act="pairRevoke"/);
  f.c.S.state.hub.pairing = { deviceId: 'current-device', paired: true };
  html = f.c.rmPhoneBindingMarkup(); assert.match(html, /手机已绑定/); assert.doesNotMatch(html, /data-act="rmShowQR"/);
  assert.equal(f.calls.length, 0);
});

test('polling and consumed QR updates retain the same scoped rescan action', () => {
  const f = fixture(); f.RM.remoteMode = 'station';
  f.c.S.state.config = { effectiveRemoteMode: 'hub', loggedIn: true, deviceId: 'current-device', effectiveHubUrl: 'https://current.example.test' };
  f.c.S.state.hub = { deviceId: 'current-device', state: 'device_auth_required', errorCode: 'device_enrollment_required', pairing: { deviceId: 'current-device', paired: false } };
  const binding = f.node('rmPhoneBinding'), qr = f.node('pairQR', { classList: { toggle() {}, remove() {} } });
  for (let i = 0; i < 3; i++) f.c.rmUpdateRemoteStatus();
  assert.match(binding.innerHTML, /请重新扫码/); assert.match(qr.innerHTML, /data-act="rmShowQR" disabled/);
  f.c.S.state.hub.state = 'connected'; f.c.S.state.hub.errorCode = ''; f.c.S.state.hub.pairing.pendingExpiresAt = 0;
  f.c.S.pairQR = 'data:image/png;base64,synthetic'; f.c.S.pairExpires = Date.now() + 60000;
  f.c.rmPairStatus(f.c.S.state.hub);
  assert.equal(f.c.S.pairQR, ''); assert.match(qr.innerHTML, /扫码绑定手机/); assert.doesNotMatch(qr.innerHTML, /disabled|<img/);
  assert.equal(f.calls.length, 0);
});

function enrollmentFixture() {
  const f = fixture(), toasts = [];
  const hubUrl = 'https://fixture.example.test/hub';
  Object.assign(f.c.S.state.config, { effectiveRemoteMode: 'hub', effectiveHubUrl: hubUrl, deviceId: 'fixture-device', loggedIn: true });
  f.c.S.state.hub = { hubUrl, deviceId: 'fixture-device', state: 'connecting' };
  f.node('rmStation', { value: hubUrl }); f.node('rmHub', { value: hubUrl });
  f.node('rmEnrollmentClaim'); f.node('rmSourceAPI'); f.node('rmConnectError');
  f.c.toast = message => toasts.push(message);
  f.result = { registered: true, stage: 'waiting_online', plugin: { hubUrl }, registration: { hubUrl, deviceId: 'fixture-device' } };
  f.c.api = async (path, body) => { f.calls.push({ path, body }); return body?.connect ? f.result : { stage: 'discovered' }; };
  f.connect = () => f.c.ACTIONS.rmConnect({ isConnected: true });
  return Object.assign(f, { hubUrl, toasts });
}

test('manual save cannot claim connection from an older device or Hub snapshot', async () => {
  for (const mismatch of ['deviceId', 'hubUrl']) {
    const f = enrollmentFixture();
    f.result = { registered: false, stage: 'saved', eligibility: { method: 'manual' }, plugin: { hubUrl: f.hubUrl } };
    f.c.S.state.hub.state = 'connected'; f.c.S.state.hub[mismatch] = 'old-fixture';
    await f.connect();
    assert.equal(f.toasts.length, 1); assert.match(f.toasts[0], /连接设置已保存/); assert.doesNotMatch(f.toasts[0], /电脑已连接/);
    assert.equal(f.RM.confirmedRegistration, undefined);
  }
});

test('registration receipts require exact device and Hub before clearing draft or confirming registration', async () => {
  for (const mismatch of ['deviceId', 'hubUrl', 'missing']) {
    const f = enrollmentFixture();
    f.node('rmEnrollmentClaim', { value: 'a'.repeat(64) }); f.c.S.pairQR = 'synthetic-current-qr';
    if (mismatch === 'missing') delete f.result.registration;
    else f.result.registration[mismatch] = 'old-fixture';
    await f.connect();
    assert.equal(f.RM.confirmedRegistration, undefined); assert.equal(f.nodes.get('rmEnrollmentClaim').value, 'a'.repeat(64));
    assert.equal(f.c.S.pairQR, 'synthetic-current-qr'); assert.match(f.toasts[0], /刷新状态核对/);
    assert.doesNotMatch(f.c.rmConnectionStepsMarkup(), /服务器已确认电脑登记/);
  }
});

test('late receipts do not apply after connection action generation or page changes', async () => {
  for (const changed of ['revision', 'page']) {
    const f = enrollmentFixture(), pending = gate(), submitted = gate();
    f.c.api = async (_path, body) => { if (!body.connect) return { stage: 'discovered' }; submitted.resolve(); return pending.promise; };
    const work = f.connect(); await submitted.promise;
    if (changed === 'revision') f.RM.connectionRevision++;
    else f.RM.loginEpoch++;
    f.c.S.pairQR = 'synthetic-new-generation-qr'; f.c.S.pairExpires = 123456;
    pending.resolve(f.result); await work;
    assert.equal(f.RM.confirmedRegistration, undefined); assert.equal(f.toasts.length, 0);
    assert.equal(f.c.S.pairQR, 'synthetic-new-generation-qr'); assert.equal(f.c.S.pairExpires, 123456);
  }
});

test('refresh after an accepted old receipt cannot confirm or clear QR for a replacement device', async () => {
  const f = enrollmentFixture(), refreshed = gate(), refreshing = gate();
  f.c.loadState = async () => { refreshing.resolve(); await refreshed.promise; return f.c.S.state; };
  const work = f.connect(); await refreshing.promise;
  f.c.S.state.config.deviceId = 'replacement-device';
  f.c.S.state.config.remoteAgentSettingsScope = 'replacement-scope';
  f.c.S.state.hub = { hubUrl: f.hubUrl, deviceId: 'replacement-device', state: 'connecting', pairing: { deviceId: 'replacement-device', paired: false } };
  f.c.S.pairQR = 'synthetic-replacement-qr'; f.c.S.pairExpires = 654321;
  refreshed.resolve(); await work;
  assert.equal(f.RM.confirmedRegistration, undefined);
  assert.equal(f.c.S.pairQR, 'synthetic-replacement-qr'); assert.equal(f.c.S.pairExpires, 654321);
  assert.doesNotMatch(f.c.rmConnectionStepsMarkup(), /服务器已确认电脑登记/);
  assert.match(f.toasts[0], /刷新状态核对/);
});

test('matching registration confirms only its generation and distinguishes phone rescan from retained pairing', async () => {
  const f = enrollmentFixture(); await f.connect();
  assert.equal(f.RM.confirmedRegistration.deviceId, 'fixture-device');
  assert.match(f.c.rmConnectionStepsMarkup(), /服务器已确认电脑登记/);
  for (const changed of ['scope', 'revision']) {
    if (changed === 'scope') f.c.S.state.config.remoteAgentSettingsScope = 'replacement-generation';
    else { f.c.S.state.config.remoteAgentSettingsScope = 'scope-fixture'; f.RM.connectionRevision++; }
    assert.doesNotMatch(f.c.rmConnectionStepsMarkup(), /服务器已确认电脑登记/);
  }
  for (const paired of [false, true]) {
    const current = enrollmentFixture(); current.c.S.state.hub.state = 'connected'; current.c.S.state.hub.pairing = { paired };
    await current.connect(); assert.match(current.toasts[0], paired ? /已绑定手机/ : /扫码绑定手机/);
  }
});

test('definitively unsent enrollment is retryable while an unknown result stays protected', () => {
  const f = enrollmentFixture(), owner = f.c.rmBeginConnectionAction({ isConnected: true }, 'station');
  owner.submitted = true; owner.enrollmentKey = 'fixture-attempt';
  const output = f.nodes.get('rmConnectError');
  f.c.rmConnectionError(owner, output, { status: 503, code: 'enrollment_result_unknown' });
  assert.equal(f.RM.pendingEnrollment.key, owner.enrollmentKey); assert.match(output.innerHTML, /检查登记结果/);
  f.c.rmConnectionError(owner, output, { status: 503, code: 'enrollment_not_sent' });
  assert.equal(f.RM.pendingEnrollment, null); assert.match(output.innerHTML, /登记尚未发送，请检查连接后重试/);
  assert.doesNotMatch(output.innerHTML, /操作结果暂未确认|检查登记结果/);
  f.RM.pendingEnrollment = { key: 'another-attempt' };
  f.c.rmConnectionError(owner, output, { status: 409, code: 'enrollment_not_sent' });
  assert.equal(f.RM.pendingEnrollment.key, 'another-attempt');
  f.c.rmConnectionError(owner, output, new Error('response lost'));
  assert.match(output.innerHTML, /操作结果暂未确认/);
  assert.equal(f.RM.pendingEnrollment.key, 'another-attempt');
});

test('diagnostic summary excludes identities, addresses, paths, raw errors and credentials', () => {
  const f = fixture(); let copied = '';
  f.c.copyText = text => { copied = text; };
  f.c.S.state = { version: 'fixture-version', config: { effectiveRemoteMode: 'hub', deviceId: 'secret-device-id', effectiveHubUrl: 'https://private.example.test', remoteConnections: [] },
    hub: { state: 'connecting', errorCode: 'remote_unavailable', error: 'secret-key-and-user-path', queued: 3, recovery: { state: 'ready' } } };
  f.c.ACTIONS.rmCopyDiagnostics(); assert.match(copied, /remote_unavailable/); assert.match(copied, /待同步消息：3/);
  assert.doesNotMatch(copied, /secret-device-id|private\.example|secret-key-and-user-path/); assert.equal(f.calls.length, 0);
});

test('accepted connection refresh removes stale saved targets from the local account snapshot', async () => {
  const f = fixture(); f.c.S.local.remoteStations = [{ hubUrl: 'https://removed.example.test' }];
  f.c.S.state.config.remoteConnections = [{ hubUrl: 'https://kept.example.test' }];
  await f.c.rmRefreshAcceptedConnection();
  assert.equal(f.c.S.local.remoteStations.length, 1); assert.equal(f.c.S.local.remoteStations[0].hubUrl, 'https://kept.example.test');
});

test('history is an independent navigation item and session rows use native keyboard buttons', () => {
  const index = fs.readFileSync(new URL('index.html', web), 'utf8');
  assert.match(index, /href="#sessions" data-route="sessions"/);
  assert.match(remote, /<button type="button" class="cx-row/);
  assert.match(remote, /aria-pressed="\$\{s.sessionKey === S.openKey/);
});

function managedFixture() {
  const f = fixture(); f.RM.remoteMode = 'bot';
  f.c.S.state.config = { effectiveRemoteMode: 'none', remoteConnectionManagement: true, remoteConnections: [
    { mode: 'bot', hubUrl: 'https://bot.example.test/hub', groupId: 'a', name: 'A', revision: 'rev-a' },
    { mode: 'bot', hubUrl: 'https://bot.example.test/hub', groupId: 'b', name: 'B', revision: 'rev-b' },
  ] };
  f.edit = (operation = 'rename', group = 'a') => f.c.ACTIONS.rmConnectionEdit({ dataset: { mode: 'bot', hub: 'https://bot.example.test/hub', group, operation } });
  f.save = () => f.c.ACTIONS.rmConnectionManageSave({ isConnected: true });
  return f;
}

test('connection management is capability gated, exact-group scoped and revision checked', async () => {
  const f = managedFixture(), row = f.c.S.state.config.remoteConnections[0];
  assert.match(f.c.rmConnectionManageMarkup(row), /rmConnectionEdit/);
  f.c.S.state.config.remoteConnectionManagement = false; assert.equal(f.c.rmConnectionManageMarkup(row), '');
  f.edit(); assert.equal(f.nodes.get('rmConnectionEditTitle'), undefined);
  f.c.S.state.config.remoteConnectionManagement = true; f.edit(); f.nodes.get('rmConnectionName').value = 'Home';
  f.c.api = async (path, body) => { f.calls.push({ path, body }); return { ok: true, ...body }; };
  await f.save(); assert.equal(f.calls.length, 1); assert.equal(f.calls[0].path, '/api/remote/connections/manage');
  assert.equal(f.calls[0].body.groupId, 'a'); assert.equal(f.calls[0].body.expectedRevision, 'rev-a'); assert.equal(f.calls[0].body.name, 'Home');
  assert.equal(f.nodes.get('modalRoot').innerHTML, '');
});

test('changed or active connections cannot be removed from a stale manage panel', async () => {
  const f = managedFixture(); f.edit('remove');
  f.c.S.state.config.remoteConnections[0].revision = 'new-revision'; await f.save();
  assert.equal(f.calls.length, 0); assert.match(f.nodes.get('rmConnectionEditError').innerHTML, /连接已变化/);
  f.c.S.state.config.effectiveRemoteMode = 'bot'; f.c.S.state.config.effectiveHubUrl = 'https://bot.example.test/hub'; f.c.S.state.config.remoteModes = { bot: { groupId: 'a' } };
  f.nodes.get('modalRoot').innerHTML = ''; f.edit('remove'); assert.equal(f.nodes.get('modalRoot').innerHTML, '');
  assert.match(f.c.rmConnectionManageMarkup(f.c.S.state.config.remoteConnections[0]), /disabled title="请先停用/);
});

test('unknown connection mutation locks replay, and a late response never closes another panel', async () => {
  const f = managedFixture(); f.edit('remove');
  f.c.api = async (path, body) => { f.calls.push({ path, body }); throw Error('fixture lost response'); };
  await f.save(); assert.equal(f.RM.connectionEditor.uncertain, true); assert.match(f.nodes.get('rmConnectionEditError').innerHTML, /rmConnectionEditRefresh/);
  await f.save(); assert.equal(f.calls.length, 1);
  const next = managedFixture(), pending = gate(); next.edit();
  next.c.api = async (_path, body) => { await pending.promise; return { ok: true, ...body }; };
  const request = next.save(); await Promise.resolve(); next.nodes.get('modalRoot').innerHTML = '<h3 id="otherPanel">Other panel</h3>';
  pending.resolve(); await request; assert.match(next.nodes.get('modalRoot').innerHTML, /Other panel/);
});
