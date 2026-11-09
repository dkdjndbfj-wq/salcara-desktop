'use strict';
/* Desktop command centre: persistent top navigation and a live overview.
   Page renderers and their owned asynchronous operations stay independent. */

const SHELL = { open: false, closeTimer: 0, mouse: { x: innerWidth / 2, y: innerHeight / 2 }, eye: { x: 0, y: 0 }, tilt: { x: 0, y: 0 }, look: null, origin: null };
const ROUTE_ICON = { overview: 'spark', accounts: 'key', setup: 'box', login: 'phone', sessions: 'chat', settings: 'gear', usage: 'chart', tools: 'plug', projects: 'folder', plugins: 'puzzle' };

/* The menu stays visible above every page; the orb is decorative. */
const zone = $('#orbZone');
const orb = $('#orb');
function setMenu() { /* kept for callers from the radial-menu era */ }
$$('#nav a').forEach((a) => a.addEventListener('click', (e) => {
  if (a.dataset.route === 'home') { e.preventDefault(); goHome(); }
}));

/* ---------- eyes follow the pointer ---------- */
addEventListener('pointermove', (e) => {
  SHELL.mouse = { x: e.clientX, y: e.clientY };
  SHELL.orbCenter = null;
  scheduleOrbFrame();
}, { passive: true });

const orbFace = $('#orbFace');
const orbMotionQuery = typeof matchMedia === 'function' ? matchMedia('(prefers-reduced-motion: reduce)') : null;
let orbFrame = 0;
const orbStyles = new Map();
function measureOrb() {
  const r = orb.getBoundingClientRect();
  SHELL.orbCenter = { x: r.left + r.width / 2, y: r.top + r.height / 2 };
}
function scheduleOrbFrame() {
  if (orbFrame || orbMotionQuery?.matches || document.hidden || document.body.classList.contains('panel-open')) return;
  orbFrame = requestAnimationFrame(animateOrb);
}
function orbStyle(node, name, value) {
  if (orbStyles.get(name) === value) return;
  orbStyles.set(name, value); node.style.setProperty(name, value);
}
function animateOrb() {
  orbFrame = 0;
  if (orbMotionQuery?.matches || document.hidden || document.body.classList.contains('panel-open')) { orbFrame = 0; return; }
  if (!SHELL.orbCenter) measureOrb();
  const { x: cx, y: cy } = SHELL.orbCenter;
  let tx = SHELL.mouse.x, ty = SHELL.mouse.y;
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
  const settled = Math.abs(goalX - SHELL.eye.x) < 0.04 && Math.abs(goalY - SHELL.eye.y) < 0.04
    && Math.abs((-dy / d) * 9 * reach - SHELL.tilt.x) < 0.04
    && Math.abs((dx / d) * 9 * reach - SHELL.tilt.y) < 0.04;
  if (!settled) scheduleOrbFrame();
}
document.addEventListener('visibilitychange', () => {
  if (orbFrame) cancelAnimationFrame(orbFrame);
  orbFrame = 0;
  SHELL.orbCenter = null;
  scheduleOrbFrame();
});
addEventListener('resize', () => { SHELL.orbCenter = null; scheduleOrbFrame(); }, { passive: true });
orbMotionQuery?.addEventListener('change', () => {
  if (orbFrame) cancelAnimationFrame(orbFrame);
  orbFrame = 0;
  if (orbMotionQuery.matches) {
    SHELL.eye = { x: 0, y: 0 }; SHELL.tilt = { x: 0, y: 0 };
    for (const name of ['--ex', '--ey']) orbStyle(orbFace, name, '0px');
    for (const name of ['--rx', '--ry']) orbStyle(orb, name, '0deg');
  } else scheduleOrbFrame();
});
scheduleOrbFrame();

(function blinkLoop() {
  setTimeout(() => {
    if (document.hidden || document.body.classList.contains('panel-open') || orbMotionQuery?.matches) { blinkLoop(); return; }
    orb.classList.add('blink');
    setTimeout(() => orb.classList.remove('blink'), 200);
    if (Math.random() < 0.25) setTimeout(() => { orb.classList.add('blink'); setTimeout(() => orb.classList.remove('blink'), 200); }, 280);
    blinkLoop();
  }, 2600 + Math.random() * 3800);
})();

