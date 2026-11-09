'use strict';
/* Salcara Bridge console — plain JS, no build step. Every dynamic string goes through esc(). */

const $ = (s, el = document) => el.querySelector(s);
const $$ = (s, el = document) => Array.from(el.querySelectorAll(s));
const esc = (v) => String(v ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

const ICONS = {
  // Salcara icon set v3 (shared with the phone app): 24 grid, 1.75 strokes, line-first.
  // A few brand icons carry one dot (class "dot"), painted in the brand blue by CSS.
  spark: '<path d="M12 3.5l1.9 5.1 5.1 1.9-5.1 1.9L12 17.5l-1.9-5.1L5 10.5l5.1-1.9z"/><circle class="dot" cx="18.5" cy="18.5" r="1.6"/>',
  github: '<path fill="currentColor" stroke="none" d="M12 .75a11.25 11.25 0 0 0-3.56 21.92c.56.1.77-.24.77-.54v-2.1c-3.13.68-3.79-1.33-3.79-1.33-.51-1.3-1.25-1.65-1.25-1.65-1.02-.7.08-.69.08-.69 1.13.08 1.72 1.16 1.72 1.16 1.01 1.72 2.64 1.22 3.28.93.1-.73.4-1.22.71-1.5-2.5-.29-5.13-1.25-5.13-5.56 0-1.23.44-2.23 1.16-3.02-.12-.29-.5-1.43.11-2.98 0 0 .95-.3 3.1 1.16a10.8 10.8 0 0 1 5.63 0c2.15-1.46 3.09-1.16 3.09-1.16.62 1.55.23 2.69.12 2.98.72.79 1.15 1.79 1.15 3.02 0 4.32-2.63 5.27-5.14 5.55.4.35.76 1.04.76 2.1v3.09c0 .3.2.65.77.54A11.25 11.25 0 0 0 12 .75z"/>',
  star: '<path d="m12 3 2.8 5.7 6.2.9-4.5 4.4 1.1 6.2-5.6-2.9-5.6 2.9 1.1-6.2L3 9.6l6.2-.9z"/>',
  home: '<path d="M4 10.5 12 4l8 6.5V18a2 2 0 0 1-2 2h-3.5v-5.5h-5V20H6a2 2 0 0 1-2-2z"/>',
  key: '<circle cx="8.5" cy="15.5" r="4"/><path d="M11.5 12.5 19.5 4.5M16.5 7.5l2.5 2.5"/>',
  plug: '<path d="M9 3v4.5M15 3v4.5M6.5 7.5h11V11a5.5 5.5 0 0 1-11 0zM12 16.5V21"/>',
  folder: '<path d="M3.5 7.5a2 2 0 0 1 2-2h4l2 2h7a2 2 0 0 1 2 2v7.5a2 2 0 0 1-2 2h-13a2 2 0 0 1-2-2z"/>',
  chat: '<path d="M7.5 4h9A3.5 3.5 0 0 1 20 7.5v5a3.5 3.5 0 0 1-3.5 3.5H12l-4.5 3.5V16A3.5 3.5 0 0 1 4 12.5v-5A3.5 3.5 0 0 1 7.5 4z"/>',
  gear: '<path d="M4 8h8.5M18.5 8H20M4 16h1.5M11.5 16H20"/><circle cx="15.5" cy="8" r="2.5"/><circle cx="8.5" cy="16" r="2.5"/>',
  edit: '<path d="M4.5 19.5l.9-3.9L15.6 5.4a2 2 0 0 1 2.8 0l.2.2a2 2 0 0 1 0 2.8L8.4 18.6z"/><path d="m14.2 6.8 3 3"/>',
  copy: '<path d="M11 9h7a2 2 0 0 1 2 2v7a2 2 0 0 1-2 2h-7a2 2 0 0 1-2-2v-7a2 2 0 0 1 2-2z"/><path d="M15 5.5A1.5 1.5 0 0 0 13.5 4H6a2 2 0 0 0-2 2v7.5A1.5 1.5 0 0 0 5.5 15"/>',
  eye: '<path d="M2.8 12c1.9-4 5.2-6.5 9.2-6.5s7.3 2.5 9.2 6.5c-1.9 4-5.2 6.5-9.2 6.5S4.7 16 2.8 12z"/><circle cx="12" cy="12" r="2.9"/>',
  info: '<circle cx="12" cy="12" r="8.5"/><path d="M12 11v5"/><circle cx="12" cy="7.8" r=".6"/>',
  warn: '<path d="M10.3 4.6 3.4 17a2 2 0 0 0 1.7 3h13.8a2 2 0 0 0 1.7-3L13.7 4.6a2 2 0 0 0-3.4 0z"/><path d="M12 10v4.5M12 17.2v.3"/>',
  check: '<circle cx="12" cy="12" r="8.5"/><path d="m8.3 12.3 2.5 2.5 4.9-5"/>',
  phone: '<rect x="6.5" y="3" width="11" height="18" rx="3"/><path d="M10.5 17.5h3"/>',
  trash: '<path d="M4.5 6.5h15M9.5 6.5V5A1.5 1.5 0 0 1 11 3.5h2A1.5 1.5 0 0 1 14.5 5v1.5M6.5 6.5l.8 11.6a2.2 2.2 0 0 0 2.2 2.1h5a2.2 2.2 0 0 0 2.2-2.1l.8-11.6"/>',
  plus: '<path d="M12 5v14M5 12h14"/>',
  download: '<path d="M12 4v10M7.5 10 12 14.5 16.5 10"/><path d="M5 16.5v1A2.5 2.5 0 0 0 7.5 20h9a2.5 2.5 0 0 0 2.5-2.5v-1"/>',
  refresh: '<path d="M19.5 12a7.5 7.5 0 1 1-2.2-5.3"/><path d="M19.5 4v4h-4"/>',
  chev: '<path d="M9.5 5.5 16 12l-6.5 6.5"/>',
  up: '<path d="M12 19V6M6.5 11.5 12 6l5.5 5.5"/>',
  terminal: '<path d="M6.5 4.5h11a3 3 0 0 1 3 3v9a3 3 0 0 1-3 3h-11a3 3 0 0 1-3-3v-9a3 3 0 0 1 3-3z"/><path d="m7.5 10 2.5 2.5L7.5 15M13 15h3.5"/>',
  file: '<path d="M7 3.5h6l5 5v9.5a2.5 2.5 0 0 1-2.5 2.5h-8.5A2.5 2.5 0 0 1 4.5 18V6A2.5 2.5 0 0 1 7 3.5z"/><path d="M13 3.5V8h5M8.5 13h7M8.5 16.5h4"/>',
  search: '<path d="M17 10.5A6.5 6.5 0 1 1 10.5 4"/><path d="m15.5 15.5 4.5 4.5"/><circle class="dot" cx="15.6" cy="5.4" r="1.5"/>',
  globe: '<circle cx="12" cy="12" r="8.5"/><path d="M3.5 12h17M12 3.5c2.4 2.4 3.5 5.2 3.5 8.5s-1.1 6.1-3.5 8.5c-2.4-2.4-3.5-5.2-3.5-8.5S9.6 5.9 12 3.5z"/>',
  box: '<path d="M12 3.5 19.5 7.7v8.6L12 20.5l-7.5-4.2V7.7z"/><path d="M4.5 7.7 12 12l7.5-4.3M12 12v8.5"/>',
  stop: '<rect x="6.5" y="6.5" width="11" height="11" rx="2.5"/>',
  send: '<path d="M12 19V6M6.5 11.5 12 6l5.5 5.5"/>',
  power: '<path d="M12 3.5v8"/><path d="M6.6 7.4a7.5 7.5 0 1 0 10.8 0"/>',
  wallet: '<path d="M4 7.5A2.5 2.5 0 0 1 6.5 5h11A2.5 2.5 0 0 1 20 7.5v9a2.5 2.5 0 0 1-2.5 2.5h-11A2.5 2.5 0 0 1 4 16.5z"/><path d="M15.5 12h1.5M4 9.5h16"/>',
  bolt: '<path d="M13.2 3.5 5.8 13.2h5.9l-1 7.3 7.5-9.8h-6z"/>',
  clock: '<circle cx="12" cy="12" r="8.5"/><path d="M12 7.5V12l3 2"/>',
  chart: '<path d="M4 19.5h16"/><path d="m5 15 4.5-4.5 3.5 3 6-6.5"/><circle class="dot" cx="19" cy="7" r="1.5"/>',
  mcp: '<circle cx="6" cy="12" r="2.3"/><circle cx="18" cy="6" r="2.3"/><circle cx="18" cy="18" r="2.3"/><path d="M8.1 11 15.9 7M8.1 13l7.8 4"/>',
  puzzle: '<path d="M9 4.5a2 2 0 1 1 4 0v1h3.5A1.5 1.5 0 0 1 18 7v3h1a2 2 0 1 1 0 4h-1v3a1.5 1.5 0 0 1-1.5 1.5H13v-1a2 2 0 1 0-4 0v1H6.5A1.5 1.5 0 0 1 5 17v-3H4a2 2 0 1 1 0-4h1V7a1.5 1.5 0 0 1 1.5-1.5H9z"/>',
  tag: '<path d="M4 5.5V11l8.5 8.5a1.5 1.5 0 0 0 2.1 0l4.9-4.9a1.5 1.5 0 0 0 0-2.1L11 4H5.5A1.5 1.5 0 0 0 4 5.5z"/><circle class="dot" cx="8.5" cy="8.5" r="1.5"/>',
  lock: '<path d="M6.5 10.5h11a1.5 1.5 0 0 1 1.5 1.5v6.5a1.5 1.5 0 0 1-1.5 1.5h-11A1.5 1.5 0 0 1 5 18.5V12a1.5 1.5 0 0 1 1.5-1.5zM8.5 10.5V8a3.5 3.5 0 0 1 7 0v2.5"/>',
  play: '<path d="M8 6.2v11.6a1.2 1.2 0 0 0 1.8 1l9.2-5.8a1.2 1.2 0 0 0 0-2L9.8 5.2A1.2 1.2 0 0 0 8 6.2z"/>',
  more: '<circle cx="5.5" cy="12" r="1.4" fill="currentColor" stroke="none"/><circle cx="12" cy="12" r="1.4" fill="currentColor" stroke="none"/><circle cx="18.5" cy="12" r="1.4" fill="currentColor" stroke="none"/>',
  close: '<path d="M6.5 6.5l11 11M17.5 6.5l-11 11"/>',
  upload: '<path d="M12 15V4M7.5 8.5 12 4l4.5 4.5M5 15v2.5A2.5 2.5 0 0 0 7.5 20h9a2.5 2.5 0 0 0 2.5-2.5V15"/>',
  branch: '<circle cx="7" cy="5.5" r="2"/><circle cx="7" cy="18.5" r="2"/><circle cx="17" cy="7.5" r="2"/><path d="M7 7.5v9M17 9.5c0 4.5-10 2.5-10 7"/>',
  image: '<path d="M7 4h10a3 3 0 0 1 3 3v10a3 3 0 0 1-3 3H7a3 3 0 0 1-3-3V7a3 3 0 0 1 3-3zM4 16.5l4.3-4.3a2 2 0 0 1 2.8 0L18 19M14 15l1.6-1.6a2 2 0 0 1 2.8 0l1.6 1.6"/><circle class="dot" cx="15.5" cy="8.5" r="1.6"/>',
  db: '<path d="M5 7c0-1.7 3.1-3 7-3s7 1.3 7 3-3.1 3-7 3-7-1.3-7-3zM5 7v10c0 1.7 3.1 3 7 3s7-1.3 7-3V7M5 12c0 1.7 3.1 3 7 3s7-1.3 7-3"/>',
  menu: '<path d="M4 8h11M4 16h16"/><circle class="dot" cx="19.5" cy="8" r="1.6"/>',
  compose: '<path d="M11 4H7.5A3.5 3.5 0 0 0 4 7.5v9A3.5 3.5 0 0 0 7.5 20h9a3.5 3.5 0 0 0 3.5-3.5V13M10.5 13.5 19 5"/><circle class="dot" cx="9.6" cy="14.4" r="1.5"/>',
};
const icon = (n) => `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round">${ICONS[n] || ''}</svg>`;
const ico = (n) => `<span class="ico">${icon(n)}</span>`;

const STATUS = {
  connected: '已连接',
  connecting: '连接中',
  not_logged_in: '本地模式',
  invalid_key: '远程授权被拒绝',
  device_auth_required: '设备需重新登记',
  source_unauthorized: '远程来源未授权',
  pair_required: '手机需重新绑定',
  remote_blocked: '远程授权待处理',
};
const TOOL_LABEL = { codex: 'Codex CLI', 'codex-desktop': 'Codex Desktop', claude: 'Claude Code', 'claude-desktop': 'Claude Desktop', 'deepseek-harness': 'DeepSeek Harness', hermes: 'Hermes Agent' };
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

async function api(path, body, method, options = {}) {
  const controller = new AbortController();
  const abort = () => controller.abort();
  if (options.signal?.aborted) abort();
  else options.signal?.addEventListener('abort', abort, { once: true });
  const timer = setTimeout(abort, options.timeoutMs || 65000);
  const opts = { method: method || (body !== undefined ? 'POST' : 'GET'), headers: {}, credentials: 'same-origin', signal: controller.signal };
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  try {
    const res = await fetch(path, opts);
    let data = null;
    try { data = await res.json(); } catch (error) { if (controller.signal.aborted) throw error; }
    if (!res.ok) throw Object.assign(new Error((data && data.error) || `请求失败 (${res.status})`), { status: res.status, code: typeof data?.code === 'string' ? data.code : undefined });
    if (data === null) throw new Error('本机响应不完整，请重试');
    return data;
  } catch (e) {
    if (options.signal?.aborted) throw Object.assign(new Error('读取已取消'), { name: 'AbortError' });
    if (controller.signal.aborted) throw new Error('本机响应超时，请重试');
    if (e instanceof TypeError) throw new Error('连不上 Salcara Bridge，程序可能已经退出');
    throw e;
  } finally { clearTimeout(timer); options.signal?.removeEventListener('abort', abort); }
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
    try {
      await navigator.clipboard.writeText(text);
    } catch (_) {
      const ta = document.createElement('textarea');
      ta.value = text;
      ta.readOnly = true;
      ta.tabIndex = -1;
      ta.setAttribute('aria-hidden', 'true');
      ta.style.cssText = 'position:fixed;left:-10000px;top:0;width:1px;height:1px;opacity:0;pointer-events:none';
      try {
        document.body.appendChild(ta);
        ta.select();
        if (document.execCommand('copy') !== true) throw new Error('copy unavailable');
      } finally {
        // A fallback failure must not leave a plaintext key outside its modal.
        ta.value = '';
        ta.remove();
      }
    }
  } catch (_) {
    toast('复制失败，请重试或手动复制', 'bad');
    return false;
  }
  toast(`已复制${what ? ' ' + what : ''}`, 'ok');
  return true;
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
  overview: { title: 'Agent', sub: '选择 API、重启工具、恢复会话' },
  accounts: { title: 'API 密钥', sub: '管理密钥，在 Agent 中选择' },
  login: { title: '远程连接', sub: '扫码绑定手机' },
  tools: { title: '工具配置', sub: '接入 Claude Code 与 Codex' },
  projects: { title: '项目文件夹', sub: '手机任务可用的目录' },
  sessions: { title: '会话恢复', sub: '恢复本地 Codex / Claude Code 会话' },
  settings: { title: '设置', sub: '启动、审批和日志' },
  usage: { title: '用量', sub: '模型与 API 用量' },
};

