import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';
import { test } from 'node:test';

// Run the shipped renderer and aggregation code. Timers, host IPC and HTTP use
// synthetic observations only; no desktop profile or provider is opened.
const ballCode = await readFile(new URL('../internal/console/web/ball.js', import.meta.url), 'utf8');
const usageCode = await readFile(new URL('../internal/console/web/usage-data.js', import.meta.url), 'utf8');
const settle = async () => { for (let i = 0; i < 8; i++) await new Promise(resolve => setImmediate(resolve)); };
const deferred = () => { let resolve, reject; const promise = new Promise((a, b) => { resolve = a; reject = b; }); return { promise, resolve, reject }; };
const options = [
  { id: 'api-a', name: '开发 API', kind: 'api', host: 'same.example.test' },
  { id: 'api-b/?#&', name: '开发 API', kind: 'api', host: 'same.example.test' },
  { id: 'official-a', name: '工作账号 · Pro', kind: 'official' },
];
const response = (body, status = 200) => ({ ok: status < 400, status, json: async () => body });
function element() {
  const handlers = new Map(), classes = new Set(), captured = new Set();
  let html = '', text = '';
  return {
    dataset: {}, style: { setProperty() {} }, attrs: {}, value: '', disabled: false,
    get innerHTML() { return html; }, set innerHTML(value) { html = String(value); text = html.replace(/<[^>]*>/g, ''); },
    get textContent() { return text; }, set textContent(value) { text = String(value); html = text; },
    classList: { add: (...xs) => xs.forEach(x => classes.add(x)), remove: (...xs) => xs.forEach(x => classes.delete(x)), contains: x => classes.has(x), toggle(x, on) { const value = on ?? !classes.has(x); if (value) classes.add(x); else classes.delete(x); return value; } },
    addEventListener(name, callback) { if (!handlers.has(name)) handlers.set(name, []); handlers.get(name).push(callback); },
    async emit(name, value = {}) { for (const callback of handlers.get(name) || []) await callback({ target: this, ...value }); },
    setAttribute(name, value) { this.attrs[name] = value; },
    getBoundingClientRect: () => ({ left: 168, top: 73, width: 64, height: 64 }),
    setPointerCapture: id => captured.add(id), hasPointerCapture: id => captured.has(id), releasePointerCapture: id => captured.delete(id),
    closest() { return null; }, contains(value) { return value === this; }, focus() { this.focused = true; },
  };
}
async function harness(initial = {}) {
  const nodes = new Map(['#float', '#ball', '#face', '#bubble', '#badge', '#sats', '#bName', '#bTok', '#bSub'].map(key => [key, element()]));
  nodes.get('#sats').inert = true;
  const globalEvents = element(), documentEvents = element(), calls = [], requests = [], timers = new Map();
  const sats = ['', 'usage', 'overview', 'accounts', 'login', 'settings'].map(route => { const el = element(); el.dataset.go = route; el.closest = selector => selector === '.sats button' ? el : null; return el; });
  const ball = nodes.get('#ball'), bubble = nodes.get('#bubble');
  ball.closest = selector => selector === '.ball' ? ball : null;
  bubble.closest = selector => selector === '.bubble' ? bubble : null;
  let nextTimer = 1, shown, dragFinished, eventSource, point = ball;
  const state = { local: { accounts: options.map(row => ({ ...row })), tools: [{ id: 'codex', available: true }], bindings: { codex: { accountId: 'api-a', appliedId: 'api-a', pending: false } } }, activity: { revision: 0, sessions: [], approvalIds: [] }, date: '2026-10-14', counts: { 'api-a': 12000, 'api-b/?#&': 89000, 'official-a': 42000 }, override: null, ...initial };
  const payload = source => {
    const selected = options.find(x => x.id === source), count = state.counts[source] ?? 101000;
    const activity = { usage: { today: { total_tokens: count, requests: 4 } }, daily_usage: [{ date: state.date, total_tokens: count, requests: 4 }] };
    const sourceData = selected?.kind === 'official' ? { ...selected, ok: true, usage: {}, localUsage: { ...activity, remaining: 999, partial: true } } : { ...(selected || options[0]), ok: true, usage: { ...activity, remaining: 12.5 } };
    return { days: 1, selectedSource: source, options, rangeEnd: state.date, sources: [sourceData] };
  };
  const host = Object.fromEntries(['setInteractive', 'dragStart', 'drag', 'dragEnd', 'dragCancel', 'show'].map(name => [name, (...args) => calls.push([name, ...args])]));
  host.onShown = fn => { shown = fn; };
  host.onDragFinished = fn => { dragFinished = fn; };
  const document = {
    visibilityState: 'visible', querySelector: selector => nodes.get(selector), querySelectorAll: selector => selector === '#sats button' ? sats : [],
    get hidden() { return this.visibilityState === 'hidden'; },
    addEventListener: documentEvents.addEventListener, elementFromPoint: () => point,
  };
  const ctx = vm.createContext({
    Date, console, AbortController, window: { salcaraBall: host, screenX: 0 }, document,
    screen: { availLeft: 0, availWidth: 1920 }, innerWidth: 400,
    addEventListener: globalEvents.addEventListener, requestAnimationFrame() {}, matchMedia: () => ({ matches: true }),
    setTimeout(fn, ms) { const id = nextTimer++; timers.set(id, { fn, ms }); return id; }, clearTimeout: id => timers.delete(id), setInterval() {},
    EventSource: class {
      static CLOSED = 2;
      constructor() { eventSource = this; this.readyState = 1; }
      addEventListener(name, fn) { this[name] = fn; }
      close() { this.readyState = 2; }
    },
    fetch: async (url, init = {}) => {
      const parsed = new URL(url, 'http://127.0.0.1'); requests.push({ url: parsed, signal: init.signal });
      if (parsed.pathname === '/api/activity') return state.activityOverride ? state.activityOverride(init) : response(state.activity);
      if (parsed.pathname === '/api/local/accounts') return state.localOverride ? state.localOverride(init) : response(state.local);
      if (parsed.pathname === '/api/usage/overview') return state.override ? state.override(parsed, init) : response(payload(parsed.searchParams.get('source') || ''));
      throw Error(`unexpected endpoint ${url}`);
    },
  });
  vm.runInContext(usageCode, ctx); vm.runInContext(ballCode, ctx); await settle();
  return {
    ctx, state, nodes, calls, requests, timers, ball, bubble, sats, payload, document,
    eval: code => vm.runInContext(code, ctx), refresh: () => ctx.refreshUsage(true), shown: value => shown(value), dragFinished: value => dragFinished(value),
    activityEvent: value => eventSource.event({ data: JSON.stringify(value) }), streamOpen: () => eventSource.onopen(), streamError: () => eventSource.onerror(),
    emit: (name, value) => globalEvents.emit(name, value), point: value => { point = value; },
    follow: () => { vm.runInContext('BALL_USAGE.sourceAt = 0', ctx); return ctx.refreshUsage(false); },
    activate: source => { state.local.bindings = { codex: { accountId: source, appliedId: source, pending: false } }; vm.runInContext('BALL_USAGE.sourceAt = 0', ctx); return ctx.refreshUsage(false); },
    visibility: async value => { document.visibilityState = value; await documentEvents.emit('visibilitychange'); await settle(); },
    timer: async ms => { const entry = [...timers].find(([, value]) => value.ms === ms); assert.ok(entry, `timer ${ms}`); timers.delete(entry[0]); await entry[1].fn(); await settle(); },
  };
}
const pointer = (pointerId = 1, x = 200, y = 105) => ({ pointerId, button: 0, screenX: x, screenY: y, clientX: x, clientY: y });

