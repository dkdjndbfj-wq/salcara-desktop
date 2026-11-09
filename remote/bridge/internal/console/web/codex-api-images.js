(() => {
  let loadingEpoch = -1;
  let writing = false, observationSequence = 0;
  const labels = { disabled: '关闭', enabled: '文字和图片', images_only: '仅图片' };
  const current = (epoch, parent) => epoch === OFFICIAL_PANEL.epoch && parent?.isConnected && $('#apiServiceAdvanced') === parent;
  function showEffective(state) {
    const node = $('#apiImageEffective');
    if (!node) return;
    const effective = labels[state.effectiveMode] || '待核对';
    const concurrency = Number.isInteger(state.effectiveMaxConcurrentRequests) ? ` · 并发 ${state.effectiveMaxConcurrentRequests}` : '';
    node.innerHTML = `<b>当前生效：${effective}${concurrency}</b><span>已保存：${labels[state.mode] || '未确认'} · 并发 ${Number.isInteger(state.maxConcurrentRequests) ? state.maxConcurrentRequests : '未确认'}</span>${state.runtimeRestricted ? '<span class="danger-text">当前限制与已保存设置不同；请核对后重新保存。</span>' : ''}${state.requiresStart ? '<span>本次服务尚未启动。</span>' : ''}<span>${state.officialExecutorAvailable === true ? '官方图片执行器已就绪' : state.officialExecutorAvailable === false ? '官方图片执行器未就绪；上游 API 的图片能力由对应服务提供。' : '官方图片执行器状态待核对'}</span>`;
  }
  globalThis.refreshAPIImageObservation = async () => {
    const parent = $('#apiServiceAdvanced'), epoch = OFFICIAL_PANEL.epoch;
    if (!parent || !$('#apiImageEffective') || writing) return;
    const sequence = ++observationSequence;
    try {
      const state = await api('/api/local/codex-api/images');
      if (current(epoch, parent) && sequence === observationSequence && !writing) showEffective(state);
    } catch (_) {
      if (current(epoch, parent) && sequence === observationSequence && $('#apiImageEffective')) $('#apiImageEffective').textContent = '当前生效状态暂时无法核对，请刷新重试；表单内容已保留。';
    }
  };
  async function renderImages() {
    const parent = $('#apiServiceAdvanced');
    if (!parent || !parent.childElementCount || parent.querySelector('#apiImageSettings')) return;
    const epoch = OFFICIAL_PANEL.epoch;
    if (loadingEpoch === epoch) return;
    loadingEpoch = epoch;
    try {
      const state = await api('/api/local/codex-api/images');
      if (epoch !== OFFICIAL_PANEL.epoch || !parent.isConnected || $('#apiServiceAdvanced') !== parent || parent.querySelector('#apiImageSettings')) return;
      parent.insertAdjacentHTML('beforeend', `<section id="apiImageSettings" class="api-settings-section"><div class="row"><b>图片服务</b><span class="spacer"></span><button class="icon-btn" data-act="codexAPIImagesRefresh" title="核对图片生效状态" aria-label="核对图片生效状态">${icon('refresh')}</button></div><div id="apiImageEffective" class="api-effective-state" role="status"></div><div class="form-grid"><div class="field"><label for="apiImageMode">图片服务</label><select id="apiImageMode" class="input"><option value="disabled" ${state.mode === 'disabled' ? 'selected' : ''}>关闭</option><option value="enabled" ${state.mode === 'enabled' ? 'selected' : ''}>文字和图片</option><option value="images_only" ${state.mode === 'images_only' ? 'selected' : ''}>仅图片</option></select></div><div class="field"><label for="apiImageConcurrency">每个上游凭据的并发上限</label><input id="apiImageConcurrency" class="input" type="number" min="1" max="16" value="${Number.isInteger(state.maxConcurrentRequests) ? state.maxConcurrentRequests : 1}"></div></div><p class="muted small">生成和编辑使用独立队列；等待中的任务可取消。图片额度由上游决定。</p><button class="btn sm" data-act="codexAPIImagesSave">保存图片设置</button><div id="apiImageStatus" class="muted small" role="status"></div></section>`);
      showEffective(state);
      if (typeof rememberAPISettings === 'function') rememberAPISettings('apiImageSettings');
    } catch (error) {
      if (epoch === OFFICIAL_PANEL.epoch && parent.isConnected && $('#apiServiceAdvanced') === parent && !parent.querySelector('#apiImageSettings')) parent.insertAdjacentHTML('beforeend', '<div id="apiImageSettings" class="field"><div class="muted small">图片设置读取失败。</div><button class="btn" data-act="codexAPIImagesRetry">重试</button></div>');
    } finally { if (loadingEpoch === epoch) loadingEpoch = -1; }
  }
  ACTIONS.codexAPIImagesRetry = async () => { $('#apiImageSettings')?.remove(); await renderImages(); };
  ACTIONS.codexAPIImagesRefresh = button => busy(button, refreshAPIImageObservation);
  ACTIONS.codexAPIImagesSave = async (button) => {
    if (writing) return;
    const epoch = OFFICIAL_PANEL.epoch;
    const parent = $('#apiServiceAdvanced');
    const modeInput = $('#apiImageMode'), concurrencyInput = $('#apiImageConcurrency'), status = $('#apiImageStatus');
    const mode = modeInput?.value;
    const maxConcurrentRequests = Number(concurrencyInput?.value);
    if (!['disabled', 'enabled', 'images_only'].includes(mode) || !Number.isInteger(maxConcurrentRequests) || maxConcurrentRequests < 1 || maxConcurrentRequests > 16) throw new Error('图片并发上限需要在 1–16 之间');
    if (status) status.textContent = '';
    const owns = () => current(epoch, parent) && status?.isConnected && $('#apiImageStatus') === status && $('#apiImageMode') === modeInput && $('#apiImageConcurrency') === concurrencyInput;
    writing = true; observationSequence++;
    const owner = apiClientPanel; if (owner) owner.settingsWriting = (owner.settingsWriting || 0) + 1;
    try {
      const state = await busy(button, () => api('/api/local/codex-api/images', { mode, maxConcurrentRequests }));
      if (!owns()) return;
      showEffective(state);
      if (typeof rememberAPISettings === 'function') rememberAPISettings('apiImageSettings', { apiImageMode: mode, apiImageConcurrency: String(maxConcurrentRequests) });
      status.textContent = modeInput.value === mode && Number(concurrencyInput.value) === maxConcurrentRequests ? '图片设置已保存' : '设置已更改，尚未保存';
    } catch (error) {
      if (!owns()) return;
      status.textContent = `${error.message}；正在核对实际生效状态。`;
      try {
        const latest = await api('/api/local/codex-api/images');
        if (!owns()) return;
        showEffective(latest);
        status.textContent = `${error.message}；已核对当前生效状态，表单内容已保留，可修正后重试。`;
      } catch (_) {
        if (owns()) status.textContent = `${error.message}；当前生效状态未能确认，请点刷新核对。表单内容已保留。`;
      }
    } finally { writing = false; if (owner) owner.settingsWriting--; }
  };
  function clearSavedImageStatus(event) {
    if (['apiImageMode', 'apiImageConcurrency'].includes(event.target?.id) && $('#apiImageStatus')) $('#apiImageStatus').textContent = '';
  }
  document.addEventListener('input', clearSavedImageStatus);
  document.addEventListener('change', clearSavedImageStatus);
  new MutationObserver(() => { void renderImages(); }).observe(document.body, { childList: true, subtree: true });
})();
