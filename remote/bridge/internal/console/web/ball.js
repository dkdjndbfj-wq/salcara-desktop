'use strict';
/* Floating ball shown while the main window is closed to the background.
   Click: satellites (quick entries) appear around it. Hover: today's token usage
   beside it. Drag: move it anywhere. The face reacts to what the Bridge is doing. */

const host = window.salcaraBall || null; // Electron preload; absent in a plain browser
const $ = (s) => document.querySelector(s);
const ICON = {
  key: '<circle cx="8.5" cy="15.5" r="4.5"/><path d="M11.7 12.3 20 4M16.5 7.5l2.5 2.5M14.2 9.8l1.8 1.8"/>',
  home: '<path d="M4 10.2 12 4l8 6.2V19a1 1 0 0 1-1 1h-4.5v-5.5h-5V20H5a1 1 0 0 1-1-1z"/>',
  chart: '<path d="M4 19.5h16"/><path d="m5 15 4.5-4.5 3.5 3 6-6.5"/><path d="M15.5 7H19v3.5"/>',
  spark: '<path d="M10.5 6Q11.4 12.6 18 13.5Q11.4 14.4 10.5 21Q9.6 14.4 3 13.5Q9.6 12.6 10.5 6Z"/><path d="M18.75 3.5v3.5M17 5.25h3.5"/>',
  phone: '<rect x="6.5" y="3" width="11" height="18" rx="2.5"/><path d="M10.5 17.5h3"/>',
  gear: '<path d="M4 7.5h9M17 7.5h3M4 16.5h3M11 16.5h9"/><circle cx="15" cy="7.5" r="2"/><circle cx="9" cy="16.5" r="2"/>',
};
document.querySelectorAll('[data-i]').forEach((el) => {
  el.innerHTML = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">${ICON[el.dataset.i]}</svg>`;
});

const float = $('#float'), ball = $('#ball'), face = $('#face'), bubble = $('#bubble');
const sats = [...document.querySelectorAll('#sats button')];
sats.forEach((b, i) => {
  const a = (360 / sats.length) * i;
  b.style.setProperty('--a', `${a}deg`);
  b.style.setProperty('--i', i);
  b.dataset.side = a > 180 ? 'l' : 'r';
});

/* ---------- mood ---------- */
const drag = { active: false, moved: false, x: 0, y: 0 };
const STATE = { burnUntil: 0, hotUntil: 0, streamAt: [], fire: { lit: false, litAt: 0, activeSince: 0, idleSince: 0 }, lastTokens: null, approvals: 0, running: new Set(), error: false, lastActive: Date.now(), hover: false, open: false };
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
});
const away = () => { eye.near = false; };
addEventListener('mouseout', (e) => { if (!e.relatedTarget) away(); });
document.addEventListener('mouseleave', away);
(function tick() {
  const k = eye.near ? 0.15 : 0.09; // wandering glances move a little softer than pointer tracking
  eye.x += (eye.gx - eye.x) * k; eye.y += (eye.gy - eye.y) * k;
  face.style.setProperty('--ex', `${eye.x.toFixed(2)}px`);
  face.style.setProperty('--ey', `${eye.y.toFixed(2)}px`);
  ball.style.setProperty('--hx', `${(34 - eye.x).toFixed(1)}%`);
  ball.style.setProperty('--hy', `${(28 - eye.y).toFixed(1)}%`);
  requestAnimationFrame(tick);
})();

// Random glances: a single look, a quick left–right scan, or back to centre.
(function wander() {
  let next = rand(900, 3200);
  if (!eye.near && !drag.active) {
    const m = ball.dataset.mood;
    const range = m === 'sleepy' ? 0.35 : m === 'working' ? 0.5 : 1;
    const roll = Math.random();
    if (roll < 0.22) { eye.gx = 0; eye.gy = 0; next = rand(1500, 4000); }
    else if (roll < 0.36 && m === 'idle') {
      const side = chance(0.5) ? 1 : -1, y = rand(-2, 2);
      eye.gx = 7 * side; eye.gy = y;
      setTimeout(() => { if (!eye.near) { eye.gx = -7 * side; eye.gy = y; } }, rand(380, 650));
      next = rand(1300, 2200);
    } else {
      const a = rand(0, Math.PI * 2), d = rand(0.35, 1) * range;
      eye.gx = Math.cos(a) * 8 * d; eye.gy = Math.sin(a) * 6 * d;
      next = rand(700, 2600);
    }
  }
  setTimeout(wander, next);
})();

