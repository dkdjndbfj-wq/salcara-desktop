'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const { EventEmitter } = require('node:events');
const { fromBall, clampBallBounds, ballLayout, restoredBallBounds, registerBallIpc } = require('../ball-ipc.cjs');

const origin = 'http://127.0.0.1:47831/';
const preload = fs.readFileSync(path.resolve(__dirname, '../preload.cjs'), 'utf8');
const main = fs.readFileSync(path.resolve(__dirname, '../main.cjs'), 'utf8');
const plain = value => JSON.parse(JSON.stringify(value));

class FakeWindow extends EventEmitter {
  constructor(bounds = { x: 100, y: 200, width: 400, height: 210 }) {
    super();
    this.bounds = { x: 100, y: 200, width: 400, height: 210, ...bounds };
    this.visible = false; this.destroyed = false; this.loading = false;
    this.positions = []; this.mouseCalls = []; this.sent = []; this.showCount = 0; this.loads = [];
    this.webContents = new EventEmitter();
    Object.assign(this.webContents, {
      mainFrame: { url: origin + 'ball.html' },
      isLoading: () => this.loading,
      send: (...args) => { this.sent.push(args); this.renderer?.emit(args[0], {}, ...args.slice(1)); },
      setWindowOpenHandler() {},
    });
  }
  isDestroyed() { return this.destroyed; }
  isVisible() { return this.visible; }
  getBounds() { return { x: this.bounds.x, y: this.bounds.y, width: this.bounds.width, height: this.bounds.height }; }
  setBounds(bounds) {
    assert.equal(this.destroyed, false);
    if (bounds.x !== this.bounds.x || bounds.y !== this.bounds.y) this.positions.push([bounds.x, bounds.y]);
    this.bounds = { ...this.bounds, ...bounds };
  }
  setPosition(x, y) { assert.equal(this.destroyed, false); this.bounds.x = x; this.bounds.y = y; this.positions.push([x, y]); }
  setIgnoreMouseEvents(...args) { assert.equal(this.destroyed, false); this.mouseCalls.push(args); }
  setAlwaysOnTop() {}
  loadURL(url) {
    this.webContents.mainFrame.url = url; this.loading = true;
    const load = { url };
    const promise = new Promise((resolve, reject) => { load.resolve = resolve; load.reject = reject; });
    this.loads.push(load); this.webContents.emit('did-start-loading'); return promise;
  }
  loaded() { this.loading = false; this.webContents.emit('did-finish-load'); this.loads.at(-1)?.resolve(); }
  failed(code = -102) {
    this.loading = false;
    this.webContents.emit('did-fail-load', {}, code, 'ERR_CONNECTION_REFUSED', origin + 'ball.html', true);
    this.loads.at(-1)?.reject(Object.assign(new Error('ERR_CONNECTION_REFUSED'), { code: 'ERR_CONNECTION_REFUSED' }));
  }
  showInactive() { this.visible = true; this.showCount++; this.emit('show'); }
  hide() { this.visible = false; this.emit('hide'); }
  close() { this.destroyed = true; this.visible = false; this.emit('closed'); }
  event() { return { sender: this.webContents, senderFrame: this.webContents.mainFrame }; }
}

function fakeScreen() {
  const screen = new EventEmitter();
  screen.cursor = { x: 300, y: 305 };
  screen.displays = [
    { workArea: { x: 0, y: 0, width: 1920, height: 1040 }, scaleFactor: 1.5 },
    { workArea: { x: -1280, y: -100, width: 1280, height: 900 }, scaleFactor: 2 },
  ];
  screen.getCursorScreenPoint = () => screen.cursor;
  screen.getPrimaryDisplay = () => screen.displays[0];
  screen.getDisplayNearestPoint = p => screen.displays.reduce((nearest, display) => {
    const distance = d => {
      const a = d.workArea;
      return Math.hypot(p.x - Math.min(Math.max(p.x, a.x), a.x + a.width), p.y - Math.min(Math.max(p.y, a.y), a.y + a.height));
    };
    return distance(display) < distance(nearest) ? display : nearest;
  });
  return screen;
}

function connectPreload(win, ipcMain, getEvent = () => win.event()) {
  const exposed = {}, ipcRenderer = new EventEmitter();
  ipcRenderer.send = (name, ...args) => ipcMain.emit(name, getEvent(), ...args);
  ipcRenderer.invoke = () => { throw new Error('Unexpected privileged invocation'); };
  win.renderer = ipcRenderer;
  vm.runInNewContext(preload, { require(name) {
    assert.equal(name, 'electron');
    return { contextBridge: { exposeInMainWorld: (name, value) => { exposed[name] = value; } }, ipcRenderer };
  } });
  return exposed.salcaraBall;
}

