import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';

// Execute the shipped handlers, request fallback, busy helper and router.
// Only DOM, transport and page renderers are synthetic; no native app opens.
const source = fs.readFileSync(new URL('../internal/console/web/app.js', import.meta.url), 'utf8').replaceAll('\r\n', '\n');
const names = ['resumeSession', 'localLaunch', 'localSwitchConfirm'];
const endpoints = { resumeSession: '/api/local/resume', localLaunch: '/api/local/launch', localSwitchConfirm: '/api/local/switch' };
const clone = value => JSON.parse(JSON.stringify(value));
const gate = () => { let resolve, reject; const promise = new Promise((a, b) => { resolve = a; reject = b; }); return { promise, resolve, reject }; };
const flush = async () => { for (let i = 0; i < 20; i++) await Promise.resolve(); };
const settled = promise => promise.then(value => ({ value }), error => ({ error }));
const success = () => ({ ok: true, result: { pid: 321, message: '合成启动回执', profileDir: 'C:/synthetic/profile' } });
function actionSource(name) {
  const start = source.indexOf(`  ${name}:`);
  assert.ok(start >= 0, `missing shipped action ${name}`);
  const rest = source.slice(start), next = rest.slice(1).search(/^  [\w$]+:/m);
  assert.ok(next >= 0, `missing action boundary after ${name}`);
  return rest.slice(0, next + 1);
}
function functionSource(name) {
  const start = source.search(new RegExp(`^(?:async )?function ${name}\\(`, 'm'));
  assert.ok(start >= 0, `missing shipped helper ${name}`);
  const end = source.indexOf('\n}', start);
  assert.ok(end > start, `missing helper boundary after ${name}`);
  return source.slice(start, end + 2);
}

function fixture(entry, { fallback = false, detail = false } = {}) {
  const nodes = new Map(), calls = [], notices = [], progressEvents = [], renders = [];
  const pending = gate();
  let transport = () => pending.promise, render = async () => {}, confirmation = true;
  function disconnect(node) {
    node.isConnected = false;
    if (nodes.get(node.id) === node) nodes.delete(node.id);
    for (const child of node.children) disconnect(child);
  }
  function element(id = '') {
    const node = { id, dataset: {}, children: [], isConnected: true, checked: false, disabled: false, textContent: '', html: '',
      get innerHTML() { return this.html; },
      set innerHTML(value) { this.children.forEach(disconnect); this.children = []; this.html = String(value); },
      get childElementCount() { return this.children.length || (this.html ? 1 : 0); },
      prepend(child) { child.parent = this; child.isConnected = true; this.children.unshift(child); if (child.id) nodes.set(child.id, child); },
      appendChild(child) { child.parent = this; child.isConnected = true; this.children.push(child); if (child.id) nodes.set(child.id, child); },
      remove() { if (this.parent) this.parent.children = this.parent.children.filter(child => child !== this); disconnect(this); },
    };
    if (id) nodes.set(id, node);
    return node;
  }
  const view = element('view'), modal = element('modalRoot');
  for (const id of ['pageTitle', 'pageSub', 'headActions']) element(id);
  if (entry === 'resumeSession' && !detail) modal.appendChild(element('restoreError'));
  if (entry === 'localSwitchConfirm') {
    const ready = element('switchReady'); ready.checked = true; modal.appendChild(ready); modal.appendChild(element('switchError'));
  }
  const button = element();
  button.dataset = { id: 'synthetic-account', target: 'codex', k: 'synthetic-original-session' };
  button.textContent = button.innerHTML = '打开';
  (entry === 'localLaunch' || detail ? view : modal).appendChild(button);
  const S = { route: 'accounts', routeRevision: 0, local: { accounts: [{ id: 'synthetic-account', target: 'old-target' }] } };
  const WB = { launchPending: false, launchGeneration: 0 };
  const progress = { finish() { progressEvents.push(['finish']); }, fail(error) { progressEvents.push(['fail', error]); } };
  const context = vm.createContext({ S, WB, ACTIONS: {}, AbortController, Error, TypeError, SyntaxError, Date,
    location: { hash: '#accounts' },
    PAGES: Object.fromEntries(['accounts', 'overview', 'settings'].map(name => [name, { title: name, sub: name }])),
    RENDER: Object.fromEntries(['accounts', 'overview', 'settings'].map(name => [name, async current => { renders.push(name); await render(name, current); }])),
    $: selector => nodes.get(selector.slice(1)) || null, $$: () => [],
    document: { getElementById: id => nodes.get(id) || null, createElement: () => element() },
    TOOL_LABEL: { codex: 'Codex CLI' }, ico: () => '',
    esc: value => String(value ?? '').replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('"', '&quot;'),
    confirm: () => confirmation, toast: (...args) => notices.push(args),
    beginLaunchProgress: (...args) => { progressEvents.push(['begin', ...clone(args)]); return progress; },
    setTimeout: () => 1, clearTimeout() {},
    fetch: async (path, options) => {
      calls.push({ kind: 'fetch', path, options, body: JSON.parse(options.body) });
      const data = await transport(path, JSON.parse(options.body));
      return { ok: data.httpStatus ? data.httpStatus < 400 : true, status: data.httpStatus || 200, json: async () => data };
    },
  });
  if (!fallback) context.launchAPI = async (path, body, handle) => {
    calls.push({ kind: 'stream', path, body: clone(body), handle });
    return transport(path, body);
  };
  const buttonOperations = source.match(/^const BUTTON_OPERATIONS = new WeakMap\(\);$/m)?.[0];
  assert.ok(buttonOperations, 'missing shipped button operation registry');
  vm.runInContext(`${buttonOperations}\n${functionSource('exclusiveUIOperation')}\n${functionSource('api')}\n${functionSource('busy')}\n${functionSource('route')}\nObject.assign(ACTIONS, {\n${names.map(actionSource).join('\n')}\n});`, context);
  return { context, S, WB, button, view, modal, nodes, calls, notices, progress, progressEvents, renders, pending,
    call: (name = entry, target = button) => context.ACTIONS[name](target),
    setTransport: fn => { transport = fn; }, setRender: fn => { render = fn; }, setConfirmation: value => { confirmation = value; },
    navigate: async name => { context.location.hash = '#' + name; await context.route(); },
    reopen() {
      if (entry === 'localLaunch' || detail) { view.innerHTML = '新页面'; return; }
      modal.innerHTML = '新窗口';
      if (entry === 'resumeSession') modal.appendChild(element('restoreError'));
      else { const ready = element('switchReady'); ready.checked = true; modal.appendChild(ready); modal.appendChild(element('switchError')); }
    },
  };
}

