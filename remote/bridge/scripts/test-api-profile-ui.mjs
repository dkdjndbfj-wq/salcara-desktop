import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';

const source = fs.readFileSync(new URL('../internal/console/web/codex-api-profile.js', import.meta.url), 'utf8');
const clone = value => JSON.parse(JSON.stringify(value));
const deferred = () => { let resolve, reject; const promise = new Promise((a, b) => { resolve = a; reject = b; }); return { promise, resolve, reject }; };
const flush = async () => { for (let n = 0; n < 15; n++) await Promise.resolve(); };
function fixture() {
  const calls = [], listeners = {}, nodes = new Map();
  let intercept;
  const data = { profile: { state: 'idle', revision: 'disk-a', generation: '', hasBackup: false }, running: true, active: false,
    clients: [{ id: 'client-a', name: 'API <A>', revision: 'source-a', models: ['model-a', 'model-b'] }, { id: 'client-b', name: 'B', revision: 'source-b', models: ['b-only'] }] };
  const context = vm.createContext({ ACTIONS: {}, OFFICIAL_PANEL: { epoch: 0 }, document: { addEventListener(type, fn) { listeners[type] = fn; } },
    $: selector => nodes.get(selector), icon: () => '', esc: value => String(value ?? '').replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('"', '&quot;'),
    officialPanel() { context.OFFICIAL_PANEL.epoch++; nodes.set('#apiProfileContent', { innerHTML: '' }); },
    loadCodexAPIOverview: async () => {},
    api: async (url, body) => { calls.push({ url, body }); if (intercept) { const result = await intercept(url, body); if (result !== undefined) return result; }
      if (!body) return clone(data);
      if (url.endsWith('/apply')) Object.assign(data, { active: true, clientId: body.clientId, model: body.model, profile: { state: 'applied', revision: 'disk-b', generation: 'generation-b', hasBackup: true } });
      else Object.assign(data, { active: false, profile: { state: 'idle', revision: 'disk-c', generation: '', hasBackup: false } });
      return { ok: true };
    },
  });
  vm.runInContext(source, context);
  return { context, data, calls, open: () => context.ACTIONS.codexAPIProfile(), apply: () => context.ACTIONS.codexAPIProfileApply(), restore: () => context.ACTIONS.codexAPIProfileRestore(),
    change: (id, value) => listeners.change({ target: { id, value } }), html: () => nodes.get('#apiProfileContent')?.innerHTML || '',
    intercept: fn => { intercept = fn; }, writes: () => calls.filter(call => call.body),
  };
}

test('profile apply binds the selected client revision and the exact cached model', async () => {
  const f = fixture(); await f.open();
  assert.equal(f.writes().length, 0); assert.match(f.html(), /API &lt;A>/);
  f.change('apiProfileClient', 'client-b'); await f.apply();
  assert.deepEqual(JSON.parse(JSON.stringify(f.writes()[0].body)), { clientId: 'client-b', clientRevision: 'source-b', profileRevision: 'disk-a', model: 'b-only' });
  assert.match(f.html(), /已应用 · 本次连接有效/);
  await f.restore();
  assert.deepEqual(JSON.parse(JSON.stringify(f.writes()[1].body)), { revision: 'disk-b', generation: 'generation-b' });
  assert.match(f.html(), /未应用/);
});

test('late panel load cannot overwrite a reopened panel', async () => {
  const f = fixture(), gate = deferred(); let reads = 0;
  f.intercept((url, body) => { if (!body && ++reads === 1) return gate.promise; });
  const first = f.open(); await flush();
  const newer = clone(f.data); newer.profile.state = 'conflict'; newer.profile.error = '新配置冲突';
  f.data.profile = newer.profile; await f.open();
  gate.resolve({ ...newer, profile: { state: 'idle', revision: 'old', hasBackup: false } }); await first;
  assert.match(f.html(), /新配置冲突/); assert.doesNotMatch(f.html(), />未应用</);
});

test('duplicate clicks and reopened panels do not duplicate an in-flight write', async () => {
  const f = fixture(), gate = deferred();
  await f.open(); f.intercept((_url, body) => body ? gate.promise : undefined);
  const first = f.apply(); await flush();
  await f.apply(); await f.open(); await f.apply();
  assert.equal(f.writes().length, 1);
  Object.assign(f.data, { active: true, profile: { state: 'applied', revision: 'new', generation: 'new-gen', hasBackup: true } });
  gate.resolve({ ok: true }); await first;
  assert.match(f.html(), /本次连接有效/);
  assert.doesNotMatch(f.html(), /data-act="codexAPIProfileRestore" disabled/);
});

test('lost mutation response is not retried and a fresh read determines the state', async () => {
  const f = fixture(); await f.open();
  f.intercept((_url, body) => { if (body) { Object.assign(f.data, { active: true, profile: { state: 'applied', revision: 'new', generation: 'new-gen', hasBackup: true } }); throw new Error('响应未确认'); } });
  await f.apply();
  assert.equal(f.writes().length, 1); assert.match(f.html(), /响应未确认/); assert.match(f.html(), /本次连接有效/);
});

test('conflict and stopped service cannot submit an apply; stopped service can restore', async () => {
  const f = fixture(); Object.assign(f.data, { running: false, clients: [], profile: { state: 'applied', revision: 'stored', generation: 'gen', hasBackup: true } });
  await f.open(); await f.apply(); assert.equal(f.writes().length, 0);
  assert.match(f.html(), /连接已失效/); await f.restore(); assert.equal(f.writes().length, 1);
  Object.assign(f.data, { running: true, profile: { state: 'conflict', revision: 'conflict', generation: 'gen', hasBackup: true, error: '外部修改' } });
  await f.open(); await f.restore(); await f.apply(); assert.equal(f.writes().length, 1);
});