function fixture() {
  const win = new FakeWindow(), screen = fakeScreen(), ipcMain = new EventEmitter(), saved = [], shown = [];
  win.visible = true;
  const state = { current: win };
  const api = registerBallIpc({ ipcMain, screen, getBall: () => state.current, consoleURL: origin, width: 400, height: 210,
    showWindow: route => shown.push(route), savePosition: position => saved.push(position) });
  api.watch(win);
  return { win, screen, ipcMain, saved, shown, state, api, host: connectPreload(win, ipcMain),
    send: (name, ...args) => ipcMain.emit(name, win.event(), ...args),
  };
}

test('the real preload reaches the separate floating window and keeps its main-window scope narrow', () => {
  const f = fixture();
  assert.equal(fromBall(f.win.event(), f.win, origin), true);
  f.host.setInteractive(true); assert.deepEqual(f.win.mouseCalls.at(-1), [false]);
  f.host.setInteractive(false); assert.deepEqual(f.win.mouseCalls.at(-1), [true, { forward: true }]);
  f.host.show('accounts'); f.host.show(42);
  assert.deepEqual(f.shown, ['accounts', '']);
  f.win.renderer.send('win:close'); assert.equal(f.win.isVisible(), true);
});

test('all ball commands reject the main page, foreign windows, subframes, origins, ports and paths', () => {
  for (const type of ['window', 'subframe', 'main page', 'index page', 'origin', 'port', 'path', 'file', 'malformed']) {
    const f = fixture(), event = f.win.event();
    if (type === 'window') event.sender = new FakeWindow().webContents;
    if (type === 'subframe') event.senderFrame = { url: origin + 'ball.html' };
    const urls = { 'main page': origin, 'index page': origin + 'index.html', origin: 'https://evil.example/ball.html', port: 'http://127.0.0.1:47832/ball.html', path: origin + 'sub/ball.html', file: 'file:///ball.html', malformed: 'invalid' };
    if (urls[type]) event.senderFrame.url = urls[type];
    for (const [channel, arg] of [['ball:show', 'usage'], ['ball:interactive', true], ['ball:drag', 'start'], ['ball:drag', 'move'], ['ball:drag', 'end'], ['ball:drag', 'cancel']]) f.ipcMain.emit(channel, event, arg);
    assert.deepEqual(f.shown, [], type); assert.deepEqual(f.win.mouseCalls, [], type); assert.deepEqual(f.saved, [], type);
  }
});

test('hidden, destroyed, loading and absent windows cannot accept late renderer work', () => {
  for (const type of ['hidden', 'destroyed', 'loading', 'absent']) {
    const f = fixture();
    if (type === 'hidden') f.win.visible = false;
    if (type === 'destroyed') f.win.destroyed = true;
    if (type === 'loading') f.win.loading = true;
    if (type === 'absent') f.state.current = null;
    f.host.show('accounts'); f.host.setInteractive(true); f.host.dragStart(); f.host.drag(); f.host.dragEnd(); f.host.dragCancel();
    assert.deepEqual(f.shown, [], type); assert.deepEqual(f.win.mouseCalls, [], type); assert.deepEqual(f.saved, [], type);
  }
});

test('mouse IPC validates booleans and active drags retain native mouse capture until release', () => {
  const f = fixture();
  for (const invalid of ['true', 1, null, {}, []]) f.send('ball:interactive', invalid);
  assert.deepEqual(f.win.mouseCalls, []);
  f.host.dragStart(); assert.deepEqual(f.win.mouseCalls.at(-1), [false]);
  f.host.setInteractive(false); assert.deepEqual(f.win.mouseCalls.at(-1), [false]);
  f.host.dragEnd(); assert.deepEqual(f.win.mouseCalls.at(-1), [true, { forward: true }]);
  assert.deepEqual(f.saved, [], 'a simple click does not rewrite remembered position');
});

test('native cursor movement uses DIP across negative-coordinate displays with different scales', () => {
  const f = fixture();
  f.host.dragStart();
  f.screen.cursor = { x: -400.4, y: 155.2 }; f.host.drag();
  assert.deepEqual(f.win.positions.at(-1), [-600, 50]);
  f.screen.cursor = { x: -300, y: 205 }; f.host.dragStart(); f.host.drag();
  assert.deepEqual(f.win.positions.at(-1), [-500, 100], 'duplicate start cannot change the drag offset');
  f.host.dragEnd(); assert.deepEqual(f.saved, [{ x: -500, y: 100 }]);
  f.screen.cursor = { x: -800, y: 400 }; f.host.drag();
  assert.deepEqual(f.win.positions.at(-1), [-500, 100]);
});

