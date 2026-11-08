'use strict';
// Plugin shortcuts share a bounded picker instead of growing the ball's ring.
(function () {
  const bridge = window.salcaraPlugins;
  if (!bridge?.uiState || !bridge?.onUIChanged) return;
  let revision = -1, entries = [], busy = new Set();
  const trigger = document.createElement('button');
  trigger.type = 'button'; trigger.title = '插件快捷入口'; trigger.setAttribute('aria-label', '插件快捷入口');
  trigger.textContent = '✦'; trigger.hidden = true; $('#sats').appendChild(trigger);
  const picker = document.createElement('section');
  picker.className = 'plugin-ball-list'; picker.hidden = true; picker.setAttribute('aria-label', '插件快捷入口');
  float.appendChild(picker);
  function placePicker() {
    const geometry = window.salcaraBallLayout;
    if (geometry) { picker.classList.toggle('opens-left', geometry.side === 'left'); picker.style.width = geometry.maxWidth + 'px'; }
  }
  window.addEventListener('ball-layout', placePicker);
  function layout() {
    const buttons = [...document.querySelectorAll('#sats button')].filter(b => !b.hidden);
    buttons.forEach((b, i) => { const angle = 360 / buttons.length * i; b.style.setProperty('--a', angle + 'deg'); b.style.setProperty('--i', i); b.dataset.side = angle > 180 ? 'l' : 'r'; });
  }
  function renderList(error = '') {
    picker.replaceChildren();
    const back = document.createElement('button'); back.type = 'button'; back.className = 'plugin-ball-back'; back.textContent = '‹ 返回快捷入口';
    back.onclick = () => { picker.hidden = true; float.classList.remove('plugin-menu'); trigger.focus(); reconcileBallPointer(); };
    picker.appendChild(back);
    for (const entry of entries) {
      const key = JSON.stringify([entry.pluginId, entry.satId]);
      const button = document.createElement('button'); button.type = 'button'; button.textContent = entry.label; button.title = entry.label; button.disabled = busy.has(key);
      button.onclick = async () => {
        if (busy.has(key)) return;
        let failure = '';
        busy.add(key); renderList();
        try { await bridge.invokeAction(entry.pluginId, 'satellite', entry.satId); setOpen(false); }
        catch (e) { failure = e.message || '插件操作未完成'; }
        finally { busy.delete(key); if (!picker.hidden) renderList(failure); }
      };
      picker.appendChild(button);
    }
    if (error) { const note = document.createElement('p'); note.textContent = error; picker.appendChild(note); }
  }
  trigger.onclick = () => { placePicker(); picker.hidden = false; float.classList.add('plugin-menu'); renderList(); picker.querySelector('button')?.focus(); };
  picker.addEventListener('keydown', closeShortcuts);
  window.closePluginBallMenu = () => { picker.hidden = true; float.classList.remove('plugin-menu'); };
  function apply(state) {
    if (!state || !Number.isSafeInteger(state.revision) || state.revision <= revision) return;
    revision = state.revision; entries = Array.isArray(state.satellites) ? state.satellites : [];
    trigger.hidden = !entries.length; layout();
    if (!entries.length) window.closePluginBallMenu(); else if (!picker.hidden) renderList();
    reconcileBallPointer();
  }
  bridge.onUIChanged(apply);
  bridge.uiState().then(apply).catch(() => {});
})();
