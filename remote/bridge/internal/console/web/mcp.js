'use strict';
/* MCP page: one list of MCP servers that Bridge writes into Codex (config.toml) and Claude Code
   (~/.claude.json). Servers the user added directly in either tool show up as "discovered" until
   imported, and Bridge never edits entries it does not manage. */

PAGES.mcp = { title: 'MCP', sub: '一次配置，同步给 Codex 与 Claude Code' };

const MCP = { data: null, seq: 0, editor: null };
const MCP_AGENTS = [['codex', 'Codex'], ['claude', 'Claude Code']];
const MCP_PRESETS = [
  { name: 'playwright', label: 'Playwright', note: '操作浏览器', icon: 'globe', command: 'npx @playwright/mcp@latest' },
  { name: 'context7', label: 'Context7', note: '查最新文档', icon: 'file', command: 'npx -y @upstash/context7-mcp' },
  { name: 'filesystem', label: '文件系统', note: '读写指定目录', icon: 'folder', command: 'npx -y @modelcontextprotocol/server-filesystem ' },
  { name: 'fetch', label: '网页抓取', note: '读取网页内容', icon: 'search', command: 'uvx mcp-server-fetch' },
  { name: 'memory', label: '记忆', note: '跨对话记住要点', icon: 'db', command: 'npx -y @modelcontextprotocol/server-memory' },
  { name: 'sequential-thinking', label: '分步思考', note: '拆解复杂问题', icon: 'spark', command: 'npx -y @modelcontextprotocol/server-sequential-thinking' },
];

function mcpFace(name) {
  let hash = 0;
  for (const ch of String(name || '')) hash = (hash * 31 + ch.charCodeAt(0)) >>> 0;
  return ['ink', 'violet', 'blue', 'pink', 'iris'][hash % 5];
}
function mcpIcon(server) {
  const preset = MCP_PRESETS.find((p) => p.name === server.name);
  return preset ? preset.icon : server.url ? 'globe' : 'terminal';
}
function mcpCommandText(server) {
  if (server.url) return server.url;
  return [server.command, ...(server.args || [])].map((part) => /\s/.test(part) ? JSON.stringify(part) : part).join(' ');
}
// Splits a command line the way a shell would for simple quoting: "a b" and 'a b' stay one argument.
function mcpSplitCommand(text) {
  const parts = [];
  let cur = '', quote = '', has = false;
  for (const ch of String(text || '').trim()) {
    if (quote) { if (ch === quote) quote = ''; else cur += ch; continue; }
    if (ch === '"' || ch === "'") { quote = ch; has = true; continue; }
    if (/\s/.test(ch)) { if (cur || has) parts.push(cur); cur = ''; has = false; continue; }
    cur += ch;
  }
  if (cur || has) parts.push(cur);
  return parts;
}
function mcpAgentsLabel(server) {
  const on = MCP_AGENTS.filter(([id]) => server.agents?.[id]).map(([, label]) => label);
  return on.length === MCP_AGENTS.length ? '全部 Agent' : on.length ? '仅 ' + on.join('、') : '没有 Agent 使用';
}

function mcpRowHTML(server) {
  const on = server.enabled !== false;
  return `<div class="mcp-row${on ? '' : ' off'}" data-mcp="${esc(server.name)}">
    <span class="mcp-ico" data-face="${on ? mcpFace(server.name) : 'mist'}">${icon(mcpIcon(server))}</span>
    <div class="mcp-main"><b>${esc(server.name)}</b><small title="${esc(mcpCommandText(server))}">${esc(mcpCommandText(server))}</small></div>
    <div class="mcp-agents" role="group" aria-label="${esc(server.name)} 用于">${MCP_AGENTS.map(([id, label]) => `<button type="button" class="mcp-agent${server.agents?.[id] ? ' on' : ''}" data-act="mcpAgent" data-name="${esc(server.name)}" data-agent="${id}" aria-pressed="${server.agents?.[id] ? 'true' : 'false'}" ${on ? '' : 'disabled'}><img src="agent-brand/${id}-app-official.png" alt="" width="16" height="16">${label}</button>`).join('')}</div>
    <button type="button" class="icon-btn" data-act="mcpEdit" data-name="${esc(server.name)}" aria-label="${esc('编辑 ' + server.name)}" title="编辑">${icon('edit')}</button>
    <label class="switch" title="${on ? '已启用' : '已停用'}"><input type="checkbox" data-mcp-toggle="${esc(server.name)}" ${on ? 'checked' : ''} aria-label="${esc('启用 ' + server.name)}"><span></span></label>
  </div>`;
}

