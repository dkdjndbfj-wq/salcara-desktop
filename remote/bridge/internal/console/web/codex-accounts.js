'use strict';
// This extension is confined to official-account / reverse-proxy controls.
// Existing Agent, phone, layout, styles and animation renderers are unchanged.
const OFFICIAL_PANEL = { credentials: null, files: null, importFormat: 'json', timer: 0, session: null, challenge: null, epoch: 0, fileRevision: 0, loginPollRevision: 0, replaceId: '', loginPending: false, mutation: 0, refresh: 0, cleanup: null, cleanupPending: false, importPreview: null, importSelection: null, importChecking: false, importResult: null, keyElement: null };
const officialSubscriptionPending = new Set();
// Mutations may finish out of order (for example, a slow quota refresh after a
// fast remove).  Keep a monotonic panel token so an older response can never
// repaint a newer card or reopen a stale modal.
function officialMutationToken() { officialLibraryRevision++; CODEX_API_OVERVIEW.revision++; S.localAccountsRevision = (S.localAccountsRevision || 0) + 1; S.officialCleanupKnown = false; syncOfficialCleanupAction(); return { epoch: OFFICIAL_PANEL.epoch, seq: ++OFFICIAL_PANEL.mutation }; }
function officialMutationCurrent(token) { return token && token.epoch === OFFICIAL_PANEL.epoch && token.seq === OFFICIAL_PANEL.mutation; }
function officialAccountActions(root) {
  root.insertAdjacentHTML('beforeend', `<button id="officialCleanupAction" class="btn" data-act="officialCleanup" hidden>清理失效账号</button>`);
}
function syncOfficialCleanupAction() {
  const button = $('#officialCleanupAction');
  if (!button || S.route !== 'accounts') return;
  button.hidden = S.officialCleanupKnown !== true || !Array.isArray(S.officialRows)
    || !S.officialRows.some(row => typeof row?.id === 'string' && row.cleanup?.eligible === true
      && Object.hasOwn(OFFICIAL_CLEANUP_REASONS, row.cleanup.reason) && typeof row.cleanup.recoverable === 'boolean');
}
const CODEX_API_OVERVIEW = { revision: 0, status: null, error: false, pending: false, expiryTimer: 0, updatedAt: 0 };
let apiClientWritePending = false;
let apiClientPanel = null;
let apiServiceLaunchPanel = null;
const API_SERVICE_TOOLS = { 'codex-desktop': 'Codex', codex: 'Codex CLI' };
const API_CLIENT_WRITE_ACTIONS = new Set(['codexAPISave', 'codexAPIRotate', 'codexAPIRevoke', 'codexAPIClientEnabled', 'codexAPIEdit', 'codexAPINew', 'codexAPIServiceOpen', 'codexAPIServiceLaunch']);
const apiClientWriteControls = new WeakMap();
function syncAPIClientWriteControls() {
  for (const button of $('#modalRoot')?.querySelectorAll?.('[data-act]') || []) {
    if (!API_CLIENT_WRITE_ACTIONS.has(button.dataset.act)) continue;
    if (apiClientWritePending) {
      if (!apiClientWriteControls.has(button)) apiClientWriteControls.set(button, button.disabled);
      button.disabled = true;
    } else if (apiClientWriteControls.has(button)) {
      button.disabled = apiClientWriteControls.get(button);
      apiClientWriteControls.delete(button);
    }
    if (!apiClientWritePending && button.dataset.act === 'codexAPISave' && apiClientPanel?.writeAttempted) button.disabled = true;
  }
}
async function withAPIClientWrite(operation) {
  if (apiClientWritePending) return;
  apiClientWritePending = true;
  syncAPIClientWriteControls();
  try { return await operation(); }
  finally { apiClientWritePending = false; syncAPIClientWriteControls(); }
}
function codexAPIState(status) {
  return { label: status.running ? '运行中' : status.requiresStart ? '待启动' : '已关闭',
    action: status.requiresStart ? '启动本次服务' : status.enabled ? '停止服务' : '启动服务',
    nextEnabled: status.requiresStart === true || !status.enabled };
}
function codexAPIConfiguredMembers(status) {
  const ids = new Set();
  for (const client of status.clients || []) {
    if (client.disabled) continue;
    for (const field of ['accountIds', 'imageAccountIds']) {
      const members = Array.isArray(client[field]) && client[field].length ? client[field] : [client.accountId];
      for (const id of members) if (typeof id === 'string' && id) ids.add(id);
    }
  }
  return ids.size;
}
function codexAPIHTTPStats(status) {
  const counts = { requests: 0, inflight: 0, httpErrors: 0 };
  for (const row of status.statistics || []) for (const key of Object.keys(counts)) {
    if (Number.isSafeInteger(row[key]) && row[key] >= 0) counts[key] += row[key];
  }
  return counts;
}
function apiServiceUpdatedLabel(time = CODEX_API_OVERVIEW.updatedAt) {
  return time ? `更新于 ${new Date(time).toLocaleTimeString()}` : '等待状态更新';
}
function apiServiceClientRows(status, accounts = []) {
  const names = new Map(accounts.map(row => [row.id, row.name || '未命名账号']));
  const name = id => names.get(id) || '账号已移除';
  const strategies = { auto: '自动', random: '随机', single_account: '固定成员', quota_high_first: '额度高优先', quota_low_first: '额度低优先', plan_high_first: '套餐高优先', plan_low_first: '套餐低优先', expiry_soon_first: '到期早优先', custom: '自定义' };
  return (status.clients || []).map(row => {
    const text = row.accountIds?.length ? `文字池 · ${row.accountIds.length} 个 · ${strategies[row.poolStrategy] || '自动'}` : `文字 · ${name(row.accountId)}`;
    const images = row.imageAccountIds?.length ? `图片池 · ${row.imageAccountIds.length} 个` : `图片 · ${name(row.accountId)}`;
    const stats = (status.statistics || []).find(item => item.clientId === row.id);
    const observed = stats ? `${stats.inflight || 0} 在途 · ${stats.requests || 0} 次请求${stats.lastStatus ? ` · 最近 HTTP ${stats.lastStatus}` : ''}${Number.isFinite(stats.lastDurationMs) ? ` · ${stats.lastDurationMs} ms` : ''}` : '暂无本次进程请求';
    const action = (act, label, extra = '', danger = false) => `<button class="btn sm${danger ? ' danger-text' : ''}" data-act="${act}" data-id="${esc(row.id)}" ${extra}>${esc(label)}</button>`;
    return `<article class="api-client-row" data-client-id="${esc(row.id)}" data-observation-key="${esc(row.id)}"><div class="api-client-info"><div class="api-client-title"><b>${esc(row.name)}</b><span class="account-state ${row.disabled ? 'muted' : 'ok'}">${row.disabled ? '已停用' : '已启用'}</span>${row.modelPrefix ? `<code>${esc(row.modelPrefix)}</code>` : ''}</div><div class="api-client-sources"><span>${esc(text)}</span><span>${esc(images)}</span></div><small>${esc(observed)}</small></div><div class="api-client-actions">${action('codexAPIEdit', '编辑配置')}${action('codexAPIClientEnabled', row.disabled ? '启用' : '停用', `data-enabled="${row.disabled === true}"`)}<details class="api-client-more"><summary>更多</summary><div>${action('codexAPIRequests', '此配置的请求')}${action('codexAPIRotate', '重置密钥')}${action('codexAPIRevoke', '撤销客户端', '', true)}</div></details></div></article>`;
  }).join('') || '<p class="muted small">先在 API 卡片上点击「加入 API 服务」。</p>';
}
function apiServiceRuntimeHTML(status) {
  const state = codexAPIState(status);
  return `<div class="row api-service-runtime"><span class="account-state ${CODEX_API_OVERVIEW.error ? 'warn' : status.running ? 'ok' : 'muted'}">${CODEX_API_OVERVIEW.error ? '状态待刷新' : state.label}</span><span class="spacer"></span><span class="muted small" id="apiServiceUpdated">${esc(apiServiceUpdatedLabel())}</span><button class="btn sm" data-act="codexAPIServiceRefresh">刷新状态</button><button class="btn sm ${status.running ? '' : 'primary'}" data-act="codexAPIEnabled" data-enabled="${state.nextEnabled}" ${CODEX_API_OVERVIEW.pending || CODEX_API_OVERVIEW.error ? 'disabled' : ''}>${state.action}</button></div>`;
}
function updateAPIServiceReadOnly(status, owner = apiClientPanel) {
  if (!currentAPIClientPanel(owner) || owner.mode !== 'service' || owner.writeAttempted) return;
  owner.clients = status.clients || [];
  if (owner.clients.length) owner.firstClient = false;
  const runtime = $('#apiServiceRuntime'), clients = $('#apiServiceClients'), summary = $('#apiServiceSummary'), launches = $('#apiServiceLaunchActions');
  if (runtime) patchOfficialObservationHTML(runtime, `<div id="apiServiceRuntime">${apiServiceRuntimeHTML(status)}</div>`);
  if (clients) patchOfficialObservationHTML(clients, `<div id="apiServiceClients" class="api-service-clients">${apiServiceClientRows(status, owner.accounts)}</div>`);
  if (summary) patchOfficialObservationHTML(summary, `<div id="apiServiceSummary">${apiServicePlanGroupsHTML(apiServiceSummary(status))}</div>`);
  if (launches) patchOfficialObservationHTML(launches, `<div id="apiServiceLaunchActions">${localAPIServiceLaunchHTML(status)}</div>`);
  if ($('#apiServiceUpstreams')) patchOfficialObservationHTML($('#apiServiceUpstreams'), `<div id="apiServiceUpstreams">${apiServiceUpstreamsHTML(status, owner.accounts)}</div>`);
  if ($('#apiServiceSetup') && owner.clients.length) $('#apiServiceSetup').hidden = true;
  syncAPIClientWriteControls();
}
function apiServiceUsageFresh(usage, now) {
  const until = Date.parse(usage?.freshUntil || '');
  return usage?.verified === true && usage.status === 'fresh' && usage.source === 'openai_codex_usage_api' && Number.isFinite(until) && until > now;
}
function apiServicePlan(member) {
  if (member.sourceType !== 'official_account') return { key: 'api', label: '上游 API', tone: 'api' };
  const plan = member.usage?.plan;
  if (member.usage?.verified !== true || plan?.verified !== true || plan.source !== 'openai_codex_usage_api') return { key: 'unknown', label: '套餐未确认', tone: 'unknown' };
  const names = { free: 'Free', go: 'Go', plus: 'Plus', pro: 'Pro', team: 'Team', business: 'Business', enterprise: 'Enterprise', edu: 'Edu' };
  const family = names[plan.family] ? plan.family : 'unknown', raw = typeof plan.raw === 'string' ? plan.raw.slice(0, 80) : '';
  const label = names[family] || '未知套餐';
  // Raw variants are kept separate, without inferring a price or seat type.
  return { key: family + '\u0000' + raw, label: raw && raw.toLowerCase() !== family ? label + ' · ' + raw : label, tone: family };
}
function apiServiceQuotaWindows(usage) {
  const windows = [];
  for (const slot of ['primary', 'secondary']) {
    const row = usage?.rateLimit?.[slot], value = row?.remainingPercent;
    if (typeof value !== 'number' || !Number.isFinite(value) || value < 0 || value > 100) continue;
    const seconds = Number.isFinite(row.windowSeconds) && row.windowSeconds > 0 ? row.windowSeconds
      : Number.isFinite(row.windowMinutes) && row.windowMinutes > 0 ? row.windowMinutes * 60 : 0;
    const label = seconds === 300 * 60 ? '5 小时' : seconds === 10080 * 60 ? '周额度'
      : seconds >= 60 && seconds % 60 === 0 ? seconds / 60 + ' 分钟' : seconds > 0 ? seconds + ' 秒' : slot === 'primary' ? '主窗口' : '次窗口';
    windows.push({ key: slot + ':' + seconds, label, remaining: value });
  }
  return windows;
}
function apiServiceSummary(status, now = Date.now()) {
  if (!Array.isArray(status?.summary?.members)) return null;
  const groups = new Map(), seen = new Set();
  const result = { total: 0, ready: 0, unknown: 0, unavailable: 0, groups: [], nextExpiry: Infinity };
  for (const member of status.summary.members) {
    if (!member || typeof member.id !== 'string' || !member.id || seen.has(member.id)) continue;
    seen.add(member.id);
    const fresh = apiServiceUsageFresh(member.usage, now);
    const availability = member.availability === 'unavailable' ? 'unavailable'
      : member.availability === 'ready' && member.sourceType === 'official_account' && fresh && !CODEX_API_OVERVIEW.error ? 'ready' : 'unknown';
    const plan = apiServicePlan(member);
    if (!groups.has(plan.key)) groups.set(plan.key, { ...plan, members: [], windows: new Map(), ready: 0, unknown: 0, unavailable: 0 });
    const group = groups.get(plan.key), windows = fresh && !CODEX_API_OVERVIEW.error ? apiServiceQuotaWindows(member.usage) : [];
    group.members.push({ name: typeof member.name === 'string' ? member.name : '未命名账号', availability, windows, stale: !!member.usage?.verified && !fresh });
    result.total++; result[availability]++; group[availability]++;
    for (const window of windows) {
      if (!group.windows.has(window.key)) group.windows.set(window.key, { label: window.label, values: [] });
      group.windows.get(window.key).values.push(window.remaining);
    }
    if (fresh) result.nextExpiry = Math.min(result.nextExpiry, Date.parse(member.usage.freshUntil));
  }
  result.groups = [...groups.values()].sort((a, b) => a.label.localeCompare(b.label, 'zh-CN'));
  return result;
}
function apiServicePercent(value) { return Math.round(value * 10) / 10 + '%'; }
function apiServicePlanGroupsHTML(summary) {
  if (!summary) return '<div class="api-service-empty">账号状态待检查</div>';
  if (!summary.total) return '<div class="api-service-empty">尚未加入账号</div>';
  return '<div class="api-service-plan-list">' + summary.groups.map((group) => {
    const ranges = [...group.windows.values()].map((window) => {
      const low = Math.min(...window.values), high = Math.max(...window.values);
      const value = low === high ? apiServicePercent(low) : apiServicePercent(low) + '–' + apiServicePercent(high);
      return `<span class="api-service-quota-range" title="${esc(window.label)}各账号剩余范围；${window.values.length}/${group.members.length} 个账号有当前数据"><span>${esc(window.label)}</span><b>${value}</b></span>`;
    }).join('');
    const pending = [group.unknown ? group.unknown + ' 未验证' : '', group.unavailable ? group.unavailable + ' 暂不可用' : ''].filter(Boolean).join(' · ');
    const members = group.members.map((member) => `<div class="api-service-member"><span class="api-service-member-name" title="${esc(member.name)}">${esc(member.name)}</span><span class="api-service-member-state ${member.availability === 'ready' ? 'ok' : 'muted'}">${({ ready: '可用', unknown: member.stale ? '待刷新' : '未验证', unavailable: '暂不可用' })[member.availability]}</span><span class="api-service-member-quota">${member.windows.map((window) => esc(window.label) + ' ' + apiServicePercent(window.remaining)).join(' · ') || '额度未验证'}</span></div>`).join('');
    return `<details class="api-service-plan" data-observation-key="${esc(JSON.stringify(group.key))}"><summary><span class="api-service-plan-name plan-${group.tone}">${esc(group.label)}</span><span class="api-service-plan-count">${group.ready}/${group.members.length} 可用</span><div class="api-service-plan-quotas">${ranges || '<span class="muted">额度未验证</span>'}</div>${pending ? `<span class="api-service-plan-pending">${pending}</span>` : ''}</summary><div class="api-service-members">${members}</div></details>`;
  }).join('') + '</div>';
}
function localAPIServiceLaunchHTML(status) {
  const canLaunch = status?.running === true && !CODEX_API_OVERVIEW.error && (status.clients || []).some((row) => !row.disabled);
  return `<div class="api-service-launch-buttons" aria-label="通过本地 API 服务打开工具">${Object.entries(API_SERVICE_TOOLS).map(([target, name]) => `<button class="btn sm" data-act="codexAPIServiceOpen" data-target="${target}" ${canLaunch ? '' : 'disabled'} title="${canLaunch ? '选择客户端并检查配置后打开' : '请先配置客户端并启动本地服务'}">${icon('external')}打开 ${esc(name)}</button>`).join('')}</div>`;
}
function apiServiceLaunchCurrent(owner) {
  return owner && apiServiceLaunchPanel === owner && OFFICIAL_PANEL.epoch === owner.epoch && S.route === owner.route && $('#officialPanel') === owner.element;
}
function apiServiceLaunchBusy() { return apiClientWritePending || typeof WB !== 'undefined' && WB.launchPending; }
function paintAPIServiceLaunchProgress(owner) {
  if (!apiServiceLaunchCurrent(owner)) return;
  const output = $('#apiServiceLaunchProgress');
  if (output) output.textContent = owner.operationStage || '';
}
function renderAPIServiceLaunch(owner) {
  if (!apiServiceLaunchCurrent(owner) || !$('#apiServiceLaunchContent')) return;
  const preview = owner.preview, blocked = owner.loading || owner.submitted;
  const select = owner.clients.length > 1 ? `<div class="field"><label for="apiServiceLaunchClient">服务配置</label><select id="apiServiceLaunchClient" class="input" ${blocked ? 'disabled' : ''}><option value="" ${!owner.clientId ? 'selected' : ''} disabled>选择已启用的客户端</option>${owner.clients.map((row) => `<option value="${esc(row.id)}" ${row.id === owner.clientId ? 'selected' : ''}>${esc(row.name)}</option>`).join('')}</select></div>` : '';
  $('#apiServiceLaunchContent').innerHTML = `${select}${preview ? `<div class="wb-open-sum"><span class="wb-brand empty">${icon('route')}</span><div><b>${esc(preview.clientName)}</b><small>通过本地 API 服务</small></div></div><div class="wb-sec-title"><span>可选模型 · ${preview.models.length}</span></div><div class="wb-models">${preview.models.map((model) => `<span class="wb-mchip">${esc(model)}</span>`).join('')}</div><label class="wb-check-row"><input id="apiServiceLaunchOverride" type="checkbox" ${owner.catalogOverride ? 'checked' : ''} ${blocked ? 'disabled' : ''}>覆盖工具的模型选择栏</label>${preview.requiresModeChange ? '<p class="wb-hint">应用时开启 Claude Desktop 第三方模式，原账号聊天不删除。</p>' : ''}<p class="wb-hint">会先停止该 Agent 的当前任务，再应用所选 API。</p>` : `<p class="muted small">${owner.loading ? '正在检查本地配置…' : '选择服务配置后继续'}</p>`}<p id="apiServiceLaunchProgress" class="wb-hint" role="status" aria-live="polite">${esc(owner.operationStage || '')}</p>${owner.error ? `<div class="callout warn" role="status">${esc(owner.error)}${owner.executionUnknown ? '<br>操作结果未确认，请检查原工具状态；不会自动重试。' : '<br>请重新打开并检查配置后再试。'}</div>` : ''}<div class="modal-foot"><button class="btn" data-act="codexAPIServiceLaunch" data-apply-only="true" ${!preview || blocked ? 'disabled' : ''}>仅应用配置</button><button class="btn primary" data-act="codexAPIServiceLaunch" ${!preview || blocked ? 'disabled' : ''}>应用并打开</button></div>`;
}

