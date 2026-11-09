'use strict';

let apiProfilePanel = null;
let apiProfileWritePending = false;

function currentAPIProfile(owner) {
  return apiProfilePanel === owner && OFFICIAL_PANEL.epoch === owner.epoch && !!$('#apiProfileContent');
}

function renderAPIProfile(owner) {
  if (!currentAPIProfile(owner)) return;
  const row = owner.data, profile = row?.profile;
  const clients = Array.isArray(row?.clients) ? row.clients : [];
  const client = clients.find(value => value.id === owner.clientId);
  const models = Array.isArray(client?.models) ? client.models : [];
  const busy = owner.loading || owner.pending || apiProfileWritePending;
  const canApply = !!profile && row.running && !!client && models.includes(owner.model)
    && ['idle', 'applied'].includes(profile.state) && !busy;
  const canRestore = !!profile?.hasBackup && ['applied', 'recovery_required'].includes(profile.state) && !busy;
  const state = owner.pending ? owner.pending === 'restore' ? '正在恢复原配置…' : '正在备份并应用…'
    : owner.loading ? '正在核对配置…'
    : profile?.state === 'applied' ? row.active ? '已应用 · 本次连接有效' : '已备份 · 连接已失效'
    : ({ idle: '未应用', conflict: '配置已被其他程序修改', recovery_required: '配置事务未完成' })[profile?.state] || '状态不可用';
  const error = owner.error || profile?.error || '';
  $('#apiProfileContent').innerHTML = `<div class="row"><span class="chip" role="status">${esc(state)}</span><button class="icon-btn" data-act="codexAPIProfileRefresh" aria-label="刷新 Codex 配置" ${busy ? 'disabled' : ''}>${icon('refresh')}</button></div>
    ${row ? `<div class="field"><label for="apiProfileClient">服务配置</label><select id="apiProfileClient" class="input" ${busy || !clients.length ? 'disabled' : ''}>${clients.length ? clients.map(value => `<option value="${esc(value.id)}" ${value.id === owner.clientId ? 'selected' : ''}>${esc(value.name)}</option>`).join('') : '<option value="">暂无可用配置</option>'}</select></div>
    <div class="field"><label for="apiProfileModel">默认模型</label><select id="apiProfileModel" class="input" ${busy || !models.length ? 'disabled' : ''}>${models.map(model => `<option value="${esc(model)}" ${model === owner.model ? 'selected' : ''}>${esc(model)}</option>`).join('') || '<option value="">暂无模型</option>'}</select></div>
    <p class="muted small">${row.running ? '应用前备份原配置，停止服务时自动恢复。' : '服务未启动，原配置仍可恢复。'}</p>
    <div class="row"><button class="btn primary" data-act="codexAPIProfileApply" ${canApply ? '' : 'disabled'}>${profile?.hasBackup ? '重新应用' : '备份并应用'}</button><button class="btn" data-act="codexAPIProfileRestore" ${canRestore ? '' : 'disabled'}>恢复原配置</button></div>` : ''}
    ${error ? `<div class="callout warn" role="alert">${esc(error)}</div>` : ''}`;
}

async function loadAPIProfile(owner) {
  if (!currentAPIProfile(owner)) return;
  const seq = ++owner.seq;
  owner.loading = true;
  renderAPIProfile(owner);
  try {
    const row = await api('/api/local/codex-api/profile');
    if (!currentAPIProfile(owner) || seq !== owner.seq) return;
    if (!row?.profile || !Array.isArray(row.clients)) throw new Error('Codex 配置状态不完整');
    owner.data = row;
    const client = row.clients.find(value => value.id === owner.clientId)
      || row.clients.find(value => value.id === row.clientId) || row.clients[0];
    owner.clientId = client?.id || '';
    owner.model = client?.models?.includes(owner.model) ? owner.model
      : client?.models?.includes(row.model) ? row.model : client?.models?.[0] || '';
  } catch (error) {
    if (currentAPIProfile(owner) && seq === owner.seq) {
      owner.data = null;
      owner.error = error.message;
    }
  } finally {
    if (currentAPIProfile(owner) && seq === owner.seq) {
      owner.loading = false;
      renderAPIProfile(owner);
    }
  }
}

async function openAPIProfile() {
  officialPanel('Codex 配置', '<div id="apiProfileContent" class="stack" aria-live="polite"></div>');
  apiManagementShell('<button class="btn" data-act="codexAPI" data-view="clients">返回客户端</button>');
  $('#officialPanel')?.closest?.('.modal')?.classList.add('api-profile-modal');
  const owner = { epoch: OFFICIAL_PANEL.epoch, seq: 0, data: null, loading: false, pending: '', error: '', clientId: '', model: '' };
  apiProfilePanel = owner;
  await loadAPIProfile(owner);
}

async function mutateAPIProfile(action) {
  const owner = apiProfilePanel;
  if (!owner || !currentAPIProfile(owner) || owner.loading || owner.pending || apiProfileWritePending || !owner.data?.profile) return;
  const profile = owner.data.profile;
  const client = owner.data.clients.find(value => value.id === owner.clientId);
  if (action === 'apply' && (!owner.data.running || !client || !client.models.includes(owner.model) || !['idle', 'applied'].includes(profile.state))) return;
  if (action === 'restore' && (!profile.hasBackup || !['applied', 'recovery_required'].includes(profile.state))) return;
  const body = action === 'apply'
    ? { clientId: client.id, clientRevision: client.revision, profileRevision: profile.revision, model: owner.model }
    : { revision: profile.revision, generation: profile.generation };
  apiProfileWritePending = true;
  owner.pending = action;
  owner.error = '';
  renderAPIProfile(owner);
  try {
    await api('/api/local/codex-api/profile/' + action, body);
  } catch (error) {
    if (currentAPIProfile(owner)) owner.error = error.message;
  } finally {
    apiProfileWritePending = false;
    if (currentAPIProfile(owner)) {
      owner.pending = '';
      await loadAPIProfile(owner);
    } else if (apiProfilePanel && currentAPIProfile(apiProfilePanel)) {
      // Reopening while a write is in flight must not leave the new panel
      // disabled forever or repaint it using the old response.
      await loadAPIProfile(apiProfilePanel);
    }
    void loadCodexAPIOverview();
  }
}

document.addEventListener('change', event => {
  const owner = apiProfilePanel;
  if (!owner || !currentAPIProfile(owner) || owner.loading || owner.pending || apiProfileWritePending) return;
  if (event.target.id === 'apiProfileClient') {
    owner.clientId = event.target.value;
    owner.model = owner.data?.clients.find(value => value.id === owner.clientId)?.models?.[0] || '';
    owner.error = '';
    renderAPIProfile(owner);
  } else if (event.target.id === 'apiProfileModel') {
    owner.model = event.target.value;
  }
});

Object.assign(ACTIONS, {
  codexAPIProfile: () => apiManagementLeave(openAPIProfile),
  codexAPIProfileRefresh: async () => {
    const owner = apiProfilePanel;
    if (!owner || !currentAPIProfile(owner) || owner.pending || apiProfileWritePending) return;
    owner.error = '';
    await loadAPIProfile(owner);
  },
  codexAPIProfileApply: () => mutateAPIProfile('apply'),
  codexAPIProfileRestore: () => mutateAPIProfile('restore'),
});
