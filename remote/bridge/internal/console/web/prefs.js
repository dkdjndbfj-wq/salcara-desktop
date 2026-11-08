'use strict';
/* 设置 → 外观、系统通知、版本更新. Appearance and notifications are per
   computer conveniences kept in this page's storage; updates come from the
   desktop shell (absent when the console is opened in a browser). */

function applyTheme(mode) {
  const root = document.documentElement;
  if (mode === 'light' || mode === 'dark') root.setAttribute('data-theme', mode); else root.removeAttribute('data-theme');
  try { if (mode === 'light' || mode === 'dark') localStorage.setItem('salcara.theme', mode); else localStorage.removeItem('salcara.theme'); } catch (_) { /* optional */ }
  try { window.salcaraWindow?.setTheme?.(mode === 'light' || mode === 'dark' ? mode : 'system'); } catch (_) { /* browser */ }
}
// One light appearance: tell the desktop shell, so its start-up page matches as well.
try { window.salcaraWindow?.setTheme?.('light'); } catch (_) { /* optional */ }
function currentTheme() {
  const t = document.documentElement.getAttribute('data-theme');
  return t === 'light' || t === 'dark' ? t : 'system';
}

function prefsCard() {
  const notifyOn = typeof NOTIFY !== 'undefined' ? NOTIFY.enabled() : true;
  return `<div class="card" id="prefsCard">
    <h3 class="card-title">${ico('bolt')}提醒与更新</h3>
    <div class="setting-row"><div class="txt"><div class="t">Codex 完成提醒</div><div class="d">窗口不在前台时，Codex 主对话完成本轮任务并返回中文回复后通知</div></div>
      <label class="switch"><input type="checkbox" id="swNotify" ${notifyOn ? 'checked' : ''}><span></span></label></div>
    <div id="updateRow"></div>
  </div>`;
}

let updateRowRevision = 0, updateRowRequest = 0;
async function renderUpdateRow() {
  const row = $('#updateRow');
  const shell = window.salcaraWindow;
  if (!row || !shell || !shell.updateStatus) return;
  const request = ++updateRowRequest;
  const ownsRow = () => row.isConnected !== false && $('#updateRow') === row && S.route === 'settings';
  let st;
  try { st = await shell.updateStatus(); } catch (_) { return; }
  if (!st || request !== updateRowRequest || !ownsRow()) return;
  // A failed status refresh must leave the existing button usable for retry.
  // Only a successfully rendered replacement retires its predecessor's actions.
  const revision = ++updateRowRevision;
  const current = () => revision === updateRowRevision && ownsRow();
  const has = st.update && st.update.version;
  const ready = st.phase === 'ready', waiting = st.phase === 'waiting', updateBusy = st.phase === 'downloading' || st.phase === 'verifying' || st.phase === 'installing';
  const desc = st.required ? `当前版本不再支持发起新任务，请更新至 <b>${esc(st.update?.version || st.minimumSupportedVersion)}</b>。已有任务可继续完成。${st.error ? ' ' + esc(st.error) : ''}`
    : !st.configured ? '这个版本还没有配置更新源，请到官网下载新版'
    : st.autoBlocked || st.distributionReason ? esc(st.error)
    : st.phase === 'error' ? esc(st.error || '更新未完成，请打开更新窗口重试')
    : waiting ? '更新已下载，当前任务结束后会自动安装并重启'
    : ready ? `新版本 <b>${esc(st.update.version)}</b> 已下载好，即将自动安装并重启`
    : updateBusy ? `正在下载新版本 <b>${esc(st.update.version)}</b>…`
    : has ? `新版本 <b>${esc(st.update.version)}</b> 已发布`
    : '每 2 小时自动检查；自动下载更新，任务结束后自动安装并重启';
  row.innerHTML = `<div class="setting-row"><div class="txt"><div class="t">版本更新 <span class="muted small">当前 ${esc(st.current || '')}</span></div>
      <div class="d">${desc}</div></div>
    <div class="row" style="gap:8px">${has || !st.configured ? `<button class="btn sm primary" type="button" id="openUpdate">${icon('download')}${ready || waiting || updateBusy ? '查看进度' : st.required ? '立即更新' : '查看更新'}</button>` : ''}
      ${st.configured && !has ? `<button class="btn sm" type="button" id="checkUpdate">${icon('refresh')}检查更新</button>` : ''}</div></div>`;
  row.querySelector('#openUpdate')?.addEventListener('click', () => { if (current()) shell.openUpdate(); });
  row.querySelector('#checkUpdate')?.addEventListener('click', async (e) => {
    if (!current()) return;
    await busy(e.currentTarget, async () => {
      try {
        const r = await shell.checkUpdate();
        if (current() && r?.phase === 'latest') toast('已经是最新版本', 'ok');
      } catch (_) { if (current()) toast('检查更新失败', 'bad'); }
      if (current()) await renderUpdateRow();
    });
  });
}

