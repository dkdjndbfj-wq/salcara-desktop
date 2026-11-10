'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const { EventEmitter } = require('node:events');
const { createCoreLifecycle } = require('../core-lifecycle.cjs');

function fixture(options = {}) {
  let time = 0, healthy = false, stopping = false, child = null, attached = false;
  const children = [], failures = [], ready = [], timers = new Set(), probes = [];
  const f = {
    children, failures, ready, timers, probes,
    get child() { return child; }, get attached() { return attached; }, get time() { return time; },
    set healthy(value) { healthy = value; }, set stopping(value) { stopping = value; },
    advance(ms) { time += ms; },
    exit(owner = child) { healthy = false; owner.exitCode = 1; owner.emit('exit', 1, null); },
    async tick() { const next = timers.values().next().value; assert.ok(next); timers.delete(next); next.fn(); await f.core.start({ watchOnly: true }); },
  };
  f.core = createCoreLifecycle({
    now: () => time, isStopping: () => stopping,
    healthy: async timeout => { probes.push(timeout); return options.probe ? options.probe(f, timeout) : healthy; },
    launch() {
      if (options.launchError) throw options.launchError;
      const owner = new EventEmitter(); owner.pid = 100 + children.length; owner.exitCode = null;
      owner.kill = () => { throw new Error('A retry must never kill a live core'); };
      children.push(owner);
      if (options.launch) options.launch(f, owner); else healthy = true;
      return owner;
    },
    onOwner(owner, external) { child = owner; attached = external; },
    async onReady(value) { ready.push(value); if (options.ready) await options.ready(f, value); },
    async onFailure(cause) { failures.push(cause); if (options.failure) await options.failure(f, cause); },
    async sleep(ms) { time += ms; if (options.sleep) await options.sleep(f, ms); },
    schedule(fn) { const timer = { fn, unref() {} }; timers.add(timer); return timer; },
    unschedule(timer) { timers.delete(timer); },
  });
  return f;
}

test('first start launches one bundled core and completes readiness once', async () => {
  const f = fixture();
  const start = f.core.start({ initial: true });
  assert.equal(f.core.start({ manual: true }), start);
  assert.equal(f.core.start(), start);
  assert.equal(await start, true);
  assert.equal(f.children.length, 1); assert.equal(f.child, f.children[0]); assert.equal(f.attached, false);
  assert.equal(f.ready.length, 1); assert.equal(f.ready[0].initial, true); assert.equal(f.failures.length, 0);
});

test('an existing core stays externally owned; healthy polls do not reload the page', async () => {
  const f = fixture(); f.healthy = true;
  assert.equal(await f.core.start(), true);
  assert.equal(f.children.length, 0); assert.equal(f.attached, true);
  await f.tick(); assert.equal(f.ready.length, 1); assert.equal(f.children.length, 0);
  f.healthy = false;
  await f.tick();
  assert.equal(f.children.length, 1); assert.equal(f.attached, false); assert.equal(f.ready.length, 2);
  assert.equal(f.ready[1].recovered, true); assert.equal(f.timers.size, 0);
});

test('synchronous missing-file failure is retryable, including after a repaired installation', async () => {
  const options = { launchError: Object.assign(new Error('missing'), { code: 'ENOENT' }) };
  const f = fixture(options);
  assert.equal(await f.core.start({ initial: true }), false);
  assert.equal(await f.core.start({ manual: true }), false);
  assert.deepEqual(f.failures.map(e => e.code), ['ENOENT', 'ENOENT']);
  delete options.launchError;
  assert.equal(await f.core.start({ manual: true }), true);
  assert.equal(f.children.length, 1); assert.equal(f.ready.length, 1);
});

for (const code of ['ENOENT', 'EACCES', 'EPERM']) {
  test(`asynchronous spawn ${code} is reported immediately and never starts a retry loop`, async () => {
    const f = fixture({ launch(_f, owner) {
      owner.pid = undefined;
      queueMicrotask(() => owner.emit('error', Object.assign(new Error('spawn fixture'), { code })));
    } });
    assert.equal(await f.core.start(), false);
    assert.equal(f.time, 0); assert.equal(f.children.length, 1); assert.equal(f.child, null);
    assert.equal(f.failures[0].code, code);
    assert.equal(await f.core.start({ manual: true }), false);
    assert.equal(f.children.length, 2); assert.equal(f.failures.length, 2);
  });
}

test('slow probes honor a wall-clock deadline; retry waits for the same still-live child', async () => {
  let available = false;
  const f = fixture({ launch() {}, probe(f, timeout) { if (!available) f.advance(timeout); return available; } });
  assert.equal(await f.core.start(), false);
  assert.equal(f.failures[0].code, 'CORE_TIMEOUT'); assert.ok(f.time <= 21500);
  const first = f.child;
  assert.equal(await f.core.start({ manual: true }), false);
  assert.equal(f.children.length, 1); assert.equal(f.child, first);
  available = true;
  assert.equal(await f.core.start({ manual: true }), true);
  assert.equal(f.children.length, 1); assert.equal(f.ready.length, 1);
});