test('movement without start, unknown phases and malformed native coordinates do not move the ball', () => {
  const f = fixture(); f.host.drag(); f.send('ball:drag', 'unknown');
  for (const cursor of [null, {}, { x: NaN, y: 0 }, { x: 0, y: Infinity }, { x: '100', y: 0 }, { x: 100000, y: 0 }]) {
    f.screen.cursor = cursor; f.host.dragStart(); f.host.drag();
  }
  assert.deepEqual(f.win.positions, []);
  f.screen.cursor = { x: 300, y: 305 }; f.host.dragStart();
  f.screen.cursor = { x: Infinity, y: 200 }; f.host.drag(); f.host.dragCancel();
  assert.deepEqual(f.saved, []); assert.deepEqual(f.win.mouseCalls.at(-1), [true, { forward: true }]);
});

test('repeated fractional-DPI moves preserve requested ball size rather than feeding rounded bounds back into movement', () => {
  for (const phase of ['end', 'cancel', 'hide']) {
    const f = fixture(), requested = [];
    const setBounds = f.win.setBounds.bind(f.win);
    f.win.setPosition = () => { throw new Error('setPosition accumulates fractional-DPI window rounding'); };
    f.win.setBounds = bounds => {
      requested.push({ ...bounds });
      setBounds({ ...bounds, width: bounds.width + 1, height: bounds.height + 1 });
    };
    f.host.dragStart();
    for (let index = 1; index <= 20; index++) {
      f.screen.cursor = { x: 300 + index * 17, y: 305 + index * 13 }; f.host.drag();
      assert.deepEqual(f.win.getBounds(), { x: 100 + index * 17, y: 200 + index * 13, width: 401, height: 211 });
    }
    if (phase === 'hide') f.win.hide(); else f.send('ball:drag', phase);
    assert.equal(requested.length, 21, phase + ' also repairs a rounded native size without compounding it');
    assert.ok(requested.every(bounds => bounds.width === 400 && bounds.height === 210));
    assert.deepEqual(f.saved, [{ x: 440, y: 460 }]);
    f.win.visible = true; f.screen.emit('display-metrics-changed');
    assert.deepEqual(requested.at(-1), { x: 440, y: 460, width: 400, height: 210 });
  }
});

test('end and cancel clamp the quick-entry ring and plugin picker into the connected display work area', () => {
  for (const phase of ['end', 'cancel']) {
    const f = fixture(); f.host.setInteractive(true); f.host.dragStart();
    f.screen.cursor = { x: 4000, y: 4000 }; f.host.drag(); f.send('ball:drag', phase);
    assert.deepEqual(f.saved, [{ x: 1624, y: 837 }]);
    assert.deepEqual(f.win.getBounds(), { x: 1624, y: 837, width: 400, height: 210 });
    assert.deepEqual(f.win.mouseCalls.at(-1), [true, { forward: true }], 'the final ball is no longer under the pointer after clamping');
    f.screen.cursor = { x: 700, y: 800 }; f.host.drag(); assert.deepEqual(f.saved, [{ x: 1624, y: 837 }]);
  }
});

test('finishing a moved drag reconciles click-through with the final native ball position through the preload', () => {
  for (const cancel of [false, true]) {
    const f = fixture(), received = [];
    const unsubscribe = f.host.onDragFinished(on => { received.push(on); f.host.setInteractive(on); });
    f.host.setInteractive(true); f.host.dragStart();
    f.screen.cursor = { x: 350, y: 355 }; f.host.drag();
    if (cancel) f.host.dragCancel(); else f.host.dragEnd();
    assert.deepEqual(received, [!cancel]);
    assert.deepEqual(f.win.mouseCalls.at(-1), cancel ? [true, { forward: true }] : [false]);
    f.host.dragStart(); f.screen.cursor = { x: 1940, y: 1080 }; f.host.drag(); f.host.dragEnd();
    assert.deepEqual(received, [!cancel, false], 'a pointer outside the clamped ball cannot retain capture');
    assert.deepEqual(f.win.mouseCalls.at(-1), [true, { forward: true }]);
    unsubscribe(); f.win.webContents.send('ball:drag-finished', true);
    assert.deepEqual(received, [!cancel, false]);
    assert.doesNotThrow(() => f.host.onDragFinished(null)());
  }
});

test('simple clicks and native lifecycle cancellation do not deliver a drag completion into a later shown page', () => {
  const f = fixture(), received = [];
  f.host.onDragFinished(on => received.push(on));
  f.host.dragStart(); f.host.dragEnd();
  f.host.dragStart(); f.screen.cursor = { x: 350, y: 355 }; f.host.drag(); f.win.hide();
  f.host.dragEnd();
  assert.deepEqual(received, []);
});

