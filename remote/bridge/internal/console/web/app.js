'use strict';
/* Salcara Bridge console — plain JS, no build step. Every dynamic string goes through esc(). */

const $ = (s, el = document) => el.querySelector(s);
const $$ = (s, el = document) => Array.from(el.querySelectorAll(s));
const esc = (v) => String(v ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

const ICONS = {
  spark: '<path d="M10.5 6Q11.4 12.6 18 13.5Q11.4 14.4 10.5 21Q9.6 14.4 3 13.5Q9.6 12.6 10.5 6Z"/><path d="M18.75 3.5v3.5M17 5.25h3.5"/>',
  home: '<path d="M4 10.2 12 4l8 6.2V19a1 1 0 0 1-1 1h-4.5v-5.5h-5V20H5a1 1 0 0 1-1-1z"/>',
  key: '<circle cx="8.5" cy="15.5" r="4.5"/><path d="M11.7 12.3 20 4M16.5 7.5l2.5 2.5M14.2 9.8l1.8 1.8"/>',
  plug: '<path d="M9 2v5M15 2v5M6 7h12v4a6 6 0 0 1-12 0zM12 17v5"/>',
  folder: '<path d="M3 6.5A1.5 1.5 0 0 1 4.5 5H9l2 2.5h8.5A1.5 1.5 0 0 1 21 9v9.5a1.5 1.5 0 0 1-1.5 1.5h-15A1.5 1.5 0 0 1 3 18.5z"/>',
  chat: '<path d="M4 5h16v11H9l-5 4z"/><path d="M8 9.5h8M8 12.5h5"/>',
  gear: '<path d="M4 7.5h9M17 7.5h3M4 16.5h3M11 16.5h9"/><circle cx="15" cy="7.5" r="2"/><circle cx="9" cy="16.5" r="2"/>',
  edit: '<path d="M4 20h4L19 9l-4-4L4 16z"/><path d="m13.5 6.5 4 4"/>',
  copy: '<rect x="8" y="8" width="12" height="12" rx="2"/><path d="M16 8V5a1 1 0 0 0-1-1H5a1 1 0 0 0-1 1v10a1 1 0 0 0 1 1h3"/>',
  eye: '<path d="M2 12s3.5-7 10-7 10 7 10 7-3.5 7-10 7S2 12 2 12z"/><circle cx="12" cy="12" r="3"/>',
  info: '<circle cx="12" cy="12" r="9"/><path d="M12 11v6M12 7.5v.5"/>',
  warn: '<path d="M12 3 2 20h20z"/><path d="M12 10v5M12 17.5v.5"/>',
  check: '<circle cx="12" cy="12" r="9"/><path d="m8 12 3 3 5-6"/>',
  phone: '<rect x="6.5" y="3" width="11" height="18" rx="2.5"/><path d="M10.5 17.5h3"/>',
  trash: '<path d="M4 7h16M9 7V4h6v3M6 7l1 13h10l1-13"/>',
  plus: '<path d="M12 5v14M5 12h14"/>',
  download: '<path d="M12 4v11M7.5 10.5 12 15l4.5-4.5"/><path d="M5 19.5h14"/>',
  refresh: '<path d="M20 11a8 8 0 0 0-14.6-4.5L4 8M4 4v4h4M4 13a8 8 0 0 0 14.6 4.5L20 16M20 20v-4h-4"/>',
  chev: '<path d="m9 6 6 6-6 6"/>',
  up: '<path d="M12 19V5M6 11l6-6 6 6"/>',
  terminal: '<rect x="3" y="4" width="18" height="16" rx="2"/><path d="m7 9 3 3-3 3M13 15h4"/>',
  file: '<path d="M6 3h8l5 5v13H6z"/><path d="M14 3v5h5"/>',
  search: '<circle cx="11" cy="11" r="6"/><path d="m20 20-4.5-4.5"/>',
  globe: '<circle cx="12" cy="12" r="9"/><path d="M3 12h18M12 3a14 14 0 0 1 0 18M12 3a14 14 0 0 0 0 18"/>',
  box: '<path d="M12 3.2 19.8 7.6v8.8L12 20.8l-7.8-4.4V7.6z"/><path d="M4.2 7.6 12 12l7.8-4.4M12 12v8.8"/>',
  stop: '<rect x="6" y="6" width="12" height="12" rx="2"/>',
  send: '<path d="M4 12 20 4l-6 16-3-7z"/>',
  power: '<path d="M12 3v9"/><path d="M6.3 7.5a8 8 0 1 0 11.4 0"/>',
  wallet: '<rect x="3" y="6" width="18" height="14" rx="2"/><path d="M16 13h2M3 10h18M6 6l9-3 1 3"/>',
  bolt: '<path d="M13 2 4 14h7l-1 8 9-12h-7z"/>',
  clock: '<circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/>',
  chart: '<path d="M4 19.5h16"/><path d="m5 15 4.5-4.5 3.5 3 6-6.5"/><path d="M15.5 7H19v3.5"/>',
};
const icon = (n) => `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">${ICONS[n] || ''}</svg>`;
const ico = (n) => `<span class="ico">${icon(n)}</span>`;

const STATUS = {
  connected: '已连接',
  connecting: '连接中',
  not_logged_in: '本地模式',
  invalid_key: 'Key 无效',
};
const TOOL_LABEL = { codex: 'Codex CLI', 'codex-desktop': 'Codex Desktop', claude: 'Claude Code', 'claude-desktop': 'Claude Desktop' };
const SESSION_STATUS = { running: '运行中', idle: '空闲', waiting_approval: '等待审批', failed: '失败' };

const S = {
  state: null,
  route: 'overview',
  sessions: [],
  sessFilter: '',
  sessLoading: false,
  openKey: null,
  openInfo: null,
  tl: [],
  tlIndex: new Map(),
  approvals: [],
  revealed: {},
  editingName: false,
  pairCode: null,
  pairExpires: 0,
  pairQR: '',
  remoteDiscovery: null,
  local: null,
  toolcfg: null,
  localFilter: '',
  localKind: '',
  localDraftModels: [],
  sessSearch: '',
  restoreTarget: '',
  restoreSessions: [],
  restoreSearch: '',
  bindingLoading: false,
};

/* ---------------- utilities ---------------- */

async function api(path, body, method) {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), 65000);
  const opts = { method: method || (body !== undefined ? 'POST' : 'GET'), headers: {}, credentials: 'same-origin', signal: controller.signal };
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  try {
    const res = await fetch(path, opts);
    let data = null;
    try { data = await res.json(); } catch (error) { if (controller.signal.aborted) throw error; }
    if (!res.ok) throw new Error((data && data.error) || `请求失败 (${res.status})`);
    if (data === null) throw new Error('本机响应不完整，请重试');
    return data;
  } catch (e) {
    if (controller.signal.aborted) throw new Error('本机响应超时，请重试');
    if (e instanceof TypeError) throw new Error('连不上 Salcara Bridge，程序可能已经退出');
    throw e;
  } finally { clearTimeout(timer); }
}

/* 系统相关的文案：Finder / 资源管理器 / 文件管理器 */
const OSX = () => (S.state && S.state.os) || '';
const fileMgr = () => ({ darwin: '访达', windows: '资源管理器' })[OSX()] || '文件管理器';
const openWord = () => OSX() === 'darwin' ? '在访达中显示' : '打开文件夹';
const launchHint = () => OSX() === 'darwin' ? '在“应用程序”里打开 Salcara Bridge' : '双击程序';

function toast(msg, kind) {
  const el = document.createElement('div');
  el.className = 'toast ' + (kind || '');
  el.textContent = msg;
  $('#toastRoot').appendChild(el);
  setTimeout(() => el.remove(), kind === 'bad' ? 5200 : 3000);
}

async function copyText(text, what) {
  try {
    await navigator.clipboard.writeText(text);
  } catch (_) {
    const ta = document.createElement('textarea');
    ta.value = text;
    document.body.appendChild(ta);
    ta.select();
    document.execCommand('copy');
    ta.remove();
  }
  toast(`已复制${what ? ' ' + what : ''}`, 'ok');
}

function relTime(ms) {
  if (!ms) return '';
  const d = Date.now() - ms;
  if (d < 60e3) return '刚刚';
  if (d < 3600e3) return Math.floor(d / 60e3) + ' 分钟前';
  if (d < 86400e3) return Math.floor(d / 3600e3) + ' 小时前';
  if (d < 7 * 86400e3) return Math.floor(d / 86400e3) + ' 天前';
  const t = new Date(ms);
  return `${t.getFullYear()}-${String(t.getMonth() + 1).padStart(2, '0')}-${String(t.getDate()).padStart(2, '0')}`;
}

const num = (v) => (typeof v === 'number' && isFinite(v) ? v : (typeof v === 'string' && v.trim() !== '' && isFinite(+v) ? +v : null));
function money(v, unit) {
  const n = num(v);
  if (n === null) return '—';
  const u = (unit || 'USD').toUpperCase();
  const s = Math.abs(n) >= 100 ? n.toFixed(0) : Math.abs(n) >= 1 ? n.toFixed(2) : n.toFixed(4).replace(/0+$/, '').replace(/\.$/, '.00');
  return u === 'USD' ? '$' + s : s + ' ' + u;
}
function compact(v) {
  const n = num(v);
  if (n === null) return '—';
  if (n >= 1e9) return (n / 1e9).toFixed(2) + 'B';
  if (n >= 1e6) return (n / 1e6).toFixed(2) + 'M';
  if (n >= 1e4) return (n / 1e3).toFixed(1) + 'K';
  return String(Math.round(n));
}
function pct(used, limit) {
  const u = num(used), l = num(limit);
  if (u === null || !l) return null;
  return Math.max(0, Math.min(100, (u / l) * 100));
}
function bar(p) {
  if (p === null) return '';
  const cls = p >= 90 ? 'bad' : p >= 70 ? 'warn' : '';
  return `<div class="bar ${cls}"><i style="width:${p.toFixed(1)}%"></i></div>`;
}

function statusPill(st) {
  const s = (st && st.state) || 'not_logged_in';
  return `<span class="st-${esc(s)}"><span class="dot"></span></span>`;
}

function toolChip(tool, client) {
  let cls = 't-codex', label = client || TOOL_LABEL[tool] || tool;
  if (tool === 'claude') cls = /desktop/i.test(client || '') ? 't-desktop' : 't-claude';
  return `<span class="chip ${cls}">${esc(label)}</span>`;
}

function renderText(t) {
  // minimal markdown: fenced code blocks + inline code
  const parts = String(t || '').split(/```/);
  return parts.map((p, i) => {
    if (i % 2 === 1) return `<pre>${esc(p.replace(/^[\w-]*\n/, ''))}</pre>`;
    return esc(p).replace(/`([^`\n]+)`/g, '<code>$1</code>');
  }).join('');
}

function renderDiff(d) {
  return String(d || '').split('\n').map((l) => {
    const c = l.startsWith('+') && !l.startsWith('+++') ? 'add' : l.startsWith('-') && !l.startsWith('---') ? 'del' : l.startsWith('@@') ? 'hunk' : '';
    return `<span class="${c}">${esc(l)}</span>`;
  }).join('\n');
}

/* ---------------- shell ---------------- */

const PAGES = {
  overview: { title: 'Agent', sub: '给每个工具选择 API，原地重启或恢复旧会话；手机远程连接是可选功能' },
  accounts: { title: 'API 密钥', sub: '每个密钥都可命名，不归属于某个工具；在 Agent 页选择密钥并确认配置' },
  login: { title: '远程连接（可选）', sub: '输入站点、检测插件、扫码绑定；本地密钥管理与启动不受影响' },
  tools: { title: '工具配置', sub: '一键把本机的 Claude Code / Codex 接入中转站' },
  projects: { title: '项目文件夹', sub: '手机只能在这些文件夹里发起任务' },
  sessions: { title: '会话恢复', sub: '查看原有本地 Codex / Claude Code 记录，恢复同一个会话继续工作' },
  settings: { title: '设置', sub: '开机自启、审批策略、日志与退出' },
  usage: { title: '用量', sub: '模型、API 与中转站额度' },
};

function setConn(st) {
  const s = (st && st.state) || 'not_logged_in';
  const label = s === 'invalid_key' && S.state?.config?.remoteDeviceOnly ? '设备验证失败' : STATUS[s] || s;
  const pill = $('#connPill');
  pill.className = 'conn st-' + s;
  $('.conn-label', pill).textContent = label;
  pill.title = (st && st.error) || '';
  const hero = $('#heroStatus');
  if (hero) {
    hero.className = 'hero-status st-' + s;
    $('.lbl', hero).textContent = label;
    const er = $('#heroErr');
    if (er) er.textContent = st && st.error && s !== 'connected' ? st.error : '';
  }
  if (S.route === 'login') {
    const text = $('#remoteStatusLabel'); if (text) text.textContent = label;
    $$('.pair-card [data-act]').forEach((button) => { if (!button.querySelector('.spin')) button.disabled = s !== 'connected'; });
  }
}

function setApprovalBadge() {
  const b = $('#approvalBadge');
  b.hidden = S.approvals.length === 0;
  b.textContent = S.approvals.length;
}

let stateRequest = null;
async function loadState() {
  if (!stateRequest) stateRequest = api('/api/state').finally(() => { stateRequest = null; });
  S.state = await stateRequest;
  setConn(S.state.hub);
  $('#sideVer').textContent = `v${S.state.version} · ${({ windows: 'Windows', darwin: 'macOS', linux: 'Linux' })[S.state.os] || S.state.os}`;
  return S.state;
}

async function route() {
  const r = (location.hash || '#overview').slice(1).split('/')[0];
  S.route = PAGES[r] ? r : 'overview';
  $$('#nav a').forEach((a) => a.classList.toggle('active', a.dataset.route === S.route));
  $('#pageTitle').textContent = PAGES[S.route].title;
  $('#pageSub').textContent = PAGES[S.route].sub;
  $('#headActions').innerHTML = '';
  const view = $('#view');
  view.innerHTML = '<div class="card"><div class="skeleton" style="width:40%"></div><div class="skeleton" style="width:70%;margin-top:12px"></div></div>';
  try {
    await RENDER[S.route](view);
  } catch (e) {
    view.innerHTML = `<div class="callout bad">${ico('warn')}<div>${esc(e.message)}</div></div>`;
  }
}

/* ---------------- 概览 ---------------- */

