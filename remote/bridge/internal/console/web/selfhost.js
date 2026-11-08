'use strict';

/* Self-hosted Hub controls live alongside direct pairing in the remote page. */
const SELFHOST = {
  seq: 0,
  requestSeq: 0,
  hub: null,
  error: '',
  poll: null,
  draft: null,
  mutating: false,
  view: null,
  pageEpoch: 0,
  open: false,
};

function selfhostStatusLabel(running, stopping) { return stopping ? '停止中' : running ? '运行中' : '未启动'; }
function selfhostStatusClass(running, stopping) { return stopping ? 'warn' : running ? 'ok' : 'muted'; }

function selfhostIsActive(view = SELFHOST.view, pageEpoch = SELFHOST.pageEpoch, seq = SELFHOST.seq) {
  return seq === SELFHOST.seq && pageEpoch === SELFHOST.pageEpoch && S.route === 'login'
    && RM.remoteMode === 'direct' && view?.isConnected !== false;
}

function selfhostStatusKey(hub) {
  if (!hub) return '';
  return JSON.stringify({
    running: !!hub.running,
    stopping: !!hub.stopping,
    addr: hub.addr || '',
    mode: hub.mode || '',
    revision: hub.revision || '',
    bases: Array.isArray(hub.bases) ? hub.bases : [],
    devices: hub.devices ?? 0,
    phones: hub.phones ?? 0,
  });
}

function selfhostHubMarkup() {
  const hub = SELFHOST.hub || {};
  const running = !!hub.running;
  const stopping = !!hub.stopping;
  const hubBusy = running || stopping || SELFHOST.mutating;
  const draft = SELFHOST.draft || {};
  const mode = hub.mode || draft.mode || 'restricted';
  const addr = hub.addr || draft.addr || '127.0.0.1:8765';
  const revision = hub.revision || draft.revision || 'self-hosted-v1';
  const bases = hub.bases || draft.bases || [];
  return `<section class="rm-sec sh-hub-panel" id="shHubPanel">
    <details class="sh-hub-details" ${SELFHOST.open ? 'open' : ''}>
      <summary class="sh-hub-summary"><span><b>本机自托管 Hub</b><small>本地中继服务设置</small></span><span class="sh-state ${selfhostStatusClass(running, stopping)}"><i></i>${selfhostStatusLabel(running, stopping)}</span></summary>
      <div class="sh-hub-body">
        ${SELFHOST.error ? `<div class="callout warn">${ico('warn')}<div>Hub 状态读取失败：${esc(SELFHOST.error)}</div></div>` : ''}
        <div class="callout">${ico('info')}<div>内置 Hub 仅监听本机回环地址。供手机访问时，需由可信 HTTPS 反向代理转发。</div></div>
        <div class="sh-grid">
          <label class="field"><span>本机监听地址</span><input class="input" id="shHubAddr" value="${esc(addr)}" placeholder="127.0.0.1:8765" ${hubBusy ? 'disabled' : ''}></label>
          <label class="field"><span>访问策略</span><select class="input" id="shHubMode" ${hubBusy ? 'disabled' : ''}><option value="open"${mode === 'open' ? ' selected' : ''}>开放（允许所有本地 API）</option><option value="restricted"${mode === 'restricted' ? ' selected' : ''}>受限（仅允许指定 API）</option></select></label>
          <label class="field"><span>策略版本</span><input class="input" id="shHubRevision" value="${esc(revision)}" ${hubBusy ? 'disabled' : ''}></label>
          <label class="field"><span>允许的 API 地址（受限模式每行一个）</span><textarea class="input mono" id="shHubBases" rows="2" ${hubBusy ? 'disabled' : ''}>${esc(bases.join('\n'))}</textarea></label>
        </div>
        <div class="sh-actions"><button class="btn primary" data-act="selfhostHubStart" ${hubBusy ? 'disabled' : ''}>${icon('play')}启动 Hub</button><button class="btn danger" data-act="selfhostHubStop" ${running && !stopping && !SELFHOST.mutating ? '' : 'disabled'}>${icon('stop')}停止 Hub</button><button class="btn" data-act="selfhostRefresh">${icon('refresh')}刷新</button><button class="btn sm" data-act="selfhostTutorial">使用教程</button></div>
        <p class="muted small">${running ? `本机地址：<code>${esc(hub.addr || '')}</code> · 设备 ${esc(hub.devices ?? 0)} · 手机连接 ${esc(hub.phones ?? 0)}` : stopping ? '正在关闭 Hub，请稍候。' : 'Hub 与上方局域网 / VPN 直连是两种独立连接。'}</p>
      </div>
    </details>
  </section>`;
}

function selfhostRememberDraft() {
  const panel = document.getElementById('shHubPanel');
  const details = panel?.querySelector?.('.sh-hub-details');
  if (details) SELFHOST.open = !!details.open;
  const addrInput = $('#shHubAddr'), modeInput = $('#shHubMode'), revisionInput = $('#shHubRevision'), basesInput = $('#shHubBases');
  if (addrInput || modeInput || revisionInput || basesInput) {
    SELFHOST.draft = {
      addr: addrInput?.value || SELFHOST.draft?.addr,
      mode: modeInput?.value || SELFHOST.draft?.mode,
      revision: revisionInput?.value || SELFHOST.draft?.revision,
      bases: basesInput ? String(basesInput.value || '').split(/\r?\n/).map(v => v.trim()).filter(Boolean) : SELFHOST.draft?.bases,
    };
  }
}

