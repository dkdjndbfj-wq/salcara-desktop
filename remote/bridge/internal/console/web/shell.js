'use strict';
/* Shell: the orb on the home screen opens a radial menu; every page opens in a
   pop-up panel. Pages keep their existing renderers (RENDER_* / ACTIONS). */

const SHELL = { open: false, closeTimer: 0, mouse: { x: innerWidth / 2, y: innerHeight / 2 }, eye: { x: 0, y: 0 }, tilt: { x: 0, y: 0 }, look: null, origin: null };
const ROUTE_ICON = { overview: 'spark', accounts: 'key', setup: 'box', login: 'phone', sessions: 'chat', settings: 'gear', usage: 'chart', tools: 'plug', projects: 'folder' };

(function layoutRing() {
  const items = [...document.querySelectorAll('#nav a')];
  items.forEach((a, i) => {
    a.style.setProperty('--a', `${(360 / items.length) * i}deg`);
    a.style.setProperty('--i', i);
  });
})();

/* ---------- open / close the radial menu ---------- */
const zone = $('#orbZone');
const orb = $('#orb');
function setMenu(open) {
  SHELL.open = open;
  zone.classList.toggle('open', open);
  orb.setAttribute('aria-expanded', String(open));
  if (open) {
    $('#hint').classList.add('gone');
    try { localStorage.setItem('salcara.hint', '1'); } catch (_) { /* optional */ }
  }
}
function insideMenu(x, y) {
  const r = zone.getBoundingClientRect();
  const dx = x - (r.left + r.width / 2), dy = y - (r.top + r.height / 2);
  return Math.hypot(dx, dy) < 320;
}
orb.addEventListener('mouseenter', () => { clearTimeout(SHELL.closeTimer); setMenu(true); });
orb.addEventListener('click', () => setMenu(!SHELL.open));
orb.addEventListener('keydown', (e) => { if (e.key === 'ArrowDown' && SHELL.open) $('#nav a')?.focus(); });
zone.addEventListener('mouseleave', () => { SHELL.closeTimer = setTimeout(() => setMenu(false), 260); });
zone.addEventListener('mouseenter', () => clearTimeout(SHELL.closeTimer));
$$('#nav a').forEach((a) => {
  a.addEventListener('mouseenter', () => { SHELL.look = a; zone.classList.add('looking'); });
  a.addEventListener('mouseleave', () => { SHELL.look = null; zone.classList.remove('looking'); });
  a.addEventListener('focus', () => setMenu(true));
  a.addEventListener('click', () => {
    const r = a.querySelector('.ico').getBoundingClientRect();
    SHELL.origin = { x: r.left + r.width / 2, y: r.top + r.height / 2 };
  });
});
try { if (localStorage.getItem('salcara.hint')) $('#hint').classList.add('gone'); } catch (_) { /* optional */ }

/* ---------- eyes follow the pointer ---------- */
addEventListener('pointermove', (e) => {
  SHELL.mouse = { x: e.clientX, y: e.clientY };
  if (SHELL.open && !insideMenu(e.clientX, e.clientY) && !document.body.classList.contains('panel-open')) {
    clearTimeout(SHELL.closeTimer);
    SHELL.closeTimer = setTimeout(() => setMenu(false), 260);
  }
}, { passive: true });

const orbFace = $('#orbFace');
let orbFrame = 0;
const orbStyles = new Map();
function orbStyle(node, name, value) {
  if (orbStyles.get(name) === value) return;
  orbStyles.set(name, value); node.style.setProperty(name, value);
}
function animateOrb() {
  orbFrame = 0;
  if (document.hidden) return;
  const r = orb.getBoundingClientRect();
  const cx = r.left + r.width / 2, cy = r.top + r.height / 2;
  let tx = SHELL.mouse.x, ty = SHELL.mouse.y;
  if (SHELL.look) { const lr = SHELL.look.getBoundingClientRect(); tx = lr.left + lr.width / 2; ty = lr.top + 27; }
  const dx = tx - cx, dy = ty - cy, d = Math.hypot(dx, dy) || 1;
  const reach = Math.min(1, d / 180);
  const goalX = (dx / d) * 34 * reach, goalY = (dy / d) * 26 * reach;
  SHELL.eye.x += (goalX - SHELL.eye.x) * 0.16;
  SHELL.eye.y += (goalY - SHELL.eye.y) * 0.16;
  SHELL.tilt.x += ((-dy / d) * 9 * reach - SHELL.tilt.x) * 0.1;
  SHELL.tilt.y += ((dx / d) * 9 * reach - SHELL.tilt.y) * 0.1;
  const face = orbFace;
  orbStyle(face, '--ex', `${SHELL.eye.x.toFixed(2)}px`);
  orbStyle(face, '--ey', `${SHELL.eye.y.toFixed(2)}px`);
  orbStyle(orb, '--rx', `${SHELL.tilt.x.toFixed(2)}deg`);
  orbStyle(orb, '--ry', `${SHELL.tilt.y.toFixed(2)}deg`);
  orbStyle(orb, '--hx', `${(36 - SHELL.eye.x * 0.5).toFixed(1)}%`);
  orbStyle(orb, '--hy', `${(30 - SHELL.eye.y * 0.5).toFixed(1)}%`);
  orbFrame = requestAnimationFrame(animateOrb);
}
document.addEventListener('visibilitychange', () => {
  if (orbFrame) cancelAnimationFrame(orbFrame);
  orbFrame = document.hidden ? 0 : requestAnimationFrame(animateOrb);
});
orbFrame = requestAnimationFrame(animateOrb);

