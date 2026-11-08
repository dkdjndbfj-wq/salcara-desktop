'use strict';
/* Remote coding pages for the desktop console: 手机远程 (station, QR pairing, API for remote
   tasks, project folders) and 对话 (Codex-style thread list and thread view).
   Loaded after app.js; reuses its helpers (api, esc, icon, toast, busy, ACTIONS, S, route). */

PAGES.login.title = '手机远程';
PAGES.login.sub = '扫码绑定手机，远程使用这台电脑上的 Agent';
PAGES.sessions.title = '对话';
PAGES.sessions.sub = '查看和继续电脑、手机发起的 Agent 对话';
PAGES.projects.title = '手机远程';

Object.assign(ICONS, {
  laptop: '<rect x="4" y="5" width="16" height="11" rx="2"/><path d="M2 19h20"/>',
  scan: '<path d="M4 8V5a1 1 0 0 1 1-1h3M16 4h3a1 1 0 0 1 1 1v3M20 16v3a1 1 0 0 1-1 1h-3M8 20H5a1 1 0 0 1-1-1v-3M7 12h10"/>',
  code: '<path d="m8 8-4 4 4 4M16 8l4 4-4 4M13.5 5l-3 14"/>',
  list: '<path d="M9 6h11M9 12h11M9 18h11"/><path d="m4 6 1 1 2-2M4 12l1 1 2-2M4 18l1 1 2-2"/>',
  bulb: '<path d="M9 18h6M10 21h4M12 3a6 6 0 0 0-3.5 10.9c.6.5 1 1.2 1 2.1h5c0-.9.4-1.6 1-2.1A6 6 0 0 0 12 3z"/>',
  arrowUp: '<path d="M12 19V5M6 11l6-6 6 6"/>',
  server: '<rect x="4" y="4" width="16" height="6" rx="1.5"/><rect x="4" y="14" width="16" height="6" rx="1.5"/><path d="M8 7h.01M8 17h.01"/>',
  link: '<path d="M9.5 14.5 14.5 9.5M7 17H5.5a3.5 3.5 0 0 1 0-7H9M15 7h1.5a3.5 3.5 0 0 1 0 7H15"/>',
});

const RM = { tool: 'codex', query: '', expanded: {}, newThread: false, newPrompt: '', agents: null, agentsPromise: null, projects: [], archived: null, archivedOpen: false, qrTimer: null, poll: null, model: '', effort: '', loginEpoch: 0, sessionsEpoch: 0, controlEpoch: 0, controls: null, controlPromise: null, newFeatures: null, featureChoice: { mode: '', skills: [] }, sideChain: null, controlPanel: '', remoteMode: 'station', direct: null, directPromise: null, directGeneration: 0, connectionAction: null, connectionRevision: 0, stationDraft: null, botDraft: '' };
const AGENT_INFO = { codex: { name: 'Codex', badge: '>_', detail: 'Codex App、CLI 与 IDE 的全部对话' }, claude: { name: 'Claude Code', badge: 'CC', detail: 'Claude Code 会话' }, 'deepseek-harness': { name: 'DeepSeek Harness', badge: 'DS', detail: 'Bridge 与手机发起的会话' }, hermes: { name: 'Hermes', badge: 'H', detail: 'Bridge 与手机发起的会话' } };
const host = (u) => { try { return new URL(u).host; } catch (_) { return u || ''; } };
const projectOf = (cwd) => String(cwd || '').replace(/[\\/]+$/, '').split(/[\\/]/).pop() || '未关联项目';
const operationId = () => {
  try { if (globalThis.crypto && typeof globalThis.crypto.randomUUID === 'function') return globalThis.crypto.randomUUID(); } catch (_) { /* fallback below */ }
  return 'xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx'.replace(/[xy]/g, (c) => { const r = Math.random() * 16 | 0; return (c === 'x' ? r : (r & 3 | 8)).toString(16); });
};

// All remote connection entry points share one mutation lease. A page change
// cancels only work not yet submitted; a submitted change is reconciled, never
// retried or misreported as cancelled because its original controls disappeared.
function rmBeginConnectionAction(button, mode, fields = [], surface) {
  // An abandoned preflight owns no mutation. A new screen may start its own
  // request without waiting for that obsolete read to time out.
  if (RM.connectionAction && !RM.connectionAction.submitted && !RM.connectionAction.current()) RM.connectionAction = null;
  if (RM.connectionAction || button?.isConnected === false) return null;
  if (!surface && (S.route !== 'login' || RM.remoteMode !== mode)) return null;
  const owner = { mode, route: S.route, routeRevision: S.routeRevision, pageEpoch: RM.loginEpoch,
    view: $('#view'), revision: RM.connectionRevision, submitted: false,
    fields: fields.map(id => ({ id, node: document.getElementById(id), value: String(document.getElementById(id)?.value || '') })), surface };
  owner.current = () => RM.connectionAction === owner && S.route === owner.route && S.routeRevision === owner.routeRevision
    && (!owner.surface ? RM.remoteMode === owner.mode && RM.loginEpoch === owner.pageEpoch && $('#view') === owner.view : owner.surface())
    && RM.connectionRevision === owner.revision && owner.fields.every(field => document.getElementById(field.id) === field.node && field.node?.isConnected !== false && String(field.node?.value || '') === field.value);
  RM.connectionAction = owner;
  return owner;
}
function rmFinishConnectionAction(owner) {
  if (RM.connectionAction !== owner) return;
  RM.connectionAction = null;
  if (S.route !== 'login') return;
  rmUpdateRemoteStatus();
  const direct = document.getElementById('rmDirectPanel');
  if (RM.remoteMode === 'direct' && direct?.isConnected !== false && direct) direct.outerHTML = renderRemoteDirectPanel(false);
}
function rmRememberConnectionDraft() {
  if (S.route !== 'login') return;
  if (RM.remoteMode === 'station' && document.getElementById('rmStation')) {
    RM.stationDraft = { url: document.getElementById('rmStation').value || '', hubUrl: document.getElementById('rmHub')?.value || '', claim: document.getElementById('rmEnrollmentClaim')?.value || '' };
  } else if (RM.remoteMode === 'bot' && document.getElementById('rmBotBinding')) RM.botDraft = document.getElementById('rmBotBinding').value || '';
}
async function rmRefreshAcceptedConnection() {
  try { return await loadState(); }
  catch (_) { return null; } // A refresh failure cannot undo an accepted mutation.
}
function rmConnectionError(owner, node, error) {
  if (!owner.current()) return;
  const unknown = owner.submitted && (!Number.isInteger(error.status) || error.status >= 500 || error.status === 408);
  const message = unknown ? `操作结果暂未确认，请刷新状态核对后再操作。${error.message || ''}` : error.message || '操作失败，请重试';
  if (node?.isConnected !== false && node) node.innerHTML = `<div class="rm-error">${esc(message)}</div>`;
  else toast(message, 'bad');
}

function sessionCapabilities(tool) {
  const item = (RM.agents && RM.agents.agents || []).find((a) => a.id === tool || a.tool === tool);
  return item && item.capabilities ? item.capabilities : {};
}

function loadAgentCatalog() {
  if (RM.agents) return Promise.resolve(RM.agents);
  if (RM.agentsPromise) return RM.agentsPromise;
  const request = api('/api/remote/agents').then((value) => {
    RM.agents = value;
    return value;
  }).finally(() => {
    if (RM.agentsPromise === request) RM.agentsPromise = null;
  });
  RM.agentsPromise = request;
  return request;
}

function controlRequest(body) { return api('/api/session/control', body); }

function currentControl(key = S.openKey) {
  return RM.controls && RM.controls.key === key ? RM.controls : null;
}

function featureChoicesFromDOM() {
  const mode = ($('#cxCollaborationMode') || {}).value || '';
  const skills = $$('.cx-skill-check:checked').map((input) => input.value);
  return { mode, skills };
}

function selectedSkillRefs(features, names) {
  const wanted = new Set(names || []);
  return ((features && features.skills) || []).filter((s) => wanted.has(s.name)).map((s) => ({ name: s.name, path: s.path || '' }));
}

function featureInputs(features) {
  if (!features) return '';
  const modes = (features.collaborationModes || []).filter(Boolean);
  const skills = features.skillsSupported ? (features.skills || []) : [];
  if (!modes.length && !skills.length) return '';
  const mode = RM.featureChoice.mode;
  const selected = RM.featureChoice.skills.filter((name) => skills.some((skill) => skill.name === name)).length;
  return `<span class="cx-feature-group">${modes.length ? `<select class="cx-chip" id="cxCollaborationMode" title="协作模式"><option value="">模式：默认</option>${modes.map((m) => `<option value="${esc(m)}" ${mode === m ? 'selected' : ''}>模式：${esc(m)}</option>`).join('')}</select>` : ''}${skills.length ? `<details class="cx-skill-picker"><summary class="cx-chip">技能${selected ? ` · ${selected}` : ''}</summary><div class="cx-skill-menu">${skills.map((s) => `<label><input class="cx-skill-check" type="checkbox" value="${esc(s.name)}" ${RM.featureChoice.skills.includes(s.name) ? 'checked' : ''}><span>${esc(s.displayName || s.name)}</span></label>`).join('')}</div></details>` : ''}</span>`;
}

function renderSessionNavigation() {
  const el = $('#cxSessionNav');
  if (!el) return;
  el.innerHTML = `<div class="cx-seg">${Object.keys(AGENT_INFO).map((t) => `<button class="${RM.tool === t ? 'on' : ''}" data-act="cxTool" data-t="${t}">${AGENT_INFO[t].name}</button>`).join('')}</div>
    <button class="cx-new" data-act="cxNew"><span class="plus">${icon('plus')}</span>新对话</button>
    ${sessionCapabilities(RM.tool).archivedSessions ? `<button class="cx-archive-toggle" data-act="cxArchived">${RM.archivedOpen ? '收起归档' : '归档会话'}</button>` : ''}
    <input class="cx-search" id="cxSearch" placeholder="搜索对话" value="${esc(RM.query)}">`;
}

/* ---------------- 手机远程 ---------------- */

function rmHubState() { return (S.state && S.state.hub && S.state.hub.state) || 'not_logged_in'; }

