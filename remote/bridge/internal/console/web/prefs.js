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
// Tell the desktop shell the saved choice once, so its start-up page matches next time.
try { const t = localStorage.getItem('salcara.theme'); window.salcaraWindow?.setTheme?.(t === 'light' || t === 'dark' ? t : 'system'); } catch (_) { /* optional */ }
function currentTheme() {
  const t = document.documentElement.getAttribute('data-theme');
  return t === 'light' || t === 'dark' ? t : 'system';
}

function prefsCard() {
  const theme = currentTheme();
  const seg = [['system', '跟随系统'], ['light', '浅色'], ['dark', '深色']]
    .map(([v, l]) => `<button type="button" data-theme-choice="${v}" aria-pressed="${theme === v}" class="${theme === v ? 'on' : ''}">${l}</button>`).join('');
  const notifyOn = typeof NOTIFY !== 'undefined' ? NOTIFY.enabled() : true;
  return `<div class="card" id="prefsCard">
    <h3 class="card-title" style="margin-bottom:14px">${ico('eye')}外观与提醒</h3>
    <div class="setting-row"><div class="txt"><div class="t">外观</div><div class="d">深色模式可以跟随系统自动切换</div></div>
      <div class="pref-seg" role="group" aria-label="外观">${seg}</div></div>
    <div class="setting-row"><div class="txt"><div class="t">系统通知</div><div class="d">窗口不在前台时，有操作等你批准、或任务完成 / 出错时弹出通知</div></div>
      <label class="switch"><input type="checkbox" id="swNotify" ${notifyOn ? 'checked' : ''}><span></span></label></div>
    <div id="updateRow"></div>
  </div>`;
}

async function renderUpdateRow() {
  const row = $('#updateRow');
  const shell = window.salcaraWindow;
  if (!row || !shell || !shell.updateStatus) return;
  let st;
  try { st = await shell.updateStatus(); } catch (_) { return; }
  if (!st) return;
  const has = st.update && st.update.version;
  const ready = st.phase === 'ready', busy = st.phase === 'downloading' || st.phase === 'verifying';
  const desc = !st.configured ? '这个版本还没有配置更新源，请到官网下载新版'
    : ready ? `新版本 <b>${esc(st.update.version)}</b> 已下载好，重启即可完成更新`
    : busy ? `正在下载新版本 <b>${esc(st.update.version)}</b>…`
    : has ? `新版本 <b>${esc(st.update.version)}</b> 已发布`
    : '每 6 小时自动检查一次；有新版本会弹出更新窗口';
  row.innerHTML = `<div class="setting-row"><div class="txt"><div class="t">版本更新 <span class="muted small">当前 ${esc(st.current || '')}</span></div>
      <div class="d">${desc}</div></div>
    <div class="row" style="gap:8px">${has ? `<button class="btn sm primary" type="button" id="openUpdate">${icon('download')}${ready ? '重启并更新' : busy ? '查看进度' : '查看更新'}</button>` : ''}
      ${st.configured && !has ? `<button class="btn sm" type="button" id="checkUpdate">${icon('refresh')}检查更新</button>` : ''}</div></div>`;
  $('#openUpdate')?.addEventListener('click', () => shell.openUpdate());
  $('#checkUpdate')?.addEventListener('click', async (e) => {
    const b = e.currentTarget; b.disabled = true; b.innerHTML = '<span class="spin"></span>检查中';
    try {
      const r = await shell.checkUpdate();
      if (r && r.phase === 'latest') toast('已经是最新版本', 'ok');
    } catch (_) { toast('检查更新失败', 'bad'); }
    renderUpdateRow();
  });
}

(function wrapSettings() {
  const base = RENDER_settings;
  RENDER_settings = async function (view) {
    await base(view);
    const anchor = view.querySelector('.set-link');
    const holder = document.createElement('div');
    holder.innerHTML = prefsCard();
    const card = holder.firstElementChild;
    if (anchor) anchor.insertAdjacentElement('afterend', card); else view.prepend(card);
    card.querySelectorAll('[data-theme-choice]').forEach((b) => b.addEventListener('click', () => {
      applyTheme(b.dataset.themeChoice);
      card.querySelectorAll('[data-theme-choice]').forEach((x) => { const on = x === b; x.classList.toggle('on', on); x.setAttribute('aria-pressed', String(on)); });
    }));
    $('#swNotify', card).addEventListener('change', (e) => {
      if (typeof NOTIFY !== 'undefined') NOTIFY.setEnabled(e.target.checked);
      if (e.target.checked && !window.salcaraWindow && 'Notification' in window && Notification.permission === 'default') Notification.requestPermission().catch(() => {});
    });
    renderUpdateRow();
  };
})();
