'use strict';
/* Remote coding pages for the desktop console: 手机远程 (station, QR pairing, API for remote
   tasks, project folders) and 对话 (Codex-style thread list and thread view).
   Loaded after app.js; reuses its helpers (api, esc, icon, toast, busy, ACTIONS, S, route). */

PAGES.login.title = '手机远程';
PAGES.login.sub = '连接中转站，扫码绑定手机，在手机上继续这台电脑上的 Codex / Claude Code 对话';
PAGES.sessions.title = '对话';
PAGES.sessions.sub = '这台电脑上的 Codex 与 Claude Code 对话，和手机看到的一样';
PAGES.projects.title = '手机远程';

Object.assign(ICONS, {
  laptop: '<rect x="4" y="5" width="16" height="11" rx="2"/><path d="M2 19h20"/>',
  scan: '<path d="M4 8V5a1 1 0 0 1 1-1h3M16 4h3a1 1 0 0 1 1 1v3M20 16v3a1 1 0 0 1-1 1h-3M8 20H5a1 1 0 0 1-1-1v-3M7 12h10"/>',
  code: '<path d="m8 8-4 4 4 4M16 8l4 4-4 4M13.5 5l-3 14"/>',
  list: '<path d="M9 6h11M9 12h11M9 18h11"/><path d="m4 6 1 1 2-2M4 12l1 1 2-2M4 18l1 1 2-2"/>',
  bulb: '<path d="M9 18h6M10 21h4M12 3a6 6 0 0 0-3.5 10.9c.6.5 1 1.2 1 2.1h5c0-.9.4-1.6 1-2.1A6 6 0 0 0 12 3z"/>',
  arrowUp: '<path d="M12 19V5M6 11l6-6 6 6"/>',
});

const RM = { tool: 'codex', query: '', expanded: {}, newThread: false, agents: null, projects: [], qrTimer: null, poll: null, model: '', effort: '' };
const AGENT_INFO = { codex: { name: 'Codex', badge: '>_', detail: 'Codex App、CLI 与 IDE 的全部对话' }, claude: { name: 'Claude Code', badge: 'CC', detail: 'Claude Code 会话' } };
const host = (u) => { try { return new URL(u).host; } catch (_) { return u || ''; } };
const projectOf = (cwd) => String(cwd || '').replace(/[\\/]+$/, '').split(/[\\/]/).pop() || '未关联项目';

/* ---------------- 手机远程 ---------------- */

function rmHubState() { return (S.state && S.state.hub && S.state.hub.state) || 'not_logged_in'; }

function rmPairStatus(st) {
  const pair = st?.pairing;
  if (!pair || pair.deviceId !== S.state?.config?.deviceId) return;
  const consumed = Boolean(S.pairQR && pair.pendingExpiresAt === 0);
  if (pair.pendingExpiresAt === 0) {
    S.pairQR = ''; S.pairExpires = 0; clearInterval(RM.qrTimer);
    const box = S.route === 'login' ? $('#pairQR') : null;
    if (box) {
      box.classList.remove('live');
      box.innerHTML = pair.paired ? `${icon('check')}<span>手机已绑定</span>`
        : '<button class="btn primary sm" data-act="rmShowQR">' + icon('scan') + '显示二维码</button>';
    }
  }
  if (consumed && pair.paired) toast('手机绑定成功', 'ok');
}