function connectionPresentation(st) {
  const config = S.state?.config;
  const mode = typeof rmEffectiveMode === 'function' ? rmEffectiveMode(config) : config?.effectiveRemoteMode || config?.activeRemoteMode;
  if (st?.consoleDisconnected) return { state: 'connecting', label: '状态连接中断', title: st.error || '暂时无法读取本机状态' };
  if (mode === 'direct') {
    const direct = typeof RM !== 'undefined' ? RM.direct : null;
    if (direct?.error || direct?.enabled === true && direct.running === false) return { state: 'error', label: '直连待恢复', title: direct.error || '直连监听尚未运行，请在手机远程页查看' };
    return { state: 'direct', label: direct?.enabled && direct.running ? '直连已启用' : '直连模式', title: '已选择电脑直连；手机是否在线请查看手机远程页。' };
  }
  const state = mode === 'none' ? 'not_logged_in' : st?.state || 'not_logged_in';
  const label = state === 'invalid_key' && config?.remoteDeviceOnly ? '设备需重新登记' : STATUS[state] || state;
  const visual = ['device_auth_required', 'source_unauthorized', 'pair_required', 'remote_blocked', 'invalid_key'].includes(state) ? 'error' : state;
  return { state: visual, label: mode === 'bot' ? `Bot · ${state === 'not_logged_in' ? '待连接' : label}` : label, title: mode === 'none' ? '' : st?.error || '' };
}

