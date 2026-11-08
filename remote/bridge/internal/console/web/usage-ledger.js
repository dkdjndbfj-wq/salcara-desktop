'use strict';
// Local provider observations are separate from upstream billing/quota data.
const LOCAL_LEDGER = { rows: [], epoch: 0, status: null };
const originalUsageHead = usageHead;
usageHead = function () {
  originalUsageHead();
  ($('#headActions .us-controls') || $('#headActions'))?.insertAdjacentHTML('beforeend', '<button class="btn sm" data-act="usageLedger">本机请求记录</button>');
};

function ledgerToken(value) { return value == null ? '未知' : Number.isSafeInteger(value) && value >= 0 ? compact(value) : '未知'; }
function ledgerLabel(names, id, fallback) { const name = names?.[id]; return typeof name === 'string' && name.trim() ? name + ' · ' + id : id || fallback; }
function ledgerState(state) { return ({ completed: '上游完成', incomplete: '未完成', failed: '失败', interrupted: '中断', cancelled: '已取消', rejected: '已拒绝', read_only: '只读', local_estimate: '本地估算', unknown: '未知' })[state] || '未知'; }
function ledgerIsCurrent(epoch) { return OFFICIAL_PANEL.epoch === epoch && !!$('#usageLedgerContent'); }
function drawLocalLedger() {
  if (!ledgerIsCurrent(LOCAL_LEDGER.epoch)) return;
  const status = LOCAL_LEDGER.status;
  const issues = [status.readError, status.saveError, status.dropped ? `记录队列曾丢弃 ${status.dropped} 条；不能将记录当作完整账单。` : '', status.unsaved ? '含未保存记录，关闭前会尝试保存。' : ''].filter(Boolean);
  $('#usageLedgerContent').innerHTML = `<p class="small muted">仅记录本机工具网关和 Codex API 的请求结果及上游返回用量。未知用量保留为未知；不包含提示词、正文、Key，也不作为计费或账号额度。</p>${issues.length ? `<div class="card small" role="alert">${issues.map(esc).join('<br>')}</div>` : ''}<div class="row"><span class="chip">${status.count} 条 · 待处理 ${status.pending}</span><button class="btn sm" data-act="usageLedgerRefresh">刷新</button><button class="btn sm" data-act="usageLedgerRetry">重试保存</button><button class="btn sm ghost danger-text" data-act="usageLedgerClear">清空记录</button></div><div class="form-grid"><div class="field"><label for="ledgerRecords">最多保留条数</label><input class="input" id="ledgerRecords" type="number" min="1" max="4096" value="${status.retention.maxRecords}"></div><div class="field"><label for="ledgerDays">保留天数</label><input class="input" id="ledgerDays" type="number" min="1" max="365" value="${status.retention.days}"></div></div><button class="btn sm" data-act="usageLedgerRetention">保存保留设置</button><div class="table-wrap"><table class="table"><thead><tr><th>时间 / 模型</th><th>结果 / HTTP</th><th>输入 / 输出</th><th>合计 / 耗时</th></tr></thead><tbody>${LOCAL_LEDGER.rows.map((row) => `<tr><td>${esc(new Date(row.time).toLocaleString())}<br>${esc(row.model || '未知模型')}<br><span class="muted small">${esc(ledgerLabel(status.accountNames, row.accountId, '未知账号'))}<br>${esc(ledgerLabel(status.clientNames, row.clientId, row.routeId))}</span></td><td>${esc(ledgerState(row.state))}<br>${row.httpStatus || '未知'}${row.upstreamHttpStatus ? ` / 上游 ${row.upstreamHttpStatus}` : ''}${!row.usageFinal ? '<br><span class="muted small">非最终用量或未知</span>' : ''}</td><td>${esc(ledgerToken(row.tokens?.inputTokens))} / ${esc(ledgerToken(row.tokens?.outputTokens))}<br><span class="small muted">缓存 ${esc(ledgerToken(row.tokens?.cachedInputTokens))} · 建缓存 ${esc(ledgerToken(row.tokens?.cacheCreationInputTokens))} · 推理 ${esc(ledgerToken(row.tokens?.reasoningOutputTokens))}</span></td><td>${esc(ledgerToken(row.tokens?.totalTokens))}<br>${Number.isSafeInteger(row.durationMs) ? row.durationMs : 0} ms</td></tr>`).join('') || '<tr><td colspan="4" class="muted">暂无本机请求记录</td></tr>'}</tbody></table></div>${LOCAL_LEDGER.rows.length < status.count ? '<button class="btn sm" data-act="usageLedgerMore">读取更早记录</button>' : ''}`;
}
async function loadLocalLedger(more = false) {
  const epoch = LOCAL_LEDGER.epoch;
  const cursor = more && LOCAL_LEDGER.rows.length ? LOCAL_LEDGER.rows.at(-1).id : '';
  const status = await api(`/api/local/usage-ledger?limit=100${cursor ? '&before=' + encodeURIComponent(cursor) : ''}`);
  if (!ledgerIsCurrent(epoch)) return;
  LOCAL_LEDGER.rows = more ? [...LOCAL_LEDGER.rows, ...status.records] : status.records;
  LOCAL_LEDGER.status = status;
  drawLocalLedger();
}
async function openLocalLedger() {
  officialPanel('本机请求记录', '<div id="usageLedgerContent" aria-live="polite">正在读取…</div>');
  const epoch = OFFICIAL_PANEL.epoch;
  LOCAL_LEDGER.epoch = epoch;
  LOCAL_LEDGER.rows = [];
  try { await loadLocalLedger(); } catch (error) { if (ledgerIsCurrent(epoch)) $('#officialError').textContent = error.message; }
}
async function reloadLocalLedger(button, more = false) {
  const epoch = LOCAL_LEDGER.epoch;
  try { await busy(button, () => loadLocalLedger(more)); }
  catch (error) { if (ledgerIsCurrent(epoch)) $('#officialError').textContent = error.message; }
}
async function mutateLocalLedger(button, path, body) {
  const epoch = LOCAL_LEDGER.epoch;
  try { await busy(button, () => api(path, body)); if (ledgerIsCurrent(epoch)) { $('#officialError').textContent = ''; await loadLocalLedger(); } }
  catch (error) { if (ledgerIsCurrent(epoch)) { $('#officialError').textContent = error.message; try { await loadLocalLedger(); } catch (_) { /* Preserve the mutation error. */ } } }
}
Object.assign(ACTIONS, {
  usageLedger: openLocalLedger,
  usageLedgerRefresh: (button) => reloadLocalLedger(button),
  usageLedgerMore: (button) => reloadLocalLedger(button, true),
  usageLedgerRetry: (button) => mutateLocalLedger(button, '/api/local/usage-ledger/retry-save', {}),
  usageLedgerClear: async (button) => { if (await confirm('清空本机请求记录？新完成的请求仍会继续记录。')) return mutateLocalLedger(button, '/api/local/usage-ledger/clear', { confirmed: true }); },
  usageLedgerRetention: (button) => {
    const maxRecords = Number($('#ledgerRecords').value), days = Number($('#ledgerDays').value);
    if (!Number.isInteger(maxRecords) || maxRecords < 1 || maxRecords > 4096 || !Number.isInteger(days) || days < 1 || days > 365) { $('#officialError').textContent = '保留条数为 1–4096，保留天数为 1–365'; return; }
    return mutateLocalLedger(button, '/api/local/usage-ledger/retention', { maxRecords, days });
  },
});
