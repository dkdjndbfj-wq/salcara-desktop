(() => {
  let loadingParent = null;
  let revision = '';
  let writing = false, observationSequence = 0;
  const current = (epoch, parent) => epoch === OFFICIAL_PANEL.epoch && parent?.isConnected && $('#apiServiceAdvanced') === parent;
  function showRunning(state) {
    if (CODEX_API_OVERVIEW.error || CODEX_API_OVERVIEW.pending) return;
    const badge = $('#apiServiceRuntime .account-state'), button = $('[data-act="codexAPIEnabled"]');
    if (typeof state.running !== 'boolean' || typeof state.enabled !== 'boolean') return;
    if (badge) { badge.textContent = state.running ? '运行中' : state.requiresStart ? '待启动' : '已关闭'; badge.className = 'account-state ' + (state.running ? 'ok' : 'muted'); }
    if (button) { button.dataset.enabled = String(state.requiresStart || !state.enabled); button.textContent = state.requiresStart ? '启动本次服务' : state.enabled ? '停止服务' : '启动服务'; }
  }
  function showNetwork(state) {
    showRunning(state);
    const summary = $('#apiServiceNetworkSummary');
    if (summary) summary.textContent = `${state.bindScope === 'lan' ? '局域网 / WSL NAT' : '仅本机'} · ${state.listeningPorts || 0} 个专用端口监听${state.lastError ? ' · ' + state.lastError : ''}`;
    const effective = $('#apiNetworkEffective');
    if (effective) effective.textContent = state.lastError || `${state.listeningPorts || 0} 个专用端口正在监听`;
  }
  globalThis.refreshAPINetworkObservation = async () => {
    const parent = $('#apiServiceAdvanced'), epoch = OFFICIAL_PANEL.epoch;
    if (!parent || !$('#apiNetworkSettings') || writing) return;
    const sequence = ++observationSequence;
    try {
      const state = await api('/api/local/codex-api/network');
      if (current(epoch, parent) && sequence === observationSequence && !writing) showNetwork(state);
    } catch (_) {
      if (current(epoch, parent) && sequence === observationSequence && $('#apiServiceNetworkSummary')) $('#apiServiceNetworkSummary').textContent = '网络运行状态待刷新';
    }
  };
  async function renderNetwork() {
    const parent = $('#apiServiceAdvanced');
    if (!parent || !parent.childElementCount || parent.querySelector('#apiNetworkSettings') || loadingParent === parent) return;
    const epoch = OFFICIAL_PANEL.epoch;
    loadingParent = parent;
    try {
      const state = await api('/api/local/codex-api/network');
      if (!current(epoch, parent) || parent.querySelector('#apiNetworkSettings')) return;
      revision = state.revision;
      if (Array.isArray(state.localHosts) && state.localHosts.length && !parent.querySelector('#apiNetworkAddressHelp')) parent.insertAdjacentHTML('beforeend', `<div id="apiNetworkAddressHelp" class="muted small">Windows 当前可用局域网主机：${state.localHosts.map(esc).join(' · ')}</div>`);
      parent.insertAdjacentHTML('beforeend', `<div id="apiNetworkSettings" class="stack"><div class="field"><label for="apiNetworkScope">专用 API 访问范围</label><select id="apiNetworkScope" class="input"><option value="loopback" ${state.bindScope === 'loopback' ? 'selected' : ''}>仅本机</option><option value="lan" ${state.bindScope === 'lan' ? 'selected' : ''}>局域网 / WSL NAT</option></select></div><div class="form-grid"><div class="field"><label for="apiNetworkPorts">专用端口（逗号分隔）</label><input id="apiNetworkPorts" class="input" value="${esc((state.ports || []).join(', '))}" placeholder="8317, 8318"></div><div class="field"><label for="apiNetworkHost">客户端连接主机</label><input id="apiNetworkHost" class="input" value="${esc(state.clientHost || '127.0.0.1')}" placeholder="127.0.0.1"></div></div><div class="muted small">端口留空使用现有本机接口。专用端口均需要客户端密钥，控制台保持本机访问。WSL 镜像网络可使用 localhost；NAT 模式需选择局域网范围并填写 Windows 的局域网 IP。</div><button class="btn" data-act="codexAPINetworkSave">保存网络设置</button><div id="apiNetworkStatus" class="muted small" role="status">${esc(state.lastError || `${state.listeningPorts} 个专用端口正在监听`)}</div><div class="field"><label>专用接口地址</label><textarea class="input" readonly>${esc((state.baseUrls || []).join('\n') || '尚未设置专用端口')}</textarea></div></div>`);
      $('#apiNetworkSettings').insertAdjacentHTML('afterbegin', `<div class="row"><b>访问与端口</b><span class="spacer"></span><button class="icon-btn" data-act="codexAPINetworkRefresh" title="刷新监听状态" aria-label="刷新监听状态">${icon('refresh')}</button></div><div id="apiNetworkEffective" class="muted small" role="status"></div>`);
      $('#apiNetworkStatus').textContent = '';
      showNetwork(state);
      if (typeof rememberAPISettings === 'function') rememberAPISettings('apiNetworkSettings');
    } catch (_) {
      if (current(epoch, parent)) parent.insertAdjacentHTML('beforeend', '<div id="apiNetworkSettings" class="field"><span class="muted small">网络设置读取失败</span><button class="btn" data-act="codexAPINetworkRetry">重试</button></div>');
    } finally { if (loadingParent === parent) loadingParent = null; }
  }
  function readNetwork() {
    const text = $('#apiNetworkPorts').value.trim();
    const raw = text ? text.split(/[,，\s]+/) : [];
    if (raw.some((part) => !/^\d+$/.test(part))) throw new Error('端口须为 1–65535 的整数');
    const ports = raw.map(Number);
    if (ports.length > 16 || new Set(ports).size !== ports.length || ports.some((port) => !Number.isInteger(port) || port < 1 || port > 65535)) throw new Error('最多设置 16 个不重复端口，范围为 1–65535');
    return { bindScope: $('#apiNetworkScope').value, ports, clientHost: $('#apiNetworkHost').value.trim(), revision };
  }
  ACTIONS.codexAPINetworkRetry = async () => { $('#apiNetworkSettings')?.remove(); await renderNetwork(); };
  ACTIONS.codexAPINetworkRefresh = button => busy(button, refreshAPINetworkObservation);
  ACTIONS.codexAPINetworkSave = async (button) => {
    if (writing) return;
    const epoch = OFFICIAL_PANEL.epoch, parent = $('#apiServiceAdvanced'), status = $('#apiNetworkStatus');
    const body = readNetwork();
    const submitted = Object.fromEntries(Array.from($('#apiNetworkSettings').querySelectorAll('input,select'), node => [node.id, node.value]));
    writing = true; observationSequence++;
    const owner = apiClientPanel; if (owner) owner.settingsWriting = (owner.settingsWriting || 0) + 1;
    try {
      const saved = await busy(button, () => api('/api/local/codex-api/network', body));
      if (!current(epoch, parent) || $('#apiNetworkStatus') !== status) return;
      revision = saved.revision;
      if (typeof rememberAPISettings === 'function') rememberAPISettings('apiNetworkSettings', submitted);
      showNetwork(saved);
      const draft = readNetwork();
      status.textContent = JSON.stringify({ ...draft, revision: '' }) === JSON.stringify({ ...body, revision: '' }) ? `已保存 · ${saved.listeningPorts} 个专用端口正在监听` : '设置已更改，尚未保存';
      if (draft.ports.join() === body.ports.join() && draft.clientHost === body.clientHost) $('#apiNetworkSettings textarea').value = (saved.baseUrls || []).join('\n') || '尚未设置专用端口';
    } catch (error) {
      if (!current(epoch, parent) || $('#apiNetworkStatus') !== status) return;
      status.textContent = error.message; revision = '';
      try {
        const latest = await api('/api/local/codex-api/network');
        if (current(epoch, parent) && $('#apiNetworkStatus') === status) { revision = latest.revision; showNetwork(latest); }
      } catch (_) { if (current(epoch, parent) && $('#apiNetworkStatus') === status) status.textContent = error.message + '；请重新打开面板再试'; }
    } finally { writing = false; if (owner) owner.settingsWriting--; }
  };
  document.addEventListener('input', (event) => { if (['apiNetworkPorts', 'apiNetworkHost'].includes(event.target?.id) && $('#apiNetworkStatus')) $('#apiNetworkStatus').textContent = ''; });
  document.addEventListener('change', (event) => { if (event.target?.id === 'apiNetworkScope' && $('#apiNetworkStatus')) $('#apiNetworkStatus').textContent = ''; });
  new MutationObserver(() => { void renderNetwork(); }).observe(document.body, { childList: true, subtree: true });
})();