function blinkOnce() { ball.classList.add('blink'); setTimeout(() => ball.classList.remove('blink'), 200); }
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
    if (!ball.dataset.quirk && !STATE.open && !drag.active) {
      let q = null;
      if (m === 'idle' && !STATE.hover) q = QUIRKS[Math.floor(Math.random() * QUIRKS.length)];
      else if (m === 'sleepy' && chance(0.4)) q = ['peek', 1100]; // half-wakes, then dozes again
      if (q) {
        ball.dataset.quirk = q[0];
        setTimeout(() => { delete ball.dataset.quirk; }, q[1] * rand(0.8, 1.3));
      }
    }
    if (m === 'working') ball.style.setProperty('--scan', `${rand(1.4, 3).toFixed(2)}s`);
    quirkLoop();
  }, rand(3500, 9000));
})();

/* ---------- mouse: the window ignores clicks except on the ball, satellites and bubble ---------- */
let interactive = false;
function setInteractive(on) {
  if (on === interactive) return;
  interactive = on;
  if (host) host.setInteractive(on);
}
const hot = (el) => el && (el.closest('.ball') || (STATE.open && el.closest('.sats button')));
addEventListener('mousemove', (e) => setInteractive(!!hot(document.elementFromPoint(e.clientX, e.clientY)) || drag.active));
document.addEventListener('mouseleave', () => { if (!drag.active) setInteractive(false); });

/* ---------- hover → usage bubble ---------- */
let peekTimer = 0;
ball.addEventListener('mouseenter', () => {
  STATE.hover = true; STATE.lastActive = Date.now(); render();
  clearTimeout(peekTimer);
  peekTimer = setTimeout(() => { placeBubble(); float.classList.add('peek'); refreshUsage(false); }, 180);
});
ball.addEventListener('mouseleave', () => {
  STATE.hover = false; render();
  clearTimeout(peekTimer);
  float.classList.remove('peek');
});
function placeBubble() {
  // Show the bubble on the side with more room on screen.
  const right = window.screenX + innerWidth / 2 < (screen.availLeft || 0) + screen.availWidth / 2;
  bubble.classList.toggle('r', right);
  bubble.classList.toggle('l', !right);
}

/* ---------- click → satellites; drag → move ---------- */
ball.addEventListener('pointerdown', (e) => {
  if (e.button !== 0) return;
  drag.active = true; drag.moved = false; drag.x = e.screenX; drag.y = e.screenY;
  ball.setPointerCapture(e.pointerId);
  if (host) host.dragStart();
});
ball.addEventListener('pointermove', (e) => {
  if (!drag.active) return;
  if (!drag.moved && Math.hypot(e.screenX - drag.x, e.screenY - drag.y) > 4) {
    drag.moved = true;
    setOpen(false);
    float.classList.remove('peek');
  }
  if (drag.moved && host) host.drag();
});
ball.addEventListener('pointerup', (e) => {
  if (!drag.active) return;
  drag.active = false;
  ball.releasePointerCapture(e.pointerId);
  if (host) host.dragEnd();
  if (!drag.moved) setOpen(!STATE.open);
});
ball.addEventListener('dblclick', () => go(''));
ball.addEventListener('keydown', (e) => { if (e.key === 'Escape') setOpen(false); });

function setOpen(open) {
  STATE.open = open;
  float.classList.toggle('open', open);
  if (open) float.classList.remove('peek');
  STATE.lastActive = Date.now();
  render();
}
sats.forEach((b) => b.addEventListener('click', () => { setOpen(false); go(b.dataset.go); }));
function go(route) {
  if (host) host.show(route);
  else window.open('/' + (route ? '#' + route : ''), '_blank');
}
addEventListener('blur', () => setOpen(false));