function setConn(st) {
  const signal = connectionPresentation(st), s = signal.state, label = signal.label;
  const pill = $('#connPill');
  pill.className = 'conn st-' + s;
  $('.conn-label', pill).textContent = `远程 · ${label}`;
  pill.title = signal.title;
  const hero = $('#heroStatus');
  if (hero) {
    hero.className = 'hero-status st-' + s;
    $('.lbl', hero).textContent = label;
    const er = $('#heroErr');
    if (er) er.textContent = ['error', 'connecting', 'invalid_key'].includes(s) ? signal.title : '';
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
  $('#sideVer').textContent = `v${S.state.version}`;
  $('#sideVer').title = ({ windows: 'Windows', darwin: 'macOS', linux: 'Linux' })[S.state.os] || S.state.os || '';
  if ($('#sideDevice')) $('#sideDevice').textContent = S.state.config?.deviceName || '';
  return S.state;
}

async function route() {
	S.routeRevision = (S.routeRevision || 0) + 1;
	if (typeof accountDailyDispose === 'function') accountDailyDispose();
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
  const today = (u.usage && u.usage.today) || u.today || {};
  const total = (u.usage && u.usage.total) || u.total || {};
  const remaining = num(u.remaining) ?? num(u.balance) ?? num(u.quota && u.quota.remaining);
  if (remaining !== null) {
    cards.push(`<div class="card stat"><div class="stat-label">${ico('wallet')}${esc(u.planName || '剩余额度')}</div>
      <div class="stat-value">${money(remaining, u.quota && num(u.remaining) === null && num(u.balance) === null ? u.quota.unit || unit : unit)}</div>
      <div class="stat-foot">${u.mode === 'quota_limited' ? 'Key 限额模式' : '账户可用'}</div></div>`);
  }
  for (const [activity, label, symbol] of [[today, '今日', 'bolt'], [total, '累计', 'clock']]) {
    const tokenCount = num(activity.total_tokens) ?? num(activity.totalTokens) ?? num(activity.tokens);
    if (tokenCount !== null || num(activity.requests) !== null) {
      cards.push(`<div class="card stat"><div class="stat-label">${ico(symbol)}${label} Token</div>
        <div class="stat-value">${compact(tokenCount)}</div>
        <div class="stat-foot">${compact(activity.requests)} 次请求</div></div>`);
    }
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
  const revision = S.routeRevision;
  const localRevision = S.localAccountsRevision = (S.localAccountsRevision || 0) + 1;
  // Keep the page's primary navigation usable while the first local reads are
  // still in flight. These links do not depend on account or Hub state.
  $('#headActions').innerHTML = `<a class="btn" href="#sessions">${icon('chat')}会话恢复</a><a class="btn primary" href="#accounts">${icon('key')}API 密钥库</a>`;
  view.innerHTML = `<div class="callout">${ico('info')}<div>正在读取本机工具和 API 配置…<br><span class="small muted">本地使用无需登录远程服务。</span></div></div>
    <div class="card"><div class="row"><div class="folder-ico">${icon('chat')}</div><div class="li-main"><div class="li-title">会话恢复</div><div class="li-sub">读取原有 Codex / Claude Code 本地记录</div></div><a class="btn" href="#sessions">打开会话</a></div></div>
    <div class="card"><div class="skeleton" style="width:38%"></div><div class="skeleton" style="width:74%;margin-top:12px"></div></div>`;
  const [st, local] = await Promise.all([loadState(), api('/api/local/accounts')]);
  if (revision !== S.routeRevision || localRevision !== S.localAccountsRevision || view.isConnected === false || S.route !== 'overview') return;
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

function agentLocalStatus(tool) {
  const binding = S.local?.bindings?.[tool.id] || {};
  if (!tool.available) return { state: 'unsupported', label: '工具未安装', detail: '先安装此工具或填写程序路径' };
  if (binding.agent) return binding.agent;
  // Older payloads contain only a remote lamp. Never reuse it as local status.
  if (!binding.accountId) return { state: 'pending', label: '待选择 API', detail: '选择 API 后可打开此 Agent' };
  if (binding.pending || !binding.appliedId) return { state: 'pending', label: 'API 待应用', detail: '打开 Agent 时应用所选 API；已有聊天保留' };
  return { state: 'ready', label: 'API 已应用', detail: '本机配置已应用；进程和任务状态请在原工具中确认' };
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
  const codex = t.kind === 'codex';
  const desktop = t.id.endsWith('-desktop');
  const autoApply = t.id !== 'claude-desktop';
  const protocol = binding.protocol || (codex ? 'responses' : 'anthropic');
  const adapted = protocol !== (codex ? 'responses' : 'anthropic');
  return `<article class="card agent-card" data-tool="${esc(t.id)}" data-available="${t.available}"><div class="tool-head"><span class="tool-logo ${codex ? 'codex' : desktop ? 'desktop' : 'claude'}">${codex ? 'Cx' : desktop ? 'CD' : 'CC'}</span><div class="li-main"><h3 class="tool-name">${esc(t.name)}</h3><div class="muted small">${desktop ? '桌面应用' : '命令行工具'} · 模型不限制品牌</div></div><span class="chip ${t.available ? 'ok' : ''}">${t.available ? '已安装' : '未检测到'}</span></div>
    <label class="agent-api-label" for="toolAPI-${esc(t.id)}">使用的 API</label><div class="agent-api-row"><select class="input" id="toolAPI-${esc(t.id)}" data-target="${esc(t.id)}" aria-label="${esc(t.name)}使用的 API"><option value="">${accounts.length ? '请选择 API…' : '先在密钥库添加 API'}</option>${accounts.map((x) => `<option value="${esc(x.id)}" ${x.id === binding.accountId ? 'selected' : ''}>${esc(x.name)}</option>`).join('')}</select><div id="lamp-${esc(t.id)}">${remoteLampHTML(agentLocalStatus(t), 'agent')}</div></div>
    <div class="agent-config" id="agentConfig-${esc(t.id)}">${a ? `<div title="${esc(a.baseUrl)}">${ico('globe')}<span>${esc(a.baseUrl)}</span></div><div>${ico('key')}<code>${esc(a.keyMasked)}</code></div>` : '<div class="muted">选择已命名的密钥；模型在原工具内选择。</div>'}</div>
    <div class="agent-protocol"><label for="toolProtocol-${esc(t.id)}">服务商提供的上游接口</label><select class="input" id="toolProtocol-${esc(t.id)}" data-target="${esc(t.id)}" aria-label="${esc(t.name)}上游接口">${[['responses', 'OpenAI Responses（兼容中转站）'], ['chat', 'OpenAI Chat Completions（Grok / DeepSeek 等）'], ['anthropic', 'Anthropic Messages（Claude 等）']].map(([v,l]) => `<option value="${v}" ${protocol === v ? 'selected' : ''}>${l}</option>`).join('')}</select><span class="help">${!autoApply ? '这里的选择仅保存在 Bridge，须在原桌面应用中手动设置；不会自动应用。' : adapted ? '经 CLIProxyAPI 本地转换；无需远程登录，但 Bridge 必须保持运行。' : '原生接口直接连接服务商，不按模型品牌过滤。'}</span></div>
    <div class="agent-applied" id="agentApplied-${esc(t.id)}">${!autoApply ? '<span class="chip warn">自动应用暂不可用</span>' : binding.pending ? `<span class="chip warn">${a ? '选择已保存，等待应用' : '未选择 API'}</span>${applied ? `<span class="muted small">当前应用：${esc(applied.name)}</span>` : ''}` : `<span class="chip ok">已应用 ${esc(applied?.name || '')}</span>`}</div>
    <div class="agent-actions"><button class="btn primary" data-act="toolRestart" data-target="${esc(t.id)}" ${autoApply && a && binding.model && t.available ? '' : 'disabled'}>${icon('refresh')}${autoApply ? '应用并重启' : '自动应用暂不可用'}</button>${a ? `<button class="icon-btn" data-act="localEdit" data-id="${esc(a.id)}" title="编辑这个密钥" aria-label="编辑这个密钥">${icon('edit')}</button>` : '<button class="btn sm" data-act="localNew">添加密钥</button>'}</div>
    <p class="agent-note muted small" id="remoteDetail-${esc(t.id)}">${esc(agentLocalStatus(t).detail || '')}</p>
    ${!autoApply ? `<p class="agent-note small">${esc(claudeDesktopApplyNote())}</p><p class="agent-note small">CLI 恢复会话仅支持本地 Code 记录，使用 Claude Code 卡片已应用的 API 并通过 Claude Code 终端打开；普通云端聊天仍在原桌面应用里查看，不能通过 Bridge 遥控。</p>` : desktop ? '<p class="agent-note muted small">重启原桌面应用保留本地历史；指定旧会话的恢复通过原 CLI 打开。</p>' : '<p class="agent-note muted small">打开原终端会话列表；不会关闭其他终端，请先手动结束旧任务。</p>'}
  </article>`;
}

function renderAgentCards() {
  const box = $('#agentToolList');
  if (box && S.local) {
    box.innerHTML = S.local.tools.map(agentCard).join('');
  }
}

async function ensureLocalAccounts(button) {
  if (S.local) return true;
  const route = S.route, routeRevision = S.routeRevision, anchor = $('#modalRoot')?.innerHTML;
  const revision = S.localAccountsRevision = (S.localAccountsRevision || 0) + 1;
  const local = await api('/api/local/accounts');
  if (revision !== S.localAccountsRevision || S.route !== route || S.routeRevision !== routeRevision || button?.isConnected === false || $('#modalRoot')?.innerHTML !== anchor) return false;
  S.local = local;
  return true;
}

async function refreshWorkbenchStatus() {
  if (S.route !== 'overview' || S.bindingLoading || !S.local) return;
  // Foreground verification owns the account snapshot until its dialog closes.
  // A status push must not invalidate that read or compete with a pending launch.
  if (typeof WB !== 'undefined' && (WB.launchPending || typeof wbAccountCurrent === 'function' && wbAccountCurrent(WB.accountOpen))) return;
  const routeRevision = S.routeRevision, revision = S.localAccountsRevision = (S.localAccountsRevision || 0) + 1;
  S.bindingLoading = true;
  try {
    const local = await api('/api/local/accounts');
    if (S.route !== 'overview' || S.routeRevision !== routeRevision || revision !== S.localAccountsRevision) return;
    S.local = local;
    for (const t of local.tools) {
      const b = local.bindings?.[t.id];
      const lamp = document.getElementById('lamp-' + t.id);
      const detail = document.getElementById('remoteDetail-' + t.id);
      if (lamp) {
        if (typeof wbRefreshCardStatus === 'function') wbRefreshCardStatus(t.id);
        else toolDraftStatus(t.id);
      }
      if (detail) detail.textContent = agentLocalStatus(t).detail || '';
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

function accountLaunchToolIcon(target) {
	if (target === 'deepseek-harness' || target === 'hermes') return `<span class="account-tool-icon" aria-hidden="true"><img src="agent-brand/${target}-app-official.png" alt="" width="22" height="22"></span>`;
  const family = target === 'codex' || target === 'codex-desktop' ? 'codex' : target === 'claude' || target === 'claude-desktop' ? 'claude' : '';
  if (!family) return icon('external');
  const cli = target === 'codex' || target === 'claude';
  return `<span class="account-tool-icon" aria-hidden="true"><img src="agent-brand/${family}-app-official.png" alt="" width="22" height="22">${cli ? `<span class="account-tool-cli">${icon('terminal')}</span>` : ''}</span>`;
}

function accountLaunchButtonsHTML(a) {
  if (a.libraryOnly || a.officialAccount?.mode === 'chatgpt_web_session') return '';
  const official = !!a.officialAccount || a.sourceType === 'official_account';
  const targets = official
    ? [['codex-desktop', 'Codex'], ['codex', 'Codex CLI']]
    : [['codex-desktop', 'Codex'], ['claude-desktop', 'Claude Desktop'], ['codex', 'Codex CLI'], ['claude', 'Claude Code'], ['deepseek-harness', 'DeepSeek Harness'], ['hermes', 'Hermes Agent']];
  return `<div class="account-launch-buttons" aria-label="使用此账号打开工具">${targets.map(([target, label]) => `<button class="btn sm account-open-button" data-act="accountOpen" data-id="${esc(a.id)}" data-target="${target}" title="${esc('使用 ' + a.name + ' 打开 ' + label)}" aria-label="${esc('使用 ' + a.name + ' 打开 ' + label)}">${accountLaunchToolIcon(target)}</button>`).join('')}</div>`;
}

// A provider card has its own identity and read budget. Never feed it the
// dashboard's cross-source summary or infer provider usage from CLI traffic.
const API_CARD_USAGE = { generation: 0, active: 0, queue: [], pending: new Map(), cache: new Map(), timers: new Set() };
function apiCardUsageDate(now) {
  const date = new Date(now);
  return `${date.getFullYear()}-${String(date.getMonth() + 1).padStart(2, '0')}-${String(date.getDate()).padStart(2, '0')}`;
}
function apiCardUsageEligible(account) {
  return !!account && typeof account.id === 'string' && !!account.id && !account.officialAccount && account.sourceType !== 'official_account' && !account.libraryOnly && typeof account.sourceRevision === 'string' && /^src-v1-[a-f0-9]{64}$/.test(account.sourceRevision);
}
function apiCardUsageMetrics(data) {
  const balance = data?.balance;
  const amount = balance && typeof balance.amount === 'number' && Number.isFinite(balance.amount) ? balance.amount : null;
  const currency = typeof balance?.currency === 'string' ? balance.currency.trim() : '';
  const validUnit = currency && currency.length <= 24 && !/[\x00-\x1f\x7f]/.test(currency);
  const number = (value) => typeof value === 'number' && Number.isSafeInteger(value) && value >= 0 ? value.toLocaleString('zh-CN') : '—';
  const money = amount !== null && validUnit ? (currency.toUpperCase() === 'USD' ? '$' : currency + ' ') + amount.toLocaleString('zh-CN', { minimumFractionDigits: 2, maximumFractionDigits: 6 }) : '—';
  const billing = data?.billing;
  const hasRate = billing?.source === 'sub2api.key_billing' && billing.scope === 'token' && typeof billing.effectiveMultiplier === 'number' && Number.isFinite(billing.effectiveMultiplier) && billing.effectiveMultiplier >= 0 && billing.effectiveMultiplier <= 100 && billing.freshUntil > Date.now();
  const group = data?.group;
  const hasGroup = group?.source === 'provider_key_usage' && Number.isSafeInteger(group.freshUntil) && group.freshUntil > Date.now() && typeof group.name === 'string' && group.name.trim() && [...group.name].length <= 120 && !/[\x00-\x1f\x7f-\x9f\u202a-\u202e\u2066-\u2069]/.test(group.name);
  const details = [hasGroup ? `分组 ${group.name}` : '', hasRate ? `当前倍率 ×${billing.effectiveMultiplier}${billing.peak ? ' · 分时计费' : ''}` : ''].filter(Boolean);
  return `<div title="供应商返回的总余额，不按 Key 拆分；同一账户的多把 Key 可显示相同余额"><span>供应商余额</span><b>${esc(money)}</b></div><div title="服务商返回，今日按服务商时区"><span>今日请求</span><b>${number(data?.today?.requests)}</b></div><div title="服务商返回，今日按服务商时区"><span>今日 Token</span><b>${number(data?.today?.tokens)}</b></div>${details.length ? `<small class="api-billing-note" title="倍率代表这把 Key 此刻的 Token 计费规则，不覆盖图片或模型专价">${esc(details.join(' · '))}</small>` : ''}`;
}
function apiCardUsageData(raw, job, now) {
  if (!raw || raw.id !== job.id || raw.sourceRevision !== job.revision || !Number.isSafeInteger(raw.observedAt) || !Number.isSafeInteger(raw.freshUntil) || raw.observedAt > now || raw.observedAt <= 0 || raw.freshUntil <= now || raw.freshUntil <= raw.observedAt) return null;
  const freshUntil = Math.min(raw.freshUntil, raw.observedAt + 60000);
  if (freshUntil <= now) return null;
  const today = raw.today?.date === apiCardUsageDate(now) ? raw.today : null;
  const billing = raw.billing?.freshUntil > now ? raw.billing : null;
  const group = raw.group?.freshUntil > now ? raw.group : null;
  return { balance: raw.balance, today, billing, group, date: apiCardUsageDate(now), freshUntil: Math.min(freshUntil, billing?.freshUntil || freshUntil, group?.freshUntil || freshUntil) };
}
function apiCardUsageCurrent(owner) {
  if (owner.generation !== API_CARD_USAGE.generation || S.route !== 'accounts' || owner.node.isConnected !== true || document.getElementById(owner.node.id) !== owner.node) return false;
  const current = S.local?.accounts?.find((row) => row.id === owner.id);
  return apiCardUsageEligible(current) && current.sourceRevision === owner.revision;
}
function apiCardUsageApply(owner, data) {
  if (!apiCardUsageCurrent(owner) || !data || data.freshUntil <= Date.now() || data.date !== apiCardUsageDate(Date.now())) return;
  owner.node.innerHTML = apiCardUsageMetrics(data);
  // Expiry only clears the display. It never starts a polling request.
  const now = Date.now(), midnight = new Date(now);
  midnight.setHours(24, 0, 0, 0);
  const timer = setTimeout(() => {
    API_CARD_USAGE.timers.delete(timer);
    if (apiCardUsageCurrent(owner)) owner.node.innerHTML = apiCardUsageMetrics(null);
  }, Math.max(0, Math.min(data.freshUntil, midnight.getTime()) - now));
  API_CARD_USAGE.timers.add(timer);
}
function apiCardUsagePump() {
  while (API_CARD_USAGE.active < 4 && API_CARD_USAGE.queue.length) {
    const job = API_CARD_USAGE.queue.shift();
    if (!job.owners.some(apiCardUsageCurrent)) { API_CARD_USAGE.pending.delete(job.key); continue; }
    API_CARD_USAGE.active++;
    Promise.resolve().then(() => api('/api/local/accounts/usage?id=' + encodeURIComponent(job.id))).then((raw) => {
      const data = apiCardUsageData(raw, job, Date.now());
      if (!data || !job.owners.some(apiCardUsageCurrent)) return;
      API_CARD_USAGE.cache.delete(job.key);
      API_CARD_USAGE.cache.set(job.key, data);
      while (API_CARD_USAGE.cache.size > 256) API_CARD_USAGE.cache.delete(API_CARD_USAGE.cache.keys().next().value);
      job.owners.forEach((owner) => apiCardUsageApply(owner, data));
    }).catch(() => { /* Missing provider usage is unknown, never a false zero. */ }).finally(() => {
      API_CARD_USAGE.pending.delete(job.key); API_CARD_USAGE.active--; apiCardUsagePump();
    });
  }
}
function loadAPICardUsage(accounts) {
  const generation = ++API_CARD_USAGE.generation;
  API_CARD_USAGE.timers.forEach(clearTimeout); API_CARD_USAGE.timers.clear();
  for (const account of accounts) {
    if (!apiCardUsageEligible(account)) continue;
    const node = document.getElementById('apiAccountUsage-' + account.id);
    if (!node) continue;
    const owner = { generation, node, id: account.id, revision: account.sourceRevision };
    const key = owner.id + '\x00' + owner.revision, cached = API_CARD_USAGE.cache.get(key);
    if (cached?.freshUntil > Date.now() && cached.date === apiCardUsageDate(Date.now())) { apiCardUsageApply(owner, cached); continue; }
    API_CARD_USAGE.cache.delete(key);
    const pending = API_CARD_USAGE.pending.get(key);
    if (pending) { pending.owners = [owner]; continue; }
    const job = { id: owner.id, revision: owner.revision, key, owners: [owner] };
    API_CARD_USAGE.pending.set(key, job); API_CARD_USAGE.queue.push(job);
  }
  apiCardUsagePump();
}

function localAccountCard(a) {
  const tools = Array.isArray(S.local?.tools) ? S.local.tools : [];
  const usedBy = tools.filter((t) => S.local?.bindings?.[t.id]?.accountId === a.id).map((t) => t.name);
  const more = typeof wbMore === 'function' ? wbMore : (body) => `<details class="wb-card-more"><summary>⋯</summary><div class="wb-more-body">${body}</div></details>`;
  if (a.officialAccount && typeof officialAccountCard === 'function') return officialAccountCard(a, usedBy, more);
  let endpoint = '地址未设置';
  try {
    const url = new URL(a.baseUrl);
    if (url.protocol === 'https:' || url.protocol === 'http:') {
      endpoint = url.origin + url.pathname;
    }
  } catch (_) { /* Never turn an unverified address into a navigation target. */ }
  const models = Number.isSafeInteger(a.modelCount) && a.modelCount >= 0 ? a.modelCount : Array.isArray(a.models) ? a.models.length : 0;
  const clients = [...new Map((Array.isArray(a.apiService?.clients) ? a.apiService.clients : []).filter((client) => client && typeof client.clientId === 'string' && client.clientId).map((client) => [client.clientId, client])).values()];
  const remoteStations = localRemoteStationRows();
  const boundRemoteStations = a.remoteHubBindingsSet === true
    ? remoteStations.filter(row => (a.remoteHubUrls || []).some(hub => localHubKey(hub) === localHubKey(row.hubUrl)))
    : remoteStations;
  const remoteBindingLabel = a.remoteHubBindingsSet === true
    ? boundRemoteStations.length ? boundRemoteStations.map(row => row.name || row.hubUrl).join('、') : '关闭'
    : remoteStations.length ? '所有已登记站点' : '未设置';
  // Active describes membership in an enabled client, not a running service,
  // a live agent process, or provider entitlement. Keep those claims separate.
  const serviceDetail = clients.map((client) => `${client.clientName || '客户端'} · ${[client.primary === true ? '主账号' : '', client.text === true ? '文字' : '', client.image === true ? '图片' : '', client.disabled === true ? '已停用' : ''].filter(Boolean).join(' / ') || '配置关联'}`).join('；');
  // Icon buttons: the label stays in the accessible name and a visually hidden span.
  const action = (name, label, symbol, danger = false) => `<button class="api-icon-button api-provider-action${danger ? ' danger-text' : ''}" data-act="${name}" data-id="${esc(a.id)}" aria-label="${esc(label + ' · ' + a.name)}" title="${esc(label)}">${icon(symbol)}<span class="sr-only">${label}</span></button>`;
  let host = '';
  try { host = new URL(endpoint).host; } catch (_) { /* unset address */ }
  const initial = [...String(a.name || '?').trim()][0] || '?';
  const protocolLabel = { auto: '自动识别', responses: 'Responses', chat: 'Chat Completions', anthropic: 'Anthropic' }[a.wire || 'auto'] || '自动识别';
  return `<article class="wb-card account-card api-provider-card key-card" data-account-id="${esc(a.id)}" data-face="${keyCardFace(a.id)}">
    <div class="key-face">
      <span class="key-glow" aria-hidden="true"></span>
      <header class="api-provider-top">
        <span class="api-provider-avatar" aria-hidden="true">${esc(initial.toUpperCase())}</span>
        <span class="api-provider-ident"><span class="api-provider-name" title="${esc(a.name)}">${esc(a.name)}</span><small class="api-provider-host">${esc(host || endpoint)}</small></span>
        ${typeof accountRemoteSupportHTML === 'function' ? accountRemoteSupportHTML(a) : ''}<span class="api-provider-badge">API</span>
      </header>
    </div>
    <div class="key-body">
      ${typeof accountDailyCardHTML === 'function' ? accountDailyCardHTML(a) : ''}
      <div class="api-provider-fields">
        <div class="api-provider-key"><span>API Key</span><code>${esc(a.keyMasked || '未保存')}</code>${icon('key')}</div>
        <div class="api-provider-address"><span>基础地址</span><code title="${esc(endpoint)}">${esc(endpoint)}</code></div>
        <div class="api-provider-remote"><span>手机远程</span><b title="${esc(remoteBindingLabel)}">${esc(remoteBindingLabel)}</b></div>
        <div class="api-provider-models"><span>已保存模型</span><b>${models}</b><button class="btn sm api-provider-service-action" data-act="apiCardMembership" data-id="${esc(a.id)}" title="${esc(serviceDetail || '加入本地 API 服务')}">${clients.length ? '移出 API 服务' : '加入 API 服务'}</button><button class="btn sm ghost" data-act="accountModelRules" data-id="${esc(a.id)}" aria-label="${esc('模型规则 · ' + a.name)}" title="模型规则">规则</button></div>
      </div>
      <div class="api-provider-stats" id="apiAccountUsage-${esc(a.id)}">${apiCardUsageMetrics(null)}</div>
      <div class="api-provider-preferences"><span class="api-protocol-label" title="上游接口">${esc(protocolLabel)}</span>${typeof accountSpeedControlHTML === 'function' ? accountSpeedControlHTML(a) : ''}</div>
      <footer class="api-provider-foot">
        ${accountLaunchButtonsHTML(a)}
        <div class="api-provider-actions">${action('accountTest', '发送测试请求', 'play')}${action('localEdit', '编辑', 'edit')}${action('localModels', '读取模型', 'refresh')}${action('localDelete', '移除', 'trash', true)}</div>
      </footer>
    </div>
  </article>`;
}

// Each key keeps one stable card colour, so the same key looks the same on every page.
function keyCardFace(id) {
  let hash = 0;
  for (const ch of String(id || '')) hash = (hash * 31 + ch.charCodeAt(0)) >>> 0;
  return ['ink', 'violet', 'blue', 'pink', 'iris'][hash % 5];
}

function renderLocalCards() {
  const box = $('#localAccountList');
  if (!box || !S.local) return;
  const q = S.localFilter.toLowerCase().trim();
  const library = typeof officialLibraryAccounts === 'function' ? officialLibraryAccounts(S.local.accounts) : S.local.accounts;
  const accounts = typeof accountDailyFilter === 'function' ? accountDailyFilter(library) : library.filter((a) => !q || [a.name, a.baseUrl].some((v) => String(v || '').toLowerCase().includes(q)));
  if (typeof accountDailyPrepare === 'function') accountDailyPrepare(library, accounts);
  box.innerHTML = (typeof codexAPIOverviewHTML === 'function' ? codexAPIOverviewHTML() : '') + (accounts.length ? accounts.map(localAccountCard).join('') + `<button class="key-add" type="button" data-act="localNew"><span>${icon('plus')}</span>添加一把新密钥</button>` : `<div class="acct-empty"><b>${library.length ? '没有匹配的账号' : '还没有 API 或账号'}</b><span>点击右上角“添加”，保存 API Key 或导入 Codex 账号。</span></div>`);
  loadAPICardUsage(accounts);
  if (typeof accountDailySync === 'function') accountDailySync();
  if (typeof scheduleOfficialCostExpiry === 'function') scheduleOfficialCostExpiry(accounts);
  if (typeof syncRemoteSupport === 'function') syncRemoteSupport();
  const count = $('#localCount');
  if (count) count.textContent = `${accounts.length} / ${library.length} 个密钥`;
  const toolbar = $('#localToolbar');
  if (toolbar) toolbar.hidden = library.length <= 5 && !q;
}

function renderAccountsPage(view, local) {
  S.officialCleanupKnown = false;
  $('#headActions').innerHTML = `<div class="account-daily-toolbar account-daily-head" id="accountDailyToolbar"></div><button class="btn primary" data-act="localNew">${icon('plus')}添加密钥</button>`;
  if (typeof officialAccountActions === 'function') officialAccountActions($('#headActions'));
  view.innerHTML = `<div class="acct-bar" id="localToolbar" ${local.accounts.length <= 5 && !S.localFilter ? 'hidden' : ''}><div class="local-toolbar"><label class="search">${icon('search')}<input class="input" id="localSearch" type="search" aria-label="搜索 API" placeholder="搜索名称或地址" value="${esc(S.localFilter)}"></label><span class="muted small" id="localCount"></span></div></div>
    <div class="row account-daily-selection" id="accountDailySelection" role="group" aria-label="批量操作" hidden></div>
    <div class="wb-grid" id="localAccountList"></div>
    <p class="su-foot">${icon('lock')} 密钥保存在本机，调用时仅用于对应服务商认证。</p>`;
  renderLocalCards();
  if (typeof loadOfficialLibrary === 'function') void loadOfficialLibrary();
  if (typeof loadCodexAPIOverview === 'function') void loadCodexAPIOverview();
}

RENDER_accounts = function (view) {
  const target = view;
  const cached = S.local;
  const revisionAtStart = S.routeRevision;
  // The add-key entry is independent of the account-list request and should
  // remain available on a cold start or when the local API is slow.
  $('#headActions').innerHTML = `<div class="account-daily-toolbar account-daily-head" id="accountDailyToolbar"></div><button class="btn primary" data-act="localNew">${icon('plus')}添加密钥</button>`;
  if (cached) renderAccountsPage(target, cached);
  else {
    target.innerHTML = `<div class="card"><div class="skeleton" style="width:34%"></div><div class="skeleton" style="width:68%;margin-top:12px"></div><p class="muted small">正在读取本机 API 和账号…</p></div><p class="su-foot">${icon('lock')} 密钥保存在本机，调用时仅用于对应服务商认证。</p>`;
    if (typeof accountDailySync === 'function') accountDailySync();
  }
  const revision = S.localAccountsRevision = (S.localAccountsRevision || 0) + 1;
  void loadState().catch(() => undefined);
  void api('/api/local/accounts').then((local) => {
    if (!local || revision !== S.localAccountsRevision || revisionAtStart !== S.routeRevision || target.isConnected === false || S.route !== 'accounts') return;
    S.local = local;
    renderAccountsPage(target, local);
  }).catch((e) => {
    if (cached || revision !== S.localAccountsRevision || revisionAtStart !== S.routeRevision || target.isConnected === false || S.route !== 'accounts') return;
    target.innerHTML = `<div class="callout bad">${ico('warn')}<div>${esc(e.message)}</div></div>`;
  });
};

function localModelCatalogHTML(models) {
  if (!Array.isArray(models) || !models.length) return '<span class="key-model-catalog-empty">尚未同步模型</span>';
  return models.map((model) => `<code role="listitem">${esc(model)}</code>`).join('');
}

function localDraftModelList(value) {
  return [...new Set(String(value || '').split(/\r?\n/).map(model => model.trim()).filter(Boolean))];
}

function localEditorProtocolSync() {
  const select = $('#laWire'), root = $('#modalRoot');
  if (!select || !root?.querySelectorAll) return;
  for (const button of root.querySelectorAll('[data-editor-wire]')) {
    const on = button.dataset.editorWire === select.value;
    button.classList.toggle('on', on);
    button.setAttribute('aria-pressed', String(on));
  }
  const hint = $('#laWireHint');
  if (hint) hint.textContent = {
    auto: '根据服务地址和模型自动选择；已知接口时可以直接指定。',
    responses: '使用 OpenAI Responses 接口，适合原生 Codex 服务。',
    chat: '使用 Chat Completions 接口，适合 DeepSeek、OpenRouter 等服务。',
    anthropic: '使用 Anthropic Messages 接口，适合 Claude 兼容服务。',
  }[select.value] || '';
}

function localEditorDraftSync() {
  const input = $('#laID'), draft = S.localEditorDraft;
  if (!input || !draft || draft.input !== input) return;
  const count = $('#laModelCount');
  if (count) count.textContent = `${localDraftModelList($('#laModels')?.value).length} 个模型`;
  const note = $('#laDraftNote');
  if (note) note.textContent = JSON.stringify(localForm()) === draft.initial ? '所有更改在保存后生效' : '有未保存的更改';
}

async function closeLocalEditor(options = {}) {
  if (typeof apiManagementActive === 'function' && apiManagementActive()) return closeAPIManagement();
  const draft = S.localEditorDraft, input = $('#laID');
  if (!draft || draft.input !== input) { $('#modalRoot').innerHTML = ''; S.localEditorDraft = null; return true; }
  // A route change may join an already pending discard confirmation. It must
  // suppress the service return callback for that shared close operation too.
  if (options.returnToService === false) draft.leavePage = true;
  if (draft.closePromise) return draft.closePromise;
  draft.closing = true;
  draft.closePromise = (async () => {
    try {
      if (draft.saving) {
        if (!await confirm('关闭正在保存的 API 配置？保存已经提交，关闭不会撤销本次保存。')) return false;
      } else if (JSON.stringify(localForm()) !== draft.initial && !await confirm('放弃未保存的 API 配置？本次修改不会保存。')) return false;
      if (S.localEditorDraft !== draft || $('#laID') !== input) return false;
      $('#modalRoot').innerHTML = '';
      S.localEditorDraft = null;
      if (draft.onReturn && !draft.leavePage) await draft.onReturn();
      return true;
    } finally { draft.closing = false; draft.closePromise = null; draft.leavePage = false; }
  })();
  return draft.closePromise;
}

function localValidateForm(body) {
  if (!body.id && !body.key.trim()) throw new Error('请填写 API Key');
  let url;
  const rawURL = body.baseUrl.trim();
  try { if (!rawURL) throw new Error(); url = new URL(rawURL.includes('://') ? rawURL : 'https://' + rawURL); } catch (_) { throw new Error('请填写完整的服务地址，例如 https://api.example.com/v1'); }
  if (!['https:', 'http:'].includes(url.protocol) || url.username || url.password || url.search || url.hash) throw new Error('服务地址应为 HTTP 或 HTTPS 地址，不包含密码、查询参数或片段');
  const limits = [['laPriority', '优先级', -1000, 1000], ['laWeight', '权重', 0, 1000], ['laMaxConcurrency', '最大并发', 0, 10000], ['laMaxRpm', '每分钟请求上限', 0, Infinity], ['laMaxTpm', '每分钟 Token 上限', 0, Infinity]];
  for (const [id, label, min, max] of limits) {
    const raw = $('#' + id)?.value?.trim();
    if (raw && (!/^-?\d+$/.test(raw) || !Number.isSafeInteger(Number(raw)) || Number(raw) < min || Number(raw) > max)) throw new Error(`${label}需填写 ${min}${Number.isFinite(max) ? '–' + max : ' 或更大'}的整数`);
  }
  for (const line of ($('#laExtraHeaders')?.value || '').split(/\r?\n/)) {
    if (line.trim() && (!/^[!#$%&'*+.^_`|~0-9A-Za-z-]+\s*:\s*\S.*$/.test(line.trim()) || /^(authorization|host)\s*:/i.test(line.trim()))) throw new Error('请求头请按“名称: 值”填写，不能覆盖 Authorization 或 Host');
  }
}

function localEditor(a, kind, options = {}) {
  if (typeof disposeOfficialPanel === 'function') disposeOfficialPanel();
  S.localDraftModels = [...(a?.models || [])];
  S.localDraftTransport = a ? { baseUrl: a.baseUrl, authMode: a.authMode, wire: a.wire } : null;
  S.localDraftModelsManual = !!a?.modelsManual;
  S.localDraftSpeed = a?.speedMode || 'standard';
  S.localDraftSourceRevision = a?.sourceRevision || '';
  // Extra headers: stored as "Key: Value\nKey: Value" for editing
  S.localDraftHeaders = a?.extraHeaders ? Object.entries(a.extraHeaders).map(([k, v]) => `${k}: ${v}`).join('\n') : '';
  const presets = [['OpenAI', 'https://api.openai.com/v1'], ['Anthropic', 'https://api.anthropic.com'], ['DeepSeek', 'https://api.deepseek.com/v1'], ['OpenRouter', 'https://openrouter.ai/api/v1']];
  const base = a?.baseUrl || 'https://salcara.top';
  const face = a && typeof keyCardFace === 'function' ? keyCardFace(a.id) : 'iris';
  const num = (v) => esc(String(v ?? ''));
  const remoteStations = localRemoteStationRows();
  const selectedRemoteHubs = a?.remoteHubBindingsSet === true
    ? new Set((a.remoteHubUrls || []).map(localHubKey))
    : a ? new Set(remoteStations.map(row => row.hubUrl))
      : new Set(remoteStations.filter(row => localHubKey(row.hubUrl) === localHubKey(S.state?.config?.effectiveHubUrl)).map(row => row.hubUrl));
  const remoteBindingLegacy = !!a && a.remoteHubBindingsSet !== true;
  const remoteBindingEnabled = remoteStations.length > 0 && (a ? a.remoteHubBindingsSet === true ? selectedRemoteHubs.size > 0 : true : selectedRemoteHubs.size > 0);
  const remoteBindingSummary = remoteStations.length
    ? remoteBindingEnabled ? '使用已绑定站点，无需重复绑定手机' : '当前关闭，可直接在这里开启'
    : S.state?.config?.remoteModes?.direct?.paired ? '已完成直连绑定，无需登记中转站' : '中转站或 Bot 只需首次绑定';
  const modelCount = S.localDraftModels.length;
  const modelSyncSummary = modelCount ? '可直接编辑模型 ID，每行一个' : '从上游获取，或手动填写模型 ID';
  const remoteHubOptions = `<section class="remote-hub-bindings" aria-label="手机远程 API 权限" data-legacy="${remoteBindingLegacy}">
    <label class="remote-binding-toggle" for="laRemoteEnabled"><input id="laRemoteEnabled" type="checkbox" ${remoteBindingEnabled ? 'checked' : ''} ${!remoteStations.length ? 'disabled' : ''}><span class="remote-binding-copy"><strong>允许中转站 / Bot 使用此 API</strong><small>${remoteBindingSummary}</small></span><span class="remote-binding-state" aria-hidden="true"></span></label>
    <div class="remote-hub-choices" id="laRemoteHubChoices" ${remoteBindingEnabled ? '' : 'hidden'}>
      ${remoteStations.length ? `<div class="remote-hub-caption">已绑定站点</div>${remoteStations.map(row => `<label class="remote-hub-choice"><input type="checkbox" data-remote-hub="${esc(row.hubUrl)}" ${selectedRemoteHubs.has(row.hubUrl) ? 'checked' : ''}><span>${esc(row.name || row.hubUrl)}</span>${localHubKey(row.hubUrl) === localHubKey(S.state?.config?.effectiveHubUrl) ? '<span class="chip">当前</span>' : ''}</label>`).join('')}<div class="help">保存后应用此 API 的远程权限，无需重新绑定。</div>` : ''}
    </div>
    ${remoteStations.length ? '' : `<div class="help remote-binding-empty">${S.state?.config?.remoteModes?.direct?.paired ? '直连手机可选择本机 API；此处的站点授权不影响直连。' : '使用中转站或 Bot 时，首次在「手机远程」绑定，之后在这里管理 API。电脑直连无需登记站点。'}</div>`}
  </section>`;
  $('#modalRoot').innerHTML = `<div class="modal-back" data-act="closeModal"><div class="modal account-modal key-editor account-add-modal" data-key-editor data-stop role="dialog" aria-modal="true" aria-labelledby="localEditorTitle">
    <div class="modal-head key-editor-head" data-face="${face}"><span class="key-glow" aria-hidden="true"></span><div><h3 id="localEditorTitle">${a ? '编辑 API Key' : '添加 API Key'}</h3><p>${a ? esc(a.name || '配置服务地址、接口与模型') : '连接服务商，配置可用模型'}</p></div><button class="icon-btn" data-act="closeModal" aria-label="关闭">${icon('close')}</button></div>
    <div class="modal-body">${!a && !options.service && typeof accountAddTabsHTML === 'function' ? accountAddTabsHTML('apikey') : ''}<input type="hidden" id="laID" value="${esc(a?.id || '')}"><input type="hidden" id="laModel" value="${esc(a?.model || '')}"><input type="hidden" id="laWorkspace" value="${esc(a?.workspace || '')}">
    <section class="api-editor-section"><div class="api-editor-section-head"><strong>连接信息</strong><small>${a ? '留空 Key 可保留原密钥' : '密钥保存在本机'}</small></div><div class="form-grid key-editor-grid">
      <div class="field full"><label for="laName">名称 <span class="muted small">可选</span></label><input class="input" id="laName" maxlength="80" placeholder="留空使用站点名称" value="${esc(a?.name || '')}"></div>
      <div class="field full"><label for="laKey">API Key</label><div class="input-wrap"><input class="input key-input" id="laKey" type="password" autocomplete="off" spellcheck="false" placeholder="${esc(a ? '已保存 ' + (a.keyMasked || '') + '，留空则不更换' : '粘贴你的 API Key')}"><button class="icon-btn" data-act="toggleVis" data-for="laKey" aria-label="显示或隐藏 Key">${icon('eye')}</button></div></div>
      <div class="field full"><label for="laBase">服务地址</label><input class="input mono" id="laBase" placeholder="https://api.example.com/v1" value="${esc(base)}"><div class="preset-row" role="group" aria-label="常用服务商">${presets.map(([name, url]) => `<button type="button" class="preset${url === base ? ' on' : ''}" data-act="localPreset" data-url="${esc(url)}">${name}</button>`).join('')}</div></div>
    </div></section>
    <section class="api-editor-section" aria-label="上游接口"><div class="api-editor-section-head"><strong>接口协议</strong></div>
      <select id="laWire" data-native hidden aria-label="上游接口">${[['auto', '自动识别'], ['responses', 'Responses'], ['chat', 'Chat Completions'], ['anthropic', 'Anthropic Messages']].map(([value, label]) => `<option value="${value}" ${(a?.wire || 'auto') === value ? 'selected' : ''}>${label}</option>`).join('')}</select>
      <div class="api-editor-protocols" role="group" aria-label="选择上游接口">${[['auto', '自动识别'], ['responses', 'Responses'], ['chat', 'Chat Completions'], ['anthropic', 'Anthropic Messages']].map(([value, label]) => `<button type="button" class="api-editor-protocol${(a?.wire || 'auto') === value ? ' on' : ''}" data-act="localWire" data-editor-wire="${value}" aria-pressed="${(a?.wire || 'auto') === value}">${label}</button>`).join('')}</div>
      <p class="help" id="laWireHint"></p>
      <div class="api-editor-speed"><label for="laSpeed">默认速度</label><select class="input" id="laSpeed" aria-label="默认速度">${[['standard', '标准'], ['fast', '快速'], ['ultrafast', '超高速']].map(([value, label]) => `<option value="${value}" ${S.localDraftSpeed === value ? 'selected' : ''}>${label}</option>`).join('')}</select></div><p class="help">快速档位适用于 Responses / Chat，需服务商与模型支持，可能增加费用。</p>
    </section>
    <section class="api-editor-section api-editor-models" aria-label="模型列表">
      <div class="api-editor-section-head"><div><strong>模型列表</strong><small id="laModelSyncState" role="status">${modelSyncSummary}</small></div><span class="chip" id="laModelCount">${modelCount} 个模型</span></div>
      <label class="sr-only" for="laModels">模型 ID，每行一个</label><textarea class="input mono" id="laModels" rows="5" maxlength="200000" spellcheck="false" placeholder="填写服务商支持的模型 ID，每行一个">${esc(S.localDraftModels.join('\n'))}</textarea>
      <div class="api-editor-model-tools"><span class="help">获取结果只填入当前草稿，保存前可自由调整。</span><button type="button" class="btn sm" data-act="localModels" aria-label="从上游获取模型">${icon('refresh')}从上游获取</button></div>
      <p class="help api-editor-catalog-help">模型保存后供 Agent 选择；同步到 Codex 原生模型列表需在 Agent 中确认配置。</p>
      <div id="localProbeResult" role="status" aria-live="polite"></div>
    </section>
    ${remoteHubOptions}
    <details class="adv key-editor-more api-editor-advanced">
      <summary><span class="chev">${icon('chev')}</span>高级设置<small>认证、限速与路由规则</small></summary>
      <div class="form-grid" style="margin-top:12px">
        <div class="field full"><label for="laAuth">认证方式</label><select class="input" id="laAuth" aria-label="认证方式">${[['auto', '自动识别'], ['bearer', 'Bearer Token'], ['api-key', 'x-api-key']].map(([value, label]) => `<option value="${value}" ${(a?.authMode || 'auto') === value ? 'selected' : ''}>${label}</option>`).join('')}</select></div>
        <div class="field"><label for="laFallbackModel">回退模型</label><input class="input" id="laFallbackModel" placeholder="不设置" value="${esc(a?.fallbackModel || '')}"></div>
        <div class="field"><label for="laPriority">优先级</label><input class="input" id="laPriority" type="number" min="-1000" max="1000" placeholder="0" value="${num(a?.priority)}"></div>
        <div class="field"><label for="laMaxConcurrency">最大并发</label><input class="input" id="laMaxConcurrency" type="number" min="0" max="10000" placeholder="不限" value="${num(a?.maxConcurrency)}"></div>
        <div class="field"><label for="laWeight">权重</label><input class="input" id="laWeight" type="number" min="0" max="1000" placeholder="1" value="${num(a?.weight)}"></div>
        <div class="field"><label for="laMaxRpm">每分钟请求上限</label><input class="input" id="laMaxRpm" type="number" min="0" placeholder="不限" value="${num(a?.maxRpm)}"></div>
        <div class="field"><label for="laMaxTpm">每分钟 Token 上限</label><input class="input" id="laMaxTpm" type="number" min="0" placeholder="不限" value="${num(a?.maxTpm)}"></div>
        <div class="field full"><label for="laSystemPrompt">系统提示词前缀</label><textarea class="input" id="laSystemPrompt" rows="2" maxlength="8000" placeholder="不注入">${esc(a?.systemPromptPrefix || '')}</textarea></div>
        <div class="field full"><label for="laModelAllowlist">只允许这些模型（每行一个）</label><textarea class="input mono" id="laModelAllowlist" rows="2" maxlength="10000" placeholder="留空表示不限">${esc((a?.modelAllowlist || []).join('\n'))}</textarea></div>
        <div class="field full"><label for="laModelDenylist">禁用这些模型（每行一个）</label><textarea class="input mono" id="laModelDenylist" rows="2" maxlength="10000">${esc((a?.modelDenylist || []).join('\n'))}</textarea></div>
        <div class="field full"><label for="laExtraHeaders">自定义请求头（每行 Key: Value）</label><textarea class="input mono" id="laExtraHeaders" rows="2" maxlength="10000" placeholder="X-Custom-Org: my-org">${esc(S.localDraftHeaders)}</textarea></div>
      </div>
    </details><div id="localSaveError" class="api-editor-error" role="alert" aria-live="polite"></div></div>
    <div class="modal-foot"><span class="api-editor-draft-note" id="laDraftNote">所有更改在保存后生效</span><span class="spacer"></span><button class="btn ghost" data-act="closeModal">取消</button><button class="btn primary" data-act="localSave">${a ? '保存更改' : '保存密钥'}</button></div></div></div>`;
  S.localEditorDraft = { input: $('#laID'), initial: JSON.stringify(localForm()), saving: false, closing: false, onReturn: options.onReturn };
  localEditorProtocolSync();
  (a ? $('#laName') : $('#laKey')).focus();
}

function localTransportForDraft(baseUrl) {
  const normalize = value => {
    try { const url = new URL(value.trim()); return url.origin + url.pathname.replace(/\/v1\/?$/, '').replace(/\/+$/, '') + url.search + url.hash; }
    catch (_) { return ''; }
  };
  const prior = S.localDraftTransport;
  const sameEndpoint = prior && normalize(baseUrl) && normalize(prior.baseUrl || '') === normalize(baseUrl);
  // A model name does not define the provider's protocol. In particular,
  // OpenRouter serves GPT/Claude names through Chat Completions.
  let authMode = 'bearer', wire = 'auto';
  try {
    const url = new URL(baseUrl.trim().includes('://') ? baseUrl.trim() : 'https://' + baseUrl.trim());
    if (url.hostname === 'api.anthropic.com') { authMode = 'api-key'; wire = 'anthropic'; }
    else if (url.hostname === 'api.openai.com') wire = 'responses';
    else if (url.hostname === 'openrouter.ai') wire = 'chat';
    else if (url.hostname === 'api.deepseek.com') wire = /^\/anthropic(?:\/|$)/.test(url.pathname) ? 'anthropic' : 'chat';
  } catch (_) {}
  const authChoice = $('#laAuth')?.value || (sameEndpoint ? prior.authMode : 'auto');
  const wireChoice = $('#laWire')?.value || (sameEndpoint ? prior.wire : 'auto');
  if (authChoice === 'bearer' || authChoice === 'api-key') authMode = authChoice;
  if (['responses', 'chat', 'anthropic'].includes(wireChoice)) wire = wireChoice;
  return { authMode, wire };
}
function localHubKey(raw) {
  return String(raw || '').trim().replace(/\/+$/, '').replace(/\/v1$/i, '').replace(/\/+$/, '');
}
function localRemoteStationRows() {
  const rows = new Map();
  for (const row of [...(S.local?.remoteStations || []), ...(S.state?.config?.remoteConnections || [])]) {
    if (!row || typeof row.hubUrl !== 'string' || !localHubKey(row.hubUrl)) continue;
    const key = localHubKey(row.hubUrl);
    if (!rows.has(key)) rows.set(key, { ...row, hubUrl: key });
  }
  return [...rows.values()];
}
function localForm() {
  const modelsText = $('#laModels')?.value;
  const models = modelsText == null ? S.localDraftModels : [...new Set(modelsText.split(/\r?\n/).map(model => model.trim()).filter(Boolean))];
  const modelsManual = !!S.localDraftModelsManual || models.join('\n') !== S.localDraftModels.join('\n');
  const baseUrl = $('#laBase').value;

  // Model allow/deny lists
  const modelAllowlist = ($('#laModelAllowlist')?.value || '').split(/\r?\n/).map(m => m.trim()).filter(Boolean);
  const modelDenylist  = ($('#laModelDenylist')?.value  || '').split(/\r?\n/).map(m => m.trim()).filter(Boolean);

  // Extra headers: parse "Key: Value" lines into an object
  const extraHeaders = {};
  ($('#laExtraHeaders')?.value || '').split(/\r?\n/).forEach(line => {
    const idx = line.indexOf(':');
    if (idx > 0) {
      const k = line.slice(0, idx).trim();
      const v = line.slice(idx + 1).trim();
      if (k && v) extraHeaders[k] = v;
    }
  });

  // Empty/zero is an explicit reset. Omission means preserve the saved value
  // for older clients, so serializing undefined would make clearing impossible.
  const priority       = parseInt($('#laPriority')?.value   || '0', 10) || 0;
  const weight         = parseInt($('#laWeight')?.value     || '0', 10) || 0;
  const maxConcurrency = parseInt($('#laMaxConcurrency')?.value || '0', 10) || 0;
  const maxRpm         = parseInt($('#laMaxRpm')?.value     || '0', 10) || 0;
  const maxTpm         = parseInt($('#laMaxTpm')?.value     || '0', 10) || 0;

  const systemPromptPrefix = ($('#laSystemPrompt')?.value || '').trim();
  const fallbackModel      = ($('#laFallbackModel')?.value || '').trim();

  const bindingRoot = $('#laRemoteEnabled')?.closest?.('.remote-hub-bindings');
  const remoteBindingEnabled = $('#laRemoteEnabled')?.checked ?? true;
  const remoteBindingTouched = bindingRoot?.dataset.touched === 'true';
  const remoteBindings = bindingRoot?.dataset.legacy === 'true' && !remoteBindingTouched ? {} : {
    remoteHubUrls: remoteBindingEnabled && typeof document !== 'undefined' ? [...document.querySelectorAll('[data-remote-hub]:checked')].map(input => input.dataset.remoteHub) : [],
    remoteHubBindingsSet: true,
  };
  return {
    id: $('#laID').value,
    ...(S.localDraftSourceRevision ? { sourceRevision: S.localDraftSourceRevision } : {}),
    name: $('#laName')?.value || '',
    kind: 'api',
    baseUrl,
    key: $('#laKey').value,
    model: $('#laModel').value,
    ...localTransportForDraft(baseUrl),
    speedMode: $('#laSpeed')?.value || S.localDraftSpeed || 'standard',
    workspace: $('#laWorkspace').value,
    models,
    modelsManual,
    ...remoteBindings,
    modelAllowlist,
    modelDenylist,
    extraHeaders,
    systemPromptPrefix,
    fallbackModel,
    priority,
    weight,
    maxConcurrency,
    maxRpm,
    maxTpm,
  };
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
  document.getElementById('lamp-' + target).innerHTML = remoteLampHTML(dirty ? { state: 'pending', label: '设置待应用', detail: '卡片设置尚未应用；原工具继续使用上一次已应用配置' } : agentLocalStatus(tool), 'agent');
  const applied = document.getElementById('agentApplied-' + target);
  if (applied) {
    const account = S.local?.accounts?.find((a) => a.id === binding.appliedId);
    applied.innerHTML = target === 'claude-desktop' ? '<span class="chip warn">自动应用暂不可用</span>' : dirty || binding.pending
      ? `<span class="chip warn">${form.id ? dirty ? '设置待应用' : '选择已保存，等待应用' : '未选择 API'}</span>${account ? `<span class="muted small">当前应用：${esc(account.name)}</span>` : ''}`
      : `<span class="chip ok">已应用 ${esc(account?.name || '')}</span>`;
  }
}

function canOpenNativeSession(s) {
  return ['codex', 'claude'].includes(s.tool) && !!s.cwd && !s.activeTurnId
    && ['idle', 'completed', 'failed', 'interrupted', 'stopped'].includes(s.status)
    && new RegExp('^' + s.tool + ':[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}$').test(s.sessionKey || '');
}

async function openRestore(target) {
  target = target.replace(/-desktop$/, '');
  S.restoreTarget = target;
  S.restoreSessions = [];
  S.restoreSearch = '';
  const kind = target;
  const native = kind === 'codex' || kind === 'claude';
  const binding = S.local?.bindings?.[target];
  const historyNote = native
    ? `读取原工具本地记录。终端恢复使用原会话编号、原项目目录和 ${kind === 'codex' ? 'Codex CLI' : 'Claude Code'} 卡片已应用的 API。${kind === 'claude' ? '不包含 Claude Desktop 普通云端聊天。' : ''}`
    : '这里列出 Bridge 与手机发起的会话，可在「对话」中查看。普通「打开 Agent」不会自动接回手机会话。' + (kind === 'hermes' ? 'Hermes 原会话固定启动 API；切换 API 或重启 Bridge 后可能需要新建会话，旧记录保留。' : 'DeepSeek 可在原会话中继续；会话存储独立于默认原生界面。');
  $('#modalRoot').innerHTML = `<div class="modal-back" data-act="closeModal"><div class="modal restore-modal" data-stop role="dialog" aria-modal="true" aria-labelledby="restoreTitle"><div class="modal-head"><h3 id="restoreTitle">查看会话</h3><div class="seg restore-seg" role="tablist" aria-label="选择工具">${['codex', 'claude', 'deepseek-harness', 'hermes'].map((t) => `<button type="button" role="tab" aria-selected="${t === target}" class="${t === target ? 'on' : ''}" data-act="toolRestore" data-target="${t}">${esc(TOOL_LABEL[t] || t)}</button>`).join('')}</div><button class="icon-btn" data-act="closeModal" aria-label="关闭">${icon('close')}</button></div><div class="modal-body"><p class="muted small">${historyNote}</p>${native && (!binding?.appliedId || binding.pending) ? '<div class="callout warn">请先在对应 CLI 工具卡片应用当前 API，才可以在终端恢复。仍可先查看已有记录。</div>' : ''}<input class="input" id="restoreSearch" type="search" placeholder="搜索标题、项目目录或模型" aria-label="搜索恢复会话"><div id="restoreList" class="restore-list"><div class="empty"><span class="spin"></span>正在读取原会话…</div></div><div id="restoreError" aria-live="polite"></div></div><div class="modal-foot"><span class="muted small" style="margin-right:auto">切换 API 不会删除这里的聊天记录</span><button class="btn" data-act="closeModal">关闭</button></div></div></div>`;
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
    const native = ['codex', 'claude'].includes(s.tool);
    const canResume = binding?.appliedId && !binding.pending && canOpenNativeSession(s);
    return `<div class="restore-row"><div class="li-main"><div class="li-title">${esc(s.title || '无标题会话')}</div><div class="sess-meta">${toolChip(s.tool, s.client)}<span class="status-dot s-${esc(s.status)}"></span>${esc(SESSION_STATUS[s.status] || s.status)} · ${esc(relTime(s.updatedAt))}${s.model ? ' · ' + esc(s.model) : ''}</div><div class="sess-cwd" title="${esc(s.cwd)}">${esc(s.cwd || '原项目目录未知')}</div></div><div class="restore-actions"><button class="btn sm" data-act="restoreView" data-k="${esc(s.sessionKey)}" data-kind="${esc(s.tool)}">查看记录</button>${native ? `<button class="btn sm primary" data-act="resumeSession" data-k="${esc(s.sessionKey)}" data-target="${esc(S.restoreTarget)}" ${canResume ? '' : 'disabled'} title="${running ? '先结束原任务' : canResume ? '恢复同一个会话，不新建' : '先应用当前 API，并确认原会话已结束且目录有效'}">终端恢复</button>` : ''}</div></div>`;
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
  const legacy = tc.legacyToolcfg || {};
  const legacyBlocked = (tool) => !!(legacy[tool] && legacy[tool].requiresWorkbench);
  const legacyButtonAttrs = (tool) => (loggedIn && !legacyBlocked(tool)) ? '' : 'disabled';
  const toolOf = (id) => (st.tools || []).find((t) => t.id === id) || { available: false };
  const stateChip = (x) => x.configured ? `<span class="chip ok">${icon('check')}已接入中转站</span>` : (x.error ? '<span class="chip bad">读取失败</span>' : '<span class="chip warn">未接入</span>');
  const needLogin = `<div class="callout">${ico('info')}<div>本地多 Key 管理与保留会话的原地切号请使用 <a href="#accounts">API 账号 / 切换</a>。下面保留旧版远程中转站的一键配置${loggedIn ? '。' : '，需要先登录远程服务。'}</div></div>`;
  const managedNotice = legacy.managed ? `<div class="callout warn">${ico('warn')}<div>当前 Bridge 正在使用进程级安全租约。旧版一键配置已暂停，避免把反代或官方账号凭据直接写入原工具；请在 <a href="#accounts">API 账号 / 切换</a> 中通过工具工作台应用或恢复。</div></div>` : '';
  const isWin = tc.os === 'windows';
  const mode = tc.codexAuthMode || 'token';
  view.innerHTML = `${needLogin}${managedNotice}
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
      <div class="row"><button class="btn primary" data-act="toolcfg" data-tool="claude" data-a="apply" ${legacyButtonAttrs('claude')}>一键配置</button>
        <button class="btn" data-act="toolcfg" data-tool="claude" data-a="restore" ${legacyBlocked('claude') ? 'disabled' : ''}>恢复原配置</button></div>
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
      <div class="row"><button class="btn primary" data-act="toolcfg" data-tool="codex" data-a="apply" ${legacyButtonAttrs('codex')}>一键配置</button>
        <button class="btn" data-act="toolcfg" data-tool="codex" data-a="restore" ${legacyBlocked('codex') ? 'disabled' : ''}>恢复原配置</button></div>
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
  // The cached machine state alone is not enough to safely paint this page:
  // toolcfg contains the managed-lease policy that can disable legacy writes.
  // Keep the page skeleton until both snapshots are known, rather than briefly
  // enabling a config mutation from an empty/default toolcfg object.
  if (cachedConfig && S.state) renderToolsPage(target, cachedState, cachedConfig);
  void Promise.all([loadState(), api('/api/toolcfg')]).then(([st, tc]) => {
    if (target.isConnected === false || S.route !== 'tools') return;
    S.toolcfg = tc;
    renderToolsPage(target, st, tc);
  }).catch((e) => {
    if (!(cachedConfig && S.state) && target.isConnected !== false && S.route === 'tools') target.innerHTML = `<div class="callout bad">${ico('warn')}<div>${esc(e.message)}</div></div>`;
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

function timelineEventIsOlder(previous, event, snapshotReplay) {
  // seq is scoped to a device; native history without seq uses source time.
  if (previous.seq > 0 && event.seq > 0 && previous.deviceId === event.deviceId) {
    if (event.seq < previous.seq) return true;
  } else if (previous.ts > 0 && event.ts > 0 && event.ts < previous.ts) return true;
  if (previous.final && !event.final) return true;
  if (previous.outputFinal && !event.outputFinal && typeof event.output === 'string' && event.output !== previous.output) return true;
  const terminal = new Set(['completed', 'failed', 'interrupted', 'cancelled']);
  if (terminal.has(previous.status) && ['running', 'started', 'pending'].includes(event.status)) return true;
  if (typeof previous.text === 'string' && typeof event.text === 'string' && previous.text !== event.text) {
    const newer = previous.seq > 0 && event.seq > previous.seq && previous.deviceId === event.deviceId
      || previous.ts > 0 && event.ts > previous.ts;
    if (!newer && previous.text.startsWith(event.text)) return true;
    if (snapshotReplay && !newer && !event.text.startsWith(previous.text)) return true;
  }
  return false;
}
function tlApply(ev, options) {
  if (ev.type === 'session.updated') {
    if (options?.snapshotReplay && S.openInfo?.sessionKey === ev.session?.sessionKey && !(ev.session.updatedAt > S.openInfo.updatedAt)) return;
    if (ev.session && !(S.openInfo?.sessionKey === ev.session.sessionKey && S.openInfo.updatedAt > ev.session.updatedAt)) S.openInfo = ev.session;
    return;
  }
  const k = tlKey(ev);
  if (ev.type === 'approval.resolved') {
    const i = S.tlIndex.get(k);
    if (i !== undefined) { S.tl[i] = Object.assign({}, S.tl[i], { resolved: ev }); return; }
  }
  if (S.tlIndex.has(k)) {
    const i = S.tlIndex.get(k);
    if (timelineEventIsOlder(S.tl[i], ev, options?.snapshotReplay)) return;
    S.tl[i] = Object.assign({}, S.tl[i], ev);
  } else {
    S.tlIndex.set(k, S.tl.length);
    S.tl.push(ev);
  }
}

const KIND_ICON = { command: 'terminal', file_change: 'edit', read: 'file', search: 'search', web: 'globe', mcp: 'box', other: 'box' };

function delegationSource(ev, tool) {
  const origin = ev.messageOrigin;
  if (ev.role !== 'user' || !origin || typeof origin !== 'object' || Array.isArray(origin) || origin.kind !== 'delegation') return '';
  // The mark identifies this conversation's runtime, never a claimed sender
  // in the message body or unverified origin metadata.
  const brand = tool === 'codex' || tool === 'codex-desktop' ? 'codex'
    : tool === 'claude' || tool === 'claude-desktop' ? 'claude' : '';
  return `<div class="msg-origin">${brand ? `<img src="agent-brand/${brand}-app-official.png" alt="" width="16" height="16" aria-hidden="true">` : ''}<span>来自另一项任务</span></div>`;
}

function tlItem(ev, tool) {
  switch (ev.type) {
    case 'message': {
      const role = ev.role === 'user' ? 'user' : 'assistant';
      const body = `<div class="msg ${role}">${renderText(ev.text)}</div>`;
      const source = delegationSource(ev, tool);
      return source ? `<div class="msg-envelope ${role}">${source}${body}</div>` : body;
    }
    case 'reasoning':
      return ev.text ? `<div class="reason">${esc(ev.text)}</div>` : '';
    case 'tool': {
      const chip = ev.status === 'running' ? '<span class="chip info"><span class="spin" style="width:10px;height:10px"></span>运行中</span>'
        : ev.status === 'failed' ? `<span class="chip bad">失败${ev.exitCode !== undefined && ev.exitCode !== null ? ' ' + esc(ev.exitCode) : ''}</span>`
          : ev.kind === 'plan' && ev.planKind === 'tasks' ? '<span class="chip">已更新</span>' : '<span class="chip ok">完成</span>';
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
      ${running ? `<button class="btn sm danger" data-act="interrupt">${icon('stop')}停止</button>` : canOpenNativeSession(s) ? `<button class="btn sm" data-act="resumeSession" data-k="${esc(s.sessionKey)}" data-target="${esc(s.tool)}">${icon('terminal')}终端恢复</button>` : ''}</div></div>
    <div class="timeline" id="timeline">${S.tl.map(ev => tlItem(ev, s.tool)).join('') || '<div class="empty">还没有内容</div>'}</div>
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

const SETTINGS_FIELDS = { autostart: 'swAuto', openConsoleOnStart: 'swOpen', allowPhoneAutoAll: 'swPhoneAuto', approval: 'pol' };
const SETTINGS_UI = { revision: 0, values: new Map(), pending: new Map(), messages: new Map() };
function settingsSnapshot(st, startedAt = SETTINGS_UI.revision) {
  const config = { ...(st.config || {}) };
  for (const key of Object.keys(SETTINGS_FIELDS)) {
    const previous = SETTINGS_UI.values.get(key);
    if (previous && (previous.revision > startedAt || SETTINGS_UI.pending.has(key))) config[key] = previous.value;
    else SETTINGS_UI.values.set(key, { value: config[key], revision: startedAt });
  }
  return { ...st, config };
}
function settingsControls(key) {
  return key === 'approval' ? $$('input[name="pol"]') : [document.getElementById(SETTINGS_FIELDS[key])].filter(Boolean);
}
function projectSettingsState() {
  for (const key of Object.keys(SETTINGS_FIELDS)) {
    const pending = SETTINGS_UI.pending.get(key), stored = SETTINGS_UI.values.get(key);
    if (!stored && !pending) continue;
    const value = pending ? pending.value : stored.value;
    for (const control of settingsControls(key)) {
      control.checked = key === 'approval' ? control.value === value : Boolean(value);
      control.disabled = Boolean(pending);
      control.setAttribute?.('aria-busy', String(Boolean(pending)));
      if (key === 'approval') control.closest('.radio-card')?.classList.toggle('sel', control.checked);
    }
    const status = document.getElementById('settingStatus-' + key);
    if (status) status.textContent = pending ? pending.message : SETTINGS_UI.messages.get(key) || '';
    if (key === 'approval') {
      const warning = document.getElementById('approvalPolicyWarning');
      if (warning) warning.hidden = value !== 'auto_all';
    }
  }
}
function saveSetting(control) {
  const key = Object.keys(SETTINGS_FIELDS).find((name) => name === 'approval' ? control.name === 'pol' : control.id === SETTINGS_FIELDS[name]);
  if (!key) return Promise.resolve();
  const active = SETTINGS_UI.pending.get(key);
  if (active) { projectSettingsState(); return active.promise; }
  const value = key === 'approval' ? control.value : control.checked;
  const previous = SETTINGS_UI.values.get(key)?.value ?? S.state?.config?.[key];
  SETTINGS_UI.values.set(key, { value: previous, revision: SETTINGS_UI.revision });
  const owner = $('#view');
  SETTINGS_UI.revision++;
  const operation = { value, message: key === 'approval' && value === 'auto_all' ? '等待确认…' : '正在保存…', promise: null };
  SETTINGS_UI.pending.set(key, operation);
  SETTINGS_UI.messages.delete(key);
  projectSettingsState();
  operation.promise = (async () => {
    let saved = false;
    try {
      if (key === 'approval' && value === 'auto_all') {
        if (!await confirm('“全部自动”会让 AI 不经确认运行任意命令，确定开启吗？')) return;
        if (S.route !== 'settings' || $('#view') !== owner || control.isConnected === false) return;
        operation.message = '正在保存…'; projectSettingsState();
      }
      const result = await api('/api/settings', { [key]: value });
      if (result?.ok !== true) throw new Error('保存结果尚未确认');
      SETTINGS_UI.values.set(key, { value, revision: ++SETTINGS_UI.revision });
      saved = true;
      if (S.state) S.state.config = { ...(S.state.config || {}), [key]: value };
      SETTINGS_UI.messages.set(key, '已保存');
    } catch (error) {
      SETTINGS_UI.messages.set(key, '未确认保存，已还原显示：' + (error?.message || String(error)));
    } finally {
      if (!saved) {
        SETTINGS_UI.values.set(key, { value: previous, revision: ++SETTINGS_UI.revision });
        if (S.state) S.state.config = { ...(S.state.config || {}), [key]: previous };
      }
      if (SETTINGS_UI.pending.get(key) === operation) SETTINGS_UI.pending.delete(key);
      projectSettingsState();
    }
  })();
  return operation.promise;
}

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
    <span class="li-main"><span class="li-title">对话记录</span><span class="li-sub">查看四种 Agent 的对话与手机任务记录，进入后加载</span></span>
    ${pending ? `<span class="chip bad">${pending} 个待批准</span>` : ''}<span class="set-link-chev">${icon('chev')}</span>
  </a>
  <div class="card" id="remoteDirectoryGrants">
    <h3 class="card-title">${icon('folder')}远程目录授权</h3>
    <p class="card-sub">允许手机浏览与创建项目的目录范围。新建项目和会话请在手机中操作。</p>
    ${(c.projects || []).map(p => `<div class="setting-row"><div class="txt"><div class="t">${esc(p.name)}</div><div class="d mono">${esc(p.path)}</div></div><button class="btn sm" data-act="rmProj" data-path="${esc(p.path)}">撤销授权</button></div>`).join('')}
    <div class="row" style="gap:8px;margin-top:12px"><input class="input" id="projPath" aria-label="远程允许目录" placeholder="选择允许手机访问的目录"><button class="btn" data-act="browse">浏览…</button><button class="btn primary" data-act="addProj">允许目录</button></div>
  </div>
  <div class="card" id="localConversationSync">
    <div class="row"><div class="li-main"><div class="li-title">本机对话同步</div><div class="li-sub">重新读取 Codex、Claude Code 和 Claude Desktop 的会话列表；当前打开的对话会一并刷新</div></div><button class="btn sm" data-act="syncLocalConversations">${icon('refresh')}重新同步</button></div>
    <div class="muted small" id="localSyncStatus" role="status" aria-live="polite" style="margin-top:8px">仅读取本机记录，不修改原应用数据；未打开的对话正文会在进入时读取。</div>
  </div>
  ${st.os === 'darwin' ? `<div class="callout small">首次访问项目文件夹时，macOS 会询问文件权限；是否允许由系统控制。</div>` : ''}
  <div class="card">
    <h3 class="card-title" style="margin-bottom:14px">${ico('power')}启动</h3>
    <div class="setting-row"><div class="txt"><div class="t">开机自动启动</div><div class="d">登录电脑后在后台运行，手机随时能连上这台电脑</div></div>
      <label class="switch"><input type="checkbox" id="swAuto" ${c.autostart ? 'checked' : ''}><span></span></label></div><div class="muted small" id="settingStatus-autostart" role="status" aria-live="polite"></div>
    <div class="setting-row"><div class="txt"><div class="t">开机启动时打开控制台</div><div class="d">关闭后开机只在后台运行，需要时${launchHint()}即可打开这个页面</div></div>
      <label class="switch"><input type="checkbox" id="swOpen" ${c.openConsoleOnStart ? 'checked' : ''}><span></span></label></div><div class="muted small" id="settingStatus-openConsoleOnStart" role="status" aria-live="polite"></div>
  </div>
  <div class="card">
    <h3 class="card-title" style="margin-bottom:6px">${ico('check')}审批策略</h3>
    <p class="card-sub" style="margin-bottom:14px">从手机发起的新任务默认使用这个策略（手机上发任务时也可以单独选择）</p>
    <div class="radio-cards">${pol.map(([v, t, d, cls]) => `<label class="radio-card ${cls} ${c.approval === v ? 'sel' : ''}"><input type="radio" name="pol" value="${v}" ${c.approval === v ? 'checked' : ''}>
      <span class="t">${cls ? `<span style="color:var(--danger)">${icon('warn')}</span>` : ''}${t}</span><span class="d">${d}</span></label>`).join('')}</div>
    <div class="muted small" id="settingStatus-approval" role="status" aria-live="polite"></div>
    <div class="callout bad" id="approvalPolicyWarning" ${c.approval === 'auto_all' ? '' : 'hidden'} style="margin-top:14px">${ico('warn')}<div><b>风险提示：</b>“全部自动”下 AI 可以不经确认运行任意命令（包括删除文件、访问网络）。请只在允许的项目文件夹里、并且信任任务内容时使用。</div></div>
    <div class="setting-row" style="margin-top:14px;border-top:1px solid var(--border)"><div class="txt"><div class="t">允许手机选择「全部自动」</div><div class="d">关闭时手机只能用「每步询问」或「自动改文件」，手机绑定泄露也无法不经确认运行命令</div></div>
      <label class="switch"><input type="checkbox" id="swPhoneAuto" ${c.allowPhoneAutoAll ? 'checked' : ''}><span></span></label></div><div class="muted small" id="settingStatus-allowPhoneAutoAll" role="status" aria-live="polite"></div>
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
    <div class="divider"></div>
    <div class="row"><div class="li-main"><div class="li-title">退出程序</div><div class="li-sub">退出后手机将看不到这台电脑，正在运行的任务会停止</div></div>
      <button class="btn danger" data-act="quit">${icon('power')}退出 Salcara Bridge</button></div>
  </div>
  <div class="card set-support">
    <div class="set-support-copy"><span class="set-link-ico" aria-hidden="true">${icon('github')}</span>
      <div class="li-main"><div class="li-title">支持 Salcara</div><div class="li-sub">觉得好用？在 GitHub 点个 Star，支持项目继续成长。</div></div></div>
    <a class="btn" href="https://github.com/dkdjndbfj-wq/salcara-desktop" target="_blank" rel="noopener noreferrer">${icon('star')}去 GitHub 点星</a>
  </div>`;
  projectSettingsState();
  loadLogs();
}

RENDER_settings = function (view) {
  const target = view;
  const startedAt = SETTINGS_UI.revision;
  const previous = S.state;
  const presentation = (st) => JSON.stringify([st.config, st.version, st.os, st.appBundle, st.exe, st.installedFrom]);
  const cachedPresentation = previous ? presentation(settingsSnapshot(previous)) : null;
  // Without a real state, wait on the skeleton instead of painting default switches that then flip.
  if (S.state) renderSettingsPage(target, settingsSnapshot(S.state));
  void loadState().then((st) => {
    if (target.isConnected === false || S.route !== 'settings') return;
    const snapshot = settingsSnapshot(st, startedAt);
    if (S.state === st) S.state = snapshot;
    if (cachedPresentation === presentation(snapshot)) { projectSettingsState(); return; }
    renderSettingsPage(target, snapshot);
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

// Confirmations open as a centred in-app dialog instead of the system box. Callers await the
// answer; a missing document (tests, early boot) falls back to the browser's own confirm.
const nativeConfirm = typeof window !== 'undefined' && typeof window.confirm === 'function' ? window.confirm.bind(window) : null;
let activeConfirmation = null;
function cancelActiveConfirmation() { activeConfirmation?.finish(false); }
function confirmDialog(message) {
  if (typeof document === 'undefined' || !document.body) return Promise.resolve(nativeConfirm ? nativeConfirm(message) : false);
  const text = String(message || '');
  const cut = text.search(/[？?]/);
  const title = cut >= 0 && cut < 60 ? text.slice(0, cut + 1) : text.split('\n')[0];
  const body = (cut >= 0 && cut < 60 ? text.slice(cut + 1) : text.split('\n').slice(1).join('\n')).trim();
  const danger = /移除|删除|清空|退出|重置|解除|卸载|断开|消耗|不可撤销/.test(title + body);
  const okLabel = danger ? (title.match(/移除|删除|清空|退出|重置|解除|卸载|断开/) || ['确定'])[0] : '确定';
  const previousFocus = activeConfirmation?.previousFocus || document.activeElement;
  activeConfirmation?.finish(false, false);
  return new Promise((resolve) => {
    document.getElementById('confirmRoot')?.remove();
    const root = document.createElement('div');
    root.id = 'confirmRoot';
    root.innerHTML = `<div class="confirm-back"><div class="confirm-modal" role="alertdialog" aria-modal="true" aria-labelledby="confirmTitle" aria-describedby="confirmBody"><span class="confirm-ico${danger ? ' danger' : ''}">${icon(danger ? 'trash' : 'info')}</span><h3 id="confirmTitle">${esc(title)}</h3>${body ? `<p id="confirmBody">${esc(body).replace(/\n+/g, '<br>')}</p>` : ''}<div class="confirm-actions"><button type="button" class="btn" data-confirm="0">取消</button><button type="button" class="btn ${danger ? 'danger-solid' : 'primary'}" data-confirm="1">${esc(okLabel)}</button></div></div></div>`;
    let settled = false;
    const done = (value, restoreFocus = true) => {
      if (settled) return;
      settled = true;
      removeEventListener('keydown', onKey, true);
      root.remove();
      if (activeConfirmation?.root === root) activeConfirmation = null;
      if (restoreFocus && previousFocus?.isConnected !== false) previousFocus?.focus?.();
      resolve(value);
    };
    const onKey = (e) => {
      if (e.isComposing) return;
      if (e.key === 'Escape') { e.preventDefault(); e.stopImmediatePropagation(); done(false); }
      else if (e.key === 'Enter') { e.preventDefault(); e.stopImmediatePropagation(); done(document.activeElement?.dataset?.confirm === '1'); }
      else if (e.key === 'Tab') {
        e.preventDefault(); e.stopImmediatePropagation();
        const next = document.activeElement?.dataset?.confirm === '1' ? '0' : '1';
        root.querySelector(`[data-confirm="${next}"]`)?.focus();
      }
    };
    root.addEventListener('click', (e) => {
      const b = e.target.closest('[data-confirm]');
      if (b) { e.stopPropagation(); done(b.dataset.confirm === '1'); }
      else if (e.target.classList.contains('confirm-back')) done(false);
    });
    addEventListener('keydown', onKey, true);
    document.body.appendChild(root);
    activeConfirmation = { root, previousFocus, finish: done };
    root.querySelector(`[data-confirm="${danger ? '0' : '1'}"]`).focus();
  });
}
if (typeof window !== 'undefined') window.confirm = confirmDialog;

const BUTTON_OPERATIONS = new WeakMap();
const APPROVAL_OPERATIONS = new Map();
function exclusiveUIOperation(operations, key, fn) {
  if (operations.has(key)) return operations.get(key);
  let resolve, reject;
  const pending = new Promise((done, fail) => { resolve = done; reject = fail; });
  operations.set(key, pending);
  const settle = (fn, value) => { if (operations.get(key) === pending) operations.delete(key); fn(value); };
  try { Promise.resolve(fn()).then((value) => settle(resolve, value), (error) => settle(reject, error)); }
  catch (error) { settle(reject, error); }
  return pending;
}
function busy(btn, fn) {
  if (!btn) { try { return Promise.resolve(fn()); } catch (error) { return Promise.reject(error); } }
  return exclusiveUIOperation(BUTTON_OPERATIONS, btn, async () => {
    const original = { html: btn.innerHTML, disabled: btn.disabled, aria: btn.getAttribute?.('aria-busy') };
    btn.disabled = true;
    btn.setAttribute?.('aria-busy', 'true');
    btn.innerHTML = '<span class="spin"></span>' + btn.textContent;
    try { return await fn(); }
    finally {
      if (btn.isConnected !== false) {
        btn.disabled = original.disabled;
        btn.innerHTML = original.html;
        if (original.aria == null) btn.removeAttribute?.('aria-busy'); else btn.setAttribute?.('aria-busy', original.aria);
      }
    }
  });
}

let localConversationSyncPending = false;
async function syncLocalConversations(button) {
  if (localConversationSyncPending || S.route !== 'settings') return;
  const status = $('#localSyncStatus');
  if (!status) return;
  const routeRevision = S.routeRevision;
  const openedKey = S.openKey;
  let syncStage = '会话索引';
  const ownsView = () => S.route === 'settings' && S.routeRevision === routeRevision && status.isConnected !== false;
  const setStatus = (message) => { if (ownsView()) status.textContent = message; };
  localConversationSyncPending = true;
  try {
    await busy(button, async () => {
      try {
        setStatus('正在重新读取本机会话索引…');
        const listing = await api('/api/sessions');
        if (!ownsView()) return;
        const sessions = Array.isArray(listing.sessions) ? listing.sessions : [];
        S.sessions = sessions;
        if (openedKey && S.openKey === openedKey) {
          syncStage = '当前对话内容';
          setStatus(`已读取 ${sessions.length} 个会话，正在重新读取当前对话…`);
          const opened = await api('/api/session?key=' + encodeURIComponent(openedKey));
          if (!ownsView() || S.openKey !== openedKey) return;
          S.openInfo = opened.session;
          S.tl = [];
          S.tlIndex = new Map();
          (opened.events || []).forEach(tlApply);
          S.approvals.filter((approval) => approval.sessionKey === openedKey).forEach(tlApply);
          setStatus(`已同步 ${sessions.length} 个会话，并重新读取当前对话内容。`);
        } else {
          setStatus(`已重新读取 ${sessions.length} 个本机会话索引；完整对话内容会在打开时读取。`);
        }
      } catch (error) {
        setStatus(`${syncStage}读取失败：${error && typeof error.message === 'string' ? error.message : String(error)}`);
      }
    });
  } finally {
    localConversationSyncPending = false;
  }
}

const ACTIONS = {
  accountOpen: async (button) => {
    if (typeof wbOpenAccount !== 'function') throw new Error('工具启动组件尚未就绪，请刷新后重试');
    await wbOpenAccount(button.dataset.target, button.dataset.id, button);
  },
  refresh: () => route(),
  syncLocalConversations,
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
    if (!await confirm('解除这台电脑的手机绑定？旧手机和未使用的二维码都会失效，本地 API 密钥与会话保留。')) return;
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
    if (b?.isConnected === false || !await ensureLocalAccounts(b)) return;
    await openRestore(b.dataset.target);
  },
  restoreView: async (b) => {
    S.sessFilter = b.dataset.kind;
    S.openKey = b.dataset.k;
    $('#modalRoot').innerHTML = '';
    if (location.hash === '#sessions') await route(); else location.hash = 'sessions';
  },
  resumeSession: async (b) => {
    if (b?.isConnected === false || (typeof WB !== 'undefined' && WB.launchPending)) return;
    const progress = typeof beginLaunchProgress === 'function' ? beginLaunchProgress(b.dataset.target, '原会话', ['提交启动请求']) : null;
    const owner = $('#restoreError'), route = S.route;
    if (typeof WB !== 'undefined') WB.launchPending = true;
    try {
    await busy(b, async () => {
      try {
        const r = await (typeof launchAPI === 'function' ? launchAPI : ((path, body) => api(path, body)))('/api/local/resume', { target: b.dataset.target, sessionKey: b.dataset.k }, progress);
        if (r?.ok !== true) throw new Error('启动结果尚未确认');
        progress?.finish();
        if (b.isConnected === false || S.route !== route || $('#restoreError') !== owner) return;
        toast(r.result.message, 'ok');
        const out = $('#restoreError');
        if (out) out.innerHTML = `<div class="callout ok">${ico('check')}<div>${esc(r.result.message)}</div></div>`;
      } catch (e) {
        progress?.fail(e);
        if (b.isConnected === false || S.route !== route || $('#restoreError') !== owner) return;
        const out = $('#restoreError');
        if (out) out.innerHTML = `<div class="callout warn">${ico('warn')}<div>${esc(e.message)}</div></div>`;
        else throw e;
      }
    });
    } finally { if (typeof WB !== 'undefined') WB.launchPending = false; }
  },
  localNew: async (b) => { if (b?.isConnected === false || !await ensureLocalAccounts(b)) return; localEditor(null, b.dataset.kind); },
  localEdit: (b) => localEditor(S.local.accounts.find((a) => a.id === b.dataset.id)),
  localPreset: (b) => {
    const input = $('#laBase');
    if (!input) return;
    input.value = b.dataset.url || '';
    for (const id of ['laAuth', 'laWire']) {
      const control = $('#' + id);
      if (control) { control.value = 'auto'; control.dispatchEvent(new Event('change', { bubbles: true })); }
    }
    $$('.preset-row .preset').forEach((x) => x.classList.toggle('on', x === b));
    input.dispatchEvent(new Event('input', { bubbles: true }));
  },
  localWire: (b) => {
    const select = $('#laWire');
    if (!select || !['auto', 'responses', 'chat', 'anthropic'].includes(b.dataset.editorWire)) return;
    select.value = b.dataset.editorWire;
    select.dispatchEvent(new Event('change', { bubbles: true }));
    localEditorProtocolSync();
    localEditorDraftSync();
  },
  localSave: async (b) => {
    await busy(b, async () => {
      const body = localForm();
      const editor = $('#laID'), submitted = JSON.stringify(body);
      const draft = S.localEditorDraft?.input === editor ? S.localEditorDraft : null;
      const errorBox = $('#localSaveError');
      if (errorBox) errorBox.textContent = '';
      if (draft) draft.saving = true;
      try {
        if (typeof localValidateForm === 'function') localValidateForm(body);
        else if (!body.id && !body.key.trim()) throw new Error('请填写 API Key');
        const saved = await api('/api/local/accounts', body);
        if (!saved?.account?.id) throw new Error('尚未确认保存结果，请重试');
        if (saved.account && typeof queueAccountRemoteProbe === 'function') void queueAccountRemoteProbe(saved.account);
        // A late save must not dismiss another editor or discard newer input.
        if ($('#laID') !== editor) return;
        if (JSON.stringify(localForm()) !== submitted || draft?.closing) {
          // The create succeeded while the user kept editing. Retain the new
          // record identity so their next save updates it instead of creating
          // a duplicate card; leave every visible draft field untouched.
          if (!body.id && saved.account?.id) editor.value = saved.account.id;
          if (saved.account.sourceRevision) S.localDraftSourceRevision = saved.account.sourceRevision;
          if (draft) draft.initial = JSON.stringify({ ...body, id: saved.account.id, ...(S.localDraftSourceRevision ? { sourceRevision: S.localDraftSourceRevision } : {}) });
          if (typeof localEditorDraftSync === 'function') localEditorDraftSync();
          return;
        }
        const wbTarget = b.dataset.wbTarget;
        const continueInAgent = wbTarget && b.isConnected && S.route === 'overview';
        $('#modalRoot').innerHTML = '';
        if (S.localEditorDraft === draft) S.localEditorDraft = null;
        if (draft?.onReturn) { toast('上游 API 已保存', 'ok'); await draft.onReturn(); return; }
        if (continueInAgent && typeof wbContinueWithSaved === 'function') {
          await wbContinueWithSaved(wbTarget, saved.account?.id);
          return;
        }
        const directoryReset = body.models?.length && Object.hasOwn(saved.account, 'models') && !saved.account.models?.length;
        toast(directoryReset ? 'API 已保存，连接信息已变更，请重新读取模型目录' : '密钥已保存，到 Agent 选择工具并确认配置', directoryReset ? '' : 'ok');
        if (wbTarget) return;
        if (location.hash === '#accounts') await route(); else location.hash = 'accounts';
      } catch (error) {
        if ($('#laID') !== editor) return;
        if (errorBox && errorBox.isConnected !== false) errorBox.textContent = error?.message || String(error);
        throw error;
      } finally {
        if (draft) draft.saving = false;
      }
    });
  },
  localModels: async (b) => {
    await busy(b, async () => {
      const modal = !b.dataset.id && $('#laID');
      const body = modal ? localForm() : { id: b.dataset.id };
      const submitted = modal ? JSON.stringify(body) : '';
      const modelsInput = modal ? $('#laModels') : null;
      const out = modal ? $('#localProbeResult') : null;
      const catalog = modal ? $('#laModelCatalog') : null;
      const status = modal ? $('#laModelSyncState') : null;
      const stillCurrent = () => !modal || $('#laID') === modal && $('#laModels') === modelsInput && JSON.stringify(localForm()) === submitted;
      const markStale = () => { if (modal && $('#laID') === modal && status) status.textContent = '配置已更改，请重新同步'; };
      if (status) status.textContent = '正在同步…';
      try {
        const result = await api('/api/local/models', modal ? { ...body, draftOnly: true } : body);
        if (!Array.isArray(result?.models) || result.models.some((model) => typeof model !== 'string')) throw new Error('上游返回的模型列表格式无效');
        if (!stillCurrent()) { markStale(); return; }
        if (modal && out) {
          const models = [...new Set(result.models.map(model => model.trim()).filter(Boolean))];
          S.localDraftModels = models;
          S.localDraftModelsManual = false;
          if (modelsInput) modelsInput.value = models.join('\n');
          if (catalog) catalog.innerHTML = localModelCatalogHTML(models);
          if (status) status.textContent = `${models.length} 个模型 · 已填入草稿`;
          out.innerHTML = `<div class="callout ${models.length ? 'ok' : 'warn'}">${ico(models.length ? 'check' : 'info')}<div>${models.length ? '模型列表已获取，保存后生效。此操作未发起生成请求。' : '上游未返回模型，可手动填写模型 ID 后保存。'}</div></div>`;
          if (typeof localEditorDraftSync === 'function') localEditorDraftSync();
        } else { toast(`已读取 ${result.models.length} 个模型；到 Agent 中配置`, 'ok'); await route(); }
      } catch (e) {
        if (!stillCurrent()) { markStale(); return; }
        if (status) status.textContent = S.localDraftModels.length ? `${S.localDraftModels.length} 个模型 · 同步失败` : '同步失败';
        if (out) out.innerHTML = `<div class="callout warn">${ico('warn')}<div>${esc(e.message)}</div></div>`; else throw e;
      }
    });
  },
  localSelect: async (b) => { const r = await api('/api/local/accounts/select', { id: b.dataset.id }); toast(r.message, 'ok'); await route(); },
  localDelete: async (b) => {
    if (!await confirm('从账号列表移除？正在运行的工具、会话与实例配置目录会保留（其中可能仍有 Key）。需要撤销访问时，请同时在服务商后台撤销 Key。')) return;
    await api('/api/local/accounts/delete', { id: b.dataset.id });
    if ($('#laID')?.value === b.dataset.id) $('#modalRoot').innerHTML = '';
    toast('已从账号列表移除', 'ok'); await route();
  },
  localLaunch: async (b) => {
    if (b?.isConnected === false || (typeof WB !== 'undefined' && WB.launchPending)) return;
    const independentOk = confirm('这是高级“独立多开”：会打开一个独立的新实例，不显示原工具的历史。如果只是换 Key 继续原工作，请取消并使用“切换并打开”。确定新建独立实例？');
    if (!(independentOk instanceof Promise ? await independentOk : independentOk)) return;
    const target = b.dataset.target || document.getElementById('localTarget-' + b.dataset.id)?.value;
    const progress = typeof beginLaunchProgress === 'function' ? beginLaunchProgress(target, '独立实例', ['提交启动请求']) : null;
    const owner = $('#view'), route = S.route;
    if (typeof WB !== 'undefined') WB.launchPending = true;
    try {
    await busy(b, async () => {
      const r = await (typeof launchAPI === 'function' ? launchAPI : ((path, body) => api(path, body)))('/api/local/launch', { id: b.dataset.id, target }, progress);
      if (r?.ok !== true) throw new Error('启动结果尚未确认');
      progress?.finish();
      if (b.isConnected === false || S.route !== route || $('#view') !== owner) return;
      toast('已启动 ' + (TOOL_LABEL[target] || (target === 'codex-desktop' ? 'Codex Desktop' : target)), 'ok');
      const hint = $('#localLaunchResult');
      if (hint) hint.remove();
      const note = document.createElement('div'); note.id = 'localLaunchResult'; note.className = 'callout ok';
      note.textContent = r.result.message + ' 实例目录：' + r.result.profileDir;
      $('#view').prepend(note);
      if (S.local) { const a = S.local.accounts.find((a) => a.id === b.dataset.id); if (a) { a.target = target; a.lastUsedAt = Date.now(); } }
    });
    } catch (error) {
      progress?.fail(error);
      if (b.isConnected !== false && S.route === route && $('#view') === owner) throw error;
    }
    finally { if (typeof WB !== 'undefined') WB.launchPending = false; }
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
    if (b?.isConnected === false || (typeof WB !== 'undefined' && WB.launchPending)) return;
    if (!$('#switchReady')?.checked) throw new Error('请先结束任务、保存内容，并勾选确认');
    const progress = typeof beginLaunchProgress === 'function' ? beginLaunchProgress(b.dataset.target, '当前 API', ['提交启动请求']) : null;
    const owner = $('#switchReady'), routeBefore = S.route;
    if (typeof WB !== 'undefined') WB.launchPending = true;
    try {
    await busy(b, async () => {
      try {
        const r = await (typeof launchAPI === 'function' ? launchAPI : ((path, body) => api(path, body)))('/api/local/switch', { id: b.dataset.id, target: b.dataset.target, confirmed: true }, progress);
        if (r?.ok !== true) throw new Error('启动结果尚未确认');
        progress?.finish();
        if (S.route !== routeBefore || $('#switchReady') !== owner) return;
        $('#modalRoot').innerHTML = '';
        const refresh = route(), revision = S.routeRevision;
        await refresh;
        if (S.route !== routeBefore || S.routeRevision !== revision || $('#modalRoot')?.innerHTML) return;
        const note = document.createElement('div'); note.className = 'callout ok'; note.textContent = r.result.message;
        $('#view').prepend(note); toast('已切换并打开原工具', 'ok');
      } catch (e) {
        progress?.fail(e);
        if (S.route !== routeBefore || $('#switchReady') !== owner) return;
        const out = $('#switchError');
        if (out) out.innerHTML = `<div class="callout warn">${ico('warn')}<div>${esc(e.message)}</div></div>`;
        else throw e;
      }
    });
    } finally { if (typeof WB !== 'undefined') WB.launchPending = false; }
  },
  localApply: async (b) => {
    if (!await confirm('只更新原工具的 CLI API 配置，不自动打开？会先备份，保留原会话。请先退出旧工具，更新后手动重开；后续打开也会使用这个 Key。')) return;
    await busy(b, async () => { const r = await api('/api/local/default', { id: b.dataset.id, action: 'apply' }); toast(r.message, 'ok'); });
  },
  localRestore: async (b) => {
    if (!await confirm('恢复通过本地账号应用前的系统配置？若配置后来被其他程序修改，会停止自动恢复以保护新改动。')) return;
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
    if (!await confirm('退出登录后，手机将看不到这台电脑。确定吗？')) return;
    await api('/api/logout', {});
    S.pairQR = ''; S.pairExpires = 0;
    toast('已退出登录');
    route();
  },
  toolcfg: async (b) => {
    const tool = b.dataset.tool, a = b.dataset.a;
    if (a === 'restore' && !await confirm('恢复到一键配置之前的原配置？')) return;
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
  closeModal: (b, e) => { if (b.classList.contains('modal-back') && e.target.closest('[data-stop]')) return; return closeLocalEditor(); },
  addProj: async (b) => {
    const p = $('#projPath').value.trim();
    if (!p) { toast('请填写文件夹路径', 'bad'); return; }
    await busy(b, async () => {
      try { await api('/api/projects', { path: p }); toast('已添加', 'ok'); route(); } catch (e) { toast(e.message, 'bad'); }
    });
  },
  rmProj: async (b) => {
    if (!await confirm('移除后手机不能再在这个文件夹里发起任务。确定吗？')) return;
    await api('/api/projects/remove', { path: b.dataset.path });
    route();
  },
  sessFilter: (b) => { S.sessFilter = b.dataset.k; $$('.filter').forEach((x) => x.classList.toggle('on', x === b)); loadSessions(); },
  reloadSessions: () => loadSessions(),
  openSess: (b) => openSession(b.dataset.k),
  approve: async (b) => {
    await exclusiveUIOperation(APPROVAL_OPERATIONS, b.dataset.id, async () => {
      const buttons = (typeof $$ === 'function' ? $$('[data-act="approve"]').filter((button) => button.dataset.id === b.dataset.id) : [b])
        .map((button) => ({ button, disabled: button.disabled }));
      buttons.forEach(({ button }) => { button.disabled = true; });
      try { await busy(b, async () => {
        const result = await api('/api/approval', { approvalId: b.dataset.id, decision: b.dataset.d });
        if (result?.ok !== true) throw new Error('处理结果尚未确认，待批准操作已保留');
        S.approvals = S.approvals.filter((a) => a.approvalId !== b.dataset.id);
        setApprovalBadge();
        renderApprovalsBox();
        toast('已处理', 'ok');
      }); } finally { buttons.forEach(({ button, disabled }) => { if (button.isConnected !== false) button.disabled = disabled; }); }
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
  quit: async (button) => {
    if (S.route !== 'settings' || button?.isConnected === false) return;
    const owner = $('#view'), body = document.body, revision = S.routeRevision;
    const current = () => S.route === 'settings' && S.routeRevision === revision && $('#view') === owner
      && owner?.isConnected !== false && button?.isConnected !== false && document.body === body;
    await busy(button, async () => {
      try {
        if (!await confirm('退出 Salcara Bridge？手机将看不到这台电脑，直到你再次打开它。') || !current()) return;
        const result = await api('/api/quit', {});
        if (!current()) return;
        if (result?.ok !== true) throw new Error('服务端未确认退出请求');
        body.innerHTML = '<div style="display:flex;height:100vh;align-items:center;justify-content:center;flex-direction:column;gap:8px;font-family:var(--font)"><h2 style="margin:0">Salcara Bridge 正在退出</h2><p style="color:#6D768B">退出请求已受理，请稍候…</p></div>';
      } catch (error) {
        if (current()) toast('退出未确认，请重试：' + (error?.message || String(error)), 'bad');
      }
    });
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
    if (t.id === 'laWire') localEditorProtocolSync();
    else if (t.id === 'localKind') { S.localKind = t.value; renderLocalCards(); }
    else if (t.id === 'laRemoteEnabled') {
      const choices = $('#laRemoteHubChoices');
      if (choices) choices.hidden = !t.checked;
      const root = t.closest('.remote-hub-bindings');
      if (root) root.dataset.touched = 'true';
      const state = t.closest('.remote-binding-toggle')?.querySelector('.remote-binding-copy small');
      if (state) state.textContent = t.checked ? '仅对下方已登记站点开放' : '当前关闭';
    }
    else if (t.matches?.('[data-remote-hub]')) t.closest('.remote-hub-bindings')?.setAttribute('data-touched', 'true');
    else if (t.id === 'remoteSaved' && t.value) { const u = new URL(t.value); $('#remoteRoot').value = u.origin; $('#remoteHub').value = t.value; S.remoteDiscovery = null; $('#remoteConnect').disabled = true; }
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
    else if (t.id === 'swAuto' || t.id === 'swOpen' || t.id === 'swPhoneAuto' || t.name === 'pol') await saveSetting(t);
    else if (t.name === 'cxmode') {
      $$('input[name=cxmode]').forEach((i) => i.closest('.radio-card').classList.toggle('sel', i.checked));
    }
    if (t.id?.startsWith('la') || t.matches?.('[data-remote-hub]')) localEditorDraftSync();
  } catch (err) { toast(err.message, 'bad'); if (t.type === 'checkbox') t.checked = !t.checked; if (t.id.startsWith('tool')) { t.disabled = false; toolDraftStatus(t.dataset.target); } }
});

document.addEventListener('input', (e) => {
  if (e.target.id?.startsWith('la')) localEditorDraftSync();
  else if (e.target.id === 'localSearch') { S.localFilter = e.target.value; renderLocalCards(); }
  else if (e.target.id === 'remoteRoot' || e.target.id === 'remoteHub') { S.remoteDiscovery = null; $('#remoteConnect').disabled = true; }
  else if (e.target.id === 'restoreSearch') { S.restoreSearch = e.target.value; renderRestoreList(); }
  else if (e.target.id === 'sessionSearch') { S.sessSearch = e.target.value; renderSessionList(); }
  else if (e.target.id.startsWith('toolModel-')) { toolDraftStatus(e.target.dataset.target); }
});

document.addEventListener('keydown', (e) => {
  if (e.target.id === 'sendText' && e.key === 'Enter' && (e.ctrlKey || e.metaKey)) { e.preventDefault(); ACTIONS.send($('[data-act=send]')); }
  if (e.target.id === 'devInput' && e.key === 'Enter') ACTIONS.saveName($('[data-act=saveName]'));
  if (e.target.id === 'projPath' && e.key === 'Enter') ACTIONS.addProj($('[data-act=addProj]'));
  if (e.key === 'Escape' && $('#modalRoot').children.length) {
    e.preventDefault();
    // The shell also handles Escape on window. Closing a nested editor must
    // not let that same key press close the API library underneath it.
    e.stopPropagation();
    void closeLocalEditor();
  }
});

/* ---------------- live stream ---------------- */

let bootApprovalChanges = null;
let sessionRenderFrame = 0, sessionRenderDirty = null;
function cancelSessionRender() {
  if (sessionRenderFrame && typeof cancelAnimationFrame === 'function') cancelAnimationFrame(sessionRenderFrame);
  sessionRenderFrame = 0; sessionRenderDirty = null;
}
function scheduleSessionRender(list, detail) {
  if (S.route !== 'sessions') return;
  const owner = S.routeRevision;
  if (!sessionRenderDirty || sessionRenderDirty.owner !== owner) sessionRenderDirty = { owner, list: false, detail: false };
  sessionRenderDirty.list ||= list; sessionRenderDirty.detail ||= detail;
  if (typeof document !== 'undefined' && document.hidden || sessionRenderFrame) return;
  const paint = () => {
    sessionRenderFrame = 0;
    const dirty = sessionRenderDirty; sessionRenderDirty = null;
    if (!dirty || S.route !== 'sessions' || S.routeRevision !== dirty.owner) return;
    if (dirty.list) renderSessionList();
    if (dirty.detail) renderDetail(false);
  };
  if (typeof requestAnimationFrame === 'function') sessionRenderFrame = requestAnimationFrame(paint);
  else paint();
}
if (typeof document !== 'undefined') document.addEventListener('visibilitychange', () => {
  if (document.hidden && sessionRenderFrame) { cancelAnimationFrame(sessionRenderFrame); sessionRenderFrame = 0; }
  else if (!document.hidden && sessionRenderDirty) scheduleSessionRender(false, false);
});
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
      if (typeof homeSessionUpdated === 'function') homeSessionUpdated(ev.session);
      if (typeof sessionReadUpdated === 'function') sessionReadUpdated(ev.session);
      if (S.route === 'sessions') scheduleSessionRender(true, false);
    }
    if (S.route === 'sessions' && S.openKey && ev.sessionKey === S.openKey) {
      if (typeof SESSION_READS !== 'undefined' && SESSION_READS.detailChanges?.key === S.openKey) SESSION_READS.detailChanges.events.push(ev);
      tlApply(ev);
      if (S.openInfo?.sessionKey === S.openKey) scheduleSessionRender(false, true);
    }
  });
  es.onerror = () => {
    setConn({ state: 'connecting', error: '与本机程序的连接断开', consoleDisconnected: true });
    // Agent configuration remains valid when the local status stream drops.
  };
}

function boot() {
  window.addEventListener('hashchange', route);
  $$('[data-icon]').forEach((el) => { el.innerHTML = icon(el.dataset.icon); });
  const approvalChanges = new Map();
  bootApprovalChanges = approvalChanges;
  S.approvalsLoadState = 'loading';
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
    S.approvalsLoadState = 'ready';
    setApprovalBadge();
    renderApprovalsBox();
  }).catch(() => {
    S.approvalsLoadState = 'error';
    setApprovalBadge();
  }).finally(() => { if (bootApprovalChanges === approvalChanges) bootApprovalChanges = null; });
}
// Boot after every page script (remote.js, workbench.js) has registered its renderers.
if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', boot); else setTimeout(boot, 0);
