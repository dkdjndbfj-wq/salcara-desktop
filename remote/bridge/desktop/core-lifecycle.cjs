'use strict';

// One owner for first start, manual retry and background recovery. A health
// timeout is not evidence that a live child exited, so it must never spawn a
// replacement over that child or kill it while it may be doing work.
function createCoreLifecycle({ launch, healthy, isStopping, onOwner, onReady, onFailure,
  now = Date.now, sleep = ms => new Promise(resolve => setTimeout(resolve, ms)),
  schedule = setTimeout, unschedule = clearTimeout, readyTimeout = 20000, watchInterval = 5000 }) {
  let child = null, active = null, failure = null, revision = 0, epoch = 0;
  let activeEpoch = 0, handledRevision = 0, watch = null, restarts = [], wasReady = false;
  const running = token => token === epoch && !isStopping();
  const clearWatch = () => { if (watch !== null) unschedule(watch); watch = null; };
  const error = (code, message) => Object.assign(new Error(message), { code });

  function failed(owner, cause, exited) {
    if (child !== owner) return; // A retired child's late event has no authority.
    if (exited || !owner.pid) { child = null; onOwner(null, false); }
    failure = { cause, retry: exited };
    revision++;
    if (!active && !isStopping()) void start();
  }

  function spawn() {
    if (child) return child;
    failure = null;
    const owner = launch();
    child = owner; revision++; onOwner(owner, false);
    // A ChildProcess can emit another error (for example a failed kill).
    // Retired children still need an error listener, but may not change state.
    owner.on('error', cause => failed(owner, cause, false));
    owner.once('exit', (code, signal) => failed(owner,
      error('CORE_EXITED', `Core exited (${code ?? signal ?? 'unknown'})`), true));
    return owner;
  }

  async function waitReady(token) {
    const deadline = now() + readyTimeout;
    while (running(token) && !failure && now() < deadline) {
      const observed = revision;
      const ok = await healthy(Math.max(1, Math.min(1500, deadline - now())));
      if (!running(token) || failure) return false;
      if (ok && observed === revision) return true;
      if (now() < deadline) await sleep(Math.min(250, deadline - now()));
    }
    return false;
  }

  function watchAttached() {
    clearWatch();
    watch = schedule(() => {
      watch = null;
      if (!isStopping()) void start({ watchOnly: true });
    }, watchInterval);
    watch.unref?.();
  }

  async function run(token, { initial = false, watchOnly = false } = {}) {
    let recovered = false;
    try {
      while (running(token)) {
        const observed = revision;
        const alreadyHealthy = await healthy(1500);
        if (!running(token)) return false;
        if (observed !== revision) continue;
        if (!alreadyHealthy) {
          if (failure && !failure.retry) throw failure.cause;
          if (failure?.retry) {
            restarts = restarts.filter(t => now() - t < 120000);
            if (restarts.length >= 4) throw error('CORE_CRASH_LOOP', 'Core repeatedly exited');
            restarts.push(now());
            await sleep(800 * restarts.length);
            if (!running(token)) return false;
          }
          spawn();
          recovered = true;
          if (!(await waitReady(token))) {
            if (!running(token)) return false;
            if (failure?.retry) continue;
            throw failure?.cause || error('CORE_TIMEOUT', 'Core readiness timed out');
          }
        }
        if (!running(token)) return false;
        failure = null;
        onOwner(child, !child);
        const readyRevision = revision;
        const current = () => running(token) && readyRevision === revision && !failure;
        // A successful attached-core poll must not reload the user's page.
        if (!watchOnly || recovered) await onReady({ initial, recovered: wasReady && recovered, current });
        if (!running(token)) return false;
        if (!current()) continue;
        handledRevision = revision; wasReady = true;
        if (!child) watchAttached();
        return true;
      }
    } catch (cause) {
      if (running(token)) {
        handledRevision = revision;
        try { await onFailure(cause); } catch { /* keep the shell alive if its page is closing */ }
      }
    }
    return false;
  }

  function start(options = {}) {
    if (isStopping()) return Promise.resolve(false);
    if (active) {
      if (activeEpoch !== epoch) return active.then(() => start(options));
      return active;
    }
    clearWatch();
    if (options.manual) { restarts = []; failure = null; }
    activeEpoch = epoch;
    const token = activeEpoch;
    // Publish the single flight before launch/probe callbacks can emit events.
    const operation = Promise.resolve().then(() => run(token, options));
    active = operation.finally(() => {
      active = null;
      // An exit can arrive while the failure page is loading. Recover that new
      // event after releasing this operation, without reopening a crash loop
      // whose final exit has already been handled by run().
      if (running(token) && failure?.retry && revision > handledRevision) void start();
    });
    return active;
  }

  return {
    start,
    // Update commit / quit cancels pending probes and delayed restarts. The
    // caller retains responsibility for safely stopping its exact owned child.
    pause() { epoch++; clearWatch(); },
  };
}

module.exports = { createCoreLifecycle };
