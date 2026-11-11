'use strict';
// Synthetic presentation checks; these do not substitute for browser rendering.
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const web = path.resolve(__dirname, '../../internal/console/web');
function fixture(mode = 'station', config = {}, hub = {}) {
  const nodes = new Map();
  const context = vm.createContext({
    PAGES: { login: {}, sessions: {}, projects: {} }, ICONS: {}, ACTIONS: {},
    S: { route: 'login', state: { config, hub }, local: { accounts: [] } },
    document: { addEventListener() {}, getElementById: id => nodes.get(id) }, window: { addEventListener() {} },
    URL, setInterval() {}, clearInterval() {}, setTimeout() {}, clearTimeout() {},
    $: id => nodes.get(id.replace(/^#/, '')), icon: () => '', esc: value => String(value ?? '').replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('"', '&quot;'),
    api() { throw new Error('presentation must not make requests'); }, toast() {}, route() {},
  });
  for (const name of ['remote-connect.js', 'remote.js']) vm.runInContext(fs.readFileSync(path.join(web, name), 'utf8'), context, { filename: name });
  vm.runInContext(`RM.remoteMode = ${JSON.stringify(mode)}`, context);
  return { context, nodes, html: () => context.renderRemoteConnectionLayout({}, context.S.state) };
}

// Semantic visibility from hidden/closed-details markup only. No CSS or layout
// claims are made here; Work settings currently prohibit browser validation.
function visibleControls(html) {
  const stack = [], actions = [], images = [], voids = new Set(['input', 'img', 'br', 'hr', 'meta', 'link']);
  for (const match of html.matchAll(/<(\/?)([a-z][\w-]*)\b([^>]*)>/gi)) {
    const [, close, rawTag, attrs] = match, tag = rawTag.toLowerCase();
    if (close) { const index = stack.map(row => row.tag).lastIndexOf(tag); if (index >= 0) stack.length = index; continue; }
    const hidden = !!stack.at(-1)?.hidden || /\bhidden(?:\s|=|$)/.test(attrs);
    if (!hidden && tag === 'button' && !/\bdisabled(?:\s|=|$)/.test(attrs)) actions.push({ action: /data-act="([^"]*)"/.exec(attrs)?.[1], primary: /class="[^"]*\bprimary\b/.test(attrs) });
    if (!hidden && tag === 'img') images.push(/src="([^"]*)"/.exec(attrs)?.[1]);
    if (!voids.has(tag)) stack.push({ tag, hidden: hidden || tag === 'details' && !/\bopen(?:\s|=|$)/.test(attrs) });
  }
  return { actions, images };
}

test('all connection modes have a right-side switch, one form and one stop action without duplicate tabs', () => {
  for (const mode of ['station', 'bot', 'direct']) {
    const f = fixture(mode, { effectiveRemoteMode: 'hub', loggedIn: true }, { state: 'connected' });
    const html = f.html();
    for (const label of ['连接方式', '连接状态', '已绑定手机', '高级设置']) assert.match(html, new RegExp(`>${label}<`));
    assert.equal((html.match(/data-mode="none"/g) || []).length, 1);
    assert.equal(html.includes('id="rmStation"'), mode === 'station');
    assert.equal(html.includes('id="rmBotBinding"'), mode === 'bot');
    assert.equal(html.includes('id="rmDirectPanel"'), mode === 'direct');
    assert.doesNotMatch(html, /id="rmModeTabs"|id="rmConnectionTitle"|远程已内置/);
    assert.ok(html.indexOf('id="rmSwitchMode"') > html.indexOf('<aside'));
    assert.ok(html.indexOf('id="rmSwitchTarget"') > html.indexOf('<aside'));
    assert.match(html, /<details class="rm-sec rm-advanced" id="rmAdvanced">/);
    assert.doesNotMatch(html.slice(0, html.indexOf('id="rmAdvanced"')), /data-act="rmRecoverySessions"/);
    assert.doesNotMatch(html, /首次登记必填|安装远程模块|API Key 始终留在/);
  }
});

test('switch choices distinguish every saved Hub and Bot group while keeping effective identity separate', () => {
  const config = { effectiveRemoteMode: 'bot', effectiveHubUrl: 'https://shared.test/hub', remoteModes: { bot: { groupId: 'alpha' } }, remoteConnections: [
    { mode: 'hub', hubUrl: 'https://shared.test/hub' }, { mode: 'hub', hubUrl: 'https://second.test/hub' },
    { mode: 'bot', hubUrl: 'https://shared.test/hub', groupId: 'alpha' }, { mode: 'bot', hubUrl: 'https://shared.test/hub', groupId: 'beta' },
    { mode: 'bot', hubUrl: 'https://other.test/hub', groupId: 'alpha' },
  ] };
  for (const mode of ['station', 'bot', 'direct']) {
    const f = fixture(mode, config);
    const html = f.html(), select = /<select[^>]+id="rmSwitchTarget"[^>]*>([\s\S]*?)<\/select>/.exec(html)[1];
    assert.equal((select.match(/<option /g) || []).length, mode === 'station' ? 2 : mode === 'bot' ? 3 : 1);
    assert.match(html, /当前启用：Bot/);
    assert.match(f.context.rmModeStatusMarkup(config), /Bot · alpha — https:\/\/shared.test\/hub/);
    assert.match(html, /<option value="hub"/); assert.match(html, /<option value="bot"/); assert.match(html, /<option value="direct"/);
  }
  const f = fixture('bot', config), targets = f.context.rmSwitchTargets(config);
  assert.equal(new Set(targets.map(row => row.key)).size, 6);
});

test('phone route alone suppresses the shared visual title and retains its accessible heading', () => {
  const shell = fs.readFileSync(path.join(web, 'shell.js'), 'utf8'), css = fs.readFileSync(path.join(web, 'remote.css'), 'utf8');
  assert.match(shell, /\$\('#panel'\)\.classList\[r === 'login' \? 'add' : 'remove'\]\('rm-compact-route'\)/);
  assert.match(css, /\.panel\.rm-compact-route > \.page-head > \.page-title \{[^}]*clip-path: inset\(50%\)/);
  assert.doesNotMatch(css, /\.page-title \{[^}]*display: none/);
});

test('active identity text escapes untrusted saved Hub and Bot group values', () => {
  const payload = '\"><img src=x onerror=alert(1)>', config = { effectiveRemoteMode: 'bot', effectiveHubUrl: 'https://hub.test/' + payload, remoteModes: { bot: { groupId: payload } } };
  const f = fixture('bot', config), markup = f.context.rmModeStatusMarkup(config);
  assert.doesNotMatch(markup, /<img|<script/);
  assert.match(markup, /&lt;img/);
  config.effectiveRemoteMode = 'hub';
  assert.doesNotMatch(f.context.rmModeStatusMarkup(config), /<img/);
});

test('phone binding and status always describe effective connection while another mode is only browsed', () => {
  for (const browse of ['station', 'bot', 'direct']) {
    const f = fixture(browse, { effectiveRemoteMode: 'hub', loggedIn: true, effectiveHubUrl: 'https://active.test' }, { state: 'connected', pairing: { paired: true } });
    assert.match(f.context.rmPhoneBindingMarkup(), /手机已绑定/);
    assert.match(f.context.rmPhoneBindingMarkup(), /data-act="pairRevoke"/);
    assert.doesNotMatch(f.context.rmPhoneBindingMarkup(), /rmDirectRevoke|同一份 Bot/);
    assert.match(f.context.rmConnectionStepsMarkup(), /电脑与远程服务已连接/);
    f.context.S.state.config.effectiveRemoteMode = 'bot';
    assert.match(f.context.rmPhoneBindingMarkup(), /同一份 Bot/);
    assert.doesNotMatch(f.context.rmPhoneBindingMarkup(), /data-act="pairRevoke"|rmShowQR/);
    f.context.S.state.config.effectiveRemoteMode = 'direct';
    vm.runInContext('RM.direct = { enabled: true, paired: true, running: true, connected: true }', f.context);
    assert.match(f.context.rmPhoneBindingMarkup(), /手机已连接/);
    assert.match(f.context.rmPhoneBindingMarkup(), /rmDirectRevoke/);
    f.context.S.state.config.effectiveRemoteMode = 'none';
    assert.match(f.context.rmPhoneBindingMarkup(), /尚未启用远程/);
    assert.doesNotMatch(f.context.rmPhoneBindingMarkup(), /data-act=/);
  }
});

test('remote Agent API and permission navigation remain reachable without adding upfront session reads', () => {
  const html = fixture('station').html();
  assert.match(html, /<details[^>]+id="rmAgentSettings"\s*>/);
  assert.match(html, /id="rmAgents"/);
  assert.match(html, /应用于当前启用连接，不随待切换目标改变/);
  assert.match(html, /href="#settings"/); assert.match(html, /href="#accounts"/);
  assert.doesNotMatch(fixture('direct').html(), /id="rmAgents"/);
});

test('saved device configuration does not prove registration or allow a QR', () => {
  const f = fixture('station', { deviceId: 'd', effectiveRemoteMode: 'hub', loggedIn: true, effectiveHubUrl: 'https://hub.test' }, { state: 'connecting' });
  const html = f.html();
  assert.doesNotMatch(html, /服务器已确认电脑登记|data-act="rmShowQR"/);
  assert.match(html, /等待登记确认/);
  assert.match(html, /id="rmEnrollmentFallback" hidden/);
  f.context.S.state.hub.state = 'connected';
  assert.match(f.html(), /服务器已确认电脑登记/);
  assert.match(f.html(), /data-act="rmShowQR"/);
});

test('station without a saved API explains the actual next step without adding another action', () => {
  const f = fixture('station', { effectiveRemoteMode: 'none' });
  const empty = f.html();
  assert.match(empty, /先在「API 密钥」添加 API，或填写管理员提供的中转站地址/);
  assert.doesNotMatch(empty, /选择已保存的 API|id="rmSourceAPI"/);
  assert.deepEqual(visibleControls(empty).actions.filter(action => action.primary).map(action => action.action), ['rmConnect']);
  f.context.S.local.accounts = [{ id: 'api-test', name: '测试 API', sourceType: 'api_key', baseUrl: 'https://api.test/v1' }];
  const saved = f.html();
  assert.match(saved, /选择已保存的 API/);
  assert.match(saved, /id="rmSourceAPI"/);
  assert.doesNotMatch(saved, /先在「API 密钥」添加 API/);
  assert.deepEqual(visibleControls(saved).actions.filter(action => action.primary).map(action => action.action), ['rmConnect']);
});

test('an acknowledged registration is scoped to its exact computer and Hub', () => {
  const f = fixture('station', { deviceId: 'd', effectiveRemoteMode: 'hub', loggedIn: true, effectiveHubUrl: 'https://hub.test' }, { state: 'connecting' });
  vm.runInContext(`RM.confirmedRegistration = { deviceId: 'd', hubUrl: 'https://hub.test' }`, f.context);
  assert.match(f.html(), /服务器已确认电脑登记/);
  assert.match(f.html(), /服务器已确认授权/);
  assert.doesNotMatch(f.html(), /data-act="rmShowQR"/);
  f.context.S.state.config.deviceId = 'other';
  assert.doesNotMatch(f.html(), /服务器已确认电脑登记/);
  f.context.S.state.config.deviceId = 'd';
  f.context.S.state.config.effectiveHubUrl = 'https://other.test';
  assert.doesNotMatch(f.html(), /服务器已确认电脑登记/);
});

test('an old connected snapshot cannot authorize a QR for another computer or Hub', () => {
  for (const hub of [{ state: 'connected', deviceId: 'other' }, { state: 'connected', hubUrl: 'https://other.test' }]) {
    const f = fixture('station', { deviceId: 'd', loggedIn: true, effectiveRemoteMode: 'hub', effectiveHubUrl: 'https://hub.test' }, hub);
    assert.doesNotMatch(f.html(), /data-act="rmShowQR"|服务器已确认电脑登记/);
  }
});

test('Bot phone management explains group authorization and never exposes Hub pairing', () => {
  const f = fixture('bot', { effectiveRemoteMode: 'bot', loggedIn: true }, { state: 'connected' });
  assert.match(f.html(), /同一份 Bot 绑定信息/);
  assert.doesNotMatch(f.html(), /data-act="rmShowQR"|data-act="pairRevoke"|id="pairQR"/);
});

test('Bot import explains invitation sensitivity and real online confirmation without extra primary actions', () => {
  const f = fixture('bot');
  const html = f.html();
  assert.match(html, /绑定信息含授权邀请，请勿公开分享/);
  assert.match(html, /以连接状态确认是否已上线/);
  assert.match(html, /aria-describedby="rmBotSecretNote"/);
  assert.equal(visibleControls(html).actions.filter(item => item.primary).length, 1);
  f.nodes.set('modalRoot', { innerHTML: '' });
  f.context.ACTIONS.rmBotTutorial();
  const help = f.nodes.get('modalRoot').innerHTML;
  assert.match(help, /不要手改 JSON/);
  assert.match(help, /开机、联网且不休眠/);
  assert.match(help, /重置原组手机绑定/);
  assert.match(help, /撤销邀请只停止新增绑定/);
  assert.doesNotMatch(help, /data-act="rmRecoverySessions"/);
});

test('a direct pairing QR appears once in phone management and not in the form', () => {
  const f = fixture('direct', { effectiveRemoteMode: 'direct' });
  vm.runInContext(`RM.direct = { enabled: true, running: true, paired: false, qrDataURL: 'data:fixture', expiresAt: Date.now() + 60000 }`, f.context);
  const html = f.html();
  assert.equal((html.match(/src="data:fixture"/g) || []).length, 1);
  assert.doesNotMatch(f.context.renderRemoteDirectPanel(false), /data:fixture/);
  f.context.S.state.config.effectiveRemoteMode = 'hub';
  assert.doesNotMatch(f.html(), /data:fixture/);
});

test('active direct pairing offers one QR action in the phone card regardless of browsed form', () => {
  for (const mode of ['station', 'bot', 'direct']) {
    const f = fixture(mode, { effectiveRemoteMode: 'direct' });
    vm.runInContext('RM.direct = { enabled: true, running: true, paired: false }', f.context);
    const html = f.html();
    assert.match(f.context.rmPhoneBindingMarkup(), /data-act="rmDirectQR"/);
    assert.equal((html.match(/data-act="rmDirectQR"/g) || []).length, 1);
    assert.doesNotMatch(f.context.rmPhoneBindingMarkup(), /启用直连后/);
  }
  const inactive = fixture('direct', { effectiveRemoteMode: 'none' });
  assert.match(inactive.context.renderRemoteDirectPanel(false), /data-act="rmDirectQR"[^>]*>[^<]*启用并配对/);
});

test('status refresh preserves editable form nodes and open advanced settings', () => {
  const f = fixture('station', { deviceId: 'd', effectiveRemoteMode: 'hub', loggedIn: true }, { state: 'connected' });
  const form = { value: 'my unfinished address', innerHTML: 'form stays', isConnected: true };
  const advanced = { open: true, innerHTML: 'advanced stays', isConnected: true };
  f.nodes.set('rmStation', form); f.nodes.set('rmAdvanced', advanced);
  f.nodes.set('rmConnectionSteps', { innerHTML: '', isConnected: true });
  f.nodes.set('rmPhoneBinding', { innerHTML: '', isConnected: true });
  f.context.rmUpdateRemoteStatus();
  assert.equal(f.nodes.get('rmStation'), form);
  assert.equal(form.value, 'my unfinished address');
  assert.equal(form.innerHTML, 'form stays');
  assert.equal(advanced.open, true);
  assert.equal(advanced.innerHTML, 'advanced stays');
  assert.match(f.nodes.get('rmConnectionSteps').innerHTML, /电脑与远程服务已连接/);
});

test('presentation loads before remote status handlers can call it', () => {
  const html = fs.readFileSync(path.join(web, 'index.html'), 'utf8');
  assert.ok(html.indexOf('src="remote-connect.js"') < html.indexOf('src="remote.js"'));
  assert.ok(html.indexOf('src="remote-connect.js"') < html.indexOf('src="shell.js"'));
});

test('normal key states expose one next action and keep recovery outside the main flow', () => {
  const cases = [
    { name: 'new station', mode: 'station', config: { effectiveRemoteMode: 'none' }, hub: {}, next: 'rmConnect' },
    { name: 'station awaiting phone', mode: 'station', config: { effectiveRemoteMode: 'hub', loggedIn: true }, hub: { state: 'connected' }, next: 'rmShowQR' },
    { name: 'station already paired', mode: 'station', config: { effectiveRemoteMode: 'hub', loggedIn: true }, hub: { state: 'connected', pairing: { paired: true } }, next: null },
    { name: 'new Bot', mode: 'bot', config: { effectiveRemoteMode: 'none' }, hub: {}, next: 'rmBotBind' },
    { name: 'connected Bot', mode: 'bot', config: { effectiveRemoteMode: 'bot', loggedIn: true, effectiveHubUrl: 'https://bot.test', remoteModes: { bot: { groupId: 'a' } }, remoteConnections: [{ mode: 'bot', hubUrl: 'https://bot.test', groupId: 'a' }] }, hub: { state: 'connected' }, next: null },
    { name: 'new direct', mode: 'direct', config: { effectiveRemoteMode: 'none' }, hub: {}, next: 'rmDirectQR' },
    { name: 'direct live QR', mode: 'direct', config: { effectiveRemoteMode: 'direct' }, hub: {}, direct: { enabled: true, running: true, qrDataURL: 'data:fixture', expiresAt: Date.now() + 60000 }, next: null, qr: 1 },
    { name: 'direct already paired', mode: 'direct', config: { effectiveRemoteMode: 'direct' }, hub: {}, direct: { enabled: true, running: true, paired: true, connected: true }, next: null },
  ];
  for (const row of cases) {
    const f = fixture(row.mode, row.config, row.hub);
    if (row.direct) vm.runInContext(`RM.direct = ${JSON.stringify(row.direct)}`, f.context);
    const html = f.html(), visible = visibleControls(html), primary = visible.actions.filter(action => action.primary);
    assert.deepEqual(primary.map(action => action.action), row.next ? [row.next] : [], row.name);
    assert.equal(visible.images.length, row.qr || 0, row.name);
    assert.equal(visible.actions.some(action => ['rmRetryEnrollment', 'rmBotIngress', 'rmReenroll', 'rmRecoverySessions'].includes(action.action)), false, row.name);
    if (row.hub.pairing?.paired) assert.equal(visible.actions.filter(action => action.action === 'pairRevoke').length, 1);
  }
});