function usageCards(u, err) {
  if (err) return `<div class="callout warn">${ico('warn')}<div>读取中转站用量失败：${esc(err)}</div></div>`;
  if (!u) return '';
  const unit = u.unit || 'USD';
  const cards = [];
  const today = (u.usage && u.usage.today) || {};
  const total = (u.usage && u.usage.total) || {};
  if (num(u.remaining) !== null || num(u.balance) !== null) {
    cards.push(`<div class="card stat"><div class="stat-label">${ico('wallet')}${esc(u.planName || '剩余额度')}</div>
      <div class="stat-value">${money(num(u.remaining) ?? u.balance, unit)}</div>
      <div class="stat-foot">${u.mode === 'quota_limited' ? 'Key 限额模式' : '账户可用'}</div></div>`);
  }
  if (u.quota && typeof u.quota === 'object') {
    const p = pct(u.quota.used, u.quota.limit);
    cards.push(`<div class="card stat"><div class="stat-label">${ico('key')}Key 总额度</div>
      <div class="stat-value">${money(u.quota.used, u.quota.unit || unit)}<span class="muted small"> / ${money(u.quota.limit, u.quota.unit || unit)}</span></div>${bar(p)}</div>`);
  }
  if (today && (today.cost !== undefined || today.requests !== undefined)) {
    cards.push(`<div class="card stat"><div class="stat-label">${ico('bolt')}今日消费</div>
      <div class="stat-value">${money(num(today.actual_cost) ?? today.cost, unit)}</div>
      <div class="stat-foot">${compact(today.requests)} 次请求 · ${compact(today.total_tokens)} tokens</div></div>`);
  }
  if (total && (total.cost !== undefined || total.requests !== undefined)) {
    cards.push(`<div class="card stat"><div class="stat-label">${ico('clock')}累计消费</div>
      <div class="stat-value">${money(num(total.actual_cost) ?? total.cost, unit)}</div>
      <div class="stat-foot">${compact(total.requests)} 次请求 · ${compact(total.total_tokens)} tokens</div></div>`);
  }
  const sub = u.subscription;
  if (sub && typeof sub === 'object') {
    [['daily', '今日'], ['weekly', '本周'], ['monthly', '本月']].forEach(([k, label]) => {
      const lim = num(sub[k + '_limit_usd']);
      if (lim) {
        cards.push(`<div class="card stat"><div class="stat-label">${ico('clock')}订阅 · ${label}</div>
          <div class="stat-value">${money(sub[k + '_usage_usd'], 'USD')}<span class="muted small"> / ${money(lim, 'USD')}</span></div>${bar(pct(sub[k + '_usage_usd'], lim))}</div>`);
      }
    });
  }
  if (Array.isArray(u.rate_limits)) {
    u.rate_limits.forEach((r) => {
      cards.push(`<div class="card stat"><div class="stat-label">${ico('clock')}${esc(r.window)} 限额</div>
        <div class="stat-value">${money(r.used, 'USD')}<span class="muted small"> / ${money(r.limit, 'USD')}</span></div>${bar(pct(r.used, r.limit))}</div>`);
    });
  }
  if (!cards.length) return '';
  let extra = '';
  if (u.days_until_expiry !== undefined && u.days_until_expiry !== null) extra = `<div class="muted small">Key 还有 ${esc(u.days_until_expiry)} 天到期</div>`;
  return `<div class="grid grid-auto">${cards.slice(0, 8).join('')}</div>${extra}`;
}

function toolCard(t, extra) {
  const cls = t.id === 'codex' ? 'codex' : t.id === 'claude' ? 'claude' : 'desktop';
  const letter = t.id === 'codex' ? 'Cx' : t.id === 'claude' ? 'CC' : 'CD';
  return `<div class="card tool-card">
    <div class="tool-head"><span class="tool-logo ${cls}">${letter}</span>
      <div><div class="tool-name">${esc(t.name || TOOL_LABEL[t.id])}</div>
      <div class="muted small">${t.available ? (t.version ? '版本 ' + esc(t.version) : '已安装') : '没有在这台电脑上找到'}</div></div>
      <span class="spacer"></span>${t.available ? '<span class="chip ok">已安装</span>' : '<span class="chip">未安装</span>'}</div>
    ${extra || ''}</div>`;
}

RENDER_overview = async function (view) {
  const [st, local] = await Promise.all([loadState(), api('/api/local/accounts')]);
  S.local = local;
  const c = st.config || {};
  const hub = st.hub || {};
  const s = hub.state || 'not_logged_in';
  $('#headActions').innerHTML = `<button class="btn" data-act="refresh">${icon('refresh')}重新检测</button><a class="btn primary" href="#accounts">${icon('key')}API 密钥库</a>`;

  view.innerHTML = `
  <div class="callout">${ico('info')}<div>先在密钥库保存 API，再在下面的工具卡片里选择并重启。沿用原工具的数据目录，不新建空实例。<br><span class="small muted">已保存 ${local.accounts.length} 个 API · 本地使用无需登录远程服务。切换前请结束任务并保存内容；Codex 桌面版与 CLI 共用原凭据。</span></div></div>
  <div id="approvalsBox"></div>
  <div class="agent-grid" id="agentToolList"></div>
  <div class="card"><div class="row"><div class="folder-ico">${icon('chat')}</div>
    <div class="li-main"><div class="li-title" id="sessCount">正在统计会话…</div><div class="li-sub">读取原有 Codex / Claude Code 本地记录，按原会话编号恢复，不迁移项目文件</div></div>
    <a class="btn" href="#sessions">会话恢复</a></div></div>
  ${localToolPathsPanel(local)}${localBackupPanel(st)}
  <details class="remote-overview"${c.loggedIn ? ' open' : ''}><summary>手机远程连接（可选）${c.loggedIn ? ' · ' + esc(STATUS[s] || s) : ' · 未启用'}</summary>
  <div class="card hero">
    <div class="hero-top">
      <span id="heroStatus" class="hero-status st-${esc(s)}"><span class="dot"></span><span class="lbl">${esc(STATUS[s] || s)}</span></span>
      <span class="hero-meta">${c.relayRoot ? esc(c.relayRoot.replace(/^https?:\/\//, '')) : '还没有连接中转站'}</span>
      <span class="spacer"></span>
      ${c.loggedIn ? '' : `<a class="btn sm" href="#login" style="background:#fff;color:#3D7BFA;border:none">连接站点</a>`}
    </div>
    <div class="hero-device" id="devName">${S.editingName
      ? `<input class="input inline-input" id="devInput" value="${esc(c.deviceName)}" maxlength="40"><button class="btn sm" data-act="saveName" style="background:#fff;color:#3D7BFA;border:none">保存</button><button class="btn sm ghost" data-act="cancelName" style="color:#fff">取消</button>`
      : `${esc(c.deviceName)}<button class="icon-btn" data-act="editName" title="修改设备名称">${icon('edit')}</button>`}</div>
    <div class="hero-meta" id="heroErr">${hub.error && s !== 'connected' ? esc(hub.error) : ''}</div>
    <div class="hero-hint">${ico('phone')}<div>先检测兼容插件并建立电脑连接，再在手机扫码绑定。设备凭证与模型 API Key 分开。</div></div>
  </div>
  <div class="card pair-card"><div class="row"><div class="li-main"><div class="li-title">扫码绑定这台电脑</div><div class="li-sub">二维码 5 分钟有效、一次性使用，可随时解绑；不需要手机填写模型 Key。</div></div><a class="btn primary" href="#login">打开远程连接</a></div></div>
  <div id="usageBox">${c.loggedIn && !c.remoteDeviceOnly ? '<div class="grid grid-4">' + '<div class="card stat"><div class="skeleton"></div><div class="skeleton" style="margin-top:12px;height:24px;width:60%"></div></div>'.repeat(4) + '</div>' : ''}</div></details>`;
  renderAgentCards();
  renderApprovalsBox();
  if (c.loggedIn && !c.remoteDeviceOnly) {
    api('/api/usage').then((r) => { const b = $('#usageBox'); if (b) b.innerHTML = usageCards(r.usage, r.error); }).catch((e) => { const b = $('#usageBox'); if (b) b.innerHTML = usageCards(null, e.message); });
  }
  api('/api/sessions').then((r) => {
    const el = $('#sessCount');
    if (!el) return;
    const list = r.sessions || [];
    const running = list.filter((x) => x.status === 'running' || x.status === 'waiting_approval').length;
    el.textContent = `最近 ${list.length} 个会话` + (running ? ` · ${running} 个正在运行` : '');
  }).catch(() => { const el = $('#sessCount'); if (el) el.textContent = '会话读取失败'; });
};

function renderApprovalsBox() {
  const box = $('#approvalsBox');
  if (!box) return;
  if (!S.approvals.length) { box.innerHTML = ''; return; }
  box.innerHTML = `<div class="card appr-banner"><div class="card-head"><h3 class="card-title">${ico('warn')}有 ${S.approvals.length} 个操作等待批准</h3><a href="#sessions" class="btn sm">打开会话</a></div>
    ${S.approvals.map((a) => `<div class="appr-row"><div class="li-main"><div class="li-title">${esc(a.title || '需要批准')}</div><div class="li-sub">${esc(a.sessionKey)}${a.cwd ? ' · ' + esc(a.cwd) : ''}</div></div>
    <button class="btn sm primary" data-act="approve" data-id="${esc(a.approvalId)}" data-d="allow">允许</button>
    <button class="btn sm danger" data-act="approve" data-id="${esc(a.approvalId)}" data-d="deny">拒绝</button></div>`).join('')}</div>`;
}

/* ---------------- 模型 / API ---------------- */

function remoteLampHTML(remote, capability = 'cli') {
  const r = remote || { state: 'off', label: '读取中', detail: '' };
  return `<span class="remote-lamp lamp-${esc(r.state)}" data-remote-capability="${esc(capability)}" title="${esc(r.detail)}"><span class="lamp-dot" aria-hidden="true"></span>${esc(r.label)}</span>`;
}

function desktopRemoteLamp(tool) {
  // Codex App, CLI and IDE share one thread store: the phone continues Codex App
  // threads through the same Codex worker, so the desktop card mirrors the Codex lamp.
  if (tool.id === 'codex-desktop') {
    const cli = S.local?.bindings?.codex?.remote;
    if (cli && cli.state === 'connected') return { state: 'connected', label: '手机可继续', detail: '手机远程能看到并继续 Codex App 的对话（与 Codex CLI 共用对话记录）。', supported: true };
    return cli ? Object.assign({}, cli, { detail: '手机远程通过 Codex 继续 Codex App 的对话。' + (cli.detail || '') }) : { state: 'off', label: '手机远程未连接', detail: '在「手机远程」连接中转站并扫码后，手机就能继续 Codex App 的对话。', supported: true };
  }
  // Claude Desktop's Code tab runs Claude Code sessions (stored in ~/.claude/projects), so the phone
  // continues them through the Claude Code agent. Ordinary chats live in Anthropic's cloud.
  const cc = S.local?.bindings?.claude?.remote;
  if (cc && cc.state === 'connected') return { state: 'connected', label: '手机可继续', detail: '手机选择 Claude Desktop 后，能看到并继续它「Code」里的会话，也能新建；手机发出的会话同样带 Claude Desktop 标记。聊天（Chat）不在手机远程范围内。', supported: true };
  return { state: 'off', label: '手机远程未连接', detail: '连接后，手机选择 Claude Desktop 就能继续它「Code」里的会话（需要电脑上装有 Claude Code）。', supported: true };
}

function claudeDesktopApplyNote() {
  return '当前 Claude Desktop 的第三方配置与聊天库兼容性尚未接入；切换部署模式可能改变原聊天库的可见性。请在原应用的 Developer → Configure Third-Party Inference 中手动配置。Bridge 不会自动改写或重启 Claude Desktop；Claude Code 不受影响。';
}