for (const name of names) {
  test(`${name} shares the streaming transport and finishes only after its receipt`, async () => {
    const f = fixture(name), running = f.call();
    assert.equal(f.calls.length, 1); assert.equal(f.calls[0].path, endpoints[name]);
    assert.equal(f.calls[0].kind, 'stream'); assert.equal(f.calls[0].handle, f.progress);
    assert.equal(f.WB.launchPending, true); assert.equal(f.button.disabled, true);
    assert.equal(f.progressEvents.length, 1); assert.equal(f.notices.length, 0);
    const expected = name === 'resumeSession' ? { target: 'codex', sessionKey: 'synthetic-original-session' }
      : { id: 'synthetic-account', target: 'codex', ...(name === 'localSwitchConfirm' ? { confirmed: true } : {}) };
    assert.deepEqual(f.calls[0].body, expected);
    f.pending.resolve(success()); await running;
    assert.equal(f.WB.launchPending, false); assert.equal(f.calls.length, 1);
    assert.deepEqual(f.progressEvents.slice(1), [['finish']]);
    assert.ok(f.notices.some(([, tone]) => tone === 'ok'));
    if (f.button.isConnected) assert.equal(f.button.disabled, false);
  });

  test(`${name} locks out every legacy launch entry until finally releases it`, async () => {
    const f = fixture(name), running = f.call();
    for (const other of names) await f.call(other, { isConnected: true, dataset: { ...f.button.dataset } });
    assert.equal(f.calls.length, 1); assert.equal(f.progressEvents.length, 1);
    f.pending.resolve(success()); await running; assert.equal(f.WB.launchPending, false);
    const disconnected = { isConnected: false, dataset: { ...f.button.dataset } };
    for (const other of names) await f.call(other, disconnected);
    assert.equal(f.calls.length, 1);
  });

  test(`${name} preserves the actual error and partial/uncertain outcome without replay`, async () => {
    for (const details of [{}, { uncertain: true }, { started: true, result: { pid: 321 } }]) {
      const f = fixture(name), error = Object.assign(new Error('原生启动失败 <exit=23>'), details);
      const running = settled(f.call()); f.pending.reject(error); const result = await running;
      assert.equal(f.progressEvents.at(-1)[0], 'fail'); assert.equal(f.progressEvents.at(-1)[1], error);
      assert.equal(f.progressEvents.some(([type]) => type === 'finish'), false);
      assert.equal(f.calls.length, 1); assert.equal(f.WB.launchPending, false); assert.equal(f.button.disabled, false);
      assert.equal(f.notices.length, 0);
      if (name === 'localLaunch') assert.equal(result.error, error);
      else { assert.equal(result.error, undefined); assert.match(f.nodes.get(name === 'resumeSession' ? 'restoreError' : 'switchError').innerHTML, /原生启动失败 &lt;exit=23>/); }
    }
  });

  test(`${name} rejects unconfirmed success and always releases the global lock`, async () => {
    for (const response of [{}, { ok: false }, { ok: 'true' }]) {
      const f = fixture(name), running = settled(f.call()); f.pending.resolve(response); await running;
      assert.equal(f.progressEvents.at(-1)[0], 'fail'); assert.equal(f.notices.length, 0);
      assert.equal(f.calls.length, 1); assert.equal(f.WB.launchPending, false); assert.equal(f.button.disabled, false);
    }
  });

  test(`${name} cannot paint a late success or error into a navigated or reopened owner`, async () => {
    for (const changed of ['navigation', 'reopen']) for (const fails of [false, true]) {
      const f = fixture(name), running = settled(f.call());
      if (changed === 'navigation') await f.navigate('settings'); else f.reopen();
      const viewHTML = f.view.innerHTML, modalHTML = f.modal.innerHTML;
      const errorNode = f.nodes.get(name === 'resumeSession' ? 'restoreError' : 'switchError');
      if (errorNode) errorNode.innerHTML = '新窗口内容';
      if (fails) f.pending.reject(new Error('旧启动错误')); else f.pending.resolve(success());
      const result = await running;
      assert.equal(result.error, undefined, 'a stale failure must not escape to the global toast handler');
      assert.equal(f.WB.launchPending, false); assert.equal(f.notices.length, 0); assert.equal(f.calls.length, 1);
      assert.equal(f.view.innerHTML, viewHTML); assert.equal(f.modal.innerHTML, modalHTML);
      assert.equal(f.view.children.length, 0);
      if (errorNode) assert.equal(errorNode.innerHTML, '新窗口内容');
    }
  });

  test(`${name} JSON fallback sends POST instead of passing progress as the HTTP method`, async () => {
    const f = fixture(name, { fallback: true }), running = f.call();
    assert.equal(f.calls.length, 1); assert.equal(f.calls[0].kind, 'fetch');
    assert.equal(f.calls[0].path, endpoints[name]); assert.equal(f.calls[0].options.method, 'POST');
    assert.equal(f.calls[0].options.credentials, 'same-origin');
    assert.equal(f.calls[0].options.headers['Content-Type'], 'application/json');
    f.pending.resolve(success()); await running;
    assert.equal(f.WB.launchPending, false); assert.deepEqual(f.progressEvents.slice(1), [['finish']]);
  });
}