test('click toggles quick entries once and every shortcut sends its exact route', async () => {
  const h = await harness();
  await h.ball.emit('pointerdown', pointer()); await h.ball.emit('pointerup', pointer()); await h.ball.emit('click', { detail: 1 });
  assert.equal(h.ball.attrs['aria-expanded'], 'true'); assert.equal(h.eval('STATE.open'), true);
  for (const button of h.sats) { await button.emit('click'); assert.deepEqual(h.calls.at(-1), ['show', button.dataset.go]); }
  assert.equal(h.eval('STATE.open'), false); assert.ok(h.calls.some(x => x[0] === 'setInteractive' && x[1]));
});

test('movement threshold and pointer ownership separate a drag from a click', async () => {
  const h = await harness();
  await h.ball.emit('pointerdown', { ...pointer(), button: 2 }); assert.equal(h.eval('drag.active'), false);
  await h.ball.emit('pointerdown', pointer()); await h.ball.emit('pointerdown', pointer(2));
  await h.ball.emit('pointermove', pointer(2, 500)); await h.ball.emit('pointerup', pointer(2));
  await h.ball.emit('pointercancel', pointer(2)); await h.ball.emit('lostpointercapture', pointer(2));
  assert.equal(h.eval('drag.active'), true); assert.equal(h.eval('drag.pointerId'), 1);
  await h.ball.emit('pointermove', pointer(1, 203)); assert.equal(h.eval('drag.moved'), false);
  await h.ball.emit('pointermove', pointer(1, 220)); await h.ball.emit('pointerup', pointer(1, 220)); await h.ball.emit('click', { detail: 1 });
  assert.equal(h.eval('STATE.open'), false); assert.equal(h.eval('drag.active'), false);
  assert.equal(h.calls.filter(x => x[0] === 'dragStart').length, 1); assert.equal(h.calls.filter(x => x[0] === 'drag').length, 1);
  assert.equal(h.calls.filter(x => x[0] === 'dragEnd').length, 1); assert.equal(h.ball.hasPointerCapture(1), false);
});

