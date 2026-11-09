'use strict';
/* Agent page: per tool, pick an API key → load the upstream models → (Codex) optionally put those
   models into Codex's own model picker → open the tool. Any model family works; the local gateway
   converts the wire protocol per request, so switching to Grok/Claude inside Codex just works. */

PAGES.overview.title = 'Agent';
PAGES.overview.sub = '给每个 Agent 选一把密钥，然后打开或让手机接手';
PAGES.accounts.title = 'API 密钥';
PAGES.accounts.sub = '共享密钥库 · 本机保存';

Object.assign(ICONS, {
  swap: '<path d="M7 7h12l-3-3M17 17H5l3 3"/>',
  lock: '<rect x="5" y="11" width="14" height="9" rx="2"/><path d="M8 11V8a4 4 0 0 1 8 0v3"/>',
  external: '<path d="M14 4h6v6M20 4l-9 9M18 14v5a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V7a1 1 0 0 1 1-1h5"/>',
  dots: '<circle cx="5" cy="12" r="1.3"/><circle cx="12" cy="12" r="1.3"/><circle cx="19" cy="12" r="1.3"/>',
  route: '<circle cx="6" cy="18" r="2"/><circle cx="18" cy="6" r="2"/><path d="M8 18h5a4 4 0 0 0 0-8h-2a4 4 0 0 1 0-8h5"/>',
});

const WB = { query: '', target: null, manual: {}, poll: 0, discoveryDelay: 500, cardSignature: '', accountOpen: null, launchPending: false, launchGeneration: 0 };
const NATIVE = { codex: 'responses', claude: 'anthropic', 'deepseek-harness': 'chat', hermes: 'chat' };
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

// The tools' own icons (bundled under agent-brand/) identify each Agent; CLI builds get a terminal badge.
function wbAgentIcon(t) {
  const kind = toolKind(t), cli = !String(t.id).endsWith('-desktop');
  const family = ['codex', 'claude', 'deepseek-harness', 'hermes'].includes(kind) ? kind : '';
  const art = family ? `<img src="agent-brand/${family}-app-official.png" alt="" width="48" height="48">` : `<span class="agent-letter">${esc(String(t.name || '?').slice(0, 2))}</span>`;
  return `<span class="agent-icon" aria-hidden="true">${art}${cli ? `<span class="agent-cli">${icon('terminal')}</span>` : ''}</span>`;
}
function toolKind(t) { return t.kind || ({ 'codex-desktop': 'codex', 'claude-desktop': 'claude' })[t.id] || String(t.id); }
function bindingFor(t) { return (S.local && S.local.bindings && S.local.bindings[t.id]) || {}; }
function accountFor(id) { return (S.local && S.local.accounts || []).find((a) => a.id === id); }

const TOOL_OPEN = { 'codex-desktop': '打开 Codex', codex: '打开 Codex CLI', claude: '打开 Claude Code', 'deepseek-harness': '打开 DeepSeek Harness', hermes: '打开 Hermes Agent' };

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
  return !t.available ? '<span class="chip">未安装</span>' : wbAccountAllowed(a, t.id) && b.pending ? '<span class="chip warn">待应用</span>' : '';
}

const wbMore = (body) => `<details class="wb-card-more"><summary aria-label="更多操作" title="更多操作">${icon('dots')}</summary><div class="wb-more-body">${body}</div></details>`;

// Shared presentation keeps ordinary tools and the Claude Desktop adapter aligned.
// Status nodes are independent of the key selector, so background updates preserve its focus.
function wbCardHeader(t, menu = '', status = wbCardStatus(t)) {
  const subtitle = t.pending ? '正在检测…' : `${t.id === 'deepseek-harness' ? 'Web · SDK' : String(t.id).endsWith('-desktop') ? '桌面应用' : '命令行'}${t.version ? ' · ' + esc(t.version) : ''}`;
  return `<div class="agent-head">${wbAgentIcon(t)}
    <div class="wb-main"><div class="wb-name">${esc(t.name)}<span id="wbStatus-${esc(t.id)}">${status}</span></div><div class="wb-sub">${subtitle}</div></div>
    ${menu}</div>`;
}
function wbCardControls(t, picker, action) {
  const lamp = agentLocalStatus(t);
  return `<div class="agent-body"><span class="agent-api-label">使用的 API</span>${picker}</div>
    <div class="agent-foot"><span class="agent-remote" id="lamp-${esc(t.id)}">${remoteLampHTML(lamp, 'agent')}</span>${action}</div>`;
}