(function blinkLoop() {
  setTimeout(() => {
    orb.classList.add('blink');
    setTimeout(() => orb.classList.remove('blink'), 200);
    if (Math.random() < 0.25) setTimeout(() => { orb.classList.add('blink'); setTimeout(() => orb.classList.remove('blink'), 200); }, 280);
    blinkLoop();
  }, 2600 + Math.random() * 3800);
})();

/* ---------- pages open in a panel ---------- */
PAGES.setup = { title: '环境', sub: '检测并一键安装需要的工具' };
Object.assign(PAGES.overview, { sub: '选择 API，打开工具' });
Object.assign(PAGES.accounts, { sub: '本机保存，所有 Agent 共用' });
Object.assign(PAGES.login, { sub: '用手机继续这台电脑上的对话' });
Object.assign(PAGES.sessions, { sub: '这台电脑上的 Codex 与 Claude Code 对话' });
Object.assign(PAGES.settings, { sub: '对话、启动、审批与日志' });
Object.assign(PAGES.usage, { sub: '模型、API 与中转站剩余额度' });

function openPanel() {
  const back = $('#panelBack');
  const panel = $('#panel');
  const o = SHELL.origin || { x: innerWidth / 2, y: innerHeight / 2 };
  const r = { left: 28, top: 28 };
  panel.style.setProperty('--ox', `${o.x - r.left}px`);
  panel.style.setProperty('--oy', `${o.y - r.top}px`);
  SHELL.origin = null;
  if (!back.hidden && !back.classList.contains('leave')) return;
  back.hidden = false;
  back.classList.remove('leave');
  back.classList.add('enter');
  document.body.classList.add('panel-open');
  setMenu(false);
}

function closePanel() {
  const back = $('#panelBack');
  if (back.hidden) return;
  document.body.classList.remove('panel-open');
  back.classList.remove('enter');
  back.classList.add('leave');
  setTimeout(() => { if (back.classList.contains('leave')) { back.hidden = true; back.classList.remove('leave'); $('#view').innerHTML = ''; } }, 220);
}

route = async function () {
  const r = (location.hash || '').slice(1).split('/')[0];
  if (!r || !PAGES[r] || !RENDER[r]) {
    S.route = 'home';
    $$('#nav a').forEach((a) => a.classList.remove('active'));
    closePanel();
    return;
  }
  const changed = S.route !== r;
  S.route = r;
  $$('#nav a').forEach((a) => a.classList.toggle('active', a.dataset.route === r));
  $('#pageIco').innerHTML = icon(ROUTE_ICON[r] || 'spark');
  $('#pageTitle').textContent = PAGES[r].title;
  $('#pageSub').textContent = PAGES[r].sub;
  openPanel();
  // A late renderer retains a detached node, never the new page's content.
  const oldView = $('#view'), view = oldView.cloneNode(false);
  oldView.replaceWith(view);
  if (changed) {
    $('#headActions').innerHTML = '';
    view.innerHTML = '<div class="card"><div class="skeleton" style="width:40%"></div><div class="skeleton" style="width:70%;margin-top:12px"></div></div>';
  }
  try {
    await RENDER[r](view);
  } catch (e) {
    view.innerHTML = `<div class="callout bad">${ico('warn')}<div>${esc(e.message)}</div></div>`;
  }
};

function goHome() { if (location.hash) history.pushState(null, '', location.pathname); route(); }
$('#panelClose').addEventListener('click', goHome);
$('#panelBack').addEventListener('mousedown', (e) => { if (e.target.id === 'panelBack') goHome(); });
addEventListener('keydown', (e) => {
  if (e.key !== 'Escape' || $('#modalRoot').children.length) return;
  if (document.body.classList.contains('panel-open')) goHome(); else setMenu(false);
});
addEventListener('popstate', () => route());

/* ---------- small signals on the home screen ---------- */
const shellBadge = setApprovalBadge;
setApprovalBadge = function () {
  shellBadge();
  let dot = $('#orbAlert');
  if (S.approvals.length && !dot) {
    dot = document.createElement('span'); dot.id = 'orbAlert'; dot.className = 'orb-alert'; dot.title = '有待你批准的操作';
    zone.appendChild(dot);
  } else if (!S.approvals.length && dot) dot.remove();
};

async function refreshSetupDot() {
  try {
    const r = await api('/api/setup');
    if (r.discoveryPending) { setTimeout(refreshSetupDot, 500); return; }
    const missing = (r.items || []).some((item) => item.required && !item.installed);
    $('#setupDot').hidden = !missing;
    if (missing) { const hint = $('#hint'); hint.textContent = '环境未就绪 · 打开「环境」一键安装'; hint.classList.remove('gone'); }
  } catch (_) { /* optional */ }
}
setTimeout(refreshSetupDot, 800);

/* "⋯" menus inside pages close on an outside click */
document.addEventListener('click', (e) => {
  $$('details.wb-card-more[open]').forEach((d) => { if (!d.contains(e.target)) d.open = false; });
});