test('cancel, lost capture and blur release drag without opening quick entries', async () => {
  for (const reason of ['pointercancel', 'lostpointercapture', 'blur']) {
    const h = await harness(); await h.ball.emit('pointerdown', pointer());
    if (reason === 'blur') await h.emit(reason); else await h.ball.emit(reason, pointer());
    await h.ball.emit('pointerup', pointer());
    assert.equal(h.eval('drag.active'), false, reason); assert.equal(h.eval('STATE.open'), false, reason);
    assert.equal(h.ball.hasPointerCapture(1), false, reason); assert.equal(h.calls.filter(x => x[0] === 'dragCancel').length, 1, reason);
  }
});

test('keyboard activation, Escape and shown payload maintain native interaction state', async () => {
  const h = await harness(); await h.ball.emit('click', { detail: 0 }); assert.equal(h.eval('STATE.open'), true);
  await h.ball.emit('keydown', { key: 'Escape' }); assert.equal(h.eval('STATE.open'), false);
  await h.ball.emit('mouseenter'); h.shown(true); await settle();
  assert.deepEqual(h.calls.filter(x => x[0] === 'setInteractive').at(-1), ['setInteractive', true]);
  assert.equal(h.eval('STATE.hover'), false); assert.equal(h.nodes.get('#float').classList.contains('peek'), false);
  assert.equal([...h.timers.values()].filter(t => t.ms === 180).length, 0);
  h.shown(false); await settle(); assert.deepEqual(h.calls.filter(x => x[0] === 'setInteractive').at(-1), ['setInteractive', false]);
});

test('hover bubble remains visible while reading and cannot reappear during a drag', async () => {
  const h = await harness(); await h.ball.emit('mouseenter'); await h.timer(180);
  assert.equal(h.nodes.get('#float').classList.contains('peek'), true);
  await h.ball.emit('mouseleave'); await h.bubble.emit('mouseenter');
  assert.equal([...h.timers.values()].filter(t => t.ms === 150).length, 0);
  h.point(h.bubble); await h.emit('mousemove', { clientX: 280, clientY: 105 });
  assert.equal(h.eval('interactive'), true);
  await h.bubble.emit('mouseleave'); await h.timer(150); assert.equal(h.nodes.get('#float').classList.contains('peek'), false);
  await h.ball.emit('mouseenter'); await h.ball.emit('pointerdown', pointer()); await h.ball.emit('pointermove', pointer(1, 230));
  assert.equal([...h.timers.values()].filter(t => t.ms === 180).length, 0, 'drag cannot reveal a delayed bubble');
});

