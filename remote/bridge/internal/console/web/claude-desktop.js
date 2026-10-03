'use strict';
// Official Claude Desktop 3P configuration has its own backend transaction.
// Never route this card through the legacy isolated-profile / CLI writer.
const CD = { status: null, generation: 0 };
const cdModels = (account) => [...new Set(account?.models || [])];

// The phone continues Claude Desktop's Code sessions through the Claude Code worker.
function cdLamp() { return desktopRemoteLamp({ id: 'claude-desktop' }); }

const cdMore = (body) => (typeof wbMore === 'function' ? wbMore(body) : `<details class="wb-card-more"><summary aria-label="更多操作">⋯</summary><div class="wb-more-body">${body}</div></details>`);

function wbClaudeDesktopCard(tool) {
  const binding = bindingFor(tool), account = accountFor(binding.accountId);
  const status = !tool.available ? '<span class="chip">未安装</span>' : binding.pending ? '<span class="chip warn">待应用</span>' : '';
  return `<article class="wb-card" data-tool="claude-desktop" data-available="${tool.available}">
    <span class="tool-logo desktop">CD</span>
    <div class="wb-main"><div class="wb-name">Claude Desktop${status}</div><div class="wb-sub">桌面应用${remoteLampHTML(cdLamp(), 'desktop')}</div></div>
    ${!tool.available && typeof wbMissing === 'function' ? wbMissing({ name: 'Claude Desktop' }) : `<select class="input wb-key" id="cdKey" aria-label="Claude Desktop 使用的 API 密钥" data-cd-key ${tool.available ? '' : 'disabled'}>
      <option value="">选择 API 密钥</option>${S.local.accounts.map((item) => `<option value="${esc(item.id)}" ${item.id === binding.accountId ? 'selected' : ''}>${esc(item.name)} · ${esc(wbHost(item.baseUrl))}</option>`).join('')}
      <option value="__new">＋ 添加 API 密钥…</option>
    </select>
    <button class="btn primary" data-act="cdOpen" ${tool.available && account ? '' : 'disabled'}>打开</button>`}
    ${cdMore(`<button class="btn sm ghost" data-act="cdRestore">${icon('refresh')}恢复原配置</button>${account ? `<button class="btn sm ghost" data-act="localEdit" data-id="${esc(account.id)}">${icon('edit')}编辑密钥</button>` : ''}
      <p class="wb-note">沿用同一个第三方聊天库，不新建实例。</p>`)}
  </article>`;
}

function cdModal(account, status, error) {
  const binding = bindingFor({ id: 'claude-desktop' }), models = cdModels(account);
  const enabled = status && status.supported && !status.managed;
  $('#modalRoot').innerHTML = `<div class="modal-back" data-act="closeModal"><div class="modal wb-open" data-stop role="dialog" aria-modal="true" aria-labelledby="cdTitle">
    <div class="modal-head"><h3 id="cdTitle">打开 Claude Desktop</h3><button class="icon-btn" data-act="closeModal" aria-label="关闭">✕</button></div>
    <div class="modal-body"><div class="wb-open-sum"><span class="wb-brand empty">${icon('key')}</span><div><b>${esc(account.name)}</b><small>${esc(wbHost(account.baseUrl))}</small></div></div>
      <div class="wb-sec-title"><span>可用模型 · ${models.length}</span><button class="btn sm" data-act="cdLoad" data-id="${esc(account.id)}">${icon('refresh')}刷新模型</button></div>
      ${models.length ? `<div class="wb-models">${models.map((model) => `<span class="wb-mchip">${esc(model)}</span>`).join('')}</div>` : '<div class="wb-empty-models">尚未读取模型</div>'}
      <label class="wb-toggle"><span class="switch"><input type="checkbox" id="cdOverride" data-cd-override ${binding.override === false ? '' : 'checked'}><span></span></span><span><b>覆盖 Claude 的模型菜单</b></span></label>
      <p class="wb-hint">模型在 Claude 内选，转换模式需保持 Bridge 运行。</p>
      ${status && (!status.supported || status.managed) ? `<div class="callout warn">${esc(status.message || (status.managed ? '受组织策略管理' : '当前版本暂不支持'))}</div>` : !status ? '<p class="wb-hint">检查中…</p>' : ''}
      ${status?.requiresModeChange ? '<p class="wb-hint">首次开启第三方模式，原账号聊天不删除。</p>' : ''}
      ${error ? `<div class="callout warn" role="alert">${esc(error)}</div>` : ''}
    </div><div class="modal-foot"><button class="btn ghost" data-act="closeModal">取消</button><span class="spacer"></span><button class="btn primary" data-act="cdGo" data-id="${esc(account.id)}" ${enabled ? '' : 'disabled'}>${icon('external')}配置并打开</button></div>
  </div></div>`;
}

