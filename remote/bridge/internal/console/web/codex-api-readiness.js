'use strict';

let apiReadinessPanel = null;
function apiReadinessState(state) { return ({ configured: '本机配置可用', pool_configuration_invalid: '账号池成员配置失效', pool_members_unavailable: '账号池暂无可用凭据', client_disabled: '客户端已停用', client_configuration_invalid: '客户端配置无效', account_missing: '绑定账号已移除', account_configuration_invalid: '账号配置无效', official_executor_unavailable: '官方执行器不可用', credential_binding_unavailable: '受保护账号绑定已失效', client_key_invalid: '客户端 Key 无效', credential_refresh_required: '凭据需要刷新', credential_login_required: '凭据需要重新登录', credential_save_pending: '凭据等待安全保存', credential_refresh_failed: '凭据暂时无法刷新' })[state] || '未知'; }
function apiReadinessTone(state) { return ({ configured: 'ok', credential_refresh_required: 'warn', credential_login_required: 'bad', credential_save_pending: 'warn', credential_refresh_failed: 'bad', pool_configuration_invalid: 'bad', pool_members_unavailable: 'bad', client_disabled: 'muted', client_configuration_invalid: 'bad', account_missing: 'bad', account_configuration_invalid: 'bad', official_executor_unavailable: 'bad', credential_binding_unavailable: 'bad', client_key_invalid: 'bad' })[state] || 'muted'; }
function apiCredentialState(state) { return ({ ready: '', refresh_recommended: '建议更新凭据，现有 Access Token 仍可使用', refresh_required: '需要更新凭据', login_required: '需要重新登录', save_pending: '轮换凭据等待安全保存', refresh_failed: '凭据暂时无法更新' })[state] || ''; }
function readinessCurrent(epoch) { return OFFICIAL_PANEL.epoch === epoch && !!$('#apiReadinessContent'); }
function apiReadinessCurrent(owner) { return apiReadinessPanel === owner && readinessCurrent(owner.epoch) && $('#apiReadinessContent') === owner.element; }
function apiReadinessRepair(row) {
  if (!row.credentialRef) return null;
  if (row.state === 'credential_save_pending' || row.credentialState === 'save_pending') return { kind: 'save', label: '重试保存', glyph: 'download' };
  if (['credential_login_required', 'credential_binding_unavailable'].includes(row.state) || row.credentialState === 'login_required') return { kind: 'login', label: '重新登录', glyph: 'key' };
  if (['credential_refresh_required', 'credential_refresh_failed'].includes(row.state) || ['refresh_required', 'refresh_recommended', 'refresh_failed'].includes(row.credentialState)) return { kind: 'refresh', label: '更新凭据', glyph: 'refresh' };
  return null;
}
function apiReadinessRowsHTML(result, owner) {
  owner.repairs = new Map();
  return (result.clients || []).map(client => {
    const members = client.members?.length ? client.members : [{ id: client.accountId, name: client.accountName, state: client.state, credentialState: client.credentialState, credentialRef: client.credentialRef }];
    const rows = members.map(member => {
      const repair = apiReadinessRepair(member), key = `${client.id || ''}:${member.id || ''}`;
      if (repair && client.id && member.id) owner.repairs.set(key, { ...repair, credentialRef: member.credentialRef });
      return `<div class="api-readiness-member"><div><b>${esc(member.name || '未知账号')}</b><span class="account-state ${apiReadinessTone(member.state)}">${esc(apiReadinessState(member.state))}</span><small>${esc(apiCredentialState(member.credentialState))}</small></div>${repair && owner.repairs.has(key) ? `<button class="btn sm" data-act="codexAPIReadinessRepair" data-key="${esc(key)}" data-kind="${repair.kind}" ${owner.repairPending ? 'disabled' : ''}>${icon(repair.glyph)}${repair.label}</button>` : ''}</div>`;
    }).join('');
    return `<section class="api-readiness-client"><div class="row"><b>${esc(client.name || '未命名客户端')}</b><span class="account-state ${apiReadinessTone(client.state)}">${esc(apiReadinessState(client.state))}</span><span class="spacer"></span><button class="icon-btn" data-act="codexAPIReadinessEdit" data-id="${esc(client.id || '')}" title="编辑客户端绑定与规则" aria-label="编辑 ${esc(client.name || '客户端')} 的绑定与规则">${icon('edit')}</button></div>${rows}</section>`;
  }).join('') || '<p class="muted">尚未配置客户端。</p>';
}
async function loadAPIReadiness(owner = apiReadinessPanel) {
  if (!owner || !apiReadinessCurrent(owner) || owner.loading) return;
  owner.loading = true;
  const sequence = ++owner.sequence;
  const button = $('#apiReadinessRefresh');
  if (button) button.disabled = true;
  try {
    const result = await api('/api/local/codex-api/readiness');
    if (!apiReadinessCurrent(owner) || sequence !== owner.sequence) return;
    if (!Array.isArray(result?.clients)) throw new Error('就绪状态响应不完整');
    owner.clients = result.clients;
    const state = result.serving ? '本次服务已开启' : result.requiresStart ? '等待本次启动' : '服务已关闭';
    $('#apiReadinessSummary').textContent = `${state} · ${result.readyClients || 0} 个客户端本机配置可用`;
    $('#apiReadinessRows').innerHTML = apiReadinessRowsHTML(result, owner);
    $('#apiReadinessScope').textContent = `只检查本机服务、账号绑定、客户端 Key 和模型规则，不自动刷新账号或调用上游。支持接口：${(result.routes || []).join(' · ')}`;
    $('#officialError').textContent = '';
  } catch (error) { if (apiReadinessCurrent(owner) && sequence === owner.sequence) $('#officialError').textContent = `${error.message}；可点重新检查重试。`; }
  finally { owner.loading = false; if (apiReadinessCurrent(owner) && button) button.disabled = false; }
}
async function showAPIReadiness() {
  if (apiReadinessPanel && apiReadinessCurrent(apiReadinessPanel)) return loadAPIReadiness();
  officialPanel('本机就绪诊断', `<div id="apiReadinessContent"><div class="row"><span id="apiReadinessSummary" class="muted small" role="status">正在检查本机配置…</span><span class="spacer"></span><button id="apiReadinessRefresh" class="btn sm" data-act="codexAPIReadinessRefresh">重新检查</button></div><div id="apiReadinessRows" class="readiness-list"></div><details class="api-pool-options"><summary>检查客户端 Key 与模型规则</summary><div class="form-grid"><div class="field"><label for="apiReadinessKey">客户端 Key（仅本次）</label><input id="apiReadinessKey" class="input" type="password" autocomplete="off" spellcheck="false" maxlength="256"></div><div class="field"><label for="apiReadinessModel">模型 ID / 别名（可选）</label><input id="apiReadinessModel" class="input" maxlength="200" autocomplete="off"></div></div><button class="btn sm" data-act="codexAPIKeyCheck">校验本机规则</button><div id="apiKeyCheckResult" role="status"></div></details><p id="apiReadinessScope" class="muted small"></p></div>`);
  const owner = apiReadinessPanel = { epoch: OFFICIAL_PANEL.epoch, element: $('#apiReadinessContent'), sequence: 0, loading: false, repairPending: false, repairs: new Map(), clients: [] };
  apiManagementShell('<button class="btn" data-act="codexAPI" data-view="clients">返回客户端</button>');
  await loadAPIReadiness(owner);
}
Object.assign(ACTIONS, {
  codexAPIReadiness: () => apiManagementLeave(showAPIReadiness),
  codexAPIReadinessRefresh: () => loadAPIReadiness(),
  codexAPIReadinessEdit: button => {
    const owner = apiReadinessPanel;
    if (!owner || !apiReadinessCurrent(owner) || !owner.clients.some(row => row.id === button.dataset.id)) return;
    return openCodexAPIClient(button.dataset.id);
  },
  codexAPIReadinessRepair: async button => {
    const owner = apiReadinessPanel, repair = owner?.repairs.get(button.dataset.key);
    if (!owner || !apiReadinessCurrent(owner) || owner.repairPending || !repair || repair.kind !== button.dataset.kind) return;
    if (repair.kind === 'login') return openOfficialAccounts(repair.credentialRef);
    owner.repairPending = true;
    try {
      await busy(button, () => (repair.kind === 'save' ? ACTIONS.officialRetrySave : ACTIONS.officialRefresh)({ dataset: { id: repair.credentialRef }, isConnected: true }));
      if (apiReadinessCurrent(owner)) await loadAPIReadiness(owner);
    } catch (error) { if (apiReadinessCurrent(owner)) $('#officialError').textContent = error.message; }
    finally { owner.repairPending = false; if (apiReadinessCurrent(owner) && owner.clients.length) $('#apiReadinessRows').innerHTML = apiReadinessRowsHTML({ clients: owner.clients }, owner); }
  },
  codexAPIKeyCheck: async button => {
    const owner = apiReadinessPanel, input = $('#apiReadinessKey');
    if (!owner || !apiReadinessCurrent(owner) || !input) return;
    const body = { key: input.value, model: $('#apiReadinessModel').value };
    input.value = '';
    try {
      const result = await busy(button, () => api('/api/local/codex-api/key-check', body));
      if (!apiReadinessCurrent(owner)) return;
      $('#officialError').textContent = '';
      $('#apiKeyCheckResult').textContent = `${result.authenticated ? 'Key 与启用客户端匹配' : 'Key 不能用于启用客户端'} · ${apiReadinessState(result.state)}${apiCredentialState(result.credentialState) ? ` · ${apiCredentialState(result.credentialState)}` : ''} · ${result.serving ? '服务已开启' : '服务当前没有开启'}${result.modelPolicyAllowed == null ? '' : result.modelPolicyAllowed ? ' · 本机模型规则允许' : ' · 本机模型规则拒绝'}。上游模型权限、额度和可达性未验证。`;
    } catch (error) { if (apiReadinessCurrent(owner)) $('#officialError').textContent = error.message; }
  },
});