test('the ball has a plain source name and closed shortcuts are absent from keyboard navigation', async () => {
  const html = await readFile(new URL('../internal/console/web/ball.html', import.meta.url), 'utf8');
  assert.doesNotMatch(html, /<select|bSource/);
  assert.match(html, /<nav[^>]+id="sats"[^>]+inert/);
  const h = await harness(); assert.equal(h.nodes.get('#sats').inert, true);
  await h.ball.emit('click', { detail: 0 }); assert.equal(h.nodes.get('#sats').inert, false);
  await h.ball.emit('keydown', { key: 'Escape' }); assert.equal(h.nodes.get('#sats').inert, true);
});

test('automatic current account uses exact inflight or applied identity, never pending selection', async () => {
  const local = { accounts: options.map(x => ({ ...x })), tools: [{id:'codex',available:true}], bindings: { codex: { pending: false, accountId: 'official-a', appliedId: 'official-a' } } };
  const h = await harness({ local });
  assert.equal(h.eval('BALL_USAGE.source'), 'official-a'); assert.equal(h.nodes.get('#bTok').textContent, '42.0K');
  assert.equal(h.nodes.get('#bName').textContent, '工作账号 · Pro'); assert.match(h.nodes.get('#bSub').textContent, /部分记录/);
  assert.doesNotMatch(h.nodes.get('#bSub').textContent, /剩余|999/, 'official local metadata cannot manufacture money');
  local.tools[0].available = false; await h.refresh(); assert.equal(h.eval('BALL_USAGE.source'), '');
  local.tools[0].available = true;
  local.bindings.codex.pending = true; await h.refresh(); assert.equal(h.eval('BALL_USAGE.source'), '');
  assert.equal(h.nodes.get('#bTok').textContent, '—');
  assert.ok(h.requests.filter(x => x.url.pathname === '/api/usage/overview').every(x => x.url.searchParams.get('source')), 'no global usage fallback');
  local.accounts[1].poolState = { inflight: 1 }; await h.refresh(); assert.equal(h.eval('BALL_USAGE.source'), 'api-b/?#&');
});

test('automatic same-name API and official changes stay distinct and omit unrelated balances', async () => {
  const h = await harness(); await h.activate('api-b/?#&');
  assert.equal(h.requests.at(-1).url.searchParams.get('source'), 'api-b/?#&'); assert.equal(h.nodes.get('#bTok').textContent, '89.0K');
  assert.equal(h.nodes.get('#bName').textContent, '开发 API');
  assert.match(h.nodes.get('#bSub').innerHTML, /\$12\.50/);
  await h.activate('official-a'); assert.equal(h.nodes.get('#bTok').textContent, '42.0K');
  assert.doesNotMatch(h.nodes.get('#bSub').textContent, /\$/);
});

test('late success and failure from a superseded source never replace the chosen account', async () => {
  for (const failure of [false, true]) {
    const h = await harness(), old = deferred();
    h.state.override = parsed => parsed.searchParams.get('source') === 'api-a' ? old.promise : response(h.payload(parsed.searchParams.get('source')));
    const first = h.refresh(); await settle(); assert.equal(h.nodes.get('#bTok').textContent, '—');
    const oldSignal = h.requests.at(-1).signal;
    await h.activate('official-a'); assert.equal(oldSignal.aborted, true); assert.equal(h.nodes.get('#bTok').textContent, '42.0K');
    if (failure) old.reject(Error('obsolete failure')); else old.resolve(response(h.payload('api-a')));
    await first; assert.equal(h.nodes.get('#bTok').textContent, '42.0K'); assert.equal(h.eval('BALL_USAGE.source'), 'official-a');
    assert.equal(h.eval('STATE.error'), false);
  }
});