test('foreign-window commands cannot release, reposition or steal the current ball drag', () => {
  const f = fixture(); f.host.dragStart();
  const alien = new FakeWindow(); alien.visible = true; const foreign = connectPreload(alien, f.ipcMain);
  foreign.dragCancel(); foreign.dragEnd(); foreign.dragStart(); foreign.setInteractive(false); foreign.show('settings');
  f.screen.cursor = { x: 350, y: 355 }; foreign.drag(); f.host.drag();
  assert.deepEqual(f.win.positions, [[150, 250]]); assert.deepEqual(alien.positions, []); assert.deepEqual(f.shown, []);
  f.host.dragEnd(); assert.deepEqual(f.saved, [{ x: 150, y: 250 }]);
});

test('hide, blur, renderer loss, reload and close always clear owned drag state', () => {
  for (const type of ['hide', 'blur', 'unresponsive', 'render-process-gone', 'did-start-loading', 'closed']) {
    const f = fixture(); f.host.dragStart(); f.screen.cursor = { x: 350, y: 355 }; f.host.drag();
    if (type === 'closed') f.win.close();
    else if (type === 'hide') f.win.hide();
    else if (type === 'did-start-loading' || type === 'render-process-gone') f.win.webContents.emit(type);
    else f.win.emit(type);
    if (type !== 'closed') {
      assert.deepEqual(f.saved, [{ x: 150, y: 250 }], type);
      assert.deepEqual(f.win.mouseCalls.at(-1), [true, { forward: true }], type);
      f.win.visible = true; f.screen.cursor = { x: 800, y: 700 }; f.host.drag();
      assert.deepEqual(f.win.positions, [[150, 250]], type);
    }
    const replacement = new FakeWindow(); replacement.visible = true;
    f.state.current = replacement; f.api.watch(replacement); f.api.reset(replacement);
    const host = connectPreload(replacement, f.ipcMain);
    f.screen.cursor = { x: 300, y: 305 }; host.dragStart(); f.screen.cursor = { x: 310, y: 315 }; host.drag();
    assert.deepEqual(replacement.positions, [[110, 210]], type);
  }
});

test('old-window lifecycle callbacks cannot change the replacement window or release its drag', () => {
  const f = fixture(), old = f.win, replacement = new FakeWindow(); replacement.visible = true;
  f.state.current = replacement; f.api.watch(replacement); f.api.reset(replacement);
  const host = connectPreload(replacement, f.ipcMain); host.dragStart();
  for (const type of ['hide', 'blur', 'closed']) old.emit(type);
  for (const type of ['did-start-loading', 'did-finish-load', 'render-process-gone']) old.webContents.emit(type);
  f.screen.cursor = { x: 340, y: 345 }; host.drag();
  assert.deepEqual(replacement.positions, [[140, 240]]); assert.deepEqual(replacement.sent, []);
});

test('a stationary pointer on a newly shown ball remains interactive through the actual preload callback', () => {
  const f = fixture(), received = [];
  const unsubscribe = f.host.onShown(on => { received.push(on); f.host.setInteractive(on); });
  f.win.webContents.send('ball:shown', f.api.reset(f.win, true));
  assert.deepEqual(received, [true]); assert.deepEqual(f.win.mouseCalls.at(-1), [false]);
  f.screen.cursor = { x: 10, y: 10 }; f.win.webContents.send('ball:shown', f.api.reset(f.win, true));
  assert.deepEqual(received, [true, false]); assert.deepEqual(f.win.mouseCalls.at(-1), [true, { forward: true }]);
  unsubscribe(); f.win.webContents.send('ball:shown', true); assert.deepEqual(received, [true, false]);
  assert.doesNotThrow(() => f.host.onShown(null)());
});

test('renderer reload and display removal rearm and constrain a visible ball without waiting for pointer movement', () => {
  const f = fixture(); f.win.loaded(); assert.deepEqual(f.win.sent.at(-1), ['ball:shown', true]);
  f.win.setBounds({ x: -1200, y: 500 }); f.screen.displays = f.screen.displays.slice(0, 1);
  f.screen.emit('display-removed');
  assert.deepEqual(f.saved, [{ x: -104, y: 500 }]); assert.deepEqual(f.win.sent.at(-1), ['ball:shown', false]);
  f.screen.displays[0].workArea.height = 600; f.screen.emit('display-metrics-changed');
  assert.deepEqual(f.saved.at(-1), { x: -104, y: 397 });
  f.win.hide(); const count = f.saved.length; f.screen.emit('display-removed'); assert.equal(f.saved.length, count);
});

