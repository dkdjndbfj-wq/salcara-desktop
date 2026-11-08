'use strict';
(() => {
  let panel = null;
  const current = owner => owner && panel === owner && owner.epoch === OFFICIAL_PANEL.epoch && owner.route === S.route && owner.element === $('#officialPanel');
  function source(account) {
    if (!account || account.libraryOnly || account.officialAccount?.mode === 'chatgpt_web_session') return '';
    return /^src-v1-[a-f0-9]{64}$/.test(account.sourceRevision || '') ? account.sourceRevision : '';
  }
  function accountFrom(local, id) { return apiClientAccounts(local).find(row => row.id === id); }
  function clientSignature(client) { return JSON.stringify(client); }
  function membership(client, id, mode) {
    const values = mode === 'text' ? client.accountIds : client.imageAccountIds;
    const explicit = Array.isArray(values) ? values : [];
    const member = explicit.length ? explicit.includes(id) : client.accountId === id;
    // Empty lists are a legacy *primary fallback*, not an empty grant. Do not
    // replace it on add, or restore it by removing the last explicit member.
    const editor = client.accountId === id || !explicit.length || member && explicit.length === 1 ||
      mode === 'text' && member && client.poolStrategy === 'single_account' && client.poolAccountId === id;
    return { member, editor };
  }
  function showError(owner, message) {
    if (current(owner) && $('#officialError')) $('#officialError').textContent = message;
  }
  function render(owner) {
    if (!current(owner) || !$('#apiCardMembers')) return;
    const rows = owner.clients.map(client => {
      const primary = client.accountId === owner.id;
      const controls = primary ? `<button class="btn sm" data-act="apiCardMemberEdit" data-id="${esc(client.id)}">更换主账号</button>` : ['text', 'image'].map(mode => {
        const state = membership(client, owner.id, mode), label = mode === 'text' ? '文字' : '图片';
        return `<button class="btn sm${state.member && !state.editor ? ' ghost danger-text' : ''}" data-act="${state.editor ? 'apiCardMemberEdit' : 'apiCardMemberToggle'}" data-id="${esc(client.id)}" data-mode="${mode}" data-enabled="${!state.member}">${state.editor ? label + '设置' : (state.member ? '移出' : '加入') + label}</button>`;
      }).join('');
      const note = primary ? '主账号 · 移出前需选择替代账号' : ['text', 'image'].filter(mode => membership(client, owner.id, mode).member).map(mode => mode === 'text' ? '文字' : '图片').join('、') || '尚未加入';
      return `<div class="restore-row"><div class="li-main"><div class="li-title">${esc(client.name || client.id)}${client.disabled ? ' <span class="chip">已停用</span>' : ''}</div><div class="muted small">${esc(note)}</div></div><div class="restore-actions">${controls}</div></div>`;
    }).join('');
    $('#apiCardMembers').innerHTML = `<div class="li-title">${esc(owner.name)}</div>${rows || '<p class="muted small">尚未配置客户端。</p><button class="btn" data-act="apiCardMemberEdit" data-id="">配置客户端</button>'}<div class="muted small">文字和图片单独设置；更换主账号或最后一个成员请进入设置。</div>`;
  }
  async function read(owner) {
    const [status, local] = await Promise.all([api('/api/local/codex-api'), api('/api/local/accounts')]);
    if (!current(owner)) return null;
    const account = accountFrom(local, owner.id);
    if (!account || !source(account) || source(account) !== owner.source) throw new Error('账号已更改，请重新打开');
    if (!Array.isArray(status.clients) || status.clients.some(client => !/^api-client-v1-[a-f0-9]{64}$/.test(client?.revision || ''))) throw new Error('请更新电脑端并重新打开客户端设置');
    return { clients: status.clients, account };
  }
  ACTIONS.apiCardMembership = async button => {
    if (button?.isConnected === false) return;
    const account = accountFrom(S.local || {}, button.dataset.id);
    if (!account || !source(account)) throw new Error('请刷新账号列表后重试');
    officialPanel('API 服务', '<div id="apiCardMembers" class="stack"><p class="muted small">正在读取…</p></div>');
    const owner = panel = { id: account.id, source: source(account), name: account.name || '', epoch: OFFICIAL_PANEL.epoch, route: S.route, element: $('#officialPanel'), clients: [], loaded: false, pending: false, attempted: false };
    try {
      const result = await read(owner);
      if (!result) return;
      owner.clients = result.clients; owner.loaded = true; render(owner);
    } catch (error) { showError(owner, error.message || '成员读取失败，请重新打开'); }
  };
  ACTIONS.apiCardMemberEdit = async button => {
    const owner = panel, id = button.dataset.id || '';
    if (!current(owner) || !owner.loaded || owner.pending || owner.attempted || button?.isConnected === false) return;
    if (id ? !owner.clients.some(row => row.id === id) : owner.clients.length) return;
    owner.pending = true;
    // The existing editor preserves all model, pool and image settings and
    // requires an explicit save. Opening it never creates/enables a client.
    await openCodexAPIClient(id);
  };
  ACTIONS.apiCardMemberToggle = async button => {
    const owner = panel, client = owner?.clients.find(row => row.id === button.dataset.id), mode = button.dataset.mode;
    if (!current(owner) || !owner.loaded || owner.pending || owner.attempted || !client || button?.isConnected === false || !['text', 'image'].includes(mode)) return;
    const state = membership(client, owner.id, mode), enabled = !state.member;
    if (state.editor || button.dataset.enabled !== String(enabled)) return;
    owner.pending = true;
    const token = officialMutationToken();
    try {
      const fresh = await read(owner);
      if (!fresh) return;
      const next = fresh.clients.find(row => row.id === client.id);
      if (!next || clientSignature(next) !== clientSignature(client)) throw new Error('客户端已更改，请重新打开');
      owner.attempted = true;
      const result = await busy(button, () => api('/api/local/codex-api/members', { clientId: client.id, accountId: owner.id, mode, enabled, expectedSourceRevision: owner.source, expectedClientRevision: client.revision }));
      // After an ambiguous response, require a fresh read rather than repeat
      // a write from a stale snapshot. Never infer success from HTTP 200 alone.
      if (!current(owner)) return;
      if (result?.ok !== true || result.clientId !== client.id || result.accountId !== owner.id || result.mode !== mode || result.enabled !== enabled) throw new Error('结果待确认，请重新打开查看');
      await refreshOfficialCards(token);
      if (!current(owner) || !officialMutationCurrent(token)) return;
      closeOfficialPanel();
      toast((enabled ? '已加入' : '已移出') + (mode === 'text' ? '文字' : '图片') + '成员', 'ok');
    } catch (error) { showError(owner, error.message || '成员更新失败，请重新打开'); }
    finally { owner.pending = false; }
  };
  document.addEventListener('keydown', event => {
    if (event.key !== 'Escape' || !current(panel)) return;
    event.preventDefault(); event.stopImmediatePropagation(); closeOfficialPanel();
  }, true);
})();
