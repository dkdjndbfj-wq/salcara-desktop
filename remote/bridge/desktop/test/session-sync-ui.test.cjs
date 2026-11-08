'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.resolve(__dirname, '../../internal/console/web/app.js'), 'utf8').replace(/\r\n/g, '\n');
const start = source.indexOf('let localConversationSyncPending = false;');
const end = source.indexOf('\n}\n\nconst ACTIONS = {', start) + 2;
assert.ok(start >= 0 && end > start, 'settings sync action exists in the console');
const actionSource = source.slice(start, end);

function fixture(api) {
  const status = { textContent: '', isConnected: true };
  const button = { disabled: false, innerHTML: '重新同步', textContent: '重新同步', isConnected: true, dataset: {} };
  const state = {
    route: 'settings', routeRevision: 4, openKey: 'codex:thread-1', sessions: [], approvals: [],
    openInfo: null, tl: [], tlIndex: new Map(),
  };
  const context = vm.createContext({
    S: state,
    $: selector => selector === '#localSyncStatus' ? status : null,
    api,
    busy: async (_button, action) => action(),
    tlApply: event => state.tl.push(event),
    encodeURIComponent,
    console,
  });
  vm.runInContext(actionSource, context, { filename: 'settings-session-sync.js' });
  return { action: context.syncLocalConversations, state, status, button };
}

test('settings sync rereads the local index and only the currently open transcript using read APIs', async () => {
  const calls = [];
  const f = fixture(async (...args) => {
    calls.push(args);
    if (args[0] === '/api/sessions') return { sessions: [{ sessionKey: 'codex:thread-1' }, { sessionKey: 'claude:a' }] };
    assert.equal(args[0], '/api/session?key=codex%3Athread-1');
    return { session: { sessionKey: 'codex:thread-1', title: 'fresh' }, events: [{ type: 'message', id: 'fresh-event' }] };
  });

  await f.action(f.button);

  assert.deepEqual(calls, [['/api/sessions'], ['/api/session?key=codex%3Athread-1']]);
  assert.equal(f.state.sessions.length, 2);
  assert.equal(f.state.openInfo.title, 'fresh');
  assert.equal(f.state.tl[0].id, 'fresh-event');
  assert.match(f.status.textContent, /重新读取当前对话内容/);
});

test('settings sync accurately reports index-only refresh when no conversation is selected', async () => {
  let calls = 0;
  const f = fixture(async (...args) => { calls++; assert.equal(args.length, 1); return { sessions: [{ sessionKey: 'claude:a' }] }; });
  f.state.openKey = null;

  await f.action(f.button);

  assert.equal(calls, 1);
  assert.match(f.status.textContent, /1 个本机会话索引/);
  assert.match(f.status.textContent, /打开时读取/);
});

test('settings sync leaves state alone after navigation while the read is pending', async () => {
  let resolveList;
  let calls = 0;
  const f = fixture(() => { calls++; return new Promise(resolve => { resolveList = resolve; }); });
  f.state.sessions = [{ sessionKey: 'before' }];

  const pending = f.action(f.button);
  await Promise.resolve();
  assert.equal(calls, 1);
  f.state.route = 'sessions';
  resolveList({ sessions: [{ sessionKey: 'stale' }] });
  await pending;

  assert.deepEqual(f.state.sessions, [{ sessionKey: 'before' }]);
  assert.equal(f.status.textContent, '正在重新读取本机会话索引…');
});

test('settings sync shows the read error without retrying or writing to native app data', async () => {
  let calls = 0;
  const f = fixture(async (...args) => { calls++; assert.equal(args.length, 1); throw new Error('会话索引读取失败'); });

  await f.action(f.button);

  assert.equal(calls, 1);
  assert.equal(f.status.textContent, '会话索引读取失败：会话索引读取失败');
});

test('settings sync identifies failure while rereading the open conversation', async () => {
  const f = fixture(async endpoint => {
    if (endpoint === '/api/sessions') return { sessions: [{ sessionKey: 'codex:thread-1' }] };
    throw new Error('对话内容读取失败');
  });

  await f.action(f.button);

  assert.match(f.status.textContent, /^当前对话内容读取失败：/);
  assert.equal(f.state.sessions.length, 1, 'the fresh index is retained even when the opened transcript fails');
});

test('settings exposes the operation and states its read-only scope', () => {
  assert.match(source, /data-act="syncLocalConversations"/);
  assert.match(source, /仅读取本机记录，不修改原应用数据/);
  assert.match(source, /syncLocalConversations,/);
});
