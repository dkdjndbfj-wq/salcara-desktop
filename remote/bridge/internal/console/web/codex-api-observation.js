'use strict';

const API_SERVICE_WATCH = { timer: 0, revision: 0, pending: false };
let apiRequestsPanel = null;
function apiServiceObservationVisible() {
  if (document.hidden) return false;
  if (currentAPIClientPanel(apiClientPanel) && apiClientPanel.mode === 'service') return true;
  return S.route === 'accounts' && !!$('#localAPIService') && !$('#officialPanel');
}
function stopAPIServiceObservation() {
  clearTimeout(API_SERVICE_WATCH.timer);
  API_SERVICE_WATCH.timer = 0;
  API_SERVICE_WATCH.revision++;
}
function scheduleAPIServiceObservation() {
  if (API_SERVICE_WATCH.timer || API_SERVICE_WATCH.pending || !apiServiceObservationVisible()) return;
  API_SERVICE_WATCH.timer = setTimeout(async () => {
    API_SERVICE_WATCH.timer = 0;
    if (!apiServiceObservationVisible()) return;
    API_SERVICE_WATCH.pending = true;
    try {
      if (apiClientWritePending || CODEX_API_OVERVIEW.pending) return;
      await refreshAPIServiceObservation();
    } catch (_) {
      if (apiServiceObservationVisible() && $('#apiServiceUpdated')) $('#apiServiceUpdated').textContent = `状态读取失败 · ${apiServiceUpdatedLabel()}`;
    } finally {
      API_SERVICE_WATCH.pending = false;
      scheduleAPIServiceObservation();
    }
  }, 5000);
}
async function refreshAPIServiceObservation() {
  const owner = apiClientPanel;
  const revision = CODEX_API_OVERVIEW.revision + 1;
  await loadCodexAPIOverview();
  if (revision !== CODEX_API_OVERVIEW.revision || !currentAPIClientPanel(owner) || owner.mode !== 'service') return;
  if (!CODEX_API_OVERVIEW.error) updateAPIServiceReadOnly(CODEX_API_OVERVIEW.status, owner);
  else if ($('#apiServiceUpdated')) $('#apiServiceUpdated').textContent = `状态读取失败 · ${apiServiceUpdatedLabel()}`;
  await Promise.allSettled([
    typeof refreshAPINetworkObservation === 'function' ? refreshAPINetworkObservation() : Promise.resolve(),
    typeof refreshAPIImageObservation === 'function' ? refreshAPIImageObservation() : Promise.resolve(),
  ]);
}
document.addEventListener('visibilitychange', () => {
  if (document.hidden) stopAPIServiceObservation();
  else scheduleAPIServiceObservation();
});