async function readAPIServiceLaunchPreview(owner) {
  if (!apiServiceLaunchCurrent(owner) || !owner.clients.some((row) => row.id === owner.clientId) || owner.submitted) return;
  const sequence = ++owner.sequence, clientId = owner.clientId;
  owner.preview = null; owner.loading = true; owner.error = ''; renderAPIServiceLaunch(owner);
  try {
    const row = await api('/api/local/codex-api/launch/preview', { clientId, target: owner.target });
    if (!apiServiceLaunchCurrent(owner) || sequence !== owner.sequence || owner.clientId !== clientId) return;
    if (row?.ready !== true || row.clientId !== clientId || row.target !== owner.target || !/^mla-v1-[a-f0-9]{64}$/.test(row.revision || '') || typeof row.clientName !== 'string' || typeof row.catalogOverride !== 'boolean' || typeof row.requiresModeChange !== 'boolean' || !Array.isArray(row.models) || !row.models.length || row.models.length > 1000 || row.models.some((model) => typeof model !== 'string' || !model || model.length > 256)) throw new Error('本地配置校验失败，请重新打开');
    owner.catalogOverride = row.catalogOverride;
    owner.preview = { clientId, target: owner.target, revision: row.revision, clientName: row.clientName, catalogOverride: row.catalogOverride, requiresModeChange: row.requiresModeChange, models: [...row.models] };
  } catch (error) {
    if (apiServiceLaunchCurrent(owner) && sequence === owner.sequence) owner.error = error.message || '本地配置校验失败，请重新打开';
  } finally {
    if (apiServiceLaunchCurrent(owner) && sequence === owner.sequence) { owner.loading = false; renderAPIServiceLaunch(owner); }
  }
}
async function openAPIServiceLaunch(button) {
  if (apiServiceLaunchBusy() || button?.isConnected === false) return;
  const target = button?.dataset?.target;
  if (!Object.hasOwn(API_SERVICE_TOOLS, target)) throw new Error('不支持此工具');
  officialPanel('打开 ' + API_SERVICE_TOOLS[target], '<div id="apiServiceLaunchContent" class="stack"></div>');
  const owner = apiServiceLaunchPanel = { epoch: OFFICIAL_PANEL.epoch, element: $('#officialPanel'), route: S.route, target, clients: [], clientId: '', sequence: 0, preview: null, loading: true, submitted: false, error: '' };
  renderAPIServiceLaunch(owner);
  try {
    const status = await api('/api/local/codex-api');
    if (!apiServiceLaunchCurrent(owner)) return;
    if (status.running !== true) throw new Error('请先启动本地 API 服务');
    owner.clients = (status.clients || []).filter((row) => !row.disabled && typeof row.id === 'string' && row.id);
    if (!owner.clients.length) throw new Error('请先加入 API / 账号');
    owner.loading = false;
    if (owner.clients.length === 1) { owner.clientId = owner.clients[0].id; await readAPIServiceLaunchPreview(owner); }
    else renderAPIServiceLaunch(owner);
  } catch (error) {
    if (apiServiceLaunchCurrent(owner)) { owner.loading = false; owner.error = error.message || '无法读取本地服务'; renderAPIServiceLaunch(owner); }
  }
}
async function confirmAPIServiceLaunch(button) {
  const owner = apiServiceLaunchPanel;
  if (!apiServiceLaunchCurrent(owner) || apiServiceLaunchBusy() || owner.loading || owner.submitted || !owner.preview || button?.isConnected === false) return;
  if (!Object.hasOwn(API_SERVICE_TOOLS, owner.target)) throw new Error('不支持此工具');
  if ($('#apiServiceLaunchClient') && $('#apiServiceLaunchClient').value !== owner.clientId) throw new Error('服务配置已变化，请重新选择');
  const operationId = safeAgentOperationId(), controller = new AbortController(), applyOnly = button.dataset.applyOnly === 'true';
  const body = { protocol: SAFE_AGENT_CHANGE_PROTOCOL, operationId, clientId: owner.clientId, target: owner.target, revision: owner.preview.revision,
    catalogOverride: $('#apiServiceLaunchOverride')?.checked === true, allowModeChange: true, applyOnly };
  owner.catalogOverride = body.catalogOverride;
  owner.submitted = true; owner.controller = controller; owner.operationStage = '正在检查账号与工具…';
  const operation = { operationId, controller };
  const progress = inlineAgentProgress(owner, () => paintAPIServiceLaunchProgress(owner), applyOnly);
  if (typeof WB !== 'undefined') { WB.launchPending = true; WB.activeOperation = operation; }
  try {
    await withAPIClientWrite(async () => {
      renderAPIServiceLaunch(owner);
      const row = await launchAPI('/api/local/codex-api/launch/safe-v1', body, progress, { signal: controller.signal });
      requireSafeAgentReceipt(row, operationId);
      progress.finish();
      if (!apiServiceLaunchCurrent(owner)) return;
      owner.controller = null;
      closeOfficialPanel(); toast(applyOnly ? '配置已应用' : '已打开', 'ok');
    });
  } catch (error) {
    progress.fail(error);
    if (apiServiceLaunchCurrent(owner)) {
      owner.executionUnknown = error.uncertain === true || error.started === true || error.applied === true;
      owner.error = error.message || '操作结果未确认'; renderAPIServiceLaunch(owner);
    }
  } finally {
    owner.controller = null;
    if (typeof WB !== 'undefined' && WB.activeOperation === operation) { WB.activeOperation = null; WB.launchPending = false; }
    void loadCodexAPIOverview();
    if (typeof wbRefreshLaunchCards === 'function') void wbRefreshLaunchCards().catch(() => undefined);
  }
}

function codexAPIOverviewHTML() {
  const status = CODEX_API_OVERVIEW.status;
  const title = '<div class="official-card-top"><span class="official-card-name">本地 API 服务</span>';
  if (!status) return `<article class="wb-card account-card api-service-card" id="localAPIService">${title}<span class="account-state">${CODEX_API_OVERVIEW.error ? '暂不可用' : '未检查'}</span></div><p class="api-service-next">${CODEX_API_OVERVIEW.error ? '暂时无法读取本机服务，重试后继续配置。' : '把已保存的 API 或账号连接到本机工具。'}</p><div class="api-service-actions"><button class="btn primary" data-act="codexAPI">配置服务</button><button class="btn" data-act="codexAPIOverviewRefresh">${CODEX_API_OVERVIEW.error ? '重试' : '检查状态'}</button></div></article>`;
  const state = codexAPIState(status), stats = codexAPIHTTPStats(status);
  const clients = (status.clients || []).filter(row => !row.disabled).length;
  const summary = apiServiceSummary(status);
  const active = status.enabled === true && status.running === true && !CODEX_API_OVERVIEW.error && !CODEX_API_OVERVIEW.pending;
  const next = CODEX_API_OVERVIEW.error ? '未能更新本机状态，请刷新后再操作。' : !clients ? '在需要使用的 API 卡片上点击「加入 API 服务」。' : status.requiresStart ? '配置已保留，启动本次服务后即可接入。' : !status.running ? '配置已就绪，启动后供客户端调用。' : '服务已启动，可在管理中接入工具或复制地址。';
  const upstream = summary ? `${summary.total} 个上游 · ${summary.ready} 个已验证${summary.unknown ? ` · ${summary.unknown} 个未检查` : ''}${summary.unavailable ? ` · ${summary.unavailable} 个暂不可用` : ''}` : '上游状态未检查';
  return `<article class="wb-card account-card api-service-card${active ? ' account-is-active' : ''}" id="localAPIService">${title}<span class="account-state ${CODEX_API_OVERVIEW.error ? 'warn' : status.running ? 'ok' : 'muted'}">${CODEX_API_OVERVIEW.error ? '状态待刷新' : state.label}</span></div><p class="api-service-next">${next}</p><code class="api-service-address">${esc(status.baseUrl || '本机接口地址待检查')}</code><div class="api-service-usage"><span>${clients} 个启用客户端</span><span>${CODEX_API_OVERVIEW.error ? '请求状态待刷新' : `${stats.inflight} 个在途 · ${stats.requests} 次请求`}</span></div><p class="muted small api-service-note">${esc(upstream)}</p><div class="api-service-actions">${!clients && !status.running && !status.requiresStart ? '<button class="btn primary" data-act="codexAPI" data-view="upstreams">管理服务</button>' : `<button class="btn primary" data-act="codexAPIOverviewToggle" data-enabled="${state.nextEnabled}" ${CODEX_API_OVERVIEW.pending || CODEX_API_OVERVIEW.error ? 'disabled' : ''}>${state.action}</button><button class="btn" data-act="codexAPI">管理服务</button>`}${CODEX_API_OVERVIEW.error ? '<button class="btn" data-act="codexAPIOverviewRefresh">重试</button>' : ''}</div></article>`;
}
function renderCodexAPIOverview() {
  const card = $('#localAPIService');
  if (S.route === 'accounts' && card) patchOfficialObservationHTML(card, codexAPIOverviewHTML());
  if (typeof accountActiveSync === 'function') accountActiveSync();
  clearTimeout(CODEX_API_OVERVIEW.expiryTimer);
  CODEX_API_OVERVIEW.expiryTimer = 0;
  const expiry = apiServiceSummary(CODEX_API_OVERVIEW.status)?.nextExpiry;
  if (S.route === 'accounts' && card && !CODEX_API_OVERVIEW.error && Number.isFinite(expiry)) {
    // Expire only the display cache; never poll a provider or rotate credentials.
    CODEX_API_OVERVIEW.expiryTimer = setTimeout(renderCodexAPIOverview, Math.max(1, Math.min(2147483647, expiry - Date.now() + 1)));
  }
}
async function loadCodexAPIOverview() {
  const revision = ++CODEX_API_OVERVIEW.revision;
  const activityLocal = S.local;
  try {
    const status = await api('/api/local/codex-api');
    if (revision !== CODEX_API_OVERVIEW.revision) return;
    CODEX_API_OVERVIEW.status = status;
    CODEX_API_OVERVIEW.updatedAt = Date.now();
    CODEX_API_OVERVIEW.error = false;
    CODEX_API_OVERVIEW.activityLocal = activityLocal;
  } catch (_) {
    if (revision !== CODEX_API_OVERVIEW.revision) return;
    CODEX_API_OVERVIEW.error = true;
  }
  renderCodexAPIOverview();
  if (typeof scheduleAPIServiceObservation === 'function') scheduleAPIServiceObservation();
}
async function toggleCodexAPIService(button, overview = false) {
  if (CODEX_API_OVERVIEW.pending) return;
  const epoch = OFFICIAL_PANEL.epoch;
  CODEX_API_OVERVIEW.pending = true;
  CODEX_API_OVERVIEW.revision++;
  renderCodexAPIOverview();
  try {
    await busy(button, () => api('/api/local/codex-api/enabled', { enabled: button.dataset.enabled === 'true' }));
  } finally {
    CODEX_API_OVERVIEW.pending = false;
    await loadCodexAPIOverview();
    if (!overview && epoch === OFFICIAL_PANEL.epoch && !CODEX_API_OVERVIEW.error) updateAPIServiceReadOnly(CODEX_API_OVERVIEW.status);
  }
}
function showCreatedAPIKey(key) {
  const input = $('#apiCreatedKey');
  if (!input || typeof key !== 'string' || !key) return;
  input.type = 'password';
  input.value = key;
  OFFICIAL_PANEL.keyElement = input;
  $('#apiCreatedKeyBox').hidden = false;
}
function presentCreatedAPIKey(key) {
  // The POST is the only time the secret is available. Never put a fallible
  // list refresh between that response and rendering it to the same owner.
  officialPanel('客户端密钥', '<p class="muted small">密钥只显示这一次，请复制保存。</p><div id="apiCreatedKeyBox" class="field"><label for="apiCreatedKey">客户端密钥</label><div class="api-key-controls"><input class="input" id="apiCreatedKey" type="password" autocomplete="off" spellcheck="false" readonly><button class="icon-btn" data-act="toggleVis" data-for="apiCreatedKey" aria-label="显示或隐藏客户端密钥">' + icon('eye') + '</button><button class="icon-btn" data-act="codexAPIKeyCopy" aria-label="复制客户端密钥">' + icon('copy') + '</button></div></div>');
  showCreatedAPIKey(key);
  apiManagementShell('<button class="btn primary" data-act="codexAPI" data-view="clients">返回客户端</button>');
}
let officialLibraryRevision = 0;
function officialLibraryAccounts(accounts) {
  const seen = new Set(accounts.filter(a => a.officialAccount).map(a => a.officialAccount.id));
  const library = [...accounts];
  for (const row of S.officialRows || []) {
    if (!row.id || seen.has(row.id)) continue;
    seen.add(row.id);
    library.push({ id: `official:${row.id}`, name: row.name, baseUrl: '', officialAccount: row, libraryOnly: true });
  }
  return library;
}
async function loadOfficialLibrary(requireSuccess = false) {
  const revision = ++officialLibraryRevision;
  try {
    const out = await api('/api/local/codex/accounts');
    if (revision !== officialLibraryRevision || S.route !== 'accounts') return false;
    if (requireSuccess && !Array.isArray(out?.accounts)) throw new Error('账号列表读取失败');
    S.officialRows = Array.isArray(out.accounts) ? out.accounts : [];
    S.officialCleanupKnown = Array.isArray(out.accounts);
    renderLocalCards();
    syncOfficialCleanupAction();
    return true;
  } catch (error) {
    // Normal card reloads are optional; import receipts must include vault-only
    // accounts before reporting that the newly saved account is visible.
    if (requireSuccess) throw error;
    return false;
  }
}
function officialPlanLabel(row) {
	const plan = row?.usage?.plan;
	if (row?.usage?.verified && plan?.verified) {
		const family = ({ free: 'Free', go: 'Go', plus: 'Plus', pro: 'Pro', team: 'Team', business: 'Business', enterprise: 'Enterprise', edu: 'Edu' })[plan.family] || '未知套餐';
		return `${family}${plan.family === 'unknown' && plan.raw ? ` (${plan.raw})` : ''}（${apiServiceUsageFresh(row.usage, Date.now()) ? 'OpenAI 已验证' : '上次验证，待刷新'}）`;
	}
	return row?.plan ? `${row.plan}（导入提示，未验证）` : '套餐未知（未查询）';
}
function officialPlanAction(row) {
  if (row?.status === 'needs_login' || row?.status === 'expired') return { action: 'officialRelogin', label: '重新授权此账号' };
  if (row?.status === 'save_failed') return { action: 'officialRetrySave', label: '重试保存账号' };
  const usage = row?.usage, count = usage?.resetCreditsAvailable;
  // A badge never redeems a credit itself: the existing prepare + explicit
  // irreversible-action confirmation remains the sole consumption path.
  if (row?.mode === 'legacy_codex_cli' && row.status === 'ready' && apiServiceUsageFresh(usage, Date.now())
      && usage.rateLimit?.limitReached === true && Number.isSafeInteger(count) && count > 0) {
    return { action: 'officialResetCredit', label: '查看并确认消耗一次官方额度重置次数' };
  }
  return { action: 'officialUsageRefresh', label: '刷新账号额度与状态' };
}
function officialPlanBadge(row) {
  const verified = row?.usage?.verified === true && row?.usage?.plan?.verified === true;
  const family = verified ? row.usage.plan.family : String(row?.plan || '').toLowerCase();
  const names = { free: 'Free', go: 'Go', plus: 'Plus', pro: 'Pro', team: 'Team', business: 'Business', enterprise: 'Enterprise', edu: 'Edu' };
  const label = names[family] || (verified ? '未知套餐' : row?.plan || '套餐未知');
  const fresh = apiServiceUsageFresh(row?.usage, Date.now());
  const tone = verified && fresh && names[family] ? family : 'unknown';
  const content = `${esc(label)}${verified && !fresh ? '<small>待刷新</small>' : !verified && row?.plan ? '<small>未验证</small>' : ''}`;
  if (typeof row?.id !== 'string' || !row.id.trim()) return `<span class="official-plan-badge plan-${tone}" title="${esc(officialPlanLabel(row))}">${content}</span>`;
  const action = officialPlanAction(row);
  return `<button type="button" class="official-plan-badge plan-${tone}" data-act="${action.action}" data-id="${esc(row.id)}" title="${esc(officialPlanLabel(row) + ' · ' + action.label)}" aria-label="${esc(label + '：' + action.label)}">${content}</button>`;
}
function officialUsagePercent(value) {
  if (value == null || value === '') return '未知';
  const number = Number(value);
  return Number.isFinite(number) && number >= 0 && number <= 100 ? `${Math.round(number * 10) / 10}%` : '未知';
}
function officialUsageTime(value) {
  if (!value) return '未知';
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? '未知' : date.toLocaleString();
}
function officialSubscriptionRemaining(value) {
  if (value == null || value === '') return '未知';
  const seconds = Number(value);
  if (!Number.isFinite(seconds) || seconds < 0) return '未知';
  const days = Math.floor(seconds / 86400);
  const hours = Math.floor((seconds % 86400) / 3600);
  if (days > 0) return `${days} 天${hours > 0 ? ` ${hours} 小时` : ''}`;
  const minutes = Math.floor((seconds % 3600) / 60);
  if (hours > 0) return `${hours} 小时${minutes > 0 ? ` ${minutes} 分钟` : ''}`;
  return `${minutes} 分钟`;
}
function officialSubscriptionSummary(row) {
  const subscription = row?.subscription;
  if (officialSubscriptionFresh(row)) {
    if (subscription.status === 'expired') return '订阅已到期';
    return `订阅剩余 ${officialSubscriptionRemaining(Math.max(0, (Date.parse(subscription.activeUntil) - Date.now()) / 1000))}`;
  }
  if (subscription?.source === 'openai_subscription_api') return '订阅待核验';
  if (subscription?.activeUntil) return '订阅到期提示 · 未验证';
  return '订阅到期时间未知';
}
function officialSubscriptionFresh(row, now = Date.now()) {
  const sub = row?.subscription;
  return sub?.source === 'openai_subscription_api' && sub.verified === true && sub.freshness === 'fresh'
    && sub.identityEpoch === row.identityEpoch && Date.parse(sub.freshUntil || '') > now
    && Number.isFinite(Date.parse(sub.activeUntil || '')) && ['active','expired'].includes(sub.status)
    && (sub.status !== 'active' || Date.parse(sub.activeUntil) > now);
}
function officialSubscriptionDetails(row) {
  const subscription = row?.subscription;
  const source = ({ import_hint: '导入提示', jwt_hint: 'JWT 提示', openai_subscription_api: '官方订阅接口' })[subscription?.source] || '尚未核验';
  return `<div class="muted small">${esc(officialSubscriptionSummary(row))} · ${esc(source)}${subscription?.activeUntil ? ` · ${esc(officialUsageTime(subscription.activeUntil))}` : ''}</div>`;
}
function officialWindowName(window, fallback) {
  const minutes = Number.isFinite(window?.windowSeconds) && window.windowSeconds > 0 ? window.windowSeconds / 60 : Number(window?.windowMinutes);
  if (minutes === 10080) return '周额度';
  if (minutes === 300) return '5 小时额度';
  if (Number.isFinite(minutes) && minutes > 0) return `${minutes} 分钟额度`;
  return fallback;
}
function officialUsageSummary(row) {
  const usage = row?.usage;
  if (!usage?.verified) return usage?.status === 'unavailable' ? '额度不可用' : '额度未验证';
  const windows = [usage.rateLimit?.primary, usage.rateLimit?.secondary].filter((window) => window && window.remainingPercent != null);
  const resetCount = usage.resetCreditsAvailable == null ? null : Number(usage.resetCreditsAvailable);
  const reset = Number.isSafeInteger(resetCount) && resetCount >= 0 ? ` · 官方重置次数 ${resetCount}` : '';
  const fresh = apiServiceUsageFresh(usage, Date.now());
  if (!windows.length) return `${fresh ? '额度已验证' : '额度已过期'}${reset}`;
  const remaining = Math.min(...windows.map((window) => Number(window.remainingPercent)));
  return `${fresh ? '额度余量' : '过期额度'} ${officialUsagePercent(remaining)}${reset}`;
}
function officialUsageDetails(row) {
  const usage = row?.usage || {};
  const limits = [];
  const pushLimit = (limit, label) => {
    if (!limit) return;
    for (const [key, window] of [['primary', limit.primary], ['secondary', limit.secondary]]) {
      if (!window) continue;
      const reset = window.resetsAt ? ` · 重置 ${officialUsageTime(window.resetsAt)}` : '';
      const name = `${label} · ${officialWindowName(window, key === 'primary' ? '主窗口' : '次窗口')}`;
      limits.push(`<div class="official-detail-row" title="${esc(name + reset)}"><span>${esc(name)}</span><b>${esc(officialUsagePercent(window.remainingPercent))}</b></div>${window.resetsAt ? `<small class="official-detail-reset">重置 ${esc(officialUsageTime(window.resetsAt))}</small>` : ''}`);
    }
  };
  // Primary quota already has a visible bar. Keep only additional details here.
  if (usage.verified === true) {
    pushLimit(usage.codeReviewRateLimit, '代码审查');
    for (const limit of usage.additionalRateLimits || []) {
      const label = limit.normalModelSlug ? `模型 ${limit.normalModelSlug}` : (limit.name || '附加额度');
      pushLimit(limit, label);
    }
  }
  const error = usage.errorCode ? `<div class="official-detail-notice">最近刷新：${esc(usage.errorCode)}</div>` : '';
  const spend = usage.spendControl?.reached === true ? '<div class="official-detail-notice">已达消费上限</div>' : '';
  const resetCount = usage.resetCreditsAvailable == null ? null : Number(usage.resetCreditsAvailable);
  const resetCredits = Number.isSafeInteger(resetCount) && resetCount >= 0
    ? `<div class="official-detail-row"><span>官方重置次数</span><b>${resetCount}</b></div>` : '';
  const content = `${limits.join('')}${resetCredits}${spend}${error}`;
  const stale = usage.verified === true && !apiServiceUsageFresh(usage, Date.now());
  return content ? `<div class="official-card-details">${stale ? '<div class="official-detail-notice">额度数据已过期</div>' : ''}${content}</div>` : '';
}
function officialAccountStatusLabel(status) {
  return ({ ready: '可用', expired: '待刷新', needs_login: '需登录', refresh_failed: '刷新失败', save_failed: '保存失败' })[status] || '状态未知';
}
function officialStatusTone(status) {
  return ({ ready: 'ok', expired: 'warn', needs_login: 'bad', refresh_failed: 'bad', save_failed: 'bad' })[status] || 'muted';
}
function officialResetCreditButton(info) {
  const usage = info?.usage, count = usage?.resetCreditsAvailable;
  if (info?.mode !== 'legacy_codex_cli' || info.status !== 'ready' || typeof info.id !== 'string' || !info.id.trim() ||
      usage?.verified !== true || usage.source !== 'openai_codex_usage_api' || !Number.isSafeInteger(count) || count <= 0) return '';
  const fresh = apiServiceUsageFresh(usage, Date.now());
  if (fresh && usage.rateLimit?.limitReached !== true) return '';
  return `<button class="btn sm ghost" data-official-reset-credit="true" data-act="${fresh ? 'officialResetCredit' : 'officialUsageRefresh'}" data-id="${esc(info.id)}">${fresh ? `消耗官方重置次数（${count}）` : '刷新官方重置次数'}</button>`;
}
function officialAPIServiceSummary(account) {
  const service = account?.apiService;
  const clients = Array.isArray(service?.clients) ? service.clients : [];
  if (!clients.length) return { label: '未加入 API 服务', action: '加入 API 服务', member: false };
  const active = clients.filter(client => client.active !== false && !client.disabled && (client.text || client.image));
  const text = active.filter((client) => client.text).length;
  const image = active.filter((client) => client.image).length;
  const label = !active.length ? 'API 服务 · 未启用路由' : active.length === 1 ? `API 服务 · ${text ? '文字' : ''}${text && image ? ' + ' : ''}${image ? '图片' : ''}` : `API 服务 · ${active.length} 个客户端`;
  return { label, action: clients.length === 1 ? '移出 API 服务' : '管理 API 服务', member: true, client: clients.length === 1 ? clients[0] : null };
}
function officialQuotaHTML(info) {
  const usage = info.usage;
  if (!usage?.verified) return '<div class="official-quota-empty">额度未查询</div>';
  const windows = ['primary', 'secondary'].map((slot) => ({ slot, window: usage.rateLimit?.[slot] })).filter((row) => row.window);
  if (!windows.length) return `<div class="official-quota-empty">${esc(officialUsageSummary(info))}</div>`;
  return windows.map(({ window, slot }) => {
    const remaining = Number(window.remainingPercent), known = window.remainingPercent != null && Number.isFinite(remaining) && remaining >= 0 && remaining <= 100;
    return `<div class="official-quota"><div class="official-quota-head"><b>${esc(officialWindowName(window, slot === 'secondary' ? '次窗口' : '主窗口'))}</b><div class="official-quota-values"><span class="official-quota-percent">${apiServiceUsageFresh(usage, Date.now()) ? '剩余' : '过期数据'} ${esc(officialUsagePercent(window.remainingPercent))}</span></div></div>${known ? `<div class="official-quota-track"><span style="width:${remaining}%"></span></div>` : ''}<small>${window.resetsAt ? '重置于 ' + esc(officialUsageTime(window.resetsAt)) : '重置时间未知'}</small></div>`;
  }).join('');
}
// Official quota observations never imply a cash balance or a used-dollar bill.
function officialQuotaCostHTML() { return ''; }
function officialCardAction(action, label, symbol, id, extra = '', danger = false) {
  return `<button class="official-card-action${danger ? ' danger-text' : ''}" data-act="${action}" data-id="${esc(id)}" ${extra} title="${esc(label)}" aria-label="${esc(label)}">${icon(symbol)}</button>`;
}