function renderMCPPage(view) {
  const data = MCP.data;
  if (!data) return;
  const discovered = data.discovered || [];
  const errors = Object.entries(data.errors || {});
  view.innerHTML = `${discovered.length ? `<section class="mcp-banner"><span class="key-glow" aria-hidden="true"></span><span class="mcp-banner-ico">${icon('mcp')}</span><div><b>一次配置，所有 Agent 都能用</b><small>在 ${esc([...new Set(discovered.flatMap((d) => d.from))].map((id) => id === 'codex' ? 'Codex' : 'Claude Code').join(' 和 '))} 里发现 ${discovered.length} 个还没同步的服务器：${esc(discovered.slice(0, 3).map((d) => d.name).join('、'))}${discovered.length > 3 ? ' 等' : ''}</small></div><button class="btn" data-act="mcpImport">全部导入</button></section>` : ''}
    ${errors.map(([, msg]) => `<div class="callout warn">${ico('warn')}<div>${esc(msg)}</div></div>`).join('')}
    ${data.servers.length ? `<section class="mcp-list">${data.servers.map(mcpRowHTML).join('')}</section>` : `<section class="mcp-empty"><span class="mcp-empty-ico">${icon('mcp')}</span><b>还没有 MCP 服务器</b><small>添加后会自动写入 Codex 和 Claude Code，不用分别配置。</small><button class="btn primary" data-act="mcpAdd">${icon('plus')}添加 MCP</button></section>`}
    <p class="su-foot">${icon('lock')} 只修改 Bridge 添加的条目；你在 Codex 或 Claude Code 里自己加的服务器不会被改动。</p>`;
}

async function loadMCP(view) {
  const seq = ++MCP.seq;
  try {
    const data = await api('/api/mcp');
    if (seq !== MCP.seq || S.route !== 'mcp' || view.isConnected === false) return;
    MCP.data = data;
    renderMCPPage(view);
  } catch (e) {
    if (seq === MCP.seq && S.route === 'mcp') view.innerHTML = `<div class="callout bad">${ico('warn')}<div>${esc(e.message)}</div></div>`;
  }
}
function mcpApply(data) {
  MCP.data = data;
  const view = $('#view');
  if (view && S.route === 'mcp') renderMCPPage(view);
  if (data.errors?.sync) toast(data.errors.sync, 'bad');
}

RENDER.mcp = function (view) {
  $('#headActions').innerHTML = `<button class="btn primary" data-act="mcpAdd">${icon('plus')}添加</button>`;
  if (MCP.data) renderMCPPage(view);
  return loadMCP(view);
};

