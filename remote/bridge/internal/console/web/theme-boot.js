// Applies the saved appearance before the page paints (follow system / light / dark),
// and follows changes made in another window (the main window and the floating ball).
(function () {
  function apply(t) {
    if (t === 'light' || t === 'dark') document.documentElement.setAttribute('data-theme', t);
    else document.documentElement.removeAttribute('data-theme');
  }
  try { apply(localStorage.getItem('salcara.theme')); } catch (_) { /* storage unavailable: follow the system */ }
  window.addEventListener('storage', function (e) { if (e.key === 'salcara.theme') apply(e.newValue); });
})();