test('failure clears old Token values and deleting the active account does not show global totals', async () => {
  const h = await harness();
  h.state.override = () => response({ error: '来源已删除', options: options.filter(x => x.id !== 'api-a') }, 409);
  await h.refresh(); assert.equal(h.nodes.get('#bTok').textContent, '—'); assert.equal(h.nodes.get('#bSub').textContent, '来源已删除');
  assert.equal(h.eval('BALL_USAGE.source'), 'api-a'); assert.equal(h.nodes.get('#bName').textContent, '开发 API');
  h.state.local.accounts = h.state.local.accounts.filter(row => row.id !== 'api-a');
  const before = h.requests.filter(x => x.url.pathname === '/api/usage/overview').length;
  await h.follow(); assert.equal(h.eval('BALL_USAGE.source'), ''); assert.equal(h.nodes.get('#bSub').textContent, '暂无使用中的账号');
  assert.equal(h.requests.filter(x => x.url.pathname === '/api/usage/overview').length, before);
});

test('source mismatch and timeout cannot leave a stale number on the ball', async () => {
  const h = await harness();
  h.state.override = () => response(h.payload('official-a')); await h.refresh();
  assert.equal(h.nodes.get('#bTok').textContent, '—'); assert.match(h.nodes.get('#bSub').textContent, /来源与当前选择不一致/);
  h.state.override = (_, init) => new Promise((resolve, reject) => init.signal.addEventListener('abort', () => reject(Error('aborted'))));
  const pending = h.refresh(); await settle(); await h.timer(30000); await pending;
  assert.equal(h.nodes.get('#bSub').textContent, '用量读取超时'); assert.equal(h.eval('BALL_USAGE.controller'), null);
});

test('account changes and day rollover do not become false Token-spending signals', async () => {
  const h = await harness(); await h.activate('api-b/?#&');
  assert.equal(h.eval('STATE.burnUntil'), 0);
  h.state.counts['api-b/?#&'] += 3000; await h.refresh(); assert.ok(h.eval('STATE.burnUntil') > Date.now());
  h.eval('STATE.burnUntil = 0'); h.state.date = '2026-10-15'; h.state.counts['api-b/?#&'] += 10000; await h.refresh();
  assert.equal(h.eval('STATE.burnUntil'), 0);
});

test('polling shares the active request and pauses while the floating window is hidden', async () => {
  const h = await harness(), pending = deferred();
  h.state.override = () => pending.promise; const refresh = h.refresh(); await settle(); const count = h.requests.length;
  await h.ctx.watchSpend(); assert.equal(h.requests.length, count, 'no duplicate query during pending refresh');
  pending.resolve(response(h.payload('api-a'))); await refresh;
  h.document.visibilityState = 'hidden'; await h.ctx.watchSpend(); assert.equal(h.requests.length, count);
});

test('provider currency strings are text and explicit unknown Tokens remain unknown', async () => {
  const h = await harness();
  h.state.override = parsed => {
    const body = h.payload(parsed.searchParams.get('source'));
    body.sources[0].usage.unit = '<img src=x onerror=bad()>';
    body.sources[0].usage.usage.today.total_tokens = null;
    return response(body);
  };
  await h.refresh(); assert.equal(h.nodes.get('#bTok').textContent, '—');
  assert.doesNotMatch(h.nodes.get('#bSub').innerHTML, /<img/); assert.match(h.nodes.get('#bSub').innerHTML, /&lt;IMG/);
});

test('finished pool requests and later applied tool changes follow the most recently used account', async () => {
  const h = await harness(), [api, backup, official] = h.state.local.accounts;
  api.lastUsedAt = Date.parse('2026-10-14T01:00:00Z');
  official.poolState = { inflight: 0, lastUsedAt: '2026-10-14T01:01:00Z' };
  await h.follow(); assert.equal(h.eval('BALL_USAGE.source'), official.id);
  backup.lastUsedAt = Date.parse('2026-10-14T01:02:00Z');
  await h.activate(backup.id); assert.equal(h.eval('BALL_USAGE.source'), backup.id);
  official.poolState = { inflight: 1, lastUsedAt: '2026-10-14T01:03:00Z' };
  api.poolState = { inflight: 1, lastUsedAt: '2026-10-14T01:04:00Z' };
  await h.follow(); assert.equal(h.eval('BALL_USAGE.source'), api.id);
  official.poolState.lastUsedAt = '2026-10-14T01:05:00Z';
  await h.follow(); assert.equal(h.eval('BALL_USAGE.source'), official.id);
});

