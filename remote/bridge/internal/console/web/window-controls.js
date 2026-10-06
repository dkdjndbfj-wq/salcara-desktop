'use strict';
// Bind before the page renderers. A failed tool request or renderer must never
// prevent the user from minimizing or closing the window.
(() => {
  const win = window.salcaraWindow;
  if (!win) return;
  document.body.classList.add('framed');
  document.getElementById('winCtl').hidden = false;
  document.getElementById('winMin').addEventListener('click', () => win.minimize());
  document.getElementById('winClose').addEventListener('click', () => win.close());
})();
