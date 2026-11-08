'use strict';
const ACCOUNT_DAILY = { plan: '', tag: '', bulk: false, selected: new Map(), library: [], visible: [], signature: '', panel: null, expiry: 0, refreshActive: 0, refreshWaiters: [] };
const ACCOUNT_DAILY_PLANS = { api: 'API Key', free: 'Free', go: 'Go', plus: 'Plus', pro: 'Pro', team: 'Team', business: 'Business', enterprise: 'Enterprise', edu: 'Edu', unknown: '套餐待确认' };
function accountActiveState(account, local = S.local) {
  if (!account?.id || account.libraryOnly || account.officialAccount?.mode === 'chatgpt_web_session' ||
      !local || ACCOUNT_DAILY.activityInvalidLocal === local) return null;
  const current = (local.accounts || []).find(row => row?.id === account.id);
  const identity = accountDailyIdentity(account);
  if (!identity || !current || accountDailyIdentity(current) !== identity) return null;
  const inflight = current.poolState?.inflight;
  if (Number.isSafeInteger(inflight) && inflight > 0) return { kind: 'request', label: '请求中', detail: `${inflight} 个在途请求` };
  // A selected account, an old appliedId, or pool membership is insufficient.
  // pending=false is the server's fingerprint/authorization check for the
  // current applied configuration; it does not assert a live tool process.
  const tools = (Array.isArray(local.tools) ? local.tools : []).filter(tool => {
    const binding = local.bindings?.[tool?.id];
    return tool?.available === true && binding?.pending === false && binding.accountId === account.id && binding.appliedId === account.id;
  });
  if (tools.length) return { kind: 'applied', label: '当前应用', detail: '当前应用于 ' + [...new Set(tools.map(tool => tool.name || tool.id))].join('、') };
  const overview = typeof CODEX_API_OVERVIEW === 'undefined' ? null : CODEX_API_OVERVIEW;
  const service = overview?.status;
  if (overview?.error || overview?.pending || overview?.activityLocal !== local || service?.enabled !== true || service.running !== true) return null;
  const clients = Array.isArray(service.clients) ? service.clients : [];
  const memberships = Array.isArray(current.apiService?.clients) ? current.apiService.clients : [];
  const routes = (client, field) => {
    const explicit = client[field];
    if (explicit !== undefined && !Array.isArray(explicit)) return [];
    const members = explicit?.length ? explicit : [client.accountId];
    if (members.some(id => typeof id !== 'string' || !id)) return [];
    const unique = [...new Set(members)];
    if (field === 'accountIds' && client.poolStrategy === 'single_account') return unique.includes(client.poolAccountId) ? [client.poolAccountId] : [];
    return unique.length === 1 ? unique : [];
  };
  const fixed = clients.filter(client => client?.disabled === false && typeof client.id === 'string' && client.id && memberships.some(member =>
    member?.clientId === client.id && member.disabled === false &&
    (member.text === true && routes(client, 'accountIds').includes(account.id) || member.image === true && routes(client, 'imageAccountIds').includes(account.id))));
  return fixed.length ? { kind: 'service', label: '服务已启用', detail: [...new Set(fixed.map(client => client.name || 'API 服务'))].join('、') + ' · 固定账号' } : null;
}
function accountActiveSync() {
  const root = $('#localAccountList');
  if (S.route !== 'accounts' || !root) return;
  const accounts = new Map((S.local?.accounts || []).filter(row => row?.id).map(row => [row.id, row]));
  for (const badge of root.querySelectorAll?.('[data-account-active]') || []) {
    const card = badge.closest('.account-card'), account = accounts.get(badge.dataset.accountActive);
    const state = account && badge.dataset.accountActiveIdentity === accountDailyIdentity(account) ? accountActiveState(account) : null;
    if (!card) continue;
    card.classList.toggle('account-is-active', !!state);
    if (state) card.dataset.accountActivity = state.kind;
    else delete card.dataset.accountActivity;
    badge.hidden = !state;
    badge.textContent = state?.label || '';
    badge.title = state?.detail || '';
    if (state) badge.setAttribute('aria-label', state.label + '：' + state.detail);
    else badge.removeAttribute('aria-label');
  }
}
function accountActiveUnavailable() {
  ACCOUNT_DAILY.activityInvalidLocal = S.local;
  accountActiveSync();
}
function accountDailyIdentity(account) {
  const info = account?.officialAccount;
  if (info) return typeof info.id === 'string' && info.id && Number.isSafeInteger(info.identityEpoch) && info.identityEpoch > 0
    ? JSON.stringify([account.id, info.id, info.identityEpoch, account.sourceRevision || '']) : '';
  return /^src-v1-[a-f0-9]{64}$/.test(account?.sourceRevision || '') ? JSON.stringify([account.id, account.sourceRevision]) : '';
}
function accountDailyMetadata(account) {
  const info = account?.officialAccount;
  const row = info ? S.local?.officialAccountTags?.[info.id] : account?.tagMetadata;
  if (!row || info && row.identityEpoch !== info.identityEpoch || !Array.isArray(row.tags) || !/^tags-v1-[a-f0-9]{64}$/.test(row.revision || '')) return null;
  return row;
}
function accountDailyPlan(account) {
  if (!account.officialAccount) return 'api';
  const usage = account.officialAccount.usage, plan = usage?.plan;
  return apiServiceUsageFresh(usage, Date.now()) && plan?.verified === true && plan.source === 'openai_codex_usage_api' && Object.hasOwn(ACCOUNT_DAILY_PLANS, plan.family) && plan.family !== 'api' ? plan.family : 'unknown';
}
function accountDailyFilter(library) {
  const q = String(S.localFilter || '').trim().toLowerCase();
  return library.filter(account => {
    const tags = accountDailyMetadata(account)?.tags || [];
    return (!q || [account.name, account.baseUrl, ...tags].some(value => String(value || '').toLowerCase().includes(q))) &&
      (!ACCOUNT_DAILY.plan || accountDailyPlan(account) === ACCOUNT_DAILY.plan) && (!ACCOUNT_DAILY.tag || tags.includes(ACCOUNT_DAILY.tag));
  });
}
function accountDailyPrepare(library, visible) {
  ACCOUNT_DAILY.library = library; ACCOUNT_DAILY.visible = visible;
  ACCOUNT_DAILY.signature = accountDailySignature(library);
  const allowed = new Map(visible.map(account => [account.id, accountDailyIdentity(account)]));
  for (const [id, identity] of ACCOUNT_DAILY.selected) {
    if (!identity || allowed.get(id) !== identity) ACCOUNT_DAILY.selected.delete(id);
  }
}
function accountDailySignature(library) {
  return JSON.stringify(library.map(account => [account.id, accountDailyIdentity(account), accountDailyMetadata(account)?.revision || '', ACCOUNT_DAILY.plan ? accountDailyPlan(account) : '']));
}
function accountDailyCardHTML(account) {
  const metadata = accountDailyMetadata(account), identity = accountDailyIdentity(account);
  const selection = ACCOUNT_DAILY.bulk ? `<label class="account-daily-check"><input type="checkbox" data-daily-select="${esc(account.id)}" ${ACCOUNT_DAILY.selected.get(account.id) === identity ? 'checked' : ''} ${identity ? '' : 'disabled'} aria-label="${esc('选择 ' + account.name)}">选择</label>` : '';
  const tags = (metadata?.tags || []).map(tag => `<span class="chip" title="${esc(tag)}">${esc(tag)}</span>`).join('');
  return `<div class="account-daily-meta">${selection}<span class="account-active-status" data-account-active="${esc(account.id)}" data-account-active-identity="${esc(identity)}" hidden></span><div class="account-daily-tags">${tags}</div><button class="btn sm ghost" data-act="accountDailyTags" data-id="${esc(account.id)}" ${metadata && identity ? '' : 'disabled'} aria-label="${esc('标签 · ' + account.name)}">标签</button></div>`;
}
function accountDailySync() {
  const root = $('#accountDailyToolbar');
  if (!root || S.route !== 'accounts') return;
  accountActiveSync();
  const tags = [...new Set(ACCOUNT_DAILY.library.flatMap(account => accountDailyMetadata(account)?.tags || []))].sort((a, b) => a.localeCompare(b, 'zh-CN'));
  if (ACCOUNT_DAILY.tag && !tags.includes(ACCOUNT_DAILY.tag)) tags.push(ACCOUNT_DAILY.tag);
  const count = ACCOUNT_DAILY.selected.size;
  const selectable = ACCOUNT_DAILY.visible.filter(account => accountDailyIdentity(account));
  const content = `<div class="row account-daily-filters"><select id="accountDailyPlan" class="input" aria-label="筛选套餐"><option value="">全部套餐</option>${Object.entries(ACCOUNT_DAILY_PLANS).map(([value, label]) => `<option value="${value}" ${ACCOUNT_DAILY.plan === value ? 'selected' : ''}>${label}</option>`).join('')}</select><select id="accountDailyTag" class="input" aria-label="筛选标签"><option value="">全部标签</option>${tags.map(tag => `<option value="${esc(tag)}" ${ACCOUNT_DAILY.tag === tag ? 'selected' : ''}>${esc(tag)}</option>`).join('')}</select><button class="btn sm" data-act="accountDailyBulk" aria-pressed="${ACCOUNT_DAILY.bulk}" aria-controls="accountDailySelection">${ACCOUNT_DAILY.bulk ? '完成选择' : '批量管理'}</button></div>`;
  if (root._accountDailyHTML !== content) { root.innerHTML = content; root._accountDailyHTML = content; }
  const selection = $('#accountDailySelection');
  if (selection) {
    const content = ACCOUNT_DAILY.bulk ? `<label class="account-daily-check"><input id="accountDailyAll" type="checkbox" ${selectable.length && count === selectable.length ? 'checked' : ''} ${selectable.length ? '' : 'disabled'}>选择当前结果</label><span class="muted small">已选 ${count} 个</span><button class="btn sm" data-act="accountDailyRefresh" ${count ? '' : 'disabled'}>刷新状态</button><button class="btn sm" data-act="accountDailyMembers" ${count ? '' : 'disabled'}>API 服务</button><button class="btn sm ghost" data-act="accountDailyClear" ${count ? '' : 'disabled'}>清空</button>` : '';
    if (selection._accountDailyHTML !== content) { selection.innerHTML = content; selection._accountDailyHTML = content; }
    selection.hidden = !ACCOUNT_DAILY.bulk;
  }
  clearTimeout(ACCOUNT_DAILY.expiry);
  const expiry = Math.min(...ACCOUNT_DAILY.library.map(account => Date.parse(account.officialAccount?.usage?.freshUntil || '')).filter(value => value > Date.now()));
  if (Number.isFinite(expiry)) ACCOUNT_DAILY.expiry = setTimeout(() => { if (S.route === 'accounts') renderLocalCards(); }, Math.min(2147483647, Math.max(1, expiry - Date.now() + 1)));
}
function accountDailyReconcile() {
  // Keep focused inputs and expanded controls intact on unchanged cache polls.
  const library = officialLibraryAccounts(S.local?.accounts || []);
  if (accountDailySignature(library) !== ACCOUNT_DAILY.signature) renderLocalCards();
  else accountActiveSync();
}
function accountDailyCurrent(owner) {
  return !!owner && ACCOUNT_DAILY.panel === owner && owner.epoch === OFFICIAL_PANEL.epoch && S.route === 'accounts' && owner.element === $('#officialPanel') && owner.list === $('#localAccountList');
}
function accountDailyRunning(owner) { return accountDailyCurrent(owner) && !owner.cancelled; }
function accountDailyRefreshSlot(owner) {
  if (!accountDailyRunning(owner)) return Promise.resolve(false);
  if (ACCOUNT_DAILY.refreshActive < 3) { ACCOUNT_DAILY.refreshActive++; return Promise.resolve(true); }
  return new Promise(resolve => ACCOUNT_DAILY.refreshWaiters.push({ owner, resolve }));
}
function accountDailyReleaseRefreshSlot() {
  ACCOUNT_DAILY.refreshActive--;
  while (ACCOUNT_DAILY.refreshActive < 3 && ACCOUNT_DAILY.refreshWaiters.length) {
    const waiting = ACCOUNT_DAILY.refreshWaiters.shift(), valid = accountDailyRunning(waiting.owner);
    if (valid) ACCOUNT_DAILY.refreshActive++;
    waiting.resolve(valid);
  }
}
function accountDailyStop(owner) {
  if (!owner || owner.cancelled) return;
  owner.cancelled = true;
  ACCOUNT_DAILY.refreshWaiters = ACCOUNT_DAILY.refreshWaiters.filter(waiting => {
    if (waiting.owner !== owner) return true;
    waiting.resolve(false); return false;
  });
  for (const job of owner.jobs || []) if (job.state === 'waiting' || job.state === 'running') {
    job.message = job.state === 'running' ? '已停止等待，请刷新核对结果' : '已取消'; job.state = 'cancelled';
  }
  accountDailyRender(owner);
}
function accountDailyCancelPanel() { accountDailyStop(ACCOUNT_DAILY.panel); ACCOUNT_DAILY.panel = null; }
function accountDailyDispose() {
  accountDailyCancelPanel(); clearTimeout(ACCOUNT_DAILY.expiry);
  ACCOUNT_DAILY.selected.clear(); ACCOUNT_DAILY.bulk = false; ACCOUNT_DAILY.library = []; ACCOUNT_DAILY.visible = [];
}
function accountDailySnapshot(account) {
  return { id: account.id, name: account.name, identity: accountDailyIdentity(account), source: account.sourceRevision || '', officialId: account.officialAccount?.id || '', identityEpoch: account.officialAccount?.identityEpoch || 0, memberEligible: !account.libraryOnly && account.officialAccount?.mode !== 'chatgpt_web_session' && /^src-v1-[a-f0-9]{64}$/.test(account.sourceRevision || ''), state: 'waiting', message: '等待处理' };
}
function accountDailySelected() {
  return ACCOUNT_DAILY.visible.filter(account => ACCOUNT_DAILY.selected.get(account.id) === accountDailyIdentity(account)).map(accountDailySnapshot);
}
function accountDailyOpen(title, kind, jobs) {
  officialPanel(title, '<div id="accountDailyContent" class="stack"></div>');
  const owner = ACCOUNT_DAILY.panel = { kind, jobs, epoch: OFFICIAL_PANEL.epoch, element: $('#officialPanel'), list: $('#localAccountList'), cancelled: false, started: false, pending: false, clients: [], clientId: '', loading: false, error: '' };
  accountDailyRender(owner); return owner;
}
function accountDailyRender(owner) {
  if (!accountDailyCurrent(owner) || !$('#accountDailyContent')) return;
  const done = owner.jobs.filter(job => !['waiting', 'running'].includes(job.state)).length;
  let controls = '';
  if (owner.kind === 'members' && !owner.started) {
    controls = `<select id="accountDailyClient" class="input" aria-label="服务客户端" ${owner.loading ? 'disabled' : ''}><option value="">选择服务客户端</option>${owner.clients.map(client => `<option value="${esc(client.id)}" ${owner.clientId === client.id ? 'selected' : ''}>${esc(client.name || client.id)}${client.disabled ? '（已停用）' : ''}</option>`).join('')}</select><div class="row">${[['text', true, '加入文字'], ['text', false, '移出文字'], ['image', true, '加入图片'], ['image', false, '移出图片']].map(([mode, enabled, label]) => `<button class="btn sm" data-act="accountDailyApplyMembers" data-mode="${mode}" data-enabled="${enabled}" ${!owner.clientId || owner.loading ? 'disabled' : ''}>${label}</button>`).join('')}</div><p class="muted small">主账号、最后成员和固定账号请在客户端设置中修改。</p>`;
  }
  $('#accountDailyContent').innerHTML = `${controls}<div class="row"><span class="muted small">${owner.loading ? '正在读取…' : `已处理 ${done} / ${owner.jobs.length}`}</span>${owner.pending && !owner.cancelled ? '<button class="btn sm ghost" data-act="accountDailyStop">停止后续操作</button>' : ''}${owner.started && !owner.pending ? '<button class="btn sm ghost" data-act="accountDailyReload">刷新列表</button>' : ''}</div>${owner.error ? `<div class="callout warn">${esc(owner.error)}</div>` : ''}<div class="account-daily-results" aria-live="polite">${owner.jobs.map(job => `<div class="restore-row"><span class="li-main">${esc(job.name || job.id)}</span><span class="small ${job.state === 'done' ? 'ok' : 'muted'}">${esc(job.message)}</span></div>`).join('')}</div>`;
}
function accountDailyJobCurrent(job, local = S.local) {
  if (job.officialId && job.id.startsWith('official:')) {
    return (S.officialRows || []).some(row => row.id === job.officialId && row.identityEpoch === job.identityEpoch);
  }
  const account = local?.accounts?.find(row => row.id === job.id);
  return !!account && accountDailyIdentity(account) === job.identity;
}
async function accountDailyRefreshOne(owner, job) {
  if (!accountDailyRunning(owner)) return;
  if (!accountDailyJobCurrent(job)) { job.state = 'changed'; job.message = '账号已变化'; return; }
  job.state = 'running'; job.message = '正在刷新…'; accountDailyRender(owner);
  try {
    const result = job.officialId
      ? await api('/api/local/accounts/daily-refresh', { id: job.officialId, identityEpoch: job.identityEpoch })
      : await api('/api/local/accounts/daily-usage', { id: job.id, expectedSourceRevision: job.source });
    if (!accountDailyRunning(owner)) return;
    if (!accountDailyJobCurrent(job)) throw new Error('账号已变化，请刷新列表');
    if (job.officialId) {
      if (result?.ok !== true || result.id !== job.officialId || result.identityEpoch !== job.identityEpoch) throw new Error('刷新结果待确认');
    } else {
      const data = apiCardUsageData(result, { id: job.id, revision: job.source }, Date.now());
      if (!data) throw new Error('用量结果已过期或来源已变化');
      API_CARD_USAGE.cache.set(job.id + '\x00' + job.source, data);
      if (!data.balance && !data.today && !data.billing && !data.group) { job.state = 'skipped'; job.message = '暂未提供用量数据'; return; }
    }
    job.state = 'done'; job.message = '已刷新';
  } catch (error) {
    if (!accountDailyRunning(owner)) return;
    job.state = 'error'; job.message = String(error.message || '刷新失败').slice(0, 160);
  } finally { accountDailyRender(owner); }
}
async function accountDailyReload(owner) {
  if (!accountDailyCurrent(owner)) return;
  const token = officialMutationToken();
  await refreshOfficialCards(token);
}
ACTIONS.accountDailyBulk = () => { ACCOUNT_DAILY.bulk = !ACCOUNT_DAILY.bulk; ACCOUNT_DAILY.selected.clear(); renderLocalCards(); };
ACTIONS.accountDailyClear = () => { ACCOUNT_DAILY.selected.clear(); renderLocalCards(); };
ACTIONS.accountDailyStop = () => accountDailyStop(ACCOUNT_DAILY.panel);
ACTIONS.accountDailyReload = () => accountDailyReload(ACCOUNT_DAILY.panel);
ACTIONS.accountDailyRefresh = async button => {
  if (button?.isConnected === false || S.route !== 'accounts') return;
  const jobs = accountDailySelected(); if (!jobs.length) return;
  const owner = accountDailyOpen('批量刷新', 'refresh', jobs);
  owner.started = owner.pending = true; officialMutationToken(); accountDailyRender(owner);
  let index = 0;
  await Promise.all(Array.from({ length: Math.min(3, jobs.length) }, async () => {
    while (accountDailyRunning(owner) && index < jobs.length) {
      const job = jobs[index++];
      if (!await accountDailyRefreshSlot(owner)) break;
      try { if (accountDailyRunning(owner)) await accountDailyRefreshOne(owner, job); }
      finally { accountDailyReleaseRefreshSlot(); }
    }
  }));
  owner.pending = false; accountDailyRender(owner);
  if (accountDailyRunning(owner)) await accountDailyReload(owner);
};
ACTIONS.accountDailyMembers = async button => {
  if (button?.isConnected === false || S.route !== 'accounts') return;
  const jobs = accountDailySelected(); if (!jobs.length) return;
  const owner = accountDailyOpen('批量设置 API 服务', 'members', jobs);
  owner.loading = true; accountDailyRender(owner);
  try {
    const status = await api('/api/local/codex-api');
    if (!accountDailyRunning(owner)) return;
    if (!Array.isArray(status?.clients) || status.clients.some(client => !/^api-client-v1-[a-f0-9]{64}$/.test(client.revision || ''))) throw new Error('服务配置读取失败，请重新打开');
    owner.clients = status.clients; if (owner.clients.length === 1) owner.clientId = owner.clients[0].id;
    if (!owner.clients.length) owner.error = '请先配置 API 服务客户端';
  } catch (error) { if (accountDailyRunning(owner)) owner.error = error.message; }
  finally { owner.loading = false; accountDailyRender(owner); }
};
function accountDailyMembership(client, job, mode, enabled) {
  if (!job.memberEligible) return '请先关联可用 API 账号';
  const ids = mode === 'text' ? client.accountIds : client.imageAccountIds;
  const explicit = Array.isArray(ids) ? ids : [], member = explicit.length ? explicit.includes(job.id) : client.accountId === job.id;
  if (member === enabled) return enabled ? '已是成员' : '不在此服务中';
  if (client.accountId === job.id || !explicit.length || !enabled && explicit.length === 1 || mode === 'text' && !enabled && client.poolStrategy === 'single_account' && client.poolAccountId === job.id) return '需在客户端设置中修改';
  return '';
}
ACTIONS.accountDailyApplyMembers = async button => {
  const owner = ACCOUNT_DAILY.panel, mode = button?.dataset?.mode, enabled = button?.dataset?.enabled === 'true';
  if (!accountDailyRunning(owner) || owner.kind !== 'members' || owner.started || owner.loading || button?.isConnected === false || !['text', 'image'].includes(mode) || !['true', 'false'].includes(button.dataset.enabled) || apiClientWritePending) return;
  const client = owner.clients.find(row => row.id === owner.clientId);
  if (!client || $('#accountDailyClient')?.value !== client.id) return;
  owner.started = owner.pending = true; officialMutationToken(); accountDailyRender(owner);
  await withAPIClientWrite(async () => {
    let expectedRevision = client.revision, stopped = '';
    for (const job of owner.jobs) {
      if (!accountDailyRunning(owner)) break;
      if (stopped) { job.state = 'skipped'; job.message = stopped; continue; }
      job.state = 'running'; job.message = '正在检查…'; accountDailyRender(owner);
      let attempted = false;
      try {
        const [status, local] = await Promise.all([api('/api/local/codex-api'), api('/api/local/accounts')]);
        if (!accountDailyRunning(owner)) break;
        const fresh = status?.clients?.find(row => row.id === client.id);
        if (!fresh || fresh.revision !== expectedRevision) { stopped = '服务已变化，后续已停止'; throw new Error(stopped); }
        if (!accountDailyJobCurrent(job, local)) { job.state = 'changed'; job.message = '账号已变化'; continue; }
        const skip = accountDailyMembership(fresh, job, mode, enabled);
        if (skip) { job.state = 'skipped'; job.message = skip; continue; }
        attempted = true; job.message = '正在保存…'; accountDailyRender(owner);
        const result = await api('/api/local/codex-api/members', { clientId: client.id, accountId: job.id, mode, enabled, expectedSourceRevision: job.source, expectedClientRevision: expectedRevision });
        if (!accountDailyRunning(owner)) break;
        if (result?.ok !== true || result.clientId !== client.id || result.accountId !== job.id || result.mode !== mode || result.enabled !== enabled || !/^api-client-v1-[a-f0-9]{64}$/.test(result.clientRevision || '')) throw new Error('保存结果待确认，请刷新列表');
        expectedRevision = result.clientRevision; job.state = 'done'; job.message = enabled ? '已加入' : '已移出';
      } catch (error) {
        if (!accountDailyRunning(owner)) break;
        job.state = attempted ? 'unknown' : 'error'; job.message = String(error.message || '操作未完成').slice(0, 160);
        // A failed/ambiguous write must not consume a newly read revision and
        // continue granting access to other rows from this older confirmation.
        stopped = '后续已停止，请刷新后重试';
      } finally { accountDailyRender(owner); }
    }
  });
  owner.pending = false; accountDailyRender(owner);
  if (accountDailyRunning(owner)) await accountDailyReload(owner);
};
ACTIONS.accountDailyTags = button => {
  if (S.route !== 'accounts' || button?.isConnected === false) return;
  const account = ACCOUNT_DAILY.visible.find(row => row.id === button.dataset.id), metadata = accountDailyMetadata(account);
  if (!account || !metadata || !accountDailyIdentity(account)) return;
  officialPanel('账号标签', `<p class="muted small">${esc(account.name)}</p><div class="field"><label for="accountDailyTagsInput">自定义标签</label><input id="accountDailyTagsInput" class="input" maxlength="400" placeholder="开发, 常用" value="${esc(metadata.tags.join(', '))}"><span class="muted small">用逗号分隔，每个最多 24 字，最多 12 个。</span></div><button class="btn primary" data-act="accountDailyTagsSave">保存标签</button>`);
  ACCOUNT_DAILY.panel = { kind: 'tags', jobs: [], account: accountDailySnapshot(account), metadata: { ...metadata, tags: [...metadata.tags] }, epoch: OFFICIAL_PANEL.epoch, element: $('#officialPanel'), list: $('#localAccountList'), pending: false, attempted: false };
};
ACTIONS.accountDailyTagsSave = async button => {
  const owner = ACCOUNT_DAILY.panel;
  if (!accountDailyRunning(owner) || owner.kind !== 'tags' || owner.pending || owner.attempted || button?.isConnected === false) return;
  const job = owner.account, input = $('#accountDailyTagsInput'); if (!input || !accountDailyJobCurrent(job)) return;
  const tags = [...new Set(input.value.split(/[,，]/).map(value => value.trim()).filter(Boolean))];
  if (tags.length > 12 || tags.some(tag => [...tag].length > 24 || /[\x00-\x1f\x7f-\x9f\u200b-\u200f\u202a-\u202e\u2060-\u206f\ufeff]/.test(tag))) throw new Error('最多 12 个标签，每个 1–24 字');
  const body = { id: job.officialId || job.id, kind: job.officialId ? 'official' : 'local', tags, expectedTagsRevision: owner.metadata.revision, ...(job.officialId ? { identityEpoch: job.identityEpoch } : { expectedSourceRevision: job.source }) };
  owner.pending = owner.attempted = true; input.disabled = true;
  const token = officialMutationToken();
  try {
    const result = await busy(button, () => api('/api/local/accounts/tags', body));
    if (!accountDailyRunning(owner)) return;
    if (result?.ok !== true || result.id !== body.id || result.kind !== body.kind || !/^tags-v1-[a-f0-9]{64}$/.test(result.metadata?.revision || '') || JSON.stringify(result.metadata.tags) !== JSON.stringify(tags) || job.officialId && result.metadata.identityEpoch !== job.identityEpoch) throw new Error('保存结果待确认，请重新打开');
    await refreshOfficialCards(token);
    if (accountDailyRunning(owner) && officialMutationCurrent(token)) { closeOfficialPanel(); toast('标签已保存', 'ok'); }
  } catch (error) { if (accountDailyCurrent(owner) && $('#officialError')) $('#officialError').textContent = error.message || '标签未保存，请重新打开'; }
  finally { owner.pending = false; }
};
document.addEventListener('change', event => {
  const target = event.target;
  if (S.route !== 'accounts') return;
  if (target.id === 'accountDailyPlan' || target.id === 'accountDailyTag') {
    accountDailyStop(ACCOUNT_DAILY.panel);
    ACCOUNT_DAILY[target.id === 'accountDailyPlan' ? 'plan' : 'tag'] = target.value; renderLocalCards();
  } else if (target.id === 'accountDailyAll') {
    accountDailyStop(ACCOUNT_DAILY.panel); ACCOUNT_DAILY.selected.clear();
    if (target.checked) for (const account of ACCOUNT_DAILY.visible) { const identity = accountDailyIdentity(account); if (identity) ACCOUNT_DAILY.selected.set(account.id, identity); }
    renderLocalCards();
  } else if (target.dataset?.dailySelect) {
    accountDailyStop(ACCOUNT_DAILY.panel);
    const account = ACCOUNT_DAILY.visible.find(row => row.id === target.dataset.dailySelect), identity = accountDailyIdentity(account);
    if (!account || !identity) return;
    if (target.checked) ACCOUNT_DAILY.selected.set(account.id, identity); else ACCOUNT_DAILY.selected.delete(account.id);
    accountDailySync();
  } else if (target.id === 'accountDailyClient') {
    const owner = ACCOUNT_DAILY.panel;
    if (!accountDailyRunning(owner) || owner.started || owner.loading) return;
    owner.clientId = owner.clients.some(client => client.id === target.value) ? target.value : ''; accountDailyRender(owner);
  }
});
document.addEventListener('keydown', event => {
  if (event.key === 'Escape' && accountDailyCurrent(ACCOUNT_DAILY.panel)) { event.preventDefault(); event.stopImmediatePropagation(); closeOfficialPanel(); }
}, true);