test('session-detail resume with no modal and independent launch ignore replaced same-route content', async () => {
  for (const name of ['resumeSession', 'localLaunch']) for (const fails of [false, true]) {
    const f = fixture(name, { detail: true }), running = settled(f.call());
    assert.equal(f.nodes.has('restoreError'), false);
    await f.navigate('settings'); await f.navigate('accounts');
    const newAccount = { id: 'synthetic-account', target: 'replacement' }; f.S.local.accounts = [newAccount];
    assert.equal(f.button.isConnected, false);
    if (fails) f.pending.reject(new Error('旧详情的启动错误')); else f.pending.resolve(success());
    const result = await running;
    assert.equal(result.error, undefined); assert.equal(f.notices.length, 0); assert.equal(f.view.children.length, 0);
    assert.equal(newAccount.target, 'replacement'); assert.equal(newAccount.lastUsedAt, undefined);
    assert.equal(f.WB.launchPending, false); assert.equal(f.calls.length, 1);
  }
});

test('switch completion cannot paint after navigation or a new dialog during its own page refresh', async () => {
  for (const changed of ['navigation', 'roundtrip', 'new-dialog']) {
    const f = fixture('localSwitchConfirm'), render = gate();
    let first = true;
    f.setRender(async () => { if (first) { first = false; await render.promise; } });
    const running = settled(f.call()); f.pending.resolve(success()); await flush();
    assert.equal(f.renders.length, 1); assert.equal(f.WB.launchPending, true);
    if (changed === 'new-dialog') f.reopen();
    else { await f.navigate('settings'); if (changed === 'roundtrip') await f.navigate('accounts'); }
    const viewHTML = f.view.innerHTML, modalHTML = f.modal.innerHTML;
    render.resolve(); const result = await running;
    assert.equal(result.error, undefined); assert.equal(f.notices.length, 0); assert.equal(f.view.children.length, 0);
    assert.equal(f.view.innerHTML, viewHTML); assert.equal(f.modal.innerHTML, modalHTML);
    assert.equal(f.WB.launchPending, false); assert.equal(f.calls.length, 1);
  }
});

test('cancelled isolation confirmation and unchecked switch make no launch request', async () => {
  const isolated = fixture('localLaunch'); isolated.setConfirmation(false); await isolated.call();
  assert.equal(isolated.calls.length, 0); assert.equal(isolated.progressEvents.length, 0); assert.equal(isolated.WB.launchPending, false);
  const switching = fixture('localSwitchConfirm'); switching.nodes.get('switchReady').checked = false;
  await assert.rejects(switching.call(), /确认/);
  assert.equal(switching.calls.length, 0); assert.equal(switching.progressEvents.length, 0); assert.equal(switching.WB.launchPending, false);
});