RENDER_login = async function (view) {
  const st = await loadState();
  if (view.isConnected === false) return;
  const c = st.config, state = rmHubState();
  const linked = c.loggedIn;
  const connected = state === 'connected';
  clearInterval(RM.poll);
  if (linked && !connected) RM.poll = setInterval(() => { if (S.route !== 'login') { clearInterval(RM.poll); return; } loadState().then(() => { if (rmHubState() === 'connected') route(); }).catch(() => undefined); }, 2500);
  const station = host(c.relayRoot || c.effectiveHubUrl);
  const qrLive = S.pairQR && S.pairExpires > Date.now();
  view.innerHTML = `<div class="rm-page">
    ${linked ? `<section class="rm-sec">
        <div class="rm-line">
          <span class="rm-dot ${connected ? 'ok' : state === 'invalid_key' ? 'bad' : 'wait'}"></span>
          <div class="rm-grow"><div class="rm-device-name" id="devName">${S.editingName
            ? `<input class="input" id="devInput" value="${esc(c.deviceName)}" maxlength="40"><button class="btn sm primary" data-act="saveName">保存</button><button class="btn sm ghost" data-act="cancelName">取消</button>`
            : `${esc(c.deviceName)}<button class="icon-btn" data-act="editName" title="修改电脑名称">${icon('edit')}</button>`}</div>
            <div class="rm-meta">${connected ? '已连接' : state === 'invalid_key' ? '站点拒绝了这台电脑' : '正在连接'} · ${esc(station)}</div></div>
          <button class="rm-link" data-act="rmDisconnect">断开</button>
        </div>${!connected && st.hub && st.hub.error ? `<div class="rm-error">${esc(st.hub.error)}</div>` : ''}
      </section>`
    : `<section class="rm-sec rm-hero">
        <h2>连接中转站</h2>
        <p>手机通过中转站继续这台电脑上的 Codex、Claude Code 对话，任务仍在电脑上运行。</p>
        <div class="rm-form"><input class="input" id="rmStation" placeholder="例如 https://salcara.top" value="${esc(c.relayRoot || '')}" autocomplete="off">
          <button class="btn primary" data-act="rmConnect">连接</button></div>
        <details class="rm-adv"><summary>插件不在默认路径？</summary><input class="input" id="rmHub" placeholder="默认：中转站地址/salcara-hub" value="${esc(c.hubUrl || '')}"></details>
        <div id="rmConnectError"></div>
      </section>`}

    <section class="rm-sec ${connected ? '' : 'off'}">
      <div class="rm-sec-head"><h3>绑定手机</h3>${connected ? `<button class="rm-link" data-act="pairRevoke">解除绑定</button>` : ''}</div>
      <div class="rm-pair">
        <div class="rm-qr ${qrLive ? 'live' : ''}" id="pairQR">${qrLive
          ? `<img src="${esc(S.pairQR)}" alt="手机绑定二维码"><span class="rm-timer" id="rmTimer"></span>`
          : connected ? st.hub?.pairing?.paired ? `${icon('check')}<span>手机已绑定</span>` : `<button class="btn primary sm" data-act="rmShowQR">${icon('scan')}显示二维码</button>` : '<span>先连接中转站</span>'}</div>
        <ol class="rm-steps">
          <li>手机打开 Salcara →「编程」→「我的电脑」</li>
          <li>点「绑定新电脑」扫码</li>
          <li>选好 Agent 和 API，点「连接」</li>
          <li class="rm-tip">二维码 5 分钟有效，只能用一次</li>
        </ol>
      </div>
    </section>

    <section class="rm-sec">
      <div class="rm-sec-head"><h3>项目文件夹</h3><span class="rm-sub">手机可以在这里新建对话</span></div>
      <div class="rm-projects">${(c.projects || []).map((p) => `<div class="rm-project">${icon('folder')}<div class="rm-grow"><div>${esc(p.name)}</div><div class="path">${esc(p.path)}</div></div><button class="rm-link" data-act="rmProj" data-path="${esc(p.path)}">移除</button></div>`).join('')}</div>
      <div class="rm-add"><input class="input" id="projPath" placeholder="${st.os === 'windows' ? 'C:\\code\\my-app' : esc((st.home || '~') + '/code/my-app')}"><button class="btn" data-act="browse">浏览…</button><button class="btn primary" data-act="addProj">添加</button></div>
    </section>
    <div id="rmLive"></div>
    <p class="rm-note">API Key 始终留在这台电脑上，中转站只转发对话。</p>
  </div>`;
  rmTickQR();
  rmLoadAgents();
  rmLoadLive();
};

const LIVE_PROMPT = '请开启 Salcara 实时桌面模式：用你的工具列出本机最近的 Codex 对话（最多 200 个，排除当前这个对话），然后调用 salcara_desktop_connect，参数 allowRemoteControl 为 true，durationSeconds 为 2592000，sessionKeys 为这些对话的 codex:真实对话ID 数组。保持这次调用一直运行，不要在这个对话里做别的事。';

async function rmLoadLive() {
  // The Codex live-desktop plugin is retired; only offer to remove it if it is still installed.
  const box = document.getElementById('rmLive');
  if (!box) return;
  let st = null;
  try { st = (await api('/api/local/desktop-companion/uninstall')).status; } catch (_) { /* unknown */ }
  box.innerHTML = st && st.installed ? `<section class="rm-sec"><div class="rm-sec-head"><h3>旧版 Codex 桌面插件</h3><button class="rm-link" data-act="rmLiveUninstall">卸载</button></div><p class="rm-sub">已不再需要，建议卸载。</p></section>` : '';
}