function agentCard(t) {
  const binding = S.local.bindings?.[t.id] || {};
  const accounts = S.local.accounts;
  const a = accounts.find((a) => a.id === binding.accountId);
  const applied = S.local.accounts.find((a) => a.id === binding.appliedId);
  const backendAvailable = S.local.tools.some((x) => x.id === t.kind && x.available);
  const codex = t.kind === 'codex';
  const desktop = t.id.endsWith('-desktop');
  const autoApply = t.id !== 'claude-desktop';
  const restoreTarget = t.id === 'claude-desktop' ? 'claude' : t.id;
  const protocol = binding.protocol || (codex ? 'responses' : 'anthropic');
  const adapted = protocol !== (codex ? 'responses' : 'anthropic');
  return `<article class="card agent-card" data-tool="${esc(t.id)}" data-available="${t.available}"><div class="tool-head"><span class="tool-logo ${codex ? 'codex' : desktop ? 'desktop' : 'claude'}">${codex ? 'Cx' : desktop ? 'CD' : 'CC'}</span><div class="li-main"><h3 class="tool-name">${esc(t.name)}</h3><div class="muted small">${desktop ? '桌面应用' : '命令行工具'} · 模型不限制品牌</div></div><span class="chip ${t.available ? 'ok' : ''}">${t.available ? '已安装' : '未检测到'}</span></div>
    <label class="agent-api-label" for="toolAPI-${esc(t.id)}">使用的 API</label><div class="agent-api-row"><select class="input" id="toolAPI-${esc(t.id)}" data-target="${esc(t.id)}" aria-label="${esc(t.name)}使用的 API"><option value="">${accounts.length ? '请选择 API…' : '先在密钥库添加 API'}</option>${accounts.map((x) => `<option value="${esc(x.id)}" ${x.id === binding.accountId ? 'selected' : ''}>${esc(x.name)}</option>`).join('')}</select><div id="lamp-${esc(t.id)}">${remoteLampHTML(desktop ? desktopRemoteLamp(t) : binding.remote, desktop ? 'desktop' : 'cli')}</div></div>
    <div class="agent-config" id="agentConfig-${esc(t.id)}">${a ? `<div title="${esc(a.baseUrl)}">${ico('globe')}<span>${esc(a.baseUrl)}</span></div><div>${ico('key')}<code>${esc(a.keyMasked)}</code></div>` : '<div class="muted">选择已命名的密钥；模型在原工具内选择。</div>'}</div>
    <div class="agent-protocol"><label for="toolProtocol-${esc(t.id)}">服务商提供的上游接口</label><select class="input" id="toolProtocol-${esc(t.id)}" data-target="${esc(t.id)}" aria-label="${esc(t.name)}上游接口">${[['responses', 'OpenAI Responses（兼容中转站）'], ['chat', 'OpenAI Chat Completions（Grok / DeepSeek 等）'], ['anthropic', 'Anthropic Messages（Claude 等）']].map(([v,l]) => `<option value="${v}" ${protocol === v ? 'selected' : ''}>${l}</option>`).join('')}</select><span class="help">${!autoApply ? '这里的选择仅保存在 Bridge，须在原桌面应用中手动设置；不会自动应用。' : adapted ? '经 CLIProxyAPI 本地转换；无需远程登录，但 Bridge 必须保持运行。' : '原生接口直接连接服务商，不按模型品牌过滤。'}</span></div>
    <div class="agent-applied" id="agentApplied-${esc(t.id)}">${!autoApply ? '<span class="chip warn">自动应用暂不可用</span>' : binding.pending ? `<span class="chip warn">${a ? '选择已保存，等待应用' : '未选择 API'}</span>${applied ? `<span class="muted small">当前应用：${esc(applied.name)}</span>` : ''}` : `<span class="chip ok">已应用 ${esc(applied?.name || '')}</span>`}</div>
    <div class="agent-actions"><button class="btn primary" data-act="toolRestart" data-target="${esc(t.id)}" ${autoApply && a && binding.model && t.available ? '' : 'disabled'}>${icon('refresh')}${autoApply ? '应用并重启' : '自动应用暂不可用'}</button><button class="btn" data-act="toolRestore" data-target="${esc(restoreTarget)}" ${backendAvailable ? '' : 'disabled'}>${icon('clock')}${desktop ? 'CLI 恢复会话' : '恢复会话'}</button>${a ? `<button class="icon-btn" data-act="localEdit" data-id="${esc(a.id)}" title="编辑这个密钥" aria-label="编辑这个密钥">${icon('edit')}</button>` : '<button class="btn sm" data-act="localNew">添加密钥</button>'}</div>
    <p class="agent-note muted small" id="remoteDetail-${esc(t.id)}">${esc(desktop ? desktopRemoteLamp(t).detail : binding.remote?.detail || '')}</p>
    ${!autoApply ? `<p class="agent-note small">${esc(claudeDesktopApplyNote())}</p><p class="agent-note small">CLI 恢复会话仅支持本地 Code 记录，使用 Claude Code 卡片已应用的 API 并通过 Claude Code 终端打开；普通云端聊天仍在原桌面应用里查看，不能通过 Bridge 遥控。</p>` : desktop ? '<p class="agent-note muted small">重启原桌面应用保留本地历史；指定旧会话的恢复通过原 CLI 打开。</p>' : '<p class="agent-note muted small">打开原终端会话列表；不会关闭其他终端，请先手动结束旧任务。</p>'}
  </article>`;
}

function renderAgentCards() {
  const box = $('#agentToolList');
  if (box && S.local) {
    box.innerHTML = S.local.tools.map(agentCard).join('');
  }
}

async function refreshWorkbenchStatus() {
  if (S.route !== 'overview' || S.bindingLoading) return;
  S.bindingLoading = true;
  try {
    const local = await api('/api/local/accounts');
    if (S.route !== 'overview') return;
    S.local = local;
    for (const t of local.tools) {
      const b = local.bindings?.[t.id];
      const lamp = document.getElementById('lamp-' + t.id);
      const detail = document.getElementById('remoteDetail-' + t.id);
      if (lamp) {
        if (typeof wbRefreshCardStatus === 'function') wbRefreshCardStatus(t.id);
        else toolDraftStatus(t.id);
      }
      if (detail) detail.textContent = t.id.endsWith('-desktop') ? desktopRemoteLamp(t).detail : b?.remote?.detail || '';
      const cliLamp = document.getElementById('cliLamp-' + t.id);
      const cliDetail = document.getElementById('cliRemoteDetail-' + t.id);
      if (cliLamp) cliLamp.innerHTML = remoteLampHTML(b?.cliRemote);
      if (cliDetail) cliDetail.textContent = (b?.cliRemote?.detail || '') + ' 此项使用对应 CLI 卡片的 API 设置，不是桌面远控。';
    }
    return local;
  } catch (_) { /* connection pill reports a disconnected core */ }
  finally { S.bindingLoading = false; }
}

function localToolPathsPanel(local) {
  return `<div class="card"><details class="adv"><summary><span class="chev">${icon('chev')}</span>工具检测与自定义程序路径</summary><div class="local-tools">${local.tools.map((t) => `<div class="field"><label for="localPath-${esc(t.id)}">${esc(t.name)} <span class="chip ${t.available ? 'ok' : 'warn'}">${t.available ? '已检测到' : '未检测到'}</span></label><input class="input mono" id="localPath-${esc(t.id)}" value="${esc(local.toolPaths?.[t.id] || '')}" placeholder="留空自动检测"><span class="help path-help">${esc(t.path || '安装工具后刷新，或填写可执行文件的绝对路径')}</span></div>`).join('')}</div><p class="muted small">不需要全部安装。程序路径不能带启动参数；手机远程执行与精确会话恢复需要对应 CLI。</p><button class="btn" data-act="localPaths">保存程序路径</button></details></div>`;
}

function localBackupPanel(st) {
  return `<div class="card"><details class="adv"><summary><span class="chev">${icon('chev')}</span>原配置备份与恢复</summary><p class="muted">更换 API 只修改必要的凭据和模型配置，正常退出并重开原桌面应用；会话、数据库、插件和项目文件不删除、不迁移。云端专属内容仍取决于原账户权限。</p><div class="row"><button class="btn sm" data-act="localRestore" data-kind="codex">恢复 Codex 原凭据</button><button class="btn sm" data-act="localRestore" data-kind="claude">恢复 Claude Code 原配置</button><button class="btn sm" data-act="localRestore" data-kind="claude-desktop">恢复 Claude Desktop 原配置</button></div><p class="muted small">API Key 以明文保存在当前用户的 Bridge 配置、工具配置和必要备份中，不是系统钥匙串。不要分享这些目录。</p><code class="path-help">${esc(st.configDir)}</code><p><a href="#tools" class="small">高级：旧版远程专用工具配置</a></p></details></div>`;
}

function localTarget(a) {
  const tools = (S.local?.tools || []).filter((t) => t.kind === a.kind);
  if (a.target && tools.some((t) => t.id === a.target && t.available)) return a.target;
  return (tools.find((t) => t.available) || tools[0] || {}).id || a.kind;
}

function localAccountCard(a) {
  const usedBy = (S.local.tools || []).filter((t) => S.local.bindings?.[t.id]?.accountId === a.id).map((t) => t.name);
  const wire = a.wire && a.wire !== 'auto' ? ({ responses: 'Responses', chat: 'Chat', anthropic: 'Messages' })[a.wire] : '';
  const more = typeof wbMore === 'function' ? wbMore : (body) => `<details class="wb-card-more"><summary>⋯</summary><div class="wb-more-body">${body}</div></details>`;
  return `<article class="wb-card account-card">
    <span class="tool-logo api">${icon('key')}</span>
    <div class="wb-main"><div class="wb-name" title="${esc(a.name)}">${esc(a.name)}${wire ? `<span class="chip">${wire}</span>` : ''}</div>
      <div class="wb-sub"><span title="${esc(a.baseUrl)}">${esc(wbHost ? wbHost(a.baseUrl) : a.baseUrl)}</span><code>${esc(a.keyMasked)}</code>${(a.models || []).length ? `<span>${a.models.length} 个模型</span>` : ''}</div></div>
    <div class="acct-used">${usedBy.length ? esc(usedBy.join('、')) : '<span class="faint">未使用</span>'}</div>
    <button class="btn" data-act="localEdit" data-id="${esc(a.id)}">编辑</button>
    ${more(`<button class="btn sm ghost" data-act="localModels" data-id="${esc(a.id)}">${icon('refresh')}读取模型</button><button class="btn sm ghost danger-text" data-act="localDelete" data-id="${esc(a.id)}">${icon('trash')}移除</button>`)}
  </article>`;
}

function renderLocalCards() {
  const box = $('#localAccountList');
  if (!box || !S.local) return;
  const q = S.localFilter.toLowerCase().trim();
  const accounts = S.local.accounts.filter((a) => !q || [a.name, a.baseUrl].some((v) => String(v || '').toLowerCase().includes(q)));
  box.innerHTML = accounts.length ? accounts.map(localAccountCard).join('') + `<button class="acct-add" data-act="localNew">${icon('plus')}添加 API 密钥</button>` : `<div class="acct-empty"><b>${S.local.accounts.length ? '没有匹配的密钥' : '还没有 API 密钥'}</b><span>保存一次服务商地址和 Key，所有 Agent 都能选用。</span><button class="btn primary" data-act="localNew">${icon('plus')}添加 API 密钥</button></div>`;
  const count = $('#localCount');
  if (count) count.textContent = `${accounts.length} / ${S.local.accounts.length} 个密钥`;
}

function renderAccountsPage(view, local) {
  $('#headActions').innerHTML = `<button class="btn primary" data-act="localNew">${icon('plus')}添加</button>`;
  view.innerHTML = `${local.accounts.length > 5 ? `<div class="local-toolbar"><input class="input" id="localSearch" type="search" aria-label="搜索 API" placeholder="搜索名称或地址" value="${esc(S.localFilter)}"><span class="muted small" id="localCount"></span></div>` : ''}
    <div class="wb-grid" id="localAccountList"></div>
    <p class="su-foot">${icon('lock')} Key 只保存在这台电脑上，不会发给中转站或手机。</p>`;
  renderLocalCards();
}

RENDER_accounts = function (view) {
  const target = view;
  const cached = S.local;
  if (cached) renderAccountsPage(target, cached);
  void loadState().catch(() => undefined);
  void api('/api/local/accounts').then((local) => {
    if (!local || target.isConnected === false || S.route !== 'accounts') return;
    S.local = local;
    renderAccountsPage(target, local);
  }).catch((e) => {
    if (cached || target.isConnected === false || S.route !== 'accounts') return;
    target.innerHTML = `<div class="callout bad">${ico('warn')}<div>${esc(e.message)}</div></div>`;
  });
};

function localEditor(a, kind) {
  S.localDraftModels = [...(a?.models || [])];
  $('#modalRoot').innerHTML = `<div class="modal-back" data-act="closeModal"><div class="modal account-modal" data-stop role="dialog" aria-modal="true" aria-labelledby="localEditorTitle"><div class="modal-head"><h3 id="localEditorTitle">${a ? '编辑' : '添加'} API 密钥</h3><button class="icon-btn" data-act="closeModal" aria-label="关闭">✕</button></div>
    <div class="modal-body"><input type="hidden" id="laID" value="${esc(a?.id || '')}"><input type="hidden" id="laModel" value="${esc(a?.model || '')}"><input type="hidden" id="laWorkspace" value="${esc(a?.workspace || '')}"><div class="form-grid"><div class="field full"><label for="laName">密钥名称</label><input class="input" id="laName" maxlength="80" placeholder="例如：Salcara 主力 / 备用 Key" value="${esc(a?.name || '')}"><span class="help">名称由你决定，同一个密钥可以给多个工具使用。</span></div>
    <div class="field full"><label for="laBase">服务商 API 地址</label><input class="input" id="laBase" placeholder="https://api.example.com 或 .../v1" value="${esc(a?.baseUrl || 'https://salcara.top')}"><span class="help">可填写任何兼容服务商；可带 /v1，支持自定义路径前缀。</span></div>
    <div class="field full"><label for="laKey">API Key</label><div class="input-wrap"><input class="input" id="laKey" type="password" autocomplete="off" spellcheck="false" placeholder="${esc(a ? '已保存 ' + a.keyMasked + '；留空保持原 Key' : '粘贴你的 API Key')}"><button class="icon-btn" data-act="toggleVis" data-for="laKey" aria-label="显示或隐藏 Key">${icon('eye')}</button></div></div>
    <div class="field full"><label for="laAuth">服务商认证方式</label><select class="input" id="laAuth"><option value="bearer" ${a?.authMode !== 'api-key' ? 'selected' : ''}>Bearer（大多数中转站）</option><option value="api-key" ${a?.authMode === 'api-key' ? 'selected' : ''}>x-api-key（Anthropic 官方等）</option></select><span class="help">这是 Key 的发送方式，不是工具归属；不确定时保留 Bearer。</span></div>
    <div class="field full"><label for="laWire">接口类型</label><select class="input" id="laWire"><option value="auto" ${!a?.wire || a?.wire === 'auto' ? 'selected' : ''}>按模型自动（Claude → Messages，GPT → Responses，其他 → Chat）</option><option value="responses" ${a?.wire === 'responses' ? 'selected' : ''}>OpenAI Responses</option><option value="chat" ${a?.wire === 'chat' ? 'selected' : ''}>OpenAI 兼容（Chat Completions）</option><option value="anthropic" ${a?.wire === 'anthropic' ? 'selected' : ''}>Claude（Anthropic Messages）</option></select><span class="help">中转站一般选「按模型自动」；只支持一种接口的服务商（例如 DeepSeek 官方）选对应的那一种。</span></div>
    <div class="field full"><div class="row"><button class="btn" data-act="localModels">读取模型目录（可选）</button><span class="muted small">已读取 ${S.localDraftModels.length} 个</span></div><span class="help">这里只保存目录；模型在原工具内选择。读取不会发起生成请求。</span></div></div><div id="localProbeResult" aria-live="polite"></div></div>
    <div class="modal-foot"><span class="muted small" style="margin-right:auto">只保存在本机，不要求远程登录</span><button class="btn" data-act="closeModal">取消</button><button class="btn primary" data-act="localSave">保存密钥</button></div></div></div>`;
  $('#laName').focus();
}

function localForm() {
  return { id: $('#laID').value, name: $('#laName').value, kind: 'api', baseUrl: $('#laBase').value, key: $('#laKey').value, model: $('#laModel').value, authMode: $('#laAuth').value, wire: $('#laWire') ? $('#laWire').value : 'auto', workspace: $('#laWorkspace').value, models: S.localDraftModels };
}

function toolForm(target) {
  return { target, id: document.getElementById('toolAPI-' + target)?.value || '', model: S.local?.bindings?.[target]?.model || '', protocol: document.getElementById('toolProtocol-' + target)?.value || '' };
}