// An absent tool keeps its installation action in the same footer position as Open.
const wbMissing = (t) => wbCardControls(t,
  `<span class="wb-missing">这台电脑还没有安装 ${esc(t.name)}</span>`,
  `<a class="btn wb-install" href="#setup">${icon('box')}一键安装</a>`);

function wbCard(t) {
  // Still discovering: show the tile's shape with a quiet "检测中" instead of a wrong "未安装".
  if (t.pending) return `<article class="wb-card agent-tile is-pending" data-tool="${esc(t.id)}" data-available="true" data-family="${esc(toolKind(t))}" aria-busy="true">
    ${wbCardHeader(t, '', '')}
    <div class="agent-body"><span class="agent-api-label">使用的 API</span><span class="sk" style="width:100%;height:38px" aria-hidden="true"></span></div>
    <div class="agent-foot" aria-hidden="true"><span class="sk" style="width:110px;height:12px"></span><span class="sk" style="width:96px;height:34px"></span></div>
  </article>`;
  if (t.id === 'claude-desktop' && typeof wbClaudeDesktopCard === 'function') return wbClaudeDesktopCard(t);
  const kind = toolKind(t), desktop = t.id.endsWith('-desktop');
  const lamp = agentLocalStatus(t);
  if (t.id === 'claude-desktop') {
    // Fallback when the dedicated Claude Desktop adapter is absent: manual setup only.
    const menu = wbMore(`<p class="wb-note">在 Claude Desktop 的 Developer 菜单配置 API；Bridge 不会自动改写或重启它。普通云端聊天不能由 Bridge 遥控。</p>`);
    return `<article class="wb-card agent-tile" data-tool="${esc(t.id)}" data-available="${t.available}" data-family="claude">
      ${wbCardHeader(t, menu)}
      ${wbCardControls(t, '<span class="wb-missing">在 Claude Desktop 里设置</span>', '')}
    </article>`;
  }
  const b = bindingFor(t), savedAccount = accountFor(b.accountId);
  const a = wbAccountAllowed(savedAccount, t.id) ? savedAccount : null;
  const menu = wbMore(`${a ? `<button class="btn sm ghost" data-act="localEdit" data-id="${esc(a.id)}">${icon('edit')}编辑密钥</button>` : ''}
    ${b.appliedId ? `<button class="btn sm ghost" data-act="localRestore" data-kind="${esc(kind)}">${icon('refresh')}恢复原设置</button>` : ''}
    <p class="wb-note" id="remoteDetail-${esc(t.id)}">${esc(lamp.detail || '')}</p>`);
  const controls = t.available ? wbCardControls(t,
    `<span class="wb-key-wrap"><span class="wb-key-face" data-face="${a ? keyCardFace(a.id) : 'none'}"></span><select class="input wb-key" id="wbKey-${esc(t.id)}" aria-label="${esc(t.name)} 使用的 API 密钥" data-wb-key="${esc(t.id)}">
      <option value="" disabled ${wbAccountAllowed(a, t.id) ? '' : 'selected'}>选择 API 密钥</option>
      ${(S.local.accounts || []).filter(x => wbAccountAllowed(x, t.id)).map((x) => `<option value="${esc(x.id)}" ${x.id === b.accountId ? 'selected' : ''}>${esc(x.name)} · ${esc(wbHost(x.baseUrl))}</option>`).join('')}
      <option value="__new">＋ 添加 API 密钥…</option>
    </select></span>`,
    `<button class="btn primary" data-act="wbOpen" data-target="${esc(t.id)}" ${a ? '' : 'disabled'}>${icon('play')}打开</button>`) : wbMissing(t);
  return `<article class="wb-card agent-tile" data-tool="${esc(t.id)}" data-available="${t.available}" data-family="${esc(kind)}">
    ${wbCardHeader(t, menu)}${controls}
  </article>`;
}

// Status pushes only refresh the lamp/chip, without replacing the API select or open dialog.
function wbRefreshCardStatus(target) {
  const t = S.local.tools.find((x) => x.id === target);
  if (!t) return;
  const lamp = document.getElementById('lamp-' + target);
  if (lamp) lamp.innerHTML = remoteLampHTML(agentLocalStatus(t), 'agent');
  const status = document.getElementById('wbStatus-' + target);
  if (status) status.innerHTML = wbCardStatus(t);
}