async function cdOpen() {
  const account = accountFor(bindingFor({ id: 'claude-desktop' }).accountId);
  if (!account) return;
  const generation = ++CD.generation;
  CD.status = null;
  cdModal(account, null);
  const modal = $('#cdTitle');
  try {
    const status = await api('/api/local/claude-desktop/status');
    if (generation !== CD.generation || $('#cdTitle') !== modal || bindingFor({ id: 'claude-desktop' }).accountId !== account.id) return;
    CD.status = status;
    cdModal(account, status);
  } catch (error) {
    if (generation === CD.generation && $('#cdTitle') === modal) cdModal(account, { supported: false }, error.message);
  }
}

Object.assign(ACTIONS, {
  cdOpen,
  cdLoad: async (button) => busy(button, async () => {
    const modal = $('#cdTitle');
    try {
      await api('/api/local/models', { id: button.dataset.id });
      S.local = await api('/api/local/accounts');
      renderAgentCards();
      if ($('#cdTitle') === modal) await cdOpen();
    } catch (error) { toast(error.message, 'bad'); }
  }),
  cdGo: async (button) => {
    if (!CD.status?.supported || CD.status.managed) return;
    const account = accountFor(button.dataset.id);
    if (!account) return;
    await busy(button, async () => {
      const modal = $('#cdTitle');
      try {
        await api('/api/local/claude-desktop/switch', { id: button.dataset.id, confirmed: true, allowModeChange: true, catalogOverride: $('#cdOverride')?.checked !== false });
        S.local = await api('/api/local/accounts');
        if ($('#cdTitle') === modal) $('#modalRoot').innerHTML = '';
        renderAgentCards();
        toast('已配置并打开 Claude Desktop', 'ok');
      } catch (error) {
        if ($('#cdTitle') === modal && accountFor(button.dataset.id)) cdModal(accountFor(button.dataset.id), CD.status, error.message);
        else toast(error.message, 'bad');
      }
    });
  },
  cdRestore: async (button) => {
    if (!confirm('恢复原配置并重启 Claude？聊天不会删除，请先保存任务。')) return;
    await busy(button, async () => {
      const result = await api('/api/local/claude-desktop/restore', { confirmed: true });
      S.local = await api('/api/local/accounts');
      renderAgentCards(); toast(result.message || '已恢复原配置', 'ok');
    });
  },
});

const cdPreviousOpen = ACTIONS.wbOpen;
ACTIONS.wbOpen = (button) => button.dataset.target === 'claude-desktop' ? cdOpen() : cdPreviousOpen(button);
const cdPreviousRestore = ACTIONS.localRestore;
ACTIONS.localRestore = (button) => button.dataset.kind === 'claude-desktop' ? ACTIONS.cdRestore(button) : cdPreviousRestore(button);
document.addEventListener('change', async (event) => {
  const select = event.target;
  if (select.hasAttribute?.('data-cd-override')) {
    try { await wbBind('claude-desktop', { override: select.checked }); }
    catch (error) { toast(error.message, 'bad'); }
    return;
  }
  if (!select.hasAttribute?.('data-cd-key')) return;
  CD.generation += 1;
  if (select.value === '__new') { select.value = bindingFor({ id: 'claude-desktop' }).accountId || ''; wbNewAccount('claude-desktop'); return; }
  try {
    const account = accountFor(select.value);
    await wbBind('claude-desktop', { id: select.value, model: '', override: true });
    if (account) await cdOpen();
  } catch (error) { toast(error.message, 'bad'); renderAgentCards(); }
});