const RM_MODE_NAMES = { none: '未启用', direct: '直连', hub: '中转站', bot: 'Bot' };
const rmNormalizeHub = value => String(value || '').trim().replace(/\/+$/, '').replace(/\/v1$/i, '');
function rmSavedConnections(config, mode) {
  const rows = new Map();
  for (const row of config?.remoteConnections || []) {
    if (!row?.hubUrl || ((row.mode === 'bot' || row.botGroupId) ? 'bot' : 'hub') !== mode) continue;
    const key = rmNormalizeHub(row.hubUrl), old = rows.get(key);
    if (!old || (row.support?.checkedAt || 0) >= (old.support?.checkedAt || 0)) rows.set(key, row);
  }
  return [...rows.values()];
}
function rmEffectiveMode(config = S.state?.config) {
  if (!config) return 'none';
  if (['none', 'direct', 'hub', 'bot'].includes(config.effectiveRemoteMode)) return config.effectiveRemoteMode;
  if (config.direct?.enabled || S.state?.direct?.enabled || RM.direct?.enabled) return 'direct';
  if (['none', 'direct', 'hub', 'bot'].includes(config.activeRemoteMode)) return config.activeRemoteMode;
  if (!config.loggedIn) return 'none';
  return rmSavedConnections(config, 'bot').some(row => rmNormalizeHub(row.hubUrl) === rmNormalizeHub(config.effectiveHubUrl || config.hubUrl)) ? 'bot' : 'hub';
}
function rmSelectInitialMode(config) {
  if (RM.modeInitialized || !config) return;
  RM.modeInitialized = true;
  const active = rmEffectiveMode(config);
  RM.remoteMode = active === 'hub' || active === 'none' ? 'station' : active;
}
function rmModeStatusMarkup(config = S.state?.config) {
  const active = rmEffectiveMode(config);
  const detail = active === 'none' ? '选择一种方式启用；已保存的配置可随时恢复。' : '同一时间只启用一种连接方式，切换时保留其他配置。';
  return `<div class="rm-active-mode"><div><strong>当前启用：${RM_MODE_NAMES[active]}</strong><span>${detail}</span></div>${active !== 'none' ? `<button class="rm-link" data-act="rmActivateMode" data-mode="none" ${RM.connectionAction ? 'disabled' : ''}>停用远程</button>` : ''}</div>`;
}
function rmUpdateModeStatus() {
  if (S.route !== 'login') return;
  const banner = document.getElementById('rmActiveMode');
  if (banner) banner.innerHTML = rmModeStatusMarkup();
  const tabs = document.getElementById('headActions');
  if (tabs) tabs.innerHTML = remoteTabMarkup();
}

function rmConnectionLabel(hub) {
  if (hub?.state === 'connected') return '已连接';
  if (hub?.state === 'invalid_key') return '设备登记凭据失效';
  if (hub?.state === 'not_logged_in') return '未连接';
  return hub?.error ? '连接中断，自动重连中' : '正在连接';
}

function rmRecoveryMarkup(hub = {}) {
  if (rmEffectiveMode() === 'direct') return `<div class="rm-sec-head"><h3>直连消息与恢复</h3><button class="rm-link" data-act="rmRecoveryRefresh">${icon('refresh')}刷新状态</button></div>
    <p class="rm-recovery-note">消息在电脑与手机之间直接传输。连接中断时，先查看电脑原会话核对任务进度与结果。</p>
    <div class="rm-mode-actions"><button class="btn sm" data-act="rmRecoverySessions">${icon('list')}查看电脑对话</button><span class="rm-recovery-note">远程使用时请保持电脑开机、联网且不休眠。</span></div>`;
  const recovery = hub.recovery || {};
  const queued = Math.max(0, Number(hub.queued) || 0);
  const labels = { ready: '已就绪', degraded: '保存异常', blocked: '恢复不可用', unavailable: '未启用' };
  const warning = recovery.state === 'blocked'
    ? '本机恢复存储无法读取。请检查磁盘、权限和完整备份后重启电脑端；不要删除恢复目录或反复发送同一任务。'
    : recovery.state === 'degraded'
      ? '最近一次消息保存失败。请保持电脑端运行，检查磁盘空间和权限，待保存恢复后再退出。'
      : recovery.historyRequired ? '部分实时进度需要从原会话补齐。请查看电脑对话核对结果，不要把连接中断当成任务失败。' : '';
  return `<div class="rm-sec-head"><h3>中转消息与恢复</h3><button class="rm-link" data-act="rmRecoveryRefresh">${icon('refresh')}刷新状态</button></div>
    <p class="rm-recovery-note">以下为中转站 / Bot 通道的本机存储与送达记录。</p>
    <dl class="rm-recovery-facts"><div><dt>本机中转消息保存</dt><dd>${esc(labels[recovery.state] || '待检查')}</dd></div><div><dt>待同步到中转站</dt><dd>${queued ? `${queued} 条进度消息` : '暂无待同步消息'}</dd></div><div><dt>最近送达中转站</dt><dd>${hub.lastEventSent ? esc(new Date(hub.lastEventSent).toLocaleString()) : '暂无记录'}</dd></div></dl>
    ${warning ? `<p class="rm-recovery-warning" role="status">${esc(warning)}</p>` : ''}
    <p class="rm-recovery-note">${queued ? '对应中转连接恢复后会自动续传已保存的进度。' : ''}消息同步状态不代表任务是否完成；手机显示“结果未知”时，先核对原会话。</p>
    <div class="rm-mode-actions"><button class="btn sm" data-act="rmRecoverySessions">${icon('list')}查看电脑对话</button><span class="rm-recovery-note">远程使用时请保持电脑开机、联网且不休眠。</span></div>`;
}

// Replace only read-only status surfaces; never erase an enrollment draft,
// device-name edit, QR timer or connection action while state is refreshed.
function rmUpdateRemoteStatus(hub = S.state?.hub) {
  if (S.route !== 'login' || !hub || hub.deviceId && hub.deviceId !== S.state?.config?.deviceId) return;
  rmUpdateModeStatus();
  if (typeof syncRemoteSupport === 'function') syncRemoteSupport();
  const recovery = document.getElementById('rmRecovery');
  if (recovery?.isConnected !== false && recovery) recovery.innerHTML = rmRecoveryMarkup(hub);
  if (RM.remoteMode === 'bot') {
    const saved = document.getElementById('rmSavedBots');
    if (saved) saved.innerHTML = rmSavedBotsMarkup(S.state?.config || {});
  }
  if (RM.remoteMode !== 'station') return;
  const active = rmEffectiveMode() === 'hub';
  const connected = active && hub.state === 'connected';
  const meta = document.getElementById('rmHubConnectionMeta');
  if (meta) meta.textContent = `${active ? rmConnectionLabel(hub) : '已保存，未启用'} · ${host(S.state?.config?.relayRoot || S.state?.config?.effectiveHubUrl)}`;
  const dot = document.getElementById('rmHubConnectionDot');
  if (dot) dot.className = `rm-dot ${!active ? '' : connected ? 'ok' : hub.error ? 'bad' : 'wait'}`;
  const error = document.getElementById('rmHubConnectionError');
  if (error) error.innerHTML = active && !connected && hub.error ? `<div class="rm-error">${esc(hub.error)}</div>` : '';
  const section = document.getElementById('rmPairSection');
  if (section?.classList) section.classList.toggle('off', !connected);
  const revoke = document.getElementById('rmPairRevoke');
  if (revoke) revoke.disabled = !connected;
  const box = document.getElementById('pairQR');
  if (box) {
    const qrLive = connected && S.pairQR && S.pairExpires > Date.now();
    box.classList?.toggle('live', Boolean(qrLive));
    box.innerHTML = qrLive ? `<img src="${esc(S.pairQR)}" alt="手机绑定二维码"><span class="rm-timer" id="rmTimer"></span>`
      : !connected ? '<span>连接恢复后可绑定手机</span>' : hub.pairing?.paired
        ? `${icon('check')}<span>手机已绑定</span>` : `<button class="btn primary sm" data-act="rmShowQR">${icon('scan')}显示二维码</button>`;
    if (qrLive) rmTickQR();
  }
}

function rmPairStatus(st) {
  rmUpdateRemoteStatus(st);
  const pair = st?.pairing;
  if (!pair || pair.deviceId !== S.state?.config?.deviceId) return;
  const consumed = Boolean(S.pairQR && pair.pendingExpiresAt === 0);
  if (pair.pendingExpiresAt === 0) {
    S.pairQR = ''; S.pairExpires = 0; clearInterval(RM.qrTimer);
    const box = S.route === 'login' && RM.remoteMode === 'station' && rmEffectiveMode() === 'hub' && st.state === 'connected' ? $('#pairQR') : null;
    if (box) {
      box.classList.remove('live');
      box.innerHTML = pair.paired ? `${icon('check')}<span>手机已绑定</span>`
        : '<button class="btn primary sm" data-act="rmShowQR">' + icon('scan') + '显示二维码</button>';
    }
  }
  if (consumed && pair.paired) toast('手机绑定成功', 'ok');
}

function remoteTabMarkup() {
  const active = rmEffectiveMode();
  return `<nav class="rm-tabs rm-head-tabs" aria-label="手机远程连接方式">
    ${[['station', '中转站', 'server'], ['direct', '直连', 'link'], ['bot', 'Bot', 'box']].map(([id, label, glyph]) => `<button class="rm-tab ${RM.remoteMode === id ? 'on' : ''}" data-act="rmMode" data-mode="${id}" aria-pressed="${RM.remoteMode === id}">${icon(glyph)}<span>${label}</span>${active === (id === 'station' ? 'hub' : id) ? '<i class="rm-tab-enabled" title="当前启用" aria-label="当前启用"></i>' : ''}</button>`).join('')}
  </nav>`;
}