/* ---------- pages open in a panel ---------- */
PAGES.setup = { title: '环境', sub: '检测并一键安装需要的工具' };
Object.assign(PAGES.overview, { sub: '给每个 Agent 选一把密钥，然后打开或让手机接手' });
Object.assign(PAGES.accounts, { sub: '保存在这台电脑，Codex、Claude Code、DeepSeek Harness 与 Hermes 都能选用' });
Object.assign(PAGES.login, { sub: '任务始终在这台电脑上运行，手机只发指令和看结果' });
Object.assign(PAGES.sessions, { sub: '查看这台电脑上四种 Agent 的对话与手机任务记录' });
Object.assign(PAGES.settings, { sub: '启动、提醒、审批与数据' });
Object.assign(PAGES.usage, { sub: '每把密钥、每个 Agent 用了多少' });

function openPanel() {
  const back = $('#panelBack');
  back.hidden = false;
  back.classList.remove('leave');
  document.body.classList.add('panel-open');
}

function closePanel() {
  const back = $('#panelBack');
  document.body.classList.remove('panel-open');
  if (back.hidden) return;
  back.hidden = true;
  $('#view').innerHTML = '';
  if (typeof animateOrb === 'function' && typeof orbFrame !== 'undefined' && !orbFrame) orbFrame = requestAnimationFrame(animateOrb);
}

