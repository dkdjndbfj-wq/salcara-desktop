// The desktop app ships one light appearance: pin it before the page paints, for the main
// window and the floating ball alike, whatever an older version saved.
(function () {
  document.documentElement.setAttribute('data-theme', 'light');
  try { localStorage.removeItem('salcara.theme'); } catch (_) { /* storage unavailable */ }
})();