function selfhostRender(view = SELFHOST.view, pageEpoch = SELFHOST.pageEpoch, seq = SELFHOST.seq) {
  if (!selfhostIsActive(view, pageEpoch, seq) || SELFHOST.mutating) return;
  const panel = document.getElementById('shHubPanel');
  if (!panel || panel.isConnected === false) return;
  selfhostRememberDraft();
  const active = typeof document !== 'undefined' ? document.activeElement : null;
  if (active && panel.contains?.(active) && /^(INPUT|SELECT|TEXTAREA)$/.test(active.tagName)) return;
  panel.outerHTML = selfhostHubMarkup();
}

async function selfhostLoad(view = SELFHOST.view, pageEpoch = SELFHOST.pageEpoch, seq = SELFHOST.seq) {
  const requestSeq = ++SELFHOST.requestSeq;
  let changed = false;
  try {
    const hub = await api('/api/local/hub/status');
    if (!selfhostIsActive(view, pageEpoch, seq) || requestSeq !== SELFHOST.requestSeq) return;
    changed = !!SELFHOST.error || selfhostStatusKey(SELFHOST.hub) !== selfhostStatusKey(hub);
    SELFHOST.hub = hub;
    SELFHOST.error = '';
  } catch (error) {
    if (!selfhostIsActive(view, pageEpoch, seq) || requestSeq !== SELFHOST.requestSeq) return;
    const message = error?.message || 'Hub 状态暂不可用';
    changed = SELFHOST.error !== message;
    SELFHOST.error = message;
  }
  if (changed) selfhostRender(view, pageEpoch, seq);
}

function selfhostAttach(view, pageEpoch) {
  clearInterval(SELFHOST.poll);
  SELFHOST.poll = null;
  SELFHOST.seq++;
  SELFHOST.requestSeq++;
  SELFHOST.view = view;
  SELFHOST.pageEpoch = pageEpoch;
  if (RM.remoteMode !== 'direct') return;
  const seq = SELFHOST.seq;
  void selfhostLoad(view, pageEpoch, seq);
  SELFHOST.poll = setInterval(() => {
    if (!selfhostIsActive(view, pageEpoch, seq)) {
      clearInterval(SELFHOST.poll);
      SELFHOST.poll = null;
      return;
    }
    void selfhostLoad(view, pageEpoch, seq);
  }, 5000);
}

function selfhostDetach() {
  selfhostRememberDraft();
  clearInterval(SELFHOST.poll);
  SELFHOST.poll = null;
  SELFHOST.seq++;
  SELFHOST.requestSeq++;
  SELFHOST.view = null;
}

function renderSelfhostHubPanel() { return selfhostHubMarkup(); }

Object.assign(ACTIONS, {
  selfhostRefresh: async () => { await selfhostLoad(); },
  selfhostTutorial: () => {
    $('#modalRoot').innerHTML = `<div class="modal-back" data-act="closeModal"><div class="modal account-modal" data-stop role="dialog" aria-modal="true" aria-labelledby="selfhostTutorialTitle"><div class="modal-head"><h3 id="selfhostTutorialTitle">本机自托管 Hub</h3><button class="icon-btn" data-act="closeModal" aria-label="关闭">${icon('close')}</button></div><div class="modal-body stack"><ol><li>在这里配置并启动 Hub。内置 Hub 只监听电脑本机的回环地址。</li><li>要让手机从外网访问，需要在可信 HTTPS 反向代理后发布该 Hub，再从“中转站”页连接公开地址。</li><li>如果只在同一局域网或 VPN 内使用、不经过 Hub，请使用本页上方的“电脑直连”。</li></ol><div class="callout warn">自托管 Hub 与电脑直连是两条独立连接；不会自动互相切换。</div></div><div class="modal-foot"><button class="btn" data-act="closeModal">关闭</button></div></div></div>`;
  },
  selfhostHubStart: async (button) => {
    const addr = ($('#shHubAddr') || {}).value?.trim() || '127.0.0.1:8765';
    const mode = ($('#shHubMode') || {}).value || 'restricted';
    const revision = ($('#shHubRevision') || {}).value?.trim() || 'self-hosted-v1';
    const bases = String(($('#shHubBases') || {}).value || '').split(/\r?\n/).map((v) => v.trim()).filter(Boolean);
    if (mode === 'restricted' && bases.length === 0) throw new Error('受限模式至少要填写一个允许的 API 地址');
    if (mode === 'open' && !await confirm('开放模式会允许所有 API 账号通过此 Hub 使用。如果之后通过 HTTPS 入口开放访问，该站点内已配对手机都可能使用这些账号。确定要继续吗？')) return;
    const startBases = mode === 'restricted' ? bases : [];
    SELFHOST.draft = { addr, mode, revision, bases: startBases };
    if (SELFHOST.mutating) return;
    SELFHOST.mutating = true;
    try {
      await busy(button, async () => {
        await api('/api/local/hub/start', { addr, mode, revision, bases: startBases, confirmOpen: mode === 'open' });
        toast('自托管 Hub 已启动', 'ok');
      });
    } finally { SELFHOST.mutating = false; }
    await selfhostLoad();
  },
  selfhostHubStop: async (button) => {
    if (!await confirm('停止自托管 Hub？手机将暂时无法通过此 Hub 连接。')) return;
    if (SELFHOST.mutating) return;
    SELFHOST.mutating = true;
    try {
      await busy(button, async () => {
        await api('/api/local/hub/stop', {});
        toast('自托管 Hub 已停止', 'ok');
      });
    } finally { SELFHOST.mutating = false; }
    await selfhostLoad();
  },
});
