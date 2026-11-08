'use strict';
/* Floating ball shown while the main window is closed to the background.
   Click: satellites (quick entries) appear around it. Hover: today's token usage
   beside it. Drag: move it anywhere. The face reacts to what the Bridge is doing. */

const host = window.salcaraBall || null; // Electron preload; absent in a plain browser
const $ = (s) => document.querySelector(s);
const ICON = {
  home: '<path d="M4.5 10.5 12 4.5l7.5 6V18a2 2 0 0 1-2 2h-11a2 2 0 0 1-2-2z"/><path d="M10 20v-5h4v5"/>',
  plug: '<path d="M9 4v4M15 4v4M6.5 8h11v3a5.5 5.5 0 0 1-11 0zM12 16.5V20"/>',
  key: '<circle cx="8.5" cy="15.5" r="4"/><path d="M11.5 12.5 19.5 4.5M16.5 7.5l2.5 2.5"/>',
  mcp: '<circle cx="6" cy="12" r="2.3"/><circle cx="18" cy="6" r="2.3"/><circle cx="18" cy="18" r="2.3"/><path d="M8.1 11 15.9 7M8.1 13l7.8 4"/>',
  phone: '<path d="M8.5 3.5h7a2 2 0 0 1 2 2v13a2 2 0 0 1-2 2h-7a2 2 0 0 1-2-2v-13a2 2 0 0 1 2-2zM11 17.5h2"/>',
  chart: '<path d="M4 19.5h16M5.5 15.5l4.5-5 3 2.5 4-5"/>',
  gear: '<path d="M4 8h8.5M18.5 8H20M4 16h1.5M11.5 16H20"/><circle cx="15.5" cy="8" r="2.5"/><circle cx="8.5" cy="16" r="2.5"/>',
};
document.querySelectorAll('[data-i]').forEach((el) => {
  el.innerHTML = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round">${ICON[el.dataset.i] || ""}</svg>`;
});

const float = $('#float'), ball = $('#ball'), face = $('#face'), bubble = $('#bubble');
const reduceMotion = matchMedia('(prefers-reduced-motion: reduce)').matches;
const sats = [...document.querySelectorAll('#sats button')];
sats.forEach((b, i) => {
  const a = (360 / sats.length) * i;
  b.style.setProperty('--a', `${a}deg`);
  b.style.setProperty('--i', i);
  b.dataset.side = a > 180 ? 'l' : 'r';
});

/* ---------- mood ---------- */
const drag = { active: false, moved: false, x: 0, y: 0, pointerId: null };
const STATE = { burnUntil: 0, hotUntil: 0, streamAt: [], fire: { lit: false, litAt: 0, activeSince: 0, idleSince: 0 }, lastTokens: null, approvals: 0, running: new Set(), error: false, lastActive: Date.now(), hover: false, open: false };
const ballErrors = new Set(), ballErrorTimers = new Map();
function setBallError(source, failed) {
  if (!failed) {
    clearTimeout(ballErrorTimers.get(source)); ballErrorTimers.delete(source); ballErrors.delete(source);
  } else if (['stream', 'activity', 'usage'].includes(source)) {
    if (!ballErrors.has(source) && !ballErrorTimers.has(source)) ballErrorTimers.set(source, setTimeout(() => {
      ballErrorTimers.delete(source); ballErrors.add(source); STATE.error = true; render();
    }, 8000));
  } else ballErrors.add(source);
  STATE.error = ballErrors.size > 0;
}
function mood() {
  if (STATE.approvals > 0) return 'alert';
  if (STATE.running.size) return 'working';
  if (STATE.error) return 'sad';
  if (Date.now() - STATE.lastActive > 120000) return 'sleepy';
  return 'idle';
}
/** Tokens are being spent: a task is running, output is streaming, or today's total just grew. */
/* Not too sensitive: a lone event or a tiny bump in usage never lights it.
   Light only after ~3 s of steady spending; once lit, stay lit at least 8 s and
   only go out after ~8 s of quiet, so short pauses between tool calls don't flicker. */