route = async function () {
  const navigation = S.localNavigationRevision = (S.localNavigationRevision || 0) + 1;
  const requestedHash = location.hash || '';
  const apiManagementOpen = typeof apiManagementActive === 'function' && apiManagementActive();
  const localEditorOpen = S.localEditorDraft && S.localEditorDraft.input === $('#laID');
  if ((apiManagementOpen || localEditorOpen) && typeof closeLocalEditor === 'function') {
    const mayLeave = await closeLocalEditor({ returnToService: false });
    // Multiple back/forward or navigation events share the same discard choice;
    // only the latest destination may render after that choice settles.
    if (navigation !== S.localNavigationRevision) return;
    if (!mayLeave) {
      const currentHash = S.route && S.route !== 'home' ? '#' + S.route : '';
      if (location.hash !== currentHash) history.replaceState(null, '', location.pathname + location.search + currentHash);
      return;
    }
  }
  if (typeof WB !== 'undefined') WB.cancelPending?.();
  S.routeRevision = (S.routeRevision || 0) + 1;
  homeCancelLoad();
  if (typeof cancelSessionReads === 'function') cancelSessionReads();
  if (typeof accountDailyDispose === 'function') accountDailyDispose();
  let r = requestedHash.slice(1).split('/')[0];
  // Every render replaces the page owner, including a refresh of this route.
  // Close its dialogs together with their state so no inert confirmation remains.
  if (typeof cancelActiveConfirmation === 'function') cancelActiveConfirmation();
  if (typeof WB !== 'undefined') { WB.launchGeneration += 1; WB.accountOpen = null; }
  $('#modalRoot').innerHTML = '';
  S.localEditorDraft = null;
  if (r === 'selfhost') {
    if (typeof RM !== 'undefined') RM.remoteMode = 'direct';
    history.replaceState(null, '', location.pathname + location.search + '#login');
    return route();
  }
  if (r !== 'login' && typeof selfhostDetach === 'function') selfhostDetach();
  if (!r || !PAGES[r] || !RENDER[r]) {
    S.route = 'home';
    $$('#nav a').forEach((a) => a.classList.toggle('active', a.dataset.route === 'home'));
    closePanel();
    if (typeof renderHome === 'function') void renderHome();
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
  // Re-entering the same page keeps what is on screen (as inert markup) until it repaints,
  // so a second route event never flashes an empty page.
  if (!changed) view.innerHTML = oldView.innerHTML || pageSkeleton(r);
  oldView.replaceWith(view);
  if (changed) {
    $('#headActions').innerHTML = '';
    view.innerHTML = pageSkeleton(r);
  }
  try {
    await RENDER[r](view);
  } catch (e) {
    view.innerHTML = `<div class="callout bad">${ico('warn')}<div>${esc(e.message)}</div></div>`;
  }
};

/* ---------- loading: every page first shows a skeleton shaped like its real content ---------- */
function pageSkeleton(r) {
  const line = (w, h = 12) => `<span class="sk" style="width:${w};height:${h}px"></span>`;
  const card = (n) => Array.from({ length: n }, () => `<div class="sk-card sk-key"><div class="sk-face sk"></div><div class="sk-body">${line('70%')}${line('45%')}<div class="sk-row">${line('28px', 28)}${line('28px', 28)}${line('28px', 28)}<i></i>${line('60px', 28)}</div></div></div>`).join('');
  const tiles = (n) => Array.from({ length: n }, () => `<div class="sk-card sk-tile"><div class="sk-head">${line('44px', 44)}<div class="sk-col">${line('60%', 14)}${line('40%')}</div></div><div class="sk-body">${line('100%', 34)}${line('100%', 34)}</div></div>`).join('');
  const rows = (n) => `<div class="sk-card sk-list">${Array.from({ length: n }, () => `<div class="sk-line">${line('34px', 34)}<div class="sk-col">${line('35%', 13)}${line('55%')}</div>${line('36px', 20)}</div>`).join('')}</div>`;
  const block = (n) => `<div class="sk-card sk-block">${line('28%', 14)}${Array.from({ length: n }, (_, i) => line(['92%', '76%', '84%', '64%'][i % 4])).join('')}</div>`;
  const body = {
    accounts: `<div class="sk-grid">${card(4)}</div>`,
    overview: `<div class="sk-grid">${tiles(4)}</div>`,
    mcp: rows(4),
    setup: `<div class="sk-grid">${tiles(6)}</div>`,
    usage: `<div class="sk-grid sk-stats">${Array.from({ length: 4 }, () => `<div class="sk-card sk-block">${line('45%')}${line('70%', 24)}</div>`).join('')}</div><div class="sk-card sk-block">${line('20%', 14)}<span class="sk" style="width:100%;height:200px"></span></div>`,
    sessions: rows(6),
  }[r] || `${block(3)}${block(4)}${block(2)}`;
  return `<div class="page-skeleton" aria-busy="true" aria-label="正在加载">${body}</div>`;
}

function goHome() { if (location.hash) history.pushState(null, '', location.pathname); route(); }
$('#panelClose').addEventListener('click', goHome);
addEventListener('keydown', (e) => {
  if (e.key !== 'Escape' || $('#modalRoot').children.length) return;
  if (document.body.classList.contains('panel-open')) goHome();
});
addEventListener('popstate', () => route());

/* ---------- small signals on the home screen ---------- */
const shellBadge = setApprovalBadge;
setApprovalBadge = function () {
  shellBadge();
  let dot = $('#orbAlert');
  if (S.approvals.length && !dot) {
    dot = document.createElement('span'); dot.id = 'orbAlert'; dot.className = 'orb-alert'; dot.title = '有待处理事项';
    zone.appendChild(dot);
  } else if (!S.approvals.length && dot) dot.remove();
  if (S.route === 'home') { homePending(); homeRenderRecent(); }
};
const shellConnection = setConn;
setConn = function (status) {
  shellConnection(status);
  if (S.route === 'home') homeRemoteStatus();
};

const SETUP_DOT = { timer: 0, delay: 500 };
async function refreshSetupDot() {
  clearTimeout(SETUP_DOT.timer);
  SETUP_DOT.timer = 0;
  if (document.hidden) return;
  try {
    const r = await api('/api/setup');
    const missing = (r.items || []).some((item) => item.required && !item.installed);
    $('#setupDot').hidden = !missing;
    HOME.needsSetup = missing;
    if (S.route === 'home') homePending();
    if (r.discoveryPending && S.route !== 'setup') {
      SETUP_DOT.timer = setTimeout(refreshSetupDot, SETUP_DOT.delay);
      SETUP_DOT.delay = Math.min(4000, SETUP_DOT.delay * 2);
    } else SETUP_DOT.delay = 500;
  } catch (_) { /* optional */ }
}
document.addEventListener('visibilitychange', () => {
  if (!document.hidden && S.route !== 'setup' && !SETUP_DOT.timer) void refreshSetupDot();
});
SETUP_DOT.timer = setTimeout(refreshSetupDot, 800);

/* "⋯" menus inside pages close on an outside click */
document.addEventListener('click', (e) => {
  $$('details.wb-card-more[open]').forEach((d) => { if (!d.contains(e.target)) d.open = false; });
});


/* ---------- home: local status, recent work and decisions ---------- */
const HOME = { seq: 0, needsSetup: false, sessions: null, sessionChanges: null, controller: null, error: '', loading: false };
function homeGreeting() {
  const h = new Date().getHours();
  return h < 5 ? '夜深了' : h < 11 ? '早上好' : h < 13 ? '中午好' : h < 18 ? '下午好' : '晚上好';
}
function homeSet(id, html) {
  const el = document.getElementById(id);
  if (el && el.homeMarkup !== html) {
    if (typeof patchSessionMarkup === 'function') patchSessionMarkup(el, html, 'home:' + id);
    else el.innerHTML = html;
    el.homeMarkup = html;
  }
}
function homeRemoteStatus() {
  homeSet('hsRemote', `<small>${esc($('#connPill .conn-label')?.textContent || '读取中')}</small>`);
}
function homePending() {
  const approvals = S.approvals || [];
  const count = approvals.length + (HOME.needsSetup ? 1 : 0);
  homeSet('homePendingCount', count ? `${count} 项` : '');
  const rows = approvals.slice(0, 3).map((a) => {
    const session = (HOME.sessions || []).find((s) => s.sessionKey === a.sessionKey);
    const tool = a.tool || session?.tool || '';
    return `<div class="home-pending">${icon('warn')}<div><b title="${esc(a.title || '需要你确认')}">${esc(a.title || '需要你确认')}</b><small>${a.kind === 'question' ? '有一个问题等你回答' : '操作正在等待确认'}</small></div><button type="button" data-act="homeOpenSession" data-k="${esc(a.sessionKey || '')}" data-tool="${esc(tool)}">查看</button></div>`;
  });
  if (approvals.length > 3) rows.push(`<p class="home-pending-note">另有 ${approvals.length - 3} 项，请在<a href="#sessions">对话中查看</a>。</p>`);
  if (HOME.needsSetup) rows.push(`<div class="home-pending">${icon('box')}<div><b>运行环境还未准备好</b><small>有必要组件尚未安装</small></div><a href="#setup">检查</a></div>`);
  if (S.approvalsLoadState !== 'ready') {
    rows.push(`<p class="home-pending-note">${S.approvalsLoadState === 'error' ? '待处理事项暂时无法同步。' : '正在同步待处理事项…'}</p>`);
  }
  homeSet('homePendingList', rows.length ? rows.join('') : `<div class="home-pending-empty"><span>${icon('check')}</span><b>暂无待处理事项</b><p>需要你确认时，会显示在这里</p></div>`);
}
function homeRenderRecent() {
  const box = $('#homeRecent');
  if (!box) return;
  if (!HOME.sessions && HOME.loading) return;
  box.hidden = false; box.removeAttribute('aria-busy');
  const error = HOME.error ? `<div class="home-empty" role="status">${icon('chat')}<div><b>${HOME.sessions ? '最近对话刷新失败，保留上次记录' : '暂时无法读取最近对话'}</b><p>${esc(HOME.error)}</p></div><button type="button" class="home-retry" data-act="homeRetry">重试</button></div>` : '';
  if (!HOME.sessions) { if (error) homeSet('homeRecentList', error); return; }
  const list = HOME.sessions.slice(0, 4);
  if (!list.length) {
    homeSet('homeRecentList', error || `<div class="home-empty">${icon('chat')}<div><b>还没有最近的对话</b><p>开始工作后，最近的对话会出现在这里。</p></div></div>`);
    return;
  }
  const approvals = new Set((S.approvals || []).map((a) => a.sessionKey));
  homeSet('homeRecentList', list.map((s) => {
    const family = s.tool === 'claude' ? 'claude' : s.tool === 'codex' ? 'codex' : '';
    const state = approvals.has(s.sessionKey) || s.status === 'waiting_approval' ? ['warn', '等待处理']
      : s.historyStale ? ['warn', '待刷新'] : s.status === 'running' ? ['run', '进行中'] : s.status === 'failed' ? ['warn', '失败']
      : s.status === 'unknown' ? ['', '状态未知'] : ['', typeof relTime === 'function' ? relTime(s.updatedAt) : ''];
    const mark = family ? `<img src="agent-brand/${family}-app-official.png" alt="" width="28" height="28">` : `<span class="hr-icon">${icon('chat')}</span>`;
    return `<button type="button" class="home-row" data-act="homeOpenSession" data-k="${esc(s.sessionKey)}" data-tool="${esc(s.tool || '')}">${mark}<span class="hr-main"><b>${esc(s.title || '无标题对话')}</b><small>${esc(s.cwd ? (typeof projectOf === 'function' ? projectOf(s.cwd) : s.cwd) : '未关联项目')}</small></span><span class="hr-state ${state[0]}">${esc(state[1])}</span><span class="hr-go">继续</span></button>`;
  }).join('') + error);
}
function homeMergeSessions(snapshot, changes) {
  const sessions = new Map(snapshot.map((s) => [s.sessionKey, s]));
  for (const [key, session] of changes || []) {
    const old = sessions.get(key);
    if (!old || !(old.updatedAt > session.updatedAt)) sessions.set(key, session);
  }
  return [...sessions.values()].sort((a, b) => b.updatedAt - a.updatedAt).slice(0, 100);
}
function homeSessionUpdated(session) {
  if (S.route !== 'home' || !session?.sessionKey) return;
  if (HOME.sessionChanges) {
    const prior = HOME.sessionChanges.get(session.sessionKey);
    if (!prior || !(prior.updatedAt > session.updatedAt)) HOME.sessionChanges.set(session.sessionKey, session);
  }
  if (!HOME.sessions) return;
  HOME.sessions = homeMergeSessions(HOME.sessions, [[session.sessionKey, session]]);
  homeRenderRecent(); homePending();
}
function homeCancelLoad() {
  HOME.controller?.abort();
  HOME.controller = null;
}
async function homeLoadStats(live, signal) {
  let local = S.local;
  if (!local) {
    const revision = S.localAccountsRevision = (S.localAccountsRevision || 0) + 1;
    const snapshot = await api('/api/local/accounts', undefined, undefined, { signal, timeoutMs: 15000 }).catch(() => null);
    if (!live()) return;
    if (revision === S.localAccountsRevision && snapshot) S.local = snapshot;
    local = S.local;
  }
  if (!live()) return;
  if (!local) { homeSet('hsAgents', '<small>暂不可用</small>'); homeSet('hsKeys', '<small>暂不可用</small>'); }
  else {
    homeSet('hsAgents', `${(local.tools || []).filter((t) => t.available).length}<small>个已就绪</small>`);
    const keys = (local.accounts || []).length;
    homeSet('hsKeys', keys ? `${keys}<small>把已保存</small>` : '<small>还没有密钥</small>');
  }
}
async function renderHome() {
  homeCancelLoad();
  const controller = new AbortController();
  HOME.controller = controller;
  const seq = ++HOME.seq, routeRevision = S.routeRevision, live = () => seq === HOME.seq && S.route === 'home' && S.routeRevision === routeRevision;
  const changes = new Map();
  HOME.sessionChanges = changes;
  HOME.loading = true;
  HOME.error = '';
  if (!HOME.sessions && seq > 1) homeSet('homeRecentList', '<div class="home-empty" role="status" aria-busy="true"><span class="spin"></span>正在读取本机对话…</div>');
  try {
    const title = $('#homeTitle');
    if (title) title.textContent = `${homeGreeting()}，开始今天的工作`;
    homePending(); homeRemoteStatus();
    homeRenderRecent();
    // Local history must start before optional configuration/usage discovery.
    const recent = api('/api/sessions', undefined, undefined, { signal: controller.signal, timeoutMs: 35000 });
    void homeLoadStats(live, controller.signal);
    if (typeof USAGE !== 'undefined') {
      Promise.resolve().then(() => USAGE.load(1, false, '', controller.signal)).then((u) => {
        if (!live()) return;
        const t = u.today;
        homeSet('hsTokens', t && t.tokens !== null ? `${USAGE.fmtTokens(t.tokens)}<small>${t.requests !== null ? ' · ' + t.requests.toLocaleString('zh-CN') + ' 次' : ''}</small>` : '<small>暂无数据</small>');
      }).catch(() => { if (live()) homeSet('hsTokens', '<small>暂无数据</small>'); });
    }
    try {
      const r = await recent;
      if (!live()) return;
      if (!Array.isArray(r.sessions)) throw new Error('本机会话响应不完整，请重试');
      const partial = Array.isArray(r.errors) && r.errors.length > 0;
      const failed = new Set(Array.isArray(r.failedTools) ? r.failedTools : []);
      const retained = partial ? (HOME.sessions || []).filter(s => !failed.size || failed.has(s.tool)).map(s => ({ ...s, historyStale: true })) : [];
      const snapshot = new Map(retained.map(s => [s.sessionKey, s]));
      for (const session of r.sessions) snapshot.set(session.sessionKey, session);
      HOME.sessions = homeMergeSessions([...snapshot.values()], changes);
      HOME.loading = false;
      HOME.error = (r.errors || []).join('；');
      homeRenderRecent(); homePending();
    } catch (error) {
      if (live() && !controller.signal.aborted) {
        HOME.loading = false;
        HOME.error = error.message || '已有记录仍保存在原工具中，请重试。';
        homeRenderRecent();
      }
    }
  } finally {
    if (HOME.sessionChanges === changes) HOME.sessionChanges = null;
  }
}
ACTIONS.homeRetry = () => { if (S.route === 'home') void renderHome(); };
ACTIONS.homeOpenSession = (b) => {
  const key = b.dataset.k || '';
  const inferred = key.startsWith('claude:') ? 'claude' : key.startsWith('codex:') ? 'codex' : '';
  S.sessFilter = b.dataset.tool || inferred;
  if (typeof RM !== 'undefined') { RM.tool = S.sessFilter; RM.newThread = false; }
  S.openKey = key || null;
  S.openInfo = null;
  location.hash = 'sessions';
};