test('identity changes reset the consumption baseline without replacing the existing animation', async () => {
  const h = await harness(); h.eval('STATE.fire.lit = true; STATE.fire.litAt = Date.now(); STATE.fire.activeSince = Date.now(); STATE.burnUntil = Date.now() + 40000');
  h.state.local.accounts[0].sourceRevision = 'replacement-identity'; h.state.counts['api-a'] = 900000;
  await h.follow(); assert.equal(h.nodes.get('#bTok').textContent, '900.0K');
  assert.equal(h.eval('STATE.burnUntil'), 0); assert.equal(h.eval('STATE.fire.lit'), true, 'minimum lit time is preserved');
  h.eval('STATE.fire.lit = false; STATE.fire.activeSince = Date.now() - 2900; STATE.running.add("live-session")');
  assert.equal(h.ctx.burning(), false, 'existing three-second ignition threshold');
  h.eval('STATE.fire.activeSince = Date.now() - 3100'); assert.equal(h.ctx.burning(), true);
  h.eval('STATE.running.clear(); STATE.fire.idleSince = Date.now() - 9000; STATE.fire.litAt = Date.now() - 9000');
  assert.equal(h.ctx.burning(), false, 'existing eight-second quiet and minimum lit duration');
});

test('recovering upstream usage from local fallback does not turn a change of accounting scope into consumption', async () => {
  const h = await harness();
  h.state.override = parsed => {
    const body = h.payload(parsed.searchParams.get('source'));
    body.sources[0].localUsage = body.sources[0].usage; body.sources[0].usage = {};
    return response(body);
  };
  await h.refresh(); assert.equal(h.eval('STATE.burnUntil'), 0);
  h.state.override = null; h.state.counts['api-a'] = 990000;
  await h.refresh(); assert.equal(h.eval('STATE.burnUntil'), 0);
  h.state.counts['api-a'] += 3000; await h.refresh(); assert.ok(h.eval('STATE.burnUntil') > Date.now());
});

test('unchanged-source polling keeps the rendered number and does not refetch upstream usage', async () => {
  const h = await harness(); const before = h.requests.filter(x => x.url.pathname === '/api/usage/overview').length;
  const pending = deferred(); h.state.localOverride = () => pending.promise;
  const scan = h.follow(); await settle(); assert.equal(h.nodes.get('#bTok').textContent, '12.0K');
  await h.ctx.refreshUsage(false); assert.equal(h.requests.filter(x => x.url.pathname === '/api/local/accounts').length, 2);
  pending.resolve(response(h.state.local)); await scan;
  assert.equal(h.requests.filter(x => x.url.pathname === '/api/usage/overview').length, before);
});

test('late source scans and hidden-window requests cannot restore an obsolete account', async () => {
  const h = await harness(), old = deferred(); h.state.localOverride = () => old.promise;
  const scan = h.follow(); await settle(); const signal = h.requests.at(-1).signal;
  await h.visibility('hidden'); assert.equal(signal.aborted, true);
  const before = h.requests.length; h.ctx.watchSpend(); assert.equal(h.requests.length, before);
  h.state.localOverride = null; h.state.local.bindings = { codex: { pending: false, accountId: 'official-a', appliedId: 'official-a' } };
  await h.visibility('visible'); assert.equal(h.nodes.get('#bTok').textContent, '42.0K');
  old.resolve(response({ accounts: options, bindings: { codex: { pending: false, accountId: 'api-a', appliedId: 'api-a' } } }));
  await scan; assert.equal(h.eval('BALL_USAGE.source'), 'official-a'); assert.equal(h.nodes.get('#bTok').textContent, '42.0K');
});