const FIRE = { igniteAfter: 3000, minLit: 8000, quietBefore: 8000, streamWindow: 6000, streamMin: 3 };
function spending() {
  const now = Date.now();
  STATE.streamAt = STATE.streamAt.filter((t) => now - t < FIRE.streamWindow);
  return STATE.running.size > 0 || STATE.streamAt.length >= FIRE.streamMin || now < STATE.burnUntil;
}
function burning() {
  const f = STATE.fire, now = Date.now(), active = spending();
  if (active) { f.idleSince = 0; if (!f.activeSince) f.activeSince = now; }
  else { f.activeSince = 0; if (!f.idleSince) f.idleSince = now; }
  if (!f.lit && active && now - f.activeSince >= FIRE.igniteAfter) { f.lit = true; f.litAt = now; }
  else if (f.lit && !active && now - f.idleSince >= FIRE.quietBefore && now - f.litAt >= FIRE.minLit) f.lit = false;
  return f.lit;
}
let wasBurning = false;
function render() {
  if (document.hidden) return;
  ball.dataset.mood = mood();
  const lit = burning();
  if (lit !== wasBurning) { wasBurning = lit; transitionFire(lit); }
  float.classList.toggle('burning', lit);
  float.classList.toggle('hot', lit && Date.now() < STATE.hotUntil);
  const badge = $('#badge');
  badge.hidden = STATE.approvals === 0;
  ball.setAttribute('aria-label', STATE.approvals ? `Salcara · ${STATE.approvals} 个操作等你批准` : STATE.running.size ? 'Salcara · 任务进行中' : 'Salcara 悬浮球');
}
setInterval(render, 1000);

/* Catching fire / going out: a breath-and-pop with a ring of colour and a burst of sparks;
   going out is a small sigh while the colour drains back down. */
function transitionFire(on) {
  float.classList.remove('igniting', 'dousing');
  void float.offsetWidth; // restart the animation
  float.classList.add(on ? 'igniting' : 'dousing');
  setTimeout(() => float.classList.remove(on ? 'igniting' : 'dousing'), 1000);
  if (!ball.dataset.quirk) {
    ball.dataset.quirk = on ? 'wide' : 'squint';
    setTimeout(() => { delete ball.dataset.quirk; }, on ? 800 : 900);
  }
  if (!on || matchMedia('(prefers-reduced-motion: reduce)').matches) return;
  setTimeout(() => {
    if (document.hidden) return;
    const ring = document.createElement('span');
    ring.className = 'ring-burst';
    float.insertBefore(ring, ball);
    ring.animate([{ transform: 'scale(.95)', opacity: 0.6 }, { transform: 'scale(1.8)', opacity: 0 }], { duration: 900, easing: 'cubic-bezier(.2,.7,.3,1)' }).onfinish = () => ring.remove();
    for (let i = 0; i < 12; i++) {
      const e = document.createElement('span');
      e.className = 'ember';
      const a = -Math.PI / 2 + (Math.random() - 0.5) * Math.PI * 1.5, r0 = 28, r1 = 28 + 16 + Math.random() * 26;
      const size = 1.8 + Math.random() * 1.8, color = ['#7CC6FF', '#3D7BFA', '#A68BF7', '#F4A6CE'][i % 4];
      Object.assign(e.style, { width: `${size}px`, height: `${size}px`, marginLeft: `${-size / 2}px`, marginTop: `${-size / 2}px`, background: color, boxShadow: `0 0 4px ${color}` });
      float.insertBefore(e, ball);
      e.animate([
        { transform: `translate(${Math.cos(a) * r0}px, ${Math.sin(a) * r0}px)`, opacity: 0 },
        { opacity: 1, offset: 0.15 },
        { transform: `translate(${Math.cos(a) * r1}px, ${Math.sin(a) * r1 - 10}px) scale(.3)`, opacity: 0 },
      ], { duration: 700 + Math.random() * 500, easing: 'cubic-bezier(.15,.7,.3,1)' }).onfinish = () => e.remove();
    }
  }, 420); // when the rising colour reaches the top
}

/* ---------- eyes: follow the pointer while it is near; otherwise wander at random ---------- */
const eye = { x: 0, y: 0, gx: 0, gy: 0, near: false };
const rand = (a, b) => a + Math.random() * (b - a);
const chance = (p) => Math.random() < p;
addEventListener('mousemove', (e) => {
  eye.near = true;
  const r = ball.getBoundingClientRect();
  const dx = e.clientX - (r.left + r.width / 2), dy = e.clientY - (r.top + r.height / 2), d = Math.hypot(dx, dy) || 1;
  const reach = Math.min(1, d / 120);
  eye.gx = (dx / d) * 8 * reach; eye.gy = (dy / d) * 6 * reach;
  scheduleBallEyes();
});
const away = () => { eye.near = false; };
addEventListener('mouseout', (e) => { if (!e.relatedTarget) away(); });
document.addEventListener('mouseleave', away);
let ballEyeFrame = 0;
function scheduleBallEyes() {
  if (!ballEyeFrame && !document.hidden && !reduceMotion) ballEyeFrame = requestAnimationFrame(tickBallEyes);
}
function tickBallEyes() {
  ballEyeFrame = 0;
  if (document.hidden) return;
  const k = eye.near ? 0.15 : 0.09;
  eye.x += (eye.gx - eye.x) * k; eye.y += (eye.gy - eye.y) * k;
  face.style.setProperty('--ex', `${eye.x.toFixed(2)}px`);
  face.style.setProperty('--ey', `${eye.y.toFixed(2)}px`);
  ball.style.setProperty('--hx', `${(34 - eye.x).toFixed(1)}%`);
  ball.style.setProperty('--hy', `${(28 - eye.y).toFixed(1)}%`);
  if (Math.abs(eye.gx - eye.x) > .02 || Math.abs(eye.gy - eye.y) > .02) scheduleBallEyes();
}
document.addEventListener('visibilitychange', () => {
  if (ballEyeFrame) cancelAnimationFrame(ballEyeFrame);
  ballEyeFrame = 0; scheduleBallEyes();
});
scheduleBallEyes();

