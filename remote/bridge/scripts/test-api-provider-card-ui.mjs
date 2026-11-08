import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

const source = await readFile(new URL('../internal/console/web/app.js', import.meta.url), 'utf8');
const start = source.indexOf('function accountLaunchToolIcon(target) {');
const end = source.indexOf('\nfunction renderLocalCards()', start);
assert.ok(start >= 0 && end > start, 'card function must be present');
const escSource = source.split('\n').find((line) => line.startsWith('const esc = '));
function harness(local = { tools: [], bindings: {} }, extra = {}) {
  const context = vm.createContext({
    URL, S: { local }, icon: (name) => `<svg data-icon="${name}"></svg>`,
    wbMore: () => { throw new Error('ordinary card operations must remain directly visible'); },
    ...extra,
  });
  vm.runInContext(escSource + '\n' + source.slice(start, end), context);
  vm.runInContext(source.slice(source.indexOf('function localHubKey('), source.indexOf('function localForm(')), context);
  return { render: (account) => context.localAccountCard(account), context };
}
const fixture = () => ({
  id: 'synthetic-api', name: '主力 API', kind: 'api', baseUrl: 'https://relay.example.test/custom/v1',
  keyMasked: 'sk-••••••••demo', key: 'synthetic-raw-key-must-not-render', wire: 'responses', models: ['model-a', 'model-b'],
});

test('spending stays hidden while current multipliers remain visible until expiry', () => {
  const h=harness();
  h.context.data={today:{actualCost:{amount:1.5,currency:'USD'}},billing:{source:'sub2api.key_billing',scope:'token',effectiveMultiplier:.1,freshUntil:Date.now()+60000}};
  let html=vm.runInContext('apiCardUsageMetrics(data)',h.context);
  assert.doesNotMatch(html,/今日费用|\$1\.5/);assert.match(html,/当前倍率 ×0\.1/);assert.doesNotMatch(html,/\$0\.15/);
  h.context.data.billing.freshUntil=1;
  html=vm.runInContext('apiCardUsageMetrics(data)',h.context);
  assert.doesNotMatch(html,/当前倍率|今日费用|\$1\.5/);
});

test('current group is explicit upstream metadata, escaped and hidden when unavailable or stale', () => {
  const h=harness();
  h.context.data={group:{name:'开发组 <img>',source:'provider_key_usage',freshUntil:Date.now()+60000},billing:{source:'sub2api.key_billing',scope:'token',effectiveMultiplier:.1,freshUntil:Date.now()+60000}};
  let html=h.context.apiCardUsageMetrics(h.context.data);
  assert.match(html,/分组 开发组 &lt;img&gt; · 当前倍率 ×0\.1/);assert.doesNotMatch(html,/<img>/);
  for(const change of [{source:'model_guess'},{freshUntil:1},{name:'bad\u202Egroup'},{name:'x'.repeat(121)},{freshUntil:Infinity}]) {
    html=h.context.apiCardUsageMetrics({...h.context.data,group:{...h.context.data.group,...change}});
    assert.doesNotMatch(html,/分组 /);assert.match(html,/当前倍率 ×0\.1/);
  }
  assert.doesNotMatch(h.context.apiCardUsageMetrics({}),/分组 |默认组|钱包余额/);
});

