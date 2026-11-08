'use strict';
/* 环境: detect Claude Code / Codex CLI (and Git on Windows) and install them
   with the vendors' official installers in one click. */

const SU = { poll: 0, discoveryPoll: 0, discoveryDelay: 500, renderView: null, renderSignature: '', job: null, cached: null, local: null };

function suRow(item, busy) {
  const ok = item.installed;
  const state = ok
    ? `<span class="su-state">${icon('check')}${item.source === 'codex-app' ? '可用' : '已就绪'}</span>`
    : item.installable
      ? `<button class="btn ${item.required ? 'primary' : ''} sm" data-act="suInstall" data-tool="${esc(item.id)}" ${busy ? 'disabled' : ''}>一键安装</button>`
      : item.download ? `<a class="btn sm" href="${esc(item.download)}" target="_blank" rel="noopener">去下载</a>` : '';
  const extra = ok && item.source === 'codex-app' && item.installable
    ? `<button class="btn ghost sm" data-act="suInstall" data-tool="${esc(item.id)}" ${busy ? 'disabled' : ''}>单独安装</button>` : '';
  return `<div class="su-row ${ok ? 'ok' : 'miss'} ${item.required ? 'req' : ''}">
    <div class="su-mark">${icon(ok ? 'check' : item.required ? 'warn' : 'box')}</div>
    <div>
      <div class="su-name">${esc(item.name)}${item.version ? `<small>${esc(item.version)}</small>` : ''}</div>
      <div class="su-role">${esc(item.role)}</div>
      ${item.note ? `<div class="su-note">${esc(item.note)}</div>` : ''}
    </div>
    <div class="row" style="gap:8px;align-items:center">${extra}${state}</div>
  </div>`;
}

function suJob(job) {
  if (!job) return '';
  const name = { claude: 'Claude Code', codex: 'Codex CLI', git: 'Git', 'claude-sdk': 'Claude 会话管理', 'deepseek-harness': 'DeepSeek Harness', hermes: 'Hermes Agent' }[job.tool] || job.tool;
  const head = job.state === 'running' ? `正在安装 ${name}…` : job.state === 'done' ? `${name} 安装完成` : `${name} 安装失败${job.error ? '：' + job.error : ''}`;
  return `<div class="card" id="suJob">
    <div class="su-head">${job.state === 'running' ? '' : icon(job.state === 'done' ? 'check' : 'warn')}<span>${esc(head)}</span><span class="spacer"></span>
      ${job.state !== 'running' ? '<button class="btn ghost sm" data-act="suHideLog">收起</button>' : ''}</div>
    ${job.state === 'running' ? '<div class="su-progress" style="margin-top:12px"></div>' : ''}
    <pre class="su-log" id="suLog" style="margin-top:12px">${esc(job.log || '准备中…')}</pre>
  </div>`;
}

function renderSetup(view, r, local) {
  if (view.isConnected === false || S.route !== 'setup') return;
  SU.job = r.job && (r.job.state === 'running' || Date.now() - (r.job.finished || 0) < 60_000) ? r.job : null;
  const busy = SU.job && SU.job.state === 'running';
  const missing = r.items.filter((item) => item.required && !item.installed);
  const setupDot = $('#setupDot');
  if (setupDot) setupDot.hidden = missing.length === 0;
  const signature = JSON.stringify({
    os: r.os,
    discoveryPending: r.discoveryPending,
    items: r.items.map((item) => [item.id, item.name, item.required, item.installed, item.source, item.installable, item.download, item.version, item.role, item.note]),
    job: SU.job && [SU.job.tool, SU.job.state, SU.job.error, SU.job.finished],
    tools: local?.tools?.map((item) => [item.id, item.name, item.available, item.path]),
    toolPaths: local?.toolPaths,
  });
  if (SU.renderView === view && SU.renderSignature === signature) {
    if (busy) suPoll();
    return;
  }
  $('#headActions').innerHTML = `<button class="btn sm" data-act="suRefresh">${icon('refresh')}重新检测</button>`;
  view.innerHTML = `
    ${missing.length ? `<div class="callout warn">${ico('warn')}<div>还差 ${missing.map((item) => esc(item.name)).join('、')}。装好后手机才能继续这台电脑上的对话。</div></div>`
      : '<div class="callout ok">' + ico('check') + '<div>环境已就绪，手机可以远程继续这台电脑上的对话。</div></div>'}
    <div class="su-list">${r.items.map((item) => suRow(item, busy)).join('')}</div>
    ${suJob(SU.job)}
    ${local && local.tools ? localToolPathsPanel(local) : ''}
    <p class="su-foot">CLI 使用官方安装脚本，会话管理使用 Anthropic 官方 SDK${r.os === 'windows' ? '，Git 通过 winget 安装' : ''}。</p>`;
  SU.renderView = view;
  SU.renderSignature = signature;
  if (busy) suPoll();
}
var RENDER_setup = function (view) {
  // Keep the panel visible while local executable/version checks run.
  if (SU.cached) renderSetup(view, SU.cached, SU.local);
  else view.innerHTML = typeof pageSkeleton === 'function' ? pageSkeleton('setup') : '<div class="card"><div class="skeleton" style="width:44%"></div><div class="skeleton" style="width:82%;margin-top:12px"></div></div>';
  SU.discoveryDelay = 500;
  void loadSetup(view);
};

async function loadSetup(view, refresh = false) {
  clearTimeout(SU.discoveryPoll);
  try {
    const [r, local] = await Promise.all([api('/api/setup' + (refresh ? '?refresh=1' : '')), api('/api/local/accounts').catch(() => null)]);
    if (view.isConnected === false || S.route !== 'setup') return;
    SU.cached = r; SU.local = local;
    renderSetup(view, r, local);
    if (r.discoveryPending || local?.discoveryPending) {
      if (!document.hidden) SU.discoveryPoll = setTimeout(() => {
        if (view.isConnected !== false && S.route === 'setup' && !document.hidden) void loadSetup(view);
      }, SU.discoveryDelay);
      SU.discoveryDelay = Math.min(4000, SU.discoveryDelay * 2);
    } else SU.discoveryDelay = 500;
  } catch (error) {
    if (view.isConnected !== false && S.route === 'setup' && !SU.cached) view.innerHTML = `<div class="callout bad">${ico('warn')}<div>${esc(error.message)}</div></div>`;
  }
}
RENDER.setup = (view) => RENDER_setup(view);

function suPoll() {
  clearTimeout(SU.poll);
  SU.poll = setTimeout(async () => {
    if (S.route !== 'setup') return;
    try {
      const { job } = await api('/api/setup/job');
      if (!job) return;
      const log = $('#suLog');
      if (log) { log.textContent = job.log || '…'; log.scrollTop = log.scrollHeight; }
      if (job.state === 'running') { suPoll(); return; }
      toast(job.state === 'done' ? '安装完成' : '安装失败，请查看日志', job.state === 'done' ? 'ok' : 'bad');
      await route();
      if (typeof refreshSetupDot === 'function') refreshSetupDot();
    } catch (_) { suPoll(); }
  }, 900);
}

Object.assign(ACTIONS, {
  suRefresh: async (b) => { await busy(b, () => loadSetup($('#view'), true)); },
  suInstall: async (b) => {
    await busy(b, async () => {
      await api('/api/setup/install', { tool: b.dataset.tool });
      await route();
    });
  },
  suHideLog: () => { const box = $('#suJob'); if (box) box.remove(); },
});
