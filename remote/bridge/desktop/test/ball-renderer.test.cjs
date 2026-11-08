'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const source = fs.readFileSync(path.resolve(__dirname, '../../internal/console/web/ball.js'), 'utf8');
const flush = async () => { for (let i = 0; i < 20; i++) await Promise.resolve(); };

function fixture({ reduced = false } = {}) {
  let now = 1000000, next = 0;
  const timers = new Map(), frames = new Map(), calls = [], streams = [], reads = [];
  const eventTarget = object => Object.assign(object, {
    listeners: new Map(),
    addEventListener(name, fn) { const list = this.listeners.get(name) || []; list.push(fn); this.listeners.set(name, list); },
    dispatchEvent(event) { for (const fn of this.listeners.get(event.type) || []) fn(event); },
  });
  const element = (classes = []) => {
    const set = new Set(classes), style = {};
    return eventTarget({
      dataset: {}, style: { setProperty: (k, v) => { style[k] = v; } }, hidden: false,
      classList: { add: (...c) => c.forEach(v => set.add(v)), remove: (...c) => c.forEach(v => set.delete(v)), contains: c => set.has(c), toggle: (c, on) => { on ??= !set.has(c); if (on) set.add(c); else set.delete(c); return on; } },
      closest: selector => selector === '.sats button' && set.has('sat') || selector.startsWith('.') && set.has(selector.slice(1)) ? true : null,
      getBoundingClientRect: () => ({ left: 168, top: 73, width: 64, height: 64 }),
      setAttribute() {}, contains: () => false, insertBefore() {}, remove() {},
      setPointerCapture() {}, hasPointerCapture: () => false, releasePointerCapture() {},
      animate: () => ({}), focus() {},
    });
  };
  const elements = Object.fromEntries(['float', 'ball', 'face', 'bubble', 'badge', 'sats', 'bTok', 'bSub', 'bName'].map(id => [id, element([id])]));
  const sat = element(['sat']); let hit = null;
  const document = eventTarget({ hidden: false, visibilityState: 'visible', activeElement: null,
    querySelector: selector => elements[selector.slice(1)], querySelectorAll: selector => selector === '#sats button' ? [sat] : [],
    elementFromPoint: () => hit, createElement: () => element(),
  });
  const shown = [], layouts = [];
  const window = eventTarget({ salcaraBall: {
    setInteractive: on => calls.push(['interactive', on]), dragStart: () => calls.push(['start']), dragCancel: () => calls.push(['cancel']), dragEnd: () => calls.push(['end']), drag() {}, show() {},
    onShown: fn => shown.push(fn), onLayout: fn => layouts.push(fn), onDragFinished() {},
  }, screenX: 0 });
  class EventSource {
    static CLOSED = 2;
    constructor() { eventTarget(this); this.readyState = 0; streams.push(this); }
    close() { this.closed = true; this.readyState = 2; }
    emit(event) { this.dispatchEvent({ type: 'event', data: JSON.stringify(event) }); }
  }
  const schedule = (fn, delay, repeat = false) => { const id = ++next; timers.set(id, { fn, at: now + delay, delay, repeat }); return id; };
  let fetcher = async url => ({ ok: true, status: 200, json: async () => url === '/api/activity' ? { revision: 0, sessions: [], approvalIds: [] } : { accounts: [], tools: [], bindings: {} } });
  const context = vm.createContext({ window, document, EventSource, AbortController,
    Date: class extends Date { static now() { return now; } }, Math, Event,
    matchMedia: () => ({ matches: reduced }), screen: { availLeft: 0, availWidth: 1920 }, innerWidth: 400,
    addEventListener: window.addEventListener.bind(window),
    setTimeout: (fn, ms) => schedule(fn, ms), clearTimeout: id => timers.delete(id),
    setInterval: (fn, ms) => schedule(fn, ms, true), clearInterval: id => timers.delete(id),
    requestAnimationFrame: fn => { const id = ++next; frames.set(id, fn); return id; }, cancelAnimationFrame: id => frames.delete(id),
    fetch: (url, options) => { reads.push(url); return fetcher(url, options); },
  });
  vm.runInContext(source, context, { filename: 'ball.js' });
  return { context, elements, document, window, calls, streams, reads, frames, shown, layouts, sat,
    run: code => vm.runInContext(code, context),
    setFetch: fn => { fetcher = fn; }, hit: target => { hit = target; },
    pointer: target => { hit = target; window.dispatchEvent({ type: 'mousemove', clientX: 220, clientY: 105 }); },
    visible(on) { document.hidden = !on; document.visibilityState = on ? 'visible' : 'hidden'; document.dispatchEvent({ type: 'visibilitychange' }); },
    async advance(ms) {
      const end = now + ms;
      for (;;) {
        const candidate = [...timers].filter(([, t]) => t.at <= end).sort((a, b) => a[1].at - b[1].at)[0];
        if (!candidate) break;
        const [id, timer] = candidate; now = timer.at; timers.delete(id);
        if (timer.repeat) timers.set(id, { ...timer, at: now + timer.delay });
        timer.fn(); await flush();
      }
      now = end; await flush();
    },
  };
}