function officialCostEstimateHTML() { return ''; }

let officialCostTimer = 0, officialCostGeneration = 0;
// These fragments contain observations, not editors. Patch existing nodes so
// timer updates keep focus and open service details, and equal data is a no-op.
function patchOfficialObservationNode(current, next) {
  if (current.nodeType !== next.nodeType || current.nodeName !== next.nodeName) { current.replaceWith(next); return; }
  if (current.nodeType !== 1) { if (current.nodeValue !== next.nodeValue) current.nodeValue = next.nodeValue; return; }
  if (current.nodeName === 'DETAILS' && current.hasAttribute('open')) next.setAttribute('open', '');
  for (const attr of [...current.attributes]) if (!next.hasAttribute(attr.name)) current.removeAttribute(attr.name);
  for (const attr of [...next.attributes]) if (current.getAttribute(attr.name) !== attr.value) current.setAttribute(attr.name, attr.value);
  const children = [...next.childNodes];
  for (let index = 0; index < children.length; index++) {
    const child = children[index], key = child.nodeType === 1 && child.getAttribute('data-observation-key');
    if (key) {
      const match = [...current.childNodes].find(row => row.nodeType === 1 && row.getAttribute('data-observation-key') === key);
      if (match && match !== current.childNodes[index]) current.insertBefore(match, current.childNodes[index] || null);
      else if (!match) { current.insertBefore(child, current.childNodes[index] || null); continue; }
    }
    if (current.childNodes[index]) patchOfficialObservationNode(current.childNodes[index], child);
    else current.appendChild(child);
  }
  while (current.childNodes.length > children.length) current.lastChild.remove();
}
function patchOfficialObservationHTML(current, html) {
  if (!current) return;
  if (!html) { current.remove(); return; }
  const template = document.createElement('template'); template.innerHTML = html;
  const next = template.content.firstElementChild;
  if (next) patchOfficialObservationNode(current, next);
}
function updateOfficialAccountObservations(root, accounts) {
  for (const card of root.querySelectorAll('.official-card[data-account-id]')) {
    const info = accounts.find(row => row.officialAccount?.id === card.dataset.accountId)?.officialAccount;
    if (!info || card.dataset.identityEpoch !== String(info.identityEpoch)) continue;
    const quotas = card.querySelector('.official-card-quotas');
    if (quotas) patchOfficialObservationHTML(quotas, `<div class="official-card-quotas">${officialQuotaHTML(info)}</div>`);
    const details = card.querySelector('.official-card-details'), detailsHTML = officialUsageDetails(info);
    if (details) patchOfficialObservationHTML(details, detailsHTML);
    else if (detailsHTML) quotas?.insertAdjacentHTML('afterend', detailsHTML);
    patchOfficialObservationHTML(card.querySelector('.official-plan-badge'), officialPlanBadge(info));
    const reset = card.querySelector('[data-official-reset-credit]'), resetHTML = officialResetCreditButton(info);
    if (reset) patchOfficialObservationHTML(reset, resetHTML);
    else if (resetHTML) card.insertAdjacentHTML('beforeend', resetHTML);
    patchOfficialObservationHTML(card.querySelector('.official-subscription'), officialSubscriptionCardHTML(info));
    patchOfficialObservationHTML(card.querySelector('.official-updated'), officialUpdatedHTML(info));
    const status = card.querySelector('.official-card-meta .account-state');
    patchOfficialObservationHTML(status, `<span class="account-state ${officialStatusTone(info.status)}">${esc(officialAccountStatusLabel(info.status))}</span>`);
    for (const tone of ['ok', 'warn', 'bad', 'muted']) {
      const name = 'account-card-' + tone, active = tone === officialStatusTone(info.status);
      if (card.classList.contains(name) !== active) card.classList.toggle(name, active);
    }
  }
}
function scheduleOfficialCostExpiry(accounts) {
  syncOfficialAccountCache();
  clearTimeout(officialCostTimer);
  const generation = ++officialCostGeneration, root = $('#localAccountList');
  const rows = accounts.filter((a) => a.officialAccount).map((a) => a.officialAccount);
  const expiry = Math.min(...rows.flatMap((info) => [info.usage?.freshUntil, info.subscription?.freshUntil, info.subscription?.activeUntil]).map((value) => Date.parse(value || '')).filter((at) => Number.isFinite(at) && at > Date.now()));
  if (!root || S.route !== 'accounts' || !Number.isFinite(expiry)) return;
  officialCostTimer = setTimeout(() => {
    if (generation !== officialCostGeneration || S.route !== 'accounts' || $('#localAccountList') !== root) return;
    updateOfficialAccountObservations(root, accounts);
    // Display expiry only: no provider polling or credential refresh.
    scheduleOfficialCostExpiry(accounts);
  }, Math.min(2147483647, Math.max(1, expiry - Date.now() + 1)));
}
// Refresh only the Bridge's sanitized in-memory observations. Provider quota
// reads remain owned by explicit actions and authorized backend maintenance.
let officialAccountCacheOwner = null;
function officialAccountCacheVisible(root) {
  return S.route === 'accounts' && document.hidden !== true && root && root.isConnected !== false && $('#localAccountList') === root;
}
function officialAccountCacheCurrent(owner) {
  return owner === officialAccountCacheOwner && officialAccountCacheVisible(owner?.root);
}
function scheduleOfficialAccountCache(owner, delay = 15000) {
  if (!officialAccountCacheCurrent(owner) || owner.pending || owner.timer) return;
  owner.timer = setTimeout(() => { owner.timer = 0; void refreshOfficialAccountCache(owner); }, delay);
}
function syncOfficialAccountCache(immediate = false) {
  const root = $('#localAccountList');
  if (!officialAccountCacheVisible(root) || officialAccountCacheOwner?.root !== root) {
    clearTimeout(officialAccountCacheOwner?.timer);
    officialAccountCacheOwner = null;
  }
  if (!officialAccountCacheVisible(root)) return;
  if (!officialAccountCacheOwner) officialAccountCacheOwner = { root, timer: 0, pending: false };
  if (immediate && !officialAccountCacheOwner.pending) {
    clearTimeout(officialAccountCacheOwner.timer); officialAccountCacheOwner.timer = 0;
  }
  scheduleOfficialAccountCache(officialAccountCacheOwner, immediate ? 0 : 15000);
}
async function refreshOfficialAccountCache(owner) {
  if (!officialAccountCacheCurrent(owner) || owner.pending) return;
  // An open editor owns its existing snapshot and any mutation receipt.
  if ($('#officialPanel') || $('#modalRoot')?.childElementCount || apiClientWritePending) { scheduleOfficialAccountCache(owner); return; }
  owner.pending = true;
  const localRevision = S.localAccountsRevision = (S.localAccountsRevision || 0) + 1;
  const libraryRevision = ++officialLibraryRevision, serviceRevision = ++CODEX_API_OVERVIEW.revision;
  const mutation = OFFICIAL_PANEL.mutation, epoch = OFFICIAL_PANEL.epoch;
  try {
    const results = await Promise.allSettled([api('/api/local/accounts'), api('/api/local/codex/accounts'), api('/api/local/codex-api')]);
    if (!officialAccountCacheCurrent(owner) || localRevision !== S.localAccountsRevision || libraryRevision !== officialLibraryRevision ||
        serviceRevision !== CODEX_API_OVERVIEW.revision || mutation !== OFFICIAL_PANEL.mutation || epoch !== OFFICIAL_PANEL.epoch ||
        $('#officialPanel') || $('#modalRoot')?.childElementCount || apiClientWritePending) return;
    if (results.some(row => row.status !== 'fulfilled')) {
      CODEX_API_OVERVIEW.error = true;
      renderCodexAPIOverview();
      if (typeof accountActiveUnavailable === 'function') accountActiveUnavailable();
      return;
    }
    const [local, library, service] = results.map(row => row.value);
    if (!Array.isArray(local?.accounts) || !Array.isArray(library?.accounts) || !Array.isArray(service?.clients)) {
      CODEX_API_OVERVIEW.error = true;
      renderCodexAPIOverview();
      if (typeof accountActiveUnavailable === 'function') accountActiveUnavailable();
      return;
    }
    S.local = local; S.officialRows = library.accounts; S.officialCleanupKnown = true;
    CODEX_API_OVERVIEW.status = service; CODEX_API_OVERVIEW.error = false;
    CODEX_API_OVERVIEW.activityLocal = local;
    const accounts = officialLibraryAccounts(local.accounts);
    updateOfficialAccountObservations(owner.root, accounts);
    if (typeof accountDailyReconcile === 'function') accountDailyReconcile();
    renderCodexAPIOverview(); scheduleOfficialCostExpiry(accounts); syncOfficialCleanupAction();
  } finally {
    owner.pending = false; scheduleOfficialAccountCache(owner);
  }
}
function apiServiceCostsHTML() { return ''; }
function officialSubscriptionCardHTML(info) {
  const stale = info.subscription?.source === 'openai_subscription_api' && !officialSubscriptionFresh(info);
  const pending = officialSubscriptionPending.has(info.id);
  return `<div class="official-subscription">${icon('clock')}<span>${esc(officialSubscriptionSummary(info))}${info.subscription?.activeUntil ? `<small>${stale ? '上次观测 ' : ''}${esc(officialUsageTime(info.subscription.activeUntil))}</small>` : ''}</span>${info.mode !== 'chatgpt_web_session' && Number.isSafeInteger(info.identityEpoch) && info.identityEpoch > 0 ? `<button class="icon-btn" data-act="officialSubscriptionRefresh" data-id="${esc(info.id)}" data-identity-epoch="${esc(info.identityEpoch)}" aria-label="核验订阅到期时间" title="核验订阅到期时间"${pending ? ' disabled aria-busy="true"' : ''}>${pending ? '<span class="spin"></span>' : icon('refresh')}</button>` : ''}</div>`;
}
function officialUpdatedHTML(info) {
  return `<div class="official-updated">${info.usage?.observedAt ? '额度更新于 ' + esc(officialUsageTime(info.usage.observedAt)) : info.usage?.verified ? '额度更新时间未知' : '尚未查询官方额度'}</div>`;
}
function officialAccountCard(a, usedBy, more) {
  const info = a.officialAccount;
  const apiService = officialAPIServiceSummary(a);
  const apiActionLabel = apiService.client?.primary ? '管理 API 服务' : apiService.action;
  const apiAction = `<button class="btn sm ghost" data-act="apiCardMembership" data-id="${esc(a.id)}">${esc(apiActionLabel)}</button>`;
  const webSession = info.mode === 'chatgpt_web_session';
  const recoverAction = info.status === 'save_failed'
    ? officialCardAction('officialRetrySave','重试保存','download',info.id)
    : ['needs_login','refresh_failed'].includes(info.status) || webSession && info.status === 'expired'
      ? officialCardAction('officialRelogin','重新登录','globe',info.id)
      : info.status === 'expired' ? officialCardAction('officialRefresh','更新凭据','key',info.id) : '';
  const accountActions = `${officialCardAction('officialRename','修改名称','edit',info.id)}${officialCardAction('officialUsageRefresh','刷新额度','refresh',info.id)}${recoverAction}${!webSession && a.libraryOnly ? officialCardAction('officialAttach','关联 API','plug',info.id) : ''}${officialCardAction('officialDelete','移除账号','trash',info.id,'',true)}`;
  const maintenanceActions = `${!webSession && !a.libraryOnly ? `<button class="btn" data-act="officialPool" data-id="${esc(a.id)}">${icon('gear')}账号池设置</button>` : ''}<button class="btn" data-act="officialUsageReset" data-id="${esc(info.id)}">${icon('refresh')}清除本地缓存</button>`;
  const launchButtons = typeof accountLaunchButtonsHTML === 'function' ? accountLaunchButtonsHTML(a) : '';
  const rulesAction = !webSession && !a.libraryOnly ? `<button class="btn sm ghost" data-act="accountModelRules" data-id="${esc(a.id)}" aria-label="${esc('模型规则 · ' + a.name)}" title="模型规则">规则</button>` : '';
  return `<article class="wb-card account-card official-card key-card account-card-${officialStatusTone(info.status)}" data-account-id="${esc(info.id)}" data-identity-epoch="${esc(info.identityEpoch)}" data-face="${webSession ? 'mist' : 'violet'}">
    <div class="key-face">
    <span class="key-glow" aria-hidden="true"></span>
    <div class="official-card-top"><span class="official-card-name" title="${esc(a.name)}">${esc(a.name)}</span>${officialPlanBadge(info)}</div>
    <div class="official-card-meta"><span class="account-state ${officialStatusTone(info.status)}">${esc(officialAccountStatusLabel(info.status))}</span>${!webSession && a.poolEnabled ? `<span>${esc(a.poolGroup)}</span>` : ''}${webSession ? '<span>网页会话 · 仅查额度</span>' : a.libraryOnly ? '<span>未关联 API</span>' : apiAction}${rulesAction}</div>
    <div class="official-card-source">${webSession ? 'ChatGPT 网页会话凭据' : 'Codex 账号凭据'}${usedBy.length ? ' · ' + esc(usedBy.join('、')) : ''} ${typeof officialRemoteSupportHTML === 'function' ? officialRemoteSupportHTML(a) : ''}</div>
    <div class="official-card-quotas">${officialQuotaHTML(info)}</div>
    </div>
    ${typeof accountDailyCardHTML === 'function' ? accountDailyCardHTML(a) : ''}
    ${officialUsageDetails(info)}
    ${officialSubscriptionCardHTML(info)}
    ${officialUpdatedHTML(info)}
    ${launchButtons}
    <div class="official-card-actions">${webSession ? '' : officialCardAction('accountTest','发送测试请求','play',a.id)}${accountActions}${more(maintenanceActions)}</div>
    ${webSession ? '' : officialResetCreditButton(info)}
  </article>`;
}
async function reconcileClosedOfficialLogin(id, attempt = 0) {
  if (!id) return;
  try {
    await api('/api/local/codex/login/cancel', { id });
    // Completion may have won before cancellation. Only this endpoint checks
    // the saved identity and links it; never rebuild a card from a cached row.
    const row = await api('/api/local/codex/login/status', { id });
    if (row.status === 'complete' && row.account) await refreshOfficialCards();
  } catch (error) {
    if (['Codex account not found', 'Codex account service closed', '账号已移除或变更，请刷新列表'].includes(error.message)) return;
    // This session no longer owns a dialog or its poll timer. Retry briefly
    // without putting its errors or controls into a newly opened panel.
    if (attempt < 2) setTimeout(() => void reconcileClosedOfficialLogin(id, attempt + 1), 3000);
  }
}
function disposeOfficialPanel() {
  if (apiManagementLeavePending) { apiManagementLeavePending.resolve(false); apiManagementLeavePending = null; }
  apiSettingsDrafts.clear();
  if (typeof stopAPIServiceObservation === 'function') stopAPIServiceObservation();
  if (typeof closeAPIRequests === 'function') closeAPIRequests(false);
  if (typeof accountDailyCancelPanel === 'function') accountDailyCancelPanel();
  apiClientPanel = null;
  apiServiceLaunchPanel?.controller?.abort();
  apiServiceLaunchPanel = null;
  const key = OFFICIAL_PANEL.keyElement || $('#apiCreatedKey');
  if (key) { key.value = ''; key.type = 'password'; }
  OFFICIAL_PANEL.keyElement = null;
  OFFICIAL_PANEL.epoch++;
  OFFICIAL_PANEL.credentials = null;
  OFFICIAL_PANEL.files = null;
  resetOfficialImportReview();
  OFFICIAL_PANEL.challenge = null;
  OFFICIAL_PANEL.importFormat = 'json';
  OFFICIAL_PANEL.replaceId = '';
  OFFICIAL_PANEL.loginPending = false;
  OFFICIAL_PANEL.cleanup = null;
  OFFICIAL_PANEL.cleanupSelection = null;
  OFFICIAL_PANEL.cleanupPending = false;
  OFFICIAL_PANEL.fileRevision++;
  clearTimeout(OFFICIAL_PANEL.timer);
	const session = OFFICIAL_PANEL.session;
	OFFICIAL_PANEL.session = null;
	if (session) void reconcileClosedOfficialLogin(session);
}
function mountOfficialPanel(shell, title, body, footer = '') {
  const root = $('#modalRoot');
  const current = root?.querySelector?.(`[data-official-shell="${shell}"]`);
  const focusedId = document.activeElement?.id || '';
  const keepTabFocus = /^(officialTab-(oauth|token|file)|accountAdd-(apikey|codex))$/.test(focusedId);
  disposeOfficialPanel();
  if (current && $('#officialPanel')) {
    $('#officialPanel').innerHTML = `${body}<div id="officialError" aria-live="polite"></div>`;
    $('#officialTitle').textContent = title;
    if (shell === 'standard') $('#officialPanelFooter').innerHTML = `${footer}<button class="btn" data-act="closeModal">关闭</button>`;
    root.dataset.officialShell = shell;
    if (keepTabFocus) document.getElementById?.(focusedId)?.focus?.();
    return;
  }
  root.dataset.officialShell = shell;
  if (shell === 'standard') {
    root.innerHTML = `<div class="modal-back" data-act="closeModal"><div class="modal account-modal" data-official-shell="standard" data-stop role="dialog" aria-modal="true" aria-labelledby="officialTitle"><div class="modal-head"><h3 id="officialTitle">${esc(title)}</h3><button class="icon-btn" data-act="closeModal" aria-label="关闭">✕</button></div><div class="modal-body stack" id="officialPanel">${body}<div id="officialError" aria-live="polite"></div></div><div class="modal-foot" id="officialPanelFooter">${footer}<button class="btn" data-act="closeModal">关闭</button></div></div></div>`;
  } else {
    root.innerHTML = `<div class="modal-back" data-act="closeModal"><div class="modal account-modal key-editor account-add-modal" data-key-editor data-official-shell="add" data-stop role="dialog" aria-modal="true" aria-labelledby="officialTitle">
    <div class="modal-head key-editor-head" data-face="iris"><span class="key-glow" aria-hidden="true"></span><div><h3 id="officialTitle">添加凭据</h3><p>API Key 与 Codex 账号都保存在本机</p></div><button class="icon-btn" data-act="closeModal" aria-label="关闭">✕</button></div>
    <div class="modal-body stack" id="officialPanel">${body}<div id="officialError" aria-live="polite"></div></div>
    <div class="modal-foot"><span class="muted small">只保存在这台电脑</span><span class="spacer"></span><button class="btn ghost" data-act="closeModal">取消</button></div>
  </div></div>`;
  }
  if (keepTabFocus) document.getElementById?.(focusedId)?.focus?.();
}
function officialPanel(title, body, footer = '') {
  mountOfficialPanel('standard', title, body, footer);
}
function officialAddPanel(body) {
  mountOfficialPanel('add', '添加凭据', body);
}
function closeOfficialPanel() {
  disposeOfficialPanel();
  const root = $('#modalRoot');
  root.innerHTML = '';
  root.dataset.officialShell = '';
}
async function openOfficialAccounts(replaceId = '') {
  if (typeof replaceId !== 'string') replaceId = '';
  const replacement = replaceId ? ` data-id="${esc(replaceId)}"` : '';
  const body = `${officialImportTabsHTML('oauth', replaceId)}<p class="muted small">浏览器授权会先生成授权页，完成登录后本窗口自动关联；设备码适用于本机回调不可用时。</p><div class="official-add-options"><button id="officialBrowserLogin" class="btn primary" data-act="officialLogin" data-mode="browser"${replacement}>浏览器授权</button><button id="officialDeviceLogin" class="btn" data-act="officialLogin" data-mode="device"${replacement}>设备码授权</button></div><div id="officialLoginState" class="small muted" aria-live="polite"></div><div id="officialImportPreview"></div>`;
  if (replaceId) officialPanel('重新登录 Codex 账号', body); else officialAddPanel(body);
}
function accountAddTabsHTML(active) {
  return `<div class="official-import-tabs account-add-tabs" role="tablist" aria-label="添加类型">${[['apikey','API Key'],['codex','Codex 账号']].map(([id,label])=>`<button id="accountAdd-${id}" type="button" role="tab" aria-selected="${id===active}" class="${id===active?'active':''}" data-act="accountAddTab" data-tab="${id}">${label}</button>`).join('')}</div>`;
}
async function openOfficialCleanup() {
  officialPanel('清理失效账号', '<div id="officialCleanupState" aria-live="polite" class="muted small">正在检查账号状态…</div>');
  const epoch = OFFICIAL_PANEL.epoch;
  const libraryRevision = officialLibraryRevision;
  let result;
  try { result = await api('/api/local/codex/cleanup/preview', {}); }
  catch (_) {
    if (epoch === OFFICIAL_PANEL.epoch && $('#officialCleanupState')) $('#officialCleanupState').textContent = '暂时无法检查，请稍后重试';
    return;
  }
  if (epoch !== OFFICIAL_PANEL.epoch || !$('#officialCleanupState')) return;
  const rows = result.accounts;
  if (!Array.isArray(rows) || rows.length > 64 || rows.some(row => !officialCleanupRowValid(row)) || new Set(rows.map(row => row.id)).size !== rows.length) {
    $('#officialCleanupState').textContent = '账号状态已变化，请重新检查'; return;
  }
  if (!rows.length && libraryRevision === officialLibraryRevision) {
    // A newer empty preview supersedes the old eligibility summary, including
    // a library read already in flight. Do not invalidate a newer library read.
    officialLibraryRevision++;
    S.officialCleanupKnown = false;
    syncOfficialCleanupAction();
  }
  OFFICIAL_PANEL.cleanup = rows.map(({ id, epoch, identityEpoch, revision }) => ({ id, epoch, identityEpoch, ...(revision ? { revision } : {}) }));
  OFFICIAL_PANEL.cleanupSelection = new Set(rows.filter(row => row.recoverable !== true).map(row => row.id));
  $('#officialCleanupState').innerHTML = rows.length
    ? `<p>选择要移除的账号及本机凭据</p><div class="stack">${rows.map(row => `<label class="restore-row"><input type="checkbox" data-cleanup-id="${esc(row.id)}" ${row.recoverable === true ? '' : 'checked'}><span class="li-main">${esc(row.name)}</span><span class="chip warn">${esc(officialCleanupReason(row))}</span></label>`).join('')}</div><p class="muted small">不删除聊天或项目。可恢复的账号默认保留，可手动勾选。</p><button id="officialCleanupConfirm" class="btn primary" data-act="officialCleanupConfirm">确认清理</button>`
    : '<p>没有可清理的失效账号</p>';
  syncOfficialCleanupSelection();
}
const OFFICIAL_CLEANUP_REASONS = { needs_login: '需重新登录', workspace_revoked: '工作空间权限已移除', account_unavailable: '账号不可用', usage_unauthorized: '授权已失效', quota_exhausted: '额度已用完', usage_forbidden: '额度访问受限' };
function officialCleanupRowValid(row) {
  if (!row || typeof row.id !== 'string' || !row.id || typeof row.name !== 'string' || !Number.isSafeInteger(row.epoch) || row.epoch <= 0 || !Number.isSafeInteger(row.identityEpoch) || row.identityEpoch <= 0) return false;
  // Legacy previews only cover the old needs-login deletion rule. Expanded
  // candidates must carry the backend's reason and exact observation revision.
  if (row.revision === undefined && row.reason === undefined && row.recoverable === undefined) return true;
  return typeof row.revision === 'string' && /^[a-f0-9]{64}$/.test(row.revision)
    && Object.hasOwn(OFFICIAL_CLEANUP_REASONS, row.reason) && typeof row.recoverable === 'boolean';
}
function officialCleanupReason(row) {
  return (OFFICIAL_CLEANUP_REASONS[row.reason] || '需重新登录') + (row.recoverable === true ? ' · 可恢复' : '');
}
function syncOfficialCleanupSelection() {
  const button = $('#officialCleanupConfirm');
  if (!button) return;
  const count = OFFICIAL_PANEL.cleanup?.filter(row => OFFICIAL_PANEL.cleanupSelection?.has(row.id)).length || 0;
  button.disabled = OFFICIAL_PANEL.cleanupPending || count === 0;
  button.textContent = `确认清理 ${count} 个账号`;
  for (const input of $('#officialCleanupState')?.querySelectorAll?.('input[data-cleanup-id]') || []) input.disabled = OFFICIAL_PANEL.cleanupPending;
}
async function confirmOfficialCleanup(button) {
  if (OFFICIAL_PANEL.cleanupPending || !OFFICIAL_PANEL.cleanup?.length) return;
  const accounts = OFFICIAL_PANEL.cleanup.filter(row => OFFICIAL_PANEL.cleanupSelection?.has(row.id)).map(row => ({ ...row }));
  if (!accounts.length) return;
  const token = officialMutationToken();
  OFFICIAL_PANEL.cleanupPending = true;
  syncOfficialCleanupSelection();
  try {
    const result = await busy(button, () => api('/api/local/codex/cleanup', { accounts }));
    if (!officialMutationCurrent(token)) return;
    OFFICIAL_PANEL.cleanup = null;
    const valid = result?.ok === true && Array.isArray(result.deleted) && Array.isArray(result.skipped);
    if (!valid) throw new Error('清理结果未确认，请重新检查');
    const message = `已清理 ${result.deleted.length} 个账号${result.skipped.length ? `，${result.skipped.length} 个状态已变化，已保留` : ''}${result.syncFailed ? '；列表同步失败，请刷新后重试' : ''}`;
    if (result.syncFailed) {
      OFFICIAL_PANEL.cleanup = accounts;
      OFFICIAL_PANEL.cleanupSelection = new Set(accounts.map(row => row.id));
      $('#officialCleanupState').innerHTML = `<p>${esc(message)}</p><button class="btn" data-act="officialCleanupConfirm">重试同步</button>`;
    } else $('#officialCleanupState').textContent = message;
    try { await refreshOfficialCards(token); }
    catch (_) { if (officialMutationCurrent(token) && $('#officialError')) $('#officialError').textContent = '清理已完成，列表刷新失败，请稍后刷新'; }
  } catch (_) {
    if (officialMutationCurrent(token)) {
      OFFICIAL_PANEL.cleanup = null;
      $('#officialCleanupState').innerHTML = '<p>清理结果未确认，请重新检查</p><button class="btn" data-act="officialCleanup">重新检查</button>';
    }
  } finally {
    if (officialMutationCurrent(token)) { OFFICIAL_PANEL.cleanupPending = false; syncOfficialCleanupSelection(); }
  }
}
function officialImportTabsHTML(active, replaceId = '') {
  return `${replaceId ? '' : accountAddTabsHTML('codex')}<div class="official-import-tabs" role="tablist" aria-label="Codex 账号添加方式">${[['oauth','授权登录'],['token','Token / JSON'],['file','文件导入']].map(([id,label])=>`<button id="officialTab-${id}" type="button" role="tab" aria-selected="${id===active}" class="${id===active?'active':''}" data-act="officialTab" data-tab="${id}" data-id="${esc(replaceId)}">${label}</button>`).join('')}</div>`;
}
function syncOfficialLoginButtons(mode = OFFICIAL_PANEL.challenge?.mode || 'browser') {
  for (const [id, choice] of [['officialBrowserLogin','browser'],['officialDeviceLogin','device']]) {
    const button = $('#' + id); if (!button) continue;
    button.disabled = !!(OFFICIAL_PANEL.loginPending || OFFICIAL_PANEL.session);
    button.className = choice === mode ? 'btn primary' : 'btn';
  }
  for (const id of ['officialTab-oauth','officialTab-token','officialTab-file','accountAdd-apikey','accountAdd-codex']) { const button = $('#' + id); if (button) button.disabled = !!(OFFICIAL_PANEL.loginPending || OFFICIAL_PANEL.session); }
}