test('local-source read failures clear the source and prevent a global usage request', async () => {
  const h = await harness(), before = h.requests.filter(x => x.url.pathname === '/api/usage/overview').length;
  h.state.localOverride = () => response({ error: 'unavailable' }, 503); await h.follow();
  assert.equal(h.eval('BALL_USAGE.source'), ''); assert.equal(h.nodes.get('#bTok').textContent, '—');
  assert.equal(h.nodes.get('#bName').textContent, '用量'); assert.equal(h.nodes.get('#bSub').textContent, '当前来源读取失败');
  assert.equal(h.requests.filter(x => x.url.pathname === '/api/usage/overview').length, before);
});

test('activity snapshot restores tasks already running before the ball opened without scanning sessions', async () => {
  const h = await harness({ activity: { revision: 7, sessions: [{ sessionKey: 'codex:active', status: 'running' }], approvalIds: ['question', 'question'] } });
  assert.deepEqual([...h.eval('STATE.running')], ['codex:active']);
  assert.equal(h.eval('STATE.approvals'), 1); assert.equal(h.ball.dataset.mood, 'alert');
  assert.ok(h.requests.every(r => r.url.pathname !== '/api/sessions'));
  h.state.activity = { revision: 8, sessions: [], approvalIds: [] };
  h.streamError(); h.streamOpen(); await settle();
  assert.equal(h.eval('STATE.running.size'), 0); assert.equal(h.eval('STATE.approvals'), 0);
  assert.equal(h.eval('STATE.error'), false);
});

test('late activity snapshots cannot resurrect completed tasks or approvals resolved in flight', async () => {
  const h = await harness(), pending = deferred();
  h.state.activityOverride = () => pending.promise;
  const reading = h.ctx.refreshActivity(); await settle();
  h.activityEvent({ type: 'session.updated', session: { sessionKey: 'codex:one', status: 'idle' }, activityRevision: 3 });
  h.activityEvent({ type: 'approval.resolved', approvalId: 'old-question', activityRevision: 4 });
  h.activityEvent({ type: 'approval.request', approvalId: 'new-question', activityRevision: 5 });
  pending.resolve(response({ revision: 2, sessions: [{ sessionKey: 'codex:one', status: 'running' }], approvalIds: ['old-question'] }));
  await reading;
  assert.equal(h.eval('STATE.running.size'), 0);
  assert.deepEqual([...h.eval('BALL_ACTIVITY.approvals')], ['new-question']);
  h.activityEvent({ type: 'approval.request', approvalId: 'old-question', activityRevision: 2 });
  assert.equal(h.eval('STATE.approvals'), 1, 'events older than the applied state are ignored');
});

test('newer activity snapshots win over older events delivered while reading', async () => {
  const h = await harness(), pending = deferred(); h.state.activityOverride = () => pending.promise;
  const reading = h.ctx.refreshActivity(); await settle();
  h.activityEvent({ type: 'session.updated', session: { sessionKey: 'claude:one', status: 'running' }, activityRevision: 1 });
  h.activityEvent({ type: 'approval.request', approvalId: 'obsolete', activityRevision: 2 });
  pending.resolve(response({ revision: 4, sessions: [], approvalIds: [] })); await reading;
  assert.equal(h.eval('STATE.running.size'), 0); assert.equal(h.eval('STATE.approvals'), 0);
  h.activityEvent({ type: 'session.updated', session: { sessionKey: 'claude:one', status: 'running' }, activityRevision: 3 });
  assert.equal(h.eval('STATE.running.size'), 0);
});

test('hidden activity reads are cancelled and late success or failure cannot replace restored state', async () => {
  for (const fails of [false, true]) {
    const h = await harness(), pending = deferred(); h.state.activityOverride = () => pending.promise;
    const reading = h.ctx.refreshActivity(); await settle();
    const signal = h.requests.findLast(r => r.url.pathname === '/api/activity').signal;
    await h.visibility('hidden'); assert.equal(signal.aborted, true);
    const before = h.requests.length; await h.ctx.refreshActivity(); assert.equal(h.requests.length, before);
    h.state.activityOverride = null; h.state.activity = { revision: 2, sessions: [], approvalIds: ['current'] };
    await h.visibility('visible');
    if (fails) pending.reject(Error('late failure')); else pending.resolve(response({ revision: 1, sessions: [], approvalIds: ['old'] }));
    await reading;
    assert.deepEqual([...h.eval('BALL_ACTIVITY.approvals')], ['current']); assert.equal(h.eval('STATE.error'), false);
  }
});