// Random glances: a single look, a quick left–right scan, or back to centre.
(function wander() {
  let next = rand(900, 3200);
  if (!document.hidden && !reduceMotion && !eye.near && !drag.active) {
    const m = ball.dataset.mood;
    const range = m === 'sleepy' ? 0.35 : m === 'working' ? 0.5 : 1;
    const roll = Math.random();
    if (roll < 0.22) { eye.gx = 0; eye.gy = 0; next = rand(1500, 4000); }
    else if (roll < 0.36 && m === 'idle') {
      const side = chance(0.5) ? 1 : -1, y = rand(-2, 2);
      eye.gx = 7 * side; eye.gy = y;
      setTimeout(() => { if (!eye.near) { eye.gx = -7 * side; eye.gy = y; scheduleBallEyes(); } }, rand(380, 650));
      next = rand(1300, 2200);
    } else {
      const a = rand(0, Math.PI * 2), d = rand(0.35, 1) * range;
      eye.gx = Math.cos(a) * 8 * d; eye.gy = Math.sin(a) * 6 * d;
      next = rand(700, 2600);
    }
  }
  scheduleBallEyes();
  setTimeout(wander, next);
})();

function blinkOnce() { if (document.hidden || reduceMotion) return; ball.classList.add('blink'); setTimeout(() => ball.classList.remove('blink'), 200); }
function doubleBlink() { blinkOnce(); setTimeout(blinkOnce, 300); }
(function blinkLoop() {
  setTimeout(() => {
    if (ball.dataset.mood === 'idle' || ball.dataset.mood === 'working') { if (chance(0.22)) doubleBlink(); else blinkOnce(); }
    blinkLoop();
  }, rand(2200, 6500));
})();

// Small random expressions while idle: wink, squint, wide, curious, head tilt, peek.
const QUIRKS = [['wink-l', 520], ['wink-r', 520], ['squint', 1200], ['wide', 900], ['curious-l', 1400], ['curious-r', 1400], ['tilt-l', 1600], ['tilt-r', 1600]];
(function quirkLoop() {
  setTimeout(() => {
    const m = ball.dataset.mood;
    if (!document.hidden && !reduceMotion && !ball.dataset.quirk && !STATE.open && !drag.active) {
      let q = null;
      if (m === 'idle' && !STATE.hover) q = QUIRKS[Math.floor(Math.random() * QUIRKS.length)];
      else if (m === 'sleepy' && chance(0.4)) q = ['peek', 1100]; // half-wakes, then dozes again
      if (q) {
        ball.dataset.quirk = q[0];
        setTimeout(() => { delete ball.dataset.quirk; }, q[1] * rand(0.8, 1.3));
      }
    }
    if (!document.hidden && !reduceMotion && m === 'working') ball.style.setProperty('--scan', `${rand(1.4, 3).toFixed(2)}s`);
    quirkLoop();
  }, rand(3500, 9000));
})();

/* ---------- mouse: the window ignores clicks except on the ball, satellites and bubble ---------- */
let interactive = null;
const ballPointer = { x: 0, y: 0, inside: false };
function setInteractive(on, force = false) {
  if (!force && on === interactive) return;
  interactive = on;
  if (host) host.setInteractive(on);
}
const hot = (el) => el && (el.closest('.ball') || (STATE.open && (el.closest('.sats button') || el.closest('.plugin-ball-list'))) || (!STATE.open && float.classList.contains('peek') && el.closest('.bubble')));
function reconcileBallPointer() {
  setInteractive(!document.hidden && (drag.active || ballPointer.inside && !!hot(document.elementFromPoint(ballPointer.x, ballPointer.y))));
}
addEventListener('mousemove', e => { ballPointer.x = e.clientX; ballPointer.y = e.clientY; ballPointer.inside = true; reconcileBallPointer(); });
document.addEventListener('mouseleave', () => { ballPointer.inside = false; if (!drag.active) setInteractive(false); });