test('short network errors do not change expression; sustained errors recover independently', async () => {
  const f = fixture(); await flush();
  f.run("setBallError('stream', true)"); await f.advance(4000);
  assert.equal(f.run('STATE.error'), false);
  f.run("setBallError('stream', false)"); await f.advance(5000);
  assert.equal(f.run('STATE.error'), false);
  f.run("setBallError('stream', true)"); await f.advance(8000);
  assert.equal(f.run('STATE.error'), true);
  f.run("setBallError('task', true); setBallError('stream', false)");
  assert.equal(f.run('STATE.error'), true);
  f.run("setBallError('task', false)"); assert.equal(f.run('STATE.error'), false);
});

test('closing menu or hover bubble restores click-through without moving the pointer', async () => {
  const f = fixture(); await flush();
  f.run('setOpen(true)'); f.pointer(f.sat); assert.deepEqual(f.calls.at(-1), ['interactive', true]);
  f.run('setOpen(false)'); assert.deepEqual(f.calls.at(-1), ['interactive', false]);
  f.run("float.classList.add('peek')"); f.pointer(f.elements.bubble); assert.deepEqual(f.calls.at(-1), ['interactive', true]);
  f.run('hidePeekSoon()'); await f.advance(150); assert.deepEqual(f.calls.at(-1), ['interactive', false]);
});

test('canceling a drag before movement synchronizes the renderer hit cache', async () => {
  const f = fixture(); await flush(); f.pointer(f.elements.ball);
  f.elements.ball.dispatchEvent({ type: 'pointerdown', button: 0, pointerId: 7, screenX: 200, screenY: 105 });
  f.elements.ball.dispatchEvent({ type: 'pointercancel', pointerId: 7 });
  assert.equal(f.run('drag.active'), false); assert.deepEqual(f.calls.at(-1), ['interactive', false]);
  f.pointer(f.elements.ball); assert.deepEqual(f.calls.at(-1), ['interactive', true]);
});

test('eyes stop requesting frames at rest and stay paused when hidden or motion is reduced', async () => {
  const f = fixture(); await flush(); f.pointer(f.elements.ball);
  let count = 0;
  while (f.frames.size && count++ < 200) { const batch = [...f.frames.values()]; f.frames.clear(); batch.forEach(fn => fn()); }
  assert.ok(count < 200); assert.equal(f.frames.size, 0);
  f.visible(false); f.pointer(f.elements.ball); assert.equal(f.frames.size, 0);
  const reduced = fixture({ reduced: true }); reduced.pointer(reduced.elements.ball); assert.equal(reduced.frames.size, 0);
});

test('hide/show owns one stream and rejects queued callbacks from the previous connection', async () => {
  const f = fixture(); await flush(); const first = f.streams[0];
  f.run('stream(); stream()'); assert.equal(f.streams.length, 1);
  f.visible(false); assert.equal(first.closed, true);
  first.emit({ type: 'approval.request', activityRevision: 100, approvalId: 'old' }); first.onerror();
  assert.equal(f.run('STATE.approvals'), 0); assert.equal(f.run('ballErrorTimers.size'), 0);
  f.visible(true); f.shown[0](true); await flush(); assert.equal(f.streams.length, 2);
  first.onopen(); assert.equal(f.run('ballStream === null'), false);
  const second = f.streams[1]; second.onopen(); await flush();
  second.emit({ type: 'approval.request', activityRevision: 1, approvalId: 'new' });
  assert.equal(f.run('STATE.approvals'), 1);
});

test('permanently closed EventSource renews the cookie and reconnects with bounded delay', async () => {
  const f = fixture(); await flush(); const first = f.streams[0];
  first.readyState = 2; first.onerror(); first.onerror();
  await f.advance(2999); assert.equal(f.streams.length, 1);
  await f.advance(1); assert.equal(f.reads.filter(url => url === '/').length, 1); assert.equal(f.streams.length, 2);
  f.streams[1].onopen(); await flush(); assert.equal(f.run('ballStreamBackoff'), 3000);
  assert.equal(f.run("ballErrorTimers.has('stream')"), false);
});

test('concurrent unauthorized reads share renewal and each request retries only once', async () => {
  const f = fixture(); await flush(); let roots = 0; const counts = new Map();
  f.setFetch(async url => {
    if (url === '/') { roots++; await Promise.resolve(); return { ok: true }; }
    counts.set(url, (counts.get(url) || 0) + 1);
    return { ok: false, status: 401 };
  });
  await f.run("Promise.all([ballFetch('/one', {}), ballFetch('/two', {})])");
  assert.equal(roots, 1); assert.equal(counts.get('/one'), 2); assert.equal(counts.get('/two'), 2);
  await f.run("ballFetch('/three', {})"); assert.equal(roots, 1); assert.equal(counts.get('/three'), 1);
});

test('hiding during cookie renewal aborts recovery and never revives the old stream', async () => {
  const f = fixture(); await flush(); let pending;
  f.setFetch((url, options) => url === '/' ? new Promise(resolve => { pending = { resolve, signal: options.signal }; }) : Promise.resolve({ ok: true, json: async () => ({ accounts: [] }) }));
  f.streams[0].readyState = 2; f.streams[0].onerror(); await f.advance(3000);
  f.visible(false); assert.equal(pending.signal.aborted, true);
  pending.resolve({ ok: true }); await flush();
  assert.equal(f.streams.length, 1); assert.equal(f.run('BALL_AUTH.epoch'), 0); assert.equal(f.run('ballStream'), null);
});