(function wrapSettings() {
  const base = RENDER_settings;
  function attachSettingsPrefs(view) {
    if (!view || view.isConnected === false || S.route !== 'settings' || view.querySelector('.page-skeleton')) return;
    const existing = view.querySelector('#prefsCard');
    if (existing && typeof existing.remove === 'function') existing.remove();
    const anchor = view.querySelector('.set-link');
    const holder = document.createElement('div');
    holder.innerHTML = prefsCard();
    const card = holder.firstElementChild;
    if (anchor) anchor.insertAdjacentElement('afterend', card); else view.prepend(card);
    $('#swNotify', card).addEventListener('change', (e) => {
      if (typeof NOTIFY !== 'undefined') NOTIFY.setEnabled(e.target.checked);
      if (e.target.checked && !window.salcaraWindow && 'Notification' in window && Notification.permission === 'default') Notification.requestPermission().catch(() => {});
    });
    renderUpdateRow();
  }
  window.__salcaraAttachSettingsPrefs = attachSettingsPrefs;
  RENDER_settings = function (view) {
    const result = base(view);
    Promise.resolve(result).then(() => attachSettingsPrefs(view));
    return result;
  };
})();

// Keep the entry visible on every page, including after dismissing the popup.
(function watchDesktopUpdates() {
  const shell = window.salcaraWindow;
  if (!shell?.updateStatus) return;
  const banner = document.getElementById('desktopUpdateBanner');
  let revision = 0;
  function render(st) {
    if (!st || !banner) return;
    banner.hidden = !st.update && !st.required;
    banner.classList.toggle('required', st.required === true);
    const title = document.getElementById('desktopUpdateTitle');
    const detail = document.getElementById('desktopUpdateDetail');
    const button = document.getElementById('desktopUpdateOpen');
    title.textContent = st.autoBlocked ? '已停止重复安装失败版本' : st.distributionReason ? '等待更新开放' : st.phase === 'waiting' ? '等待任务结束后自动更新' : st.required ? '当前版本需要更新' : st.phase === 'ready' ? '即将自动安装更新' : '发现新版本';
    detail.textContent = st.autoBlocked || st.distributionReason ? st.error : st.required ? (st.policyError || '已暂停发起新任务，已有任务结束后自动安装；审批和停止仍可使用。') : `版本 ${st.update?.version || ''} · ${st.phase === 'ready' || st.phase === 'waiting' ? '任务结束后自动安装并重启' : st.phase === 'error' ? '更新未完成，点击重试' : '自动下载更新，点击查看进度'}`;
    button.textContent = ['downloading', 'verifying', 'ready', 'waiting', 'installing'].includes(st.phase) ? '查看进度' : st.required ? '立即更新' : '查看更新';
    button.onclick = () => shell.openUpdate();
    if (S.route === 'settings') void renderUpdateRow();
  }
  shell.onUpdateState?.(st => { revision++; render(st); });
  const initial = revision;
  shell.updateStatus().then(st => { if (initial === revision) render(st); }).catch(() => {});
})();