function mcpEditor(server) {
  const editing = !!server;
  const s = server || { name: '', command: '', args: [], url: '', env: {}, agents: { codex: true, claude: true }, enabled: true };
  const remote = !!s.url;
  const env = Object.entries(s.env || {}).map(([k, v]) => `${k}=${v}`).join('\n');
  $('#modalRoot').innerHTML = `<div class="modal-back" data-act="closeModal"><div class="modal mcp-editor" data-stop role="dialog" aria-modal="true" aria-labelledby="mcpTitle">
    <div class="modal-head"><div><h3 id="mcpTitle">${editing ? '编辑 ' + esc(s.name) : '添加 MCP 服务器'}</h3><p>${editing ? '修改后会同步到选中的 Agent' : '选一个常用的，或者粘贴命令、地址'}</p></div><button class="icon-btn" data-act="closeModal" aria-label="关闭">${icon('close')}</button></div>
    <div class="modal-body">
      ${editing ? '' : `<div class="mcp-presets">${MCP_PRESETS.map((p) => `<button type="button" class="mcp-preset" data-act="mcpPreset" data-name="${esc(p.name)}"><span class="mcp-ico" data-face="${mcpFace(p.name)}">${icon(p.icon)}</span><b>${esc(p.label)}</b><small>${esc(p.note)}</small></button>`).join('')}</div>`}
      <input type="hidden" id="mcpPrev" value="${esc(editing ? s.name : '')}">
      <div class="form-grid">
        <div class="field"><label for="mcpName">名称</label><input class="input" id="mcpName" maxlength="64" placeholder="例如 playwright" value="${esc(s.name)}"></div>
        <div class="field"><label>类型</label><div class="seg mcp-kind" role="radiogroup"><button type="button" role="radio" class="${remote ? '' : 'on'}" aria-checked="${!remote}" data-act="mcpKind" data-kind="command">本地命令</button><button type="button" role="radio" class="${remote ? 'on' : ''}" aria-checked="${remote}" data-act="mcpKind" data-kind="url">远程地址</button></div></div>
        <div class="field full" id="mcpCommandField" ${remote ? 'hidden' : ''}><label for="mcpCommand">启动命令</label><input class="input mono" id="mcpCommand" placeholder="npx -y @scope/server" value="${esc(remote ? '' : mcpCommandText(s))}"></div>
        <div class="field full" id="mcpUrlField" ${remote ? '' : 'hidden'}><label for="mcpUrl">服务地址</label><input class="input mono" id="mcpUrl" placeholder="https://example.com/mcp" value="${esc(s.url || '')}"></div>
        <div class="field full"><label for="mcpEnv">环境变量（每行 KEY=VALUE，可选）</label><textarea class="input mono" id="mcpEnv" rows="2" placeholder="GITHUB_TOKEN=...">${esc(env)}</textarea></div>
        <div class="field full"><label>给哪些 Agent 用</label><div class="mcp-agent-pick">${MCP_AGENTS.map(([id, label]) => `<label class="mcp-check"><input type="checkbox" data-mcp-agent="${id}" ${s.agents?.[id] ? 'checked' : ''}><img src="agent-brand/${id}-app-official.png" alt="" width="18" height="18">${label}</label>`).join('')}</div></div>
      </div>
      <div id="mcpEditStatus" class="muted small" role="status" aria-live="polite"></div>
    </div>
    <div class="modal-foot">${editing ? `<button class="btn ghost danger-text" data-act="mcpRemove" data-name="${esc(s.name)}">${icon('trash')}删除</button>` : ''}<span class="spacer"></span><button class="btn ghost" data-act="closeModal">取消</button><button class="btn primary" data-act="mcpSave">${editing ? '保存' : '添加'}</button></div>
  </div></div>`;
  MCP.editor = { field: $('#mcpPrev'), view: $('#view'), route: S.route };
  $(editing ? '#mcpCommand' : '#mcpName')?.focus();
}

function currentMCPEditor(owner) {
  return owner && owner === MCP.editor && owner.field?.isConnected !== false
    && $('#mcpPrev') === owner.field && $('#view') === owner.view && S.route === owner.route;
}
function mcpEditorStatus(owner, message) {
  if (currentMCPEditor(owner)) {
    const status = $('#mcpEditStatus');
    if (status) status.textContent = message;
  }
}
function mcpEditorPending(owner, pending) {
  owner.pending = pending;
  if (!currentMCPEditor(owner)) return;
  $$('[data-act="mcpSave"], [data-act="mcpRemove"]', $('#modalRoot')).forEach((button) => { button.disabled = pending; });
}

function mcpFormServer() {
  const remote = !$('#mcpUrlField').hidden;
  const env = {};
  for (const line of $('#mcpEnv').value.split(/\r?\n/)) {
    const i = line.indexOf('=');
    if (i > 0) env[line.slice(0, i).trim()] = line.slice(i + 1).trim();
  }
  const parts = remote ? [] : mcpSplitCommand($('#mcpCommand').value);
  const agents = {};
  $$('[data-mcp-agent]').forEach((el) => { agents[el.dataset.mcpAgent] = el.checked; });
  const prev = $('#mcpPrev').value;
  const current = MCP.data?.servers?.find((x) => x.name === prev);
  return {
    name: $('#mcpName').value.trim(),
    command: remote ? '' : parts[0] || '',
    args: remote ? [] : parts.slice(1),
    url: remote ? $('#mcpUrl').value.trim() : '',
    env,
    agents,
    enabled: current ? current.enabled !== false : true,
  };
}