function officialImportFormatHTML() {
  return '<div class="field"><label for="officialImportFormat">内容格式</label><select class="input" id="officialImportFormat"><option value="json">自动识别 JSON / JSONL</option><option value="access_token">Access Token（每行一个，或 JSON 数组）</option><option value="refresh_token">Refresh Token（每行一个，或 JSON 数组）</option></select></div>';
}
function openOfficialFiles(replaceId = '') {
  const body = `${officialImportTabsHTML('file',replaceId)}${officialImportFormatHTML()}<div class="field"><label for="officialFile">选择文件</label><input class="input" id="officialFile" type="file" accept="application/json,text/plain,.json,.jsonl,.ndjson,.txt" multiple><p class="muted small">支持多选，总计 1 MiB、最多 64 个账号。预览后确认导入。</p></div><div id="officialImportPreview"></div>`;
  if (replaceId) officialPanel('替换账号凭据', body); else officialAddPanel(body);
  OFFICIAL_PANEL.replaceId = replaceId;
}
function openOfficialPaste(replaceId = '') {
  const body = `${officialImportTabsHTML('token',replaceId)}${officialImportFormatHTML()}<div class="field"><label for="officialPasteText">凭据内容</label><textarea class="input" id="officialPasteText" rows="8" autocomplete="off" autocapitalize="off" spellcheck="false"></textarea><p class="muted small">支持 auth.json、账号数组、CLIProxyAPI、Sub2API 与 Session JSON。</p></div><div id="officialImportPreview"></div><button class="btn primary" data-act="officialPastePreview">预览导入</button>`;
  if (replaceId) officialPanel('替换账号凭据', body); else officialAddPanel(body);
  OFFICIAL_PANEL.replaceId = replaceId;
}
function resetOfficialImportReview() {
  OFFICIAL_PANEL.importPreview = null;
  OFFICIAL_PANEL.importSelection = null;
  OFFICIAL_PANEL.importChecking = false;
  OFFICIAL_PANEL.importResult = null;
}
function officialImportResultCurrent(result) {
  return result && OFFICIAL_PANEL.importResult === result && officialMutationCurrent(result.token)
    && result.revision === OFFICIAL_PANEL.fileRevision && !!$('#officialImportPreview');
}
function renderOfficialImportResult(result) {
  if (!officialImportResultCurrent(result)) return;
  $('#officialImportPreview').innerHTML = `<p role="status">${esc(result.message)}</p>${result.retry ? `<button class="btn" data-act="officialImportRefresh" ${result.pending ? 'disabled' : ''}>${result.pending ? '正在刷新…' : '刷新账号列表'}</button>` : ''}`;
}
async function refreshOfficialImportResult() {
  const result = OFFICIAL_PANEL.importResult;
  if (!officialImportResultCurrent(result) || result.pending) return;
  result.pending = true;
  renderOfficialImportResult(result);
  try {
    const refreshed = await refreshOfficialCards(result.token, true);
    if (!officialImportResultCurrent(result)) return;
    if (!refreshed) throw new Error('账号列表刷新已被更新的读取替代');
    if (result.status === 'complete') {
      closeOfficialPanel();
      toast('账号已导入', 'ok');
      return;
    }
    result.message = result.status === 'saved' ? '账号已保存，请在列表中完成关联或刷新凭据。' : '列表已刷新，请核对账号是否已导入。';
  } catch (_) {
    if (!officialImportResultCurrent(result)) return;
    result.message = result.status === 'complete' ? '账号已导入，列表刷新失败，请重试。'
      : result.status === 'saved' ? '账号已保存，列表刷新失败，请重试。' : '导入结果未确认，暂时无法刷新账号列表。';
  } finally {
    if (officialImportResultCurrent(result)) {
      result.pending = false;
      result.retry = true;
      renderOfficialImportResult(result);
    }
  }
}
function setOfficialImportPreview(preview) {
  if (!Array.isArray(preview?.accounts) || !preview.accounts.length || preview.accounts.length > 64 || preview.accounts.some(row => !row || typeof row.name !== 'string')) throw new Error('导入预览无效，请重新选择文件');
  OFFICIAL_PANEL.importPreview = { accounts: preview.accounts.map((row, index) => ({ name: row.name, mode: row.mode, index })) };
  OFFICIAL_PANEL.importSelection = new Set(preview.accounts.map((_, index) => index));
  renderOfficialImportReview();
}
function officialImportPreviewHTML(preview) {
  const selected = OFFICIAL_PANEL.importSelection || new Set(preview.accounts.map((_, index) => index));
  const checking = OFFICIAL_PANEL.importChecking;
  const labels = { verified: '认证通过', limited: '额度受限', invalid: '凭据失效', needs_refresh: '待刷新', unknown: '暂未确认', quota_only: '仅查额度' };
  const checked = preview.accounts.some(row => row.status);
  return `<section class="official-import-review"><div class="official-import-review-head"><b>确认导入 · ${preview.accounts.length} 个账号</b><button class="btn sm" data-act="officialImportCheck" ${checking ? 'disabled' : ''}>${checking ? '正在检测…' : checked ? '重新检测' : '检测可用性'}</button></div><div class="official-import-review-list">${preview.accounts.map((row, index) => `<label class="official-import-review-row"><input type="checkbox" data-import-index="${index}" ${selected.has(index) ? 'checked' : ''}><span>${esc(row.name)}${row.mode === 'chatgpt_web_session' ? '<small>网页会话 · 仅查额度</small>' : ''}</span><span class="chip ${row.status === 'invalid' ? 'bad' : row.status === 'verified' ? 'ok' : ''}">${labels[row.status] || '未检测'}</span></label>`).join('')}</div><div class="row"><button class="btn sm ghost" data-act="officialImportFilter" data-filter="all" ${checking ? 'disabled' : ''}>全选</button><button class="btn sm ghost" data-act="officialImportFilter" data-filter="invalid" ${!checked || checking ? 'disabled' : ''}>剔除失效</button><button class="btn sm ghost" data-act="officialImportFilter" data-filter="verified" ${!checked || checking ? 'disabled' : ''}>仅保留已通过</button><button class="btn primary" data-act="officialImport" ${!selected.size || checking ? 'disabled' : ''}>导入 ${selected.size} 个账号</button></div><p class="muted small">仅检查认证与额度，不消耗模型 Token。超时、待刷新不会当作失效；不修改原文件。</p><div id="officialImportCheckError" class="muted small" role="status"></div></section>`;
}
function renderOfficialImportReview() {
  if ($('#officialImportPreview') && OFFICIAL_PANEL.importPreview) $('#officialImportPreview').innerHTML = officialImportPreviewHTML(OFFICIAL_PANEL.importPreview);
}
function filterOfficialImport(filter) {
  if (!OFFICIAL_PANEL.importPreview || OFFICIAL_PANEL.importChecking) return;
  const rows = OFFICIAL_PANEL.importPreview.accounts;
  if (filter === 'all') OFFICIAL_PANEL.importSelection = new Set(rows.map((_, index) => index));
  else if (filter === 'invalid') OFFICIAL_PANEL.importSelection = new Set([...OFFICIAL_PANEL.importSelection].filter(index => rows[index]?.status !== 'invalid'));
  else if (filter === 'verified') OFFICIAL_PANEL.importSelection = new Set(rows.flatMap((row, index) => row.status === 'verified' ? [index] : []));
  renderOfficialImportReview();
}
async function checkOfficialImport() {
  if (OFFICIAL_PANEL.importChecking || !OFFICIAL_PANEL.importPreview || (!OFFICIAL_PANEL.credentials && !OFFICIAL_PANEL.files)) return;
  const epoch = OFFICIAL_PANEL.epoch, revision = OFFICIAL_PANEL.fileRevision, preview = OFFICIAL_PANEL.importPreview;
  const current = () => epoch === OFFICIAL_PANEL.epoch && revision === OFFICIAL_PANEL.fileRevision && OFFICIAL_PANEL.importPreview === preview;
  const body = { ...(OFFICIAL_PANEL.files ? { files: [...OFFICIAL_PANEL.files] } : { credentials: OFFICIAL_PANEL.credentials }), format: OFFICIAL_PANEL.importFormat };
  OFFICIAL_PANEL.importChecking = true;
  renderOfficialImportReview();
  let failed = false;
  try {
    const out = await api('/api/local/codex/import/check', body);
    if (!current()) return;
    const statuses = new Set(['verified','limited','invalid','needs_refresh','unknown','quota_only']);
    if (!Array.isArray(out?.accounts) || out.accounts.length !== preview.accounts.length || out.accounts.some((row, index) => row.index !== index || !statuses.has(row.status))) throw new Error('invalid result');
    preview.accounts = preview.accounts.map((row, index) => ({ ...row, status: out.accounts[index].status }));
  } catch (_) { failed = true; }
  finally {
    if (current()) {
      OFFICIAL_PANEL.importChecking = false;
      renderOfficialImportReview();
      if (failed && $('#officialImportCheckError')) $('#officialImportCheckError').textContent = '检测暂未完成，账号仍保留，可稍后重试。';
    }
  }
}
async function previewOfficialPaste(button) {
  const epoch = OFFICIAL_PANEL.epoch, revision = ++OFFICIAL_PANEL.fileRevision;
  const credentials = $('#officialPasteText')?.value || '', format = $('#officialImportFormat')?.value;
  OFFICIAL_PANEL.credentials = null;
  OFFICIAL_PANEL.files = null;
  resetOfficialImportReview();
  if ($('#officialImportPreview')) $('#officialImportPreview').innerHTML = '';
  if (!credentials.trim() || new TextEncoder().encode(credentials).length > 1024 * 1024) throw new Error('凭据内容为空或超过 1 MiB');
  const preview = await busy(button, () => api('/api/local/codex/import/preview', { credentials, format }));
  if (epoch !== OFFICIAL_PANEL.epoch || revision !== OFFICIAL_PANEL.fileRevision || !$('#officialImportPreview')) return;
  if (OFFICIAL_PANEL.replaceId && preview.accounts.length !== 1) throw new Error('替换导入只能选择一个账号');
  OFFICIAL_PANEL.credentials = credentials;
  OFFICIAL_PANEL.importFormat = format;
  $('#officialPasteText').value = '';
  setOfficialImportPreview(preview);
  if ($('#officialError')) $('#officialError').textContent = '';
}
async function openOfficialPool(id) {
  officialPanel('官方账号池设置', '<div id="officialPoolContent" class="stack"></div>');
  const epoch = OFFICIAL_PANEL.epoch;
  const local = await api('/api/local/accounts');
  if (epoch !== OFFICIAL_PANEL.epoch || !$('#officialPoolContent')) return;
  const row = (local.accounts || []).find((account) => account.id === id && account.officialAccount);
  if (!row) throw new Error('账号尚未关联');
  const strategies = [['', '兼容模式（优先级 / 当前负载）'], ['auto', '自动（套餐优先，再看额度）'], ['random', '随机'], ['single_account', '仅当前应用账号'], ['quota_high_first', '剩余额度高优先'], ['quota_low_first', '剩余额度低优先'], ['plan_high_first', '套餐高优先'], ['plan_low_first', '套餐低优先'], ['expiry_soon_first', '订阅提示较早到期优先'], ['custom', '自定义优先级 / 权重 / 备用层']];
  const strategyOptions = strategies.map(([value, label]) => `<option value="${value}" ${row.poolStrategy === value ? 'selected' : ''}>${label}</option>`).join('');
  $('#officialPoolContent').innerHTML = `<div class="field"><label for="officialPoolEnabled">账号池状态</label><select class="input" id="officialPoolEnabled"><option value="false" ${row.poolEnabled ? '' : 'selected'}>停用</option><option value="true" ${row.poolEnabled ? 'selected' : ''}>启用</option></select></div><div class="field"><label for="officialPoolGroup">账号池名称</label><input class="input" id="officialPoolGroup" maxlength="80" value="${esc(row.poolGroup || 'codex-official')}"></div><div class="field"><label for="officialPoolStrategy">路由策略（应用到工具的账号是策略锚点）</label><select class="input" id="officialPoolStrategy">${strategyOptions}</select><div class="muted small">套餐和额度排序只使用当前身份最近一次已验证且仍有效的官方数据；未知值排在已知值之后。订阅到期来自未验证提示，只参与排序。</div></div><div class="form-grid"><div class="field"><label for="officialPoolPriority">优先级（-1000–1000）</label><input class="input" id="officialPoolPriority" type="number" min="-1000" max="1000" value="${Number(row.priority) || 0}"></div><div class="field"><label for="officialPoolWeight">权重（0–1000）</label><input class="input" id="officialPoolWeight" type="number" min="0" max="1000" value="${Number(row.weight) || 0}"></div><div class="field"><label for="officialPoolBackup">自定义备用层</label><select class="input" id="officialPoolBackup"><option value="false" ${row.poolBackup ? '' : 'selected'}>主账号</option><option value="true" ${row.poolBackup ? 'selected' : ''}>备用账号</option></select></div><div class="field"><label for="officialPoolConcurrency">账号并发上限（0 表示不限）</label><input class="input" id="officialPoolConcurrency" type="number" min="0" max="10000" value="${Number(row.maxConcurrency) || 0}"></div></div><div class="field"><label for="officialQuotaReserve">额度保留百分比（0–99）</label><input class="input" id="officialQuotaReserve" type="number" min="0" max="99" value="${Number(row.quotaReservePercent) || 0}"><div class="muted small">高于 0 时，使用中的账号会自动更新额度；额度未知、过期或未超过保留值时暂停请求。</div></div><div class="row"><button class="btn primary" data-act="officialPoolSave" data-id="${esc(row.id)}">保存</button><button class="btn" data-act="closeModal">返回密钥库</button></div>`;
}
async function refreshOfficialCards(token, requireLibrary = false) {
  if (token && !officialMutationCurrent(token)) return false;
  const activityRoute = S.route, activityRouteRevision = S.routeRevision, activityRoot = $('#localAccountList');
  const revision = S.localAccountsRevision = (S.localAccountsRevision || 0) + 1;
  const refresh = ++OFFICIAL_PANEL.refresh;
  officialLibraryRevision++;
  const current = () => revision === S.localAccountsRevision && refresh === OFFICIAL_PANEL.refresh && (!token || officialMutationCurrent(token));
  let local;
  try { local = await api('/api/local/accounts'); }
  catch (error) {
    if (current() && S.route === activityRoute && S.routeRevision === activityRouteRevision && $('#localAccountList') === activityRoot && typeof accountActiveUnavailable === 'function') accountActiveUnavailable();
    throw error;
  }
  if (!current()) return false;
  S.local = local;
  if (S.route === 'accounts') {
    renderLocalCards();
    void loadCodexAPIOverview();
    const loaded = await loadOfficialLibrary(requireLibrary);
    if (requireLibrary && !loaded) return false;
  }
  return current();
}
function officialLoginChallenge(row) {
  if (row.status !== 'pending' || (row.expiresAt && !(Date.parse(row.expiresAt) > Date.now()))) return null;
  const device = row.mode === 'device';
  try {
    const url = new URL(device ? row.verificationURL : row.authorizationURL);
    if (url.protocol === 'https:' && url.host === 'auth.openai.com' && url.pathname === (device ? '/codex/device' : '/oauth/authorize') && !url.username && !url.password && !url.hash) {
      const code = device && typeof row.userCode === 'string' && /^[A-Za-z0-9-]{1,64}$/.test(row.userCode) ? row.userCode : '';
      if (device && !code) return null;
      return { id: row.id, mode: device ? 'device' : 'browser', url: url.href, code, expiresAt: row.expiresAt };
    }
  } catch (_) { /* Only a validated provider challenge becomes a link. */ }
  return null;
}
function officialLoginStartingHTML(mode) {
  const device = mode === 'device';
  return `<div class="official-login-status is-connecting" role="status"><span class="official-login-spinner" aria-hidden="true"></span><div><strong>${device ? '正在申请一次性设备码…' : '正在连接授权服务…'}</strong><p>${device ? '即将显示授权页和设备码。' : '正在准备安全登录页面。'}</p></div></div>`;
}
function officialLoginStartFailureHTML(error) {
  const message = String(error?.message || '');
  const detail = message === '官方账号服务尚未就绪' || /service unavailable|not ready/i.test(message)
    ? 'Codex 授权服务暂不可用，请刷新页面后重试。'
    : /network|fetch|connection|timeout/i.test(message)
      ? '无法连接授权服务，请检查网络后重试。'
      : '授权未能启动，请稍后重试。';
  return `<div class="official-login-status is-error" role="alert"><strong>授权启动失败</strong><p>${esc(detail)}</p></div>`;
}
function officialLoginProgressHTML(row) {
  if (row.status === 'failed' && row.errorCode === 'callback_unavailable') return '<div class="official-login-status is-error"><strong>本机回调不可用</strong><p>请改选上方的“设备码授权”继续。</p></div>';
  if (row.status === 'failed' && row.errorCode === 'expired') return '<div class="official-login-status is-error"><strong>授权已过期</strong><p>请重新发起授权。</p></div>';
  const terminalLabel = ({ complete: '已登录', failed: '登录失败', cancelled: '已取消' })[row.status];
  if (row.status !== 'pending') return `<div class="official-login-status ${row.status === 'failed' ? 'is-error' : ''}"><strong>${esc(terminalLabel || '登录状态未知')}</strong>${row.status === 'failed' ? '<p>请检查网络后重新授权。</p>' : ''}</div>`;
  const challenge = officialLoginChallenge(row);
  const link = challenge ? `<a class="btn sm" href="${esc(challenge.url)}" target="_blank" rel="noopener noreferrer" referrerpolicy="no-referrer">打开授权页</a><button class="btn sm" data-act="officialLoginCopy">复制授权链接</button>` : '';
  const linkField = challenge ? `<div class="field"><label for="officialAuthorizationURL">本次授权链接</label><input class="input" id="officialAuthorizationURL" readonly value="${esc(challenge.url)}" autocomplete="off" spellcheck="false"></div>` : '';
  const label = challenge ? (challenge.mode === 'device' ? '等待设备码确认' : '等待浏览器完成登录') : '正在准备授权信息';
  const instructions = challenge?.mode === 'device'
    ? '在授权页输入下方的一次性设备码并确认。此方式不依赖本机回调，结果会自动同步到此窗口。'
    : challenge ? '在打开的登录页完成授权，本窗口会自动连接并添加账号。若未自动返回，可粘贴下方 localhost 回调地址。'
      : '正在获取安全授权链接，请稍候。';
  const expiry = challenge?.expiresAt ? `<small>有效至 ${esc(officialUsageTime(challenge.expiresAt))}</small>` : '';
  const manual = challenge?.mode === 'browser' ? '<section class="official-login-callback"><div class="field"><label for="officialCallbackURL">未自动完成时，粘贴 localhost 回调地址</label><input class="input" id="officialCallbackURL" type="password" placeholder="完整回调地址（含 code 与 state）" autocomplete="off" autocapitalize="off" spellcheck="false"></div><button class="btn sm" data-act="officialLoginCallback">提交回调</button></section>' : '';
  const code = challenge?.code ? `<div class="row"><div class="official-device-code"><small>一次性设备码</small><code>${esc(challenge.code)}</code></div><button class="btn sm" data-act="officialLoginCopyCode">复制设备码</button></div>` : '';
  return `<div class="official-login-status"><strong>${esc(label)}</strong><p>${esc(instructions)}</p>${expiry}${code}${linkField}<div class="row">${link}<button class="btn sm" data-act="officialLoginCancel">取消登录</button></div>${manual}</div>`;
}
async function pollOfficialLogin(id, epoch) {
  if (epoch !== OFFICIAL_PANEL.epoch || OFFICIAL_PANEL.session !== id || !$('#officialLoginState')) return;
  const revision = ++OFFICIAL_PANEL.loginPollRevision;
  clearTimeout(OFFICIAL_PANEL.timer);
  try {
    const row = await api('/api/local/codex/login/status', { id });
    if (epoch !== OFFICIAL_PANEL.epoch || revision !== OFFICIAL_PANEL.loginPollRevision || !$('#officialLoginState')) return;
    if (OFFICIAL_PANEL.session !== id) return;
    OFFICIAL_PANEL.challenge = officialLoginChallenge(row);
    syncOfficialLoginButtons(row.mode);
    if ($('#officialError')) $('#officialError').textContent = '';
    const stateNode = $('#officialLoginState'), html = officialLoginProgressHTML(row);
    // Preserve a callback being typed while the same challenge is polled.
    if (stateNode.dataset.loginProgress !== html) {
      stateNode.innerHTML = html;
      stateNode.dataset.loginProgress = html;
    }
    if (row.status === 'pending') OFFICIAL_PANEL.timer = setTimeout(() => pollOfficialLogin(id, epoch), 1500);
    else {
      OFFICIAL_PANEL.session = null;
      OFFICIAL_PANEL.challenge = null;
      syncOfficialLoginButtons(row.mode);
      if (row.account) {
        // A completed login releases the controls, but its pending list read
        // must not close a newer authorization in this same dialog.
        const current = () => epoch === OFFICIAL_PANEL.epoch && revision === OFFICIAL_PANEL.loginPollRevision && OFFICIAL_PANEL.session === null && !!$('#officialLoginState');
        const finish = async () => {
          if (!current()) return;
          try {
            await refreshOfficialCards();
          } catch (_) {
            if (!current()) return;
            if ($('#officialError')) $('#officialError').textContent = '账号已登录，暂时无法刷新列表，正在重试';
            OFFICIAL_PANEL.timer = setTimeout(finish, 3000);
            return;
          }
          if (current()) closeOfficialPanel();
        };
        await finish();
      }
    }
  } catch (error) {
    if (epoch !== OFFICIAL_PANEL.epoch || revision !== OFFICIAL_PANEL.loginPollRevision || OFFICIAL_PANEL.session !== id) return;
    if (['Codex account not found', 'Codex account service closed', '账号已移除或变更，请刷新列表'].includes(error.message)) {
      OFFICIAL_PANEL.session = null;
      OFFICIAL_PANEL.challenge = null;
      syncOfficialLoginButtons();
      if ($('#officialLoginState')) $('#officialLoginState').textContent = '授权已失效，请重新授权';
      return;
    }
    if ($('#officialError')) $('#officialError').textContent = '暂时无法查询授权状态，正在重试';
    OFFICIAL_PANEL.timer = setTimeout(() => pollOfficialLogin(id, epoch), 3000);
  }
}
async function copyOfficialChallenge(codeOnly) {
  const challenge = OFFICIAL_PANEL.challenge;
  if (!challenge || challenge.id !== OFFICIAL_PANEL.session || (challenge.expiresAt && !(Date.parse(challenge.expiresAt) > Date.now()))) throw new Error('授权链接已失效，请重新授权');
  if (codeOnly && !challenge.code) return;
  await copyText(codeOnly ? challenge.code : challenge.url, codeOnly ? '设备码' : '授权链接');
}
Object.assign(ACTIONS, {
  officialCleanup: openOfficialCleanup,
  officialCleanupConfirm: confirmOfficialCleanup,
  accountAddTab: async (button) => {
    if (OFFICIAL_PANEL.loginPending || OFFICIAL_PANEL.session) return;
    const editingAPI = S.localEditorDraft?.input === $('#laID');
    if (editingAPI) {
      if (button.dataset.tab === 'apikey') return;
      if (typeof closeLocalEditor === 'function' && !await closeLocalEditor()) return;
    }
    if (button.dataset.tab === 'codex') return openOfficialAccounts();
    if (button.dataset.tab === 'apikey') return ACTIONS.localNew({ dataset: {} });
  },
  officialAccounts: openOfficialAccounts,
  officialRelogin: (button) => openOfficialAccounts(button.dataset.id),
  officialTab: (button) => {
    if (OFFICIAL_PANEL.loginPending || OFFICIAL_PANEL.session) return;
    const id = button.dataset.id || '';
    if (button.dataset.tab === 'oauth') return openOfficialAccounts(id);
    if (button.dataset.tab === 'token') return openOfficialPaste(id);
    if (button.dataset.tab === 'file') return openOfficialFiles(id);
  },
  officialLoginCopy: () => copyOfficialChallenge(false),
  officialLoginCopyCode: () => copyOfficialChallenge(true),
  officialLoginCallback: async (button) => {
    const id = OFFICIAL_PANEL.session, epoch = OFFICIAL_PANEL.epoch, input = $('#officialCallbackURL');
    if (!id || !input || OFFICIAL_PANEL.challenge?.mode !== 'browser') return;
    const callbackURL = input.value;
    if (!callbackURL || new TextEncoder().encode(callbackURL).length > 20 * 1024) throw new Error('请粘贴本次登录的完整回调地址');
    const stateNode = $('#officialLoginState');
    if (stateNode) stateNode.textContent = '正在核验回调…';
    try { await busy(button, () => api('/api/local/codex/login/callback', { id, callbackURL })); }
    catch (error) {
      if (epoch === OFFICIAL_PANEL.epoch && OFFICIAL_PANEL.session === id && stateNode) stateNode.textContent = '回调未通过核验，请检查完整地址后重试。';
      throw error;
    }
    if (epoch !== OFFICIAL_PANEL.epoch || OFFICIAL_PANEL.session !== id) return;
    input.value = '';
    if ($('#officialError')) $('#officialError').textContent = '';
    if (stateNode) stateNode.textContent = '回调已提交，正在确认登录状态…';
    await pollOfficialLogin(id, epoch);
  },
  officialLoginCancel: async (button) => {
    const id = OFFICIAL_PANEL.session, epoch = OFFICIAL_PANEL.epoch;
    if (!id) return;
    await busy(button, () => api('/api/local/codex/login/cancel', { id }));
    if (epoch !== OFFICIAL_PANEL.epoch || OFFICIAL_PANEL.session !== id) return;
    clearTimeout(OFFICIAL_PANEL.timer);
    OFFICIAL_PANEL.challenge = null;
    const stateNode = $('#officialLoginState');
    if (stateNode) {
      stateNode.textContent = '正在确认取消结果…';
      stateNode.dataset.loginProgress = stateNode.innerHTML;
    }
    // Completion can win before cancellation reaches the service. Keep the
    // session until its actual terminal status also performs account linkage.
    await pollOfficialLogin(id, epoch);
  },
  officialLogin: async (button) => {
    if (OFFICIAL_PANEL.loginPending || OFFICIAL_PANEL.session) return;
    const epoch = OFFICIAL_PANEL.epoch;
    const mode = button?.dataset.mode || 'browser';
    OFFICIAL_PANEL.loginPending = true;
    OFFICIAL_PANEL.loginPollRevision++;
    clearTimeout(OFFICIAL_PANEL.timer);
    syncOfficialLoginButtons(mode);
    const stateNode = $('#officialLoginState');
    if (stateNode) {
      stateNode.innerHTML = officialLoginStartingHTML(mode);
      stateNode.dataset.loginProgress = stateNode.innerHTML;
    }
    try {
      await busy(button, async () => {
        if (!['browser', 'device'].includes(mode)) throw new Error('请选择授权方式');
        const row = await api('/api/local/codex/login', { mode, ...(button?.dataset.id ? { replaceId: button.dataset.id } : {}) });
        if (epoch !== OFFICIAL_PANEL.epoch) { await reconcileClosedOfficialLogin(row.id); return; }
        OFFICIAL_PANEL.session = row.id;
        OFFICIAL_PANEL.challenge = null;
        // Cancellation must remain available even if the first status request
        // fails before the provider has returned a link or device code.
        if (stateNode) {
          stateNode.innerHTML = officialLoginProgressHTML({ status: 'pending', mode, ...row });
          stateNode.dataset.loginProgress = stateNode.innerHTML;
        }
        await pollOfficialLogin(row.id, epoch);
      });
    } catch (error) {
      if (epoch === OFFICIAL_PANEL.epoch && !OFFICIAL_PANEL.session) {
        if (stateNode) {
          stateNode.innerHTML = officialLoginStartFailureHTML(error);
          stateNode.dataset.loginProgress = stateNode.innerHTML;
        }
      }
    } finally { if (epoch === OFFICIAL_PANEL.epoch) { OFFICIAL_PANEL.loginPending = false; syncOfficialLoginButtons(mode); } }
  },
  officialReimport: (button) => openOfficialFiles(button.dataset.id),
  officialChooseFile: (button) => openOfficialFiles(button?.dataset.id || ''),
  officialPaste: (button) => openOfficialPaste(button?.dataset.id || ''),
  officialPastePreview: previewOfficialPaste,
  officialImportCheck: checkOfficialImport,
  officialImportFilter: (button) => filterOfficialImport(button.dataset.filter),
  officialImportRefresh: refreshOfficialImportResult,
  officialImport: async (button) => {
    if (OFFICIAL_PANEL.importChecking || (!OFFICIAL_PANEL.credentials && !OFFICIAL_PANEL.files)) return;
    const selection = OFFICIAL_PANEL.importSelection ? [...OFFICIAL_PANEL.importSelection].sort((a,b) => a-b) : null;
    if (selection && !selection.length) return;
    const token = officialMutationToken();
    const revision = OFFICIAL_PANEL.fileRevision;
    const credentials = OFFICIAL_PANEL.credentials;
    const files = OFFICIAL_PANEL.files;
    const format = OFFICIAL_PANEL.importFormat;
    const replaceId = OFFICIAL_PANEL.replaceId;
    OFFICIAL_PANEL.credentials = null;
    OFFICIAL_PANEL.files = null;
    if ($('#officialImportPreview')) $('#officialImportPreview').innerHTML = '';
    resetOfficialImportReview();
    // Keep only a safe receipt after submission. A failed list refresh must not
    // invite resubmitting credentials that were already committed or rotated.
    const result = OFFICIAL_PANEL.importResult = { token, revision, status: 'unknown', pending: true, retry: false, message: '正在导入…' };
    if ($('#officialError')) $('#officialError').textContent = '';
    renderOfficialImportResult(result);
    try {
      const out = await busy(button, () => api('/api/local/codex/import', { ...(files ? { files } : { credentials }), format, ...(replaceId ? { replaceId } : {}), ...(selection ? { selection } : {}) }));
      if (!officialImportResultCurrent(result)) return;
      result.status = out?.ok === true ? 'complete' : out?.saved === true ? 'saved' : 'unknown';
    } catch (_) {
      if (!officialImportResultCurrent(result)) return;
    }
    if (!officialImportResultCurrent(result)) return;
    result.pending = false;
    if (result.status === 'unknown') {
      result.message = '导入结果未确认，请刷新账号列表后核对。';
      result.retry = true;
      renderOfficialImportResult(result);
      return;
    }
    result.message = '账号已保存，正在刷新列表…';
    await refreshOfficialImportResult();
  },
  officialAttach: async (button) => { const token = officialMutationToken(); await api('/api/local/codex/attach', { id: button.dataset.id }); await refreshOfficialCards(token); },
  officialRefresh: async (button) => { const token = officialMutationToken(); await api('/api/local/codex/refresh', { id: button.dataset.id }); await refreshOfficialCards(token); },
  officialUsageRefresh: async (button) => {
    const token = officialMutationToken();
    const out = await busy(button, () => api('/api/local/codex/usage/refresh', { id: button.dataset.id }));
	await refreshOfficialCards(token);
	if (!officialMutationCurrent(token)) return;
	toast(out.ok ? '官方额度已刷新' : (out.error || '额度状态暂时不可用'), out.ok ? 'ok' : 'bad');
  },
  officialSubscriptionRefresh: async (button) => {
    const id = button.dataset.id, identityEpoch = Number(button.dataset.identityEpoch), route = S.route;
    if (button.isConnected === false || !Number.isSafeInteger(identityEpoch) || identityEpoch < 1 || officialSubscriptionPending.has(id)) return;
    const row = officialLibraryAccounts(S.local?.accounts || []).find(account => account.officialAccount?.id === id)?.officialAccount;
    if (!row || row.identityEpoch !== identityEpoch) return;
    const token = officialMutationToken();
    officialSubscriptionPending.add(id);
    try {
      const out = await busy(button, () => api('/api/local/codex/subscription/refresh', {id,identityEpoch}));
      if (S.route !== route || !officialMutationCurrent(token)) return;
      const current = officialLibraryAccounts(S.local?.accounts || []).find(account => account.officialAccount?.id === id)?.officialAccount;
      if (!current || current.identityEpoch !== identityEpoch) return;
      // Reload sanitized observations; never splice a late response into a
      // replacement account or overwrite a newer credential epoch.
      await refreshOfficialCards(token);
      if (S.route === route && officialMutationCurrent(token)) toast(out.ok ? '订阅已核验' : out.error || '订阅暂不可确认', out.ok ? 'ok' : 'bad');
    } finally {
      officialSubscriptionPending.delete(id);
      // A card reread can replace the original busy button. Reconcile only
      // this control so completion cannot repaint another page or card draft.
      for (const current of document.querySelectorAll?.('[data-act="officialSubscriptionRefresh"]') || []) {
        if (current.dataset.id !== id || Number(current.dataset.identityEpoch) !== identityEpoch) continue;
        current.disabled = false; current.removeAttribute('aria-busy'); current.innerHTML = icon('refresh');
      }
    }
  },
  officialUsageReset: async (button) => {
    if (!await confirm('只清除本机缓存的额度和冷却状态，不会重置官方账号额度。继续？')) return;
    const token = officialMutationToken();
    const out = await busy(button, () => api('/api/local/codex/usage/reset', { id: button.dataset.id }));
    await refreshOfficialCards(token);
    if (!officialMutationCurrent(token)) return;
    toast(out.message || '已清除本地额度缓存，请刷新额度', 'ok');
  },
  officialResetCredit: async (button) => {
    const token = officialMutationToken();
    let prepared = await busy(button, () => api('/api/local/codex/reset-credit/prepare', { id: button.dataset.id }));
    if (!officialMutationCurrent(token)) return;
    let request = prepared.request || {};
    if (request.state === 'succeeded') {
      // A click after an acknowledged success may create one new operation.
      // The server requires a fresh observation after that exact predecessor;
      // retrying this CAS never advances through multiple redemptions.
      const usage = await busy(button, () => api('/api/local/codex/usage/refresh', { id: button.dataset.id }));
      if (!officialMutationCurrent(token)) return;
      if (usage.ok !== true) {
        toast(usage.error || '请先刷新官方额度，确认可用重置次数', 'bad');
        return;
      }
      prepared = await busy(button, () => api('/api/local/codex/reset-credit/prepare', { id: button.dataset.id, previousRedeemRequestId: request.redeemRequestId }));
      if (!officialMutationCurrent(token)) return;
      request = prepared.request || {};
    }
    if (request.state === 'succeeded') {
      toast('这次官方重置已经完成，请刷新额度确认', 'ok');
      return;
    }
    if (request.state === 'unknown' || request.state === 'consuming') {
      toast(request.state === 'consuming' ? '官方重置正在处理，请稍后刷新查看' : '上次重置结果未知，请核实额度；不会重复消耗次数', 'bad');
      return;
    }
    if (prepared.ok === false || !['prepared', 'failed'].includes(request.state) || !request.redeemRequestId) {
      toast(prepared.error || '官方重置状态不可用，请刷新后重试', 'bad');
      return;
    }
    if (!await confirm('这会向 OpenAI 消耗一次官方额度重置次数，且不可撤销。继续？')) return;
    // The confirmation can leave the panel open while another mutation wins.
    // Re-check immediately before the irreversible provider call.
    if (!officialMutationCurrent(token)) return;
    const out = await busy(button, () => api('/api/local/codex/reset-credit/consume', { id: button.dataset.id, redeemRequestId: request.redeemRequestId }));
    await refreshOfficialCards(token);
    if (!officialMutationCurrent(token)) return;
    if (!out.ok) {
      toast(out.error || '官方额度重置未完成；请先刷新额度确认', 'bad');
      return;
    }
    toast(out.message || '已消费一次官方额度重置次数，请刷新额度确认结果', 'ok');
  },
  officialAPIMember: (button) => ACTIONS.apiCardMembership({ dataset: { id: button.dataset.accountId || button.dataset.id }, isConnected: button.isConnected }),
  officialPool: async (button) => openOfficialPool(button.dataset.id),
  officialPoolSave: async (button) => {
    const token = officialMutationToken();
    const reserveInput = $('#officialQuotaReserve'), enabledInput = $('#officialPoolEnabled'), groupInput = $('#officialPoolGroup');
    const strategyInput = $('#officialPoolStrategy'), priorityInput = $('#officialPoolPriority'), weightInput = $('#officialPoolWeight'), backupInput = $('#officialPoolBackup'), concurrencyInput = $('#officialPoolConcurrency');
    const reserveText = reserveInput.value, enabledText = enabledInput.value, groupText = groupInput.value;
    const strategyText = strategyInput.value, priorityText = priorityInput.value, weightText = weightInput.value, backupText = backupInput.value, concurrencyText = concurrencyInput.value;
    const reserve = Number(reserveText);
    if (!Number.isInteger(reserve) || reserve < 0 || reserve > 99) throw new Error('额度保留百分比必须是 0–99 的整数');
    if (!['', 'auto', 'random', 'single_account', 'quota_high_first', 'quota_low_first', 'plan_high_first', 'plan_low_first', 'expiry_soon_first', 'custom'].includes(strategyText)) throw new Error('账号池路由策略无效');
    const priority = Number(priorityText), weight = Number(weightText), maxConcurrency = Number(concurrencyText);
    if (!Number.isInteger(priority) || priority < -1000 || priority > 1000) throw new Error('账号池优先级必须是 -1000–1000 的整数');
    if (!Number.isInteger(weight) || weight < 0 || weight > 1000) throw new Error('账号池权重必须是 0–1000 的整数');
    if (!Number.isInteger(maxConcurrency) || maxConcurrency < 0 || maxConcurrency > 10000) throw new Error('账号并发上限必须是 0–10000 的整数');
    const enabled = enabledText === 'true';
    const group = groupText.trim();
    if (enabled && !group) throw new Error('启用账号池前需要填写账号池名称');
    await busy(button, () => api('/api/local/accounts/pool', { id: button.dataset.id, enabled, group, strategy: strategyText, priority, weight, backup: backupText === 'true', maxConcurrency, quotaReservePercent: reserve }));
    await refreshOfficialCards(token);
    if (!officialMutationCurrent(token) || $('#officialQuotaReserve') !== reserveInput || $('#officialPoolEnabled') !== enabledInput || $('#officialPoolGroup') !== groupInput || $('#officialPoolStrategy') !== strategyInput || $('#officialPoolPriority') !== priorityInput || $('#officialPoolWeight') !== weightInput || $('#officialPoolBackup') !== backupInput || $('#officialPoolConcurrency') !== concurrencyInput || reserveInput.value !== reserveText || enabledInput.value !== enabledText || groupInput.value !== groupText || strategyInput.value !== strategyText || priorityInput.value !== priorityText || weightInput.value !== weightText || backupInput.value !== backupText || concurrencyInput.value !== concurrencyText) return;
    closeOfficialPanel();
    toast('账号池设置已保存', 'ok');
  },
  officialRetrySave: async (button) => { const token = officialMutationToken(); await busy(button, () => api('/api/local/codex/retry-save', { id: button.dataset.id })); await refreshOfficialCards(token); },
  officialDelete: async (button) => { if (!await confirm('移除这个 Codex 账号？不会删除原会话。')) return; const token = officialMutationToken(); await api('/api/local/codex/delete', { id: button.dataset.id }); await refreshOfficialCards(token); },
  codexAPI: button => openCodexAPI(button?.dataset?.view || apiServiceView),
  codexAPIEnabled: (button) => toggleCodexAPIService(button),
  codexAPIOverviewToggle: (button) => toggleCodexAPIService(button, true),
  codexAPIOverviewRefresh: (button) => busy(button, loadCodexAPIOverview),
  codexAPIKeyCopy: async () => {
    const input = $('#apiCreatedKey');
    if (!input?.value || $('#apiCreatedKeyBox')?.hidden) throw new Error('密钥仅在创建或重置后可复制');
    await copyText(input.value, '客户端密钥');
  },
  codexAPIRevoke: (button) => withAPIClientWrite(async () => { const owner = requireAPIClientPanel('service', button.dataset.id); if (!await confirm('撤销此客户端？它的密钥将失效，上游 API 会保留。')) return; if (!currentAPIClientPanel(owner)) return; await busy(button, () => api('/api/local/codex-api/clients/revoke', { id: button.dataset.id })); await loadCodexAPIOverview(); if (currentAPIClientPanel(owner) && !CODEX_API_OVERVIEW.error) updateAPIServiceReadOnly(CODEX_API_OVERVIEW.status, owner); }),
  codexAPIEdit: (button) => apiManagementLeave(() => openCodexAPIClient(button.dataset.id)),
  codexAPINew: () => apiManagementLeave(() => openCodexAPIClient('')),
  codexAPIServiceOpen: button => apiManagementLeave(() => openAPIServiceLaunch(button)),
  codexAPIServiceLaunch: confirmAPIServiceLaunch,
  codexAPISave: (button) => withAPIClientWrite(async () => {
    const owner = requireAPIClientPanel('editor', button.dataset.id || '');
    const modelAliases = {};
    for (const line of $('#apiClientAliases').value.split('\n').map((v) => v.trim()).filter(Boolean)) {
      const split = line.indexOf('=');
      if (split < 1 || split === line.length - 1) throw new Error('模型别名格式：别名 = 模型 ID');
      const alias = line.slice(0, split).trim(), actual = line.slice(split + 1).trim();
      if (Object.hasOwn(modelAliases, alias)) throw new Error('模型别名不能重复');
      Object.defineProperty(modelAliases, alias, { value: actual, enumerable: true });
    }
    const maxConcurrency = Number($('#apiClientConcurrency').value);
    if (!Number.isInteger(maxConcurrency) || maxConcurrency < 1 || maxConcurrency > 128) throw new Error('并发上限为 1–128');
    const binding = readAPIClientBinding();
    readAPIClientAccount(owner);
    const readModels = (id) => [...new Set($(id).value.split('\n').map((value) => value.trim()).filter(Boolean))];
    owner.writeAttempted = true;
    const fields = Array.from(owner.element.querySelectorAll?.('input,select,textarea') || [], node => ({ node, disabled: node.disabled }));
    for (const { node } of fields) node.disabled = true;
    let row;
    try { row = await busy(button, () => api('/api/local/codex-api/clients', { id: button.dataset.id, name: $('#apiClientName').value,
      ...binding, disabled: $('#apiClientDisabled').value === 'true',
      modelAllowlist: readModels('#apiClientAllowlist'), modelDenylist: readModels('#apiClientDenylist'),
      modelAllowPrefixes: readModels('#apiClientAllowPrefixes'), modelDenyPrefixes: readModels('#apiClientDenyPrefixes'),
      modelAliases, maxConcurrency })); }
    catch (error) {
      if (currentAPIClientPanel(owner)) {
        // Only a definite rejection is safe to resubmit. A lost create response
        // may already have committed a one-time key; keep the draft for review.
        if ([400,401,403,404,409,422].includes(error.status)) owner.writeAttempted = false;
        if ($('#officialError')) $('#officialError').textContent = owner.writeAttempted ? `${error.message}。结果未确认，输入已保留；请返回客户端核对，避免重复创建。` : `${error.message}。输入已保留，可修正后保存。`;
        syncAPIClientDraft();
      }
      return;
    } finally { for (const { node, disabled } of fields) if (node.isConnected) node.disabled = disabled; }
    if (!currentAPIClientPanel(owner)) return;
    if (!row?.id || row.ok === false) { $('#officialError').textContent = '保存结果未确认，输入已保留；请返回客户端核对。'; syncAPIClientDraft(); return; }
    if (row.key) presentCreatedAPIKey(row.key);
    else if (!owner.clientId) throw new Error('客户端已提交，但未收到密钥，请检查列表后重置密钥');
    else await openCodexAPI('clients');
    void loadCodexAPIOverview();
  }),
  codexAPIRotate: (button) => withAPIClientWrite(async () => {
    const owner = requireAPIClientPanel(null, button.dataset.id);
    if (!await confirm('重置后旧客户端密钥将立即失效。继续？')) return;
    if (!currentAPIClientPanel(owner)) return;
    owner.writeAttempted = true;
    // Rotate is atomic: no stale editor fields or hidden policies are written.
    const row = await busy(button, () => api('/api/local/codex-api/clients/rotate', { id: button.dataset.id }));
    if (!currentAPIClientPanel(owner)) return;
    if (!row.key) throw new Error('重置已提交，但未收到密钥，请重新打开后检查');
    presentCreatedAPIKey(row.key);
    void loadCodexAPIOverview();
  }),
});
function apiClientAccounts(local) {
  return (local.accounts || []).filter((account) => !account.libraryOnly && account.officialAccount?.mode !== 'chatgpt_web_session');
}
function currentAPIClientPanel(owner) {
  return owner && apiClientPanel === owner && owner.epoch === OFFICIAL_PANEL.epoch && $('#officialPanel') === owner.element;
}
function requireAPIClientPanel(mode, clientId) {
  const owner = apiClientPanel;
  if (!currentAPIClientPanel(owner) || mode && owner.mode !== mode) throw new Error('请重新打开密钥设置');
  if (owner.writeAttempted) throw new Error('操作结果待确认，请重新打开密钥设置');
  if (clientId !== undefined && (owner.mode === 'editor' ? owner.clientId !== clientId : !owner.clients.some((row) => row.id === clientId))) throw new Error('密钥已变更，请重新打开');
  return owner;
}
function readAPIClientAccount(owner) {
  const accountId = $('#apiClientAccount')?.value || '';
  if (!accountId || !owner.accounts.some((row) => row.id === accountId)) throw new Error('请选择一个可用账号；原账号已移除时需要重新选择');
  return accountId;
}
function readAPIClientBinding() {
  const accountId = $('#apiClientAccount')?.value || '';
  if (!accountId) throw new Error('请选择一个可用账号；原账号已移除时需要重新选择');
  const modelPrefix = ($('#apiClientModelPrefix')?.value || '').trim();
  if (modelPrefix && !/^[A-Za-z0-9][A-Za-z0-9._-]{0,62}\/$/.test(modelPrefix)) throw new Error('模型命名空间须以字母或数字开头，仅含字母、数字、点、下划线或短横线，并以 / 结尾（最多 64 字符）');
  const imageAccountIds = [...($('#apiClientImageAccounts')?.selectedOptions || [])].map((option) => option.value);
  const accountIds = [...($('#apiClientTextAccounts')?.selectedOptions || [])].map((option) => option.value);
  // An empty explicit member list is the legacy fixed-account mode. Clear
  // stale pool controls in the payload so removing the last member cannot
  // submit an invalid policy that the backend must reject.
  const poolStrategy = accountIds.length ? ($('#apiClientPoolStrategy')?.value || '') : '';
  const poolAccountId = accountIds.length ? ($('#apiClientPoolAccount')?.value || '') : '';
  if (poolStrategy === 'single_account' && !accountIds.includes(poolAccountId)) throw new Error('请从已选文字池中选择固定成员');
  return { accountId, modelPrefix, imageAccountIds, accountIds, poolStrategy, poolAccountId };
}
function orderedAPIClientPoolAccounts(accounts, selected) {
  // Multi-select serializes in DOM order; preserve the saved pool order even
  // when the shared vault has been sorted differently since the last edit.
  return [...selected.map((id) => accounts.find((row) => row.id === id) || { id, name: '已移除 · ' + id }),
    ...accounts.filter((row) => !selected.includes(row.id))];
}
let apiServiceView = 'upstreams';
let apiManagementLeavePending = null;
const apiSettingsDrafts = new Map();
function apiManagementShell(footer = '') {
  $('#officialPanel')?.closest?.('.modal')?.classList?.add?.('api-management-modal');
  const node = $('#officialPanelFooter');
  if (node) { node.innerHTML = footer; node.hidden = !footer; }
}
function apiClientDraftSnapshot() {
  return JSON.stringify(['Name','Account','Disabled','Concurrency','Allowlist','Denylist','Aliases','ModelPrefix','AllowPrefixes','DenyPrefixes','PoolStrategy','PoolAccount'].map(key => $('#apiClient' + key)?.value || '').concat(['TextAccounts','ImageAccounts'].map(key => Array.from($('#apiClient' + key)?.selectedOptions || [], row => row.value))));
}
function rememberAPISettings(id, submitted) {
  const section = $('#' + id);
  if (!section) return;
  for (const node of section.querySelectorAll?.('input,select') || []) apiSettingsDrafts.set(node, submitted?.[node.id] ?? node.value);
}
function apiManagementHasDraft() {
  if (currentAPIClientPanel(apiClientPanel) && apiClientPanel.mode === 'editor') return apiClientPanel.initialDraft !== apiClientDraftSnapshot();
  return Array.from(apiSettingsDrafts).some(([node, initial]) => node.isConnected && node.value !== initial);
}
function apiManagementActive() {
  return currentAPIClientPanel(apiClientPanel) && ['service', 'editor'].includes(apiClientPanel.mode);
}
async function apiManagementLeave(next) {
  if (!apiManagementActive()) { await next(); return true; }
  if (apiClientWritePending || CODEX_API_OVERVIEW.pending || apiClientPanel.settingsWriting) { toast('正在保存，请稍候再离开'); return false; }
  if (apiManagementLeavePending) return apiManagementLeavePending.promise;
  if (apiManagementHasDraft()) {
    let resolve;
    const promise = new Promise(done => { resolve = done; });
    apiManagementLeavePending = { promise, resolve, next };
    $('#officialPanel').insertAdjacentHTML('afterbegin', '<div id="apiUnsaved" class="api-unsaved" role="alert"><b>有未保存的更改</b><p>继续编辑，或放弃本次更改后离开。</p><div class="row"><button class="btn primary" data-act="apiManagementStay">继续编辑</button><button class="btn" data-act="apiManagementDiscard">放弃更改</button></div></div>');
    $('[data-act="apiManagementStay"]')?.focus?.();
    return promise;
  }
  await next(); return true;
}
function closeAPIManagement() { return apiManagementLeave(() => closeOfficialPanel()); }
function selectAPIServiceView(view, focus = false) {
  if (!['upstreams', 'clients', 'settings'].includes(view)) view = 'upstreams';
  apiServiceView = view;
  for (const id of ['upstreams', 'clients', 'settings']) {
    const pane = $('#apiService-' + id), button = $('#apiServiceTab-' + id);
    if (pane) pane.hidden = id !== view;
    if (button) { button.setAttribute('aria-selected', String(id === view)); button.tabIndex = id === view ? 0 : -1; }
  }
  if (focus) $('#apiServiceTab-' + view)?.focus?.();
}
function apiServiceUpstreamsHTML(status, accounts) {
  const members = new Map((status.summary?.members || []).map(row => [row.id, row]));
  const joined = account => (status.clients || []).filter(row => ['accountIds','imageAccountIds'].some(field => (row[field]?.length ? row[field] : [row.accountId]).includes(account.id)));
  return accounts.filter(account => joined(account).length).map(account => {
    const member = members.get(account.id);
    const known = member && !CODEX_API_OVERVIEW.error && member.availability === 'ready' && member.sourceType === 'official_account' && apiServiceUsageFresh(member.usage, Date.now());
    const unavailable = member?.availability === 'unavailable';
    const label = unavailable ? '暂不可用' : known ? '已验证' : '未检查';
    const clients = joined(account);
    return `<article class="api-upstream-row" data-observation-key="${esc(account.id)}"><div><div class="api-client-title"><b>${esc(account.name || '未命名 API')}</b><span class="account-state ${unavailable ? 'warn' : known ? 'ok' : 'muted'}">${label}</span></div><p class="muted small">${esc(account.officialAccount ? 'Codex 官方账号' : account.baseUrl || 'API 地址未配置')}</p><small>${esc(clients.map(row => row.name + (row.disabled ? '（停用）' : '')).join('、'))}</small></div><div class="api-upstream-actions">${account.officialAccount ? '' : `<button class="btn sm" data-act="apiUpstreamEdit" data-id="${esc(account.id)}">编辑 API</button>`}<button class="btn sm" data-act="apiCardMembership" data-id="${esc(account.id)}">管理加入范围</button></div></article>`;
  }).join('') || '<div class="api-service-empty"><b>还没有加入 API</b><p>回到 API 卡片点击「加入 API 服务」，这里会自动显示已加入的成员。</p></div>';
}
async function openAPIUpstream(id = '') {
  return apiManagementLeave(async () => {
    const owner = apiClientPanel;
    const row = id ? owner?.accounts.find(value => value.id === id) : null;
    if (id && !row) throw new Error('上游已变更，请刷新后重试');
    localEditor(row, undefined, { onReturn: () => openCodexAPI('upstreams'), service: true });
  });
}
Object.assign(ACTIONS, {
  apiServiceChoose: () => apiManagementLeave(() => { closeOfficialPanel(); if (S.route !== 'accounts') location.hash = '#accounts'; else $('#localAccountList')?.scrollIntoView?.({ block: 'start', behavior: 'smooth' }); }),
  apiServiceTab: button => selectAPIServiceView(button.dataset.view),
  apiUpstreamNew: () => openAPIUpstream(),
  apiUpstreamEdit: button => openAPIUpstream(button.dataset.id),
  apiUpstreamUse: button => apiManagementLeave(() => openCodexAPIClient('', button.dataset.id)),
  codexAPIEditorRetry: button => openCodexAPIClient(button.dataset.id, button.dataset.account),
  apiManagementBack: () => apiManagementLeave(() => openCodexAPI('clients')),
  apiManagementStay: () => { const pending = apiManagementLeavePending; apiManagementLeavePending = null; $('#apiUnsaved')?.remove(); pending?.resolve(false); },
  apiManagementDiscard: async () => { const pending = apiManagementLeavePending; apiManagementLeavePending = null; $('#apiUnsaved')?.remove(); if (pending) { try { await pending.next(); pending.resolve(true); } catch (error) { pending.resolve(false); throw error; } } },
});
document.addEventListener('keydown', event => {
  if (!event.target?.id?.startsWith('apiServiceTab-') || !['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(event.key)) return;
  event.preventDefault();
  const tabs = ['upstreams', 'clients', 'settings'], index = tabs.indexOf(apiServiceView);
  selectAPIServiceView(event.key === 'Home' ? tabs[0] : event.key === 'End' ? tabs[2] : tabs[(index + (event.key === 'ArrowRight' ? 1 : 2)) % 3], true);
});
function syncAPIClientDraft() {
  if (!currentAPIClientPanel(apiClientPanel) || apiClientPanel.mode !== 'editor') return;
  const dirty = apiManagementHasDraft();
  if ($('#apiClientDraftNote')) $('#apiClientDraftNote').textContent = apiClientPanel.writeAttempted ? '结果待核对，输入已保留' : dirty ? '有未保存的更改' : '保存后生效';
}
document.addEventListener('input', syncAPIClientDraft);
document.addEventListener('change', syncAPIClientDraft);
function renderClientImageAccounts(accounts, selected = []) {
  accounts = orderedAPIClientPoolAccounts(accounts, selected);
  $('#apiClientPools').insertAdjacentHTML('beforeend', `<details class="api-pool-options"><summary>图片账号池 · ${selected.length ? `已选 ${selected.length} 个` : '可选'}</summary>${apiClientPoolChoicesHTML('apiClientImageAccounts', accounts, selected)}<p class="muted small">留空使用默认上游。选中后，图片请求使用当前图片任务最少的成员。</p></details>`);
}
function renderClientTextPool(accounts, selected = [], strategy = '', poolAccountId = '') {
  accounts = orderedAPIClientPoolAccounts(accounts, selected);
  if (poolAccountId && !accounts.some((row) => row.id === poolAccountId)) accounts = [...accounts, { id: poolAccountId, name: '已移除 · ' + poolAccountId }];
  $('#apiClientPools').insertAdjacentHTML('afterbegin', `<details class="api-pool-options"><summary>文字账号池 · ${selected.length ? `已选 ${selected.length} 个` : '可选'}</summary>${apiClientPoolChoicesHTML('apiClientTextAccounts', accounts, selected)}<p class="muted small">留空使用默认上游；选中后只在这些成员中调度，默认上游不会自动加入。</p><div class="form-grid"><div class="field"><label for="apiClientPoolStrategy">文字池策略</label><select id="apiClientPoolStrategy" class="input">${[['','自动'],['random','随机'],['single_account','固定成员'],['quota_high_first','额度高优先'],['quota_low_first','额度低优先'],['plan_high_first','套餐高优先'],['plan_low_first','套餐低优先'],['expiry_soon_first','订阅早到期优先'],['custom','自定义优先级']].map(([key,name]) => `<option value="${key}" ${(strategy === key || key === '' && strategy === 'auto') ? 'selected' : ''}>${name}</option>`).join('')}</select></div><div class="field"><label for="apiClientPoolAccount">固定成员（仅固定成员策略）</label><select id="apiClientPoolAccount" class="input"><option value="">请选择</option>${accounts.map((row) => `<option value="${esc(row.id)}" ${poolAccountId === row.id ? 'selected' : ''}>${esc(row.name)}</option>`).join('')}</select></div></div></details>`);
}
function apiClientPoolChoicesHTML(id, accounts, selected) {
  return `<select id="${id}" data-native multiple hidden aria-hidden="true">${accounts.map(row => `<option value="${esc(row.id)}" ${selected.includes(row.id) ? 'selected' : ''}>${esc(row.name)}</option>`).join('')}</select><div class="api-pool-choices" role="group" aria-label="${id.includes('Text') ? '文字' : '图片'}账号池">${accounts.map(row => `<label><input type="checkbox" data-api-pool="${id}" value="${esc(row.id)}" ${selected.includes(row.id) ? 'checked' : ''}><span>${esc(row.name)}</span></label>`).join('')}</div>`;
}
document.addEventListener('change', event => {
  const id = event.target?.dataset?.apiPool;
  if (!['apiClientTextAccounts','apiClientImageAccounts'].includes(id)) return;
  const select = $('#' + id);
  for (const option of select?.options || []) if (option.value === event.target.value) option.selected = event.target.checked;
  const detail = select?.closest?.('details'), count = select?.selectedOptions?.length || 0;
  if (detail?.querySelector('summary')) detail.querySelector('summary').textContent = `${id.includes('Text') ? '文字' : '图片'}账号池 · ${count ? `已选 ${count} 个` : '可选'}`;
  syncAPIClientDraft();
});
async function openCodexAPI(view = apiServiceView) {
  if (typeof view !== 'string') view = 'upstreams';
  officialPanel('本地 API 服务', '<div id="apiServiceContent" class="stack"><p class="muted">正在读取本机配置…</p></div>');
  apiManagementShell();
  const epoch = OFFICIAL_PANEL.epoch;
  CODEX_API_OVERVIEW.revision++;
  let status, local;
  try { [status, local] = await Promise.all([api('/api/local/codex-api'), api('/api/local/accounts')]); }
  catch (error) {
    if (epoch === OFFICIAL_PANEL.epoch && $('#apiServiceContent')) $('#apiServiceContent').innerHTML = `<div class="api-service-empty"><b>本机配置读取失败</b><p>${esc(error.message)}</p><button class="btn" data-act="codexAPI">重新读取</button></div>`;
    return;
  }
  if (epoch !== OFFICIAL_PANEL.epoch || !$('#apiServiceContent')) return;
  // Enabled is durable user intent; the runtime lease is deliberately not.
  // After a Bridge restart the service is therefore disabled until the user
  // explicitly arms this process again. Keep that recovery action visible
  // instead of offering a misleading "关闭" button that would clear intent.
  CODEX_API_OVERVIEW.updatedAt = Date.now();
  CODEX_API_OVERVIEW.status = status; CODEX_API_OVERVIEW.error = false;
  const accounts = apiClientAccounts(local), clients = status.clients || [];
  apiClientPanel = { epoch, element: $('#officialPanel'), mode: 'service', clients, accounts, firstClient: !clients.length, writeAttempted: false };
  $('#apiServiceContent').innerHTML = `<div class="api-management-summary"><div id="apiServiceRuntime">${apiServiceRuntimeHTML(status)}</div><div class="api-endpoint"><span class="muted small">本机接口</span><code>${esc(status.baseUrl)}</code><button class="btn sm" data-act="copyVal" data-v="${esc(status.baseUrl)}" data-w="接口地址">复制地址</button></div></div>
    <div class="api-management-tabs" role="tablist" aria-label="本地 API 管理">${[['upstreams','已加入 API'],['clients','连接工具'],['settings','高级设置']].map(([id,label]) => `<button id="apiServiceTab-${id}" role="tab" aria-controls="apiService-${id}" data-act="apiServiceTab" data-view="${id}">${label}</button>`).join('')}</div>
    <section id="apiService-upstreams" role="tabpanel" aria-labelledby="apiServiceTab-upstreams"><div class="api-section-heading"><div><h4>已加入的 API</h4><p class="muted small">加入时已完成选择，无需再选一次。未检查表示尚未验证可用性。</p></div><button class="btn" data-act="apiServiceChoose">返回 API 卡片</button></div><div id="apiServiceUpstreams">${apiServiceUpstreamsHTML(status, accounts)}</div><details class="api-pool-options"><summary>额度与状态</summary><div id="apiServiceSummary">${apiServicePlanGroupsHTML(apiServiceSummary(status))}</div></details></section>
    <section id="apiService-clients" role="tabpanel" aria-labelledby="apiServiceTab-clients" hidden><section class="api-management-section"><h4>在本机工具中使用</h4><p class="muted small">使用已加入的 API，自动为工具配置连接。</p><div id="apiServiceLaunchActions">${localAPIServiceLaunchHTML(status)}</div></section><div id="apiServiceConnect">${typeof apiConnectionHTML === 'function' ? apiConnectionHTML(status) : ''}</div><div class="api-service-toolbar"><button class="btn sm" data-act="codexAPIReadiness">检查就绪</button><button class="btn sm" data-act="codexAPIRequests">全部请求记录</button></div></section>
    <section id="apiService-settings" role="tabpanel" aria-labelledby="apiServiceTab-settings" hidden><details class="api-pool-options"><summary>独立密钥、权限与调度</summary><p class="muted small">本机工具使用默认服务即可；需要不同模型权限或独立密钥时再添加接入配置。</p><div id="apiServiceClients" class="api-service-clients">${apiServiceClientRows(status, accounts)}</div><button class="btn sm" data-act="codexAPINew" ${clients.length ? '' : 'disabled'}>添加独立接入配置</button></details><details class="api-pool-options"><summary>仅写入 Codex 配置，不打开工具</summary><button class="btn sm" data-act="codexAPIProfile">管理 Codex 配置文件</button></details><div class="api-section-heading"><div><h4>网络与图片</h4><p class="muted small">分别保存，切换分区保留输入。</p></div></div><div id="apiServiceNetworkSummary" class="muted small" role="status"></div><div id="apiServiceAdvanced" class="stack"><p class="muted small">仅影响这台电脑提供的 API 服务。</p></div></section>`;
  selectAPIServiceView(view);
  syncAPIClientWriteControls();
  if (typeof scheduleAPIServiceObservation === 'function') scheduleAPIServiceObservation();
}
async function openCodexAPIClient(id, accountId = '') {
  officialPanel(id ? '编辑客户端' : '添加客户端', '<div id="apiClientEditor" class="stack"><p class="muted">正在读取配置…</p></div>');
  apiManagementShell('<button class="btn" data-act="apiManagementBack">返回客户端</button>');
  const epoch = OFFICIAL_PANEL.epoch;
  let status, local;
  try { [status, local] = await Promise.all([api('/api/local/codex-api'), api('/api/local/accounts')]); }
  catch (error) {
    if (epoch === OFFICIAL_PANEL.epoch && $('#apiClientEditor')) $('#apiClientEditor').innerHTML = `<div class="api-service-empty"><b>客户端配置读取失败</b><p>${esc(error.message)}</p><button class="btn" data-act="codexAPIEditorRetry" data-id="${esc(id)}" data-account="${esc(accountId)}">重新读取</button></div>`;
    return;
  }
  if (epoch !== OFFICIAL_PANEL.epoch || !$('#apiClientEditor')) return;
  const row = id ? status.clients.find((client) => client.id === id) : { id: '', name: '本机客户端', accountId };
  if (!row) throw new Error('客户端已被移除');
  const accounts = apiClientAccounts(local);
  const accountMissing = !accounts.some((account) => account.id === row.accountId);
  apiClientPanel = { epoch, element: $('#officialPanel'), mode: 'editor', clients: status.clients, clientId: id, accounts, writeAttempted: false };
  const prefixFields = `<div class="field"><label for="apiClientModelPrefix">模型命名空间（可选，例如 team/）</label><input id="apiClientModelPrefix" class="input" maxlength="64" value="${esc(row.modelPrefix || '')}" placeholder="team/"><div class="muted small">别名不含此前缀；权限始终按解析后的实际上游模型检查。</div></div><div class="field"><label>允许模型前缀（每行一个文字前缀，与允许模型列表合并；两者留空表示不限）</label><textarea id="apiClientAllowPrefixes" class="input">${esc((row.modelAllowPrefixes || []).join('\n'))}</textarea></div><div class="field"><label>拒绝模型前缀（按实际上游模型 ID 检查，优先于所有允许规则）</label><textarea id="apiClientDenyPrefixes" class="input">${esc((row.modelDenyPrefixes || []).join('\n'))}</textarea></div>`;
  $('#apiClientEditor').innerHTML = `<p class="muted small">为一个工具或使用场景建立独立接入配置。所有选择在保存后生效。</p><section class="api-management-section"><div class="form-grid"><div class="field"><label for="apiClientName">客户端名称</label><input id="apiClientName" class="input" maxlength="80" value="${esc(row.name)}" placeholder="例如：开发工具、图片工作台"></div><div class="field"><label for="apiClientDisabled">客户端状态</label><select id="apiClientDisabled" class="input"><option value="false" ${row.disabled ? '' : 'selected'}>启用</option><option value="true" ${row.disabled ? 'selected' : ''}>停用</option></select></div></div></section>
    <section class="api-management-section"><h4>成员与调度</h4><div class="field"><label for="apiClientAccount">主账号（未配置账号池时使用）</label><select id="apiClientAccount" class="input">${accountMissing ? `<option value="" selected disabled>${id ? '原账号已移除，请重新选择' : '选择 API / 账号'}</option>` : ''}${accounts.map(a => `<option value="${esc(a.id)}" ${a.id === row.accountId ? 'selected' : ''}>${esc(a.name)}</option>`).join('')}</select><p class="help">未配置账号池时，文字和图片都使用这个上游。</p></div><div id="apiClientPools"></div></section>
    <details class="api-pool-options"><summary>模型权限与并发</summary><div class="stack"><div class="field"><label for="apiClientConcurrency">并发上限</label><input id="apiClientConcurrency" class="input" type="number" min="1" max="128" value="${row.maxConcurrency || 16}"></div><div class="field"><label for="apiClientAllowlist">允许模型（每行一个，留空不限）</label><textarea id="apiClientAllowlist" class="input" rows="3">${esc((row.modelAllowlist || []).join('\n'))}</textarea></div><div class="field"><label for="apiClientDenylist">拒绝模型（优先于允许规则）</label><textarea id="apiClientDenylist" class="input" rows="3">${esc((row.modelDenylist || []).join('\n'))}</textarea></div><div class="field"><label for="apiClientAliases">模型别名（每行：别名 = 模型 ID）</label><textarea id="apiClientAliases" class="input" rows="3">${esc(Object.entries(row.modelAliases || {}).map(([alias, model]) => `${alias} = ${model}`).join('\n'))}</textarea><p class="help">权限按解析后的实际上游模型检查。</p></div></div></details>`;
  $('#apiClientAliases').closest('.field').insertAdjacentHTML('beforebegin', prefixFields);
  if (id) $('#apiClientEditor').insertAdjacentHTML('afterbegin', `<div class="row"><span class="muted small">${esc(row.name)} · 客户端配置</span><span class="spacer"></span><button class="icon-btn" data-act="codexAPIRequests" data-id="${esc(id)}" title="查看最近请求（保留编辑内容）" aria-label="查看最近请求并保留编辑内容">${icon('list')}</button></div>`);
  renderClientImageAccounts(accounts, row.imageAccountIds || []);
  renderClientTextPool(accounts, row.accountIds || [], row.poolStrategy || '', row.poolAccountId || '');
  // Keep one footer and one scrolling body; actions no longer scroll away.
  apiManagementShell(`<span id="apiClientDraftNote" class="muted small" role="status">保存后生效</span><span class="spacer"></span><button class="btn" data-act="apiManagementBack">取消</button><button class="btn primary" data-act="codexAPISave" data-id="${esc(id)}" ${accounts.length ? '' : 'disabled'}>${id ? '保存更改' : '创建客户端'}</button>`);
  apiClientPanel.initialDraft = apiClientDraftSnapshot();
  syncAPIClientWriteControls();
}
document.addEventListener('change', async (event) => {
  if (event.target.dataset?.cleanupId) {
    const id = event.target.dataset.cleanupId;
    if (OFFICIAL_PANEL.cleanupPending || !OFFICIAL_PANEL.cleanup?.some(row => row.id === id)
        || event.target.closest?.('#officialCleanupState') !== $('#officialCleanupState')) return;
    if (event.target.checked) OFFICIAL_PANEL.cleanupSelection.add(id);
    else OFFICIAL_PANEL.cleanupSelection.delete(id);
    syncOfficialCleanupSelection();
    return;
  }
  if (event.target.id === 'apiServiceLaunchClient') {
    const owner = apiServiceLaunchPanel;
    if (!apiServiceLaunchCurrent(owner) || owner.submitted || apiServiceLaunchBusy()) return;
    owner.clientId = event.target.value;
    owner.preview = null;
    await readAPIServiceLaunchPreview(owner);
    return;
  }
  if (event.target.id !== 'officialFile') return;
  const epoch = OFFICIAL_PANEL.epoch, revision = ++OFFICIAL_PANEL.fileRevision, files = Array.from(event.target.files || []);
  if (!files.length) return;
  const replaceId = OFFICIAL_PANEL.replaceId;
  const format = $('#officialImportFormat')?.value || 'json';
  OFFICIAL_PANEL.credentials = null;
  OFFICIAL_PANEL.files = null;
  resetOfficialImportReview();
  if ($('#officialImportPreview')) $('#officialImportPreview').innerHTML = '';
  event.target.value = '';
  try {
    if (files.length > 64 || files.reduce((total, file) => total + file.size, 0) > 1024 * 1024) throw new Error('最多选择 64 个文件，总计不超过 1 MiB');
    // Send exact bytes as a string; JSON.parse/stringify here would erase
    // duplicate or case-confusable fields before the backend can reject them.
    const contents = await Promise.all(files.map((file) => file.text()));
    if (epoch !== OFFICIAL_PANEL.epoch || revision !== OFFICIAL_PANEL.fileRevision) return;
    if (contents.reduce((total, value) => total + new TextEncoder().encode(value).length, 0) > 1024 * 1024) throw new Error('文件内容超过 1 MiB');
    const preview = await api('/api/local/codex/import/preview', { files: contents, format });
    if (epoch !== OFFICIAL_PANEL.epoch || revision !== OFFICIAL_PANEL.fileRevision || !$('#officialImportPreview')) return;
    if (replaceId && preview.accounts.length !== 1) throw new Error('替换导入只能选择一个账号');
    OFFICIAL_PANEL.files = contents;
    OFFICIAL_PANEL.importFormat = format;
    setOfficialImportPreview(preview);
    if ($('#officialError')) $('#officialError').textContent = '';
  } catch (error) { if (epoch !== OFFICIAL_PANEL.epoch || revision !== OFFICIAL_PANEL.fileRevision) return; OFFICIAL_PANEL.credentials = null; OFFICIAL_PANEL.files = null; if ($('#officialError')) $('#officialError').textContent = error.message || '导入文件无效'; }
});
function invalidateOfficialPaste(event) {
  if (!['officialPasteText', 'officialImportFormat'].includes(event.target?.id)) return;
  OFFICIAL_PANEL.fileRevision++;
  OFFICIAL_PANEL.credentials = null;
  OFFICIAL_PANEL.files = null;
  resetOfficialImportReview();
  if ($('#officialImportPreview')) $('#officialImportPreview').innerHTML = '';
}
document.addEventListener('input', invalidateOfficialPaste);
document.addEventListener('change', (event) => {
  const raw = event.target?.dataset?.importIndex;
  if (raw == null || !OFFICIAL_PANEL.importPreview || !OFFICIAL_PANEL.importSelection || !/^\d+$/.test(raw)) return;
  const index = Number(raw);
  if (index >= OFFICIAL_PANEL.importPreview.accounts.length) return;
  if (event.target.checked) OFFICIAL_PANEL.importSelection.add(index); else OFFICIAL_PANEL.importSelection.delete(index);
  renderOfficialImportReview();
});
document.addEventListener('change', invalidateOfficialPaste);
new MutationObserver(() => { if (!$('#officialPanel')) disposeOfficialPanel(); }).observe($('#modalRoot'), { childList: true });
document.addEventListener('visibilitychange', () => syncOfficialAccountCache(true));
const officialAccountCacheView = $('#view');
if (officialAccountCacheView) new MutationObserver(() => syncOfficialAccountCache()).observe(officialAccountCacheView, { childList: true });
