'use strict';
(() => {
  const cache = new Map(), pending = new Map(), attempted = new Set(), queue = [], stationUncertain = new Set();
  let active = 0, expiryTimer = 0, officialPicker = null, stationBusy = '';
  let stationAccountRows = null, stationAccountRequest = 0;
  const unknown = () => ({ apiRemote: 'unknown', accountRemote: 'unknown' });
  function base(raw) {
    try {
      if (typeof raw !== 'string' || /[%\\\s?#]/.test(raw)) return '';
      const u = new URL(raw);
      if (u.username || u.password || u.search || u.hash || u.pathname.includes('//') || !['https:', 'http:'].includes(u.protocol)) return '';
      return u.origin + u.pathname.replace(/\/$/, '').replace(/\/v1$/i, '');
    } catch (_) { return ''; }
  }
  function accountKey(account) {
    if (!account || account.libraryOnly || account.officialAccount || !['', 'api_key', 'reverse_proxy'].includes(account.sourceType || '') || !/^src-v1-[a-f0-9]{64}$/.test(account.sourceRevision || '')) return '';
    const endpoint = base(account.baseUrl);
    return endpoint ? account.id + '\u0000' + account.sourceRevision + '\u0000' + endpoint : '';
  }
  function state(support, field) {
    if (!support || !Number.isSafeInteger(support.checkedAt) || support.checkedAt <= 0 || support.checkedAt > Date.now() + 30000 || !Number.isSafeInteger(support.freshUntil) || support.freshUntil <= Date.now() || support.freshUntil <= support.checkedAt || support.freshUntil > support.checkedAt + 300000) return 'unknown';
    return ['supported', 'unsupported'].includes(support[field]) ? support[field] : 'unknown';
  }
  function trimCache() {
    while (cache.size > 256) cache.delete(cache.keys().next().value);
    while (attempted.size > 512) attempted.delete(attempted.values().next().value);
  }
  function data(account) { return cache.get(accountKey(account)) || { support: account.remoteSupport || unknown() }; }
  function label(status) { return { supported: '可用', unsupported: '暂不支持', unknown: '待检测' }[status]; }
  function marker(kind, status, attrs = '', waiting = false) {
    const name = kind === 'apiRemote' ? 'API 远程' : '账号远程';
    const text = name + ' · ' + (waiting ? '检测中' : label(status));
    const symbol = kind === 'apiRemote' ? '<path d="m8 8-4 4 4 4m8-8 4 4-4 4m-3-11-2 22"/>' : '<circle cx="12" cy="8" r="3"/><path d="M5 21v-3a7 7 0 0 1 14 0v3"/>';
    const glyph = waiting ? '<circle cx="12" cy="12" r="8" opacity=".2"/><path d="M12 4a8 8 0 0 1 8 8"/>' : symbol;
    return `<button class="remote-capability is-${status}${waiting ? ' is-checking' : ''}" ${attrs} title="${esc(text + '；仅表示站点能力，连接后仍需授权。点按可重新检测')}" aria-label="${esc(text)}" aria-busy="${waiting}" ${waiting ? 'disabled' : ''}><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${glyph}</svg><i aria-hidden="true"></i></button>`;
  }
  function accountMarks(account) {
    const item = data(account), waiting = pending.has(accountKey(account));
    return ['apiRemote', 'accountRemote'].map(kind => marker(kind, waiting ? 'unknown' : state(item.support, kind), `data-act="accountRemoteProbe" data-id="${esc(account.id)}" data-source="${esc(account.sourceRevision || '')}" data-base="${esc(base(account.baseUrl))}"`, waiting)).join('');
  }
  globalThis.accountRemoteSupportHTML = account => `<div class="account-remote-support" id="accountRemote-${esc(account.id)}" data-source="${esc(account.sourceRevision || '')}" data-base="${esc(base(account.baseUrl))}">${accountMarks(account)}</div>`;
  function uniqueStations(source, mode) {
    const rows = new Map();
    for (const row of source) {
      if (!row || typeof row.hubUrl !== 'string') continue;
      // A Hub and a Bot may save different device identities at one URL.
      // Filter the mode before URL deduplication so a newer Bot cannot hide its Hub.
      if (mode && (row.mode === 'bot' || row.botGroupId ? 'bot' : 'hub') !== mode) continue;
      const key = base(row.hubUrl) || row.hubUrl, old = rows.get(key);
      if (!old || (row.support?.checkedAt || 0) >= (old.support?.checkedAt || 0)) rows.set(key, row);
    }
    return [...rows.values()];
  }
  function stations(mode) {
    return uniqueStations([...(S.state?.config?.remoteConnections || []), ...(S.local?.remoteStations || [])], mode)
      .map(row => stationUncertain.has(row.hubUrl) ? { ...row, support: unknown() } : row);
  }
  function officialReady(account) {
    return !!account?.officialAccount && !account.libraryOnly && account.officialAccount.mode !== 'chatgpt_web_session' && /^src-v1-[a-f0-9]{64}$/.test(account.sourceRevision || '');
  }
  function officialMarks(account) {
    if (!officialReady(account)) return '';
    const count = stations().filter(row => state(row.support, 'accountRemote') === 'supported').length;
    return `<button class="official-remote-entry ${count ? 'available' : ''}" data-act="officialRemoteStations" data-id="${esc(account.id)}" title="选择允许官方账号远程的已保存站点" aria-label="账号远程 · ${count ? count + ' 个可用站点' : '暂不支持'}">${icon('phone')}账号远程<span>${count ? count + ' 站' : '暂不支持'}</span></button>`;
  }
  globalThis.officialRemoteSupportHTML = account => officialReady(account) ? `<span id="officialRemote-${esc(account.id)}" data-source="${esc(account.sourceRevision)}">${officialMarks(account)}</span>` : '';
  function accountStationScope(row) {
    const normalize = value => String(value || '').trim().replace(/\/+$/, '').replace(/\/v1$/i, '').replace(/\/+$/, '');
    const accounts = stationAccountRows || S.local?.accounts || [];
    const eligible = accounts.filter(account => account.remoteHubBindingsSet !== true ||
      (account.remoteHubUrls || []).some(hub => normalize(hub) === normalize(row.hubUrl)));
    const names = eligible.slice(0, 4).map(account => account.name || '未命名 API');
    return { count: eligible.length, total: accounts.length, names, legacy: eligible.some(account => account.remoteHubBindingsSet !== true) };
  }
  function stationRowsHTML(rows, official) {
    rows = uniqueStations(rows, official ? '' : 'hub');
    if (!rows.length) return '<p class="muted small">尚无已保存站点</p>';
    return rows.map(row => {
      const attrs = `data-act="remoteStationProbe" data-hub="${esc(row.hubUrl)}"`, ready = state(row.support, 'accountRemote') === 'supported', waiting = stationBusy === row.hubUrl;
      const mode = typeof rmEffectiveMode === 'function' ? rmEffectiveMode() : S.state?.config?.effectiveRemoteMode || (S.state?.config?.loggedIn ? 'hub' : 'none');
      const current = mode === (row.mode === 'bot' || row.botGroupId ? 'bot' : 'hub') && base(row.hubUrl) === base(S.state?.config?.effectiveHubUrl);
      const badges = ['apiRemote', 'accountRemote'].map(kind => marker(kind, state(row.support, kind), attrs, waiting)).join('');
      const scope = accountStationScope(row);
      const scopeText = stationAccountRows ? `电脑允许 ${scope.count} / ${scope.total} 个 API` : '正在读取 API 范围';
      const scopeTitle = stationAccountRows ? `${scope.names.join('、')}${scope.legacy ? `${scope.names.length ? '；' : ''}含旧版未限定站点的 API` : ''}` : '从本机 API 设置读取';
      return `<div class="remote-station-row"><div class="remote-station-name-wrap"><div class="remote-station-name" title="${esc(row.hubUrl)}">${esc(row.name || row.hubUrl)}<span class="remote-station-current ${current ? '' : 'is-inactive'}">${current ? '当前启用' : '已保存，未启用'}</span></div><small class="remote-station-api-scope" title="${esc(scopeTitle)}">${esc(scopeText)}</small></div><div class="account-remote-support">${badges}</div><button class="btn sm ghost" data-act="remoteStationProbe" data-hub="${esc(row.hubUrl)}" ${stationBusy ? 'disabled' : ''}>${waiting ? '处理中' : '检测'}</button>${!official || ready ? `<button class="btn sm" data-act="remoteStationSelect" data-hub="${esc(row.hubUrl)}" ${stationBusy || !official && current ? 'disabled' : ''}>${official ? '选择' : current ? '已启用' : '启用中转站'}</button>` : '<span class="remote-station-unavailable">暂不支持</span>'}</div>`;
    }).join('');
  }
  globalThis.remoteSavedStationsHTML = config => {
    stationAccountRows = null;
    const request = ++stationAccountRequest;
    Promise.resolve().then(() => api('/api/local/accounts')).then(local => {
      const root = $('#savedRemoteStations');
      if (request !== stationAccountRequest || S.route !== 'login' || !root) return;
      stationAccountRows = Array.isArray(local.accounts) ? local.accounts : [];
      root.innerHTML = stationRowsHTML(stations('hub'), false);
    }).catch(() => undefined);
    return `<section class="rm-sec"><div class="rm-sec-head"><h3>已保存站点</h3><span class="rm-sub">API 远程 · 账号远程</span></div><div id="savedRemoteStations" class="remote-stations">${stationRowsHTML(config.remoteConnections || [], false)}</div><div id="remoteSupportError" class="rm-error" aria-live="polite"></div></section>`;
  };
  function nextExpiry() {
    clearTimeout(expiryTimer);
    const supports = [...cache.values()].map(row => row.support).concat((S.local?.accounts || []).map(row => row.remoteSupport), stations().map(row => row.support));
    const time = Math.min(...supports.map(row => row?.freshUntil).filter(value => Number.isSafeInteger(value) && value > Date.now()));
    if (Number.isFinite(time)) expiryTimer = setTimeout(() => { sync(); }, Math.min(2147483647, Math.max(1, time - Date.now() + 1)));
  }
  function currentPicker(owner) {
    return owner && officialPicker === owner && owner.epoch === OFFICIAL_PANEL.epoch && owner.route === S.route && owner.element === $('#officialPanel');
  }
  function renderPicker(owner) {
    if (!currentPicker(owner) || !$('#officialStationChoices')) return;
    const rows = stations(), count = rows.filter(row => state(row.support, 'accountRemote') === 'supported').length;
    $('#officialStationChoices').innerHTML = `${count ? '' : '<p class="muted small">暂不支持，可检测已保存站点。</p>'}${stationRowsHTML(rows, true)}<button class="btn ghost" data-act="remoteSupportSettings">添加站点</button>`;
  }
  function sync() {
    for (const account of S.local?.accounts || []) {
      const node = document.getElementById('accountRemote-' + account.id);
      if (S.route === 'accounts' && node && node.dataset.source === account.sourceRevision && node.dataset.base === base(account.baseUrl)) node.innerHTML = accountMarks(account);
      const officialNode = document.getElementById('officialRemote-' + account.id);
      if (S.route === 'accounts' && officialNode && officialNode.dataset.source === account.sourceRevision && officialReady(account)) officialNode.innerHTML = officialMarks(account);
    }
    if (S.route === 'login' && $('#savedRemoteStations')) $('#savedRemoteStations').innerHTML = stationRowsHTML(stations('hub'), false);
    renderPicker(officialPicker);
    nextExpiry();
  }
  globalThis.syncRemoteSupport = sync;
  function applyResult(job, result) {
    if (!result || result.id !== job.account.id || result.sourceRevision !== job.account.sourceRevision || result.baseUrl !== base(job.account.baseUrl) || typeof result.ok !== 'boolean') return { support: unknown(), error: '检测结果已过期，请重试' };
    // A saved source may finish detection before the refreshed list arrives.
    // Keep its result under the full source key; rendering only reads that key.
    return { support: result.ok ? result.remoteSupport || unknown() : unknown(), error: result.ok ? '' : result.error || '暂时无法确认本站远程能力' };
  }
  function pump() {
    while (active < 2 && queue.length) {
      const job = queue.shift(); active++;
      Promise.resolve().then(() => api('/api/local/accounts/remote-support', { id: job.account.id, expectedSourceRevision: job.account.sourceRevision }))
        .then(result => applyResult(job, result)).catch(() => ({ support: unknown(), error: '检测失败，请刷新账号后重试' }))
        .then(result => {
          if (result) { cache.delete(job.key); cache.set(job.key, result); trimCache(); }
          job.resolve(result);
        }).finally(() => { active--; pending.delete(job.key); sync(); pump(); });
    }
  }
  globalThis.queueAccountRemoteProbe = (account, force = false) => {
    const key = accountKey(account);
    if (!key) return Promise.resolve(null);
    if (pending.has(key)) return pending.get(key);
    if (!force && attempted.has(key)) return Promise.resolve(cache.get(key));
    attempted.add(key);
    let resolve;
    const promise = new Promise(done => { resolve = done; });
    pending.set(key, promise); queue.push({ account: { ...account }, key, resolve }); sync(); pump();
    return promise;
  };
  ACTIONS.accountRemoteProbe = async button => {
    if (button?.isConnected === false) return;
    const account = (S.local?.accounts || []).find(row => row.id === button.dataset.id);
    if (!accountKey(account) || button.dataset.source !== account.sourceRevision || button.dataset.base !== base(account.baseUrl)) return;
    await queueAccountRemoteProbe(account, true);
  };
  ACTIONS.officialRemoteStations = async button => {
    if (button?.isConnected === false) return;
    const account = (S.local?.accounts || []).find(row => row.id === button.dataset.id);
    if (!officialReady(account)) throw new Error('请刷新账号列表后重试');
    officialPanel('账号远程', '<div id="officialStationChoices" class="remote-stations"><p class="muted small">正在读取…</p></div>');
    const owner = officialPicker = { id: account.id, source: account.sourceRevision, epoch: OFFICIAL_PANEL.epoch, route: S.route, element: $('#officialPanel') };
    const revision = S.localAccountsRevision = (S.localAccountsRevision || 0) + 1;
    try {
      const local = await api('/api/local/accounts');
      if (!currentPicker(owner)) return;
      if (revision !== S.localAccountsRevision) {
        if ($('#officialError')) $('#officialError').textContent = '账号状态已更新，请重新打开站点选择';
        return;
      }
      const latest = (local.accounts || []).find(row => row.id === owner.id);
      if (!officialReady(latest) || latest.sourceRevision !== owner.source) throw new Error('账号已更改，请刷新列表后重试');
      S.local = local; renderPicker(owner);
    } catch (e) { if (revision === S.localAccountsRevision && currentPicker(owner) && $('#officialError')) $('#officialError').textContent = e.message; }
  };
  function surface(button) {
    if (button?.isConnected === false) return null;
    if (currentPicker(officialPicker) && button.closest?.('#officialStationChoices') === $('#officialStationChoices')) return { official: true, owner: officialPicker };
    const element = $('#savedRemoteStations');
    if (S.route === 'login' && element && button.closest?.('#savedRemoteStations') === element) return { official: false, element };
    return null;
  }
  const currentSurface = owner => owner && (owner.official ? currentPicker(owner.owner) : S.route === 'login' && $('#savedRemoteStations') === owner.element);
  async function updateStations(owner) {
    const revision = S.localAccountsRevision = (S.localAccountsRevision || 0) + 1;
    const local = await api('/api/local/accounts');
    if (!currentSurface(owner) || revision !== S.localAccountsRevision) return;
    S.local = local;
    stationAccountRows = Array.isArray(local.accounts) ? local.accounts : [];
    if (S.state?.config) S.state.config.remoteConnections = local.remoteStations || [];
    if (S.route === 'accounts') renderLocalCards();
    sync();
  }
  async function stationAction(button, connect) {
    const owner = surface(button), row = stations(owner?.official ? '' : 'hub').find(row => row.hubUrl === button.dataset.hub);
    if (!owner || !row || stationBusy || connect && owner.official && state(row.support, 'accountRemote') !== 'supported') return;
    const lease = rmBeginConnectionAction(button, 'station', [], () => currentSurface(owner));
    if (!lease) return;
    stationBusy = row.hubUrl; button.disabled = true;
    const errorNode = owner.official ? $('#officialError') : $('#remoteSupportError');
    if (errorNode) errorNode.textContent = '';
    stationUncertain.add(row.hubUrl); sync();
    try {
      const root = new URL(row.hubUrl).origin;
      if (!lease.current()) return;
      lease.submitted = connect;
      if (connect && !owner.official) {
        const result = await api('/api/remote/mode', { mode: 'hub', hubUrl: row.hubUrl });
        if (result?.ok !== true || result.activeMode !== 'hub') throw new Error('服务器未确认连接方式，请刷新状态后核对');
      } else await api('/api/remote/discover', { url: root, hubUrl: row.hubUrl, connect, requireOfficial: connect && owner.official });
      let refreshed = null;
      if (connect) { S.pairQR = ''; S.pairExpires = 0; stationUncertain.delete(row.hubUrl); refreshed = await rmRefreshAcceptedConnection(); }
      if (!lease.current()) return;
      if (connect) {
        if (owner.official) closeOfficialPanel();
        location.hash = 'login';
        if (S.route === 'login' && refreshed) await route();
        toast('连接设置已保存，中转站已启用，正在连接', 'ok');
      } else { await updateStations(owner); stationUncertain.delete(row.hubUrl); sync(); }
    } catch (e) {
      if (connect) await rmRefreshAcceptedConnection();
      if (!lease.current()) return;
      try { await updateStations(owner); } catch (_) { /* Keep detection failure visibly unknown below. */ }
      if (!lease.current()) return;
      const node = owner.official ? $('#officialError') : $('#remoteSupportError');
      rmConnectionError(lease, node, e);
    } finally { stationBusy = ''; rmFinishConnectionAction(lease); if (button.isConnected !== false) button.disabled = false; sync(); }
  }
  ACTIONS.remoteStationProbe = button => stationAction(button, false);
  ACTIONS.remoteStationSelect = button => stationAction(button, true);
  ACTIONS.remoteSupportSettings = () => { closeOfficialPanel(); location.hash = 'login'; };
  document.addEventListener('keydown', event => {
    if (event.key !== 'Escape' || !currentPicker(officialPicker)) return;
    event.preventDefault(); event.stopImmediatePropagation(); closeOfficialPanel();
  }, true);
})();
