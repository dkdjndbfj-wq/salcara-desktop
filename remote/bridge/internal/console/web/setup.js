'use strict';
/* 环境: detect Claude Code / Codex CLI (and Git on Windows) and install them
   with the vendors' official installers in one click. */

const SU = { poll: 0, job: null };

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
  const name = { claude: 'Claude Code', codex: 'Codex CLI', git: 'Git' }[job.tool] || job.tool;
  const head = job.state === 'running' ? `正在安装 ${name}…` : job.state === 'done' ? `${name} 安装完成` : `${name} 安装失败${job.error ? '：' + job.error : ''}`;
  return `<div class="card" id="suJob">
    <div class="su-head">${job.state === 'running' ? '' : icon(job.state === 'done' ? 'check' : 'warn')}<span>${esc(head)}</span><span class="spacer"></span>
      ${job.state !== 'running' ? '<button class="btn ghost sm" data-act="suHideLog">收起</button>' : ''}</div>
    ${job.state === 'running' ? '<div class="su-progress" style="margin-top:12px"></div>' : ''}
    <pre class="su-log" id="suLog" style="margin-top:12px">${esc(job.log || '准备中…')}</pre>
  </div>`;
}

var RENDER_setup = async function (view) {
  const [r, local] = await Promise.all([api('/api/setup'), api('/api/local/accounts').catch(() => null)]);
  SU.job = r.job && (r.job.state === 'running' || Date.now() - (r.job.finished || 0) < 60_000) ? r.job : null;
  const busy = SU.job && SU.job.state === 'running';
  const missing = r.items.filter((item) => item.required && !item.installed);
  $('#headActions').innerHTML = `<button class="btn sm" data-act="suRefresh">${icon('refresh')}重新检测</button>`;
  view.innerHTML = `
    ${missing.length ? `<div class="callout warn">${ico('warn')}<div>还差 ${missing.map((item) => esc(item.name)).join('、')}。装好后手机才能继续这台电脑上的对话。</div></div>`
      : '<div class="callout ok">' + ico('check') + '<div>环境已就绪，手机可以远程继续这台电脑上的对话。</div></div>'}
    <div class="su-list">${r.items.map((item) => suRow(item, busy)).join('')}</div>
    ${suJob(SU.job)}
    ${local && local.tools ? localToolPathsPanel(local) : ''}
    <p class="su-foot">使用 Anthropic 与 OpenAI 的官方安装脚本${r.os === 'windows' ? '，Git 通过 winget 安装' : ''}。</p>`;
  if (busy) suPoll();
};
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
  suRefresh: async (b) => { await busy(b, () => route()); },
  suInstall: async (b) => {
    await busy(b, async () => {
      await api('/api/setup/install', { tool: b.dataset.tool });
      await route();
    });
  },
  suHideLog: () => { const box = $('#suJob'); if (box) box.remove(); },
});
