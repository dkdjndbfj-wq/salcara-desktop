'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const { EventEmitter } = require('node:events');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const { pathToFileURL } = require('node:url');
const { createCoreLifecycle } = require('../core-lifecycle.cjs');
const desktop = path.resolve(__dirname, '..');
const source = fs.readFileSync(path.join(desktop, 'main.cjs'), 'utf8');

function fixture() {
  const handlers = new Map(), appEvents = new Map(), pages = [], children = [], launches = [];
  let clock = 0;
  const f = { handlers, appEvents, pages, children, launches, exists: true, available: false, spawnCode: '',
    quits: 0, policySyncs: 0, receipts: 0, cleanups: 0, shown: 0, ballShown: 0, kills: 0 };
  const frame = { url: pathToFileURL(path.join(desktop, 'loading.html')).href };
  const contents = { mainFrame: frame, getURL: () => frame.url, executeJavaScript: async () => true, send() {} };
  f.win = { webContents: contents, isDestroyed: () => false, isVisible: () => true,
    async loadFile(file, options) { frame.url = pathToFileURL(file).href; pages.push({ file, options }); },
    async loadURL(url) { frame.url = url; pages.push({ url }); },
  };
  f.event = { sender: contents, senderFrame: frame };
  const app = { isPackaged: true, requestSingleInstanceLock: () => true, quit() { f.quits++; },
    on: (name, fn) => appEvents.set(name, fn), whenReady: () => new Promise(() => {}),
    getPath: () => path.join(desktop, 'synthetic-user-data'), setAppUserModelId() {}, getVersion: () => '1.6.6' };
  f.updater = { cleanup() { f.cleanups++; }, state: () => ({}), afterRestart() { f.receipts++; return null; } };
  const context = vm.createContext({ fixture: f, URL, AbortSignal,
    console: { error() {} }, __dirname: desktop,
    process: { platform: 'win32', arch: 'x64', env: {}, argv: [], execPath: 'E:\\fixture\\Salcara Bridge.exe', resourcesPath: 'E:\\fixture\\resources' },
    setTimeout() { return { unref() {} }; }, clearTimeout() {}, setInterval() {}, clearInterval() {},
    fetch: async url => {
      if (url.endsWith('healthz')) return { ok: f.available, json: async () => ({ service: 'salcara-bridge' }) };
      if (url.endsWith('/api/update/commit')) { const child = children.at(-1); f.available = false; child.exitCode = 0; child.emit('exit', 0, null); }
      return { ok: true, json: async () => ({ ok: true }) };
    },
    require(name) {
      if (name === 'electron') return { app, nativeTheme: {}, Notification: { isSupported: () => false },
        ipcMain: { on: (name, fn) => handlers.set(name, fn), handle: (name, fn) => handlers.set(name, fn) },
        session: { defaultSession: { cookies: { get: async () => [{ value: 'synthetic-cookie' }] } } } };
      if (name === 'node:fs') return { ...fs, existsSync: () => f.exists };
      if (name === 'node:child_process') return { spawn(executable, args, options) {
        launches.push({ executable, args, options });
        const child = new EventEmitter(); child.pid = f.spawnCode ? undefined : 500 + children.length; child.exitCode = null;
        child.kill = () => { f.kills++; }; children.push(child);
        if (f.spawnCode) queueMicrotask(() => child.emit('error', Object.assign(new Error('synthetic spawn error'), { code: f.spawnCode })));
        else f.available = true;
        return child;
      } };
      if (name === './core-lifecycle.cjs') return { createCoreLifecycle: options => createCoreLifecycle({ ...options,
        now: () => clock, sleep: async ms => { clock += ms; }, schedule: () => ({ unref() {} }), unschedule() {},
      }) };
      if (name === './ball-ipc.cjs') return { registerBallIpc: () => ({}) };
      if (name === './update-ipc.cjs') return { registerUpdateIpc: () => ({ allowed: () => false }) };
      if (name === './window-ipc.cjs') return { registerWindowIpc() {} };
      return name.startsWith('./') ? require(path.resolve(desktop, name)) : require(name);
    },
  });
  vm.runInContext(source, context);
  vm.runInContext(`window = fixture.win; createWindow = () => window; createTray = () => {};
    getUpdater = () => fixture.updater; startPluginSystem = async () => {};
    readBallState = () => ({}); applyAppearance = () => {}; readAppearance = () => 'light';
    showWindow = () => fixture.shown++; showBall = () => fixture.ballShown++; reloadBall = () => {};
    syncUpdatePolicy = async () => { fixture.policySyncs++; };`, context);
  f.context = context; f.start = () => context.start(); f.retry = () => handlers.get('core:retry')(f.event);
  return f;
}