function toolDraftStatus(target) {
  const card = document.querySelector(`[data-tool="${target}"]`);
  if (!card) return;
  const form = toolForm(target), binding = S.local?.bindings?.[target] || {};
  const button = card.querySelector('[data-act=toolRestart]');
  if (button) button.disabled = target === 'claude-desktop' || !(form.id && form.model && card.dataset.available === 'true');
  const dirty = form.id !== (binding.accountId || '') || form.model !== (binding.model || '') || form.protocol !== (binding.protocol || (target.startsWith('codex') ? 'responses' : 'anthropic'));
  const desktop = target.endsWith('-desktop');
  const tool = S.local?.tools?.find((t) => t.id === target) || { id: target, available: card.dataset.available === 'true' };
  document.getElementById('lamp-' + target).innerHTML = remoteLampHTML(desktop ? desktopRemoteLamp(tool) : dirty ? { state: 'pending', label: '设置待应用', detail: '卡片设置尚未应用；CLI 远程仍使用上一次已应用配置' } : binding.remote, desktop ? 'desktop' : 'cli');
  const applied = document.getElementById('agentApplied-' + target);
  if (applied) {
    const account = S.local?.accounts?.find((a) => a.id === binding.appliedId);
    applied.innerHTML = target === 'claude-desktop' ? '<span class="chip warn">自动应用暂不可用</span>' : dirty || binding.pending
      ? `<span class="chip warn">${form.id ? dirty ? '设置待应用' : '选择已保存，等待应用' : '未选择 API'}</span>${account ? `<span class="muted small">当前应用：${esc(account.name)}</span>` : ''}`
      : `<span class="chip ok">已应用 ${esc(account?.name || '')}</span>`;
  }
}

async function openRestore(target) {
  if (target === 'claude-desktop') target = 'claude';
  S.restoreTarget = target;
  S.restoreSessions = [];
  S.restoreSearch = '';
  const kind = target.split('-')[0];
  const binding = S.local?.bindings?.[target];
  $('#modalRoot').innerHTML = `<div class="modal-back" data-act="closeModal"><div class="modal restore-modal" data-stop role="dialog" aria-modal="true" aria-labelledby="restoreTitle"><div class="modal-head"><h3 id="restoreTitle">恢复 ${esc(TOOL_LABEL[target])} 会话</h3><button class="icon-btn" data-act="closeModal" aria-label="关闭">✕</button></div><div class="modal-body"><p class="muted small">读取原工具本地记录。精确恢复通过 ${kind === 'codex' ? 'Codex CLI' : 'Claude Code'} 终端打开，使用原会话编号和原项目目录，不新建会话。${kind === 'claude' ? '使用 Claude Code 卡片已应用的 API，不包含 Claude Desktop 普通云端聊天。' : ''}</p>${!binding?.appliedId || binding.pending ? '<div class="callout warn">请先在对应 CLI 工具卡片应用并重启当前 API，才可以在终端恢复。仍可先查看已有记录。</div>' : ''}<input class="input" id="restoreSearch" type="search" placeholder="搜索标题、项目目录或模型" aria-label="搜索恢复会话"><div id="restoreList" class="restore-list"><div class="empty"><span class="spin"></span>正在读取原会话…</div></div><div id="restoreError" aria-live="polite"></div></div><div class="modal-foot"><span class="muted small" style="margin-right:auto">正在执行的会话不能重复打开</span><button class="btn" data-act="closeModal">关闭</button></div></div></div>`;
  try {
    const r = await api('/api/sessions?tool=' + kind);
    if (S.restoreTarget !== target || !$('#restoreList')) return;
    S.restoreSessions = r.sessions || [];
    renderRestoreList();
  } catch (e) {
    const list = $('#restoreList');
    if (list) list.innerHTML = `<div class="callout bad">${esc(e.message)}</div>`;
  }
}

function matchesSession(s, query) {
  const q = query.toLowerCase().trim();
  return !q || [s.title, s.cwd, s.model, s.sessionKey].some((v) => String(v || '').toLowerCase().includes(q));
}

function renderRestoreList() {
  const list = $('#restoreList');
  if (!list) return;
  const rows = S.restoreSessions.filter((s) => matchesSession(s, S.restoreSearch));
  const binding = S.local?.bindings?.[S.restoreTarget];
  list.innerHTML = rows.map((s) => {
    const running = s.status === 'running' || s.status === 'waiting_approval';
    const canResume = binding?.appliedId && !binding.pending && !running && s.cwd;
    return `<div class="restore-row"><div class="li-main"><div class="li-title">${esc(s.title || '无标题会话')}</div><div class="sess-meta">${toolChip(s.tool, s.client)}<span class="status-dot s-${esc(s.status)}"></span>${esc(SESSION_STATUS[s.status] || s.status)} · ${esc(relTime(s.updatedAt))}${s.model ? ' · ' + esc(s.model) : ''}</div><div class="sess-cwd" title="${esc(s.cwd)}">${esc(s.cwd || '原项目目录未知')}</div></div><div class="restore-actions"><button class="btn sm" data-act="restoreView" data-k="${esc(s.sessionKey)}" data-kind="${esc(s.tool)}">查看记录</button><button class="btn sm primary" data-act="resumeSession" data-k="${esc(s.sessionKey)}" data-target="${esc(S.restoreTarget)}" ${canResume ? '' : 'disabled'} title="${running ? '先结束原任务' : canResume ? '恢复同一个会话，不新建' : '先应用当前 API，原会话须有项目目录'}">终端恢复</button></div></div>`;
  }).join('') || `<div class="empty">${S.restoreSessions.length ? '没有匹配的会话' : '没有找到原工具本地记录。这里不会自动新建会话。'}</div>`;
}

/* ---------------- 登录 / 中转站 ---------------- */

RENDER_login = async function (view) {
  const st = await loadState();
  const c = st.config;
  const keyPh = c.accountKey ? `已保存 ${c.accountKey}，留空表示不修改` : 'sk-…';
  view.innerHTML = `
  <div class="card">
    <div class="card-head"><div><h3 class="card-title">${ico('key')}中转站账号</h3>
      <p class="card-sub">Bridge 用这个 Key 登录远程编程服务；只有中转站的有效用户才能使用。</p></div>
      ${c.loggedIn ? `<span class="chip ok">${icon('check')}已登录</span>` : '<span class="chip">未登录</span>'}</div>
    <div class="form-grid">
      <div class="field"><label for="fRoot">中转站地址</label>
        <input class="input" id="fRoot" placeholder="https://api.example.com" value="${esc(c.relayRoot)}">
        <span class="help">填网站根地址即可，不用带 /v1</span></div>
      <div class="field"><label for="fKey">API Key</label>
        <div class="input-wrap"><input class="input" id="fKey" type="password" autocomplete="off" placeholder="${esc(keyPh)}">
        <button class="icon-btn" data-act="toggleVis" data-for="fKey" title="显示/隐藏">${icon('eye')}</button></div>
        <span class="help">在中转站网页「API 密钥」里创建；手机端用同一个 Key</span></div>
    </div>
    <div class="divider"></div>
    <details class="adv"${c.codexKey || c.claudeKey || c.hubUrl ? ' open' : ''}><summary><span class="chev">${icon('chev')}</span>高级：给 Codex / Claude Code 分别指定 Key</summary>
      <div class="callout" style="margin:14px 0">${ico('info')}<div>sub2api 的分组是按平台划分的：<b>Codex 需要 OpenAI 分组的 Key</b>，<b>Claude Code 需要 Anthropic（Claude）分组的 Key</b>。如果你的账号 Key 只属于一个分组，另一个工具就在这里填对应分组的 Key；留空表示使用上面的账号 Key。</div></div>
      <div class="form-grid">
        <div class="field"><label for="fCodex">Codex Key（OpenAI 分组）</label>
          <div class="input-wrap"><input class="input" id="fCodex" type="password" autocomplete="off" placeholder="${esc(c.codexKey ? '已保存 ' + c.codexKey + '，留空不修改' : '留空 = 使用账号 Key')}">
          <button class="icon-btn" data-act="toggleVis" data-for="fCodex">${icon('eye')}</button></div>
          ${c.codexKey ? '<label class="row small muted" style="font-weight:500"><input type="checkbox" id="fCodexClear"> 清除，改用账号 Key</label>' : ''}</div>
        <div class="field"><label for="fClaude">Claude Key（Anthropic 分组）</label>
          <div class="input-wrap"><input class="input" id="fClaude" type="password" autocomplete="off" placeholder="${esc(c.claudeKey ? '已保存 ' + c.claudeKey + '，留空不修改' : '留空 = 使用账号 Key')}">
          <button class="icon-btn" data-act="toggleVis" data-for="fClaude">${icon('eye')}</button></div>
          ${c.claudeKey ? '<label class="row small muted" style="font-weight:500"><input type="checkbox" id="fClaudeClear"> 清除，改用账号 Key</label>' : ''}</div>
        <div class="field"><label for="fHub">远程编程服务地址（可选）</label>
          <input class="input" id="fHub" placeholder="${esc(c.relayRoot ? c.relayRoot + '/salcara-hub' : '默认 = 中转站地址/salcara-hub')}" value="${esc(c.hubUrl)}">
          <span class="help">一般不用填，默认是 中转站地址/salcara-hub</span></div>
      </div>
    </details>
    <div class="form-actions">
      <button class="btn grad" data-act="login">${icon('check')}保存并登录</button>
      <button class="btn" data-act="testKey">测试连接</button>
      <span class="spacer"></span>
      ${c.loggedIn ? `<button class="btn ghost" data-act="logout">退出登录</button>` : ''}
    </div>
    <div id="testResult" style="margin-top:14px"></div>
  </div>
  <div class="card"><h3 class="card-title">${ico('info')}这些信息怎么用</h3>
    <ul class="muted" style="margin:10px 0 0;padding-left:20px;line-height:1.9">
      <li>账号 Key 只用于登录远程编程服务（服务端只保存它的哈希），和手机端互相识别。</li>
      <li>在手机上发起的任务，Bridge 会把中转站地址和对应的 Key 注入 Codex / Claude Code 进程，费用照常记在你的中转站账户。</li>
      <li>Key 保存在本机配置文件 <code>${esc(c.configPath)}</code>（权限 0600，仅当前用户可读）。</li>
    </ul></div>`;
};

function testResultHTML(u) {
  const parts = [];
  if (u.planName) parts.push(esc(u.planName));
  if (num(u.remaining) !== null) parts.push('剩余 ' + money(u.remaining, u.unit));
  if (u.usage && u.usage.today) parts.push('今日 ' + compact(u.usage.today.requests) + ' 次请求');
  return `<div class="callout ok">${ico('check')}<div>Key 有效${parts.length ? '：' + parts.join(' · ') : ''}</div></div>`;
}

// Discover a compatible remote plugin before enrolling a device. Legacy API-key
// login stays on the backend for compatibility, separate from this default flow.
RENDER_login = async function (view) {
  const st = await loadState(), c = st.config, connected = st.hub?.state === 'connected';
  const plugin = S.remoteDiscovery?.plugin;
  view.innerHTML = `<div class="callout">${ico('phone')}<div>输入中转站地址，先检测远程插件，再连接电脑，最后在手机上扫码绑定。只验证设备所有权，不要求中转站账号或模型 API Key。<br><span class="small muted">换站点要重新建立连接；不同站点使用独立设备凭证。模型调用仍使用工具卡片里的 API。</span></div></div>
    <div class="card"><div class="card-head"><h3 class="card-title">1 · 检测中转站插件</h3></div><div class="field"><label for="remoteRoot">中转站地址</label><input class="input" id="remoteRoot" placeholder="https://你的中转站域名" value="${esc(c.relayRoot || '')}"><span class="help">只填写地址，不填写 API Key；公网连接必须使用 HTTPS。</span></div>
    <details class="adv"><summary>插件路径不是默认路径？</summary><div class="field"><label for="remoteHub">插件地址（可选，与中转站同源）</label><input class="input" id="remoteHub" placeholder="默认：中转站地址/salcara-hub" value="${esc(c.hubUrl || '')}"></div></details>
    ${(c.remoteConnections || []).length ? `<div class="field"><label for="remoteSaved">曾连接的站点</label><select class="input" id="remoteSaved"><option value="">选择已保存的站点…</option>${c.remoteConnections.map((x) => `<option value="${esc(x.hubUrl)}">${esc(x.name)}</option>`).join('')}</select><span class="help">只选择地址，仍需检测并连接；不会复用其他站点的凭证。</span></div>` : ''}
    <div class="row"><button class="btn" data-act="remoteProbe">${icon('search')}检测插件</button><button class="btn primary" id="remoteConnect" data-act="remoteConnect" ${plugin ? '' : 'disabled'}>2 · 连接这台电脑</button></div><div id="remoteProbeResult" aria-live="polite">${plugin ? `<div class="callout ok">${ico('check')}<div>插件 v${esc(plugin.version)} · 协议 v${esc(plugin.protocolVersion)} · 支持设备扫码<br>${esc(plugin.hubUrl)}</div></div>` : ''}</div></div>
    <div class="card"><div class="row"><span class="folder-ico">${icon('plug')}</span><div class="li-main"><div class="li-title" id="remoteStatusLabel">${esc(st.hub?.state === 'invalid_key' && c.remoteDeviceOnly ? '设备验证失败' : STATUS[st.hub?.state] || '本地模式')}</div><div class="li-sub">${c.remoteDeviceOnly ? esc(c.effectiveHubUrl) : c.loggedIn ? '正在使用旧版账号连接；重新检测并连接可改为设备扫码' : '连接成功后才可生成二维码'}</div></div><button class="btn sm" data-act="refresh">刷新状态</button>${c.loggedIn ? '<button class="btn sm" data-act="logout">断开连接</button>' : ''}</div></div>
    <div class="card pair-card"><h3 class="card-title">3 · 手机扫码绑定</h3><p class="muted small">手机也先输入本站地址并检测插件，再扫描电脑二维码。二维码 5 分钟有效，仅使用一次；不要把二维码发给别人。成功重新绑定后旧手机失效。</p><div class="row"><button class="btn primary" data-act="pairStart" ${connected ? '' : 'disabled'}>${icon('phone')}生成绑定二维码</button><button class="btn danger" data-act="pairRevoke" ${connected ? '' : 'disabled'}>解除手机绑定</button></div><div id="pairQR" class="pair-qr">${S.pairQR && S.pairExpires > Date.now() ? `<img src="${esc(S.pairQR)}" alt="一次性手机绑定二维码"><p class="muted small">有效期至 ${new Date(S.pairExpires).toLocaleTimeString()}</p>` : ''}</div></div>
    <div class="callout warn">${ico('warn')}<div>本站远程插件会中继会话和控制指令，只连接你信任的中转站。扫码授权仅针对这台电脑，不允许访问本站其他设备。修改 API Key 不会取消手机绑定。</div></div>`;
};