Object.assign(ACTIONS, {
  rmLiveCopy: () => copyText(LIVE_PROMPT, '开启指令'),
  rmLiveInstall: async (b) => {
    if (!confirm('安装 Codex 桌面插件？\n\n会先备份 Codex 配置，再添加 Salcara 桌面插件；不改 API、对话和项目，不会关闭 Codex。安装后需要重启一次 Codex。')) return;
    await busy(b, async () => {
      try { const r = await api('/api/local/desktop-companion/install', { confirmed: true }); toast((r.status && r.status.message) || '已安装，重启一次 Codex 后开启', 'ok'); }
      catch (e) { toast(e.message, 'bad'); }
    });
    rmLoadLive();
  },
  rmLiveUninstall: async (b) => {
    if (!confirm('卸载 Codex 桌面插件？只移除 Salcara 添加的那一段配置，其他设置、对话不受影响。')) return;
    await busy(b, async () => {
      try { await api('/api/local/desktop-companion/uninstall', { confirmed: true }); toast('已卸载；重启 Codex 后插件完全停止', 'ok'); }
      catch (e) { toast(e.message, 'bad'); }
    });
    rmLoadLive();
  },
  rmLiveStop: async (b) => {
    if (!confirm('结束实时桌面模式？正在执行的任务不会停止，之后手机改用后台方式继续。')) return;
    await busy(b, async () => {
      try { await api('/api/local/desktop-companion/disconnect', { confirmed: true }); toast('已结束实时桌面模式', 'ok'); }
      catch (e) { toast(e.message, 'bad'); }
    });
    rmLoadLive();
  },
});
RENDER_projects = (view) => RENDER_login(view);

function rmTickQR() {
  clearInterval(RM.qrTimer);
  const tick = () => {
    const el = document.getElementById('rmTimer');
    if (!el) { clearInterval(RM.qrTimer); return; }
    const left = Math.max(0, Math.round((S.pairExpires - Date.now()) / 1000));
    if (!left) { S.pairQR = ''; clearInterval(RM.qrTimer); if (S.route === 'login') route(); return; }
    el.textContent = `${Math.floor(left / 60)}:${String(left % 60).padStart(2, '0')} 后失效`;
  };
  if (S.pairQR) { tick(); RM.qrTimer = setInterval(tick, 1000); }
}

async function rmLoadAgents() {
  const box = document.getElementById('rmAgents');
  if (!box) return;
  try {
    RM.agents = await api('/api/remote/agents');
  } catch (e) { box.innerHTML = `<div class="rm-error" style="margin:12px">${esc(e.message)}</div>`; return; }
  const apis = RM.agents.apis || [];
  box.innerHTML = ['codex', 'claude'].map((id) => {
    const a = (RM.agents.agents || []).find((x) => x.id === id) || { api: {} };
    const phone = a.api.source === 'phone';
    const current = a.api.source === 'tool' ? `${AGENT_INFO[id].name} 自己的登录` : (a.api.name || '电脑上的 API') + (a.api.model ? ' · ' + a.api.model : '');
    const chosen = apis.find((x) => x.id === a.api.accountId);
    return `<div class="rm-agent" data-agent="${id}">
      <span class="rm-badge ${id}">${AGENT_INFO[id].badge}</span>
      <div><div class="rm-agent-name">${AGENT_INFO[id].name}${a.available ? '' : ' <span class="chip">未安装</span>'}</div><div class="rm-agent-sub">${phone ? '远程任务：' : '跟随电脑：'}${esc(current)}</div></div>
      <select class="input" data-rm-api="${id}"><option value="">跟随电脑设置</option>${apis.map((x) => `<option value="${esc(x.id)}" ${phone && x.id === a.api.accountId ? 'selected' : ''}>${esc(x.name)}</option>`).join('')}</select>
      <input class="input" data-rm-model="${id}" list="rmModels-${id}" placeholder="模型（可选）" value="${esc(phone ? a.api.model : '')}" ${phone ? '' : 'disabled'}>
      <datalist id="rmModels-${id}">${((chosen && chosen.models) || []).map((m) => `<option value="${esc(m)}">`).join('')}</datalist>
      <button class="rm-btn sm soft" data-act="rmSaveApi" data-agent="${id}">保存</button>
    </div>`;
  }).join('') + (apis.length ? '' : '<div class="rm-note" style="padding:12px">API 密钥库是空的；在「API」页添加后可在这里为远程任务单独选择。</div>');
}

