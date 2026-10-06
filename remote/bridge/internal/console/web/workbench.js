'use strict';
/* Agent page: per tool, pick an API key → load the upstream models → (Codex) optionally put those
   models into Codex's own model picker → open the tool. Any model family works; the local gateway
   converts the wire protocol per request, so switching to Grok/Claude inside Codex just works. */

PAGES.overview.title = 'Agent';
PAGES.overview.sub = '选择 API，打开工具';
PAGES.accounts.title = 'API 密钥';
PAGES.accounts.sub = '共享密钥库 · 本机保存';

Object.assign(ICONS, {
  swap: '<path d="M7 7h12l-3-3M17 17H5l3 3"/>',
  lock: '<rect x="5" y="11" width="14" height="9" rx="2"/><path d="M8 11V8a4 4 0 0 1 8 0v3"/>',
  external: '<path d="M14 4h6v6M20 4l-9 9M18 14v5a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V7a1 1 0 0 1 1-1h5"/>',
  dots: '<circle cx="5" cy="12" r="1.3"/><circle cx="12" cy="12" r="1.3"/><circle cx="19" cy="12" r="1.3"/>',
  route: '<circle cx="6" cy="18" r="2"/><circle cx="18" cy="6" r="2"/><path d="M8 18h5a4 4 0 0 0 0-8h-2a4 4 0 0 1 0-8h5"/>',
});

const WB = { query: '', target: null, manual: {} };
const NATIVE = { codex: 'responses', claude: 'anthropic' };
const WIRE_LABEL = { responses: 'Responses', chat: 'Chat Completions', anthropic: 'Claude Messages' };

/** Same rule as the Bridge (config.InferProtocol): provider type wins, else the model family decides. */
function inferProtocol(model, wire) {
  if (wire === 'responses' || wire === 'chat' || wire === 'anthropic') return wire;
  const m = String(model || '').toLowerCase().trim().split('/').pop();
  if (!m) return '';
  if (m.startsWith('claude') || m.includes('anthropic')) return 'anthropic';
  if (/^(gpt|codex|o1|o3|o4|chatgpt)/.test(m)) return 'responses';
  if (/^(grok|deepseek|qwen|qwq|glm|kimi|moonshot|gemini|gemma|doubao|mistral|codestral|llama|minimax|abab|hunyuan|ernie|yi-|step-|baichuan|spark)/.test(m)) return 'chat';
  return '';
}

const BRANDS = [
  [/claude|anthropic/i, 'Claude', '#D97757', 'C'], [/^(gpt|o\d|codex|chatgpt)/i, 'OpenAI', '#10A37F', 'G'], [/grok/i, 'xAI', '#111827', 'X'],
  [/gemini|gemma/i, 'Google', '#4285F4', 'G'], [/deepseek/i, 'DeepSeek', '#4D6BFE', 'D'], [/qwen|qwq/i, 'Qwen', '#615CED', 'Q'],
  [/glm/i, 'GLM', '#3859FF', 'Z'], [/kimi|moonshot/i, 'Kimi', '#16191E', 'K'], [/doubao/i, '豆包', '#1E6FFF', '豆'], [/minimax|abab/i, 'MiniMax', '#E7344A', 'M'],
];
function brandOf(model) {
  const name = String(model || '').split('/').pop();
  const hit = BRANDS.find(([re]) => re.test(name));
  return hit ? { name: hit[1], color: hit[2], letter: hit[3] } : { name: '', color: '#8A93A9', letter: (name[0] || '?').toUpperCase() };
}
const brandDot = (model, size = 30) => { const b = brandOf(model); return `<span class="wb-brand" style="--b:${b.color};width:${size}px;height:${size}px;font-size:${Math.round(size * .42)}px">${esc(b.letter)}</span>`; };
const wbHost = (u) => { try { return new URL(u).host; } catch (_) { return u || ''; } };

function toolKind(t) { return t.kind || String(t.id).split('-')[0]; }
function bindingFor(t) { return (S.local && S.local.bindings && S.local.bindings[t.id]) || {}; }
function accountFor(id) { return (S.local && S.local.accounts || []).find((a) => a.id === id); }

const TOOL_OPEN = { 'codex-desktop': '打开 Codex', codex: '打开 Codex CLI', claude: '打开 Claude Code' };

// No default-model picker: the tool's own model menu is where models are chosen.
// Bridge still needs a starting model for config, so take a native one first.
function defaultModel(a, kind) {
  const list = modelsOf(a);
  return list.find((m) => inferProtocol(m, a.wire) === NATIVE[kind]) || list[0] || '';
}
function modelsOf(a) { return a ? [...new Set([...(a.models || []), ...(a.model ? [a.model] : [])])] : []; }
function mixed(a, kind) { return modelsOf(a).some((m) => { const p = inferProtocol(m, a.wire); return p && p !== NATIVE[kind]; }); }

