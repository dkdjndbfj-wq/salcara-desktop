'use strict';
(() => {
  let panel = null;
  const current = owner => owner && panel === owner && owner.epoch === OFFICIAL_PANEL.epoch && owner.route === S.route && owner.element === $('#officialPanel');
  const revision = account => account && !account.libraryOnly && account.officialAccount?.mode !== 'chatgpt_web_session' && /^src-v1-[a-f0-9]{64}$/.test(account.sourceRevision || '') ? account.sourceRevision : '';
  const row = (local, id) => (Array.isArray(local?.accounts) ? local.accounts : []).find(account => account.id === id);
  const same = (a, b) => JSON.stringify(a || []) === JSON.stringify(b || []);
  function error(owner, message) {
    if (current(owner) && $('#officialError')) $('#officialError').textContent = message;
    if (current(owner) && owner.reload) owner.reload.hidden = !message || owner.loaded && !owner.attempted;
  }
  function rules(value) {
    const values = value.split(/\r?\n/).map(model => model.trim()).filter(Boolean);
    if (values.length > 1000 || values.some(model => new TextEncoder().encode(model).length > 200 || /[\r\n\u0000]/.test(model))) throw new Error('每项填写一个模型 ID，最多 1000 项，每项不超过 200 字节');
    return [...new Set(values)];
  }
  ACTIONS.accountModelRules = async button => {
    if (button?.isConnected === false) return;
    const account = row(S.local, button.dataset.id), source = revision(account);
    if (!source) throw new Error('请刷新账号列表后重试');
    officialPanel('模型规则', '<div id="accountModelRulesContent" class="stack"><p class="muted small">正在读取…</p></div>', '<button id="accountModelRulesReload" class="btn" data-act="accountModelRulesReload" hidden>重新读取</button>');
    const owner = panel = { id: account.id, source, epoch: OFFICIAL_PANEL.epoch, route: S.route, element: $('#officialPanel'), reload: $('#accountModelRulesReload'), loaded: false, pending: false, attempted: false };
    try {
      const local = await api('/api/local/accounts');
      if (!current(owner)) return;
      const latest = row(local, owner.id);
      if (revision(latest) !== owner.source) throw new Error('账号已更改，请刷新列表后重新打开');
      $('#accountModelRulesContent').innerHTML = `<div class="li-title">${esc(latest.name || '')}</div><div class="field"><label for="accountModelAllow">允许模型</label><textarea id="accountModelAllow" class="input mono" rows="4" maxlength="201000" spellcheck="false" placeholder="留空允许全部模型">${esc((latest.modelAllowlist || []).join('\n'))}</textarea></div><div class="field"><label for="accountModelDeny">禁止模型</label><textarea id="accountModelDeny" class="input mono" rows="4" maxlength="201000" spellcheck="false" placeholder="每行一个模型 ID">${esc((latest.modelDenylist || []).join('\n'))}</textarea></div><p class="muted small">每行一个完整模型 ID；禁止规则优先。更改后需重新选择模型并应用工具。</p><button id="accountModelRulesSave" class="btn primary" data-act="accountModelRulesSave">保存</button>`;
      owner.allow = $('#accountModelAllow'); owner.deny = $('#accountModelDeny'); owner.save = $('#accountModelRulesSave'); owner.loaded = true;
      owner.allow.focus?.();
    } catch (e) { error(owner, e.message || '规则读取失败，请重新打开'); }
  };
  ACTIONS.accountModelRulesReload = async button => {
    const owner = panel;
    if (!current(owner) || owner.pending || button !== owner.reload || button?.hidden || button?.isConnected === false) return;
    owner.pending = true; button.disabled = true;
    try {
      const refreshed = await refreshOfficialCards();
      if (!current(owner)) return;
      if (refreshed === false) throw new Error('账号列表已更新，请重试');
      await ACTIONS.accountModelRules({ dataset: { id: owner.id } });
    } catch (e) { error(owner, e.message || '读取失败，请重试'); }
    finally { owner.pending = false; if (current(owner)) button.disabled = false; }
  };
  ACTIONS.accountModelRulesSave = async button => {
    const owner = panel;
    if (!current(owner) || !owner.loaded || owner.pending || owner.attempted || button?.isConnected === false || button !== owner.save || owner.allow !== $('#accountModelAllow') || owner.deny !== $('#accountModelDeny')) return;
    let allow, deny;
    try { allow = rules(owner.allow.value); deny = rules(owner.deny.value); }
    catch (e) { error(owner, e.message); return; }
    error(owner, '');
    owner.pending = true; owner.attempted = true;
    owner.allow.disabled = owner.deny.disabled = owner.save.disabled = true;
    const token = officialMutationToken();
    try {
      const result = await api('/api/local/accounts/model-rules', { id: owner.id, expectedSourceRevision: owner.source, modelAllowlist: allow, modelDenylist: deny });
      if (!current(owner)) return;
      if (result?.ok !== true || result.id !== owner.id || !revision(result) || !same(result.modelAllowlist, allow) || !same(result.modelDenylist, deny)) throw new Error('保存结果待确认，请重新打开检查');
      await refreshOfficialCards(token);
      if (!current(owner) || !officialMutationCurrent(token)) return;
      closeOfficialPanel(); toast('模型规则已保存', 'ok');
    } catch (e) { error(owner, e.message || '保存结果待确认，请重新打开检查'); }
    finally { owner.pending = false; }
  };
  document.addEventListener('keydown', event => {
    if (event.key !== 'Escape' || !current(panel)) return;
    event.preventDefault(); event.stopImmediatePropagation(); closeOfficialPanel();
  }, true);
})();