/* ---------- data ---------- */
let usageAt = 0;
async function refreshUsage(force) {
  if (!force && Date.now() - usageAt < 60000) return;
  usageAt = Date.now();
  try {
    const d = await USAGE.load(1, false);
    const today = d.today || (d.series[0] && { tokens: d.series[0].tokens, cost: d.series[0].cost });
    $('#bTok').textContent = today ? USAGE.fmtTokens(today.tokens) : '—';
    const parts = [];
    if (today) parts.push(USAGE.fmtMoney(today.cost));
    if (d.balance) parts.push(`剩余 <em>${USAGE.fmtMoney(d.balance.value, d.balance.unit)}</em>`);
    $('#bSub').innerHTML = parts.length ? parts.join(' · ') : (d.sources.length ? '服务商未提供用量' : '还没有保存 API');
    STATE.error = !!(d.balance && d.balance.value !== null && d.balance.value < 1);
  } catch (e) {
    $('#bSub').textContent = '读取失败';
    STATE.error = true;
  }
  render();
}

async function refreshApprovals() {
  try {
    const r = await fetch('/api/approvals', { credentials: 'same-origin' }).then((x) => x.json());
    STATE.approvals = (r.approvals || []).length;
  } catch (_) { /* keep last */ }
  render();
}

function stream() {
  const es = new EventSource('/api/stream');
  es.addEventListener('event', (m) => {
    let ev; try { ev = JSON.parse(m.data); } catch (_) { return; }
    if (ev.type === 'message' || ev.type === 'reasoning' || ev.type === 'tool' || ev.type === 'turn') {
      STATE.streamAt.push(Date.now());
      if (STATE.streamAt.length >= FIRE.streamMin) STATE.hotUntil = Date.now() + 2500; // steady streaming → the colours flow faster
    }
    if (ev.type === 'approval.request' || ev.type === 'approval.resolved') { STATE.lastActive = Date.now(); refreshApprovals(); return; }
    if (ev.type === 'session.updated' && ev.session) {
      const s = ev.session, key = s.sessionKey;
      const busy = s.status === 'running' || s.status === 'waiting_approval';
      if (busy) STATE.running.add(key);
      else if (STATE.running.delete(key)) {
        if (s.status === 'failed') STATE.error = true;
        else { STATE.error = false; doubleBlink(); }
        usageAt = 0;
      }
      STATE.lastActive = Date.now();
    }
    render();
  });
  es.onerror = () => { STATE.error = true; render(); };
  es.onopen = () => { STATE.error = false; render(); };
}

if (host && host.onShown) host.onShown(() => { refreshApprovals(); refreshUsage(true); doubleBlink(); render(); });
refreshApprovals();
refreshUsage(true);
stream();
setInterval(refreshApprovals, 15000);

// Tools that talk to the relay directly (not through the Bridge) still spend tokens:
// watch today's total and light up while it keeps growing.
async function watchSpend() {
  if (document.visibilityState === 'visible') {
    try {
      const d = await USAGE.load(1, true);
      const tokens = d.today ? d.today.tokens : (d.series[0] ? d.series[0].tokens : null);
      if (tokens !== null && STATE.lastTokens !== null && tokens - STATE.lastTokens >= 2000) STATE.burnUntil = Math.max(STATE.burnUntil, Date.now() + 40000);
      if (tokens !== null) STATE.lastTokens = tokens;
      render();
    } catch (_) { /* keep last */ }
  }
  setTimeout(watchSpend, 30000);
}
setTimeout(watchSpend, 2000);

/* ---------- sparks: a few tiny points of the brand colours drift up off the body ---------- */
const EMBER = ['#7CC6FF', '#3D7BFA', '#A68BF7', '#F4A6CE'];
const reduceMotion = matchMedia('(prefers-reduced-motion: reduce)').matches;
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
  setTimeout(emberLoop, on ? rand(hotNow ? 120 : 260, hotNow ? 260 : 520) : 400);
})();
render();