/* ---------------- 工具配置 ---------------- */

function renderToolsPage(view, st, rawTc) {
  const tc = Object.assign({ os: st.os || 'windows', codexAuthMode: 'token', envKeyName: 'OPENAI_API_KEY', claude: {}, codex: {}, desktop: {} }, rawTc || {});
  tc.claude = Object.assign({}, tc.claude || {});
  tc.codex = Object.assign({}, tc.codex || {});
  tc.desktop = Object.assign({}, tc.desktop || {});
  const c = st.config;
  const loggedIn = c.loggedIn && !c.remoteDeviceOnly;
  const cl = tc.claude, cx = tc.codex;
  const toolOf = (id) => (st.tools || []).find((t) => t.id === id) || { available: false };
  const stateChip = (x) => x.configured ? `<span class="chip ok">${icon('check')}已接入中转站</span>` : (x.error ? '<span class="chip bad">读取失败</span>' : '<span class="chip warn">未接入</span>');
  const needLogin = `<div class="callout">${ico('info')}<div>本地多 Key 管理与保留会话的原地切号请使用 <a href="#accounts">API 账号 / 切换</a>。下面保留旧版远程中转站的一键配置${loggedIn ? '。' : '，需要先登录远程服务。'}</div></div>`;
  const isWin = tc.os === 'windows';
  const mode = tc.codexAuthMode || 'token';
  view.innerHTML = `${needLogin}
  <div class="grid grid-2">
    <div class="card tool-card">
      <div class="tool-head"><span class="tool-logo claude">CC</span><div><div class="tool-name">Claude Code</div>
        <div class="muted small">${toolOf('claude').available ? '已安装 ' + esc(toolOf('claude').version || '') : '未检测到 claude 命令'}</div></div>
        <span class="spacer"></span>${stateChip(cl)}</div>
      <dl class="kv"><dt>配置文件</dt><dd class="mono">${esc(cl.path)} <button class="btn sm" data-act="reveal" data-w="claude">${icon('folder')}${openWord()}</button></dd>
        <dt>当前地址</dt><dd>${cl.baseUrl ? esc(cl.baseUrl) : '<span class="muted">官方（未设置 ANTHROPIC_BASE_URL）</span>'}</dd>
        <dt>Key</dt><dd>${cl.tokenMatches ? '<span class="chip ok">与 Bridge 一致</span>' : '<span class="muted">未设置或不同</span>'}</dd>
        <dt>备份</dt><dd>${cl.hasBackup ? '已备份原配置 <code>settings.json.salcara-bak</code>' : '<span class="muted">无</span>'}</dd></dl>
      ${cl.apiKeySet ? `<div class="callout warn">${ico('warn')}<div>settings.json 里还设置了 <code>ANTHROPIC_API_KEY</code>，它的优先级更高，可能导致不走中转站。建议删掉。</div></div>` : ''}
      ${cl.error ? `<div class="callout bad">${ico('warn')}<div>${esc(cl.error)}</div></div>` : ''}
      <div class="muted small">写入 <code>env</code>：<code>ANTHROPIC_BASE_URL</code> = 中转站地址（不带 /v1）、<code>ANTHROPIC_AUTH_TOKEN</code> = Claude Key、<code>CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC</code> = 1。其他设置保持不变，第一次修改前自动备份。</div>
      <div class="row"><button class="btn primary" data-act="toolcfg" data-tool="claude" data-a="apply" ${loggedIn ? '' : 'disabled'}>一键配置</button>
        <button class="btn" data-act="toolcfg" data-tool="claude" data-a="restore">恢复原配置</button></div>
    </div>

    <div class="card tool-card">
      <div class="tool-head"><span class="tool-logo codex">Cx</span><div><div class="tool-name">Codex</div>
        <div class="muted small">${toolOf('codex').available ? '已安装 ' + esc(toolOf('codex').version || '') : '未检测到 codex 命令'}</div></div>
        <span class="spacer"></span>${stateChip(cx)}</div>
      <dl class="kv"><dt>配置文件</dt><dd class="mono">${esc(cx.path)} <button class="btn sm" data-act="reveal" data-w="codex">${icon('folder')}${openWord()}</button></dd>
        <dt>当前提供方</dt><dd>${cx.provider ? esc(cx.provider) : '<span class="muted">openai（默认）</span>'}</dd>
        <dt>地址</dt><dd>${cx.baseUrl ? esc(cx.baseUrl) : '<span class="muted">—</span>'}</dd>
        <dt>认证方式</dt><dd>${cx.authMode === 'env' ? '环境变量 ' + esc(tc.envKeyName) : cx.authMode === 'token' ? 'Key 写在配置文件里' : '<span class="muted">—</span>'}</dd>
        <dt>备份</dt><dd>${cx.hasBackup ? '已备份原配置 <code>config.toml.salcara-bak</code>' : '<span class="muted">无</span>'}</dd></dl>
      <div class="field"><label>Key 保存方式</label>
        <div class="radio-cards" style="grid-template-columns:1fr 1fr">
          <label class="radio-card ${mode !== 'env' ? 'sel' : ''}"><input type="radio" name="cxmode" value="token" ${mode !== 'env' ? 'checked' : ''}><span class="t">写入配置文件</span><span class="d">experimental_bearer_token，最省事（Key 以明文存在 config.toml）</span></label>
          <label class="radio-card ${mode === 'env' ? 'sel' : ''}"><input type="radio" name="cxmode" value="env" ${mode === 'env' ? 'checked' : ''}><span class="t">环境变量</span><span class="d">env_key = ${esc(tc.envKeyName)}${isWin ? '，自动用 setx 设置' : tc.os === 'darwin' ? '，需要自己在 ~/.zshrc 里 export（从程序坞打开的 App 读不到，Mac 上推荐左边的方式）' : '，需要你自己在 shell 里 export'}</span></label>
        </div></div>
      <div class="muted small">设置 <code>model_provider = "salcara"</code>，并写入 <code>[model_providers.salcara]</code>：base_url = 中转站地址/v1、wire_api = "responses"、requires_openai_auth = false、supports_websockets = false。其他配置和注释保持不变，第一次修改前自动备份。</div>
      <div class="row"><button class="btn primary" data-act="toolcfg" data-tool="codex" data-a="apply" ${loggedIn ? '' : 'disabled'}>一键配置</button>
        <button class="btn" data-act="toolcfg" data-tool="codex" data-a="restore">恢复原配置</button></div>
    </div>
  </div>

  <div class="card tool-card">
    <div class="tool-head"><span class="tool-logo desktop">CD</span><div><div class="tool-name">Claude Desktop</div>
      <div class="muted small">${tc.desktop && tc.desktop.available ? '已安装 ' + esc(tc.desktop.version || '') : '没有检测到 Claude 桌面版'}</div></div>
      <span class="spacer"></span><span class="chip info">在应用内设置</span></div>
    <p class="muted" style="margin:0">Claude 桌面版的第三方模型接入在应用里设置，按下面几步操作即可（只需一次）：</p>
    <ol class="steps">
      <li><div>打开 Claude 桌面版，${tc.os === 'darwin' ? '屏幕顶部菜单栏' : '菜单'} <b>Help → Troubleshooting → Enable Developer Mode</b>。</div></li>
      <li><div>菜单 <b>Developer → Configure Third-Party Inference</b>。</div></li>
      <li><div><b>Connection type</b> 选 <b>Gateway</b>，<b>Credential</b> 选 <b>Static API key</b>，<b>Auth scheme</b> 选 <b>Bearer</b>。</div></li>
      <li><div style="flex:1;min-width:0"><b>Gateway base URL</b> 填：
        <div class="copy-field"><code>${esc(c.relayRoot || '（请先登录中转站）')}</code><button class="btn sm" data-act="copyVal" data-v="${esc(c.relayRoot)}" data-w="地址" ${c.relayRoot ? '' : 'disabled'}>${icon('copy')}复制</button></div></div></li>
      <li><div style="flex:1;min-width:0"><b>API key</b> 填你的 Claude Key：
        <div class="copy-field"><code>${esc(c.claudeKey || c.accountKey || '（请先登录中转站）')}</code><button class="btn sm" data-act="copySecret" data-which="claude" ${loggedIn ? '' : 'disabled'}>${icon('copy')}复制</button></div></div></li>
      <li><div>保存后重启 Claude 桌面版${tc.os === 'darwin' ? '（⌘Q 完全退出再打开）' : ''}。${tc.desktopConfigDir ? `它的设置保存在 <code>${esc(tc.desktopConfigDir)}</code>。` : ''}</div></li>
    </ol>
    <div class="callout">${ico('info')}<div>Claude 桌面版里 <b>Code</b> 标签页的会话本质上就是 Claude Code 会话，会出现在「会话」页面里（标记为 Claude Desktop），手机上也能看到。</div></div>
  </div>`;
}

RENDER_tools = function (view) {
  const target = view;
  const cachedState = S.state || { os: 'windows', config: {}, tools: [] };
  const cachedConfig = S.toolcfg;
  if (cachedConfig || S.state) renderToolsPage(target, cachedState, cachedConfig);
  void Promise.all([loadState(), api('/api/toolcfg')]).then(([st, tc]) => {
    if (target.isConnected === false || S.route !== 'tools') return;
    S.toolcfg = tc;
    renderToolsPage(target, st, tc);
  }).catch((e) => {
    if (!cachedConfig && !S.state && target.isConnected !== false && S.route === 'tools') target.innerHTML = `<div class="callout bad">${ico('warn')}<div>${esc(e.message)}</div></div>`;
  });
};

/* ---------------- 项目文件夹 ---------------- */

function renderProjectsPage(view, st) {
  const cfg = st.config || {};
  const ps = cfg.projects || [];
  view.innerHTML = `
  <div class="callout">${ico('info')}<div>为了安全，手机只能在下面这些文件夹（以及它们的子文件夹）里新建任务。会话里的命令也在这些文件夹里执行。</div></div>
  <div class="card">
    <div class="card-head"><h3 class="card-title">${ico('plus')}添加文件夹</h3></div>
    <div class="row" style="flex-wrap:nowrap">
      <input class="input" id="projPath" placeholder="${st.os === 'windows' ? 'C:\\code\\my-app' : esc((st.home || (st.os === 'darwin' ? '/Users/me' : '/home/me')) + '/code/my-app')}">
      <button class="btn" data-act="browse">${icon('folder')}浏览…</button>
      <button class="btn primary" data-act="addProj">添加</button>
    </div>
  </div>
  <div class="card">
    <div class="card-head"><h3 class="card-title">${ico('folder')}允许的文件夹 <span class="chip">${ps.length}</span></h3></div>
    ${ps.length ? `<div class="list">${ps.map((p) => `<div class="list-item"><div class="folder-ico">${icon('folder')}</div>
      <div class="li-main"><div class="li-title">${esc(p.name)}</div><div class="li-sub mono">${esc(p.path)}</div></div>
      <button class="btn sm danger" data-act="rmProj" data-path="${esc(p.path)}">${icon('trash')}移除</button></div>`).join('')}</div>`
      : `<div class="empty"><div class="big">还没有允许任何文件夹</div>添加你平时写代码的项目文件夹，手机上就能选它来发任务</div>`}
  </div>`;
}

RENDER_projects = function (view) {
  const target = view;
  const cached = S.state || { os: 'windows', home: '', config: { projects: [] } };
  if (S.state) renderProjectsPage(target, cached);
  void loadState().then((st) => {
    if (target.isConnected === false || S.route !== 'projects') return;
    renderProjectsPage(target, st);
  }).catch((e) => {
    if (!S.state && target.isConnected !== false && S.route === 'projects') target.innerHTML = `<div class="callout bad">${ico('warn')}<div>${esc(e.message)}</div></div>`;
  });
};

async function openBrowser(start) {
  const root = $('#modalRoot');
  let cur = null;
  async function load(p) {
    try {
      cur = await api('/api/fs?path=' + encodeURIComponent(p || ''));
    } catch (e) { toast(e.message, 'bad'); return; }
    root.innerHTML = `<div class="modal-back" data-act="closeModal"><div class="modal" data-stop>
      <div class="modal-head"><h3>选择项目文件夹</h3><button class="icon-btn" data-act="closeModal">✕</button></div>
      <div class="modal-body">
        <div class="roots">${cur.roots.map((r) => `<button class="btn sm" data-act="fsGo" data-p="${esc(r.path)}">${esc(r.name)}</button>`).join('')}</div>
        <div class="crumb">${esc(cur.path)}</div>
        <div style="margin:8px 0 14px">
          ${cur.parent ? `<div class="dir" data-act="fsGo" data-p="${esc(cur.parent)}">${ico('up')}<span>上一级</span></div>` : ''}
          ${cur.dirs.length ? cur.dirs.map((d) => `<div class="dir" data-act="fsGo" data-p="${esc(d.path)}">${ico('folder')}<span>${esc(d.name)}</span></div>`).join('') : '<div class="empty small">没有子文件夹</div>'}
        </div>
      </div>
      <div class="modal-foot"><span class="muted small" style="margin-right:auto">选中的是当前打开的文件夹</span>
        <button class="btn" data-act="closeModal">取消</button><button class="btn primary" data-act="fsPick">选择这个文件夹</button></div>
    </div></div>`;
  }
  S.fsLoad = load;
  S.fsCur = () => cur;
  await load(start);
}

/* ---------------- 会话 ---------------- */

RENDER_sessions = async function (view) {
  $('#headActions').innerHTML = `<button class="btn" data-act="reloadSessions">${icon('refresh')}刷新</button>`;
  view.innerHTML = `
  <div class="row"><div class="filters">
    ${[['', '全部'], ['codex', 'Codex'], ['claude', 'Claude Code / Desktop']].map(([k, l]) => `<button class="filter ${S.sessFilter === k ? 'on' : ''}" data-act="sessFilter" data-k="${k}">${l}</button>`).join('')}
  </div></div><input class="input" id="sessionSearch" type="search" placeholder="搜索会话标题、项目目录或模型" aria-label="搜索会话" value="${esc(S.sessSearch)}">
  <div id="approvalsBox"></div>
  <div class="sess-layout">
    <div class="card sess-list" id="sessList"><div class="skeleton"></div><div class="skeleton" style="margin-top:12px;width:70%"></div></div>
    <div class="card detail" id="sessDetail"><div class="empty" style="margin:auto"><div class="big">选择一个会话</div>查看实时进度、批准操作，或者继续对话</div></div>
  </div>`;
  renderApprovalsBox();
  await loadSessions();
  if (S.openKey) openSession(S.openKey, true);
};