function renderRemoteDirectPanel(withSelfhost = true) {
  if (withSelfhost && typeof selfhostRememberDraft === 'function') selfhostRememberDraft();
  const d = RM.direct || {};
  const active = rmEffectiveMode() === 'direct';
  const paired = d.paired || S.state?.config?.remoteModes?.direct?.paired;
  const status = !active ? paired ? '已配对，未启用' : '未启用' : d.error ? '暂不可用' : d.connected ? '手机已连接' : paired ? '已配对，等待连接' : d.running ? '等待配对' : d.enabled ? '启动失败' : '正在启动';
  const tone = !active ? '' : d.error || d.enabled && !d.running ? 'bad' : d.connected ? 'ok' : 'wait';
  return `<section class="rm-sec rm-mode-panel" id="rmDirectPanel">
    <div class="rm-mode-head"><div><h2>电脑直连</h2><p>通过局域网或 VPN 的 TLS 连接，消息不经过 Hub。</p></div><span class="rm-mode-state"><i class="rm-dot ${tone}"></i>${esc(status)}</span></div>
    ${d.error ? `<div class="rm-error">${esc(d.error)}</div>` : ''}
    <div class="rm-direct-facts"><div><span>连接地址</span><b>${esc(active && d.running ? (d.secureUrls || []).join(', ') || 'TLS 已启动' : '未启动')}</b></div><div><span>手机</span><b>${esc(active && d.connected ? '已连接' : paired ? '已配对' : '未配对')}</b></div></div>
    <div class="rm-mode-actions">${paired ? `<button class="btn primary" data-act="rmActivateMode" data-mode="direct" ${RM.connectionAction || active && d.running ? 'disabled' : ''}>${active && d.running ? '直连已启用' : active ? '恢复直连' : '启用直连'}</button>` : `<button class="btn primary" data-act="rmDirectQR" ${RM.connectionAction ? 'disabled' : ''}>${icon('phone')}${active ? '生成配对二维码' : '启用并配对'}</button>`}<button class="btn" data-act="rmDirectRefresh">${icon('refresh')}刷新</button><button class="rm-link bad" data-act="rmDirectRevoke" ${!RM.connectionAction && (paired || d.enabled) ? '' : 'disabled'}>撤销直连</button></div>
    ${!active && paired ? '<p class="rm-recovery-note">配对已保留，启用后手机可继续连接，无需重新扫码。</p>' : ''}
    ${active && d.qrDataURL && Number(d.expiresAt) > Date.now() ? `<div class="rm-direct-qr"><img src="${esc(d.qrDataURL)}" alt="直连配对二维码"><span>二维码有效至 ${esc(new Date(Number(d.expiresAt)).toLocaleTimeString())}</span></div>` : ''}
    <button class="rm-help-link" data-act="rmDirectTutorial">查看直连说明</button>
  </section>${withSelfhost && typeof renderSelfhostHubPanel === 'function' ? renderSelfhostHubPanel() : ''}`;
}

function rmSavedBotsMarkup(c) {
  return rmSavedConnections(c, 'bot').map(entry => {
    const current = rmEffectiveMode(c) === 'bot' && rmNormalizeHub(entry.hubUrl) === rmNormalizeHub(c.effectiveHubUrl || c.hubUrl);
    const connected = current && c.loggedIn && rmHubState() === 'connected';
    const status = !current ? '已保存，未启用' : connected ? '已连接' : S.state?.hub?.error || S.state?.hub?.state === 'invalid_key' ? rmConnectionLabel(S.state?.hub) : '正在连接';
    return `<div class="rm-saved-mode"><div><b>${esc(entry.name || entry.botGroupId || entry.groupId || 'Bot 中继')}</b><span>${esc(host(entry.hubUrl))}</span></div><span class="rm-mode-state"><i class="rm-dot ${connected ? 'ok' : current ? 'wait' : ''}"></i>${status}</span><button class="btn sm ${current ? '' : 'primary'}" data-act="rmActivateMode" data-mode="bot" data-hub="${esc(entry.hubUrl)}" ${RM.connectionAction || current ? 'disabled' : ''}>${current ? '已启用' : '启用 Bot'}</button></div>`;
  }).join('');
}
function renderRemoteBotPanel(c) {
  const entries = rmSavedConnections(c, 'bot');
  return `<section class="rm-sec rm-mode-panel" id="rmBotPanel">
    <div class="rm-mode-head"><div><h2>Bot 中继</h2><p>只转发电脑与手机消息，Agent 仍运行在本机。</p></div>${entries.length ? '' : '<span class="rm-mode-state">未登记</span>'}</div>
    <div id="rmSavedBots" class="rm-saved-modes">${rmSavedBotsMarkup(c)}</div>
    ${entries.length ? `<details class="rm-new-connection" ${RM.botDraft ? 'open' : ''}><summary>添加其他 Bot 绑定</summary>` : ''}
    <label class="field"><span>绑定 JSON</span><textarea class="input rm-bot-input" id="rmBotBinding" rows="6" placeholder="粘贴 Hub 管理页生成的 JSON">${esc(RM.botDraft)}</textarea></label>
    <div class="rm-mode-actions"><button class="btn primary" data-act="rmBotBind">${icon('upload')}导入并登记</button><button class="rm-help-link" data-act="rmBotTutorial">查看教程</button></div>
    <div id="rmBotError"></div>${entries.length ? '</details>' : ''}
  </section>`;
}

function renderRemoteStationPanel(view, st, c, linked, connected, station, qrLive) {
  const draft = RM.stationDraft;
  const saved = rmSavedConnections(c, 'hub').length > 0;
  const savedMarkup = typeof remoteSavedStationsHTML === 'function' ? remoteSavedStationsHTML(c) : '';
  return `${!linked && saved ? savedMarkup : ''}${linked ? `<section class="rm-sec">
      <div class="rm-line">
        <span id="rmHubConnectionDot" class="rm-dot ${connected ? 'ok' : st.hub?.error ? 'bad' : 'wait'}"></span>
        <div class="rm-grow"><div class="rm-device-name" id="devName">${S.editingName
          ? `<input class="input" id="devInput" value="${esc(c.deviceName)}" maxlength="40"><button class="btn sm primary" data-act="saveName">保存</button><button class="btn sm ghost" data-act="cancelName">取消</button>`
          : `${esc(c.deviceName)}<button class="icon-btn" data-act="editName" title="修改电脑名称">${icon('edit')}</button>`}</div>
          <div class="rm-meta" id="rmHubConnectionMeta">${esc(rmConnectionLabel(st.hub))} · ${esc(station)}</div></div>
        <button class="rm-link" data-act="rmActivateMode" data-mode="none">停用</button>
      </div><div id="rmHubConnectionError">${!connected && st.hub && st.hub.error ? `<div class="rm-error">${esc(st.hub.error)}</div>` : ''}</div>
    </section>`
  : `<section class="rm-sec rm-hero">${saved ? `<details class="rm-new-connection" ${draft?.url || draft?.claim ? 'open' : ''}><summary>添加其他中转站</summary>` : ''}
      ${saved ? '' : '<h2>连接中转站</h2>'}
      <p>Hub 只转发远程设备消息，模型仍由这台电脑上的 Agent 调用。</p>
      <div class="rm-form"><input class="input" id="rmStation" placeholder="Hub 地址，例如 https://hub.example.com/salcara-hub" value="${esc(draft ? draft.url : c.relayRoot || '')}" autocomplete="url">
        <button class="btn primary" data-act="rmConnect">连接</button></div>
      <details class="rm-adv"><summary>高级选项</summary>
        <label class="field"><span>自定义 Hub 路径</span><input class="input" id="rmHub" placeholder="默认：地址/salcara-hub" value="${esc(draft ? draft.hubUrl : c.hubUrl || '')}" autocomplete="url"></label>
        <label class="field"><span>新电脑首次登记码</span><input class="input" id="rmEnrollmentClaim" type="password" maxlength="64" autocomplete="off" autocapitalize="none" spellcheck="false" placeholder="首次登记必填，已登记电脑留空" value="${esc(draft?.claim || '')}"></label>
        <p class="rm-note">登记失败时保留以便重试；这里填写设备登记码，不是 Sub2API 账号或 API Key。</p>
      </details>
      <div id="rmConnectError"></div>
    ${saved ? '</details>' : ''}</section>`}
  ${!saved || linked ? savedMarkup : ''}
  ${linked ? `<section class="rm-sec ${connected ? '' : 'off'}" id="rmPairSection">
    <div class="rm-sec-head"><h3>绑定手机</h3>${linked ? `<button class="rm-link" id="rmPairRevoke" data-act="pairRevoke" ${connected ? '' : 'disabled'}>解除绑定</button>` : ''}</div>
    <div class="rm-pair">
      <div class="rm-qr ${qrLive ? 'live' : ''}" id="pairQR">${qrLive
        ? `<img src="${esc(S.pairQR)}" alt="手机绑定二维码"><span class="rm-timer" id="rmTimer"></span>`
        : connected ? st.hub?.pairing?.paired ? `${icon('check')}<span>手机已绑定</span>` : `<button class="btn primary sm" data-act="rmShowQR">${icon('scan')}显示二维码</button>` : '<span>先连接中转站</span>'}</div>
      <ol class="rm-steps"><li>手机打开 Salcara →「编程」→「我的电脑」</li><li>点「绑定新电脑」扫码</li><li>确认 Agent</li><li class="rm-tip">二维码 5 分钟有效，只能用一次</li></ol>
    </div>
  </section>` : ''}
  <section class="rm-sec"><div class="rm-sec-head"><h3>项目文件夹</h3><span class="rm-sub">手机可以在这里新建对话</span></div>
    <div class="rm-projects">${(c.projects || []).map((p) => `<div class="rm-project">${icon('folder')}<div class="rm-grow"><div>${esc(p.name)}</div><div class="path">${esc(p.path)}</div></div><button class="rm-link" data-act="rmProj" data-path="${esc(p.path)}">移除</button></div>`).join('')}</div>
    <div class="rm-add"><input class="input" id="projPath" placeholder="${st.os === 'windows' ? 'C:\\code\\my-app' : esc((st.home || '~') + '/code/my-app')}"><button class="btn" data-act="browse">浏览…</button><button class="btn primary" data-act="addProj">添加</button></div>
  </section><div id="rmLive"></div><p class="rm-note">API Key 始终留在这台电脑上，中转站只转发对话。</p>`;
}