Object.assign(ACTIONS, {
  mcpAdd: () => mcpEditor(null),
  mcpEdit: (b) => mcpEditor(MCP.data?.servers?.find((x) => x.name === b.dataset.name) || null),
  mcpPreset: (b) => {
    const p = MCP_PRESETS.find((x) => x.name === b.dataset.name);
    if (!p) return;
    $$('.mcp-preset').forEach((x) => x.classList.toggle('on', x === b));
    $('#mcpName').value = p.name;
    ACTIONS.mcpKind({ dataset: { kind: 'command' } });
    $('#mcpCommand').value = p.command;
    $('#mcpCommand').focus();
  },
  mcpKind: (b) => {
    const url = b.dataset.kind === 'url';
    $('#mcpUrlField').hidden = !url;
    $('#mcpCommandField').hidden = url;
    $$('.mcp-kind button').forEach((x) => { const on = (x.dataset.kind === 'url') === url; x.classList.toggle('on', on); x.setAttribute('aria-checked', String(on)); });
  },
  mcpSave: async (b) => {
    const owner = MCP.editor;
    if (!currentMCPEditor(owner) || owner.pending || b?.isConnected === false) return;
    const server = mcpFormServer();
    const previousName = owner.field.value, draft = JSON.stringify(server);
    if (!server.name) throw new Error('请填写名称');
    if (!server.command && !server.url) throw new Error('请填写启动命令或服务地址');
    mcpEditorPending(owner, true);
    try { await busy(b, async () => {
      mcpEditorStatus(owner, '正在保存并同步…');
      try {
        const data = await api('/api/mcp/save', { server, previousName });
        const current = currentMCPEditor(owner);
        const unchanged = current && JSON.stringify(mcpFormServer()) === draft;
        mcpApply(data);
        if (!current) return;
        // A rename may have committed even when synchronizing an Agent failed.
        if (data.servers?.some((row) => row.name === server.name)) owner.field.value = server.name;
        if (data.errors?.sync) { mcpEditorStatus(owner, '配置已保存，同步未完成：' + data.errors.sync); return; }
        if (!unchanged) { mcpEditorStatus(owner, '已保存之前的内容；当前修改尚未保存。'); return; }
        $('#modalRoot').innerHTML = '';
        MCP.editor = null;
        toast('已同步，重新打开 Codex 或 Claude Code 后生效', 'ok');
      } catch (error) { mcpEditorStatus(owner, '未确认保存，编辑内容已保留：' + (error?.message || String(error))); }
    }); } finally { mcpEditorPending(owner, false); }
  },
  mcpRemove: async (b) => {
    const owner = MCP.editor;
    if (!currentMCPEditor(owner) || owner.pending || b?.isConnected === false) return;
    if (!await confirm(`删除「${b.dataset.name}」？会从 Codex 和 Claude Code 里一起移除。`)) return;
    if (!currentMCPEditor(owner) || owner.pending || b?.isConnected === false) return;
    mcpEditorPending(owner, true);
    try { await busy(b, async () => {
      try {
        const data = await api('/api/mcp/remove', { name: b.dataset.name });
        mcpApply(data);
        if (!currentMCPEditor(owner)) return;
        $('#modalRoot').innerHTML = '';
        MCP.editor = null;
        toast('已删除', 'ok');
      } catch (error) { mcpEditorStatus(owner, '未确认删除：' + (error?.message || String(error))); }
    }); } finally { mcpEditorPending(owner, false); }
  },
  mcpAgent: async (b) => {
    const on = b.getAttribute('aria-pressed') !== 'true';
    await busy(b, async () => mcpApply(await api('/api/mcp/toggle', { name: b.dataset.name, agent: b.dataset.agent, enabled: on })));
  },
  mcpImport: async (b) => {
    await busy(b, async () => {
      const data = await api('/api/mcp/import', {});
      mcpApply(data);
      if (!data.errors?.sync) toast('已导入并同步，重新打开 Agent 后生效', 'ok');
    });
  },
});

document.addEventListener('change', async (e) => {
  const name = e.target?.dataset?.mcpToggle;
  if (!name) return;
  e.target.disabled = true;
  try { mcpApply(await api('/api/mcp/toggle', { name, enabled: e.target.checked })); }
  catch (err) { e.target.checked = !e.target.checked; toast(err.message, 'bad'); }
  finally { e.target.disabled = false; }
});