test('ordinary API card shows direct actions and only masked credentials', () => {
  const html = harness().render(fixture());
  assert.match(html, /api-provider-card/);
  assert.match(html, /api-provider-badge">API</);
  assert.match(html, /sk-••••••••demo/);
  assert.match(html, /relay\.example\.test\/custom\/v1/);
  for (const action of ['localEdit', 'localModels', 'localDelete', 'apiCardMembership', 'accountModelRules']) assert.match(html, new RegExp(`data-act="${action}"`));
  for (const action of ['localEdit', 'localModels', 'localDelete']) assert.match(html, new RegExp(`data-act="${action}" data-id="synthetic-api"`));
  assert.doesNotMatch(html, /<details|更多|wb-card-more|synthetic-raw-key|toggleVis|OAuth|工具配置/);
  for (const label of ['供应商余额', '今日请求', '今日 Token']) assert.match(html, new RegExp(label + '<\\/span><b>—<\\/b>'));
  assert.match(html, /加入 API 服务/);
  assert.doesNotMatch(html, /api-provider-meta|api-provider-wire|class="api-provider-service"|未加入 API 服务/);
  assert.match(html, /已保存模型<\/span><b>2<\/b><button[^>]+data-act="apiCardMembership"/);
  for (const target of ['codex-desktop', 'codex', 'claude-desktop', 'claude']) {
    assert.match(html, new RegExp(`data-act="accountOpen" data-id="synthetic-api" data-target="${target}"`));
  }
  assert.doesNotMatch(html, /data-act="localLaunch"/);
});

test('official credentials only offer Codex; quota-only and unlinked accounts cannot launch', () => {
  const h = harness();
  const render = h.context.accountLaunchButtonsHTML;
  assert.equal(render({ ...fixture(), libraryOnly: true }), '');
  assert.equal(render({ ...fixture(), officialAccount: { mode: 'chatgpt_web_session' } }), '');
  for (const extra of [{ officialAccount: { mode: 'legacy_codex_cli' } }, { sourceType: 'official_account' }]) {
    const html = render({ ...fixture(), ...extra });
    assert.match(html, /data-target="codex-desktop"/);
    assert.match(html, /data-target="codex"/);
    assert.doesNotMatch(html, /data-target="claude/);
  }
});

test('service membership comes from real active client roles, never optimistic member flags', () => {
  const account = fixture();
  account.apiService = { member: true, activeClientCount: 99, clients: [] };
  assert.match(harness().render(account), />加入 API 服务<\/button>/);
  account.apiService.clients = [{ clientId: 'client-a', clientName: '文字接口', active: true, text: true, image: false, primary: true, disabled: false }];
  const html = harness().render(account);
  assert.match(html, /文字接口 · 主账号 \/ 文字/);
  assert.match(html, />移出 API 服务<\/button>/);
  assert.doesNotMatch(html, /服务运行中|正在运行|本机已启动|已绑定/);
});

test('primary anchors, disabled clients and unknown activation remain configured, not running', () => {
  for (const client of [
    { primary: true, text: false, image: false, active: false },
    { text: true, image: true, active: false, disabled: true },
    { text: true, active: true, disabled: true },
    { text: true },
    { primary: true, text: false, image: false, active: true },
  ]) {
    const html = harness().render({ ...fixture(), apiService: { clients: [{ clientId: 'client-a', clientName: '配置接口', ...client }] } });
    assert.match(html, />移出 API 服务<\/button>/);
    assert.doesNotMatch(html, /已加入 \d+ 个启用客户端/);
  }
});

test('client IDs are counted once and malformed entries do not invent membership', () => {
  const client = { clientId: 'client-a', active: true, image: true };
  const html = harness().render({ ...fixture(), apiService: { clients: [null, {}, { active: true, text: true }, client, client] } });
  assert.equal((html.match(/data-act="apiCardMembership"/g) || []).length, 1);
  assert.equal((html.match(/客户端 · 图片/g) || []).length, 1);
  assert.doesNotMatch(html, /已加入 [2-9]/);
});

test('API card omits tool binding details without removing direct launch actions', () => {
  const local = {
    tools: [{ id: 'codex', name: 'Codex' }, { id: 'claude', name: 'Claude Code' }, { id: 'future', name: 'Future Agent' }],
    bindings: {
      codex: { accountId: 'new-api', appliedId: 'synthetic-api', pending: true },
      claude: { accountId: 'synthetic-api', appliedId: 'synthetic-api', pending: false },
      future: { accountId: 'synthetic-api', appliedId: 'other-api', pending: true },
    },
  };
  const html = harness(local).render(fixture());
  assert.doesNotMatch(html, /工具配置|上次应用|已应用|待应用|Future Agent|正在运行|使用中/);
  assert.equal((html.match(/data-act="accountOpen"/g) || []).length, 6);
});

test('tool binding and unrelated usage do not become provider card statistics', () => {
  const html = harness({ tools: [{ id: 'codex', name: 'Codex' }], bindings: { codex: { accountId: 'synthetic-api', appliedId: 'synthetic-api' } } }).render(fixture());
  assert.equal((html.match(/<b>—<\/b>/g) || []).length, 3);
  assert.doesNotMatch(html, /<small>已应用|<small>待应用/);
});

test('model count accepts safe backend numbers and otherwise uses the saved array', () => {
  assert.match(harness().render({ ...fixture(), modelCount: 0 }), /已保存模型<\/span><b>0<\/b>/);
  assert.match(harness().render({ ...fixture(), modelCount: -5 }), /已保存模型<\/span><b>2<\/b>/);
  assert.match(harness().render({ ...fixture(), modelCount: '<img>', models: [] }), /已保存模型<\/span><b>0<\/b>/);
  assert.match(harness(undefined, { S: {} }).render(fixture()), /供应商余额<\/span><b>—<\/b>/);
});

test('untrusted names and IDs are escaped; URL credentials and query fragments never appear', () => {
  const account = { ...fixture(), id: 'id" onclick="alert(1)', name: '<img src=x onerror=alert(1)>', baseUrl: 'https://name:synthetic-password@relay.example.test/v1?api_key=synthetic-url-secret#private' };
  const html = harness().render(account);
  assert.match(html, /&lt;img/);
  assert.match(html, /data-id="id&quot; onclick=&quot;alert\(1\)"/);
  assert.match(html, /https:\/\/relay\.example\.test\/v1/);
  assert.doesNotMatch(html, /<img src=x|synthetic-password|synthetic-url-secret|#private/);
  assert.doesNotMatch(harness().render({ ...fixture(), baseUrl: 'javascript:alert(1)' }), /javascript:/);
});

test('official accounts retain their dedicated card renderer and selected-tool context', () => {
  let called;
  const h = harness({ tools: [{ id: 'codex', name: 'Codex' }], bindings: { codex: { accountId: 'synthetic-api' } } }, {
    officialAccountCard: (...args) => { called = args; return '<article>official fixture</article>'; },
  });
  const account = { ...fixture(), officialAccount: { id: 'official-fixture', status: 'ready' } };
  assert.equal(h.render(account), '<article>official fixture</article>');
  assert.equal(called[0], account);
  assert.deepEqual([...called[1]], ['Codex']);
  assert.equal(typeof called[2], 'function');
});

test('launch row uses bundled tool artwork, CLI corner marks and full accessible names', () => {
  const h = harness(), html = h.context.accountLaunchButtonsHTML(fixture());
  assert.equal((html.match(/<img /g) || []).length, 4);
  assert.equal((html.match(/class="account-tool-cli"/g) || []).length, 2);
  for (const family of ['codex', 'claude']) assert.equal((html.match(new RegExp('agent-brand/' + family + '-app-official.png', 'g')) || []).length, 2);
  for (const name of ['Codex', 'Codex CLI', 'Claude Code', 'Claude Desktop']) assert.match(html, new RegExp('aria-label="使用 主力 API 打开 ' + name + '"'));
  assert.doesNotMatch(html, /<span>Codex|<span>Claude/);
  const official = h.context.accountLaunchButtonsHTML({ ...fixture(), sourceType: 'official_account' });
  assert.equal((official.match(/<img /g) || []).length, 2);
  assert.doesNotMatch(official, /claude-app/);
});

test('local service shares icon helper but only offers its two Codex targets', async () => {
  const h = harness();
  const serviceSource = await readFile(new URL('../internal/console/web/codex-accounts.js', import.meta.url), 'utf8');
  const begin = serviceSource.indexOf('function localAPIServiceLaunchHTML(status) {');
  const finish = serviceSource.indexOf('\nfunction apiServiceLaunchCurrent(', begin);
  vm.runInContext(`const API_SERVICE_TOOLS = {'codex-desktop':'Codex',codex:'Codex CLI'}; const CODEX_API_OVERVIEW = {};\n` + serviceSource.slice(begin, finish), h.context);
  const html = h.context.localAPIServiceLaunchHTML({ running: true, clients: [{ disabled: false }] });
  assert.equal((html.match(/<img /g) || []).length, 2);
  assert.match(html, /aria-label="使用本地 API 服务打开 Codex CLI"/);
  assert.doesNotMatch(html, /claude|<span>Codex/);
});

const revision = (letter = 'a') => 'src-v1-' + letter.repeat(64);
function deferred() { let resolve, reject; const promise = new Promise((yes, no) => { resolve = yes; reject = no; }); return { promise, resolve, reject }; }
async function settle() { for (let i = 0; i < 24; i++) await Promise.resolve(); }
function usageHarness(accounts = [{ ...fixture(), sourceRevision: revision() }]) {
  let now = new Date(2026, 9, 13, 12).getTime(), nextTimer = 0;
  const nodes = new Map(), timers = new Map(), requests = [];
  class Clock extends Date { constructor(value = now) { super(value); } static now() { return now; } }
  const local = { tools: [], accounts };
  const h = harness(local, {
    S: { route: 'accounts', local }, Date: Clock,
    document: { getElementById: (id) => nodes.get(id) || null },
    setTimeout: (fn, delay) => { const id = ++nextTimer; timers.set(id, { fn, at: now + delay }); return id; },
    clearTimeout: (id) => timers.delete(id),
    api: (path) => { const pending = deferred(); requests.push({ path, ...pending }); return pending.promise; },
  });
  function mount(rows = accounts) {
    for (const node of nodes.values()) node.isConnected = false;
    nodes.clear(); local.accounts = rows;
    for (const row of rows) nodes.set('apiAccountUsage-' + row.id, { id: 'apiAccountUsage-' + row.id, isConnected: true, innerHTML: h.context.apiCardUsageMetrics(null) });
    h.context.loadAPICardUsage(rows);
    return nodes;
  }
  const payload = (account = accounts[0], extra = {}) => ({
    id: account.id, sourceRevision: account.sourceRevision, observedAt: now, freshUntil: now + 60000,
    balance: { amount: 38.61, currency: 'USD' }, today: { date: h.context.apiCardUsageDate(now), basis: 'provider_today', requests: 0, tokens: 12500 }, ...extra,
  });
  return { ...h, local, requests, nodes, timers, mount, payload, now: () => now, advance: (ms) => { now += ms; for (const [id, timer] of [...timers]) if (timer.at <= now) { timers.delete(id); timer.fn(); } } };
}

test('card paints unknown first, fetches only exact API ID and updates only its three metrics', async () => {
  const h = usageHarness(); h.mount();
  const node = [...h.nodes.values()][0];
  assert.equal((node.innerHTML.match(/<b>—<\/b>/g) || []).length, 3);
  await settle(); assert.equal(h.requests.length, 1);
  assert.equal(h.requests[0].path, '/api/local/accounts/usage?id=synthetic-api');
  h.requests[0].resolve(h.payload()); await settle();
  assert.match(node.innerHTML, /\$38\.61/);
  assert.match(node.innerHTML, /今日请求<\/span><b>0/);
  assert.match(node.innerHTML, /12,500/);
  assert.match(node.innerHTML, /今日按服务商时区/);
});

test('group display travels with source-fenced usage and expires without additional requests', async () => {
  const h = usageHarness(); h.mount(); await settle();
  h.requests[0].resolve(h.payload(undefined,{group:{name:'工作组',source:'provider_key_usage',freshUntil:h.now()+2000}}));
  await settle();
  assert.match([...h.nodes.values()][0].innerHTML,/分组 工作组/);
  h.advance(2001);
  assert.doesNotMatch([...h.nodes.values()][0].innerHTML,/分组 工作组/);
  assert.equal(h.requests.length,1);
});

test('card read pool caps concurrency at four and does not mix different APIs on one host', async () => {
  const accounts = Array.from({ length: 7 }, (_, i) => ({ ...fixture(), id: 'api-' + i, sourceRevision: revision() }));
  const h = usageHarness(accounts); h.mount(); await settle();
  assert.equal(h.requests.length, 4);
  h.requests[2].resolve(h.payload(accounts[2], { balance: { amount: -5, currency: 'CNY' } })); await settle();
  assert.equal(h.requests.length, 5);
  assert.match(h.nodes.get('apiAccountUsage-api-2').innerHTML, /CNY -5/);
  assert.doesNotMatch(h.nodes.get('apiAccountUsage-api-1').innerHTML, /CNY|\$38/);
  for (let i = 0; i < 4; i++) if (i !== 2) h.requests[i].resolve(h.payload(accounts[i]));
  await settle(); assert.equal(h.requests.length, 7);
});

test('leaving the page discards queued work; returning may read only currently visible cards', async () => {
  const accounts = Array.from({ length: 9 }, (_, i) => ({ ...fixture(), id: 'api-' + i, sourceRevision: revision() }));
  const h = usageHarness(accounts); h.mount(); await settle();
  h.context.S.route = 'overview';
  h.requests.slice().forEach((row, i) => row.resolve(h.payload(accounts[i]))); await settle();
  assert.equal(h.requests.length, 4, 'no queued stale page reads started');
  h.context.S.route = 'accounts'; h.mount([accounts[8]]); await settle();
  assert.equal(h.requests.length, 5);
  assert.equal(h.requests[4].path, '/api/local/accounts/usage?id=api-8');
});

test('same identity rerender shares pending read but late reply never writes detached card', async () => {
  const h = usageHarness(); h.mount(); await settle(); const old = [...h.nodes.values()][0];
  h.mount(); await settle(); assert.equal(h.requests.length, 1);
  h.requests[0].resolve(h.payload()); await settle();
  assert.doesNotMatch(old.innerHTML, /\$38/);
  assert.match([...h.nodes.values()][0].innerHTML, /\$38/);
  h.mount(); await settle(); assert.equal(h.requests.length, 1, 'fresh exact revision cached');
  h.advance(60000);
  assert.equal(([...h.nodes.values()][0].innerHTML.match(/<b>—<\/b>/g) || []).length, 3);
  assert.equal(h.requests.length, 1, 'expiry never auto polls');
});

test('leaving page, removing account, converting to official, or changing key fences late responses', async () => {
  for (const change of ['route', 'delete', 'official', 'revision']) {
    const h = usageHarness(); h.mount(); await settle(); const old = [...h.nodes.values()][0];
    if (change === 'route') h.context.S.route = 'overview';
    if (change === 'delete') h.local.accounts = [];
    if (change === 'official') h.local.accounts = [{ ...h.local.accounts[0], sourceType: 'official_account' }];
    if (change === 'revision') h.local.accounts = [{ ...h.local.accounts[0], sourceRevision: revision('b') }];
    h.requests[0].resolve(h.payload()); await settle();
    assert.doesNotMatch(old.innerHTML, /\$38/, change);
  }
});

test('changed revision can fetch anew without consuming prior credential response', async () => {
  const h = usageHarness(); h.mount(); await settle();
  const changed = { ...h.local.accounts[0], sourceRevision: revision('b') }; h.mount([changed]); await settle();
  assert.equal(h.requests.length, 2);
  h.requests[0].resolve(h.payload()); await settle(); assert.doesNotMatch([...h.nodes.values()][0].innerHTML, /\$38/);
  h.requests[1].resolve(h.payload(changed, { balance: { amount: 21, currency: 'EUR' } })); await settle();
  assert.match([...h.nodes.values()][0].innerHTML, /EUR 21/);
});

test('unknown, malformed and stale values never invent zeros or currency', async () => {
  const h = usageHarness();
  for (const input of [null, {}, { balance: { amount: '10', currency: 'USD' }, today: { requests: null, tokens: '99' } }, { balance: { amount: 1, currency: '' }, today: { requests: -1, tokens: Infinity } }]) {
    assert.equal((h.context.apiCardUsageMetrics(input).match(/<b>—<\/b>/g) || []).length, 3);
  }
  for (const extra of [{ id: 'other-api' }, { sourceRevision: revision('b') }, { observedAt: h.now() + 1 }, { freshUntil: h.now() }, { observedAt: h.now() - 60001, freshUntil: h.now() + 1000 }]) {
    const row = h.payload(undefined, extra);
    assert.equal(h.context.apiCardUsageData(row, { id: 'synthetic-api', revision: revision() }, h.now()), null);
  }
  const data = h.context.apiCardUsageData(h.payload(undefined, { today: { date: '1900-01-01', requests: 99, tokens: 99 } }), { id: 'synthetic-api', revision: revision() }, h.now());
  assert.equal(data.today, null);
});

test('absent source revision, official account, and library placeholders do not start provider requests', async () => {
  for (const patch of [{ sourceRevision: undefined }, { sourceRevision: 'unknown' }, { sourceRevision: [revision()] }, { officialAccount: {} }, { sourceType: 'official_account' }, { libraryOnly: true }]) {
    const h = usageHarness([{ ...fixture(), sourceRevision: revision(), ...patch }]); h.mount(); await settle(); assert.equal(h.requests.length, 0);
  }
});

test('failed lookup remains unknown and a later manual rerender can retry', async () => {
  const h = usageHarness(); h.mount(); await settle(); h.requests[0].reject(new Error('synthetic unavailable')); await settle();
  assert.equal(([...h.nodes.values()][0].innerHTML.match(/<b>—<\/b>/g) || []).length, 3);
  assert.equal(h.requests.length, 1);
  h.mount(); await settle(); assert.equal(h.requests.length, 2);
});

test('midnight expires today values without network polling', async () => {
  const h = usageHarness(); h.advance(12 * 3600000 - 1000); h.mount(); await settle();
  h.requests[0].resolve(h.payload()); await settle(); assert.match([...h.nodes.values()][0].innerHTML, /12,500/);
  h.advance(1000); assert.doesNotMatch([...h.nodes.values()][0].innerHTML, /12,500/); assert.equal(h.requests.length, 1);
});

test('currency labels and account IDs stay text or encoded query data', async () => {
  const account = { ...fixture(), id: 'a&other=b?', sourceRevision: revision() };
  const h = usageHarness([account]); h.mount(); await settle();
  assert.equal(h.requests[0].path, '/api/local/accounts/usage?id=a%26other%3Db%3F');
  h.requests[0].resolve(h.payload(account, { balance: { amount: 1, currency: '<img src=x>' } })); await settle();
  const html = [...h.nodes.values()][0].innerHTML;
  assert.doesNotMatch(html, /<img|\$1/);
  assert.match(html, /&lt;img src=x&gt; 1/);
});
