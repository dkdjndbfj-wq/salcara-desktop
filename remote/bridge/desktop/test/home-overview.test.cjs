'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const { setup, deferred, web, appSource } = require('./helpers/workbench.cjs');
const shell = fs.readFileSync(path.join(web, 'shell.js'), 'utf8');

function fixture(onSessions = async () => ({ sessions: [] })) {
  const f = setup(), requests = [], events = new Map();
  f.context.AbortController = AbortController;
  for (const id of ['homeTitle', 'homeRecent', 'homeRecentList', 'homePendingCount', 'homePendingList', 'hsAgents', 'hsKeys', 'hsRemote', 'hsTokens']) {
    f.document.elements.set(id, { innerHTML: '', textContent: '', hidden: false, removeAttribute() {} });
  }
  Object.assign(f.S, { route: 'home', routeRevision: 1, approvalsLoadState: 'ready', sessions: [], approvals: [] });
  f.context.EventSource = class { addEventListener(type, handler) { events.set(type, handler); } };
  const previous = f.context.api;
  f.context.api = async (url, body) => {
    requests.push(url);
    return url === '/api/sessions' ? onSessions() : previous(url, body);
  };
  vm.runInContext(shell.slice(shell.indexOf('const HOME =')), f.context, { filename: 'shell-home.js' });
  f.context.connectStream();
  return Object.assign(f, {
    requests,
    html: id => f.document.getElementById(id).innerHTML,
    emitSession: session => events.get('event')({ data: JSON.stringify({ type: 'session.updated', session }) }),
    render: () => f.context.renderHome(),
  });
}

const session = (key, overrides = {}) => ({ sessionKey: key, tool: key.split(':')[0], title: key, cwd: '', status: 'idle', updatedAt: 10, ...overrides });

test('home merges a pushed update and a new conversation into an older session snapshot', async () => {
  const response = deferred(), f = fixture(() => response.promise);
  const read = f.render();
  f.emitSession(session('codex:existing', { title: 'New title', status: 'running', updatedAt: 20 }));
  f.emitSession(session('claude:new', { title: 'New conversation', updatedAt: 30 }));
  response.resolve({ sessions: [session('codex:existing', { title: 'Old title' })] });
  await read;

  const html = f.html('homeRecentList');
  assert.match(html, /New title/);
  assert.match(html, /New conversation/);
  assert.match(html, /进行中/);
  assert.doesNotMatch(html, /Old title/);
  assert.ok(html.indexOf('New conversation') < html.indexOf('New title'));
});

test('home ignores an old route response without clearing the current read or its pushed changes', async () => {
  const old = deferred(), current = deferred();
  let reads = 0;
  const f = fixture(() => (++reads === 1 ? old.promise : current.promise));
  const first = f.render();
  f.S.route = 'settings'; f.S.routeRevision++;
  f.S.route = 'home'; f.S.routeRevision++;
  const second = f.render();
  f.emitSession(session('claude:live', { title: 'Current pushed title', updatedAt: 50 }));
  old.resolve({ sessions: [session('codex:old', { title: 'Old route title' })] });
  await first;
  assert.doesNotMatch(f.html('homeRecentList'), /Old route title/);
  current.resolve({ sessions: [session('codex:current', { title: 'Current snapshot title' })] });
  await second;

  assert.match(f.html('homeRecentList'), /Current pushed title/);
  assert.match(f.html('homeRecentList'), /Current snapshot title/);
  assert.doesNotMatch(f.html('homeRecentList'), /Old route title/);
});

test('a filtered sessions page does not restrict the home snapshot or its live updates', async () => {
  const f = fixture(async () => ({ sessions: [session('codex:one'), session('claude:two')] }));
  f.S.sessFilter = 'claude';
  f.S.sessions = [session('claude:filtered')];
  await f.render();
  f.emitSession(session('codex:three', { title: 'Codex push despite filter', updatedAt: 40 }));

  assert.deepEqual(f.requests, ['/api/sessions']);
  assert.match(f.html('homeRecentList'), /codex:one/);
  assert.match(f.html('homeRecentList'), /claude:two/);
  assert.match(f.html('homeRecentList'), /Codex push despite filter/);
  assert.equal(f.S.sessFilter, 'claude');
  assert.equal(f.S.sessions.length, 1, 'the overview does not replace the filtered page cache');
});