test('saved positions reject malformed values and safely fit a tiny monitor', () => {
  const screen = fakeScreen(), expected = { x: 1624, y: 785, width: 400, height: 210 };
  for (const saved of [undefined, null, [], {}, 'text', 4, { x: 1 }, { x: null, y: null }, { x: '10', y: 20 }, { x: Infinity, y: 0 }, { x: -100000, y: 0 }]) assert.deepEqual(restoredBallBounds(saved, screen, 400, 210), expected);
  assert.deepEqual(restoredBallBounds({ x: -1200, y: -90 }, screen, 400, 210), { x: -1200, y: -90, width: 400, height: 210 });
  screen.displays = [{ workArea: { x: -50, y: -20, width: 100, height: 100 }, scaleFactor: 3 }];
  assert.deepEqual(restoredBallBounds({ x: 9000, y: -9000 }, screen, 400, 210), { x: -200, y: -75, width: 400, height: 210 });
});

function mainFixture(saved = '{}') {
  const windows = [], screen = fakeScreen(), ipcMain = new EventEmitter(), writes = [], calls = [], timers = new Map(), errors = [];
  let nextTimer = 0;
  ipcMain.handle = () => {};
  const app = new EventEmitter();
  Object.assign(app, { requestSingleInstanceLock: () => true, whenReady: () => new Promise(() => {}), quit() {}, getPath: () => path.resolve('fixture-data') });
  const context = vm.createContext({ URL, Promise, console: { ...console, error: (...args) => errors.push(args) }, process: { env: {}, platform: 'win32' }, __dirname: path.resolve(__dirname, '..'),
    setTimeout: (fn, delay) => { const id = ++nextTimer; timers.set(id, { fn, delay }); return id; },
    clearTimeout: id => timers.delete(id),
    require(name) {
      if (name === 'electron') return { app, screen, ipcMain, BrowserWindow: class extends FakeWindow { constructor(options) { super(options); windows.push(this); } } };
      if (name === './package.json') return { version: '1.6.4' };
      if (name === 'node:child_process') return { spawn() { throw new Error('Test must not launch real Bridge or Agent'); } };
      if (name === 'node:fs') return { readFileSync(filename) { assert.equal(path.basename(filename), 'floating-ball.json'); return saved; }, mkdirSync() {}, writeFileSync(filename, body) { writes.push([filename, body]); saved = body; } };
      if (name.startsWith('./')) return require(path.resolve(__dirname, '..', name));
      return require(name);
    },
  });
  vm.runInContext(main, context);
  context.mainWindowFixture = { isDestroyed: () => false, isVisible: () => false, show: () => calls.push('show'), isMinimized: () => true, restore: () => calls.push('restore'), focus: () => calls.push('focus'),
    webContents: { getURL: () => origin, executeJavaScript: script => { calls.push(script); return Promise.resolve(); } } };
  vm.runInContext('window = mainWindowFixture; registerIpc();', context);
  return { context, windows, screen, ipcMain, writes, calls, timers, errors, run: code => vm.runInContext(code, context),
    runNextTimer() { const [id, timer] = timers.entries().next().value || []; if (!timer) return; timers.delete(id); timer.fn(); },
  };
}

test('actual main-window registration accepts the ball preload, opens allowed routes and hides the ball', async () => {
  const f = mainFixture(); f.run('showBall()'); const win = f.windows[0]; await win.loaded();
  assert.equal(win.visible, true); assert.equal(win.showCount, 1);
  const host = connectPreload(win, f.ipcMain); host.show('accounts');
  assert.equal(win.visible, false); assert.deepEqual(f.calls, ['show', 'restore', 'focus', 'location.hash = "#accounts"']);
  f.run('showBall()'); host.show('javascript:alert(1)');
  assert.equal(f.calls.at(-1), "history.pushState(null, '', location.pathname); dispatchEvent(new PopStateEvent('popstate'))");
});

test('hide, disable and quit cancel a pending show before the floating page finishes loading', async () => {
  for (const cancel of ['hideBall()', 'setBallEnabled(false)', 'quitting = true']) {
    const f = mainFixture(); f.run('showBall()'); const win = f.windows[0];
    f.run(cancel); await win.loaded(); assert.equal(win.showCount, 0, cancel); assert.equal(win.visible, false, cancel);
  }
});