function apiRequestState(value) {
  return ({ completed: '上游完成', incomplete: '未完成', failed: '失败', interrupted: '中断', cancelled: '已取消', rejected: '已拒绝', read_only: '只读', unknown: '结果未知' })[value] || '未记录';
}
function apiRequestDuration(value) { return Number.isFinite(value) && value >= 0 ? `${value} ms` : '未记录'; }
function apiRoutingReason(value) { return ({ fixed_account: '固定账号', pool_selected: '账号池选择', session_affinity: '沿用会话账号', failover: '备用账号接替' })[value] || '未记录'; }
function apiFailureStage(value) { return ({ selection: '选择账号', authorization: '本机权限校验', connect: '连接上游', headers: '等待首个响应数据', upstream: '上游处理', stream: '响应传输', cancelled: '请求取消' })[value] || '未记录'; }
function apiAttemptReason(value) { return ({ rate_limited: '上游限流', unavailable: '上游暂不可用', auth_rejected: '上游认证被拒绝', model_unavailable: '模型暂不可用', connect_failed: '连接上游失败', headers_timeout: '首个响应数据超时', stream_idle_timeout: '响应长时间无数据', stream_interrupted: '响应流中断', cancelled: '请求已取消', unknown_result: '结果未知', fixed_account: '使用固定账号', pool_selected: '账号池选择', session_affinity: '沿用会话账号', failover: '备用账号接替' })[value] || '未记录'; }
function apiRequestsCurrent(owner) { return apiRequestsPanel === owner && owner?.element?.isConnected && $('#apiRequestsOverlay') === owner.element; }
function apiRequestRowsHTML(result) {
  const account = id => result.accountNames?.[id] || '历史 / 未命名账号';
  const client = id => result.clientNames?.[id] || '历史 / 未命名客户端';
  return (result.records || []).map(row => {
    const attempts = Array.isArray(row.attempts) ? row.attempts.slice(0, 3) : [];
    const summary = `${apiRequestState(row.state)} · HTTP ${row.httpStatus || '未记录'} · ${apiRequestDuration(row.durationMs)}`;
    return `<details class="api-request-row" data-record="${esc(row.id || '')}"><summary><div><b>${esc(row.model || '模型未记录')}</b><small>${esc(client(row.clientId))} → ${esc(account(row.accountId))}</small></div><div><span>${esc(summary)}</span><small>${row.time ? esc(new Date(row.time).toLocaleString()) : '时间未记录'}</small></div></summary><div class="api-request-detail"><dl><div><dt title="请求开始到首个响应数据的耗时">首个响应数据</dt><dd>${esc(apiRequestDuration(row.firstByteMs))}</dd></div><div><dt>选择原因</dt><dd>${apiRoutingReason(row.routingReason)}</dd></div><div><dt>失败阶段</dt><dd>${apiFailureStage(row.failureStage)}</dd></div></dl>${attempts.length ? `<ol class="api-request-attempts">${attempts.map(item => `<li><b>${esc(account(item.accountId))}</b><span>${esc(item.model || '模型未记录')} · ${esc(apiRequestState(item.state))} · HTTP ${item.upstreamHttpStatus || item.httpStatus || '未记录'}</span><small>${apiAttemptReason(item.reason)} · 耗时 ${esc(apiRequestDuration(item.durationMs))} · 首个响应数据 ${esc(apiRequestDuration(item.firstByteMs))}${Number.isFinite(item.retryAfterMs) && item.retryAfterMs > 0 ? ` · 建议等待 ${item.retryAfterMs} ms` : ''}</small></li>`).join('')}</ol>` : '<p class="muted small">此记录没有分次尝试明细。</p>'}</div></details>`;
  }).join('') || '<p class="api-service-empty">当前范围还没有本机请求记录。</p>';
}
async function loadAPIRequests(owner = apiRequestsPanel) {
  if (!apiRequestsCurrent(owner) || owner.pending) return;
  owner.pending = true;
  const sequence = ++owner.sequence;
  const status = $('#apiRequestsStatus'), button = $('#apiRequestsRefresh');
  if (status) status.textContent = '正在读取本机记录…';
  if (button) button.disabled = true;
  try {
    const query = new URLSearchParams({ limit: '30' });
    if (owner.clientId) query.set('clientId', owner.clientId);
    const result = await api('/api/local/codex-api/requests?' + query);
    if (!apiRequestsCurrent(owner) || sequence !== owner.sequence) return;
    if (!Array.isArray(result?.records)) throw new Error('请求记录响应不完整');
    const open = new Set(Array.from(owner.element.querySelectorAll?.('details[open]') || [], node => node.dataset.record));
    $('#apiRequestsList').innerHTML = apiRequestRowsHTML(result);
    for (const node of owner.element.querySelectorAll?.('details[data-record]') || []) node.open = open.has(node.dataset.record);
    const issues = [result.status?.readError, result.status?.saveError, result.status?.unsaved ? '含尚未保存的记录' : '', result.status?.dropped ? '记录不完整，曾丢弃部分记录' : ''].filter(Boolean);
    status.textContent = `${result.records.length} 条最近记录 · ${result.checkedAt ? apiServiceUpdatedLabel(result.checkedAt) : '更新时间未记录'}${issues.length ? ' · ' + issues.join('；') : ''}`;
  } catch (error) {
    if (apiRequestsCurrent(owner) && sequence === owner.sequence) status.textContent = `读取失败：${error.message}。可点刷新重试。`;
  } finally {
    owner.pending = false;
    if (apiRequestsCurrent(owner) && sequence === owner.sequence && button) button.disabled = false;
  }
}
function closeAPIRequests(restoreFocus = true) {
  const owner = apiRequestsPanel;
  if (!owner) return;
  apiRequestsPanel = null;
  owner.sequence++;
  owner.element.remove();
  if (restoreFocus && owner.previousFocus?.isConnected) owner.previousFocus.focus?.();
}
async function openAPIRequests(button) {
  closeAPIRequests(false);
  const clientId = button?.dataset?.id || '';
  const previousFocus = document.activeElement;
  const name = (apiClientPanel?.clients || CODEX_API_OVERVIEW.status?.clients || []).find(row => row.id === clientId)?.name;
  document.body.insertAdjacentHTML('beforeend', `<div id="apiRequestsOverlay" class="modal-back api-requests-overlay"><section class="modal api-requests-modal" role="dialog" aria-modal="true" aria-labelledby="apiRequestsTitle" data-stop><div class="modal-head"><h3 id="apiRequestsTitle">${esc(name ? name + ' · 最近请求' : '本地 API · 最近请求')}</h3><button class="icon-btn" data-act="codexAPIRequestsClose" title="关闭请求记录" aria-label="关闭请求记录">${icon('close')}</button></div><div class="modal-body"><div class="row"><span id="apiRequestsStatus" class="muted small" role="status"></span><span class="spacer"></span><button id="apiRequestsRefresh" class="icon-btn" data-act="codexAPIRequestsRefresh" title="刷新请求记录" aria-label="刷新请求记录">${icon('refresh')}</button></div><p class="muted small">显示本机已记录的实际账号与请求结果，不包含提示词、正文或密钥。</p><div id="apiRequestsList"></div></div></section></div>`);
  const owner = apiRequestsPanel = { element: $('#apiRequestsOverlay'), clientId, previousFocus, sequence: 0, pending: false };
  owner.element.querySelector?.('[data-act="codexAPIRequestsClose"]')?.focus?.();
  await loadAPIRequests(owner);
}
document.addEventListener('click', event => { if (apiRequestsCurrent(apiRequestsPanel) && event.target === apiRequestsPanel.element) closeAPIRequests(); });
document.addEventListener('keydown', event => {
  if (!apiRequestsCurrent(apiRequestsPanel)) return;
  if (event.key === 'Escape') { event.preventDefault(); event.stopImmediatePropagation(); closeAPIRequests(); }
  else if (event.key === 'Tab') {
    const targets = Array.from(apiRequestsPanel.element.querySelectorAll?.('button:not([disabled]), summary') || []);
    if (!targets.length) return;
    const first = targets[0], last = targets.at(-1);
    if (event.shiftKey && (document.activeElement === first || !apiRequestsPanel.element.contains?.(document.activeElement))) { event.preventDefault(); last.focus?.(); }
    else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first.focus?.(); }
  }
}, true);
Object.assign(ACTIONS, {
  codexAPIServiceRefresh: button => busy(button, refreshAPIServiceObservation),
  codexAPIRequests: openAPIRequests,
  codexAPIRequestsRefresh: () => loadAPIRequests(),
  codexAPIRequestsClose: (button, event) => { if (button.id === 'apiRequestsOverlay' && event?.target?.closest?.('[data-stop]')) return; closeAPIRequests(); },
  codexAPIClientEnabled: button => withAPIClientWrite(async () => {
    const owner = requireAPIClientPanel('service', button.dataset.id);
    const row = owner.clients.find(item => item.id === button.dataset.id);
    const enabled = button.dataset.enabled === 'true';
    CODEX_API_OVERVIEW.revision++;
    try {
      await busy(button, async () => {
        const result = await api('/api/local/codex-api/clients/enabled', { id: row.id, enabled, expectedRevision: row.revision });
        if (result?.ok !== true || result.id !== row.id || result.enabled !== enabled) throw new Error('操作结果未确认，请刷新后核对');
      });
      if (currentAPIClientPanel(owner) && $('#officialError')) $('#officialError').textContent = '';
    } catch (error) { if (currentAPIClientPanel(owner) && $('#officialError')) $('#officialError').textContent = error.message; }
    await refreshAPIServiceObservation();
  }),
});