test('packaged first start uses the bundled absolute core path, without a separate installation', async () => {
  const f = fixture(); await f.start();
  assert.equal(f.launches.length, 1);
  assert.equal(f.launches[0].executable, path.join('E:\\fixture\\resources', 'SalcaraBridge.exe'));
  assert.equal(f.launches[0].options.windowsHide, true);
  assert.equal(f.launches[0].options.env.SALCARA_EMBEDDED_WINDOW, '1');
  assert.equal(f.launches[0].options.env.SALCARA_DESKTOP_EXE, 'E:\\fixture\\Salcara Bridge.exe');
  assert.equal(f.pages.at(-1).url, 'http://127.0.0.1:47831/?boot=1');
  assert.equal(f.policySyncs, 1); assert.equal(f.receipts, 1); assert.equal(f.quits, 0);
});

test('missing bundled core keeps the shell open and refreshes the retry page on every failure', async () => {
  const f = fixture(); f.exists = false;
  await f.start();
  assert.equal(f.quits, 0); assert.equal(f.launches.length, 0);
  assert.match(f.pages.at(-1).options.query.error, /内置.*缺失/);
  await f.retry();
  assert.equal(f.pages.length, 2); assert.equal(f.pages.at(-1).options.query.retry, '1');
  f.exists = true;
  await Promise.all([f.retry(), f.retry(), f.retry()]);
  assert.equal(f.launches.length, 1); assert.equal(f.policySyncs, 1); assert.equal(f.receipts, 1);
  assert.equal(f.pages.at(-1).url, 'http://127.0.0.1:47831/');
});

for (const code of ['ENOENT', 'EACCES', 'EPERM']) {
  test(`spawn ${code} reaches a useful error page and a working manual retry`, async () => {
    const f = fixture(); f.spawnCode = code;
    await f.start();
    assert.equal(f.quits, 0); assert.equal(f.pages.at(-1).options.query.retry, '1');
    assert.match(f.pages.at(-1).options.query.error, code === 'ENOENT' ? /缺失/ : /权限|拦截/);
    f.spawnCode = ''; await f.retry();
    assert.equal(f.launches.length, 2); assert.equal(f.policySyncs, 1); assert.equal(f.receipts, 1);
  });
}

test('retry rejects another window, subframes and lookalike loading-page URLs', async () => {
  const f = fixture(); f.exists = false; await f.start();
  const retry = f.handlers.get('core:retry'); f.exists = true;
  await retry({ ...f.event, sender: {} });
  await retry({ ...f.event, senderFrame: { url: f.event.senderFrame.url } });
  f.event.senderFrame.url = 'file:///unrelated/loading.html'; await retry(f.event);
  f.event.senderFrame.url = pathToFileURL(path.join(desktop, 'loading.html.evil')).href; await retry(f.event);
  assert.equal(f.launches.length, 0);
  f.event.senderFrame.url = pathToFileURL(path.join(desktop, 'loading.html')).href + '?retry=1';
  await retry(f.event); assert.equal(f.launches.length, 1);
});

test('a hidden failed startup becomes visible and recovery keeps the floating-ball behavior', async () => {
  const f = fixture(); f.context.process.argv.push('--background'); f.win.isVisible = () => false; f.exists = false;
  await f.start(); assert.equal(f.shown, 1);
  f.exists = true; await f.retry();
  assert.equal(f.ballShown, 1); assert.equal(f.pages.at(-1).url, 'http://127.0.0.1:47831/');
});

test('update commit cannot auto-resurrect its child, while abort restores readiness', async () => {
  const f = fixture(); await f.start();
  await f.context.stopForUpdate(); await f.context.commitForUpdate();
  await Promise.resolve();
  assert.equal(f.launches.length, 1); assert.equal(f.kills, 0);
  assert.equal(vm.runInContext('quitting', f.context), true);
  await f.context.abortForUpdate();
  assert.equal(f.launches.length, 2); assert.equal(f.policySyncs, 2); assert.equal(f.receipts, 2);
  assert.equal(vm.runInContext('quitting', f.context), false);
});

test('an attached core is never claimed for update or killed on quit', async () => {
  const f = fixture(); f.available = true; await f.start();
  await assert.rejects(f.context.stopForUpdate(), /其他进程/);
  f.appEvents.get('before-quit')({});
  assert.equal(f.launches.length, 0); assert.equal(f.kills, 0);
});