/* The open dialog holds the per-tool settings: upstream models and the Codex picker override. */
function wbOpenModal(target, error, draft) {
  const t = S.local.tools.find((x) => x.id === target);
  if (!t) return;
  const bd = draft ? { ...bindingFor(t), accountId: draft.id, override: draft.override } : bindingFor(t), a = accountFor(bd.accountId);
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
      <p class="wb-hint">${icon('route')}模型在原软件内选，使用期间请保持 Bridge 运行。${converts ? '当前 API 包含需要转换接口的模型。' : ''}Bridge 重启后请重新应用配置。</p>
      <p class="wb-hint">${icon('info')}${t.id === 'deepseek-harness' || t.id === 'hermes' ? '使用所选 API 启动原生 Agent，保留已有记录。' : desktop ? '重启原应用，保留聊天和项目。' : '打开终端，可在原工具选择会话。'}</p>
      <label class="wb-check-row"><input type="checkbox" id="wbReady" ${draft?.ready ? 'checked' : ''}>同系列工具任务已经结束</label>
      ${wbLaunchFeedback(draft, error)}
    </div>
    <div class="modal-foot"><button class="btn ghost" data-act="closeModal">取消</button><span class="spacer"></span><button class="btn" data-act="wbApplyGo" data-apply-only="true" data-id="${esc(a.id)}" data-target="${esc(t.id)}" ${!draft?.verified || !draft.ready || draft.loading ? 'disabled' : ''}>仅应用配置</button><button class="btn primary" data-act="wbOpenGo" data-id="${esc(a.id)}" data-target="${esc(t.id)}" ${!draft?.verified || !draft.ready || draft.loading ? 'disabled' : ''}>${icon('external')}确认并打开</button></div>
  </div></div>`;
}

// Account-card launches stage a selection in the existing dialog. Opening,
// refreshing a directory or toggling its picker never changes a tool binding.
function wbAccountAllowed(account, target) {
  if (!account || account.libraryOnly || account.officialAccount?.mode === 'chatgpt_web_session' || account.mode === 'chatgpt_web_session') return false;
  return !(account.officialAccount || account.sourceType === 'official_account') || target === 'codex' || target === 'codex-desktop';
}
function wbAccountStamp(account) {
  return JSON.stringify([account?.id, account?.sourceRevision, account?.name, account?.baseUrl, account?.authMode, account?.wire, account?.protocol, account?.sourceType, account?.keyMasked, account?.officialAccount?.id, account?.officialAccount?.mode, account?.officialAccount?.identityEpoch]);
}
function wbAccountCurrent(draft) {
  return !!draft && WB.accountOpen === draft && draft.generation === WB.launchGeneration && S.route === draft.route && S.routeRevision === draft.routeRevision && (draft.node ? $(draft.title) === draft.node : $('#modalRoot')?.innerHTML === draft.anchorHTML);
}
function wbLaunchFeedback(draft, error) {
  const status = draft?.verifying ? '正在检查账号与工具…' : draft?.loading ? '正在读取模型目录…' : draft?.verified ? '确认后才会保存选择并配置工具，取消会保留当前配置。' : '';
  return `<div id="switchError" aria-live="polite">${status ? `<p class="wb-hint" role="status">${esc(status)}</p>` : ''}${draft?.catalogError ? `<p class="wb-hint">${esc(draft.catalogError)} · 可重新加载，或沿用原工具的模型设置。</p>` : ''}${error ? `<div class="callout warn"><div>${esc(error)}${draft?.executionUnknown ? '<br>请先查看原工具的启动状态；本次不会自动再次启动。' : !draft?.verified ? '<br><button class="btn sm" data-act="wbRecheck">重新检查</button>' : ''}</div></div>` : ''}</div>`;
}
function wbUpdateLaunchControls(draft) {
  if (!wbAccountCurrent(draft)) return;
  const go = $('#modalRoot')?.querySelector(`[data-act="${draft.target === 'claude-desktop' ? 'cdGo' : 'wbOpenGo'}"]`);
  if (go) go.disabled = WB.launchPending || !draft.verified || !draft.ready || draft.loading || draft.target === 'claude-desktop' && (!draft.status?.supported || draft.status.managed);
  const apply = $('#modalRoot')?.querySelector('[data-act="wbApplyGo"]');
  if (apply) apply.disabled = WB.launchPending || !draft.verified || !draft.ready || draft.loading;
}
function wbAccountRender(draft, error) {
  if (WB.accountOpen !== draft || S.route !== draft.route) return;
  const account = accountFor(draft.id);
  if (!wbAccountAllowed(account, draft.target)) {
    draft.verified = false;
    $('#modalRoot').innerHTML = '';
    WB.accountOpen = null;
    toast(error || '账号已变化，请重新选择', 'bad');
    return;
  }
  if (draft.target === 'claude-desktop') cdModal(account, draft.status, error, draft);
  else wbOpenModal(draft.target, error, draft);
  draft.node = $(draft.title);
}
async function wbRefreshLaunchCards() {
  const route = S.route, routeRevision = S.routeRevision, revision = S.localAccountsRevision = (S.localAccountsRevision || 0) + 1;
  const current = () => revision === S.localAccountsRevision && S.route === route && S.routeRevision === routeRevision;
  let local;
  try { local = await api('/api/local/accounts'); }
  catch (error) {
    if (current() && typeof accountActiveUnavailable === 'function') accountActiveUnavailable();
    throw error;
  }
  if (!current()) return;
  S.local = local;
  renderAgentCards();
  if (route === 'accounts' && typeof renderLocalCards === 'function') renderLocalCards();
}
async function wbOpenAccount(target, id, button) {
  if (button?.isConnected === false || WB.launchPending) return;
  if (!['codex-desktop', 'codex', 'claude', 'claude-desktop', 'deepseek-harness', 'hermes'].includes(target)) throw new Error('不支持此工具');
  const tool = S.local?.tools?.find(item => item.id === target), account = accountFor(id);
  if (!wbAccountAllowed(account, target)) throw new Error('此账号不能用于该工具');
  if (target === 'claude-desktop' && typeof cdModal !== 'function') throw new Error('Claude Desktop 配置组件尚未就绪');
  if (wbAccountCurrent(WB.accountOpen) && WB.accountOpen.target === target && WB.accountOpen.id === id && (WB.accountOpen.verifying || WB.accountOpen.verified)) return;
  const binding = tool ? bindingFor(tool) : {};
  const draft = { target, id, route: S.route, routeRevision: S.routeRevision, generation: ++WB.launchGeneration, title: target === 'claude-desktop' ? '#cdTitle' : '#wbOpenTitle', node: null, anchorHTML: $('#modalRoot')?.innerHTML, ready: false, override: target === 'claude-desktop' ? binding.override !== false : !!binding.override, status: null, loading: false, verifying: true, verified: false };
  WB.accountOpen = draft;
  if (tool) wbAccountRender(draft);
  const revision = S.localAccountsRevision = (S.localAccountsRevision || 0) + 1;
  try {
    const fresh = await api('/api/local/accounts');
    if (!wbAccountCurrent(draft)) return;
    const currentAccount = fresh.accounts?.find(item => item.id === id), currentTool = fresh.tools?.find(item => item.id === target);
    if (!wbAccountAllowed(currentAccount, target)) throw new Error('此账号已移除或不能用于该工具');
    if (!/^src-v1-[a-f0-9]{64}$/.test(currentAccount.sourceRevision || '')) throw new Error('请刷新或更新电脑端后重新打开');
    if (!currentTool?.available) throw new Error('请先在环境中安装或识别该工具');
    if (revision !== S.localAccountsRevision) throw new Error('工具状态已变化，请重新打开');
    S.local = fresh;
    draft.sourceRevision = currentAccount.sourceRevision;
    draft.sourceStamp = wbAccountStamp(currentAccount);
    // Confirmation on the cached summary must never carry into a newly read source.
    draft.ready = false;
    draft.verified = true;
    draft.verifying = false;
    wbAccountRender(draft);
  } catch (error) {
    draft.verifying = false;
    if (wbAccountCurrent(draft)) { if (draft.node) wbAccountRender(draft, error.message); else toast(error.message, 'bad'); }
    return;
  } finally { draft.verifying = false; }
  if (target === 'claude-desktop') await wbAccountStatus(draft);
  else if (!modelsOf(accountFor(id)).length) await wbAccountModels(draft, true);
}
async function wbAccountStatus(draft) {
  try {
    const status = await api('/api/local/claude-desktop/status');
    if (!wbAccountCurrent(draft)) return;
    draft.status = status;
    wbAccountRender(draft);
  } catch (error) {
    if (wbAccountCurrent(draft)) { draft.status = { supported: false }; draft.verified = false; draft.ready = false; wbAccountRender(draft, error.message); }
  }
}
async function wbAccountModels(draft, quiet) {
  if (!wbAccountCurrent(draft) || !draft.verified || draft.loading || WB.launchPending) return;
  draft.loading = true;
  draft.catalogError = '';
  wbAccountRender(draft);
  try {
    const result = await api('/api/local/models', { id: draft.id });
    if (!wbAccountCurrent(draft)) return;
    const revision = S.localAccountsRevision = (S.localAccountsRevision || 0) + 1;
    const local = await api('/api/local/accounts');
    if (!wbAccountCurrent(draft) || revision !== S.localAccountsRevision) return;
    S.local = local;
    if (!wbAccountAllowed(accountFor(draft.id), draft.target) || wbAccountStamp(accountFor(draft.id)) !== draft.sourceStamp) {
      draft.verified = false; draft.ready = false;
      wbAccountRender(draft, '账号已变化，请重新打开后确认');
      return;
    }
    renderAgentCards();
    if (S.route === 'accounts' && typeof renderLocalCards === 'function') renderLocalCards();
    if (!quiet) toast(`加载了 ${result.models.length} 个模型`, 'ok');
  } catch (error) {
    if (wbAccountCurrent(draft)) draft.catalogError = error.message || '模型目录暂不可用';
  } finally { const current = wbAccountCurrent(draft); draft.loading = false; if (current) wbAccountRender(draft, !draft.verified ? '账号已变化，请重新检查' : undefined); }
}
async function wbAccountGo(button) {
  const draft = WB.accountOpen;
  if (!wbAccountCurrent(draft) || !draft.verified || WB.launchPending || draft.loading || button?.isConnected === false || button?.dataset.id !== draft.id || (button.dataset.target && button.dataset.target !== draft.target)) return;
  const ready = $(draft.target === 'claude-desktop' ? '#cdReady' : '#wbReady')?.checked;
  if (!ready) { toast('请先结束正在进行的任务', 'bad'); return; }
  if (draft.target === 'claude-desktop' && (!draft.status?.supported || draft.status.managed)) return;
  draft.ready = true;
  const override = draft.override;
  const applyOnly = draft.target !== 'claude-desktop' && button.dataset.applyOnly === 'true';
  const progress = typeof beginLaunchProgress === 'function' ? beginLaunchProgress(draft.target, accountFor(draft.id)?.name, draft.target === 'claude-desktop' ? ['检查账号与工具', '提交启动请求'] : ['检查账号与工具', '保存工具选择', applyOnly ? '应用配置' : '提交启动请求'], applyOnly ? '应用配置' : '启动') : null;
  WB.launchPending = true;
  S.localAccountsRevision = (S.localAccountsRevision || 0) + 1;
  try {
    await busy(button, async () => {
      const fresh = await api('/api/local/accounts');
      if (!wbAccountCurrent(draft)) { progress?.cancel(); return; }
      const account = fresh.accounts?.find(item => item.id === draft.id), tool = fresh.tools?.find(item => item.id === draft.target);
      if (!tool?.available || !wbAccountAllowed(account, draft.target) || wbAccountStamp(account) !== draft.sourceStamp) {
        draft.verified = false; draft.ready = false;
        throw new Error('账号或工具已变化，请重新打开后确认');
      }
      if (draft.target === 'claude-desktop') {
        progress?.step(1);
        const result = await (typeof launchAPI === 'function' ? launchAPI : ((path, body) => api(path, body)))('/api/local/claude-desktop/switch', { id: draft.id, confirmed: true, allowModeChange: true, catalogOverride: override, expectedSourceRevision: draft.sourceRevision }, progress);
        if (result?.ok !== true) throw new Error('启动结果待确认，请检查原工具');
      } else {
        progress?.step(1);
        const binding = fresh.bindings?.[draft.target] || {}, model = (binding.accountId === draft.id ? binding.model : '') || account.model || defaultModel(account, toolKind(tool));
        const bound = await api('/api/local/bind', { target: draft.target, id: draft.id, model: model || '', protocol: inferProtocol(model, account.wire) || NATIVE[toolKind(tool)], ...(toolKind(tool) === 'codex' ? { override } : {}), expectedSourceRevision: draft.sourceRevision });
        if (bound?.ok !== true) throw new Error('保存结果待确认，请重新打开');
        // A close/navigation while the binding is saved cancels the launch.
        // It remains a pending selection; no rollback can overwrite another actor.
        if (!wbAccountCurrent(draft)) { progress?.cancel(); return; }
        progress?.step(2);
        const result = await (typeof launchAPI === 'function' ? launchAPI : ((path, body) => api(path, body)))('/api/local/switch', { id: draft.id, target: draft.target, confirmed: true, keepModel: true, ...(applyOnly ? { applyOnly: true } : {}), expectedSourceRevision: draft.sourceRevision }, progress);
        if (result?.ok !== true) throw new Error('启动结果待确认，请检查原工具');
      }
      progress?.finish();
      if (wbAccountCurrent(draft)) { $('#modalRoot').innerHTML = ''; WB.accountOpen = null; toast(applyOnly ? '配置已应用，未启动工具；已打开的工具请重新打开后使用' : '已打开', 'ok'); }
    });
  } catch (error) {
    progress?.fail(error);
    if (wbAccountCurrent(draft)) { draft.verified = false; draft.ready = false; draft.executionUnknown = error.uncertain === true || error.started === true; wbAccountRender(draft, error.message); }
  } finally {
    WB.launchPending = false;
    try { await wbRefreshLaunchCards(); } catch (_) { /* A completed launch never becomes an automatic retry when the display refresh fails. */ }
    wbUpdateLaunchControls(draft);
  }
}

const wbPlaceholderTools = () => ['codex-desktop', 'codex', 'claude', 'claude-desktop', 'deepseek-harness', 'hermes'].map((id) => ({
  id, kind: toolKind({ id }), name: TOOL_LABEL[id] || id, available: false, path: '', custom: false, pending: true,
}));
function renderWorkbench(view, local) {
  if (view.isConnected === false || S.route !== 'overview') return;
  if (!local.tools?.length && local.discoveryPending) local = { ...local, tools: wbPlaceholderTools() };
  S.local = local;
  $('#headActions').innerHTML = '';
  const empty = !local.accounts.length;
  view.innerHTML = `${empty ? `<div class="wb-empty"><div><h2>添加你的第一个 API 密钥</h2><p>保存一次服务商地址和 Key，多个 Agent 都可以选择使用。</p></div><button class="btn primary" data-act="localNew">${icon('plus')}添加 API 密钥</button></div>` : ''}
    <div id="approvalsBox"></div>
    <div class="wb-grid agent-grid" id="agentToolList">${ordered(local.tools).map(wbCard).join('')}</div>`;
  WB.cardSignature = workbenchCardSignature(local);
  renderApprovalsBox();
}
RENDER_overview = function (view) {
  // Paint immediately from the last local snapshot. Tool discovery (including
  // Windows AppX/PowerShell) refreshes the same cards in the background.
  // First visit: keep the page skeleton until the real tool list arrives, so no tool
  // flashes as "not installed" while discovery is still running.
  WB.discoveryDelay = 500;
  WB.cardSignature = '';
  if (S.local) renderWorkbench(view, S.local);
  clearTimeout(WB.poll);
  const target = view, routeRevision = S.routeRevision, revision = S.localAccountsRevision = (S.localAccountsRevision || 0) + 1;
  const current = () => target.isConnected !== false && S.route === 'overview' && S.routeRevision === routeRevision && revision === S.localAccountsRevision;
  void api('/api/local/accounts').then((local) => {
    if (!current()) return;
    renderWorkbench(target, local);
    if (local.discoveryPending || local.nativePending) wbPollDiscovery(target);
  }).catch((error) => {
    if (!current()) return;
    const retry = `<div class="callout warn" role="alert"><div>读取 Agent 状态失败：${esc(error.message)}<br><button class="btn sm" data-act="refresh">重新读取</button></div></div>`;
    if (S.local) target.insertAdjacentHTML('afterbegin', retry);
    else target.innerHTML = retry;
  });
};

function wbPollDiscovery(view) {
  clearTimeout(WB.poll);
  if (document.hidden) return;
  const delay = WB.discoveryDelay;
  WB.discoveryDelay = Math.min(4000, WB.discoveryDelay * 2);
  WB.poll = setTimeout(async () => {
    if (view.isConnected === false || S.route !== 'overview' || document.hidden) return;
    const local = await refreshWorkbenchStatus();
    if (view.isConnected === false || S.route !== 'overview') return;
    if (local) renderAgentCards();
    const latest = local || S.local;
    if (latest?.discoveryPending || latest?.nativePending || !local && S.bindingLoading) wbPollDiscovery(view);
    else WB.discoveryDelay = 500;
  }, delay);
}

document.addEventListener('visibilitychange', () => {
  if (!document.hidden && S.route === 'overview' && (S.local?.discoveryPending || S.local?.nativePending)) wbPollDiscovery($('#view'));
});

const TOOL_ORDER = ['codex-desktop', 'codex', 'claude', 'claude-desktop', 'deepseek-harness', 'hermes'];
const ordered = (tools) => [...tools].sort((x, y) => TOOL_ORDER.indexOf(x.id) - TOOL_ORDER.indexOf(y.id));

function renderAgentCards() {
  const box = $('#agentToolList');
  if (!box || !S.local) return;
  const signature = workbenchCardSignature(S.local);
  if (signature === WB.cardSignature) return;
  box.innerHTML = ordered(S.local.tools).map(wbCard).join('');
  WB.cardSignature = signature;
}

function workbenchCardSignature(local) {
  return JSON.stringify({ tools: local?.tools || [], accounts: local?.accounts || [], bindings: local?.bindings || {} });
}

function wbNewAccount(target) {
  const t = S.local.tools.find((x) => x.id === target);
  if (!t || !t.available) return;
  localEditor(null, toolKind(t));
  // Keep the continuation on this dialog's Save button; cancelling removes it entirely.
  const save = $('#modalRoot [data-act="localSave"]');
  if (save) save.dataset.wbTarget = target;
}

// All entry points share one draft and one explicit commit. A dropdown is a
// preview of a choice, not permission to persist or restart a tool.
async function wbStageSelection(target, id, select) {
  if (select) select.value = bindingFor({ id: target }).accountId || '';
  if (WB.launchPending || !id) return;
  if (id === '__new') { wbNewAccount(target); return; }
  await wbOpenAccount(target, id, select);
}
async function wbContinueWithSaved(target, id) {
  const route = S.route, revision = S.routeRevision, generation = WB.launchGeneration;
  const anchor = $('#modalRoot').innerHTML;
  const localRevision = S.localAccountsRevision = (S.localAccountsRevision || 0) + 1;
  const local = await api('/api/local/accounts');
  if (S.route !== route || S.routeRevision !== revision || WB.launchGeneration !== generation || localRevision !== S.localAccountsRevision || $('#modalRoot').innerHTML !== anchor) return;
  S.local = local;
  renderAgentCards();
  const tool = local.tools?.find(item => item.id === target);
  if (!tool?.available || !accountFor(id)) { toast('密钥已保存，请在 Agent 中选择', 'ok'); return; }
  await wbOpenAccount(target, id);
}

Object.assign(ACTIONS, {
  wbLoad: async (button) => {
    const draft = WB.accountOpen;
    if (wbAccountCurrent(draft) && draft.id === button.dataset.id && draft.target === button.dataset.target) await wbAccountModels(draft, false);
  },
  wbRecheck: async () => {
    const draft = WB.accountOpen;
    if (!wbAccountCurrent(draft) || WB.launchPending || draft.verifying || draft.loading || draft.executionUnknown) return;
    draft.verified = false;
    await wbOpenAccount(draft.target, draft.id);
  },
  wbOpen: async (button) => {
    const target = button.dataset.target, tool = S.local?.tools?.find(item => item.id === target);
    if (!tool) return;
    const id = bindingFor(tool).accountId;
    if (id) await wbOpenAccount(target, id, button);
  },
  wbOpenGo: wbAccountGo,
  wbApplyGo: wbAccountGo,
});

document.addEventListener('change', async (event) => {
  const el = event.target;
  if (!el.dataset) return;
  try {
    if (el.dataset.wbKey) { await wbStageSelection(el.dataset.wbKey, el.value, el); return; }
    const draft = WB.accountOpen;
    if (!wbAccountCurrent(draft)) return;
    if (el.dataset.wbOverride && draft.target === el.dataset.wbOverride) {
      if (WB.launchPending) { el.checked = draft.override; return; }
      draft.override = el.checked;
    } else if (el.id === 'wbReady') {
      draft.ready = el.checked;
      wbUpdateLaunchControls(draft);
    }
  } catch (error) { toast(error.message, 'bad'); renderAgentCards(); }
});
