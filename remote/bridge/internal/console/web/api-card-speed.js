'use strict';
const API_SPEED_SAVES = new Map();
const API_SPEED_LABELS = { standard: '标准', fast: '快速', ultrafast: '超高速' };
function accountSpeedControlHTML(account) {
  const value = Object.hasOwn(API_SPEED_LABELS, account.speedMode) ? account.speedMode : 'standard';
  const pending = API_SPEED_SAVES.has(account.id);
  const eligible = /^src-v1-[a-f0-9]{64}$/.test(account.sourceRevision || '') && account.wire !== 'anthropic';
  const hint = account.wire === 'anthropic' ? 'Anthropic 接口不使用此速度档位' : '标准跟随客户端或服务商默认；快速与超高速仅用于 OpenAI 兼容接口，需服务商与模型支持，可能增加费用';
  return `<label class="api-speed-control" title="${esc(hint)}"><span>速度</span><select data-native data-account-speed="${esc(account.id)}" data-source-revision="${esc(account.sourceRevision || '')}" aria-label="${esc(account.name + '的请求速度')}" ${pending || !eligible ? 'disabled' : ''} aria-busy="${pending}">${Object.entries(API_SPEED_LABELS).map(([key, label]) => `<option value="${key}" ${key === value ? 'selected' : ''}>${label}</option>`).join('')}</select></label>`;
}
async function saveAccountSpeed(control) {
  const id = control?.dataset?.accountSpeed;
  const account = S.local?.accounts?.find(row => row.id === id);
  if (!account || S.route !== 'accounts' || control.isConnected === false) return;
  const previous = Object.hasOwn(API_SPEED_LABELS, account.speedMode) ? account.speedMode : 'standard';
  const mode = control.value;
  if (API_SPEED_SAVES.has(id)) { control.value = API_SPEED_SAVES.get(id).mode; return; }
  if (!Object.hasOwn(API_SPEED_LABELS, mode) || mode === previous || account.wire === 'anthropic') { control.value = previous; return; }
  const sourceRevision = account.sourceRevision;
  if (!/^src-v1-[a-f0-9]{64}$/.test(sourceRevision || '') || control.dataset.sourceRevision !== sourceRevision) { control.value = previous; toast('账号已更新，请刷新列表后重试', 'bad'); return; }
  const owner = { route: S.routeRevision, control, sourceRevision, mode };
  const current = () => S.route === 'accounts' && S.routeRevision === owner.route && S.local?.accounts?.find(row => row.id === id)?.sourceRevision === sourceRevision;
  API_SPEED_SAVES.set(id, owner);
  S.localAccountsRevision = (S.localAccountsRevision || 0) + 1;
  control.disabled = true; control.setAttribute('aria-busy', 'true');
  try {
    const result = await api('/api/local/accounts/speed', { id, sourceRevision, speedMode: mode });
    if (!current()) return;
    if (result?.ok !== true || result.id !== id || result.speedMode !== mode || !/^src-v1-[a-f0-9]{64}$/.test(result.sourceRevision || '')) throw new Error('保存结果未确认，请刷新列表查看');
    const next = { ...account, speedMode: mode, sourceRevision: result.sourceRevision };
    S.local.accounts = S.local.accounts.map(row => row.id === id ? next : row);
    S.localAccountsRevision = (S.localAccountsRevision || 0) + 1;
    // Existing native connections retain their old lease until the user applies again.
    for (const binding of Object.values(S.local.bindings || {})) if (binding.accountId === id) binding.pending = true;
    toast(`已设为${API_SPEED_LABELS[mode]}；已打开的 Agent 需重新应用。快档以服务商支持与计费为准。`, 'ok');
  } catch (error) {
    if (current()) { control.value = previous; toast(error?.message || '未确认保存，请刷新列表查看', 'bad'); }
  } finally {
    if (API_SPEED_SAVES.get(id) === owner) API_SPEED_SAVES.delete(id);
    control.disabled = false; control.removeAttribute('aria-busy');
    if (S.route === 'accounts' && S.routeRevision === owner.route) renderLocalCards();
  }
}
document.addEventListener('change', event => {
  if (event.target?.matches?.('select[data-account-speed]')) void saveAccountSpeed(event.target);
});