test('home preserves failed, unknown and waiting states before approval details arrive', async () => {
  const f = fixture(async () => ({ sessions: [
    session('codex:failed', { status: 'failed' }),
    session('claude:unknown', { status: 'unknown' }),
    session('codex:waiting', { status: 'waiting_approval' }),
  ] }));
  await f.render();
  assert.match(f.html('homeRecentList'), />失败</);
  assert.match(f.html('homeRecentList'), />状态未知</);
  assert.match(f.html('homeRecentList'), />等待处理</);
});

test('failed session reads are explicit and a later successful read restores the recent list', async () => {
  let failed = true;
  const f = fixture(async () => { if (failed) throw new Error('offline'); return { sessions: [session('codex:restored')] }; });
  await f.render();
  assert.match(f.html('homeRecentList'), /暂时无法读取最近对话/);
  assert.doesNotMatch(f.html('homeRecentList'), /还没有最近的对话/);
  failed = false;
  await f.render();
  assert.match(f.html('homeRecentList'), /codex:restored/);
  assert.doesNotMatch(f.html('homeRecentList'), /暂时无法读取/);
});

test('pending items distinguish loading and failure from a confirmed empty list', () => {
  const f = fixture();
  for (const state of ['loading', 'error']) {
    f.S.approvalsLoadState = state;
    f.context.homePending();
    assert.doesNotMatch(f.html('homePendingList'), /暂无待处理事项/);
    assert.match(f.html('homePendingList'), state === 'loading' ? /正在同步/ : /暂时无法同步/);
  }
  f.S.approvalsLoadState = 'ready';
  f.context.homePending();
  assert.match(f.html('homePendingList'), /暂无待处理事项/);
});

test('known questions remain visible when synchronization fails and open the exact tool session', () => {
  const f = fixture();
  f.S.approvalsLoadState = 'error';
  f.S.approvals = [{ approvalId: 'question-1', sessionKey: 'hermes:work', tool: 'hermes', kind: 'question', title: 'Choose next step' }];
  f.context.homePending();
  assert.match(f.html('homePendingList'), /Choose next step/);
  assert.match(f.html('homePendingList'), /等你回答/);
  assert.match(f.html('homePendingList'), /暂时无法同步/);
  assert.match(f.html('homePendingList'), /data-k="hermes:work" data-tool="hermes"/);
  f.context.RM = { tool: 'codex', newThread: true };
  f.S.openInfo = { sessionKey: 'old' };
  f.ACTIONS.homeOpenSession({ dataset: { k: 'hermes:work', tool: 'hermes' } });
  assert.equal(f.S.openKey, 'hermes:work');
  assert.equal(f.S.sessFilter, 'hermes');
  assert.equal(f.context.RM.tool, 'hermes');
  assert.equal(f.context.RM.newThread, false);
  assert.equal(f.S.openInfo, null);
  assert.equal(f.context.location.hash, 'sessions');
});

test('cold home startup retains events received while the account library is still loading', async () => {
  const accounts = deferred(), response = deferred();
  const f = fixture(() => response.promise), previous = f.context.api;
  f.S.local = null;
  f.context.api = (url, body) => url === '/api/local/accounts' ? accounts.promise : previous(url, body);
  const read = f.render();
  f.emitSession(session('codex:cold', { title: 'Fresh during account read', updatedAt: 30 }));
  accounts.resolve(f.backend);
  await Promise.resolve();
  response.resolve({ sessions: [session('codex:cold', { title: 'Old startup title', updatedAt: 10 })] });
  await read;
  assert.match(f.html('homeRecentList'), /Fresh during account read/);
  assert.doesNotMatch(f.html('homeRecentList'), /Old startup title/);
});

test('an older pushed session cannot erase a newer update buffered during a snapshot read', async () => {
  const response = deferred(), f = fixture(() => response.promise);
  const read = f.render();
  f.emitSession(session('codex:ordered', { title: 'Latest event', updatedAt: 30 }));
  f.emitSession(session('codex:ordered', { title: 'Older event', updatedAt: 20 }));
  response.resolve({ sessions: [session('codex:ordered', { title: 'Old snapshot', updatedAt: 10 })] });
  await read;
  assert.match(f.html('homeRecentList'), /Latest event/);
  assert.doesNotMatch(f.html('homeRecentList'), /Older event|Old snapshot/);
});

test('cold history renders while account discovery is still pending and remote state is invalid', async () => {
  const accounts = deferred(), f = fixture(async () => ({ sessions: [session('claude:local')] }));
  f.S.local = null; f.S.state = { hub: { state: 'invalid_key' } };
  const previous = f.context.api;
  f.context.api = (url, ...args) => url === '/api/local/accounts' ? accounts.promise : previous(url, ...args);
  await f.render();
  assert.match(f.html('homeRecentList'), /claude:local/);
  accounts.resolve(f.backend);
});