test('repeated pending shows reveal only once and cannot revive a replaced window', async () => {
  const f = mainFixture(); f.run('showBall(); showBall(); showBall()');
  const first = f.windows[0]; await first.loaded(); assert.equal(first.showCount, 1);
  first.loadURL(origin + 'ball.html'); f.run('showBall()'); first.close();
  f.run('showBall()'); const replacement = f.windows[1];
  await first.loaded(); assert.equal(first.showCount, 1); assert.equal(replacement.showCount, 0);
  await replacement.loaded(); assert.equal(replacement.showCount, 1); assert.equal(f.run('ball === null'), false);
});

test('repeated show requests preserve a visible ball, its live drag and its remembered final position', async () => {
  const f = mainFixture('{"x":100,"y":200}'); f.run('showBall()'); const win = f.windows[0]; await win.loaded();
  const host = connectPreload(win, f.ipcMain);
  f.screen.cursor = { x: 300, y: 305 }; host.dragStart();
  f.screen.cursor = { x: 350, y: 355 }; host.drag();
  f.run('showBall(); showBall()');
  assert.deepEqual(win.getBounds(), { x: 150, y: 250, width: 400, height: 210 });
  assert.equal(win.showCount, 1);
  f.screen.cursor = { x: 380, y: 385 }; host.drag(); host.dragEnd();
  assert.deepEqual(win.getBounds(), { x: 180, y: 280, width: 400, height: 210 });
  assert.deepEqual(JSON.parse(f.writes.at(-1)[1]), { x: 180, y: 280 });
});

test('late startup and pending load callbacks cannot show a ball over an already visible main window', async () => {
  const f = mainFixture();
  f.run('window.isVisible = () => true; startHidden = true; closedDuringStart = true; showBall()');
  assert.equal(f.windows.length, 0, 'startup flags do not override the currently visible main window');
  f.run('window.isVisible = () => false; showBall()');
  const win = f.windows[0]; f.run('window.isVisible = () => true'); await win.loaded();
  assert.equal(win.showCount, 0, 'visibility is checked again when loading completes');
  f.run('window.isVisible = () => false; showBall()');
  assert.equal(win.showCount, 1);
});

test('a failed floating-page load stays hidden and retries the correct page without an unhandled rejection', async () => {
  const f = mainFixture(); f.run('showBall()'); const win = f.windows[0];
  win.failed(); await Promise.resolve();
  win.webContents.emit('did-finish-load');
  assert.equal(win.visible, false, 'Chromium finishing an error document is not a usable ball');
  assert.equal(win.showCount, 0);
  assert.equal(f.timers.size, 1, 'the load event and Promise rejection schedule only one retry');
  assert.equal(f.errors.length, 1);
  f.runNextTimer(); assert.equal(win.loads.length, 2); assert.equal(win.loads.at(-1).url, origin + 'ball.html');
  await win.loaded(); assert.equal(win.visible, true); assert.equal(win.showCount, 1);
});

test('native finish and stop-loading events alone cannot reveal an unconfirmed page', async () => {
  const f = mainFixture(); f.run('showBall()'); const win = f.windows[0];
  win.loading = true; win.webContents.emit('did-finish-load');
  assert.equal(win.visible, false, 'real Electron can still report loading inside did-finish-load');
  win.loading = false; win.webContents.emit('did-stop-loading');
  assert.equal(win.visible, false, 'an error page emits these events too');
  win.loads[0].resolve(); await Promise.resolve();
  assert.equal(win.visible, true); assert.equal(win.showCount, 1);
});

test('a superseded load cannot reveal, fail or schedule a retry for the current page', async () => {
  for (const outcome of ['resolve', 'reject']) {
    const f = mainFixture(); f.run('showBall()'); const win = f.windows[0], old = win.loads[0];
    f.run('reloadBall()');
    if (outcome === 'resolve') old.resolve(); else old.reject(new Error('superseded navigation'));
    await Promise.resolve(); await Promise.resolve();
    assert.equal(win.visible, false, outcome); assert.equal(f.errors.length, 0, outcome); assert.equal(f.timers.size, 0, outcome);
    await win.loaded(); assert.equal(win.visible, true, outcome); assert.equal(win.showCount, 1, outcome);
  }
});

test('a crashed visible renderer reloads without restoring the main window or losing the ball position', async () => {
  const f = mainFixture('{"x":100,"y":200}'); f.run('showBall()'); const win = f.windows[0]; await win.loaded();
  const host = connectPreload(win, f.ipcMain); host.dragStart();
  f.screen.cursor = { x: 350, y: 355 }; host.drag();
  win.webContents.emit('render-process-gone', {}, { reason: 'crashed', exitCode: 1 });
  assert.equal(win.visible, false);
  assert.deepEqual(win.mouseCalls.at(-1), [true, { forward: true }]);
  f.runNextTimer(); assert.equal(win.loads.length, 2); await win.loaded();
  assert.equal(win.visible, true); assert.equal(win.showCount, 2);
  assert.deepEqual(win.getBounds(), { x: 150, y: 250, width: 400, height: 210 });
  assert.deepEqual(f.calls, []);
});