document.addEventListener('change', (e) => {
  const id = e.target.dataset && e.target.dataset.rmApi;
  if (!id || !RM.agents) return;
  const model = document.querySelector(`[data-rm-model="${id}"]`);
  const list = document.getElementById('rmModels-' + id);
  const chosen = (RM.agents.apis || []).find((x) => x.id === e.target.value);
  if (model) { model.disabled = !e.target.value; if (!e.target.value) model.value = ''; }
  if (list) list.innerHTML = ((chosen && chosen.models) || []).map((m) => `<option value="${esc(m)}">`).join('');
});

Object.assign(ACTIONS, {
  rmConnect: async (b) => {
    const out = document.getElementById('rmConnectError');
    if (out) out.innerHTML = '';
    await busy(b, async () => {
      try {
        const body = { url: document.getElementById('rmStation').value.trim(), hubUrl: (document.getElementById('rmHub') || {}).value || '' };
        if (!body.url) throw new Error('请输入中转站地址');
        await api('/api/remote/discover', body);
        await api('/api/remote/discover', Object.assign({ connect: true }, body));
        S.pairQR = ''; S.pairExpires = 0;
        toast('已连接，接下来用手机扫码', 'ok');
        await route();
      } catch (e) { if (out) out.innerHTML = `<div class="rm-error">${esc(e.message)}</div>`; }
    });
  },
  rmDisconnect: async () => {
    if (!confirm('断开后手机将连不上这台电脑（绑定保留，重新连接即可恢复）。确定吗？')) return;
    await api('/api/logout', {});
    S.pairQR = ''; S.pairExpires = 0;
    route();
  },
  rmShowQR: async (b) => { await ACTIONS.pairStart(b); route(); },
  rmSaveApi: async (b) => {
    const id = b.dataset.agent;
    const sel = document.querySelector(`[data-rm-api="${id}"]`);
    const model = document.querySelector(`[data-rm-model="${id}"]`);
    await busy(b, async () => {
      try {
        await api('/api/remote/agents/api', { agent: id, accountId: sel.value, model: sel.value ? model.value.trim() : '' });
        toast(sel.value ? '已保存：手机发起的任务将使用这个 API' : '已改为跟随电脑设置', 'ok');
        rmLoadAgents();
      } catch (e) { toast(e.message, 'bad'); }
    });
  },
  rmProj: async (b) => {
    if (!confirm('移除后手机不能再在这个文件夹里新建对话。确定吗？')) return;
    await api('/api/projects/remove', { path: b.dataset.path });
    route();
  },
});

/* ---------------- 对话 ---------------- */

RENDER_sessions = async function (view) {
  $('#headActions').innerHTML = `<button class="btn" data-act="reloadSessions">${icon('refresh')}刷新</button>`;
  if (S.sessFilter === 'claude') RM.tool = 'claude';
  view.innerHTML = `<div class="cx">
    <aside class="cx-side">
      <div class="cx-side-top">
        <div class="cx-seg">${['codex', 'claude'].map((t) => `<button class="${RM.tool === t ? 'on' : ''}" data-act="cxTool" data-t="${t}">${AGENT_INFO[t].name}</button>`).join('')}</div>
        <button class="cx-new" data-act="cxNew"><span class="plus">${icon('plus')}</span>新对话</button>
        <input class="cx-search" id="cxSearch" placeholder="搜索对话" value="${esc(RM.query)}">
      </div>
      <div class="cx-list" id="sessList"><div class="empty"><span class="spin"></span></div></div>
    </aside>
    <section class="cx-main" id="sessDetail"></section>
  </div>`;
  renderDetail(true);
  await loadSessions();
  if (S.openKey) openSession(S.openKey, true);
};

async function loadSessions() {
  try {
    const r = await api('/api/sessions');
    S.sessions = r.sessions || [];
  } catch (e) {
    const el = $('#sessList');
    if (el) el.innerHTML = `<div class="rm-error" style="margin:8px">${esc(e.message)}</div>`;
    return;
  }
  renderSessionList();
}