function wbCardStatus(t) {
  const b = bindingFor(t), a = accountFor(b.accountId);
  return !t.available ? '<span class="chip">未安装</span>' : a && b.pending ? '<span class="chip warn">待应用</span>' : '';
}

// A tool that is not installed shows where to get it instead of a disabled key picker and button.
const wbMissing = (t) => `<span class="wb-missing">没有在这台电脑上找到 ${esc(t.name)}</span><a class="btn wb-install" href="#setup">${icon('box')}去安装</a>`;

const wbMore = (body) => `<details class="wb-card-more"><summary aria-label="更多操作" title="更多操作">${icon('dots')}</summary><div class="wb-more-body">${body}</div></details>`;

function wbCard(t) {
  if (t.id === 'claude-desktop' && typeof wbClaudeDesktopCard === 'function') return wbClaudeDesktopCard(t);
  const kind = toolKind(t), desktop = t.id.endsWith('-desktop');
  const logo = `<span class="tool-logo ${kind === 'codex' ? 'codex' : desktop ? 'desktop' : 'claude'}">${kind === 'codex' ? 'Cx' : desktop ? 'CD' : 'CC'}</span>`;
  const lamp = desktop ? desktopRemoteLamp(t) : bindingFor(t).remote || {};
  const lampHtml = `<span id="lamp-${esc(t.id)}">${remoteLampHTML(lamp, desktop ? 'desktop' : 'cli')}</span>`;
  const restoreAvailable = S.local.tools.some((x) => x.id === kind && x.available);
  const restoreButton = `<button class="btn sm ghost" data-act="toolRestore" data-target="${esc(t.id)}" ${restoreAvailable ? '' : 'disabled'}>${icon('clock')}${desktop ? '用 CLI 恢复会话' : '恢复会话'}</button>`;
  if (t.id === 'claude-desktop') {
    // Fallback when the dedicated Claude Desktop adapter is absent: manual setup only.
    return `<article class="wb-card" data-tool="${esc(t.id)}" data-available="${t.available}">
      ${logo}
      <div class="wb-main"><div class="wb-name">${esc(t.name)}<span id="wbStatus-${esc(t.id)}">${wbCardStatus(t)}</span></div>
        <div class="wb-sub">在原应用中设置 API${lampHtml}</div></div>
      <span></span><span></span>
      ${wbMore(`<button class="btn sm ghost" data-act="toolRestore" data-target="claude" ${restoreAvailable ? '' : 'disabled'}>${icon('clock')}用 CLI 恢复会话</button>
        <p class="wb-note">在 Claude Desktop 的 Developer 菜单配置 API；Bridge 不会自动改写或重启它。普通云端聊天不能由 Bridge 遥控。</p>`)}
    </article>`;
  }
  const b = bindingFor(t), a = accountFor(b.accountId);
  return `<article class="wb-card" data-tool="${esc(t.id)}" data-available="${t.available}">
    ${logo}
    <div class="wb-main"><div class="wb-name">${esc(t.name)}<span id="wbStatus-${esc(t.id)}">${wbCardStatus(t)}</span></div>
      <div class="wb-sub">${desktop ? '桌面应用' : '命令行'}${t.version ? ' · ' + esc(t.version) : ''}${lampHtml}</div></div>
    ${t.available ? `<select class="input wb-key" id="wbKey-${esc(t.id)}" aria-label="${esc(t.name)} 使用的 API 密钥" data-wb-key="${esc(t.id)}" ${t.available ? '' : 'disabled'}>
      <option value="">原工具自己的登录</option>
      ${(S.local.accounts || []).map((x) => `<option value="${esc(x.id)}" ${x.id === b.accountId ? 'selected' : ''}>${esc(x.name)} · ${esc(wbHost(x.baseUrl))}</option>`).join('')}
      <option value="__new">＋ 添加 API 密钥…</option>
    </select>
    <button class="btn primary" data-act="wbOpen" data-target="${esc(t.id)}" ${t.available && a ? '' : 'disabled'}>打开</button>` : wbMissing(t)}
    ${wbMore(`${restoreButton}
      ${a ? `<button class="btn sm ghost" data-act="localEdit" data-id="${esc(a.id)}">${icon('edit')}编辑密钥</button>` : ''}
      ${b.appliedId ? `<button class="btn sm ghost" data-act="localRestore" data-kind="${esc(kind)}">${icon('refresh')}恢复原设置</button>` : ''}
      <p class="wb-note" id="remoteDetail-${esc(t.id)}">${esc(lamp.detail || '')}</p>`)}
  </article>`;
}

