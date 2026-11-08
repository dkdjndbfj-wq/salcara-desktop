'use strict';

// The floating page is a separate, sandboxed BrowserWindow. Its small command
// surface must not inherit the main console's notifications, dialogs or updater.
function fromBall(event, win, consoleURL) {
  if (!event || !win || win.isDestroyed() || event.sender !== win.webContents || event.senderFrame !== win.webContents.mainFrame) return false;
  try {
    const url = new URL(event.senderFrame.url), base = new URL(consoleURL);
    return url.origin === base.origin && url.pathname === '/ball.html';
  } catch { return false; }
}

const coordinate = value => Number.isFinite(value) && Math.abs(value) < 100000;
const point = value => Boolean(value) && coordinate(value.x) && coordinate(value.y);

function clampBallBounds(bounds, screen) {
  // Electron's cursor, BrowserWindow bounds and display workArea are all DIP.
  // Do not apply scaleFactor again when crossing displays with different DPI.
  const display = screen.getDisplayNearestPoint({ x: Math.round(bounds.x + bounds.width / 2), y: Math.round(bounds.y + bounds.height / 2) });
  const area = display.workArea;
  // Keep the quick-entry ring reachable. Side content flips toward the work
  // area's interior (ballLayout), so no-plugin placement need not move inward.
  // The picker reaches 97 DIP vertically; one extra DIP covers native rounding.
  const horizontal = Math.min(96, area.width / 2), vertical = Math.min(98, area.height / 2);
  return { ...bounds,
    x: Math.round(Math.min(Math.max(bounds.x, area.x - bounds.width / 2 + horizontal), area.x + area.width - bounds.width / 2 - horizontal)),
    y: Math.round(Math.min(Math.max(bounds.y, area.y - bounds.height / 2 + vertical), area.y + area.height - bounds.height / 2 - vertical)),
  };
}

function ballLayout(bounds, screen) {
  const center = { x: bounds.x + bounds.width / 2, y: bounds.y + bounds.height / 2 };
  const area = screen.getDisplayNearestPoint({ x: Math.round(center.x), y: Math.round(center.y) }).workArea;
  const left = center.x - area.x, right = area.x + area.width - center.x;
  const side = right >= left ? 'right' : 'left', room = side === 'right' ? right : left;
  // Clamp to both the physical work area and the transparent window itself.
  // Plugin picker starts 18 DIP from centre; usage bubble starts 44 DIP away.
  return {
    side,
    maxWidth: Math.max(0, Math.floor(Math.min(room, bounds.width / 2) - 18 - 8)),
    bubbleMaxWidth: Math.max(0, Math.floor(Math.min(room, bounds.width / 2) - 44 - 8)),
  };
}

function restoredBallBounds(saved, screen, width, height) {
  const area = screen.getPrimaryDisplay().workArea;
  const position = point(saved) ? saved : { x: area.x + area.width - width / 2 - 84, y: area.y + area.height - height / 2 - 150 };
  return clampBallBounds({ x: position.x, y: position.y, width, height }, screen);
}