test('hide, disable, quit, main-window opening and replacement cancel stale floating-page recovery', async () => {
  for (const cancel of ['hideBall()', 'setBallEnabled(false)', 'quitting = true', 'window.isVisible = () => true', 'ball.close()']) {
    const f = mainFixture(); f.run('showBall()'); const win = f.windows[0];
    win.failed(); await Promise.resolve(); assert.equal(f.timers.size, 1);
    f.run(cancel); f.runNextTimer();
    assert.equal(win.loads.length, 1, cancel); assert.equal(win.showCount, 0, cancel);
  }
});

test('repeated failures stop automatic retries but a later explicit show can load again', async () => {
  const f = mainFixture(); f.run('showBall()'); const win = f.windows[0];
  for (let attempt = 0; attempt < 4; attempt++) {
    win.failed(); await Promise.resolve();
    if (attempt < 3) { assert.equal(f.timers.size, 1); f.runNextTimer(); }
  }
  assert.equal(win.loads.length, 4); assert.equal(f.timers.size, 0); assert.equal(win.visible, false);
  f.run('showBall()'); assert.equal(win.loads.length, 5); await win.loaded();
  assert.equal(win.visible, true); assert.equal(win.showCount, 1);
});

test('reloading a hidden ball does not reopen it and repeated loading shows do not stack callbacks', async () => {
  const f = mainFixture(); f.run('showBall()'); const win = f.windows[0];
  for (let i = 0; i < 40; i++) f.run('showBall()');
  assert.ok(win.webContents.listenerCount('did-finish-load') <= 2, 'one lifecycle observer and one reveal observer at most');
  await win.loaded(); assert.equal(win.showCount, 1);
  f.run('hideBall(); reloadBall()'); await win.loaded(); assert.equal(win.visible, false); assert.equal(win.showCount, 1);
  f.run('showBall()'); assert.equal(win.visible, true); assert.equal(win.showCount, 2);
});

test('actual main deserializes malformed remembered state without reading any user files', async () => {
  for (const state of ['null', '[]', '"text"', '3', '{broken', '{"x":null,"y":false}', '{"x":"1","y":9000}']) {
    const f = mainFixture(state);
    assert.deepEqual(plain(f.run('ballBounds()')), { x: 1624, y: 785, width: 400, height: 210 }, state);
    f.run('showBall()'); await f.windows[0].loaded(); assert.equal(f.windows[0].visible, true, state);
  }
});

test('side content flips inward at either display edge without changing the saved horizontal docking distance', () => {
  const screen = fakeScreen();
  for (const display of screen.displays) {
    const area = display.workArea;
    for (const edge of ['left','right']) for (const vertical of ['top','bottom']) {
      const bounds = clampBallBounds({x: edge === 'left' ? area.x - 199 : area.x + area.width - 201,
        y: vertical === 'top' ? area.y - 104 : area.y + area.height - 106,width:400,height:210},screen);
      const layout = ballLayout(bounds,screen), cx=bounds.x+200;
      assert.equal(layout.side,edge === 'left' ? 'right':'left');
      assert.equal(layout.maxWidth,174); assert.equal(layout.bubbleMaxWidth,148);
      assert.equal(edge === 'left' ? cx-area.x : area.x+area.width-cx,96);
      const left = layout.side === 'right' ? cx+18 : cx-18-layout.maxWidth;
      assert.ok(left>=area.x && left+layout.maxWidth<=area.x+area.width,'plugin picker remains in the chosen work area');
      assert.ok(bounds.y+8>=area.y && bounds.y+202<=area.y+area.height,'the full picker height is visible');
      const bubbleLeft = layout.side === 'right' ? cx+44 : cx-44-layout.bubbleMaxWidth;
      assert.ok(bubbleLeft>=area.x && bubbleLeft+layout.bubbleMaxWidth<=area.x+area.width,'usage bubble remains in the chosen work area');
    }
  }
});