function renderSessionList() {
  const el = $('#sessList');
  if (!el) return;
  const q = RM.query.toLowerCase().trim();
  const pending = new Set(S.approvals.map((a) => a.sessionKey));
  const list = S.sessions.filter((s) => s.tool === RM.tool && (!q || [s.title, s.cwd, s.model].some((v) => String(v || '').toLowerCase().includes(q))));
  if (!list.length) { el.innerHTML = `<div class="empty">${q ? '没有匹配的对话' : `还没有 ${AGENT_INFO[RM.tool].name} 对话`}</div>`; return; }
  const groups = new Map();
  list.slice().sort((a, b) => b.updatedAt - a.updatedAt).forEach((s) => {
    const k = (s.cwd || '').replace(/[\\/]+$/, '').toLowerCase() || '__none';
    if (!groups.has(k)) groups.set(k, { k, name: s.cwd ? projectOf(s.cwd) : '未关联项目', items: [] });
    groups.get(k).items.push(s);
  });
  el.innerHTML = [...groups.values()].map((g) => {
    const shown = q || RM.expanded[g.k] ? g.items : g.items.slice(0, 8);
    return `<div class="cx-proj">${icon('folder')}<span>${esc(g.name)}</span></div>${shown.map((s) => {
      const lead = pending.has(s.sessionKey) ? '<span class="rm-dot" style="background:var(--rm-warn)"></span>' : s.status === 'running' ? '<span class="rm-dot wait" style="background:var(--rm-blue)"></span>' : '';
      return `<div class="cx-row ${s.sessionKey === S.openKey && !RM.newThread ? 'sel' : ''}" data-act="openSess" data-k="${esc(s.sessionKey)}" title="${esc(s.title || '')}"><span class="lead">${lead}</span><span class="t">${esc(s.title || '无标题对话')}</span><span class="time">${pending.has(s.sessionKey) ? '待批准' : s.status === 'running' ? '运行中' : esc(relTime(s.updatedAt))}</span></div>`;
    }).join('')}${!q && g.items.length > 8 ? `<button class="cx-more" data-act="cxMore" data-k="${esc(g.k)}">${RM.expanded[g.k] ? '收起' : `显示全部 ${g.items.length} 个`}</button>` : ''}`;
  }).join('');
}

function cxDiff(d) { return `<div class="cx-diff">${renderDiff(d)}</div>`; }
function cxStats(d) {
  let add = 0, del = 0;
  String(d || '').split('\n').forEach((l) => { if (l.startsWith('+') && !l.startsWith('+++')) add++; else if (l.startsWith('-') && !l.startsWith('---')) del++; });
  return { add, del };
}
function cxFiles(ev) {
  const diff = ev.diff || '';
  const parts = diff.split(/(?=^diff --git |^--- (?:a\/|\/dev\/null))/m).filter((p) => /^\+\+\+ /m.test(p));
  const one = (part, fallback) => { const m = part.match(/^\+\+\+ (?:b\/)?(.+)$/m); return Object.assign({ path: (m ? m[1] : fallback).trim(), diff: part }, cxStats(part)); };
  return parts.length > 1 ? parts.map((p) => one(p, ev.title)) : [one(diff, ev.detail || ev.title || '')];
}

function cxBlocks(running) {
  const blocks = []; let work = [];
  const flush = (live) => { if (work.length) blocks.push({ type: 'work', items: work, live }); work = []; };
  S.tl.forEach((ev) => {
    if (ev.type === 'message') { flush(false); blocks.push({ type: ev.role === 'user' ? 'user' : 'bot', ev }); }
    else if (ev.type === 'turn') { flush(false); if (ev.status === 'failed' || ev.status === 'interrupted') blocks.push({ type: 'turn', ev }); }
    else if (ev.type === 'notice' && ev.level === 'error') { flush(false); blocks.push({ type: 'notice', ev }); }
    else if (ev.type === 'approval.request' && !ev.resolved) { /* shown above the composer */ }
    else if (ev.type === 'reasoning' || ev.type === 'tool' || ev.type === 'notice' || ev.type === 'approval.request' || ev.type === 'approval.resolved') work.push(ev);
  });
  flush(running);
  return blocks;
}