function registerBallIpc({ ipcMain, screen, getBall, consoleURL, showWindow, savePosition, width, height }) {
  let drag = null, interactive = false;
  const live = win => win && !win.isDestroyed();
  const allowed = event => {
    const win = getBall();
    return fromBall(event, win, consoleURL) && win.isVisible() && !win.webContents.isLoading() ? win : null;
  };
  const apply = (win, on) => {
    if (!live(win)) return;
    if (on) win.setIgnoreMouseEvents(false);
    else win.setIgnoreMouseEvents(true, { forward: true });
  };
  const cursorOnBall = win => {
    const cursor = screen.getCursorScreenPoint(), bounds = win.getBounds();
    return point(cursor) && Math.hypot(cursor.x - bounds.x - bounds.width / 2, cursor.y - bounds.y - bounds.height / 2) <= 36;
  };
  const layout = win => {
    if (live(win)) win.webContents.send('ball:layout', ballLayout(win.getBounds(), screen));
  };
  const constrain = win => {
    if (!live(win)) return;
    const before = win.getBounds(), after = clampBallBounds({ ...before, width, height }, screen);
    if (before.x !== after.x || before.y !== after.y || before.width !== width || before.height !== height) win.setBounds(after);
    savePosition({ x: after.x, y: after.y });
  };
  const finish = (win, cancel = false, notify = false) => {
    const active = drag?.win === win ? drag : null;
    if (active) {
      drag = null;
      if (active.moved) constrain(win);
    }
    // Ending at a display edge can move the ball away from the pointer. Do
    // not leave the transparent window intercepting the next stationary click.
    if (active?.moved && live(win)) interactive = cursorOnBall(win);
    if (cancel) interactive = false;
    apply(win, interactive);
    if (notify && active && (active.moved || cancel) && live(win)) {
      if (active.moved) layout(win);
      // A cancelled click also changed native click-through. Acknowledge it so
      // the renderer cannot retain `interactive=true` and suppress the next
      // mousemove update while the stationary pointer is still over the ball.
      win.webContents.send('ball:drag-finished', interactive);
    }
  };
  const reset = (win, shown = false) => {
    if (drag && drag.win !== win) finish(drag.win, true);
    finish(win, true);
    if (!shown || !live(win)) return false;
    // A stationary pointer already over a newly shown ball must be clickable
    // before the first forwarded mousemove reaches the renderer.
    interactive = cursorOnBall(win);
    apply(win, interactive);
    layout(win);
    return interactive;
  };

  ipcMain.on('ball:show', (event, route) => {
    if (allowed(event)) showWindow(typeof route === 'string' ? route : '');
  });
  ipcMain.on('ball:interactive', (event, on) => {
    const win = allowed(event);
    if (!win || typeof on !== 'boolean') return;
    interactive = on;
    apply(win, on || drag?.win === win);
  });
  ipcMain.on('ball:drag', (event, phase) => {
    const win = allowed(event);
    if (!win) return;
    if (phase === 'end' || phase === 'cancel') { finish(win, phase === 'cancel', true); return; }
    const cursor = screen.getCursorScreenPoint();
    if (!point(cursor)) return;
    if (phase === 'start') {
      if (drag) return;
      const bounds = win.getBounds();
      drag = { win, dx: cursor.x - bounds.x, dy: cursor.y - bounds.y, moved: false };
      apply(win, true);
    } else if (phase === 'move' && drag?.win === win) {
      const x = Math.round(cursor.x - drag.dx), y = Math.round(cursor.y - drag.dy);
      if (!point({ x, y })) return;
      const bounds = win.getBounds();
      if (x !== bounds.x || y !== bounds.y) {
        drag.moved = true;
        // setPosition reuses rounded native bounds and can add a DIP on every
        // move at fractional Windows DPI. Always request the fixed ball size.
        win.setBounds({ x, y, width, height });
      }
    }
  });

  function watch(win) {
    const current = () => getBall() === win;
    for (const event of ['hide', 'blur', 'closed', 'unresponsive']) win.on(event, () => { if (current()) reset(win); });
    for (const event of ['did-start-loading', 'render-process-gone']) win.webContents.on(event, () => { if (current()) reset(win); });
    win.webContents.on('did-finish-load', () => {
      if (current() && live(win) && win.isVisible()) win.webContents.send('ball:shown', reset(win, true));
    });
  }
  const displayChanged = () => {
    const win = getBall();
    if (!live(win) || !win.isVisible()) return;
    reset(win); constrain(win);
    win.webContents.send('ball:shown', reset(win, true));
  };
  screen.on('display-added', displayChanged);
  screen.on('display-removed', displayChanged);
  screen.on('display-metrics-changed', displayChanged);
  return { reset, watch };
}

module.exports = { fromBall, clampBallBounds, ballLayout, restoredBallBounds, registerBallIpc };