async function loadSessions() {
  try {
    const r = await api('/api/sessions' + (S.sessFilter ? '?tool=' + S.sessFilter : ''));
    S.sessions = r.sessions || [];
  } catch (e) {
    const el = $('#sessList');
    if (el) el.innerHTML = `<div class="callout bad">${ico('warn')}<div>${esc(e.message)}</div></div>`;
    return;
  }
  renderSessionList();
}

function renderSessionList() {
  const el = $('#sessList');
  if (!el) return;
  if (!S.sessions.length) {
    el.innerHTML = '<div class="empty"><div class="big">还没有会话</div>在终端里用 codex / claude，或者在手机上发起任务后，会话会出现在这里</div>';
    return;
  }
  el.innerHTML = S.sessions.filter((s) => matchesSession(s, S.sessSearch)).map((s) => `<div class="sess ${s.sessionKey === S.openKey ? 'sel' : ''}" data-act="openSess" data-k="${esc(s.sessionKey)}">
    <div class="sess-title">${esc(s.title || '（无标题）')}</div>
    <div class="sess-meta">${toolChip(s.tool, s.client)}<span class="status-dot s-${esc(s.status)}"></span>${esc(SESSION_STATUS[s.status] || s.status)}<span class="spacer"></span>${esc(relTime(s.updatedAt))}</div>
    ${s.cwd ? `<div class="sess-cwd">${esc(s.cwd)}</div>` : ''}</div>`).join('') || '<div class="empty">没有匹配的会话</div>';
}

function tlKey(ev) {
  if (ev.type === 'message' || ev.type === 'reasoning' || ev.type === 'tool') return ev.type + ':' + (ev.id || Math.random());
  if (ev.type === 'approval.request' || ev.type === 'approval.resolved') return 'ap:' + ev.approvalId;
  return 'x:' + Math.random();
}

function tlApply(ev) {
  if (ev.type === 'session.updated') {
    if (ev.session) S.openInfo = ev.session;
    return;
  }
  const k = tlKey(ev);
  if (ev.type === 'approval.resolved') {
    const i = S.tlIndex.get(k);
    if (i !== undefined) { S.tl[i] = Object.assign({}, S.tl[i], { resolved: ev }); return; }
  }
  if (S.tlIndex.has(k)) {
    const i = S.tlIndex.get(k);
    S.tl[i] = Object.assign({}, S.tl[i], ev);
  } else {
    S.tlIndex.set(k, S.tl.length);
    S.tl.push(ev);
  }
}

const KIND_ICON = { command: 'terminal', file_change: 'edit', read: 'file', search: 'search', web: 'globe', mcp: 'box', other: 'box' };

function tlItem(ev) {
  switch (ev.type) {
    case 'message':
      return `<div class="msg ${ev.role === 'user' ? 'user' : 'assistant'}">${renderText(ev.text)}</div>`;
    case 'reasoning':
      return ev.text ? `<div class="reason">${esc(ev.text)}</div>` : '';
    case 'tool': {
      const chip = ev.status === 'running' ? '<span class="chip info"><span class="spin" style="width:10px;height:10px"></span>运行中</span>'
        : ev.status === 'failed' ? `<span class="chip bad">失败${ev.exitCode !== undefined && ev.exitCode !== null ? ' ' + esc(ev.exitCode) : ''}</span>` : '<span class="chip ok">完成</span>';
      const has = ev.detail || ev.output || ev.diff;
      return `<details class="tl-tool"><summary>${ico(KIND_ICON[ev.kind] || 'box')}<span class="tt">${esc(ev.title || ev.kind)}</span>${chip}</summary>
        ${has ? `<div class="body">${ev.detail ? `<pre>${esc(ev.detail)}</pre>` : ''}${ev.diff ? `<pre class="diff">${renderDiff(ev.diff)}</pre>` : ''}${ev.output ? `<pre>${esc(ev.output)}</pre>` : ''}</div>` : ''}</details>`;
    }
    case 'approval.request':
    case 'approval.resolved': {
      const r = ev.resolved || (ev.type === 'approval.resolved' ? ev : null);
      const dec = r ? ({ allow: '已允许', allow_session: '已允许（本会话）', deny: '已拒绝' }[r.decision] || r.decision) : '';
      const by = r ? ({ phone: '手机', desktop: '电脑', timeout: '超时' }[r.by] || r.by || '') : '';
      return `<div class="appr ${r ? 'done' : ''}"><div class="appr-title">${ico(r ? 'check' : 'warn')}${esc(ev.title || '需要批准')}</div>
        ${ev.detail ? `<pre>${esc(ev.detail)}</pre>` : ''}${ev.diff ? `<pre class="diff">${renderDiff(ev.diff)}</pre>` : ''}
        ${r ? `<div class="muted small">${esc(dec)}${by ? ' · 由' + esc(by) + '处理' : ''}</div>`
          : `<div class="row"><button class="btn sm primary" data-act="approve" data-id="${esc(ev.approvalId)}" data-d="allow">允许</button>
             <button class="btn sm" data-act="approve" data-id="${esc(ev.approvalId)}" data-d="allow_session">本会话都允许</button>
             <button class="btn sm danger" data-act="approve" data-id="${esc(ev.approvalId)}" data-d="deny">拒绝</button></div>`}</div>`;
    }
    case 'turn': {
      const label = { started: '开始执行', completed: '本轮完成', failed: '执行失败', interrupted: '已停止' }[ev.status] || ev.status;
      const usage = ev.usage ? ` · ${compact(ev.usage.inputTokens)} 输入 / ${compact(ev.usage.outputTokens)} 输出 tokens` : '';
      return `<div class="turn">${esc(label)}${esc(usage)}${ev.error ? ' · ' + esc(ev.error) : ''}</div>`;
    }
    case 'notice':
      return `<div class="notice ${esc(ev.level)}">${esc(ev.text)}</div>`;
  }
  return '';
}

function renderDetail(scroll) {
  const el = $('#sessDetail');
  if (!el) return;
  const s = S.openInfo || {};
  const running = s.status === 'running' || s.status === 'waiting_approval';
  const tlEl = $('#timeline');
  const atBottom = !tlEl || tlEl.scrollHeight - tlEl.scrollTop - tlEl.clientHeight < 80;
  el.innerHTML = `<div class="detail-head"><div class="row"><div class="li-main"><div class="detail-title">${esc(s.title || S.openKey)}</div>
      <div class="sess-meta" style="margin-top:6px">${toolChip(s.tool, s.client)}<span class="status-dot s-${esc(s.status)}"></span>${esc(SESSION_STATUS[s.status] || s.status || '')}${s.model ? ' · ' + esc(s.model) : ''}${s.cwd ? ` · <span class="mono">${esc(s.cwd)}</span>` : ''}</div></div>
      ${running ? `<button class="btn sm danger" data-act="interrupt">${icon('stop')}停止</button>` : s.cwd && s.sessionKey ? `<button class="btn sm" data-act="resumeSession" data-k="${esc(s.sessionKey)}" data-target="${esc(s.tool)}">${icon('terminal')}终端恢复</button>` : ''}</div></div>
    <div class="timeline" id="timeline">${S.tl.map(tlItem).join('') || '<div class="empty">还没有内容</div>'}</div>
    <div class="composer">${s.controllable === false ? '<div class="muted small" style="flex:1">这个会话目前不能从这里继续</div>'
      : `<textarea class="input" id="sendText" placeholder="继续对话…（${OSX() === 'darwin' ? '⌘' : 'Ctrl+'}Enter 发送）"></textarea><button class="btn primary" data-act="send">${icon('send')}发送</button>`}</div>`;
  const t = $('#timeline');
  if (t && (scroll || atBottom)) t.scrollTop = t.scrollHeight;
}

async function openSession(key, keepScroll) {
  S.openKey = key;
  renderSessionList();
  const el = $('#sessDetail');
  if (el && !keepScroll) el.innerHTML = '<div class="empty" style="margin:auto"><span class="spin"></span></div>';
  try {
    const r = await api('/api/session?key=' + encodeURIComponent(key));
    if (S.openKey !== key) return;
    S.openInfo = r.session;
    S.tl = [];
    S.tlIndex = new Map();
    (r.events || []).forEach(tlApply);
    S.approvals.filter((a) => a.sessionKey === key).forEach(tlApply);
    renderDetail(true);
  } catch (e) {
    if (el) el.innerHTML = `<div class="callout bad" style="margin:20px">${ico('warn')}<div>${esc(e.message)}</div></div>`;
  }
}

/* ---------------- 设置 ---------------- */

function renderSettingsPage(view, st) {
  const c = st.config || {};
  const pol = [
    ['ask', '每次都询问', '执行命令、修改文件前都要你在手机或电脑上批准（推荐）', ''],
    ['auto_edits', '自动批准改文件', '修改项目内的文件自动通过，运行命令仍然询问', ''],
    ['auto_all', '全部自动', '不再询问，AI 可以直接运行任何命令。只在你完全信任任务时使用', 'danger'],
  ];
  const pending = S.approvals.length;
  view.innerHTML = `
  <a class="card set-link" href="#sessions">
    <span class="set-link-ico">${icon('chat')}</span>
    <span class="li-main"><span class="li-title">对话</span><span class="li-sub">这台电脑上的 Codex 与 Claude Code 对话，可恢复继续</span></span>
    ${pending ? `<span class="chip bad">${pending} 个待批准</span>` : ''}<span class="set-link-chev">${icon('chev')}</span>
  </a>
  <div class="card">
    <h3 class="card-title" style="margin-bottom:14px">${ico('power')}启动</h3>
    <div class="setting-row"><div class="txt"><div class="t">开机自动启动</div><div class="d">登录电脑后在后台运行，手机随时能连上这台电脑</div></div>
      <label class="switch"><input type="checkbox" id="swAuto" ${c.autostart ? 'checked' : ''}><span></span></label></div>
    <div class="setting-row"><div class="txt"><div class="t">开机启动时打开控制台</div><div class="d">关闭后开机只在后台运行，需要时${launchHint()}即可打开这个页面</div></div>
      <label class="switch"><input type="checkbox" id="swOpen" ${c.openConsoleOnStart ? 'checked' : ''}><span></span></label></div>
  </div>
  <div class="card">
    <h3 class="card-title" style="margin-bottom:6px">${ico('check')}审批策略</h3>
    <p class="card-sub" style="margin-bottom:14px">从手机发起的新任务默认使用这个策略（手机上发任务时也可以单独选择）</p>
    <div class="radio-cards">${pol.map(([v, t, d, cls]) => `<label class="radio-card ${cls} ${c.approval === v ? 'sel' : ''}"><input type="radio" name="pol" value="${v}" ${c.approval === v ? 'checked' : ''}>
      <span class="t">${cls ? `<span style="color:var(--danger)">${icon('warn')}</span>` : ''}${t}</span><span class="d">${d}</span></label>`).join('')}</div>
    ${c.approval === 'auto_all' ? `<div class="callout bad" style="margin-top:14px">${ico('warn')}<div><b>风险提示：</b>“全部自动”下 AI 可以不经确认运行任意命令（包括删除文件、访问网络）。请只在允许的项目文件夹里、并且信任任务内容时使用。</div></div>` : ''}
    <div class="setting-row" style="margin-top:14px;border-top:1px solid var(--border)"><div class="txt"><div class="t">允许手机选择「全部自动」</div><div class="d">关闭时手机只能用「每步询问」或「自动改文件」，手机绑定泄露也无法不经确认运行命令</div></div>
      <label class="switch"><input type="checkbox" id="swPhoneAuto" ${c.allowPhoneAutoAll ? 'checked' : ''}><span></span></label></div>
  </div>
  <div class="card"><details class="adv logs-box"><summary><span class="chev">${icon('chev')}</span>${ico('terminal')}运行日志</summary>
    <pre class="logs" id="logs">加载中…</pre>
    <div class="row" style="margin-top:10px;align-items:center"><span class="muted small" id="logPath"></span><span class="spacer"></span><button class="btn sm" data-act="loadLogs">${icon('refresh')}刷新</button></div>
  </details></div>
  ${localBackupPanel(st)}
  <div class="card">
    <h3 class="card-title" style="margin-bottom:12px">${ico('info')}关于</h3>
    <dl class="kv"><dt>版本</dt><dd>${esc(st.version)}</dd><dt>设备 ID</dt><dd class="mono">${esc(c.deviceId)}</dd>
      <dt>程序位置</dt><dd class="mono">${esc(st.appBundle || st.exe || '—')} <button class="btn sm" data-act="reveal" data-w="exe">${icon('folder')}${openWord()}</button></dd>
      <dt>配置文件</dt><dd class="mono">${esc(c.configPath)} <button class="btn sm" data-act="reveal" data-w="config">${icon('folder')}${openWord()}</button></dd><dt>服务地址</dt><dd class="mono">${esc(c.effectiveHubUrl || '—')}</dd></dl>
    ${st.installedFrom ? `<div class="callout ok" style="margin-top:12px">${ico('check')}<div>已自动安装到 <code>${esc(st.appBundle)}</code>，开机自启也指向这里。原来下载的 <code>${esc(st.installedFrom)}</code> 可以删除。</div></div>` : ''}
    ${st.os === 'darwin' ? `<div class="callout" style="margin-top:12px">${ico('info')}<div>第一次让 Codex / Claude Code 读写“文稿”“桌面”“下载”里的项目时，macOS 会弹窗询问 <b>“Salcara Bridge”想访问…</b>，请点<b>允许</b>；点错了可以到 <b>系统设置 → 隐私与安全性 → 文件和文件夹</b> 里重新打开。</div></div>` : ''}
    <div class="divider"></div>
    <div class="row"><div class="li-main"><div class="li-title">退出程序</div><div class="li-sub">退出后手机将看不到这台电脑，正在运行的任务会停止</div></div>
      <button class="btn danger" data-act="quit">${icon('power')}退出 Salcara Bridge</button></div>
  </div>`;
  loadLogs();
}