/* ---------- hover → usage bubble ---------- */
let peekTimer = 0;
function hidePeekSoon() {
  clearTimeout(peekTimer);
  peekTimer = setTimeout(() => {
    if (bubble.contains(document.activeElement)) return;
    float.classList.remove('peek'); STATE.hover = false; reconcileBallPointer(); render();
  }, 150);
}
ball.addEventListener('mouseenter', () => {
  setInteractive(true);
  STATE.hover = true; STATE.lastActive = Date.now(); render();
  clearTimeout(peekTimer);
  peekTimer = setTimeout(() => { placeBubble(); float.classList.add('peek'); refreshUsage(false); }, 180);
});
ball.addEventListener('mouseleave', () => {
  hidePeekSoon();
});
bubble.addEventListener('mouseenter', () => { clearTimeout(peekTimer); STATE.hover = true; setInteractive(true); });
bubble.addEventListener('mouseleave', hidePeekSoon);
bubble.addEventListener('focusin', () => { clearTimeout(peekTimer); float.classList.add('peek'); setInteractive(true); });
bubble.addEventListener('focusout', (event) => { if (!bubble.contains(event.relatedTarget)) hidePeekSoon(); });
function placeBubble() {
  // Show the bubble on the side with more room on screen.
  const right = window.salcaraBallLayout ? window.salcaraBallLayout.side === 'right' : window.screenX + innerWidth / 2 < (screen.availLeft || 0) + screen.availWidth / 2;
  bubble.classList.toggle('r', right);
  bubble.classList.toggle('l', !right);
  if (window.salcaraBallLayout) bubble.style.maxWidth = window.salcaraBallLayout.bubbleMaxWidth + 'px';
}
if (host?.onLayout) host.onLayout(layout => {
  if (!layout || !['left', 'right'].includes(layout.side) || !Number.isFinite(layout.maxWidth) || !Number.isFinite(layout.bubbleMaxWidth)) return;
  window.salcaraBallLayout = { side: layout.side, maxWidth: Math.max(0, Math.min(174, layout.maxWidth)), bubbleMaxWidth: Math.max(0, Math.min(148, layout.bubbleMaxWidth)) };
  placeBubble(); window.dispatchEvent(new Event('ball-layout')); reconcileBallPointer();
});

/* ---------- click → satellites; drag → move ---------- */
ball.addEventListener('pointerdown', (e) => {
  if (e.button !== 0) return;
  if (drag.active) return;
  setInteractive(true);
  drag.active = true; drag.moved = false; drag.x = e.screenX; drag.y = e.screenY;
  drag.pointerId = e.pointerId;
  try { ball.setPointerCapture(e.pointerId); } catch { cancelDrag(); return; }
  if (host) host.dragStart();
});
ball.addEventListener('pointermove', (e) => {
  if (!drag.active || e.pointerId !== drag.pointerId) return;
  if (!drag.moved && Math.hypot(e.screenX - drag.x, e.screenY - drag.y) > 4) {
    drag.moved = true;
    clearTimeout(peekTimer);
    setOpen(false);
    float.classList.remove('peek');
  }
  if (drag.moved && host) host.drag();
});
ball.addEventListener('pointerup', (e) => {
  if (!drag.active || e.pointerId !== drag.pointerId) return;
  const moved = drag.moved;
  drag.active = false;
  drag.pointerId = null;
  if (ball.hasPointerCapture(e.pointerId)) ball.releasePointerCapture(e.pointerId);
  if (host) host.dragEnd();
  if (!moved) setOpen(!STATE.open);
});
function cancelDrag() {
  if (!drag.active) return;
  const id = drag.pointerId;
  drag.active = false; drag.pointerId = null; drag.moved = false;
  if (id !== null && ball.hasPointerCapture(id)) ball.releasePointerCapture(id);
  if (host) { if (host.dragCancel) host.dragCancel(); else host.dragEnd(); }
  setInteractive(false, true);
}
ball.addEventListener('pointercancel', (event) => { if (event.pointerId === drag.pointerId) cancelDrag(); });
ball.addEventListener('lostpointercapture', (event) => { if (event.pointerId === drag.pointerId) cancelDrag(); });
ball.addEventListener('click', (e) => { if (e.detail === 0) setOpen(!STATE.open); });
ball.addEventListener('dblclick', () => { setOpen(false); go(''); });
function closeShortcuts(e) {
  if (e.key !== 'Escape') return;
  const wasOpen = STATE.open;
  setOpen(false);
  if (wasOpen) { e.preventDefault?.(); ball.focus?.(); }
}
ball.addEventListener('keydown', closeShortcuts);
$('#sats').addEventListener('keydown', closeShortcuts);

function setOpen(open) {
  STATE.open = open;
  if (!open && typeof window.closePluginBallMenu === 'function') window.closePluginBallMenu();
  ball.setAttribute('aria-expanded', String(open));
  $('#sats').inert = !open;
  float.classList.toggle('open', open);
  if (open) { clearTimeout(peekTimer); float.classList.remove('peek'); }
  STATE.lastActive = Date.now();
  reconcileBallPointer(); render();
}
sats.forEach((b) => b.addEventListener('click', () => { setOpen(false); go(b.dataset.go); }));
function go(route) {
  if (host) host.show(route);
  else window.open('/' + (route ? '#' + route : ''), '_blank');
}
addEventListener('blur', () => { cancelDrag(); setOpen(false); clearTimeout(peekTimer); STATE.hover = false; float.classList.remove('peek'); setInteractive(false, true); });
if (host?.onDragFinished) host.onDragFinished((on) => setInteractive(on === true, true));