// Status pushes only refresh the lamp/chip, without replacing the API select or open dialog.
function wbRefreshCardStatus(target) {
  const t = S.local.tools.find((x) => x.id === target);
  if (!t) return;
  const lamp = document.getElementById('lamp-' + target);
  if (lamp) lamp.innerHTML = remoteLampHTML(t.id.endsWith('-desktop') ? desktopRemoteLamp(t) : bindingFor(t).remote, t.id.endsWith('-desktop') ? 'desktop' : 'cli');
  const status = document.getElementById('wbStatus-' + target);
  if (status) status.innerHTML = wbCardStatus(t);
}

/* The open dialog holds the per-tool settings: upstream models and the Codex picker override. */
function wbOpenModal(target, error) {
  const t = S.local.tools.find((x) => x.id === target);
  const bd = bindingFor(t), a = accountFor(bd.accountId);
  if (!t || !a) { $('#modalRoot').innerHTML = ''; return; }
  const kind = toolKind(t), desktop = t.id.endsWith('-desktop'), models = modelsOf(a);
  const converts = mixed(a, kind) && (kind !== 'codex' || bd.override) || (bd.model && inferProtocol(bd.model, a.wire) && inferProtocol(bd.model, a.wire) !== NATIVE[kind]);
  $('#modalRoot').innerHTML = `<div class="modal-back" data-act="closeModal"><div class="modal wb-open" data-stop role="dialog" aria-modal="true" aria-labelledby="wbOpenTitle">
    <div class="modal-head"><h3 id="wbOpenTitle">${esc(TOOL_OPEN[t.id] || '打开')}</h3><button class="icon-btn" data-act="closeModal" aria-label="关闭">✕</button></div>
    <div class="modal-body">
      <div class="wb-open-sum"><span class="wb-brand empty">${icon('key')}</span><div><b>${esc(a.name)}</b><small>${esc(wbHost(a.baseUrl))}</small></div></div>
      <div class="wb-sec-title"><span>上游模型${models.length ? ` · ${models.length}` : ''}</span><button class="btn sm" data-act="wbLoad" data-target="${esc(t.id)}" data-id="${esc(a.id)}">${icon('refresh')}${models.length ? '重新加载' : '加载模型'}</button></div>
      ${models.length ? `<div class="wb-models">${models.map((m) => `<span class="wb-mchip">${brandDot(m, 16)}${esc(m)}</span>`).join('')}</div>` : '<div class="wb-empty-models">还没有加载，点右上角「加载模型」</div>'}
      ${kind === 'codex' && models.length ? `<label class="wb-toggle"><span class="switch"><input type="checkbox" data-wb-override="${esc(t.id)}" ${bd.override ? 'checked' : ''}><span></span></span>
        <span><b>覆盖 Codex 的模型选择栏</b></span></label>` : ''}
      ${converts ? `<p class="wb-hint">${icon('route')}模型在原软件内选，转换模式需保持 Bridge 运行。</p>` : ''}
      <p class="wb-hint">${icon('info')}${desktop ? '重启原应用，保留聊天和项目。' : '打开终端，继续原会话。'}</p>
      <label class="wb-check-row"><input type="checkbox" id="wbReady" checked>正在进行的任务已经结束</label>
      <div id="switchError" aria-live="polite">${error ? `<div class="callout warn" style="margin-top:12px">${ico('warn')}<div>${esc(error)}</div></div>` : ''}</div>
    </div>
    <div class="modal-foot"><button class="btn ghost" data-act="closeModal">取消</button><span class="spacer"></span><button class="btn primary" data-act="wbOpenGo" data-id="${esc(a.id)}" data-target="${esc(t.id)}">${icon('external')}打开</button></div>
  </div></div>`;
}

RENDER_overview = async function (view) {
  void loadState().catch(() => undefined);
  const local = await api('/api/local/accounts');
  if (view.isConnected === false) return;
  S.local = local;
  $('#headActions').innerHTML = `<a class="btn" href="#accounts">${icon('key')}API 密钥</a>`;
  const empty = !local.accounts.length;
  view.innerHTML = `${empty ? `<div class="wb-empty"><div><h2>添加你的第一个 API 密钥</h2><p>保存一次服务商地址和 Key，多个 Agent 都可以选择使用。</p></div><button class="btn primary" data-act="localNew">${icon('plus')}添加 API 密钥</button></div>` : ''}
    <div id="approvalsBox"></div>
    <div class="wb-grid" id="agentToolList">${ordered(local.tools).map(wbCard).join('')}</div>`;
  renderApprovalsBox();
};