function cxSummary(items) {
  const tools = items.filter((e) => e.type === 'tool');
  const n = (k) => tools.filter((e) => k.includes(e.kind)).length;
  const files = new Set(tools.filter((e) => e.kind === 'file_change').flatMap((e) => cxFiles(e).map((f) => f.path))).size;
  const parts = [n(['command']) && `运行了 ${n(['command'])} 条命令`, files && `编辑了 ${files} 个文件`, n(['read', 'search']) && `查看了 ${n(['read', 'search'])} 处`, n(['web']) && `搜索网页 ${n(['web'])} 次`, n(['mcp', 'other']) && `调用了 ${n(['mcp', 'other'])} 个工具`].filter(Boolean);
  return parts.length ? parts.join(' · ') : items.some((e) => e.type === 'reasoning') ? '思考过程' : '过程';
}

function cxCurrent(items) {
  for (let i = items.length - 1; i >= 0; i--) {
    const e = items[i];
    if (e.type === 'tool' && e.status === 'running') return e.title;
    if (e.type === 'reasoning' && !e.final) return '思考：' + (String(e.text || '').trim().split('\n')[0] || '').replace(/\*\*/g, '');
  }
  return '正在处理';
}

function cxWork(b) {
  const files = b.items.filter((e) => e.type === 'tool' && e.kind === 'file_change').flatMap(cxFiles);
  const steps = b.items.map((e) => {
    if (e.type === 'reasoning') return e.text ? `<div class="cx-step reason">${icon('bulb')}<span>${esc(String(e.text).replace(/\*\*/g, '').trim())}</span></div>` : '';
    if (e.type === 'notice') return `<div class="cx-step">${icon('info')}<span>${esc(e.text)}</span></div>`;
    if (e.type === 'approval.request' || e.type === 'approval.resolved') {
      const r = e.resolved || (e.type === 'approval.resolved' ? e : null);
      return `<div class="cx-step">${icon('check')}<span>${r && r.decision === 'deny' ? '已拒绝' : '已允许'}：${esc(e.title || '操作')}</span></div>`;
    }
    if (e.kind === 'file_change') return '';
    const body = (e.detail && e.detail !== e.title ? (e.kind === 'command' ? '$ ' : '') + e.detail + '\n' : '') + (e.output || '');
    const status = e.status === 'running' ? '<span class="spin" style="width:11px;height:11px"></span>' : e.status === 'failed' ? '<span class="chip bad">失败</span>' : '';
    return `<div class="cx-step">${icon(KIND_ICON[e.kind] || 'box')}${body.trim() ? `<details><summary>${esc(e.title || e.kind)} ${status}</summary><div class="cx-out">${esc(body.trim())}</div></details>` : `<span>${esc(e.title || e.kind)}</span>${status}`}</div>`;
  }).join('');
  return `<details class="cx-work"><summary>${b.live ? '<span class="spin" style="width:12px;height:12px"></span>' : icon('list')}<span>${esc(b.live ? cxCurrent(b.items) : cxSummary(b.items))}</span></summary><div class="cx-steps">${steps}</div></details>
    ${files.length ? `<div class="cx-files">${files.map((f) => `<details class="cx-file"><summary>${icon('file')}<span style="flex:1">${esc(f.path.split(/[\\/]/).pop() || f.path)}</span>${f.add ? `<span class="add">+${f.add}</span>` : ''}${f.del ? `<span class="del">−${f.del}</span>` : ''}</summary>${f.diff ? cxDiff(f.diff) : ''}</details>`).join('')}</div>` : ''}`;
}

