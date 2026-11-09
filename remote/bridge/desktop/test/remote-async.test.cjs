'use strict';

// UI-only race contracts. These fixtures never connect to a Hub or a local
// Bridge; they only hold synthetic responses until the page has been replaced.
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.resolve(__dirname, '../../internal/console/web/remote.js'), 'utf8');

function deferred() { let resolve, reject; const promise = new Promise((done, fail) => { resolve = done; reject = fail; }); return { promise, resolve, reject }; }
function fixture() {
  const calls = [], pending = [], notices = [], routed = [], listeners = new Map();
  const nodes = new Map();
  const node = id => { if (!nodes.has(id)) nodes.set(id, { id, innerHTML: '', textContent: '', isConnected: true, classList: { toggle() {}, remove() {} } }); return nodes.get(id); };
  const context = vm.createContext({
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
  vm.runInContext(source, context, { filename: 'remote.js' });
  return { context, calls, pending, node, notices, routed, emit(type, target) { for (const fn of listeners.get(type) || []) fn({ target }); } };
}

function connectionFixture(mode = 'station') {
  const f = fixture();
  f.context.S.route = 'login';
  f.context.S.routeRevision = 1;
  f.context.S.state = { config: { deviceId: 'test-device', loggedIn: false, effectiveHubUrl: 'https://old.test/salcara-hub', remoteConnections: [] }, hub: { state: 'not_logged_in' } };
  vm.runInContext(`RM.remoteMode = ${JSON.stringify(mode)}`, f.context);
  f.node('rmStation').value = 'https://one.test';
  f.node('rmHub').value = '';
  f.node('rmEnrollmentClaim').value = 'a'.repeat(64);
  return f;
}

test('saved API selection supplies only its full address and clears another station claim', () => {
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
  const f = fixture();
  f.context.S.route = 'login';
  f.context.AGENT_INFO = { codex: { name: 'Codex', badge: 'C' }, claude: { name: 'Claude', badge: 'A' } };
  const loading = f.context.rmLoadAgents();
  f.pending[0].resolve({
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
  assert.match(f.node('pairQR').innerHTML, /连接恢复后/);
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
  ]) assert.equal(f.context.rmConnectionLabel(status), label);
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
  assert.equal(f.context.S.openInfo, undefined, 'late detail must not replace the selected conversation');
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

test('standalone Hub enrollment code is sent only on connect and cleared after success', async () => {
  const f = fixture();
  f.context.S.route = 'login';
  f.context.S.state = { config: { loggedIn: false, relayRoot: '', hubUrl: '', remoteConnections: [] } };
  const view = { isConnected: true, innerHTML: '' };
  f.context.RENDER_login(view);
  assert.match(view.innerHTML, /id="rmEnrollmentClaim"/);
  assert.match(view.innerHTML, /首次登记必填，已登记电脑留空/);
  assert.match(view.innerHTML, /登记失败时保留以便重试/);
  assert.match(view.innerHTML, /Sub2API 账号或 API Key/);

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
  connect.resolve({ ok: true });
  await connecting;
  assert.equal(f.node('rmEnrollmentClaim').value, '');
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
  commit.resolve({ ok: true });
  await connecting;
  assert.equal(f.node('rmEnrollmentClaim').value, '');
  assert.match(f.notices.at(-1).message, /连接设置已保存/);
  assert.doesNotMatch(f.notices.at(-1).message, /^已连接/);
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
  commit.resolve({ ok: true });
  await connecting;
  assert.equal(reads, 1, 'accepted changes still reconcile current state');
  assert.equal(f.node('rmEnrollmentClaim').value, 'b'.repeat(64));
  assert.equal(f.routed.length, 0);
  assert.equal(f.notices.at(-1).kind, 'ok');
});

test('accepted connection followed by refresh failure remains saved and never retries', async () => {
  const f = connectionFixture();
  f.context.loadState = async () => { throw new Error('display refresh failed'); };
  const connecting = f.context.ACTIONS.rmConnect({});
  pendingFor(f, '/api/remote/discover').resolve({ ok: true });
  await tick();
  pendingFor(f, '/api/remote/discover').resolve({ ok: true });
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
  assert.equal(vm.runInContext('RM.stationDraft.claim', f.context), 'a'.repeat(64));
});

test('remote mode controls stay in the header while a first state request is pending', async () => {
  const f = connectionFixture();
  f.context.S.state = null;
  const first = deferred(), current = deferred();
  let reads = 0;
  f.context.loadState = () => ++reads === 1 ? first.promise : current.promise;
  const view = f.node('view');
  f.context.route = () => { f.context.S.routeRevision++; f.context.RENDER_login(view); };
  f.context.RENDER_login(view);
  assert.match(f.node('headActions').innerHTML, /class="rm-tabs rm-head-tabs"/);
  assert.match(f.node('headActions').innerHTML, /data-mode="station" aria-pressed="true"/);
  await f.context.ACTIONS.rmMode({ dataset: { mode: 'bot' } });
  assert.match(f.node('headActions').innerHTML, /data-mode="bot" aria-pressed="true"/);
  current.resolve({ config: { loggedIn: false, remoteConnections: [] } });
  await tick();
  assert.match(view.innerHTML, /id="rmBotPanel"/);
  assert.doesNotMatch(view.innerHTML, /class="rm-tabs/);
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
  assert.match(f.node('rmDirectPanel').outerHTML, /data:fixture/);
  const paired = f.context.rmLoadDirect(2);
  pendingFor(f, '/api/local/direct/status').resolve({ enabled: true, running: true, paired: true });
  await paired;
  assert.doesNotMatch(f.node('rmDirectPanel').outerHTML, /data:fixture/);
  assert.match(f.node('rmDirectPanel').outerHTML, /已配对/);
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
  const config = { loggedIn: true, effectiveHubUrl: 'https://current.test/salcara-hub', remoteConnections: [{ hubUrl: 'https://saved.test/salcara-hub', botGroupId: 'old' }] };
  assert.match(f.context.renderRemoteBotPanel(config), /已保存，未启用/);
  config.remoteConnections.push({ hubUrl: config.effectiveHubUrl, botGroupId: 'current' });
  assert.match(f.context.renderRemoteBotPanel(config), /正在连接/);
  f.context.S.state.hub.state = 'connected';
  assert.match(f.context.renderRemoteBotPanel(config), /已连接/);
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
  pendingFor(f, '/api/remote/discover').resolve({ ok: true }); await connecting;
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
  commit.resolve({ ok: true }); await current;
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
  assert.match(f.context.renderRemoteBotPanel(config), /已保存，未启用/);
  assert.doesNotMatch(f.context.renderRemoteBotPanel(config), />已连接</);
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
    const bot = { hubUrl, name: 'saved-bot', mode: 'bot', support: { checkedAt: 3 } };
    const config = f.context.S.state.config;
    Object.assign(config, { loggedIn: true, effectiveRemoteMode: 'bot', effectiveHubUrl: hubUrl, remoteConnections: botFirst ? [bot, hub, { ...hub }] : [hub, { ...hub }, bot] });
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
    const enableBot = f.context.ACTIONS.rmActivateMode({ dataset: { mode: 'bot', hub: hubUrl } });
    const botRequest = pendingFor(f, '/api/remote/mode');
    assert.deepEqual({ ...botRequest.body }, { mode: 'bot', hubUrl });
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