function renderRemoteLoginPage(view, st) {
  if (view.isConnected === false) return;
  const pageEpoch = ++RM.loginEpoch;
  const c = st.config, state = rmHubState();
  const linked = c.loggedIn && rmEffectiveMode(c) === 'hub';
  const connected = linked && state === 'connected';
  clearInterval(RM.poll);
  if (RM.remoteMode === 'direct') RM.poll = setInterval(() => {
    if (S.route !== 'login' || RM.remoteMode !== 'direct' || pageEpoch !== RM.loginEpoch) { clearInterval(RM.poll); return; }
    if (!document.hidden && !RM.connectionAction) {
      void rmLoadDirect(pageEpoch);
      loadState().then(() => { if (pageEpoch === RM.loginEpoch && S.route === 'login') rmUpdateRemoteStatus(); }).catch(() => undefined);
    }
  }, 2500);
  else RM.poll = setInterval(() => {
    if (S.route !== 'login' || pageEpoch !== RM.loginEpoch) { clearInterval(RM.poll); return; }
    if (document.hidden || RM.connectionAction) return;
    loadState().then(() => { if (pageEpoch === RM.loginEpoch && S.route === 'login') rmUpdateRemoteStatus(); }).catch(() => undefined);
  }, 5000);
  const station = host(c.relayRoot || c.effectiveHubUrl);
  const qrLive = S.pairQR && S.pairExpires > Date.now();
  view.innerHTML = `<div class="rm-page"><div id="rmActiveMode">${rmModeStatusMarkup(c)}</div><div id="rmModeError" aria-live="polite"></div><div id="approvalsBox"></div>
    ${RM.remoteMode === 'station' ? renderRemoteStationPanel(view, st, c, linked, connected, station, qrLive) : RM.remoteMode === 'bot' ? renderRemoteBotPanel(c) : renderRemoteDirectPanel()}<section class="rm-sec" id="rmRecovery" aria-label="消息与恢复">${rmRecoveryMarkup(st.hub)}</section></div>`;
  if (typeof renderApprovalsBox === 'function') renderApprovalsBox();
  rmTickQR();
  if (typeof selfhostAttach === 'function') selfhostAttach(view, pageEpoch);
  if (RM.remoteMode === 'station') { rmLoadAgents(pageEpoch); rmLoadLive(pageEpoch); }
  if (RM.remoteMode === 'direct') rmLoadDirect(pageEpoch);
  if (typeof syncRemoteSupport === 'function') syncRemoteSupport();
}

RENDER_login = function (view) {
  const target = view;
  if (target.isConnected === false) return;
  const routeRevision = S.routeRevision;
  rmSelectInitialMode(S.state?.config);
  $('#headActions').innerHTML = remoteTabMarkup();
  if (S.state) renderRemoteLoginPage(target, S.state);
  const inputRevision = RM.connectionRevision;
  const ownsTarget = () => target.isConnected !== false && S.route === 'login' && routeRevision === S.routeRevision;
  void loadState().then((st) => {
    if (!ownsTarget() || inputRevision !== RM.connectionRevision || RM.connectionAction) return;
    rmSelectInitialMode(st.config);
    $('#headActions').innerHTML = remoteTabMarkup();
    renderRemoteLoginPage(target, st);
  }).catch((e) => {
    if (!S.state && ownsTarget() && inputRevision === RM.connectionRevision) target.innerHTML = `<div class="rm-error" style="margin:20px">${esc(e.message)}</div>`;
  });
};

const LIVE_PROMPT = '请开启 Salcara 实时桌面模式：用你的工具列出本机最近的 Codex 对话（最多 200 个，排除当前这个对话），然后调用 salcara_desktop_connect，参数 allowRemoteControl 为 true，durationSeconds 为 2592000，sessionKeys 为这些对话的 codex:真实对话ID 数组。保持这次调用一直运行，不要在这个对话里做别的事。';

async function rmLoadLive(pageEpoch = RM.loginEpoch) {
  // The Codex live-desktop plugin is retired; only offer to remove it if it is still installed.
  const box = document.getElementById('rmLive');
  if (!box) return;
  let st = null;
  try { st = (await api('/api/local/desktop-companion/uninstall')).status; } catch (_) { /* unknown */ }
  if (pageEpoch !== RM.loginEpoch || S.route !== 'login' || box.isConnected === false) return;
  box.innerHTML = st && st.installed ? `<section class="rm-sec"><div class="rm-sec-head"><h3>旧版 Codex 桌面插件</h3><button class="rm-link" data-act="rmLiveUninstall">卸载</button></div><p class="rm-sub">已不再需要，建议卸载。</p></section>` : '';
}

async function rmLoadDirect(pageEpoch = RM.loginEpoch) {
  const generation = RM.directGeneration;
  let request = RM.directPromise;
  if (!request || request.generation !== generation) {
    request = { generation, promise: null };
    request.promise = api('/api/local/direct/status').then(value => {
      if (generation !== RM.directGeneration) return;
      const qr = RM.direct;
      RM.direct = value.enabled && !value.paired && qr?.expiresAt > Date.now() ? { ...value, qrDataURL: qr.qrDataURL, expiresAt: qr.expiresAt } : value;
    }).catch(error => {
      if (generation === RM.directGeneration) RM.direct = { ...RM.direct, error: error.message || '直连状态暂不可用' };
    }).finally(() => { if (RM.directPromise === request) RM.directPromise = null; });
    RM.directPromise = request;
  }
  await request.promise;
  const box = document.getElementById('rmDirectPanel');
  if (generation === RM.directGeneration && typeof setConn === 'function') setConn(S.state?.hub);
  if (generation === RM.directGeneration && pageEpoch === RM.loginEpoch && S.route === 'login' && RM.remoteMode === 'direct' && box?.isConnected !== false && box) box.outerHTML = renderRemoteDirectPanel(false);
  return RM.direct;
}