function renderDetail(scroll) {
  const el = $('#sessDetail');
  if (!el) return;
  if (RM.newThread) { cxRenderNew(el); return; }
  if (!S.openKey) {
    el.innerHTML = `<div class="cx-empty"><div class="rm-hero-icon" style="margin:0 auto">${icon('code')}</div><h3>选择一个对话</h3><div>或者新建一个，让 ${AGENT_INFO[RM.tool].name} 在电脑上开始工作</div></div>`;
    return;
  }
  const s = S.openInfo || {};
  const running = s.status === 'running' || s.status === 'waiting_approval';
  const pending = S.tl.filter((e) => e.type === 'approval.request' && !e.resolved);
  const thread = $('#cxThread');
  const atBottom = !thread || thread.scrollHeight - thread.scrollTop - thread.clientHeight < 80;
  const draft = ($('#sendText') || {}).value || '';
  const blocks = cxBlocks(s.status === 'running');
  const last = blocks[blocks.length - 1];
  const thinking = s.status === 'running' && !(last && last.type === 'work' && last.live) && !(last && last.type === 'bot' && !last.ev.final);
  el.innerHTML = `<div class="cx-head"><div style="flex:1;min-width:0"><div class="title">${esc(s.title || '对话')}</div>
      <div class="meta">${esc([s.cwd ? projectOf(s.cwd) : '', s.client, s.model].filter(Boolean).join(' · '))}</div></div>
      ${s.tool === 'codex' && s.sessionKey ? `<button class="btn sm" data-act="cxNavigate" title="让 Codex 桌面端跳到这个对话">${icon('laptop')}在 Codex 中打开</button>` : ''}
      ${s.cwd && s.sessionKey && !running ? `<button class="btn sm" data-act="resumeSession" data-k="${esc(s.sessionKey)}" data-target="${esc(s.tool)}" title="在终端里用原工具打开同一个对话">${icon('terminal')}终端打开</button>` : ''}</div>
    <div class="cx-thread" id="cxThread"><div class="cx-col">${blocks.map((b) => b.type === 'user' ? `<div class="cx-user">${esc(b.ev.text)}</div>`
      : b.type === 'bot' ? `<div class="cx-bot">${renderText(b.ev.text)}</div>`
        : b.type === 'work' ? cxWork(b)
          : b.type === 'turn' ? `<div class="cx-notice">${b.ev.status === 'failed' ? '出错了' + (b.ev.error ? '：' + esc(b.ev.error) : '') : '已停止'}</div>`
            : `<div class="cx-notice">${esc(b.ev.text)}</div>`).join('') || '<div class="empty">还没有内容</div>'}
      ${thinking ? '<div class="cx-live"><span class="spin" style="width:12px;height:12px"></span>正在处理</div>' : ''}</div></div>
    ${pending.length ? `<div class="cx-composer-wrap" style="padding-bottom:0"><div class="cx-col" style="padding:0"><div class="cx-appr"><div class="k">${esc(AGENT_INFO[s.tool] ? AGENT_INFO[s.tool].name : s.tool)} 请求批准${pending.length > 1 ? ` · 还有 ${pending.length - 1} 个` : ''}</div><div class="t">${esc(pending[0].title || '需要批准')}</div>
      ${pending[0].detail ? `<pre>${esc(pending[0].detail)}</pre>` : ''}${pending[0].diff ? cxDiff(pending[0].diff) : ''}
      <div class="row"><button class="btn sm primary" data-act="approve" data-id="${esc(pending[0].approvalId)}" data-d="allow">允许</button><button class="btn sm" data-act="approve" data-id="${esc(pending[0].approvalId)}" data-d="allow_session">本对话都允许</button><button class="btn sm danger" data-act="approve" data-id="${esc(pending[0].approvalId)}" data-d="deny">拒绝</button></div></div></div></div>` : ''}
    <div class="cx-composer-wrap"><div class="cx-composer">
      <textarea id="sendText" rows="2" placeholder="${s.controllable === false && running ? '这个对话正在别处运行，结束后可继续' : running ? '补充说明，完成后接着做' : '继续对话'}（${OSX() === 'darwin' ? '⌘' : 'Ctrl+'}Enter 发送）">${esc(draft)}</textarea>
      <div class="cx-bar">${s.tool === 'codex' ? `<select class="cx-chip" id="cxEffort" title="推理强度"><option value="">推理：默认</option>${[['low', '低'], ['medium', '中'], ['high', '高']].map(([v, l]) => `<option value="${v}" ${RM.effort === v ? 'selected' : ''}>推理：${l}</option>`).join('')}</select>` : ''}
        <span class="grow"></span>
        ${running && s.controllable !== false ? `<button class="cx-send stop" data-act="interrupt" title="停止">${icon('stop')}</button>` : ''}
        <button class="cx-send" data-act="send" title="发送">${icon('arrowUp')}</button></div>
    </div></div>`;
  const t = $('#cxThread');
  if (t && (scroll || atBottom)) t.scrollTop = t.scrollHeight;
}

