import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';
const source = await readFile(new URL('../internal/console/web/codex-accounts.js', import.meta.url), 'utf8');
const esc = (v) => String(v).replace(/[&<>"']/g, (c) => ({ '&':'&amp;', '<':'&lt;', '>':'&gt;', '"':'&quot;', "'":'&#39;' })[c]);
function harness() {
  let now = 1900000000000, timer;
  const card = { outerHTML: '' }, calls = [];
  class Clock extends Date { static now() { return now; } }
  const context = vm.createContext({ Date: Clock, URL, TextEncoder, $: (selector) => selector === '#localAPIService' ? card : null,
    ACTIONS: {}, S: { route: 'accounts', local: { accounts: [{ id: 'not-a-member', name: 'NOT IN SERVICE' }] } }, esc, icon: () => '',
    setTimeout: (fn, delay) => { timer = { fn, delay }; return 1; }, clearTimeout: () => { timer = null; },
    MutationObserver: class { observe() {} }, document: { addEventListener() {} }, busy: async (_b, fn) => fn(),
    api: async (...args) => { calls.push(args); throw Error('No network expected'); }, confirm: () => true, toast() {},
  });
  vm.runInContext(source, context);
  context.patchOfficialObservationHTML = (current, html) => { if (current) current.outerHTML = html; };
  return { context, calls, card, now, setTime: (value) => { now = value; }, get timer() { return timer; },
    run: (code) => vm.runInContext(code, context),
    render(members) { context.fixture = { enabled: true, running: true, clients: [], statistics: [], summary: { checkedAt: now, members } }; return vm.runInContext('CODEX_API_OVERVIEW.status=fixture;codexAPIOverviewHTML()', context); },
  };
}
function member(id, overrides = {}) {
  return { id, name: '账号 ' + id, sourceType: 'official_account', availability: 'ready',
    usage: { verified: true, status: 'fresh', source: 'openai_codex_usage_api', freshUntil: new Date(1900000060000).toISOString(),
      plan: { verified: true, source: 'openai_codex_usage_api', family: 'pro', raw: 'pro' },
      rateLimit: { primary: { windowMinutes: 300, remainingPercent: 60 }, secondary: { windowMinutes: 10080, remainingPercent: 80 } } },
    ...overrides };
}
const plain = (v) => JSON.parse(JSON.stringify(v));

test('service summary never projects quota observations into dollar balances', () => {
  const h = harness();
  h.context.cost = {status:'estimated',freshUntil:new Date(h.now+60000).toISOString(),totalUSD:20,windowUsedUSD:5,remainingUSD:15,knownAccounts:2,totalAccounts:2};
  assert.equal(h.run('apiServiceCostsHTML(cost)'), '');
  h.render([member('a')]);
  h.run('fixture.summary.costEstimate=cost');
  const html = h.run('codexAPIOverviewHTML()');
  assert.doesNotMatch(html, /\$|总量（估算）|已用（估算）|剩余（估算）/);
  assert.match(html, /60%|80%/);
  assert.equal(h.calls.length, 0);
});

test('summary only uses backend routed members, deduplicates ids and never falls back to vault', () => {
  const h = harness(); const html = h.render([member('a'), member('a'), member('b')]);
  const summary = h.run('apiServiceSummary(fixture)');
  assert.equal(summary.total, 2); assert.equal(summary.ready, 2);
  assert.doesNotMatch(html, /NOT IN SERVICE/);
  assert.match(html, /可用账号/); assert.match(html, /已加入账号/);
});
test('ordinary configured APIs and unverifiable official snapshots never become ready', () => {
  const h = harness(), stale = member('stale'), missing = member('missing'), imported = member('hint');
  stale.usage.status = 'stale'; missing.usage.freshUntil = undefined; imported.usage.source = 'jwt_hint';
  h.render([member('key', { sourceType:'api_key' }), stale, missing, imported]);
  const s = h.run('apiServiceSummary(fixture)');
  assert.equal(s.ready, 0); assert.equal(s.unknown, 4);
  assert.match(h.run('codexAPIOverviewHTML()'), /上游 API/);
});
test('quota ranges stay within the exact raw plan and matching window instead of summing percentages', () => {
  const h = harness(), a = member('a'), b = member('b'), c = member('c');
  a.usage.rateLimit.primary.remainingPercent = 20;
  b.usage.rateLimit.primary.remainingPercent = 70;
  c.usage.plan.raw = 'prolite'; c.usage.rateLimit.primary.remainingPercent = 99;
  const html = h.render([a,b,c]);
  assert.equal(h.run('apiServiceSummary(fixture).groups.length'), 2);
  assert.match(html, /20%–70%/); assert.match(html, /Pro · prolite/);
  assert.doesNotMatch(html, /90%|189%|\$\d/);
});
test('different durations and missing windows are not silently combined', () => {
  const h = harness(), a = member('a'), b = member('b');
  a.usage.rateLimit.secondary = undefined;
  b.usage.rateLimit.primary.windowMinutes = 60;
  h.render([a,b]);
  const g = h.run('apiServiceSummary(fixture).groups[0]');
  assert.equal(g.windows.size, 3);
  assert.match(h.run('codexAPIOverviewHTML()'), /1\/2 个账号有当前数据/);
});
test('null, strings, non-finite and out-of-range quota data stay unknown', () => {
  const h = harness();
  for (const remainingPercent of [null, '', '80', NaN, Infinity, -1, 101]) {
    h.context.usage = { rateLimit: { primary: { remainingPercent, windowMinutes: 300 } } };
    assert.equal(h.run('apiServiceQuotaWindows(usage).length'), 0);
  }
});
test('stale or failed status reads do not display cached green availability or live ranges', () => {
  const h = harness(); h.render([member('a')]); h.run('CODEX_API_OVERVIEW.error=true');
  const html = h.run('codexAPIOverviewHTML()');
  assert.match(html, /状态待刷新/); assert.match(html, /<b>—<\/b><span>可用账号/);
  assert.doesNotMatch(html, /60%|80%/);
  assert.equal(h.run('apiServiceSummary(fixture).unknown'), 1);
});
test('expiry updates only local display and stops scheduling after cache expires', () => {
  const h = harness(); h.render([member('a')]); h.run('renderCodexAPIOverview()');
  assert.equal(h.timer.delay, 60001);
  const update = h.timer.fn; h.setTime(h.now + 60001); update();
  assert.equal(h.timer, null); assert.match(h.card.outerHTML, /0<\/b><span>可用账号/);
  assert.equal(h.calls.length, 0);
});
test('leaving accounts route prevents expiry timer from touching another page', () => {
  const h = harness(); h.render([member('a')]); h.run('renderCodexAPIOverview()');
  const timer = h.timer.fn, html = h.card.outerHTML;
  h.context.S.route = 'overview'; h.setTime(h.now+61000); timer();
  assert.equal(h.card.outerHTML, html); assert.equal(h.timer, null); assert.equal(h.calls.length, 0);
});
test('malicious labels are escaped and raw tiers are not mapped to unproven prices', () => {
  const h = harness(), a = member('a', { name:'<img src=x onerror=alert(1)>' });
  a.usage.plan.raw = '<script>pro500</script>';
  const html = h.render([a]);
  assert.doesNotMatch(html, /<img|<script>|\$500/);
  assert.match(html, /&lt;img/); assert.match(html, /&lt;script&gt;/);
});
test('missing summary is visibly unknown and main card has no endpoint or manual-key action', () => {
  const h = harness();
  h.context.fixture = { running:true, baseUrl:'http://127.0.0.1:9999/codex-api/v1', clients:[{ id:'x', accountId:'a' }], statistics:[] };
  const html = h.run('CODEX_API_OVERVIEW.status=fixture;codexAPIOverviewHTML()');
  assert.match(html, /账号状态待检查/); assert.match(html, /data-act="codexAPI"/);
  assert.doesNotMatch(html, /127\.0\.0\.1|copyVal|codexAPIRotate|apiCreatedKey/);
  assert.equal(h.run('apiServiceSummary(fixture)'), null);
});
test('HTTP counts remain request statistics and unknown members stay visible separately', () => {
  const h = harness();
  h.render([member('a'), member('b', {availability:'unavailable'}), member('c',{availability:'unknown'})]);
  const s = h.run('apiServiceSummary(fixture)');
  assert.deepEqual(plain({ ready:s.ready, unavailable:s.unavailable, unknown:s.unknown }), { ready:1, unavailable:1, unknown:1 });
  const html = h.run('codexAPIOverviewHTML()');
  assert.match(html, /1 未验证/); assert.match(html, /1 暂不可用/);
});

test('the local API aggregate card glows only for confirmed running service state', () => {
  const h = harness();
  const html = h.render([member('a'), member('b')]);
  assert.match(html, /<article class="wb-card account-card api-service-card account-is-active" id="localAPIService"/);
  assert.equal((html.match(/account-is-active/g) || []).length, 1, 'service activity belongs only to the aggregate card');
  for (const mutate of ['fixture.running=false', 'fixture.running="true"', 'fixture.enabled=false', 'CODEX_API_OVERVIEW.error=true', 'CODEX_API_OVERVIEW.pending=true', 'CODEX_API_OVERVIEW.status=null']) {
    h.run('CODEX_API_OVERVIEW.error=false; CODEX_API_OVERVIEW.pending=false'); h.render([member('a')]);
    h.run(mutate); h.run('renderCodexAPIOverview()');
    assert.doesNotMatch(h.card.outerHTML, /account-is-active/, mutate);
  }
  assert.equal(h.calls.length, 0);
});

test('service stop and failed status reread remove the aggregate border through existing reconciliation', async () => {
  const h = harness(); h.render([member('a')]); h.run('renderCodexAPIOverview()');
  assert.match(h.card.outerHTML, /account-is-active/);
  h.context.api = async () => ({ enabled: false, running: false, clients: [] });
  await h.run('loadCodexAPIOverview()'); assert.doesNotMatch(h.card.outerHTML, /account-is-active/);
  h.render([member('a')]); h.run('renderCodexAPIOverview()'); assert.match(h.card.outerHTML, /account-is-active/);
  h.context.api = async () => { throw Error('synthetic status unavailable'); };
  await h.run('loadCodexAPIOverview()'); assert.doesNotMatch(h.card.outerHTML, /account-is-active/); assert.match(h.card.outerHTML, /状态待刷新/);
});

test('a pending service toggle immediately clears the border until current status is reread', async () => {
  const h = harness(); h.render([member('a')]); h.run('renderCodexAPIOverview()');
  let finish;
  h.context.api = url => url.endsWith('/enabled') ? new Promise(resolve => { finish = resolve; }) : Promise.resolve({ enabled: false, running: false, clients: [] });
  const pending = h.run('toggleCodexAPIService({dataset:{enabled:"false"}},true)');
  assert.doesNotMatch(h.card.outerHTML, /account-is-active/); assert.equal(h.run('CODEX_API_OVERVIEW.pending'), true);
  finish({ ok: true }); await pending;
  assert.doesNotMatch(h.card.outerHTML, /account-is-active/); assert.equal(h.run('CODEX_API_OVERVIEW.pending'), false);
});
