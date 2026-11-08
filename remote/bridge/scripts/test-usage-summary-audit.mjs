import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

// Run the shipped aggregation boundary, with synthetic observations only.
// No credentials, native files, browser state, network or paid API is used.
const chartParts = new Map();
const chart = { innerHTML: '', clientWidth: 640, querySelector: (selector) => {
  if (selector === 'svg') return { querySelector: (name) => chartParts.get(name), getBoundingClientRect: () => ({ left: 0, width: 640 }) };
  if (selector === '.us-tip') return chartParts.get(selector);
  return null;
} };
for (const name of ['.us-guide', '.us-dot', '.us-hit', '.us-tip']) chartParts.set(name, { hidden: true, innerHTML: '', style: {}, setAttribute() {}, addEventListener() {} });
const context = vm.createContext({ Date, console, RENDER_usage: null, $: () => chart, $$: () => [], icon: () => '', ico: () => '', esc: (value) => String(value).replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('"', '&quot;'), addEventListener() {} });
vm.runInContext(await readFile(new URL('../internal/console/web/usage-data.js', import.meta.url), 'utf8'), context);
vm.runInContext(await readFile(new URL('../internal/console/web/usage.js', import.meta.url), 'utf8'), context);
const summarize = vm.runInContext('USAGE.summarize', context);
const render = (data) => { const view = { innerHTML: '' }; vm.runInContext('drawUsage', context)(view, data); return view.innerHTML; };
const now = new Date();
const date = `${now.getFullYear()}-${String(now.getMonth() + 1).padStart(2, '0')}-${String(now.getDate()).padStart(2, '0')}`;
const source = (id, usage) => ({ id, name: id, kind: 'api', ok: true, usage });
const cases = [
  ['Token-only observations do not manufacture spending data', () => {
    const result = summarize({ days: 7, sources: [source('tokens-only', { daily_usage: [{ date, total_tokens: 123 }], today: { total_tokens: 123 } })] });
    assert.equal('rangeCost' in result, false);
    assert.equal('cost' in result.today, false);
    assert.equal(result.rangeTokens, 123);
    const html = render(result);
    assert.ok(html.includes('今日 Token')); assert.ok(!html.includes('花费'));
    assert.ok(!html.includes('$0.00'), 'missing charge must not render as a zero-dollar bill');
  }],
  ['provided and missing charges are ignored while all Token activity is counted', () => {
    const result = summarize({ days: 7, sources: [source('priced', { daily_usage: [{ date, total_tokens: 10, actual_cost: 0.2 }] }), source('unpriced', { daily_usage: [{ date, total_tokens: 20 }] })] });
    assert.equal(result.rangeTokens, 30);
    assert.equal('rangeCost' in result, false);
    assert.equal('cost' in result.sources[0].range, false);
    assert.equal('cost' in result.sources[1].range, false);
    assert.equal(result.sources[0].usage.daily_usage[0].actual_cost, .2, 'raw provider metadata is preserved');
    assert.ok(!render(result).includes('部分来源金额')); assert.ok(render(result).includes('近 7 天 Token'));
  }],
  ['raw historical monetary metadata is preserved without deriving a spending summary', () => {
    const payload = { days: 7, sources: [source('actual-zero', { remaining: 0, daily_usage: [{ date, total_tokens: 123, actual_cost: 0 }], today: { total_tokens: 123, actual_cost: 0 } })] };
    const before = JSON.stringify(payload);
    const result = summarize(payload);
    assert.equal(JSON.stringify(payload), before);
    assert.equal(result.sources[0].usage.today.actual_cost, 0);
    assert.equal('cost' in result.today, false);
    assert.equal(result.balance.value, 0);
    assert.ok(!render(result).includes('今日花费'), 'retained charge metadata does not create a spending display');
  }],
  ['balance-only source preserves real remaining balance without changing Token totals', () => {
    const result = summarize({ days: 7, sources: [source('priced', { daily_usage: [{ date, total_tokens: 10, actual_cost: 0.2 }], today: { total_tokens: 10, actual_cost: 0.2 } }), source('balance-only', { remaining: 5 })] });
    assert.equal(result.balance.value, 5);
    assert.equal(result.today.tokens, 10);
    assert.equal('rangeCostKnown' in result, false);
    assert.equal(result.rangeTokens, 10);
  }],
  ['retained charge gaps do not remove known Token activity', () => {
    const ago = (offset) => { const day = new Date(now); day.setDate(day.getDate() - offset); return `${day.getFullYear()}-${String(day.getMonth() + 1).padStart(2, '0')}-${String(day.getDate()).padStart(2, '0')}`; };
    const result = summarize({ days: 3, sources: [source('mixed-days', { daily_usage: [{ date: ago(2), total_tokens: 10, actual_cost: 0.1 }, { date: ago(1), total_tokens: 20 }, { date, total_tokens: 30, actual_cost: 0.2 }] })] });
    vm.runInContext("USAGE_VIEW.metric = 'cost'", context);
    render(result);
    const line = chart.innerHTML.match(/<path d="([^"]*)" class="us-line"/);
    assert.ok(line);
    assert.equal((line[1].match(/M/g) || []).length, 1, 'known Token activity remains continuous regardless of charge data');
    assert.ok(!chart.innerHTML.includes('NaN'));
    assert.equal('cost' in result.series[1], false);
    assert.equal(result.rangeTokens, 60);
    vm.runInContext("USAGE_VIEW.metric = 'tokens'", context);
  }],
  ['Token trend remains available when all charges are unknown', () => {
    const result = summarize({ days: 3, sources: [source('no-charge', { daily_usage: [{ date, total_tokens: 10 }] })] });
    vm.runInContext("USAGE_VIEW.metric = 'cost'", context);
    render(result);
    assert.ok(chart.innerHTML.includes('每日 Token 趋势'));
    assert.ok(chart.innerHTML.includes('<svg')); assert.ok(!chart.innerHTML.includes('花费'));
    vm.runInContext("USAGE_VIEW.metric = 'tokens'", context);
  }],
  ['summary never reads monetary fields and local fallback does not double-count upstream Tokens', () => {
    const activity = { date, total_tokens: 10, requests: 2, model: 'synthetic-model' };
    for (const key of ['cost', 'actual_cost', 'total_cost', 'actualCost']) Object.defineProperty(activity, key, { get() { throw Error('monetary data read'); } });
    const remote = source('upstream', { daily_usage: [activity], model_stats: [activity], today: activity });
    const local = { ...source('local', { daily_usage: [{ date, total_tokens: 50 }], model_stats: [{ model: 'synthetic-model', total_tokens: 50 }], today: { total_tokens: 50 } }), kind: 'local' };
    const result = summarize({ days: 1, sources: [remote, local] });
    assert.equal(result.rangeTokens, 10); assert.equal(result.today.tokens, 10); assert.equal(result.models[0].tokens, 10);
    const fallback = summarize({ days: 1, sources: [source('balance', { remaining: 5 }), local] });
    assert.equal(fallback.rangeTokens, 50); assert.equal(fallback.today.tokens, 50); assert.equal(fallback.models[0].tokens, 50);
    assert.equal(fallback.curveLocal, true); assert.equal(fallback.todayLocal, true); assert.equal(fallback.balance.value, 5);
  }],
  ['overview shows actual balances and Token activity while ignoring spend and estimated remaining amounts', async () => {
    const app = await readFile(new URL('../internal/console/web/app.js', import.meta.url), 'utf8');
    const start = app.indexOf('function usageCards('), end = app.indexOf('\nfunction toolCard(', start);
    const ctx = vm.createContext({ esc: String, ico: () => '', num: (v) => typeof v === 'number' && Number.isFinite(v) ? v : null, money: (v) => '$' + v, compact: (v) => v == null ? '—' : String(v) });
    vm.runInContext(app.slice(start, end), ctx);
    const html = ctx.usageCards({ remaining: 12.5, usage: { today: { total_tokens: 1234, requests: 2, actual_cost: 0.33 }, total: { total_tokens: 9876, requests: 8, actual_cost: 0.55 } }, quota: { used: 7, limit: 100 }, subscription: { daily_limit_usd: 10, daily_usage_usd: 2 }, rate_limits: [{ used: 3, limit: 4 }] });
    assert.match(html, /今日 Token|累计 Token/); assert.match(html, /1234/); assert.match(html, /9876/); assert.match(html, /\$12\.5/);
    assert.doesNotMatch(html, /消费|\$0\.33|\$0\.55|\$93|Key 总额度|订阅 ·/);
    assert.equal(ctx.usageCards({ quota: { used: 7, limit: 100 } }), '', 'remaining money is never inferred from usage and limit');
    assert.match(ctx.usageCards({ quota: { remaining: 0 } }), /\$0/);
  }],
  ['floating ball shows Token requests and actual balance, with missing activity left unknown', async () => {
    const ball = await readFile(new URL('../internal/console/web/ball.js', import.meta.url), 'utf8');
    // Include the production authenticated-fetch wrapper used by refreshUsage.
    const start = ball.indexOf('const BALL_AUTH = '), end = ball.indexOf('\ndocument.addEventListener(\'visibilitychange\'', start);
    assert.ok(start >= 0 && end > start, 'the complete ball usage boundary exists');
    const nodes = new Map(['#bName', '#bTok', '#bSub'].map(key => [key, { innerHTML: '', textContent: '' }]));
    let data = summarize({ days: 1, sources: [source('upstream', { remaining: 12.5, today: { total_tokens: 1234, requests: 2, actual_cost: .33 } })] });
    const ctx = vm.createContext({ Date, AbortController, setTimeout, clearTimeout, document: { visibilityState: 'visible', addEventListener() {} }, fetch: async () => ({ok:true,json:async()=>({accounts:[{id:'upstream',name:'日常开发'}],tools:[{id:'codex',available:true}],bindings:{codex:{accountId:'upstream',appliedId:'upstream',pending:false}}})}), STATE: {}, setBallError() {}, $: (key) => nodes.get(key), render() {}, USAGE: { load: async () => data, fmtTokens: vm.runInContext('USAGE.fmtTokens', context), fmtMoney: vm.runInContext('USAGE.fmtMoney', context) } });
    vm.runInContext(ball.slice(start, end), ctx); await ctx.refreshUsage(true);
    assert.equal(nodes.get('#bName').textContent, '日常开发'); assert.equal(nodes.get('#bTok').textContent, '1.2K'); assert.match(nodes.get('#bSub').innerHTML, /剩余 <em>\$12\.50<\/em>/); assert.doesNotMatch(nodes.get('#bSub').innerHTML, /0\.33/);
    data = summarize({ days: 1, sources: [source('unavailable', {})] }); await ctx.refreshUsage(true);
    assert.equal(nodes.get('#bTok').textContent, '—'); assert.equal(nodes.get('#bSub').innerHTML, '暂无 Token 记录');
  }],
];
let failures = 0;
for (const [name, test] of cases) {
  try { await test(); console.log(`PASS ${name}`); }
  catch (error) { failures++; console.error(`FAIL ${name}: ${error.message}`); }
}
if (failures) process.exitCode = 1;