RENDER_settings = function (view) {
  const target = view;
  const fallback = { version: '', os: 'windows', appBundle: '', exe: '', config: {
    approval: 'ask', projects: [], autostart: false, openConsoleOnStart: false,
    allowPhoneAutoAll: false, deviceId: '', configPath: '', effectiveHubUrl: ''
  } };
  const cached = S.state || fallback;
  renderSettingsPage(target, cached);
  void loadState().then((st) => {
    if (target.isConnected === false || S.route !== 'settings') return;
    renderSettingsPage(target, st);
    if (typeof window.__salcaraAttachSettingsPrefs === 'function') window.__salcaraAttachSettingsPrefs(target);
  }).catch((e) => {
    if (!S.state && target.isConnected !== false && S.route === 'settings') target.innerHTML = `<div class="callout bad">${ico('warn')}<div>${esc(e.message)}</div></div>`;
  });
};

async function loadLogs() {
  try {
    const r = await api('/api/logs');
    const el = $('#logs');
    if (!el) return;
    el.textContent = r.lines.length ? r.lines.join('\n') : '（暂无日志）';
    el.scrollTop = el.scrollHeight;
    $('#logPath').textContent = r.path ? '日志文件：' + r.path : '';
  } catch (e) { toast(e.message, 'bad'); }
}

const RENDER = {
  overview: (v) => RENDER_overview(v),
  accounts: (v) => RENDER_accounts(v),
  login: (v) => RENDER_login(v),
  tools: (v) => RENDER_tools(v),
  projects: (v) => RENDER_projects(v),
  sessions: (v) => RENDER_sessions(v),
  settings: (v) => RENDER_settings(v),
  usage: (v) => RENDER_usage(v),
};
var RENDER_overview, RENDER_accounts, RENDER_login, RENDER_tools, RENDER_projects, RENDER_sessions, RENDER_settings, RENDER_usage;

/* ---------------- actions ---------------- */

async function busy(btn, fn) {
  if (btn) { btn.disabled = true; btn.dataset.html = btn.innerHTML; btn.innerHTML = '<span class="spin"></span>' + btn.textContent; }
  try { return await fn(); } finally { if (btn && btn.isConnected) { btn.disabled = false; btn.innerHTML = btn.dataset.html; } }
}

const ACTIONS = {
  refresh: () => route(),
  remoteProbe: async (b) => {
    await busy(b, async () => {
      const out = $('#remoteProbeResult');
      S.remoteDiscovery = null; $('#remoteConnect').disabled = true;
      try {
        const body = { url: $('#remoteRoot').value, hubUrl: $('#remoteHub').value };
        const r = await api('/api/remote/discover', body);
        S.remoteDiscovery = { ...body, plugin: r.plugin };
        out.innerHTML = `<div class="callout ok">${ico('check')}<div>插件 v${esc(r.plugin.version)} · 协议 v${esc(r.plugin.protocolVersion)} · 支持设备扫码<br>${esc(r.plugin.hubUrl)}<br>可以连接，检测本身还没有授权手机控制电脑。</div></div>`;
        $('#remoteConnect').disabled = false;
      } catch (e) { out.innerHTML = `<div class="callout warn">${ico('warn')}<div>${esc(e.message)}</div></div>`; }
    });
  },
  remoteConnect: async (b) => {
    await busy(b, async () => {
      if (!S.remoteDiscovery || S.remoteDiscovery.url !== $('#remoteRoot').value || S.remoteDiscovery.hubUrl !== $('#remoteHub').value) throw new Error('地址已更改，请先重新检测插件');
      const result = await api('/api/remote/discover', { url: $('#remoteRoot').value, hubUrl: $('#remoteHub').value, connect: true });
      S.remoteDiscovery = { url: result.plugin.root, hubUrl: result.plugin.hubUrl, plugin: result.plugin };
      S.pairQR = ''; S.pairExpires = 0;
      toast('正在建立设备连接；连接成功后生成二维码', 'ok'); await route();
    });
  },
  pairRevoke: async (b) => {
    if (!confirm('解除这台电脑的手机绑定？旧手机和未使用的二维码都会失效，本地 API 密钥与会话保留。')) return;
    await busy(b, async () => { const r = await api('/api/pair/revoke', {}); S.pairQR = ''; S.pairExpires = 0; toast(r.message, 'ok'); await route(); });
  },
  toolRestart: async (b) => {
    if (b.dataset.target === 'claude-desktop') throw new Error('自动应用暂不可用。' + claudeDesktopApplyNote());
    const form = toolForm(b.dataset.target);
    if (!form.id || !form.model) throw new Error('请先选择 API 和模型');
    await api('/api/local/bind', form);
    S.local = await api('/api/local/accounts');
    b.dataset.id = form.id;
    await ACTIONS.localSwitch(b);
  },
  toolReadModels: async (b) => {
    await busy(b, async () => {
      const form = toolForm(b.dataset.target);
      if (!form.id) throw new Error('请先选择 API');
      await api('/api/local/bind', form);
      const r = await api('/api/local/models', { id: form.id });
      S.local = await api('/api/local/accounts');
      renderAgentCards();
      toast(`已读取 ${r.models.length} 个模型，请在这张卡片的模型输入框选择`, 'ok');
    });
  },
  toolRestore: async (b) => {
    if (!S.local) S.local = await api('/api/local/accounts');
    await openRestore(b.dataset.target);
  },
  restoreView: async (b) => {
    S.sessFilter = b.dataset.kind;
    S.openKey = b.dataset.k;
    $('#modalRoot').innerHTML = '';
    if (location.hash === '#sessions') await route(); else location.hash = 'sessions';
  },
  resumeSession: async (b) => {
    await busy(b, async () => {
      try {
        const r = await api('/api/local/resume', { target: b.dataset.target, sessionKey: b.dataset.k });
        toast(r.result.message, 'ok');
        const out = $('#restoreError');
        if (out) out.innerHTML = `<div class="callout ok">${ico('check')}<div>${esc(r.result.message)}</div></div>`;
      } catch (e) {
        const out = $('#restoreError');
        if (out) out.innerHTML = `<div class="callout warn">${ico('warn')}<div>${esc(e.message)}</div></div>`;
        else throw e;
      }
    });
  },
  localNew: async (b) => { if (!S.local) S.local = await api('/api/local/accounts'); localEditor(null, b.dataset.kind); },
  localEdit: (b) => localEditor(S.local.accounts.find((a) => a.id === b.dataset.id)),
  localSave: async (b) => {
    await busy(b, async () => {
      const body = localForm();
      if (!body.name.trim()) throw new Error('请填写账号名称');
      if (!body.id && !body.key.trim()) throw new Error('请填写 API Key');
      const saved = await api('/api/local/accounts', body);
      const wbTarget = b.dataset.wbTarget;
      const continueInAgent = wbTarget && b.isConnected && S.route === 'overview';
      $('#modalRoot').innerHTML = '';
      if (continueInAgent && typeof wbContinueWithSaved === 'function') {
        await wbContinueWithSaved(wbTarget, saved.account?.id);
        return;
      }
      toast('密钥已保存，到 Agent 选择工具并确认配置', 'ok');
      if (wbTarget) return;
      if (location.hash === '#accounts') await route(); else location.hash = 'accounts';
    });
  },
  localModels: async (b) => {
    await busy(b, async () => {
      const modal = !b.dataset.id && $('#laID');
      const body = modal ? localForm() : { id: b.dataset.id };
      const out = modal ? $('#localProbeResult') : null;
      try {
        const result = await api('/api/local/models', body);
        if (modal && out) {
          S.localDraftModels = result.models;
          out.innerHTML = `<div class="callout ok">${ico('check')}<div>读取到 ${result.models.length} 个模型，保存后可在 Agent 中配置。这里只验证目录访问，不代表推理已测试。</div></div><div class="model-catalog">${result.models.map((m) => `<code>${esc(m)}</code>`).join('')}</div>`;
        } else { toast(`已读取 ${result.models.length} 个模型；到 Agent 中配置`, 'ok'); await route(); }
      } catch (e) {
        if (out) out.innerHTML = `<div class="callout warn">${ico('warn')}<div>${esc(e.message)}</div></div>`; else throw e;
      }
    });
  },
  localSelect: async (b) => { const r = await api('/api/local/accounts/select', { id: b.dataset.id }); toast(r.message, 'ok'); await route(); },
  localDelete: async (b) => {
    if (!confirm('从账号列表移除？正在运行的工具、会话与实例配置目录会保留（其中可能仍有 Key）。需要撤销访问时，请同时在服务商后台撤销 Key。')) return;
    await api('/api/local/accounts/delete', { id: b.dataset.id }); toast('已从账号列表移除'); await route();
  },
  localLaunch: async (b) => {
    if (!confirm('这是高级“独立多开”：会打开一个独立的新实例，不显示原工具的历史。如果只是换 Key 继续原工作，请取消并使用“切换并打开”。确定新建独立实例？')) return;
    await busy(b, async () => {
      const target = b.dataset.target || document.getElementById('localTarget-' + b.dataset.id)?.value;
      const r = await api('/api/local/launch', { id: b.dataset.id, target });
      toast('已启动 ' + (TOOL_LABEL[target] || (target === 'codex-desktop' ? 'Codex Desktop' : target)), 'ok');
      const hint = $('#localLaunchResult');
      if (hint) hint.remove();
      const note = document.createElement('div'); note.id = 'localLaunchResult'; note.className = 'callout ok';
      note.textContent = r.result.message + ' 实例目录：' + r.result.profileDir;
      $('#view').prepend(note);
      if (S.local) { const a = S.local.accounts.find((a) => a.id === b.dataset.id); if (a) { a.target = target; a.lastUsedAt = Date.now(); } }
    });
  },
  localSwitch: async (b) => {
    await busy(b, async () => {
      const target = b.dataset.target || document.getElementById('localTarget-' + b.dataset.id)?.value;
      const r = await api('/api/local/switch/preview', { id: b.dataset.id, target });
      const a = { ...S.local.accounts.find((x) => x.id === b.dataset.id), model: r.info.model };
      $('#modalRoot').innerHTML = `<div class="modal-back" data-act="closeModal"><div class="modal account-modal" data-stop role="dialog" aria-modal="true" aria-labelledby="switchTitle"><div class="modal-head"><h3 id="switchTitle">切换原来的 ${esc(r.info.name)}</h3><button class="icon-btn" data-act="closeModal" aria-label="关闭">✕</button></div><div class="modal-body"><p>切换到 <b>${esc(a.name)}</b> · ${esc(a.model)}</p><div class="callout">${ico('info')}<div>${esc(r.info.message)}</div></div><p class="muted small">原配置 / 会话目录</p><code class="path-help">${esc(r.info.profileDir)}</code>${r.info.appData ? `<p class="muted small">原桌面数据目录（不更换）</p><code class="path-help">${esc(r.info.appData)}</code>` : ''}${r.info.provider ? `<p class="muted small">保留 Codex 供应商标识：${esc(r.info.provider)}</p>` : ''}<p class="muted small">云端专属内容仍取决于原账户权限；切换不会把云端数据迁移到新 Key。</p><label class="row"><input type="checkbox" id="switchReady">我已结束正在执行的任务，保存了编辑内容${target.endsWith('-desktop') ? '' : '，并退出旧终端进程'}</label><div id="switchError" aria-live="polite"></div></div><div class="modal-foot"><button class="btn" data-act="closeModal">取消</button><button class="btn primary" data-act="localSwitchConfirm" data-id="${esc(a.id)}" data-target="${esc(target)}">${icon('refresh')}确认切换并打开</button></div></div></div>`;
    });
  },
  localSwitchConfirm: async (b) => {
    if (!$('#switchReady')?.checked) throw new Error('请先结束任务、保存内容，并勾选确认');
    await busy(b, async () => {
      try {
        const r = await api('/api/local/switch', { id: b.dataset.id, target: b.dataset.target, confirmed: true });
        $('#modalRoot').innerHTML = '';
        await route();
        const note = document.createElement('div'); note.className = 'callout ok'; note.textContent = r.result.message;
        $('#view').prepend(note); toast('已切换并打开原工具', 'ok');
      } catch (e) {
        const out = $('#switchError');
        if (out) out.innerHTML = `<div class="callout warn">${ico('warn')}<div>${esc(e.message)}</div></div>`;
        else throw e;
      }
    });
  },
  localApply: async (b) => {
    if (!confirm('只更新原工具的 CLI API 配置，不自动打开？会先备份，保留原会话。请先退出旧工具，更新后手动重开；后续打开也会使用这个 Key。')) return;
    await busy(b, async () => { const r = await api('/api/local/default', { id: b.dataset.id, action: 'apply' }); toast(r.message, 'ok'); });
  },
  localRestore: async (b) => {
    if (!confirm('恢复通过本地账号应用前的系统配置？若配置后来被其他程序修改，会停止自动恢复以保护新改动。')) return;
    await busy(b, async () => { const r = await api('/api/local/default', { kind: b.dataset.kind, action: 'restore' }); toast(r.message, 'ok'); await route(); });
  },
  localPaths: async (b) => {
    await busy(b, async () => {
      const paths = {}; (S.local?.tools || []).forEach((t) => { paths[t.id] = document.getElementById('localPath-' + t.id).value; });
      await api('/api/local/tools', { paths }); toast('程序路径已保存', 'ok'); await route();
    });
  },
  pairStart: async (button) => {
    await busy(button, async () => {
      try {
        const result = await api('/api/pair/start', {});
        S.pairCode = result.code;
        S.pairExpires = result.expiresAt;
        S.pairQR = result.qrUrl || '';
        const qr = $('#pairQR');
        if (qr) qr.innerHTML = result.qrUrl ? `<img src="${esc(result.qrUrl)}" alt="一次性手机绑定二维码"><p class="muted small">有效期至 ${new Date(result.expiresAt).toLocaleTimeString()}</p>` : '<p class="muted">此旧版插件仅支持配对码，请更新插件使用扫码。</p>';
        setTimeout(() => { if (S.pairExpires <= Date.now()) { S.pairQR = ''; const box = $('#pairQR'); if (box) box.innerHTML = '<p class="muted">二维码已过期，请重新生成。</p>'; } }, Math.max(0, result.expiresAt - Date.now()) + 100);
        const output = $('#pairCode');
        if (output) output.textContent = result.code;
        toast(result.qrUrl ? '用手机 Salcara 顶部「编程」扫码' : '旧版插件：在手机上输入配对码', 'ok');
      } catch (error) { toast(error.message, 'bad'); }
    });
  },
  reveal: async (b) => { try { await api('/api/reveal', { which: b.dataset.w }); } catch (e) { toast(e.message, 'bad'); } },
  editName: () => { S.editingName = true; route().then(() => { const i = $('#devInput'); if (i) { i.focus(); i.select(); } }); },
  cancelName: () => { S.editingName = false; route(); },
  saveName: async (b) => {
    await busy(b, async () => {
      try { await api('/api/device', { name: $('#devInput').value }); S.editingName = false; toast('设备名称已更新', 'ok'); route(); } catch (e) { toast(e.message, 'bad'); }
    });
  },
  toggleVis: (b) => { const i = document.getElementById(b.dataset.for); i.type = i.type === 'password' ? 'text' : 'password'; },
  testKey: async (b) => {
    await busy(b, async () => {
      const out = $('#testResult');
      try {
        const r = await api('/api/test', { relayRoot: $('#fRoot').value, key: $('#fKey').value });
        out.innerHTML = testResultHTML(r.usage || {});
      } catch (e) { out.innerHTML = `<div class="callout bad">${ico('warn')}<div>${esc(e.message)}</div></div>`; }
    });
  },
  login: async (b) => {
    await busy(b, async () => {
      const body = { relayRoot: $('#fRoot').value, accountKey: $('#fKey').value, hubUrl: $('#fHub').value };
      const cx = $('#fCodex').value.trim(), cl = $('#fClaude').value.trim();
      if (cx) body.codexKey = cx; else if ($('#fCodexClear') && $('#fCodexClear').checked) body.codexKey = '';
      if (cl) body.claudeKey = cl; else if ($('#fClaudeClear') && $('#fClaudeClear').checked) body.claudeKey = '';
      try {
        const r = await api('/api/login', body);
        toast(r.autostartEnabled ? '登录成功，已开启开机自启' : '登录成功', 'ok');
        await route();
        const out = $('#testResult');
        if (out) out.innerHTML = testResultHTML(r.usage || {});
      } catch (e) { const out = $('#testResult'); out.innerHTML = `<div class="callout bad">${ico('warn')}<div>${esc(e.message)}</div></div>`; }
    });
  },
  logout: async () => {
    if (!confirm('退出登录后，手机将看不到这台电脑。确定吗？')) return;
    await api('/api/logout', {});
    S.pairQR = ''; S.pairExpires = 0;
    toast('已退出登录');
    route();
  },
  toolcfg: async (b) => {
    const tool = b.dataset.tool, a = b.dataset.a;
    if (a === 'restore' && !confirm('恢复到一键配置之前的原配置？')) return;
    const m = $('input[name=cxmode]:checked');
    await busy(b, async () => {
      try {
        const r = await api('/api/toolcfg', { tool, action: a, mode: m ? m.value : 'token' });
        toast(r.note || '完成', 'ok');
        route();
      } catch (e) { toast(e.message, 'bad'); }
    });
  },
  copyVal: (b) => copyText(b.dataset.v, b.dataset.w),
  copySecret: async (b) => { try { const r = await api('/api/secret?which=' + b.dataset.which); copyText(r.value, 'Key'); } catch (e) { toast(e.message, 'bad'); } },
  browse: async (button) => {
    const input = $('#projPath');
    if (!window.salcaraWindow?.chooseDirectory) {
      if (window.salcaraWindow) throw new Error('请更新电脑端以使用系统文件夹选择器');
      return openBrowser(input.value.trim()); // standalone browser console only
    }
    await busy(button, async () => {
      const folder = await window.salcaraWindow.chooseDirectory(input.value.trim());
      if (folder && input.isConnected) input.value = folder;
    });
  },
  fsGo: (b) => S.fsLoad(b.dataset.p),
  fsPick: () => { const c = S.fsCur(); $('#modalRoot').innerHTML = ''; const i = $('#projPath'); if (i && c) { i.value = c.path; ACTIONS.addProj($('[data-act=addProj]')); } },
  closeModal: (b, e) => { if (b.classList.contains('modal-back') && e.target.closest('[data-stop]')) return; $('#modalRoot').innerHTML = ''; },
  addProj: async (b) => {
    const p = $('#projPath').value.trim();
    if (!p) { toast('请填写文件夹路径', 'bad'); return; }
    await busy(b, async () => {
      try { await api('/api/projects', { path: p }); toast('已添加', 'ok'); route(); } catch (e) { toast(e.message, 'bad'); }
    });
  },
  rmProj: async (b) => {
    if (!confirm('移除后手机不能再在这个文件夹里发起任务。确定吗？')) return;
    await api('/api/projects/remove', { path: b.dataset.path });
    route();
  },
  sessFilter: (b) => { S.sessFilter = b.dataset.k; $$('.filter').forEach((x) => x.classList.toggle('on', x === b)); loadSessions(); },
  reloadSessions: () => loadSessions(),
  openSess: (b) => openSession(b.dataset.k),
  approve: async (b) => {
    await busy(b, async () => {
      try { await api('/api/approval', { approvalId: b.dataset.id, decision: b.dataset.d }); toast('已处理', 'ok'); } catch (e) { toast(e.message, 'bad'); }
      S.approvals = S.approvals.filter((a) => a.approvalId !== b.dataset.id);
      setApprovalBadge();
      renderApprovalsBox();
    });
  },
  send: async (b) => {
    const t = $('#sendText');
    const text = t.value.trim();
    if (!text) return;
    await busy(b, async () => {
      try { await api('/api/session/send', { sessionKey: S.openKey, text }); t.value = ''; } catch (e) { toast(e.message, 'bad'); }
    });
  },
  interrupt: async (b) => { await busy(b, async () => { try { await api('/api/session/interrupt', { sessionKey: S.openKey }); toast('已发送停止'); } catch (e) { toast(e.message, 'bad'); } }); },
  saveModels: async (b) => {
    await busy(b, async () => {
      try { await api('/api/settings', { codexModel: $('#mCodex').value, claudeModel: $('#mClaude').value }); toast('已保存', 'ok'); } catch (e) { toast(e.message, 'bad'); }
    });
  },
  loadLogs: () => loadLogs(),
  quit: async () => {
    if (!confirm('退出 Salcara Bridge？手机将看不到这台电脑，直到你再次打开它。')) return;
    try { await api('/api/quit', {}); } catch (_) { /* ignore */ }
    document.body.innerHTML = '<div style="display:flex;height:100vh;align-items:center;justify-content:center;flex-direction:column;gap:8px;font-family:var(--font)"><h2 style="margin:0">Salcara Bridge 已退出</h2><p style="color:#6D768B">可以关闭这个页面了。需要时' + launchHint() + '即可重新打开。</p></div>';
  },
};