Object.assign(ACTIONS, {
  rmRecoverySessions: () => { location.hash = '#sessions'; },
  rmRecoveryRefresh: async (button) => {
    const pageEpoch = RM.loginEpoch, revision = RM.connectionRevision;
    await busy(button, async () => {
      try {
        await loadState();
        if (pageEpoch !== RM.loginEpoch || revision !== RM.connectionRevision || S.route !== 'login') return;
        rmUpdateRemoteStatus();
        toast('状态已刷新', 'ok');
      } catch (error) {
        if (pageEpoch === RM.loginEpoch && S.route === 'login') toast(error.message || '状态暂时不可用，请稍后刷新', 'bad');
      }
    });
  },
  rmMode: async (b) => {
    const mode = ['station', 'direct', 'bot'].includes(b.dataset.mode) ? b.dataset.mode : 'station';
    RM.modeInitialized = true;
    if (RM.remoteMode === mode) return;
    rmRememberConnectionDraft();
    RM.connectionRevision++;
    if (typeof selfhostRememberDraft === 'function') selfhostRememberDraft();
    RM.remoteMode = mode;
    await route();
  },
  rmLiveCopy: () => copyText(LIVE_PROMPT, '开启指令'),
  rmLiveInstall: async (b) => {
    if (!await confirm('安装 Codex 桌面插件？\n\n会先备份 Codex 配置，再添加 Salcara 桌面插件；不改 API、对话和项目，不会关闭 Codex。安装后需要重启一次 Codex。')) return;
    await busy(b, async () => {
      try { const r = await api('/api/local/desktop-companion/install', { confirmed: true }); toast((r.status && r.status.message) || '已安装，重启一次 Codex 后开启', 'ok'); }
      catch (e) { toast(e.message, 'bad'); }
    });
    rmLoadLive();
  },
  rmLiveUninstall: async (b) => {
    if (!await confirm('卸载 Codex 桌面插件？只移除 Salcara 添加的那一段配置，其他设置、对话不受影响。')) return;
    await busy(b, async () => {
      try { await api('/api/local/desktop-companion/uninstall', { confirmed: true }); toast('已卸载；重启 Codex 后插件完全停止', 'ok'); }
      catch (e) { toast(e.message, 'bad'); }
    });
    rmLoadLive();
  },
  rmLiveStop: async (b) => {
    if (!await confirm('结束实时桌面模式？正在执行的任务不会停止，之后手机改用后台方式继续。')) return;
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

function rmAPIStationReason(option) {
  if (!option || option.remoteEligibility?.supported !== false) return '';
  return {
    external_api_not_allowed: '本站未授权此 API 地址',
    reverse_proxy_not_allowed: '本站未授权此逆向代理地址',
  }[option.remoteEligibility.reason] || '';
}

async function rmLoadAgents(pageEpoch = RM.loginEpoch) {
  const box = document.getElementById('rmAgents');
  if (!box) return;
  try {
    RM.agents = await api('/api/remote/agents');
  } catch (e) { if (pageEpoch === RM.loginEpoch && S.route === 'login' && box.isConnected !== false) box.innerHTML = `<div class="rm-error" style="margin:12px">${esc(e.message)}</div>`; return; }
  if (pageEpoch !== RM.loginEpoch || S.route !== 'login' || box.isConnected === false) return;
  const apis = RM.agents.apis || [];
  box.innerHTML = Object.keys(AGENT_INFO).map((id) => {
    const a = (RM.agents.agents || []).find((x) => x.id === id) || { api: {} };
    const phone = a.api.source === 'phone';
    const current = a.api.source === 'tool' ? `${AGENT_INFO[id].name} 自己的登录` : (a.api.name || '电脑上的 API') + (a.api.model ? ' · ' + a.api.model : '');
    const chosen = apis.find((x) => x.id === a.api.accountId);
    return `<div class="rm-agent" data-agent="${id}">
      <span class="rm-badge ${id}">${AGENT_INFO[id].badge}</span>
      <div><div class="rm-agent-name">${AGENT_INFO[id].name}${a.available ? '' : ' <span class="chip">未安装</span>'}</div><div class="rm-agent-sub">${phone ? '远程任务：' : '跟随电脑：'}${esc(current)}</div>${rmAPIStationReason(chosen) ? `<div class="rm-error">${esc(rmAPIStationReason(chosen))}；请选择已授权的来源。</div>` : ''}</div>
      <select class="input" data-rm-api="${id}"><option value="">跟随电脑设置</option>${apis.map((x) => `<option value="${esc(x.id)}" ${phone && x.id === a.api.accountId ? 'selected' : ''} ${rmAPIStationReason(x) ? 'disabled' : ''}>${esc(x.name)}${rmAPIStationReason(x) ? ' · ' + esc(rmAPIStationReason(x)) : ''}</option>`).join('')}</select>
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
  rmActivateMode: async (b) => {
    const mode = b.dataset.mode;
    if (!['none', 'direct', 'hub', 'bot'].includes(mode)) return;
    const owner = rmBeginConnectionAction(b, RM.remoteMode);
    if (!owner) return;
    rmRememberConnectionDraft();
    const out = document.getElementById('rmModeError');
    if (out) out.innerHTML = '';
    const body = { mode };
    if (b.dataset.hub) body.hubUrl = b.dataset.hub;
    ++RM.directGeneration;
    try { await busy(b, async () => {
      try {
        owner.submitted = true;
        const result = await api('/api/remote/mode', body);
        if (result?.ok !== true || result.activeMode !== mode) throw new Error('服务器未确认连接方式，请刷新状态后核对');
        S.pairQR = ''; S.pairExpires = 0;
        RM.direct = null;
        const state = await rmRefreshAcceptedConnection();
        toast(mode === 'none' ? '远程已停用，配置和配对已保留' : `${RM_MODE_NAMES[mode]}已启用，连接状态正在刷新`, 'ok');
        if (owner.current() && state) await route();
      } catch (error) { await rmRefreshAcceptedConnection(); rmConnectionError(owner, out, error); }
    }); } finally { rmFinishConnectionAction(owner); }
  },
  rmConnect: async (b) => {
    const owner = rmBeginConnectionAction(b, 'station', ['rmStation', 'rmHub', 'rmEnrollmentClaim']);
    if (!owner) return;
    const [url, hubUrl, enrollmentClaim] = owner.fields.map(field => field.value.trim());
    const body = { url, hubUrl };
    rmRememberConnectionDraft();
    const out = document.getElementById('rmConnectError');
    if (out) out.innerHTML = '';
    try { await busy(b, async () => {
      try {
        if (!body.url) throw new Error('请输入中转站地址');
        if (enrollmentClaim && !/^[a-f0-9]{64}$/i.test(enrollmentClaim)) throw new Error('请填写完整的 64 位设备登记码');
        await api('/api/remote/discover', body);
        if (!owner.current()) return;
        const connectBody = Object.assign({ connect: true }, body);
        if (enrollmentClaim) connectBody.enrollmentClaim = enrollmentClaim;
        owner.submitted = true;
        await api('/api/remote/discover', connectBody);
        if (owner.current()) { owner.fields[2].node.value = ''; owner.fields[2].value = ''; RM.stationDraft = null; }
        S.pairQR = ''; S.pairExpires = 0;
        const state = await rmRefreshAcceptedConnection();
        toast(`连接设置已保存：${host(body.hubUrl || body.url)}，正在连接中转站`, 'ok');
        if (owner.current() && state) await route();
      } catch (e) { if (owner.submitted) await rmRefreshAcceptedConnection(); rmConnectionError(owner, out, e); }
    }); } finally { rmFinishConnectionAction(owner); }
  },
  rmBotBind: async (b) => {
    const owner = rmBeginConnectionAction(b, 'bot', ['rmBotBinding']);
    if (!owner) return;
    rmRememberConnectionDraft();
    const out = document.getElementById('rmBotError');
    if (out) out.innerHTML = '';
    try { await busy(b, async () => {
      try {
        const value = JSON.parse(owner.fields[0].value || '');
        if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error('请粘贴完整的 Bot 绑定 JSON');
        if (!owner.current()) return;
        owner.submitted = true;
        await api('/api/remote/bot-bind', value);
        if (owner.current()) { RM.botDraft = ''; owner.fields[0].node.value = ''; owner.fields[0].value = ''; }
        S.pairQR = ''; S.pairExpires = 0;
        const state = await rmRefreshAcceptedConnection();
        toast('Bot 绑定已保存，正在连接', 'ok');
        if (owner.current() && state) await route();
      } catch (e) { if (owner.submitted) await rmRefreshAcceptedConnection(); rmConnectionError(owner, out, e); }
    }); } finally { rmFinishConnectionAction(owner); }
  },
  rmDirectQR: async (b) => {
    const owner = rmBeginConnectionAction(b, 'direct');
    if (!owner) return;
    const generation = ++RM.directGeneration;
    try { await busy(b, async () => {
      try {
        owner.submitted = true;
        const qr = await api('/api/local/direct/qr', {});
        if (generation !== RM.directGeneration) return;
        RM.direct = { ...RM.direct, ...qr, paired: false, error: '' };
        await rmRefreshAcceptedConnection();
        await rmLoadDirect(owner.pageEpoch);
        if (owner.current()) toast('直连配对二维码已生成', 'ok');
      } catch (e) { await rmLoadDirect(owner.pageEpoch); rmConnectionError(owner, null, e); }
    }); } finally { rmFinishConnectionAction(owner); }
  },
  rmDirectRefresh: async () => {
    const epoch = RM.loginEpoch;
    await Promise.all([rmRefreshAcceptedConnection(), typeof selfhostLoad === 'function' ? selfhostLoad() : Promise.resolve()]);
    await rmLoadDirect(epoch);
    if (epoch === RM.loginEpoch) rmUpdateRemoteStatus();
  },
  rmDirectRevoke: async (b) => {
    const owner = rmBeginConnectionAction(b, 'direct');
    if (!owner) return;
    try {
      if (!await confirm('撤销电脑与手机的直连配对，并关闭直连监听？') || !owner.current()) return;
      ++RM.directGeneration;
      await busy(b, async () => {
        try {
          owner.submitted = true;
          await api('/api/local/direct/revoke', {});
          RM.direct = null;
          await rmRefreshAcceptedConnection();
          await rmLoadDirect(owner.pageEpoch);
          toast('直连配对已撤销', 'ok');
        } catch (e) { await rmLoadDirect(owner.pageEpoch); rmConnectionError(owner, null, e); }
      });
    } finally { rmFinishConnectionAction(owner); }
  },
  rmDirectTutorial: () => {
    $('#modalRoot').innerHTML = `<div class="modal-back" data-act="closeModal"><div class="modal account-modal" data-stop role="dialog" aria-modal="true" aria-labelledby="rmDirectTitle"><div class="modal-head"><h3 id="rmDirectTitle">电脑直连</h3><button class="icon-btn" data-act="closeModal" aria-label="关闭">${icon('close')}</button></div><div class="modal-body stack"><p>电脑会在当前局域网或 VPN 地址启动 TLS 直连，手机扫码后直接连接。</p><ol><li>手机与电脑处于同一局域网或 VPN。</li><li>扫描二维码并核对手机上的证书指纹。</li><li>直连中断时不会自动改走 Hub。</li></ol></div><div class="modal-foot"><button class="btn" data-act="closeModal">关闭</button></div></div></div>`;
  },
  rmBotTutorial: () => {
    $('#modalRoot').innerHTML = `<div class="modal-back" data-act="closeModal"><div class="modal account-modal" data-stop role="dialog" aria-modal="true" aria-labelledby="rmBotTitle"><div class="modal-head"><h3 id="rmBotTitle">Bot 绑定</h3><button class="icon-btn" data-act="closeModal" aria-label="关闭">${icon('close')}</button></div><div class="modal-body stack"><ol><li>在 Hub 管理页复制 Bot JSON。</li><li>粘贴到此页并导入。</li><li>在手机「设备管理」导入同一 JSON。</li></ol><p class="muted small">Bot 只做中继；Codex、Claude CLI 和其他 Agent 继续运行在电脑上。</p></div><div class="modal-foot"><button class="btn" data-act="closeModal">关闭</button></div></div></div>`;
  },
  rmDisconnect: async (b) => {
    const owner = rmBeginConnectionAction(b, 'station');
    if (!owner) return;
    try {
      if (!await confirm('断开后手机将连不上这台电脑（绑定保留，重新连接即可恢复）。确定吗？') || !owner.current()) return;
      await busy(b, async () => {
        try {
          owner.submitted = true;
          await api('/api/logout', {});
          S.pairQR = ''; S.pairExpires = 0;
          const state = await rmRefreshAcceptedConnection();
          toast('已断开中转站，手机绑定保留', 'ok');
          if (owner.current() && state) await route();
        } catch (e) { await rmRefreshAcceptedConnection(); rmConnectionError(owner, null, e); }
      });
    } finally { rmFinishConnectionAction(owner); }
  },
  rmShowQR: async (b) => {
    const owner = rmBeginConnectionAction(b, 'station');
    if (!owner) return;
    const identity = JSON.stringify([S.state?.config?.deviceId, S.state?.config?.effectiveHubUrl]);
    try { await busy(b, async () => {
      try {
        owner.submitted = true;
        const result = await api('/api/pair/start', {});
        if (identity !== JSON.stringify([S.state?.config?.deviceId, S.state?.config?.effectiveHubUrl])) return;
        S.pairCode = result.code; S.pairQR = result.qrUrl || ''; S.pairExpires = result.expiresAt;
        if (owner.current()) { toast('请用手机扫码绑定这台电脑', 'ok'); await route(); }
      } catch (e) { await rmRefreshAcceptedConnection(); rmConnectionError(owner, null, e); }
    }); } finally { rmFinishConnectionAction(owner); }
  },
  pairRevoke: async (b) => {
    const owner = rmBeginConnectionAction(b, 'station');
    if (!owner) return;
    try {
      if (!await confirm('解除这台电脑的手机绑定？旧手机和未使用的二维码都会失效，本地 API 密钥与会话保留。') || !owner.current()) return;
      await busy(b, async () => {
        try {
          owner.submitted = true;
          const result = await api('/api/pair/revoke', {});
          S.pairQR = ''; S.pairExpires = 0;
          const state = await rmRefreshAcceptedConnection();
          toast(result.message || '手机绑定已解除', 'ok');
          if (owner.current() && state) await route();
        } catch (e) { await rmRefreshAcceptedConnection(); rmConnectionError(owner, null, e); }
      });
    } finally { rmFinishConnectionAction(owner); }
  },
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
    if (!await confirm('移除后手机不能再在这个文件夹里新建对话。确定吗？')) return;
    await api('/api/projects/remove', { path: b.dataset.path });
    route();
  },
});

/* ---------------- 对话 ---------------- */

RENDER_sessions = async function (view) {
  const pageEpoch = ++RM.sessionsEpoch;
  $('#headActions').innerHTML = `<button class="btn" data-act="reloadSessions">${icon('refresh')}刷新</button>`;
  if (Object.hasOwn(AGENT_INFO, S.sessFilter)) RM.tool = S.sessFilter;
  view.innerHTML = `<div class="cx">
    <aside class="cx-side">
      <div class="cx-side-top" id="cxSessionNav"></div>
      <div class="cx-list" id="sessList"><div class="empty"><span class="spin"></span></div></div>
    </aside>
    <section class="cx-main" id="sessDetail"></section>
  </div>`;
  renderSessionNavigation();
  renderDetail(true);
  const agentsPromise = loadAgentCatalog().then((r) => {
    if (pageEpoch === RM.sessionsEpoch && S.route === 'sessions') {
      RM.agents = r;
      renderSessionNavigation();
      renderSessionList();
    }
  }).catch((e) => {
    if (pageEpoch === RM.sessionsEpoch && S.route === 'sessions') {
      const nav = $('#cxSessionNav');
      if (nav) nav.insertAdjacentHTML('beforeend', `<div class="muted small cx-capability-error">会话能力读取失败：${esc(e.message || String(e))} <button class="rm-link" data-act="cxReloadAgents">重试</button></div>`);
    }
  });
  await loadSessions(pageEpoch);
  void agentsPromise;
  if (pageEpoch === RM.sessionsEpoch && S.route === 'sessions' && S.openKey) openSession(S.openKey, true, pageEpoch);
};

async function loadSessions(pageEpoch = RM.sessionsEpoch) {
  try {
    const r = await api('/api/sessions');
    if (pageEpoch !== RM.sessionsEpoch || S.route !== 'sessions') return false;
    S.sessions = r.sessions || [];
  } catch (e) {
    const el = $('#sessList');
    if (pageEpoch === RM.sessionsEpoch && S.route === 'sessions' && el && el.isConnected !== false) el.innerHTML = `<div class="rm-error" style="margin:8px">${esc(e.message)}</div>`;
    return false;
  }
  if (pageEpoch !== RM.sessionsEpoch || S.route !== 'sessions') return false;
  renderSessionList();
  return true;
}

function renderSessionList() {
  const el = $('#sessList');
  if (!el) return;
  const q = RM.query.toLowerCase().trim();
  const pending = new Set(S.approvals.map((a) => a.sessionKey));
  const list = S.sessions.filter((s) => s.tool === RM.tool && (!q || [s.title, s.cwd, s.model].some((v) => String(v || '').toLowerCase().includes(q))));
  if (!list.length) { el.innerHTML = `<div class="empty">${q ? '没有匹配的对话' : `还没有 ${AGENT_INFO[RM.tool].name} 对话`}</div>${renderArchivedSessions(q)}`; return; }
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
  }).join('') + renderArchivedSessions(q);
}

function renderArchivedSessions(query = '') {
  if (!RM.archivedOpen) return '';
  const items = (RM.archived && RM.archived.sessions || []).filter((s) => s.tool === RM.tool && (!query || String(s.title || '').toLowerCase().includes(query)));
  if (!items.length) return `<div class="cx-archived"><div class="cx-archive-title">归档会话</div><div class="empty">${RM.archivedLoading ? '<span class="spin"></span>' : '没有归档会话'}</div></div>`;
  return `<div class="cx-archived"><div class="cx-archive-title">归档会话</div>${items.map((s) => `<div class="cx-archived-row"><span class="t" title="${esc(s.title || '')}">${esc(s.title || '无标题对话')}</span><button class="btn sm" data-act="cxUnarchiveArchived" data-k="${esc(s.sessionKey)}">恢复</button></div>`).join('')}</div>`;
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

function controlErrorText(c) {
  return Object.values((c && c.errors) || {}).filter(Boolean).map((e) => String(e)).join(' · ');
}

function cxControlPanel() {
  const c = currentControl();
  if (!c || !RM.controlPanel) return '';
  const panel = RM.controlPanel;
  if (panel === 'context') {
    const value = c.context;
    if (!value) return '';
    const snap = value.snapshot;
    const comp = value.compaction;
    const used = snap ? compact(snap.usedTokens) : '—';
    const window = snap && snap.windowTokens ? compact(snap.windowTokens) : '—';
    const state = comp && comp.state && comp.state !== 'completed' ? ` · ${esc(comp.state)}` : '';
    return `<div class="cx-control-panel"><div class="cx-panel-title">上下文</div><div class="cx-context-value">${esc(used)} / ${esc(window)} tokens${snap && snap.estimated ? ' · 估算' : ''}${state}</div>${snap && snap.model ? `<div class="muted small">${esc(snap.model)}</div>` : ''}<div class="row"><button class="btn sm" data-act="cxContextRefresh">刷新</button>${value.canCompact ? `<button class="btn sm" data-act="cxCompact" ${comp && comp.blocking ? 'disabled' : ''}>${comp && comp.blocking ? '<span class="spin"></span>压缩中' : '压缩上下文'}</button>` : ''}</div>${value.unavailableReason ? `<div class="muted small">${esc(value.unavailableReason)}</div>` : ''}</div>`;
  }
  if (panel === 'actions') {
    const value = c.actions;
    if (!value) return '';
    const actions = value.actions || [];
    return `<div class="cx-control-panel"><div class="cx-panel-title">会话</div>${actions.includes('rename') ? `<div class="cx-inline"><input class="input" id="cxRename" maxlength="160" placeholder="新的标题" value="${esc(value.title || '')}"><button class="btn sm" data-act="cxRename">保存</button></div>` : ''}<div class="row">${actions.includes('fork') ? '<button class="btn sm" data-act="cxFork">分支</button>' : ''}${actions.includes('archive') ? '<button class="btn sm danger" data-act="cxArchive">归档</button>' : ''}${actions.includes('unarchive') ? '<button class="btn sm" data-act="cxUnarchive">恢复归档</button>' : ''}</div>${value.unavailableReason ? `<div class="muted small">${esc(value.unavailableReason)}</div>` : ''}</div>`;
  }
  if (panel === 'workflow') {
    const value = c.workflows;
    if (!value) return '';
    const goal = value.goal;
    const actions = value.goalActions || [];
    return `<div class="cx-control-panel"><div class="cx-panel-title">任务目标${goal ? ` · ${esc(goal.status)}` : ''}</div>${goal ? `<div class="cx-goal">${esc(goal.objective)}${goal.tokenBudget ? ` · ${compact(goal.tokensUsed)} / ${compact(goal.tokenBudget)} tokens` : ''}</div>` : ''}${actions.includes('set') ? `<div class="cx-inline"><input class="input" id="cxGoal" maxlength="1000" placeholder="设置任务目标"><input class="input cx-budget" id="cxBudget" inputmode="numeric" placeholder="预算"><button class="btn sm" data-act="cxGoalSet">设置</button></div>` : ''}<div class="row">${actions.includes('pause') ? '<button class="btn sm" data-act="cxGoalPause">暂停</button>' : ''}${actions.includes('resume') ? '<button class="btn sm" data-act="cxGoalResume">恢复</button>' : ''}${actions.includes('clear') ? '<button class="btn sm danger" data-act="cxGoalClear">清除目标</button>' : ''}${value.canReview ? '<button class="btn sm" data-act="cxReview">复盘当前改动</button>' : ''}</div>${value.unavailableReason ? `<div class="muted small">${esc(value.unavailableReason)}</div>` : ''}</div>`;
  }
  if (panel === 'side') {
    const value = c.sideCatalog;
    if (!value) return '';
    const answer = RM.sideChain && RM.sideChain.key === S.openKey ? RM.sideChain.answer : '';
    return `<div class="cx-control-panel"><div class="cx-panel-title">独立侧问</div><div class="muted small">${value.contextMessages || 0} 条上下文${value.contextTruncated ? ' · 已截取' : ''}</div><div class="cx-inline"><input class="input" id="cxSideQuestion" maxlength="16000" placeholder="只问一个独立问题"><button class="btn sm" data-act="cxSideAsk">提问</button></div>${answer ? `<div class="cx-side-answer">${renderText(answer)}</div>` : ''}${value.unavailableReason ? `<div class="muted small">${esc(value.unavailableReason)}</div>` : ''}</div>`;
  }
  return '';
}

function cxControlTools(s) {
  const c = currentControl(s.sessionKey);
  if (!c) return '';
  const buttons = [];
  if (c.context) buttons.push(`<button class="btn sm ${RM.controlPanel === 'context' ? 'on' : ''}" data-act="cxPanel" data-panel="context">上下文</button>`);
  const actions = c.actions && c.actions.actions || [];
  if (actions.length) buttons.push(`<button class="btn sm ${RM.controlPanel === 'actions' ? 'on' : ''}" data-act="cxPanel" data-panel="actions">会话</button>`);
  const workflows = c.workflows;
  if (workflows && (workflows.goalSupported || workflows.canReview)) buttons.push(`<button class="btn sm ${RM.controlPanel === 'workflow' ? 'on' : ''}" data-act="cxPanel" data-panel="workflow">目标</button>`);
  if (c.sideCatalog && c.sideCatalog.available) buttons.push(`<button class="btn sm ${RM.controlPanel === 'side' ? 'on' : ''}" data-act="cxPanel" data-panel="side">侧问</button>`);
  const errors = controlErrorText(c);
  if (c.loading) buttons.push('<span class="muted small"><span class="spin"></span>读取可用操作</span>');
  else if (errors) buttons.push(`<span class="muted small cx-control-error" title="${esc(errors)}">${esc(errors)}</span>`);
  return buttons.length || errors ? `<div class="cx-controls">${buttons.join('')}</div>${cxControlPanel()}` : '';
}

function renderControlTools() {
  const mount = $('#cxControlMount');
  if (mount && S.openInfo) mount.innerHTML = cxControlTools(S.openInfo);
}

function renderFeatureOptions() {
  const mount = $('#cxFeatureMount');
  if (!mount) return;
  const features = RM.newThread ? RM.newFeatures : currentControl(S.openKey)?.features;
  mount.innerHTML = featureInputs(features);
}

async function loadSessionControls(key, pageEpoch = RM.sessionsEpoch) {
  const localEpoch = ++RM.controlEpoch;
  const s = S.openInfo;
  if (!s || !key || s.sessionKey !== key) return;
  RM.controls = { key, epoch: localEpoch, caps: {}, loading: true, errors: {}, context: null, actions: null, workflows: null, sideCatalog: null, features: null };
  RM.controlPanel = '';
  renderControlTools();
  renderFeatureOptions();
  try {
    if (!RM.agents) await loadAgentCatalog();
    if (pageEpoch !== RM.sessionsEpoch || localEpoch !== RM.controlEpoch || S.route !== 'sessions' || S.openKey !== key) return;
    const caps = sessionCapabilities(s.tool);
    RM.controls.caps = caps;
    const jobs = [];
    const add = (name, body) => jobs.push([name, controlRequest(Object.assign({ tool: s.tool, sessionKey: key }, body))]);
    if (caps.context) add('context', { type: 'session.context' });
    if (caps.sessionActions) add('actions', { type: 'session.actions' });
    if (caps.sessionWorkflows) add('workflows', { type: 'session.workflows' });
    if (caps.sideQuestion) add('sideCatalog', { type: 'session.side-question.catalog' });
    if (caps.features) add('features', { type: 'features.list' });
    if (!jobs.length && !RM.agents) RM.controls.errors.general = '无法读取本机 Agent 能力';
    const settled = await Promise.all(jobs.map(async ([name, promise]) => {
      try { return [name, await promise, null]; } catch (e) { return [name, null, e && e.message || String(e)]; }
    }));
    if (pageEpoch !== RM.sessionsEpoch || localEpoch !== RM.controlEpoch || S.route !== 'sessions' || S.openKey !== key) return;
    for (const [name, value, error] of settled) { if (value) RM.controls[name] = value; if (error) RM.controls.errors[name] = error; }
    RM.controls.loading = false;
    renderControlTools();
    renderFeatureOptions();
  } catch (e) {
    if (pageEpoch === RM.sessionsEpoch && localEpoch === RM.controlEpoch && S.route === 'sessions' && S.openKey === key) {
      RM.controls.loading = false; RM.controls.errors.general = e.message || String(e); renderControlTools(); renderFeatureOptions();
    }
  }
}

async function loadFeaturesForCwd(cwd, tool = RM.tool) {
  const epoch = ++RM.controlEpoch;
  RM.newFeatures = null;
  renderFeatureOptions();
  try {
    if (!RM.agents) await loadAgentCatalog();
    const caps = sessionCapabilities(tool);
    if (!caps.features || !cwd || !RM.newThread || S.route !== 'sessions') return;
    const value = await controlRequest({ type: 'features.list', tool, cwd });
    if (epoch !== RM.controlEpoch || !RM.newThread || S.route !== 'sessions') return;
    RM.newFeatures = value; renderFeatureOptions();
  } catch (e) {
    if (epoch === RM.controlEpoch && RM.newThread && S.route === 'sessions') { RM.newFeatures = { unavailableReason: e.message || String(e), collaborationModes: [], skills: [], skillsSupported: false }; renderFeatureOptions(); }
  }
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
  const features = currentControl(s.sessionKey)?.features;
  el.innerHTML = `<div class="cx-head"><div style="flex:1;min-width:0"><div class="title">${esc(s.title || '对话')}</div>
      <div class="meta">${esc([s.cwd ? projectOf(s.cwd) : '', s.client, s.model].filter(Boolean).join(' · '))}</div><div class="meta">在此发送使用电脑当前 API 设置；手机单独选择的 API 仍保留在手机远程设置中。</div></div>
      ${s.tool === 'codex' && s.sessionKey ? `<button class="btn sm" data-act="cxNavigate" title="让 Codex 桌面端跳到这个对话">${icon('laptop')}在 Codex 中打开</button>` : ''}
      ${canOpenNativeSession(s) ? `<button class="btn sm" data-act="resumeSession" data-k="${esc(s.sessionKey)}" data-target="${esc(s.tool)}" title="在终端里用原工具打开同一个对话">${icon('terminal')}终端打开</button>` : ''}</div>
      <div id="cxControlMount">${cxControlTools(s)}</div>
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
      <textarea id="sendText" rows="2" ${s.controllable === false ? 'disabled' : ''} placeholder="${s.controllable === false ? '当前只能查看，请先核对原任务状态' : running ? '补充说明，完成后接着做' : '继续对话'}（${OSX() === 'darwin' ? '⌘' : 'Ctrl+'}Enter 发送）">${esc(draft)}</textarea>
      <div class="cx-bar">${s.tool === 'codex' ? `<select class="cx-chip" id="cxEffort" title="推理强度"><option value="">推理：默认</option>${[['low', '低'], ['medium', '中'], ['high', '高']].map(([v, l]) => `<option value="${v}" ${RM.effort === v ? 'selected' : ''}>推理：${l}</option>`).join('')}</select>` : ''}<div id="cxFeatureMount">${featureInputs(features)}</div>
        <span class="grow"></span>
        ${running && s.controllable !== false ? `<button class="cx-send stop" data-act="interrupt" title="停止">${icon('stop')}</button>` : ''}
        <button class="cx-send" data-act="send" title="发送" ${s.controllable === false ? 'disabled' : ''}>${icon('arrowUp')}</button></div>
    </div></div>`;
  const t = $('#cxThread');
  if (t && (scroll || atBottom)) t.scrollTop = t.scrollHeight;
}

function cxRenderNew(el) {
  el.innerHTML = `<div class="cx-head"><div style="flex:1"><div class="title">新对话</div><div class="meta">${AGENT_INFO[RM.tool].name} 会在电脑上的项目里工作</div></div></div>
    <div class="cx-thread"><div class="cx-empty"><div class="rm-hero-icon" style="margin:0 auto">${icon('code')}</div><h3>要构建什么？</h3>
      <select class="input" id="cxProject" style="max-width:420px;margin:10px auto 0">${RM.projects.map((p) => `<option value="${esc(p.path)}">${esc(p.name || projectOf(p.path))} — ${esc(p.path)}</option>`).join('') || '<option value="">先在「手机远程」添加项目文件夹</option>'}</select><div id="cxFeatureMount">${featureInputs(RM.newFeatures)}</div></div></div>
    <div class="cx-composer-wrap"><div class="cx-composer"><textarea id="cxPrompt" rows="3" placeholder="描述要做的事（${OSX() === 'darwin' ? '⌘' : 'Ctrl+'}Enter 发送）">${esc(RM.newPrompt)}</textarea>
      <div class="cx-bar"><select class="cx-chip" id="cxMode"><option value="ask">每一步都问我</option>${RM.tool === 'hermes' ? '' : '<option value="auto_edits">自动改文件</option>'}<option value="auto_all">全部自动</option></select><span class="grow"></span>
      <button class="cx-send" data-act="cxStart" title="开始">${icon('arrowUp')}</button></div></div></div>`;
  const p = document.getElementById('cxPrompt'); if (p) p.focus();
}

async function openSession(key, keepScroll, pageEpoch = RM.sessionsEpoch) {
  RM.newThread = false;
  ++RM.controlEpoch;
  RM.controls = null;
  RM.controlPromise = null;
  RM.controlPanel = '';
  RM.sideChain = null;
  S.openKey = key;
  renderSessionList();
  const el = $('#sessDetail');
  if (el && !keepScroll) el.innerHTML = '<div class="cx-empty"><span class="spin"></span></div>';
  try {
    const r = await api('/api/session?key=' + encodeURIComponent(key));
    if (pageEpoch !== RM.sessionsEpoch || S.route !== 'sessions' || S.openKey !== key) return;
    S.openInfo = r.session;
    S.tl = [];
    S.tlIndex = new Map();
    (r.events || []).forEach(tlApply);
    S.approvals.filter((a) => a.sessionKey === key).forEach(tlApply);
    renderDetail(true);
    RM.controlPromise = loadSessionControls(key, pageEpoch);
    void RM.controlPromise;
  } catch (e) {
    if (pageEpoch === RM.sessionsEpoch && S.route === 'sessions' && S.openKey === key && el && el.isConnected !== false) el.innerHTML = `<div class="rm-error" style="margin:20px">${esc(e.message)}</div>`;
  }
}

Object.assign(ACTIONS, {
  cxTool: (b) => { RM.tool = b.dataset.t; S.sessFilter = RM.tool; RM.archived = null; RM.archivedOpen = false; RM.controls = null; RM.newFeatures = null; RM.controlPanel = ''; S.openKey = null; S.openInfo = null; RM.newThread = false; renderSessionNavigation(); renderSessionList(); renderDetail(); },
  cxReloadAgents: async (b) => {
    const pageEpoch = RM.sessionsEpoch;
    await busy(b, async () => {
      try {
        RM.agents = await api('/api/remote/agents');
        if (pageEpoch !== RM.sessionsEpoch || S.route !== 'sessions') return;
        renderSessionNavigation(); renderSessionList();
        if (S.openKey) await loadSessionControls(S.openKey, pageEpoch);
        else renderDetail(false);
      } catch (e) { toast(e.message, 'bad'); }
    });
  },
  cxNavigate: async (b) => {
    await busy(b, async () => {
      try { await api('/api/session/navigate', { sessionKey: S.openKey }); toast('已让 Codex 打开这个对话；内容没更新的话，重启一次 Codex', 'ok'); }
      catch (e) { toast(e.message, 'bad'); }
    });
  },
  cxMore: (b) => { RM.expanded[b.dataset.k] = !RM.expanded[b.dataset.k]; renderSessionList(); },
  cxArchived: async (b) => {
    RM.archivedOpen = !RM.archivedOpen;
    if (RM.archivedOpen && !RM.archived) {
      RM.archivedLoading = true; renderSessionList();
      try { RM.archived = await controlRequest({ type: 'sessions.archived', tool: RM.tool, limit: 100 }); }
      catch (e) { toast(e.message, 'bad'); RM.archived = { sessions: [] }; }
      RM.archivedLoading = false;
    }
    renderSessionList();
  },
  cxUnarchiveArchived: async (b) => {
    await openSession(b.dataset.k, false);
    await RM.controlPromise;
    const c = currentControl(b.dataset.k);
    if (!c || !c.actions || !(c.actions.actions || []).includes('unarchive')) { toast('此会话当前不能恢复', 'bad'); return; }
    const s = S.openInfo;
    try {
      const r = await controlRequest({ type: 'session.action', tool: s.tool, sessionKey: s.sessionKey, action: 'unarchive', expectedRevision: c.actions.revision, operationId: operationId() });
      if (r.state !== 'completed') throw new Error(r.error || '恢复结果尚未确认');
      toast('已恢复', 'ok'); RM.archived = null; await loadSessions(RM.sessionsEpoch); await openSession(s.sessionKey, false);
    } catch (e) { toast(e.message, 'bad'); }
  },
  cxPanel: (b) => { const panel = b.dataset.panel || ''; RM.controlPanel = RM.controlPanel === panel ? '' : panel; renderControlTools(); },
  cxCompact: async (b) => {
    const c = currentControl(); const s = S.openInfo;
    if (!c || !c.context || !s) return;
    await busy(b, async () => {
      try {
        const r = await controlRequest({ type: 'session.compact', tool: s.tool, sessionKey: s.sessionKey, expectedRevision: c.context.revision, operationId: operationId() });
        if (currentControl(s.sessionKey) === c) { c.context = r; renderDetail(false); }
      } catch (e) { toast(e.message, 'bad'); }
    });
  },
  cxContextRefresh: async (b) => {
    const c = currentControl(); const s = S.openInfo;
    if (!c || !s) return;
    await busy(b, async () => { try { const value = await controlRequest({ type: 'session.context', tool: s.tool, sessionKey: s.sessionKey }); if (currentControl(s.sessionKey) === c) { c.context = value; renderDetail(false); } } catch (e) { toast(e.message, 'bad'); } });
  },
  cxRename: async (b) => {
    const c = currentControl(); const s = S.openInfo; const input = $('#cxRename');
    if (!c || !c.actions || !s || !input || !input.value.trim()) return;
    await busy(b, async () => {
      try { const r = await controlRequest({ type: 'session.action', tool: s.tool, sessionKey: s.sessionKey, action: 'rename', name: input.value.trim(), expectedRevision: c.actions.revision, operationId: operationId() }); if (r.state !== 'completed') throw new Error(r.error || '标题修改结果尚未确认'); toast('标题已更新', 'ok'); await openSession(s.sessionKey, true); }
      catch (e) { toast(e.message, 'bad'); }
    });
  },
  cxArchive: async (b) => {
    const c = currentControl(); const s = S.openInfo;
    if (!c || !c.actions || !s) return;
    await busy(b, async () => {
      try { const r = await controlRequest({ type: 'session.action', tool: s.tool, sessionKey: s.sessionKey, action: 'archive', expectedRevision: c.actions.revision, operationId: operationId() }); if (r.state !== 'completed') throw new Error(r.error || '归档结果尚未确认'); toast('已归档', 'ok'); S.openKey = null; S.openInfo = null; RM.controls = null; await loadSessions(RM.sessionsEpoch); renderDetail(false); }
      catch (e) { toast(e.message, 'bad'); }
    });
  },
  cxFork: async (b) => {
    const c = currentControl(); const s = S.openInfo;
    if (!c || !c.actions || !s) return;
    await busy(b, async () => {
      try { const r = await controlRequest({ type: 'session.action', tool: s.tool, sessionKey: s.sessionKey, action: 'fork', expectedRevision: c.actions.revision, operationId: operationId() }); if (r.state !== 'completed') throw new Error(r.error || '分支创建结果尚未确认'); const key = r.session && r.session.sessionKey; toast('已创建分支', 'ok'); await loadSessions(RM.sessionsEpoch); if (key) await openSession(key, false); }
      catch (e) { toast(e.message, 'bad'); }
    });
  },
  cxUnarchive: async (b) => {
    const c = currentControl(); const s = S.openInfo;
    if (!c || !c.actions || !s) return;
    await busy(b, async () => {
      try { const r = await controlRequest({ type: 'session.action', tool: s.tool, sessionKey: s.sessionKey, action: 'unarchive', expectedRevision: c.actions.revision, operationId: operationId() }); if (r.state !== 'completed') throw new Error(r.error || '恢复结果尚未确认'); toast('已恢复', 'ok'); await loadSessions(RM.sessionsEpoch); await openSession(s.sessionKey, false); }
      catch (e) { toast(e.message, 'bad'); }
    });
  },
  cxGoalSet: async (b) => {
    const c = currentControl(); const s = S.openInfo; const input = $('#cxGoal');
    if (!c || !c.workflows || !s || !input || !input.value.trim()) return;
    const body = { type: 'session.workflow', tool: s.tool, sessionKey: s.sessionKey, kind: 'goal.set', objective: input.value.trim(), expectedRevision: c.workflows.revision, operationId: operationId() };
    const budget = Number.parseInt(($('#cxBudget') || {}).value || '', 10); if (Number.isFinite(budget) && budget > 0) body.tokenBudget = budget;
    await busy(b, async () => { try { const r = await controlRequest(body); if (r.state !== 'completed') throw new Error(r.error || '目标设置结果尚未确认'); toast('目标已设置', 'ok'); await loadSessionControls(s.sessionKey); } catch (e) { toast(e.message, 'bad'); } });
  },
  cxGoalPause: async (b) => { await ACTIONS.cxGoalMutation(b, 'goal.pause'); },
  cxGoalResume: async (b) => { await ACTIONS.cxGoalMutation(b, 'goal.resume'); },
  cxGoalClear: async (b) => { await ACTIONS.cxGoalMutation(b, 'goal.clear'); },
  cxReview: async (b) => { await ACTIONS.cxGoalMutation(b, 'review', { target: { type: 'uncommittedChanges' } }); },
  cxGoalMutation: async (b, kind, extra = {}) => {
    const c = currentControl(); const s = S.openInfo;
    if (!c || !c.workflows || !s) return;
    await busy(b, async () => { try { const r = await controlRequest(Object.assign({ type: 'session.workflow', tool: s.tool, sessionKey: s.sessionKey, kind, expectedRevision: c.workflows.revision, operationId: operationId() }, extra)); if (r.state === 'unconfirmed') throw new Error(r.error || '操作结果尚未确认'); if (kind === 'review' && r.turnId) { toast('已开始复盘', 'ok'); } else { toast('目标已更新', 'ok'); } await loadSessionControls(s.sessionKey); } catch (e) { toast(e.message, 'bad'); } });
  },
  cxSideAsk: async (b) => {
    const c = currentControl(); const s = S.openInfo; const input = $('#cxSideQuestion');
    if (!c || !c.sideCatalog || !s || !input || !input.value.trim()) return;
    await busy(b, async () => { try { const r = await controlRequest({ type: 'session.side-question.ask', tool: s.tool, sessionKey: s.sessionKey, question: input.value.trim(), expectedRevision: c.sideCatalog.revision, operationId: operationId() }); if (r.state !== 'completed') throw new Error(r.error || '侧问结果尚未确认'); RM.sideChain = { key: s.sessionKey, answer: r.answer || '' }; input.value = ''; renderDetail(false); } catch (e) { toast(e.message, 'bad'); } });
  },
  cxNew: async () => {
    const pageEpoch = RM.sessionsEpoch;
    RM.newThread = true; RM.newPrompt = ''; S.openKey = null; S.openInfo = null; RM.controls = null; RM.newFeatures = null; RM.controlPanel = ''; RM.featureChoice = { mode: '', skills: [] }; renderSessionList(); renderDetail();
    try { RM.projects = (await api('/api/remote/projects')).projects || []; } catch (_) { RM.projects = (S.state && S.state.config && S.state.config.projects) || []; }
    if (pageEpoch === RM.sessionsEpoch && S.route === 'sessions' && RM.newThread) { renderDetail(); const cwd = ($('#cxProject') || {}).value || ''; if (cwd) await loadFeaturesForCwd(cwd, RM.tool); }
  },
  cxStart: async (b) => {
    const prompt = ($('#cxPrompt') || {}).value || RM.newPrompt || '';
    const cwd = ($('#cxProject') || {}).value || '';
    if (!prompt.trim()) return;
    if (!cwd) { toast('先选择项目文件夹', 'bad'); return; }
    await busy(b, async () => {
      try {
        const selected = featureChoicesFromDOM();
        const body = { tool: RM.tool, cwd, prompt: prompt.trim(), approval: ($('#cxMode') || {}).value || 'ask' };
        if (selected.mode) body.collaborationMode = selected.mode;
        const refs = selectedSkillRefs(RM.newFeatures, selected.skills); if (refs.length) body.skills = refs;
        const r = await api('/api/session/start', body);
        RM.newThread = false;
        RM.newPrompt = '';
        const pageEpoch = RM.sessionsEpoch;
        await loadSessions(pageEpoch);
        if (pageEpoch === RM.sessionsEpoch && S.route === 'sessions') await openSession(r.sessionKey, false, pageEpoch);
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
        const selected = featureChoicesFromDOM();
        const body = Object.assign({ sessionKey: S.openKey, text }, RM.effort ? { effort: RM.effort } : {});
        if (selected.mode) body.collaborationMode = selected.mode;
        const refs = selectedSkillRefs(currentControl(S.openKey)?.features, selected.skills); if (refs.length) body.skills = refs;
        await api('/api/session/send', body);
        t.value = '';
        if (S.openInfo) S.openInfo.status = 'running';
        renderDetail(true);
      } catch (e) { toast(e.message, 'bad'); }
    });
  },
});

document.addEventListener('input', (e) => {
  if (S.route === 'login' && ['devInput', 'projPath'].includes(e.target.id)) RM.connectionRevision++;
  if (['rmStation', 'rmHub', 'rmEnrollmentClaim', 'rmBotBinding'].includes(e.target.id)) {
    RM.connectionRevision++;
    rmRememberConnectionDraft();
  }
  if (e.target.id === 'cxSearch') { RM.query = e.target.value; renderSessionList(); }
  if (e.target.id === 'cxPrompt') RM.newPrompt = e.target.value;
});
document.addEventListener('change', (e) => {
  if (e.target.id === 'cxProject' && RM.newThread) {
    RM.featureChoice = { mode: '', skills: [] };
    loadFeaturesForCwd(e.target.value, RM.tool);
  }
  if (e.target.id === 'cxCollaborationMode' || e.target.classList?.contains('cx-skill-check')) {
    RM.featureChoice = featureChoicesFromDOM();
  }
});
document.addEventListener('keydown', (e) => {
  if (e.target.id === 'cxPrompt' && e.key === 'Enter' && (e.ctrlKey || e.metaKey)) { e.preventDefault(); ACTIONS.cxStart($('[data-act=cxStart]')); }
  if (e.target.id === 'rmStation' && e.key === 'Enter') ACTIONS.rmConnect($('[data-act=rmConnect]'));
});
