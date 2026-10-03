'use strict';
/* Start-up animation, part 2: see boot.css. Runs only when the desktop app
   hands over from its start-up page (?boot=1), once. */
(function () {
  const root = document.documentElement;
  if (!/[?&]boot=1(?:&|$)/.test(location.search)) return;
  // Reloads and later visits start normally.
  try { history.replaceState(history.state, '', location.pathname + location.hash); } catch (_) { /* optional */ }
  if (matchMedia('(prefers-reduced-motion: reduce)').matches) return;
  root.classList.add('booting');

  const EASE = 'cubic-bezier(.7, 0, .2, 1)';
  let finished = false;
  function finish() {
    if (finished) return;
    finished = true;
    root.classList.remove('booting');
    const boot = document.getElementById('boot');
    if (boot) boot.remove();
  }
  setTimeout(finish, 3500); // never leave the screen covered

  function rectCenter(r) { return { x: r.left + r.width / 2, y: r.top + r.height / 2 }; }

  function run() {
    const scene = document.querySelector('.boot-scene');
    const bootOrb = document.querySelector('.boot-orb');
    const bootBrand = document.querySelector('.boot-brand');
    const orb = document.getElementById('orb');
    const hudBrand = document.querySelector('.hud-brand');
    const zone = document.getElementById('orbZone');
    if (!scene || !bootOrb || !orb) return finish();
    const panelOpen = document.body.classList.contains('panel-open');
    const from = bootOrb.getBoundingClientRect(), to = orb.getBoundingClientRect();
    const fly = !panelOpen && to.width > 0;
    const D = 950;

    if (fly) {
      const a = rectCenter(from), b = rectCenter(to), k = to.width / from.width;
      scene.animate([
        { transform: 'translate(0, 0) scale(1)' },
        { transform: `translate(${(b.x - a.x) * -0.02}px, ${(b.y - a.y) * -0.02}px) scale(.965)`, offset: .14 },
        { transform: `translate(${b.x - a.x}px, ${b.y - a.y}px) scale(${k})` },
      ], { duration: D, easing: EASE, fill: 'forwards' });
      // a blink mid-flight
      document.querySelectorAll('.boot-face i').forEach((eye) => eye.animate(
        [{ transform: 'scaleY(1)' }, { transform: 'scaleY(.08)' }, { transform: 'scaleY(1)' }],
        { duration: 200, delay: D * .55, easing: 'ease-in-out' }));
    } else {
      scene.animate([{ opacity: 1, transform: 'scale(1)' }, { opacity: 0, transform: 'scale(.9)' }], { duration: 420, easing: 'ease-in', fill: 'forwards' });
    }

    if (bootBrand && hudBrand && fly) {
      // An arc around the orb, not through it: sideways first, then up into the header.
      const s = bootBrand.getBoundingClientRect(), t = hudBrand.getBoundingClientRect(), k = t.width / s.width;
      const dx = t.left - s.left, dy = t.top - s.top;
      bootBrand.animate([
        { transform: 'translate(0, 0) scale(1)', easing: 'cubic-bezier(.6, 0, .4, 1)' },
        { transform: `translate(${dx * .78}px, ${dy * .12}px) scale(${1 - (1 - k) * .55})`, offset: .45, easing: 'cubic-bezier(.3, 0, .2, 1)' },
        { transform: `translate(${dx}px, ${dy}px) scale(${k})` },
      ], { duration: D - 40, delay: 40, fill: 'forwards' });
    } else if (bootBrand) {
      bootBrand.animate([{ opacity: 1 }, { opacity: 0 }], { duration: 300, fill: 'forwards' });
    }

    setTimeout(() => {
      finish();
      const foot = document.querySelector('.stage > .foot');
      foot?.animate([{ opacity: 0, transform: 'translateY(8px)' }, { opacity: 1, transform: 'none' }], { duration: 520, easing: 'cubic-bezier(.16, 1, .3, 1)' });
      if (!fly) {
        document.querySelector('.stage > .orb-zone')?.animate([{ opacity: 0 }, { opacity: 1 }], { duration: 400 });
        document.querySelector('.stage > .hud')?.animate([{ opacity: 0 }, { opacity: 1 }], { duration: 400 });
        return;
      }
      const ripple = document.createElement('i');
      ripple.className = 'boot-ripple';
      const size = orb.offsetWidth;
      ripple.style.width = ripple.style.height = `${size}px`;
      zone.appendChild(ripple);
      ripple.animate([{ transform: 'scale(1)', opacity: .75 }, { transform: 'scale(1.75)', opacity: 0 }],
        { duration: 900, easing: 'cubic-bezier(.16, 1, .3, 1)' }).finished.finally(() => ripple.remove());
    }, fly ? D : 420);
  }

  function start() { requestAnimationFrame(() => requestAnimationFrame(run)); }
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', start, { once: true });
  else start();
})();