test('a repeated crash is bounded and a manual retry can recover afterwards', async () => {
  let crash = true;
  const f = fixture({ launch(f, owner) {
    if (crash) queueMicrotask(() => f.exit(owner)); else f.healthy = true;
  } });
  assert.equal(await f.core.start(), false);
  assert.equal(f.children.length, 5); assert.equal(f.failures.at(-1).code, 'CORE_CRASH_LOOP');
  assert.equal(f.ready.length, 0);
  crash = false;
  assert.equal(await f.core.start({ manual: true }), true);
  assert.equal(f.children.length, 6);
});

test('old child error/exit events cannot retire a replacement or spawn another core', async () => {
  let failFirst = true;
  const f = fixture({ launch(f, owner) {
    if (failFirst) { owner.pid = undefined; queueMicrotask(() => owner.emit('error', Object.assign(new Error('fixture'), { code: 'ENOENT' }))); }
    else f.healthy = true;
  } });
  await f.core.start(); const old = f.children[0]; failFirst = false;
  await f.core.start({ manual: true }); const current = f.child;
  old.emit('exit', 1, null);
  assert.doesNotThrow(() => old.emit('error', new Error('late retired child error')));
  assert.equal(f.child, current); assert.equal(f.children.length, 2); assert.equal(f.ready.length, 1);
});

test('multiple child errors are consumed without an unhandled error or a duplicate process', async () => {
  const f = fixture(); await f.core.start(); const owner = f.child;
  assert.doesNotThrow(() => {
    owner.emit('error', Object.assign(new Error('first operation'), { code: 'EPERM' }));
    owner.emit('error', Object.assign(new Error('second operation'), { code: 'EPERM' }));
  });
  await f.core.start();
  assert.equal(f.children.length, 1); assert.equal(f.child, owner);
});

test('an exit while the timeout page is loading is recovered after that page settles', async () => {
  let resolvePage, enteredPage;
  const showingPage = new Promise(resolve => { enteredPage = resolve; });
  const f = fixture({ launch(f) { if (f.children.length > 1) f.healthy = true; },
    failure() { enteredPage(); return new Promise(resolve => { resolvePage = resolve; }); } });
  const first = f.core.start(); await showingPage;
  f.exit(); resolvePage();
  assert.equal(await first, false);
  assert.equal(await f.core.start(), true);
  assert.equal(f.children.length, 2); assert.equal(f.ready.length, 1);
});

test('quit or update pause during a probe prevents a late spawn and page completion', async () => {
  let resolve;
  const f = fixture({ probe: () => new Promise(done => { resolve = done; }) });
  const started = f.core.start(); await Promise.resolve();
  f.core.pause(); f.stopping = true; resolve(false);
  assert.equal(await started, false);
  assert.equal(f.children.length, 0); assert.equal(f.ready.length, 0); assert.equal(f.failures.length, 0);
});

test('update cancellation starts a fresh attempt only after the cancelled probe settles', async () => {
  let resolve, first = true;
  const f = fixture({ probe: () => first ? new Promise(done => { resolve = done; }) : true });
  const cancelled = f.core.start(); await Promise.resolve(); f.core.pause();
  const resumed = f.core.start({ manual: true });
  first = false; resolve(false);
  assert.equal(await cancelled, false); assert.equal(await resumed, true);
  assert.equal(f.children.length, 0); assert.equal(f.ready.length, 1); assert.equal(f.attached, true);
});

test('quit during crash backoff does not resurrect the core', async () => {
  const f = fixture({ launch(f, owner) { queueMicrotask(() => f.exit(owner)); }, sleep(f) { f.stopping = true; f.core.pause(); } });
  assert.equal(await f.core.start(), false);
  assert.equal(f.children.length, 1); assert.equal(f.failures.length, 0);
});

test('an exit during page hand-over invalidates that completion and recovers once', async () => {
  let once = true;
  const f = fixture({ ready(f, ready) { if (once) { once = false; f.exit(); assert.equal(ready.current(), false); } } });
  assert.equal(await f.core.start(), true);
  assert.equal(f.children.length, 2); assert.equal(f.ready.length, 2);
});

test('an automatic recovery and simultaneous manual retries share one replacement', async () => {
  const f = fixture(); await f.core.start();
  f.exit();
  const recovery = f.core.start({ manual: true });
  assert.equal(f.core.start({ manual: true }), recovery);
  assert.equal(await recovery, true);
  assert.equal(f.children.length, 2); assert.equal(f.ready.length, 2);
});

test('page loading failure can be retried without replacing a healthy core', async () => {
  let fail = true;
  const f = fixture({ ready() { if (fail) throw new Error('navigation failed'); } });
  assert.equal(await f.core.start(), false); assert.equal(f.failures.length, 1);
  fail = false;
  assert.equal(await f.core.start({ manual: true }), true); assert.equal(f.children.length, 1);
});

test('pausing an attached core cancels its watch without killing somebody else’s process', async () => {
  const f = fixture(); f.healthy = true; await f.core.start();
  assert.equal(f.timers.size, 1); f.core.pause();
  assert.equal(f.timers.size, 0); assert.equal(f.child, null); assert.equal(f.children.length, 0);
});
