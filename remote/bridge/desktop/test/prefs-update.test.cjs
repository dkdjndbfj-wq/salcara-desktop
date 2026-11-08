'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const web = path.resolve(__dirname, '../../internal/console/web');
const prefs = fs.readFileSync(path.join(web, 'prefs.js'), 'utf8');
const app = fs.readFileSync(path.join(web, 'app.js'), 'utf8');
const busySource = app.slice(app.indexOf('const BUTTON_OPERATIONS ='), app.indexOf('let localConversationSyncPending ='));
const updateSource = prefs.slice(prefs.indexOf('let updateRowRevision ='), prefs.indexOf('(function wrapSettings()'));
function deferred() {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}
function fixture(shell = {}) {
  let html = '', row, statusReads = 0;
  const buttons = new Map(), notices = [];
  const button = id => {
    const attributes = new Map(), events = new Map();
    return { id, innerHTML: id === 'checkUpdate' ? '检查更新' : '查看更新', textContent: id === 'checkUpdate' ? '检查更新' : '查看更新', disabled: false, isConnected: true,
      setAttribute: (key, value) => attributes.set(key, value), getAttribute: key => attributes.get(key) ?? null,
      removeAttribute: key => attributes.delete(key), addEventListener: (name, fn) => events.set(name, fn),
      click() { return events.get('click')?.({ currentTarget: this }); } };
  };
  row = { isConnected: true, writes: 0, querySelector: selector => buttons.get(selector.slice(1)) || null,
    get innerHTML() { return html; },
    set innerHTML(value) {
      html = value; this.writes++;
      for (const old of buttons.values()) old.isConnected = false;
      buttons.clear();
      for (const id of ['checkUpdate', 'openUpdate']) if (html.includes('id="' + id + '"')) buttons.set(id, button(id));
    } };
  const context = vm.createContext({ $: selector => selector === '#updateRow' ? row : null,
    S: { route: 'settings' }, esc: String, icon: () => '', toast: (...args) => notices.push(args),
    window: { salcaraWindow: {
      updateStatus: async () => { statusReads++; return { configured: true, current: '1.6.4', phase: 'latest', ...shell.status }; },
      ...shell,
    } } });
  vm.runInContext(busySource, context, { filename: 'app-busy.js' });
  vm.runInContext(updateSource, context, { filename: 'prefs-update.js' });
  return { context, row, notices, buttons, statusReads: () => statusReads, render: () => context.renderUpdateRow() };
}

test('settings check-update calls the shell once while busy and leaves a usable button after success', async () => {
  const request = deferred(); let checks = 0;
  const f = fixture({ checkUpdate: () => { checks++; return request.promise; } });
  await f.render();
  const button = f.buttons.get('checkUpdate'), first = button.click(), second = button.click();
  assert.equal(checks, 1);
  assert.equal(button.disabled, true);
  assert.equal(button.getAttribute('aria-busy'), 'true');
  request.resolve({ phase: 'latest' });
  await Promise.all([first, second]);
  assert.deepEqual(f.notices, [['已经是最新版本', 'ok']]);
  assert.equal(f.statusReads(), 2);
  assert.equal(f.buttons.get('checkUpdate').disabled, false);
  assert.equal(f.buttons.get('checkUpdate').getAttribute('aria-busy'), null);
});

test('a rejected check shows failure and restores the button even when the status reread also fails', async () => {
  const request = deferred(); let reads = 0, checks = 0;
  const f = fixture({ checkUpdate: () => { checks++; return checks === 1 ? request.promise : Promise.resolve({ phase: 'latest' }); }, updateStatus: async () => {
    if (++reads > 1) throw new Error('status IPC unavailable');
    return { configured: true, phase: 'latest', current: '1.6.4' };
  } });
  await f.render();
  const button = f.buttons.get('checkUpdate'), pending = button.click();
  request.reject(new Error('check IPC unavailable'));
  await pending;
  assert.deepEqual(f.notices, [['检查更新失败', 'bad']]);
  assert.equal(button.disabled, false);
  assert.equal(button.innerHTML, '检查更新');
  assert.equal(button.getAttribute('aria-busy'), null);
  await button.click();
  assert.equal(checks, 2, 'the surviving button remains retryable after an unavailable status read');
  assert.deepEqual(f.notices.at(-1), ['已经是最新版本', 'ok']);
});

test('late update success and failure cannot toast or refresh after leaving settings', async () => {
  for (const failed of [false, true]) {
    const request = deferred(), f = fixture({ checkUpdate: () => request.promise });
    await f.render();
    const button = f.buttons.get('checkUpdate'), pending = button.click(), writes = f.row.writes;
    f.context.S.route = 'accounts'; f.row.isConnected = false; button.isConnected = false;
    if (failed) request.reject(new Error('late failure')); else request.resolve({ phase: 'latest' });
    await pending;
    assert.deepEqual(f.notices, []);
    assert.equal(f.statusReads(), 1);
    assert.equal(f.row.writes, writes);
    await button.click();
    assert.equal(f.statusReads(), 1, 'detached actions stay inactive');
  }
});

test('available, downloading and ready update entries only open the update window while the row owns the page', async () => {
  for (const [phase, label] of [['available', '查看更新'], ['downloading', '查看进度'], ['ready', '查看进度'], ['waiting', '查看进度']]) {
    let opens = 0;
    const f = fixture({ status: { phase, update: { version: '1.6.5' } }, openUpdate: () => { opens++; } });
    await f.render(); assert.match(f.row.innerHTML, new RegExp(label));
    assert.equal(f.buttons.has('checkUpdate'), false);
    const button = f.buttons.get('openUpdate'); await button.click(); assert.equal(opens, 1);
    f.context.S.route = 'accounts'; await button.click(); assert.equal(opens, 1);
  }
});

test('notification setting accurately describes the implemented Codex completion reminder', () => {
  const context = vm.createContext({ ico: () => '', NOTIFY: { enabled: () => false } });
  vm.runInContext(prefs.slice(prefs.indexOf('function prefsCard()'), prefs.indexOf('let updateRowRevision =')), context);
  const html = context.prefsCard();
  assert.match(html, /Codex 完成提醒/);
  assert.match(html, /主对话.*中文回复/);
  assert.doesNotMatch(html, /等你批准|任务完成 \/ 出错|checked/);
});
