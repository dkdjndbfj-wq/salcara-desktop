'use strict';
// Official Claude Desktop 3P configuration has its own backend transaction.
// Never route this card through the legacy isolated-profile / CLI writer.
const cdModels = (account) => [...new Set(account?.models || [])];

// The phone continues Claude Desktop's Code sessions through the Claude Code worker.
function cdLamp() { return desktopRemoteLamp({ id: 'claude-desktop' }); }

const cdMore = (body) => (typeof wbMore === 'function' ? wbMore(body) : `<details class="wb-card-more"><summary aria-label="更多操作">⋯</summary><div class="wb-more-body">${body}</div></details>`);

function wbClaudeDesktopCard(tool) {
  const binding = bindingFor(tool), savedAccount = accountFor(binding.accountId);
  const account = savedAccount && wbAccountAllowed(savedAccount, 'claude-desktop') ? savedAccount : null;
  const status = !tool.available ? '<span class="chip">未安装</span>' : account && binding.pending ? '<span class="chip warn">待应用</span>' : '';
  const menu = cdMore(`${account ? '<button class="btn sm ghost" data-act="wbConfigure" data-target="claude-desktop">API / 模型设置</button>' : ''}<button class="btn sm ghost" data-act="cdRestore">${icon('refresh')}恢复原配置</button>${account ? `<button class="btn sm ghost" data-act="localEdit" data-id="${esc(account.id)}">${icon('edit')}编辑密钥</button>` : ''}
    <p class="wb-note">沿用同一个第三方聊天库，不新建实例。</p>`);
  const controls = tool.available ? wbCardControls(tool,
    `<span class="wb-key-wrap"><span class="wb-key-face" data-face="${account && typeof keyCardFace === 'function' ? keyCardFace(account.id) : 'none'}"></span><select class="input wb-key" id="cdKey" aria-label="Claude Desktop 使用的 API 密钥" data-cd-key>
      <option value="" disabled ${account ? '' : 'selected'}>选择 API 密钥</option>${S.local.accounts.filter(item => wbAccountAllowed(item, 'claude-desktop')).map((item) => `<option value="${esc(item.id)}" ${item.id === binding.accountId ? 'selected' : ''}>${esc(item.name)} · ${esc(wbHost(item.baseUrl))}</option>`).join('')}
      <option value="__new">＋ 添加 API 密钥…</option>
    </select></span>`,
    `<button class="btn primary" data-act="cdOpen" ${account ? '' : 'disabled'}>${icon('play')}打开</button>`) : wbMissing(tool);
  return `<article class="wb-card agent-tile" data-tool="claude-desktop" data-available="${tool.available}" data-family="claude">
    ${wbCardHeader(tool, menu, status)}${controls}
  </article>`;
}

function cdModal(account, status, error, draft) {
  const binding = draft ? { override: draft.override } : bindingFor({ id: 'claude-desktop' }), models = cdModels(account);
  const enabled = status && status.supported && !status.managed && draft?.verified && !draft.loading && !WB.launchPending;
  $('#modalRoot').innerHTML = `<div class="modal-back" data-act="closeModal"><div class="modal wb-open" data-stop role="dialog" aria-modal="true" aria-labelledby="cdTitle">
    <div class="modal-head"><h3 id="cdTitle">打开 Claude Desktop</h3><button class="icon-btn" data-act="closeModal" aria-label="关闭">✕</button></div>
    <div class="modal-body"><div class="wb-open-sum"><span class="wb-brand empty">${icon('key')}</span><div><b>${esc(account.name)}</b><small>${esc(wbHost(account.baseUrl))}</small></div></div>
      <div class="wb-sec-title"><span>可用模型 · ${models.length}</span><button class="btn sm" data-act="cdLoad" data-id="${esc(account.id)}">${icon('refresh')}刷新模型</button></div>
      ${models.length ? `<div class="wb-models">${models.map((model) => `<span class="wb-mchip">${esc(model)}</span>`).join('')}</div>` : '<div class="wb-empty-models">尚未读取模型</div>'}
      <label class="wb-toggle"><span class="switch"><input type="checkbox" id="cdOverride" data-cd-override ${binding.override === false ? '' : 'checked'}><span></span></span><span><b>覆盖 Claude 的模型菜单</b></span></label>
      <p class="wb-hint">模型在 Claude 内选，使用期间请保持 Bridge 运行；Bridge 重启后请重新应用配置。${status?.upstreamVerified ? '<span class="chip ok">上游已验证</span>' : '<span class="chip warn">仅验证本机配置，尚未验证上游推理</span>'}</p>
      ${status && (!status.supported || status.managed) ? `<div class="callout warn">${esc(status.message || (status.managed ? '受组织策略管理' : '当前版本暂不支持'))}</div>` : !status ? '<p class="wb-hint">检查中…</p>' : ''}
      ${status?.requiresModeChange ? '<p class="wb-hint">首次开启第三方模式，原账号聊天不删除。</p>' : ''}
      <p class="wb-hint">会先停止该 Agent 的当前任务，再应用所选 API。</p>
      ${wbLaunchFeedback(draft, error)}
    </div><div class="modal-foot"><button class="btn ghost" data-act="closeModal">取消</button><span class="spacer"></span><button class="btn" data-act="wbApplyGo" data-apply-only="true" data-id="${esc(account.id)}" data-target="claude-desktop" ${enabled ? '' : 'disabled'}>仅应用配置</button><button class="btn primary" data-act="cdGo" data-id="${esc(account.id)}" ${enabled ? '' : 'disabled'}>${icon('external')}应用并打开</button></div>
  </div></div>`;
}

async function cdOpen(button) {
  const id = bindingFor({ id: 'claude-desktop' }).accountId;
  if (id) await wbOpenAccount('claude-desktop', id, button, { quickLaunch: true });
}

Object.assign(ACTIONS, {
  cdOpen,
  cdLoad: async (button) => {
    const draft = WB.accountOpen;
    if (wbAccountCurrent(draft) && draft.target === 'claude-desktop' && draft.id === button.dataset.id) await wbAccountModels(draft, false);
  },
  cdGo: wbAccountGo,
  cdRestore: async (button) => {
    if (!await confirm('恢复原配置并重启 Claude？聊天不会删除，请先保存任务。')) return;
    await busy(button, async () => {
      const result = await api('/api/local/claude-desktop/restore', { confirmed: true });
      toast(result.message || '已恢复原配置', 'ok');
      try { await wbRefreshLaunchCards(); } catch (_) { /* The restore already succeeded; a display refresh must not invite another restart. */ }
    });
  },
});

const cdPreviousRestore = ACTIONS.localRestore;
ACTIONS.localRestore = (button) => button.dataset.kind === 'claude-desktop' ? ACTIONS.cdRestore(button) : cdPreviousRestore(button);
document.addEventListener('change', async (event) => {
  const select = event.target;
  try {
    if (select.hasAttribute?.('data-cd-key')) { await wbStageSelection('claude-desktop', select.value, select); return; }
    const draft = WB.accountOpen;
    if (!wbAccountCurrent(draft) || draft.target !== 'claude-desktop') return;
    if (select.hasAttribute?.('data-cd-override')) {
      if (WB.launchPending) { select.checked = draft.override; return; }
      draft.override = select.checked;
    }
  } catch (error) { toast(error.message, 'bad'); renderAgentCards(); }
});