const TOOL_ORDER = ['codex-desktop', 'codex', 'claude', 'claude-desktop'];
const ordered = (tools) => [...tools].sort((x, y) => TOOL_ORDER.indexOf(x.id) - TOOL_ORDER.indexOf(y.id));

function renderAgentCards() {
  const box = $('#agentToolList');
  if (box && S.local) box.innerHTML = ordered(S.local.tools).map(wbCard).join('');
}

async function wbBind(target, patch) {
  const t = S.local.tools.find((x) => x.id === target);
  const b = bindingFor(t);
  const id = 'id' in patch ? patch.id : b.accountId || '';
  const a = accountFor(id);
  let model = 'model' in patch ? patch.model : (b.accountId === id ? b.model : '') || (a && a.model) || '';
  if (a && !model) model = defaultModel(a, toolKind(t));
  const protocol = id ? (inferProtocol(model, a && a.wire) || NATIVE[toolKind(t)]) : '';
  const body = { target, id, model: model || '', protocol };
  if ('override' in patch) body.override = patch.override;
  await api('/api/local/bind', body);
  S.local = await api('/api/local/accounts');
  renderAgentCards();
  if ($('#wbOpenTitle')) wbOpenModal(target);
}

async function wbLoadModels(target, id, quiet) {
  try {
    const r = await api('/api/local/models', { id });
    S.local = await api('/api/local/accounts');
    const t = S.local.tools.find((x) => x.id === target), a = accountFor(id);
    if (!t || bindingFor(t).accountId !== id || !a) return;
    // Suggest the Codex picker override when the catalog has non-GPT models.
    const patch = {};
    if (a && !modelsOf(a).includes(bindingFor(t).model)) patch.model = defaultModel(a, toolKind(t));
    if (t && toolKind(t) === 'codex' && a && mixed(a, 'codex') && !bindingFor(t).override) patch.override = true;
    if (Object.keys(patch).length) await wbBind(target, patch);
    else { renderAgentCards(); if ($('#wbOpenTitle')) wbOpenModal(target); }
    toast(`加载了 ${r.models.length} 个模型`, 'ok');
  } catch (e) {
    if (!quiet) toast(e.message, 'bad'); else renderAgentCards();
  }
}

function wbNewAccount(target) {
  const t = S.local.tools.find((x) => x.id === target);
  if (!t || !t.available) return;
  localEditor(null, toolKind(t));
  // Keep the continuation on this dialog's Save button; cancelling removes it entirely.
  const save = $('#modalRoot [data-act="localSave"]');
  if (save) save.dataset.wbTarget = target;
}

async function wbContinueWithSaved(target, id) {
  S.local = await api('/api/local/accounts');
  const t = S.local.tools.find((x) => x.id === target);
  if (!t || !t.available || !accountFor(id)) {
    renderAgentCards();
    toast('密钥已保存，请在 Agent 中选择', 'ok');
    return;
  }
  await wbBind(target, { id, model: '', ...(target === 'claude-desktop' ? { override: true } : {}) });
  await ACTIONS.wbOpen({ dataset: { target } });
}

Object.assign(ACTIONS, {
  wbLoad: async (b) => { await busy(b, () => wbLoadModels(b.dataset.target, b.dataset.id, false)); },
  wbOpen: async (b) => {
    const t = S.local.tools.find((x) => x.id === b.dataset.target), a = accountFor(bindingFor(t).accountId);
    wbOpenModal(t.id);
    if (a && !modelsOf(a).length) await wbLoadModels(t.id, a.id, true);
  },
  wbOpenGo: async (b) => {
    if (!$('#wbReady')?.checked) { toast('请先结束正在进行的任务', 'bad'); return; }
    await busy(b, async () => {
      try {
        await api('/api/local/switch', { id: b.dataset.id, target: b.dataset.target, confirmed: true, keepModel: true });
        $('#modalRoot').innerHTML = '';
        S.local = await api('/api/local/accounts'); renderAgentCards();
        toast('已打开', 'ok');
      } catch (e) {
        wbOpenModal(b.dataset.target, e.message);
      }
    });
  },
});

document.addEventListener('change', async (e) => {
  const el = e.target;
  if (!el.dataset) return;
  try {
    if (el.dataset.wbKey) {
      if (el.value === '__new') { el.value = bindingFor({ id: el.dataset.wbKey }).accountId || ''; wbNewAccount(el.dataset.wbKey); return; }
      await wbBind(el.dataset.wbKey, { id: el.value, model: '' });
      if (el.value) await ACTIONS.wbOpen({ dataset: { target: el.dataset.wbKey } });
    } else if (el.dataset.wbOverride) {
      await wbBind(el.dataset.wbOverride, { override: el.checked });

    }
  } catch (err) { toast(err.message, 'bad'); renderAgentCards(); }
});