test('activity timeouts retain known approvals and a healthy stream cannot erase usage errors', async () => {
  const h = await harness({ activity: { revision: 1, sessions: [], approvalIds: ['pending'] } });
  h.state.activityOverride = init => new Promise((resolve, reject) => init.signal.addEventListener('abort', () => reject(Error('timeout'))));
  const reading = h.ctx.refreshActivity(); await settle(); await h.timer(10000); await reading;
  assert.equal(h.eval('STATE.approvals'), 1); assert.equal(h.eval('STATE.error'), false);
  await h.timer(8000);
  assert.equal(h.eval('STATE.approvals'), 1); assert.equal(h.eval('STATE.error'), true);
  h.state.activityOverride = null; await h.ctx.refreshActivity(); assert.equal(h.eval('STATE.error'), false);
  h.state.override = () => response({ error: '用量接口暂时不可用' }, 503); await h.refresh();
  assert.equal(h.eval('STATE.error'), false); await h.timer(8000);
  h.streamOpen(); await settle(); assert.equal(h.eval('STATE.error'), true);
  assert.equal(h.nodes.get('#bSub').textContent, '用量接口暂时不可用');
  h.state.override = null; await h.refresh(); assert.equal(h.eval('STATE.error'), false);
});

test('native post-drag hit testing, hidden capture and satellite Escape release interaction correctly', async () => {
  const h = await harness(); await h.ball.emit('mouseenter');
  h.dragFinished(false); assert.equal(h.eval('interactive'), false);
  assert.deepEqual(h.calls.at(-1), ['setInteractive', false]);
  await h.ball.emit('pointerdown', pointer()); await h.visibility('hidden');
  assert.equal(h.eval('drag.active'), false); assert.equal(h.ball.hasPointerCapture(1), false);
  assert.equal(h.eval('interactive'), false);
  await h.visibility('visible'); await h.ball.emit('click', { detail: 0 });
  let prevented = false;
  await h.nodes.get('#sats').emit('keydown', { key: 'Escape', preventDefault() { prevented = true; } });
  assert.equal(h.eval('STATE.open'), false); assert.equal(h.ball.focused, true); assert.equal(prevented, true);
});

test('malformed stream payloads are ignored and streaming observations stay bounded', async () => {
  const h = await harness(); h.activityEvent(null); h.activityEvent('not-an-event');
  for (let i = 0; i < 50; i++) h.activityEvent({ type: 'message' });
  assert.equal(h.eval('STATE.streamAt.length'), 3); assert.equal(h.ctx.spending(), true);
});

test('a restarted Bridge can replace a higher prior revision without late old snapshots returning', async () => {
  const h = await harness({ activity: { revision: 90, sessions: [{ sessionKey: 'codex:old', status: 'running' }], approvalIds: ['old'] } });
  const pending = deferred(); h.state.activityOverride = () => pending.promise;
  const oldRead = h.ctx.refreshActivity(); await settle();
  h.streamError(); h.state.activityOverride = null;
  h.state.activity = { revision: 0, sessions: [], approvalIds: [] };
  h.streamOpen(); await settle();
  assert.equal(h.eval('STATE.running.size'), 0); assert.equal(h.eval('STATE.approvals'), 0);
  h.activityEvent({ type: 'session.updated', session: { sessionKey: 'claude:new', status: 'running' }, activityRevision: 1 });
  pending.resolve(response({ revision: 91, sessions: [{ sessionKey: 'codex:old', status: 'running' }], approvalIds: ['old'] }));
  await oldRead;
  assert.deepEqual([...h.eval('STATE.running')], ['claude:new']);
  assert.equal(h.eval('BALL_ACTIVITY.revision'), 1); assert.equal(h.eval('STATE.error'), false);
});