test('native layout is replayed on reveal, drag completion and display topology changes', () => {
  const f=fixture();
  f.win.setBounds({x:1624,y:785,width:400,height:210});
  f.api.reset(f.win,true);
  assert.deepEqual(f.win.sent.at(-1),['ball:layout',{side:'left',maxWidth:174,bubbleMaxWidth:148}]);
  f.screen.cursor={x:1824,y:890}; f.host.dragStart();
  f.screen.cursor={x:96,y:300}; f.host.drag(); f.host.dragEnd();
  const layouts=f.win.sent.filter(([type])=>type==='ball:layout');
  assert.deepEqual(layouts.at(-1)[1],{side:'right',maxWidth:174,bubbleMaxWidth:148});
  const count=layouts.length;
  f.screen.emit('display-added');
  assert.equal(f.win.sent.filter(([type])=>type==='ball:layout').length,count+1);
  assert.deepEqual(f.win.sent.at(-1),['ball:shown',true]);
});

test('tiny work areas reduce side content width without scaling DIP a second time', () => {
  const screen=fakeScreen();
  screen.displays=[{workArea:{x:-50,y:-20,width:100,height:100},scaleFactor:3}];
  const bounds=restoredBallBounds(null,screen,400,210), layout=ballLayout(bounds,screen);
  assert.deepEqual(layout,{side:'right',maxWidth:24,bubbleMaxWidth:0});
  screen.displays[0].scaleFactor=1;
  assert.deepEqual(ballLayout(bounds,screen),layout,'Electron work areas already use DIP');
});

test('cancelling a click before movement synchronizes native click-through with the renderer cache', () => {
  const f=fixture(), received=[];
  let cached=true;
  f.host.onDragFinished(on=>{received.push(on);cached=on;f.host.setInteractive(on);});
  f.host.setInteractive(true); f.host.dragStart(); f.host.dragCancel();
  assert.deepEqual(received,[false]);
  assert.equal(cached,false);
  assert.deepEqual(f.win.mouseCalls.at(-1),[true,{forward:true}]);
  // A subsequent mousemove over the orb is now different from the cache and
  // can restore capture without an artificial move out of the whole window.
  if (!cached) { cached=true; f.host.setInteractive(true); }
  assert.deepEqual(f.win.mouseCalls.at(-1),[false]);
  assert.deepEqual(f.saved,[],'cancelled clicks must not rewrite the position');
  f.host.dragCancel(); assert.deepEqual(received,[false],'late duplicate cancellation has no gesture to acknowledge');
});

test('a superseded native navigation failure cannot poison a newer loading or ready page', async () => {
  for (const ready of [false,true]) {
    const f=mainFixture(); f.run('showBall()'); const win=f.windows[0], old=win.loads[0];
    f.run('reloadBall()');
    if (ready) await win.loaded();
    win.webContents.emit('did-fail-load',{},-102,'ERR_CONNECTION_REFUSED',origin+'ball.html',true);
    old.reject(Object.assign(new Error('superseded load failed'),{code:'ERR_CONNECTION_REFUSED'}));
    await Promise.resolve(); await Promise.resolve();
    assert.equal(f.run('ballPage.state'),ready?'ready':'loading');
    assert.equal(f.errors.length,0); assert.equal(f.timers.size,0);
    assert.equal(win.visible,ready);
    if (!ready) { await win.loaded(); assert.equal(win.visible,true); }
    assert.equal(win.showCount,1);
  }
});

test('the current load rejection still starts bounded recovery without relying on native load events', async () => {
  const f=mainFixture(); f.run('showBall()'); const win=f.windows[0];
  win.loading=false;
  win.loads[0].reject(Object.assign(new Error('ERR_CONNECTION_REFUSED'),{code:'ERR_CONNECTION_REFUSED'}));
  await Promise.resolve();
  assert.equal(f.run('ballPage.state'),'failed'); assert.equal(win.visible,false);
  assert.equal(f.errors.length,1); assert.equal(f.timers.size,1);
  f.runNextTimer(); await win.loaded();
  assert.equal(f.run('ballPage.state'),'ready'); assert.equal(win.visible,true);
});

test('unresponsive renderers release click capture and use the same finite recovery budget as crashes', async () => {
  const f=mainFixture('{"x":100,"y":200}'); f.run('showBall()'); const win=f.windows[0]; await win.loaded();
  const host=connectPreload(win,f.ipcMain); host.setInteractive(true);
  for (let attempt=0;attempt<4;attempt++) {
    win.emit('unresponsive'); win.emit('unresponsive');
    assert.equal(win.visible,false); assert.deepEqual(win.mouseCalls.at(-1),[true,{forward:true}]);
    assert.equal(f.errors.length,attempt+1,'duplicate native signals do not double-count a failure');
    if (attempt<3) { assert.equal(f.timers.size,1); f.runNextTimer(); await win.loaded(); assert.equal(win.visible,true); }
  }
  assert.equal(f.timers.size,0); assert.equal(win.loads.length,4); assert.deepEqual(f.calls,[]);
});