test('a synchronous optional usage failure cannot strand the home skeleton', async () => {
  const f = fixture(async () => ({ sessions: [session('codex:local')] }));
  f.context.USAGE = { load() { throw new Error('optional usage unavailable'); } };
  await f.render();
  assert.match(f.html('homeRecentList'), /codex:local/);
});

test('refresh failure keeps the previous list visible and provides retry without empty state', async () => {
  let fail = false;
  const f = fixture(async () => { if (fail) throw new Error('history read failed'); return { sessions: [session('codex:cached')] }; });
  await f.render(); fail = true; await f.render();
  assert.match(f.html('homeRecentList'), /codex:cached/);
  assert.match(f.html('homeRecentList'), /history read failed/);
  assert.match(f.html('homeRecentList'), /data-act="homeRetry"/);
  assert.doesNotMatch(f.html('homeRecentList'), /还没有最近的对话/);
});

test('malformed session responses are errors and cancellation never paints a failure', async () => {
  const f = fixture(async () => ({}));
  await f.render();
  assert.match(f.html('homeRecentList'), /响应不完整/);
  const pending = deferred();
  const g = fixture(() => pending.promise);
  const reading = g.render(); g.context.homeCancelLoad(); g.S.route = 'settings';
  pending.reject(new Error('aborted')); await reading;
  assert.doesNotMatch(g.html('homeRecentList'), /暂时无法读取|aborted/);
});

for (const cancelled of [false, true]) test(`a stalled home fetch ${cancelled ? 'is cancelled on navigation' : 'times out, exits loading and retries'}`, async () => {
  const f = fixture(), timers = new Map();
  let pendingSignal, respond = false;
  f.context.setTimeout = (callback, ms) => { const timer = { callback, ms }; timers.set(timer, timer); return timer; };
  f.context.clearTimeout = timer => timers.delete(timer);
  f.context.fetch = (url, { signal }) => {
    assert.equal(url, '/api/sessions');
    pendingSignal = signal;
    if (respond) return Promise.resolve({ ok: true, json: async () => ({ sessions: [session('codex:after-timeout')] }) });
    return new Promise((_, reject) => signal.addEventListener('abort', () => reject(signal.reason), { once: true }));
  };
  // Exercise the production timeout and cancellation wrapper, not a rejected API stub.
  vm.runInContext(appSource.slice(appSource.indexOf('async function api('), appSource.indexOf('/* 系统相关的文案')), f.context);
  const read = f.render();
  const deadline = [...timers.values()].find(timer => timer.ms === 35000);
  assert.ok(deadline, 'local history must have a bounded deadline');
  if (cancelled) { f.S.route = 'settings'; f.S.routeRevision++; f.context.homeCancelLoad(); }
  else deadline.callback();
  await read;
  assert.equal(pendingSignal.aborted, true);
  assert.equal(timers.size, 0, 'settled reads release their deadline');
  if (cancelled) {
    assert.doesNotMatch(f.html('homeRecentList'), /暂时无法读取|超时|homeRetry/);
  } else {
    assert.equal(vm.runInContext('HOME.loading', f.context), false);
    assert.match(f.html('homeRecentList'), /本机响应超时/);
    assert.match(f.html('homeRecentList'), /data-act="homeRetry"/);
    assert.doesNotMatch(f.html('homeRecentList'), /正在读取|还没有最近的对话/);
    respond = true; await f.render();
    assert.match(f.html('homeRecentList'), /codex:after-timeout/);
    assert.doesNotMatch(f.html('homeRecentList'), /响应超时|homeRetry/);
    assert.equal(timers.size, 0);
  }
});

test('partial failures retain only failed Agent rows and successful recovery clears stale state', async () => {
  let response = { sessions: [session('codex:old'), session('claude:kept')] };
  const f = fixture(async () => response); await f.render();
  response = { sessions: [session('codex:new')], errors: ['Claude scan failed'], failedTools: ['claude'] }; await f.render();
  assert.match(f.html('homeRecentList'), /claude:kept/); assert.match(f.html('homeRecentList'), /待刷新/);
  assert.match(f.html('homeRecentList'), /codex:new/); assert.doesNotMatch(f.html('homeRecentList'), /codex:old/);
  response = { sessions: [session('codex:new')] }; await f.render();
  assert.doesNotMatch(f.html('homeRecentList'), /claude:kept|待刷新/);
});