document.addEventListener('click', (e) => {
  const el = e.target.closest('[data-act]');
  if (!el) return;
  if (el.tagName === 'A' && el.getAttribute('href')) return;
  const fn = ACTIONS[el.dataset.act];
  if (!fn) return;
  if (el.dataset.act === 'closeModal' && el.classList.contains('modal-back') && e.target !== el) return;
  e.preventDefault();
  Promise.resolve(fn(el, e)).catch((err) => toast(err.message, 'bad'));
});

document.addEventListener('change', async (e) => {
  const t = e.target;
  try {
    if (t.id === 'localKind') { S.localKind = t.value; renderLocalCards(); }
    else if (t.id === 'remoteSaved' && t.value) { const u = new URL(t.value); $('#remoteRoot').value = u.origin; $('#remoteHub').value = t.value; S.remoteDiscovery = null; $('#remoteConnect').disabled = true; }
    else if (t.id === 'laBase') { try { if (new URL(t.value).hostname === 'api.anthropic.com') $('#laAuth').value = 'api-key'; } catch (_) {} }
    else if (t.id.startsWith('toolAPI-') || t.id.startsWith('toolModel-') || t.id.startsWith('toolProtocol-')) {
      const form = toolForm(t.dataset.target), keyChanged = t.id.startsWith('toolAPI-');
      if (keyChanged) {
        const account = S.local.accounts.find((a) => a.id === form.id);
        try {
          const host = new URL(account?.baseUrl).hostname;
          if (host === 'api.anthropic.com') form.protocol = 'anthropic';
          else if (host === 'api.x.ai') form.protocol = 'chat';
        } catch (_) {}
      }
      t.disabled = true;
      await api('/api/local/bind', form);
      S.local = await api('/api/local/accounts');
      if (keyChanged) renderAgentCards();
      else { t.disabled = false; toolDraftStatus(t.dataset.target); }
      toast(form.id ? '卡片设置已保存，点击“应用并重启”才会切换' : '已取消选择');
    }
    else if (t.id.startsWith('localTarget-')) { const a = S.local.accounts.find((a) => 'localTarget-' + a.id === t.id); const b = t.closest('.account-launch').querySelector('button'); if (b) b.disabled = !a?.model || !S.local.tools.some((x) => x.id === t.value && x.available); }
    else if (t.id === 'swAuto') { await api('/api/settings', { autostart: t.checked }); toast(t.checked ? '已开启开机自启' : '已关闭开机自启', 'ok'); }
    else if (t.id === 'swOpen') { await api('/api/settings', { openConsoleOnStart: t.checked }); toast('已保存', 'ok'); }
    else if (t.id === 'swPhoneAuto') { await api('/api/settings', { allowPhoneAutoAll: t.checked }); toast(t.checked ? '手机可以选择「全部自动」' : '已禁止手机使用「全部自动」', 'ok'); }
    else if (t.name === 'pol') {
      if (t.value === 'auto_all' && !confirm('“全部自动”会让 AI 不经确认运行任意命令，确定开启吗？')) { route(); return; }
      await api('/api/settings', { approval: t.value }); toast('审批策略已更新', 'ok'); route();
    } else if (t.name === 'cxmode') {
      $$('input[name=cxmode]').forEach((i) => i.closest('.radio-card').classList.toggle('sel', i.checked));
    }
  } catch (err) { toast(err.message, 'bad'); if (t.type === 'checkbox') t.checked = !t.checked; if (t.id.startsWith('tool')) { t.disabled = false; toolDraftStatus(t.dataset.target); } }
});

document.addEventListener('input', (e) => {
  if (e.target.id === 'localSearch') { S.localFilter = e.target.value; renderLocalCards(); }
  else if (e.target.id === 'remoteRoot' || e.target.id === 'remoteHub') { S.remoteDiscovery = null; $('#remoteConnect').disabled = true; }
  else if (e.target.id === 'restoreSearch') { S.restoreSearch = e.target.value; renderRestoreList(); }
  else if (e.target.id === 'sessionSearch') { S.sessSearch = e.target.value; renderSessionList(); }
  else if (e.target.id.startsWith('toolModel-')) { toolDraftStatus(e.target.dataset.target); }
});

document.addEventListener('keydown', (e) => {
  if (e.target.id === 'sendText' && e.key === 'Enter' && (e.ctrlKey || e.metaKey)) { e.preventDefault(); ACTIONS.send($('[data-act=send]')); }
  if (e.target.id === 'devInput' && e.key === 'Enter') ACTIONS.saveName($('[data-act=saveName]'));
  if (e.target.id === 'projPath' && e.key === 'Enter') ACTIONS.addProj($('[data-act=addProj]'));
  if (e.key === 'Escape') $('#modalRoot').innerHTML = '';
});

/* ---------------- live stream ---------------- */

let bootApprovalChanges = null;
function connectStream() {
  const es = new EventSource('/api/stream');
  es.addEventListener('status', (m) => {
    const st = JSON.parse(m.data);
    if (S.state) S.state.hub = st;
    setConn(st);
    if (typeof rmPairStatus === 'function') rmPairStatus(st);
    refreshWorkbenchStatus();
  });
  es.addEventListener('event', (m) => {
    const ev = JSON.parse(m.data);
    if (bootApprovalChanges && (ev.type === 'approval.request' || ev.type === 'approval.resolved')) bootApprovalChanges.set(ev.approvalId, ev);
    if (typeof NOTIFY !== 'undefined') NOTIFY.onEvent(ev);
    if (ev.type === 'approval.request') {
      if (!S.approvals.find((a) => a.approvalId === ev.approvalId)) S.approvals.push(ev);
      setApprovalBadge(); renderApprovalsBox();
    } else if (ev.type === 'approval.resolved') {
      S.approvals = S.approvals.filter((a) => a.approvalId !== ev.approvalId);
      setApprovalBadge(); renderApprovalsBox();
    }
    if (ev.type === 'session.updated' && ev.session) {
      const i = S.sessions.findIndex((s) => s.sessionKey === ev.session.sessionKey);
      if (i >= 0) S.sessions[i] = ev.session; else if (!S.sessFilter || S.sessFilter === ev.session.tool) S.sessions.unshift(ev.session);
      S.sessions.sort((a, b) => b.updatedAt - a.updatedAt);
      if (S.route === 'sessions') renderSessionList();
    }
    if (S.route === 'sessions' && S.openKey && ev.sessionKey === S.openKey) {
      tlApply(ev);
      renderDetail(false);
    }
  });
  es.onerror = () => {
    setConn({ state: 'connecting', error: '与本机程序的连接断开' });
    $$('.remote-lamp[data-remote-capability="cli"]').forEach((lamp) => { lamp.className = 'remote-lamp lamp-connecting'; lamp.innerHTML = '<span class="lamp-dot"></span>状态连接中断'; });
  };
}

function boot() {
  window.addEventListener('hashchange', route);
  $$('[data-icon]').forEach((el) => { el.innerHTML = icon(el.dataset.icon); });
  const approvalChanges = new Map();
  bootApprovalChanges = approvalChanges;
  connectStream();
  void route();
  void loadState().catch(() => undefined);
  // Paint the local shell before reading approvals or any optional status.
  void api('/api/approvals').then((r) => {
    // The event stream is already live. A late startup snapshot must neither
    // erase new questions nor resurrect permissions resolved during this read.
    const approvals = new Map((r.approvals || []).map(item => [item.approvalId, item]));
    for (const [id, event] of approvalChanges) {
      if (event.type === 'approval.request') approvals.set(id, event);
      else approvals.delete(id);
    }
    S.approvals = [...approvals.values()];
    setApprovalBadge();
    renderApprovalsBox();
  }).catch(() => undefined).finally(() => { if (bootApprovalChanges === approvalChanges) bootApprovalChanges = null; });
}
// Boot after every page script (remote.js, workbench.js) has registered its renderers.
if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', boot); else setTimeout(boot, 0);
