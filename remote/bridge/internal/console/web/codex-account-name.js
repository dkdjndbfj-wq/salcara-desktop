'use strict';
let officialNameEditor = null;
function officialNameCurrent(owner) {
  return owner && officialNameEditor === owner && owner.epoch === OFFICIAL_PANEL.epoch && owner.route === S.route && owner.element === $('#officialPanel');
}
Object.assign(ACTIONS, {
  officialRename(button) {
    if (button?.isConnected === false) return;
    const row = officialLibraryAccounts(S.local?.accounts || []).find(a => a.officialAccount?.id === button.dataset.id)?.officialAccount;
    if (!row || !Number.isSafeInteger(row.identityEpoch) || row.identityEpoch <= 0) throw new Error('请刷新账号列表后重试');
    officialPanel('修改名称', '<div class="field"><label for="officialNameInput">账号名称</label><input id="officialNameInput" class="input" maxlength="120" autocomplete="off"></div>', '<button class="btn ghost" data-act="officialNameDefault">使用邮箱</button><button class="btn primary" data-act="officialNameSave">保存</button>');
    officialNameEditor = { id: row.id, identityEpoch: row.identityEpoch, epoch: OFFICIAL_PANEL.epoch, route: S.route, element: $('#officialPanel'), pending: false };
    $('#officialNameInput').value = row.name || '';
    $('#officialNameInput').focus?.();
  },
  officialNameDefault(button) { return saveOfficialName(button, ''); },
  officialNameSave(button) { return saveOfficialName(button); },
});
async function saveOfficialName(button, reset) {
  const owner = officialNameEditor;
  if (!officialNameCurrent(owner) || owner.pending || button?.isConnected === false) return;
  const name = reset === '' ? '' : $('#officialNameInput')?.value?.trim();
  if (typeof name !== 'string' || [...name].length > 120 || new TextEncoder().encode(name).length > 320 || /[\u0000-\u001f\u007f-\u009f]/.test(name)) throw new Error('名称格式无效');
  owner.pending = true;
  const token = officialMutationToken();
  try {
    const result = await busy(button, () => api('/api/local/codex/rename', { id: owner.id, name, identityEpoch: owner.identityEpoch }));
    if (!officialNameCurrent(owner)) return;
    if (result?.ok !== true) throw new Error(result?.saved === true ? '名称已保存，列表同步未完成，请刷新后重试' : '保存结果待确认，请刷新账号列表');
    await refreshOfficialCards(token);
    if (!officialNameCurrent(owner) || !officialMutationCurrent(token)) return;
    closeOfficialPanel(); toast('名称已保存', 'ok');
  } catch (error) {
    if (officialNameCurrent(owner) && $('#officialError')) $('#officialError').textContent = error.message;
  } finally { owner.pending = false; }
}

// Escape belongs to the nested name dialog, not the API library behind it.
document.addEventListener('keydown', event => {
  if (event.key !== 'Escape' || !officialNameCurrent(officialNameEditor)) return;
  event.preventDefault(); event.stopImmediatePropagation(); closeOfficialPanel();
}, true);