/* ---------- data ---------- */
let ballStream = null, ballStreamRetry = 0, ballStreamBackoff = 3000;
const BALL_AUTH = { pending: null, controller: null, epoch: 0, attemptedAt: -Infinity };
// The Bridge rotates its HttpOnly cookie on restart. Refresh it through the
// same-origin entry page, once for concurrent failures; retry each read once.
function renewBallSession(epoch = BALL_AUTH.epoch) {
  if (document.hidden) return Promise.resolve(false);
  if (epoch !== BALL_AUTH.epoch) return Promise.resolve(true);
  if (BALL_AUTH.pending) return BALL_AUTH.pending;
  if (Date.now() - BALL_AUTH.attemptedAt < 3000) return Promise.resolve(false);
  BALL_AUTH.attemptedAt = Date.now();
  const controller = new AbortController(); BALL_AUTH.controller = controller;
  const timer = setTimeout(() => controller.abort(), 4000);
  const pending = (async () => {
    try {
      const response = await fetch('/', { credentials: 'same-origin', cache: 'no-store', signal: controller.signal });
      await response.body?.cancel();
      if (!response.ok || controller.signal.aborted) return false;
      BALL_AUTH.epoch++; return true;
    } catch { return false; }
    finally { clearTimeout(timer); if (BALL_AUTH.controller === controller) { BALL_AUTH.controller = null; BALL_AUTH.pending = null; } }
  })();
  BALL_AUTH.pending = pending;
  return pending;
}
async function ballFetch(url, options) {
  const epoch = BALL_AUTH.epoch;
  const response = await fetch(url, options);
  if (response.status !== 401 || options?.signal?.aborted || document.hidden) return response;
  if (!await renewBallSession(epoch) || options?.signal?.aborted || document.hidden) return response;
  return fetch(url, options);
}
let usageAt = 0;
const BALL_USAGE = { source: '', identity: '', seq: 0, controller: null, sourceSeq: 0, sourceController: null, sourceAt: 0, stamp: '' };
const ballEscape = (value) => String(value ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
function ballPreferredSource(local) {
  const accounts = Array.isArray(local?.accounts) ? local.accounts : [];
  const eligible = accounts.filter(a => a?.id && !a.libraryOnly && a.officialAccount?.mode !== 'chatgpt_web_session');
  const requestAt = a => Math.max(0, Date.parse(a.poolState?.lastUsedAt || '') || 0);
  const appliedAt = a => Number.isFinite(a.lastUsedAt) ? a.lastUsedAt : 0;
  const newest = (list, time) => list.sort((a, b) => time(b) - time(a) || Number(b.id === BALL_USAGE.source) - Number(a.id === BALL_USAGE.source))[0]?.id || '';
  const running = eligible.filter(a => Number.isSafeInteger(a.poolState?.inflight) && a.poolState.inflight > 0);
  if (running.length) return newest(running, requestAt);
  const available = new Set((local?.tools || []).filter(tool => tool?.available === true).map(tool => tool.id));
  const bindings = Object.entries(local?.bindings || {}).filter(([tool, b]) => available.has(tool) && b?.pending === false && b.accountId && b.accountId === b.appliedId).map(([, b]) => b);
  const applied = eligible.filter(a => bindings.some(b => b.accountId === a.id));
  // Finished short requests still identify the actual pool account. A later
  // successful tool switch supersedes that observation, even across tools.
  return newest(eligible.filter(a => requestAt(a) > 0 || applied.includes(a)), a => Math.max(requestAt(a), applied.includes(a) ? appliedAt(a) : 0));
}
function ballUsageMessage(message) {
  $('#bTok').textContent = '—';
  $('#bSub').textContent = message; $('#bSub').title = message;
}
function cancelBallUsage() {
  BALL_USAGE.seq++;
  BALL_USAGE.controller?.abort(); BALL_USAGE.controller = null;
}
async function refreshUsage(force) {
  if (document.hidden) return;
  if (!force && (document.visibilityState !== 'visible' || BALL_USAGE.sourceController || Date.now() - BALL_USAGE.sourceAt < 1000)) return;
  const sourceSeq = ++BALL_USAGE.sourceSeq;
  BALL_USAGE.sourceController?.abort();
  const controller = new AbortController();
  BALL_USAGE.sourceController = controller; BALL_USAGE.sourceAt = Date.now();
  const timer = setTimeout(() => controller.abort(), 10000);
  if (force) { cancelBallUsage(); ballUsageMessage('读取中…'); }
  let load = false;
  try {
    const response = await ballFetch('/api/local/accounts', { credentials: 'same-origin', signal: controller.signal });
    if (!response.ok) throw new Error('当前来源读取失败');
    const local = await response.json();
    if (sourceSeq !== BALL_USAGE.sourceSeq) return;
    if (!Array.isArray(local?.accounts)) throw new Error('当前来源读取失败');
    const source = ballPreferredSource(local), account = local.accounts.find(a => a.id === source);
    const identity = account ? JSON.stringify([source, account.sourceRevision || '', account.officialAccount?.id || '', account.officialAccount?.identityEpoch || 0]) : '';
    const changed = identity !== BALL_USAGE.identity;
    if (changed) {
      cancelBallUsage(); BALL_USAGE.stamp = ''; STATE.lastTokens = null; STATE.burnUntil = 0; usageAt = 0;
      ballUsageMessage(source ? '读取中…' : '暂无使用中的账号');
    }
    BALL_USAGE.source = source; BALL_USAGE.identity = identity;
    $('#bName').textContent = account?.name || (source ? account?.host || source : '用量');
    $('#bName').title = $('#bName').textContent;
    if (!source) { ballUsageMessage('暂无使用中的账号'); setBallError('usage', false); render(); return; }
    load = !BALL_USAGE.controller && (force || changed || Date.now() - usageAt >= 30000);
  } catch (e) {
    if (sourceSeq !== BALL_USAGE.sourceSeq) return;
    cancelBallUsage(); BALL_USAGE.source = ''; BALL_USAGE.identity = ''; BALL_USAGE.stamp = ''; STATE.lastTokens = null;
    $('#bName').textContent = '用量'; $('#bName').title = '';
    ballUsageMessage(controller.signal.aborted ? '当前来源读取超时' : e.message || '当前来源读取失败');
    setBallError('usage', true); render();
  } finally {
    clearTimeout(timer);
    if (sourceSeq === BALL_USAGE.sourceSeq) BALL_USAGE.sourceController = null;
  }
  if (load && sourceSeq === BALL_USAGE.sourceSeq) return loadBallUsage(force);
}
async function loadBallUsage(force) {
  const source = BALL_USAGE.source, identity = BALL_USAGE.identity;
  if (!source) return;
  usageAt = Date.now();
  const seq = ++BALL_USAGE.seq, controller = new AbortController();
  BALL_USAGE.controller = controller;
  const timer = setTimeout(() => controller.abort(), 30000);
  try {
    const d = await USAGE.load(1, force, source, controller.signal);
    if (seq !== BALL_USAGE.seq || identity !== BALL_USAGE.identity) return;
    const today = d.today || (d.hasDaily && d.series[0]);
    $('#bTok').textContent = today ? USAGE.fmtTokens(today.tokens) : '—';
    const parts = [];
    if (d.balance) parts.push(`剩余 <em>${ballEscape(USAGE.fmtMoney(d.balance.value, d.balance.unit))}</em>`);
    else if (today?.requests != null) parts.push(`${USAGE.fmtTokens(today.requests)} 次请求`);
    if (d.partial) parts.push('部分记录');
    const failed = d.sources.find(s => s.localUsageError || s.error && !s.localUsage);
    $('#bSub').innerHTML = parts.length ? parts.join(' · ') : (failed ? ballEscape(failed.localUsageError || failed.error) : '暂无 Token 记录');
    $('#bSub').title = $('#bSub').textContent;
    const observation = d.today ? (d.todayLocal ? 'local' : 'upstream') : (d.curveLocal ? 'local' : 'upstream');
    const stamp = `${identity}:${d.series.at(-1)?.date || ''}:${observation}`, tokens = today?.tokens;
    if (BALL_USAGE.stamp === stamp && typeof tokens === 'number' && typeof STATE.lastTokens === 'number' && tokens - STATE.lastTokens >= 2000) STATE.burnUntil = Math.max(STATE.burnUntil || 0, Date.now() + 40000);
    BALL_USAGE.stamp = stamp; STATE.lastTokens = typeof tokens === 'number' ? tokens : null;
    setBallError('usage', !!failed || !!(d.balance && d.balance.value !== null && d.balance.value < 1));
  } catch (e) {
    if (seq !== BALL_USAGE.seq) return;
    ballUsageMessage(controller.signal.aborted ? '用量读取超时' : e.message || '用量读取失败');
    STATE.lastTokens = null; BALL_USAGE.stamp = '';
    setBallError('usage', true);
  } finally {
    clearTimeout(timer);
    if (seq === BALL_USAGE.seq) BALL_USAGE.controller = null;
  }
  if (seq === BALL_USAGE.seq) render();
}
document.addEventListener('visibilitychange', () => {
  float.classList.toggle('paused', document.hidden);
  if (document.visibilityState === 'visible') { stream(); refreshUsage(true); refreshActivity(); render(); return; }
  stopBallStream();
  BALL_AUTH.controller?.abort();
  for (const source of ['stream', 'activity', 'usage']) setBallError(source, false);
  cancelDrag(); setOpen(false); clearTimeout(peekTimer);
  STATE.hover = false; float.classList.remove('peek'); setInteractive(false, true);
  BALL_USAGE.sourceSeq++; BALL_USAGE.sourceController?.abort(); BALL_USAGE.sourceController = null;
  cancelBallUsage(); BALL_USAGE.sourceAt = 0;
  cancelBallActivity();
});

// A process-local snapshot observes only events already emitted by the Bridge.
// Reading /api/sessions here would start agents and scan histories just to draw
// a status indicator. Revisioned live changes win over a late snapshot instead.
const BALL_ACTIVITY = { seq: 0, revision: 0, controller: null, changes: null, approvals: new Set() };
const busySession = s => s.status === 'running' || s.status === 'waiting_approval';
function applyActivityEvent(ev, running, approvals) {
  if (ev.type === 'session.updated' && ev.session?.sessionKey) {
    if (busySession(ev.session)) running.add(ev.session.sessionKey); else running.delete(ev.session.sessionKey);
  } else if (ev.approvalId) {
    if (ev.type === 'approval.request') approvals.add(ev.approvalId);
    else if (ev.type === 'approval.resolved') approvals.delete(ev.approvalId);
  }
}
function cancelBallActivity() {
  BALL_ACTIVITY.seq++;
  BALL_ACTIVITY.controller?.abort(); BALL_ACTIVITY.controller = null; BALL_ACTIVITY.changes = null;
}
async function refreshActivity() {
  if (document.visibilityState !== 'visible' || BALL_ACTIVITY.controller) return;
  const seq = ++BALL_ACTIVITY.seq, minimumRevision = BALL_ACTIVITY.revision;
  const controller = new AbortController(), changes = new Map();
  BALL_ACTIVITY.controller = controller; BALL_ACTIVITY.changes = changes;
  const timer = setTimeout(() => controller.abort(), 10000);
  try {
    const response = await ballFetch('/api/activity', { credentials: 'same-origin', signal: controller.signal });
    if (!response.ok) throw new Error('任务状态读取失败');
    const snapshot = await response.json();
    if (seq !== BALL_ACTIVITY.seq) return;
    if (controller.signal.aborted) throw new Error('任务状态读取超时');
    if (!Number.isSafeInteger(snapshot?.revision) || snapshot.revision < minimumRevision || !Array.isArray(snapshot.sessions) || !Array.isArray(snapshot.approvalIds)) throw new Error('任务状态无效');
    const running = new Set(snapshot.sessions.filter(s => typeof s?.sessionKey === 'string' && s.sessionKey && busySession(s)).map(s => s.sessionKey));
    const approvals = new Set(snapshot.approvalIds.filter(id => typeof id === 'string' && id));
    for (const ev of changes.values()) {
      if (ev.activityRevision > snapshot.revision) applyActivityEvent(ev, running, approvals);
    }
    STATE.running = running; BALL_ACTIVITY.approvals = approvals; STATE.approvals = approvals.size;
    BALL_ACTIVITY.revision = Math.max(BALL_ACTIVITY.revision, snapshot.revision);
    setBallError('activity', false);
  } catch (_) {
    if (seq === BALL_ACTIVITY.seq) setBallError('activity', true);
  } finally {
    clearTimeout(timer);
    if (seq === BALL_ACTIVITY.seq) { BALL_ACTIVITY.controller = null; BALL_ACTIVITY.changes = null; render(); }
  }
}
function receiveActivity(ev) {
  if (!Number.isSafeInteger(ev.activityRevision) || ev.activityRevision <= BALL_ACTIVITY.revision) return false;
  BALL_ACTIVITY.revision = ev.activityRevision;
  const key = ev.type === 'session.updated' ? 's:' + ev.session?.sessionKey : 'a:' + ev.approvalId;
  BALL_ACTIVITY.changes?.set(key, ev);
  applyActivityEvent(ev, STATE.running, BALL_ACTIVITY.approvals);
  STATE.approvals = BALL_ACTIVITY.approvals.size;
  STATE.lastActive = Date.now();
  return true;
}

function stopBallStream() {
  clearTimeout(ballStreamRetry); ballStreamRetry = 0;
  const es = ballStream; ballStream = null; es?.close();
}
function stream() {
  if (ballStream || document.hidden) return;
  const es = ballStream = new EventSource('/api/stream');
  es.addEventListener('event', (m) => {
    if (es !== ballStream || document.hidden) return;
    let ev; try { ev = JSON.parse(m.data); } catch (_) { return; }
    if (!ev || typeof ev !== 'object') return;
    if (ev.type === 'message' || ev.type === 'reasoning' || ev.type === 'tool' || ev.type === 'turn') {
      STATE.streamAt.push(Date.now());
      STATE.streamAt = STATE.streamAt.filter(t => Date.now() - t < FIRE.streamWindow).slice(-FIRE.streamMin);
      if (STATE.streamAt.length >= FIRE.streamMin) STATE.hotUntil = Date.now() + 2500; // steady streaming → the colours flow faster
    }
    if (ev.type === 'approval.request' || ev.type === 'approval.resolved') { if (receiveActivity(ev)) render(); return; }
    if (ev.type === 'session.updated' && ev.session) {
      const s = ev.session, key = s.sessionKey;
      const wasBusy = STATE.running.has(key);
      if (!receiveActivity(ev)) return;
      if (!busySession(s) && wasBusy) {
        setBallError('task', s.status === 'failed');
        if (s.status !== 'failed') doubleBlink();
        usageAt = 0;
      }
      refreshUsage(false);
    }
    render();
  });
  es.onerror = () => {
    if (es !== ballStream || document.hidden) return;
    cancelBallActivity(); setBallError('stream', true); render();
    // A 401 leaves EventSource CLOSED permanently. Recover with bounded
    // backoff instead of relying on its network-only automatic reconnect.
    if (es.readyState === EventSource.CLOSED && !ballStreamRetry) {
      ballStreamRetry = setTimeout(async () => {
        ballStreamRetry = 0;
        await renewBallSession();
        if (es !== ballStream || document.hidden) return;
        es.close(); ballStream = null; stream();
      }, ballStreamBackoff);
      ballStreamBackoff = Math.min(30000, ballStreamBackoff * 2);
    }
  };
  es.onopen = () => {
    if (es !== ballStream || document.hidden) return;
    clearTimeout(ballStreamRetry); ballStreamRetry = 0; ballStreamBackoff = 3000;
    // Reconnection may reach a restarted Bridge whose process-local revision
    // starts at zero. Invalidate reads from the prior connection first.
    cancelBallActivity(); BALL_ACTIVITY.revision = 0;
    setBallError('stream', false); refreshActivity(); render();
  };
}

if (host && host.onShown) host.onShown((initialInteractive) => {
  cancelDrag(); setOpen(false); clearTimeout(peekTimer);
  STATE.hover = false; float.classList.remove('peek');
  setInteractive(initialInteractive === true, true);
  stream(); refreshActivity(); refreshUsage(true); doubleBlink(); render();
});
refreshActivity();
refreshUsage(true);
stream();
setInterval(refreshActivity, 15000);

// Tools that talk to the relay directly (not through the Bridge) still spend tokens:
// watch today's total and light up while it keeps growing.
function watchSpend() {
  // Local activity follows quickly; the provider's usage is read at most every
  // 30 seconds for an unchanged identity, without blocking source detection.
  refreshUsage(false);
  setTimeout(watchSpend, 5000);
}
setTimeout(watchSpend, 2000);

/* ---------- sparks: a few tiny points of the brand colours drift up off the body ---------- */
const EMBER = ['#7CC6FF', '#3D7BFA', '#A68BF7', '#F4A6CE'];
(function emberLoop() {
  const on = float.classList.contains('burning') && !STATE.open && !reduceMotion && document.visibilityState === 'visible';
  const hotNow = float.classList.contains('hot');
  if (on) {
    const e = document.createElement('span');
    e.className = 'ember';
    const size = rand(1.6, hotNow ? 3 : 2.4), a = rand(-1.1, 1.1); // angle from straight up
    const x = Math.sin(a) * 30, y = -Math.cos(a) * 30, lift = rand(22, hotNow ? 46 : 34), drift = rand(-8, 8);
    const color = EMBER[Math.floor(Math.random() * EMBER.length)];
    Object.assign(e.style, { width: `${size}px`, height: `${size}px`, marginLeft: `${-size / 2}px`, marginTop: `${-size / 2}px`, background: color, boxShadow: `0 0 4px ${color}` });
    float.insertBefore(e, ball);
    e.animate([
      { transform: `translate(${x}px, ${y}px)`, opacity: 0 },
      { opacity: 0.9, offset: 0.2 },
      { transform: `translate(${x + drift}px, ${y - lift}px) scale(.4)`, opacity: 0 },
    ], { duration: rand(1100, 1800), easing: 'cubic-bezier(.25,.6,.4,1)' }).onfinish = () => e.remove();
  }
  setTimeout(emberLoop, on ? rand(hotNow ? 120 : 260, hotNow ? 260 : 520) : document.hidden ? 3000 : 400);
})();
render();
float.classList.toggle('paused', document.hidden);
