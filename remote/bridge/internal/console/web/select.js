'use strict';
/* Styled drop-downs. Every <select> in the console keeps working as before (its
   value and "change" events are what the page logic uses); it is visually
   replaced by a button and a floating list that match the rest of the app.
   A select with data-native keeps the system look. */

(function () {
  const OPEN = { select: null, menu: null, button: null };

  function label(select) {
    const option = select.options[select.selectedIndex];
    return option ? option.textContent.trim() : '';
  }

  function sync(select) {
    const button = select._csButton;
    if (!button) return;
    button.querySelector('.cs-label').textContent = label(select) || '请选择';
    button.disabled = select.disabled;
    button.classList.toggle('placeholder', !select.value);
  }

  function close() {
    if (OPEN.menu) OPEN.menu.remove();
    if (OPEN.button) { OPEN.button.setAttribute('aria-expanded', 'false'); OPEN.button.classList.remove('open'); }
    OPEN.select = OPEN.menu = OPEN.button = null;
  }

  function choose(select, value) {
    close();
    if (select.value === value) { select._csButton.focus(); return; }
    select.value = value;
    sync(select);
    select.dispatchEvent(new Event('change', { bubbles: true }));
    // Page code may reset the value (for example "add a key…"); reflect it.
    requestAnimationFrame(() => sync(select));
    select._csButton.focus();
  }

  function open(select) {
    if (OPEN.select === select) { close(); return; }
    close();
    const button = select._csButton;
    const menu = document.createElement('div');
    menu.className = 'cs-menu';
    menu.setAttribute('role', 'listbox');
    menu.setAttribute('aria-label', select.getAttribute('aria-label') || '选项');
    [...select.options].forEach((option, index) => {
      const item = document.createElement('button');
      item.type = 'button';
      item.className = 'cs-item';
      item.setAttribute('role', 'option');
      item.setAttribute('aria-selected', String(index === select.selectedIndex));
      item.disabled = option.disabled;
      if (option.value === '__new' || option.value.startsWith('__')) item.classList.add('action');
      item.innerHTML = '<span class="cs-check"></span><span class="cs-text"></span>';
      item.querySelector('.cs-text').textContent = option.textContent.trim();
      item.addEventListener('click', () => choose(select, option.value));
      menu.appendChild(item);
    });
    document.body.appendChild(menu);
    const r = button.getBoundingClientRect();
    const height = Math.min(menu.scrollHeight, 320);
    const below = innerHeight - r.bottom - 12;
    const top = below >= Math.min(height, 200) || below >= r.top ? r.bottom + 6 : Math.max(12, r.top - 6 - height);
    Object.assign(menu.style, { left: `${Math.max(12, Math.min(r.left, innerWidth - Math.max(r.width, 220) - 12))}px`, top: `${top}px`, minWidth: `${Math.max(r.width, 220)}px`, maxHeight: `${Math.min(320, Math.max(below, r.top - 18))}px` });
    OPEN.select = select; OPEN.menu = menu; OPEN.button = button;
    button.setAttribute('aria-expanded', 'true');
    button.classList.add('open');
    (menu.querySelector('[aria-selected="true"]') || menu.querySelector('.cs-item:not(:disabled)'))?.focus();
  }

  function enhance(select) {
    if (select._csButton || select.hasAttribute('data-native') || select.multiple) return;
    const button = document.createElement('button');
    button.type = 'button';
    button.className = 'cs-btn ' + (select.className || '').replace(/\binput\b/, '').trim();
    button.setAttribute('aria-haspopup', 'listbox');
    button.setAttribute('aria-expanded', 'false');
    if (select.getAttribute('aria-label')) button.setAttribute('aria-label', select.getAttribute('aria-label'));
    button.innerHTML = '<span class="cs-label"></span><svg class="cs-chev" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="m4.5 6.5 3.5 3.5 3.5-3.5"/></svg>';
    button.addEventListener('click', (e) => { e.preventDefault(); e.stopPropagation(); if (!select.disabled) open(select); });
    button.addEventListener('keydown', (e) => {
      if (['ArrowDown', 'ArrowUp', 'Enter', ' '].includes(e.key)) { e.preventDefault(); if (!select.disabled) open(select); }
    });
    select._csButton = button;
    select.classList.add('cs-native');
    select.tabIndex = -1;
    select.setAttribute('aria-hidden', 'true');
    select.insertAdjacentElement('afterend', button);
    select.addEventListener('change', () => sync(select));
    new MutationObserver(() => sync(select)).observe(select, { attributes: true, childList: true, subtree: true });
    sync(select);
  }

  function scan(root) {
    if (root.tagName === 'SELECT') enhance(root);
    root.querySelectorAll?.('select').forEach(enhance);
  }

  // Keyboard inside the open list.
  document.addEventListener('keydown', (e) => {
    if (!OPEN.menu) return;
    const items = [...OPEN.menu.querySelectorAll('.cs-item:not(:disabled)')];
    const index = items.indexOf(document.activeElement);
    if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); const b = OPEN.button; close(); b?.focus(); }
    else if (e.key === 'ArrowDown') { e.preventDefault(); items[Math.min(items.length - 1, index + 1)]?.focus(); }
    else if (e.key === 'ArrowUp') { e.preventDefault(); items[Math.max(0, index - 1)]?.focus(); }
    else if (e.key === 'Tab') close();
  }, true);
  document.addEventListener('mousedown', (e) => { if (OPEN.menu && !OPEN.menu.contains(e.target) && !OPEN.button.contains(e.target)) close(); }, true);
  addEventListener('resize', close);
  document.addEventListener('scroll', (e) => { if (OPEN.menu && !OPEN.menu.contains(e.target)) close(); }, true);
  addEventListener('hashchange', close);

  new MutationObserver((records) => {
    for (const record of records) record.addedNodes.forEach((node) => { if (node.nodeType === 1) scan(node); });
  }).observe(document.body, { childList: true, subtree: true });
  scan(document.body);
})();