function cxRenderNew(el) {
  el.innerHTML = `<div class="cx-head"><div style="flex:1"><div class="title">新对话</div><div class="meta">${AGENT_INFO[RM.tool].name} 会在电脑上的项目里工作</div></div></div>
    <div class="cx-thread"><div class="cx-empty"><div class="rm-hero-icon" style="margin:0 auto">${icon('code')}</div><h3>要构建什么？</h3>
      <select class="input" id="cxProject" style="max-width:420px;margin:10px auto 0">${RM.projects.map((p) => `<option value="${esc(p.path)}">${esc(p.name || projectOf(p.path))} — ${esc(p.path)}</option>`).join('') || '<option value="">先在「手机远程」添加项目文件夹</option>'}</select></div></div>
    <div class="cx-composer-wrap"><div class="cx-composer"><textarea id="cxPrompt" rows="3" placeholder="描述要做的事（${OSX() === 'darwin' ? '⌘' : 'Ctrl+'}Enter 发送）"></textarea>
      <div class="cx-bar"><select class="cx-chip" id="cxMode"><option value="ask">每一步都问我</option><option value="auto_edits">自动改文件</option><option value="auto_all">全部自动</option></select><span class="grow"></span>
      <button class="cx-send" data-act="cxStart" title="开始">${icon('arrowUp')}</button></div></div></div>`;
  const p = document.getElementById('cxPrompt'); if (p) p.focus();
}

async function openSession(key, keepScroll) {
  RM.newThread = false;
  S.openKey = key;
  renderSessionList();
  const el = $('#sessDetail');
  if (el && !keepScroll) el.innerHTML = '<div class="cx-empty"><span class="spin"></span></div>';
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
    if (el) el.innerHTML = `<div class="rm-error" style="margin:20px">${esc(e.message)}</div>`;
  }
}

Object.assign(ACTIONS, {
  cxTool: (b) => { RM.tool = b.dataset.t; S.sessFilter = RM.tool; $$('.cx-seg button').forEach((x) => x.classList.toggle('on', x === b)); renderSessionList(); if (RM.newThread) renderDetail(); },
  cxNavigate: async (b) => {
    await busy(b, async () => {
      try { await api('/api/session/navigate', { sessionKey: S.openKey }); toast('已让 Codex 打开这个对话；内容没更新的话，重启一次 Codex', 'ok'); }
      catch (e) { toast(e.message, 'bad'); }
    });
  },
  cxMore: (b) => { RM.expanded[b.dataset.k] = !RM.expanded[b.dataset.k]; renderSessionList(); },
  cxNew: async () => {
    RM.newThread = true; S.openKey = null; renderSessionList(); renderDetail();
    try { RM.projects = (await api('/api/remote/projects')).projects || []; } catch (_) { RM.projects = (S.state && S.state.config && S.state.config.projects) || []; }
    if (RM.newThread) renderDetail();
  },
  cxStart: async (b) => {
    const prompt = ($('#cxPrompt') || {}).value || '';
    const cwd = ($('#cxProject') || {}).value || '';
    if (!prompt.trim()) return;
    if (!cwd) { toast('先选择项目文件夹', 'bad'); return; }
    await busy(b, async () => {
      try {
        const r = await api('/api/session/start', { tool: RM.tool, cwd, prompt: prompt.trim(), approval: ($('#cxMode') || {}).value || 'ask' });
        RM.newThread = false;
        await loadSessions();
        await openSession(r.sessionKey);
      } catch (e) { toast(e.message, 'bad'); }
    });
  },
  send: async (b) => {
    const t = $('#sendText');
    const text = t ? t.value.trim() : '';
    if (!text) return;
    RM.effort = ($('#cxEffort') || {}).value || '';
    await busy(b, async () => {
      try {
        await api('/api/session/send', Object.assign({ sessionKey: S.openKey, text }, RM.effort ? { effort: RM.effort } : {}));
        t.value = '';
        if (S.openInfo) S.openInfo.status = 'running';
        renderDetail(true);
      } catch (e) { toast(e.message, 'bad'); }
    });
  },
});

document.addEventListener('input', (e) => {
  if (e.target.id === 'cxSearch') { RM.query = e.target.value; renderSessionList(); }
});
document.addEventListener('keydown', (e) => {
  if (e.target.id === 'cxPrompt' && e.key === 'Enter' && (e.ctrlKey || e.metaKey)) { e.preventDefault(); ACTIONS.cxStart($('[data-act=cxStart]')); }
  if (e.target.id === 'rmStation' && e.key === 'Enter') ACTIONS.rmConnect($('[data-act=rmConnect]'));
});
