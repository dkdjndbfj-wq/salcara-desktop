const { app, BrowserWindow, Menu, Notification, Tray, nativeImage, nativeTheme, shell, ipcMain, screen } = require('electron');
const { spawn } = require('node:child_process');
const fs = require('node:fs');
const path = require('node:path');

const testBuild = require('./package.json').version.includes('-test.');
const appTitle = testBuild ? 'Salcara Bridge Test' : 'Salcara Bridge';
// A prerelease cannot share the original desktop vault, local port or Electron
// instance lock by accident. Applying an API to an agent is still explicit.
if (testBuild) {
  if (!process.env.SALCARA_BRIDGE_CONFIG_DIR) process.env.SALCARA_BRIDGE_CONFIG_DIR = path.join(app.getPath('appData'), 'SalcaraBridge-Test-1.5');
  if (!process.env.SALCARA_BRIDGE_PORT) process.env.SALCARA_BRIDGE_PORT = '47841';
}
const requestedPort = Number(process.env.SALCARA_BRIDGE_PORT || 47831);
const consolePort = Number.isInteger(requestedPort) && requestedPort > 0 && requestedPort < 65536 ? requestedPort : testBuild ? 47841 : 47831;
process.env.SALCARA_BRIDGE_PORT = String(consolePort);
const consoleURL = `http://127.0.0.1:${consolePort}/`;
// Keep portable/test setups separate from the user's normal desktop UI state.
if (process.env.SALCARA_BRIDGE_CONFIG_DIR && path.isAbsolute(process.env.SALCARA_BRIDGE_CONFIG_DIR)) {
  app.setPath('userData', path.join(process.env.SALCARA_BRIDGE_CONFIG_DIR, 'desktop-ui'));
}
let window;
let ball;
let tray;
let bridge;
let quitting = false;
let startHidden = false; // launched at login: stay in the background with the tray and ball
let coreRestarts = [];   // timestamps of automatic core restarts, to stop a crash loop
let attached = false;    // using a core that was already running (not our child)
let updater = null;      // desktop/updater.cjs, created on first use
let updateWin = null;    // the update window
let announced = '';      // the version the update window was last opened for by itself

if (!app.requestSingleInstanceLock()) {
  app.quit();
} else {
  app.on('second-instance', (_event, argv) => { if (!(argv || []).includes('--background')) showWindow(); });
  app.whenReady().then(start).catch((error) => {
    console.error('Salcara Bridge startup failed:', error);
    app.quit();
  });
}

// The app icon is the home screen's orb (desktop/icons; rendered for every size).
const ICONS = new Set(['app.ico', 'app.png', 'tray.ico', 'tray.png']);
function appIcon() { return resource(process.platform === 'win32' ? 'app.ico' : 'app.png'); }
function trayIcon() { return resource(process.platform === 'win32' ? 'tray.ico' : 'tray.png'); }

function resource(name) {
  if (app.isPackaged) return path.join(process.resourcesPath, name);
  if (ICONS.has(name)) return path.join(__dirname, 'icons', name);
  if (name === 'brand-logo.png') return path.resolve(__dirname, '../../../assets/brand-logo.png');
  return path.join(__dirname, 'bin', name);
}

function bridgeExecutable() {
  return resource(process.platform === 'win32' ? 'SalcaraBridge.exe' : 'SalcaraBridge');
}

function showWindow(route) {
  if (!window || window.isDestroyed()) return;
  closedDuringStart = false;
  hideBall();
  window.show();
  if (window.isMinimized()) window.restore();
  window.focus();
  if (typeof route === 'string') navigate(route);
}

function navigate(route) {
  const target = ROUTES.has(route) ? route : '';
  if (!window || window.isDestroyed() || !window.webContents.getURL().startsWith(consoleURL)) return;
  window.webContents.executeJavaScript(target ? `location.hash = ${JSON.stringify('#' + target)}` : "history.pushState(null, '', location.pathname); dispatchEvent(new PopStateEvent('popstate'))").catch(() => {});
}

/* ---------- fixed-size window; its position is remembered ---------- */
const WIN_W = 1120;
const WIN_H = 780;
function windowStateFile() { return path.join(app.getPath('userData'), 'window-state.json'); }
// The window is always 1120×780, shrunk only on a screen too small to hold it.
function windowBounds() {
  let s = {};
  try { s = JSON.parse(fs.readFileSync(windowStateFile(), 'utf8')) || {}; } catch { /* first start */ }
  const ok = (n) => Number.isFinite(n) && Math.abs(n) < 100000;
  const hasPos = ok(s.x) && ok(s.y);
  const display = hasPos ? screen.getDisplayMatching({ x: s.x, y: s.y, width: WIN_W, height: WIN_H }) : screen.getPrimaryDisplay();
  const area = display.workArea;
  const bounds = { width: Math.min(WIN_W, area.width), height: Math.min(WIN_H, area.height) };
  // Only reuse a position that is still on a connected display.
  if (hasPos && s.x + 80 < area.x + area.width && s.x + bounds.width - 80 > area.x && s.y >= area.y - 10 && s.y + 40 < area.y + area.height) {
    Object.assign(bounds, { x: Math.round(s.x), y: Math.round(s.y) });
  }
  return bounds;
}
let windowStateTimer;
function saveWindowState() {
  clearTimeout(windowStateTimer);
  windowStateTimer = setTimeout(() => {
    if (!window || window.isDestroyed()) return;
    try {
      const { x, y } = window.getBounds();
      fs.mkdirSync(path.dirname(windowStateFile()), { recursive: true });
      fs.writeFileSync(windowStateFile(), JSON.stringify({ x, y }));
    } catch { /* best effort */ }
  }, 400);
}

// Closing the window keeps Salcara running in the background with a floating ball.
function hideToBall() {
  if (!window || window.isDestroyed()) return;
  window.hide();
  showBall();
}

/* ---------- floating ball ---------- */
const BALL_W = 400;
const BALL_H = 210;
const ROUTES = new Set(['', 'overview', 'accounts', 'setup', 'login', 'usage', 'settings', 'sessions']);
let ballEnabled = true;
let closedDuringStart = false; // closed on the start-up page: go straight to the ball once the core is up
let ballDrag = null;

function ballStateFile() { return path.join(app.getPath('userData'), 'floating-ball.json'); }
function readBallState() {
  try { return JSON.parse(fs.readFileSync(ballStateFile(), 'utf8')); } catch { return {}; }
}
function saveBallState(extra) {
  try {
    const state = { ...readBallState(), ...extra };
    fs.mkdirSync(path.dirname(ballStateFile()), { recursive: true });
    fs.writeFileSync(ballStateFile(), JSON.stringify(state));
  } catch { /* best effort */ }
}

// Keep the ball's centre on a visible display.
function ballBounds() {
  const saved = readBallState();
  const display = saved.x !== undefined ? screen.getDisplayNearestPoint({ x: saved.x + BALL_W / 2, y: saved.y + BALL_H / 2 }) : screen.getPrimaryDisplay();
  const wa = display.workArea;
  let x = saved.x !== undefined ? saved.x : wa.x + wa.width - BALL_W / 2 - 84;
  let y = saved.y !== undefined ? saved.y : wa.y + wa.height - BALL_H / 2 - 150;
  x = Math.min(Math.max(x, wa.x - BALL_W / 2 + 40), wa.x + wa.width - BALL_W / 2 - 40);
  y = Math.min(Math.max(y, wa.y - BALL_H / 2 + 40), wa.y + wa.height - BALL_H / 2 - 40);
  return { x: Math.round(x), y: Math.round(y), width: BALL_W, height: BALL_H };
}

function createBall() {
  ball = new BrowserWindow({
    ...ballBounds(),
    frame: false,
    transparent: true,
    backgroundColor: '#00000000',
    resizable: false,
    movable: true,
    minimizable: false,
    maximizable: false,
    fullscreenable: false,
    skipTaskbar: true,
    hasShadow: false,
    alwaysOnTop: true,
    show: false,
    title: appTitle,
    webPreferences: { preload: path.join(__dirname, 'preload.cjs'), nodeIntegration: false, contextIsolation: true, sandbox: true, webviewTag: false },
  });
  ball.setAlwaysOnTop(true, 'floating');
  if (process.platform === 'darwin') ball.setVisibleOnAllWorkspaces(true, { visibleOnFullScreen: true });
  ball.setIgnoreMouseEvents(true, { forward: true });
  ball.webContents.setWindowOpenHandler(() => ({ action: 'deny' }));
  ball.webContents.on('will-navigate', (event) => event.preventDefault());
  ball.on('closed', () => { ball = null; });
  ball.loadURL(consoleURL + 'ball.html');
  return ball;
}

function showBall() {
  if (!ballEnabled || quitting) return;
  if (!ball || ball.isDestroyed()) createBall();
  ball.setBounds(ballBounds());
  ball.setIgnoreMouseEvents(true, { forward: true });
  const reveal = () => { if (ball && !ball.isDestroyed()) { ball.showInactive(); ball.webContents.send('ball:shown'); } };
  if (ball.webContents.isLoading()) ball.webContents.once('did-finish-load', reveal); else reveal();
}

function hideBall() {
  if (ball && !ball.isDestroyed()) ball.hide();
}

function fromConsole(event) {
  const url = event.senderFrame && event.senderFrame.url;
  return typeof url === 'string' && url.startsWith(consoleURL);
}

// The local start-up page (file://…/loading.html) in the main window.
function fromSplash(event) {
  const url = event.senderFrame && event.senderFrame.url;
  return typeof url === 'string' && url.startsWith('file:') && url.includes('loading.html') && Boolean(window) && !window.isDestroyed() && event.sender === window.webContents;
}

/* ---------- appearance (follow system / light / dark) ---------- */
// The console keeps the choice in its own storage and tells the shell, so the
// start-up page, the window background and the system colour scheme match it.
function appearanceFile() { return path.join(app.getPath('userData'), 'appearance.json'); }
function readAppearance() {
  try { const t = JSON.parse(fs.readFileSync(appearanceFile(), 'utf8')).theme; return t === 'light' || t === 'dark' ? t : 'system'; } catch { return 'system'; }
}
function applyAppearance(theme) {
  try { nativeTheme.themeSource = theme; } catch { /* older shells: follow the system */ }
}
function windowBackground() {
  try { return nativeTheme.shouldUseDarkColors ? '#121419' : '#f3f5f9'; } catch { return '#f3f5f9'; }
}

function registerIpc() {
  ipcMain.on('win:theme', (event, theme) => {
    if (!fromConsole(event) || !['system', 'light', 'dark'].includes(theme) || theme === readAppearance()) return;
    applyAppearance(theme);
    try { fs.mkdirSync(path.dirname(appearanceFile()), { recursive: true }); fs.writeFileSync(appearanceFile(), JSON.stringify({ theme })); } catch { /* best effort */ }
    if (window && !window.isDestroyed()) window.setBackgroundColor(windowBackground());
  });
  ipcMain.on('win:minimize', (event) => { if ((fromConsole(event) || fromSplash(event)) && window && !window.isDestroyed()) window.minimize(); });
  ipcMain.on('win:close', (event) => { if (fromConsole(event)) hideToBall(); });
  ipcMain.on('win:show', (event, route) => { if (fromConsole(event)) showWindow(typeof route === 'string' ? route : ''); });
  ipcMain.handle('win:state', (event) => (fromConsole(event) && window && !window.isDestroyed() ? { focused: window.isFocused() && window.isVisible() } : {}));
  // System notifications for the console page (approvals, finished tasks).
  ipcMain.on('notify', (event, payload) => {
    if (!fromConsole(event) || !Notification.isSupported() || !payload || typeof payload !== 'object') return;
    const title = String(payload.title || '').slice(0, 80), body = String(payload.body || '').slice(0, 240);
    const route = typeof payload.route === 'string' && ROUTES.has(payload.route) ? payload.route : '';
    if (!title) return;
    const note = new Notification({ title, body, silent: Boolean(payload.silent), icon: nativeImage.createFromPath(appIcon()) });
    note.on('click', () => showWindow(route));
    note.show();
  });
  // Settings page: status, "check now", "show the update window".
  ipcMain.handle('update:status', (event) => (fromConsole(event) ? updateSummary() : null));
  ipcMain.handle('update:check', async (event) => {
    if (!fromConsole(event)) return null;
    const st = await checkForUpdate(true);
    if (st.phase === 'available' || st.phase === 'error') openUpdateWindow(false);
    return updateSummary();
  });
  ipcMain.on('update:open', (event) => { if (fromConsole(event)) openUpdateWindow(false); });
  // The update window itself.
  const fromUpdate = (event) => Boolean(updateWin) && !updateWin.isDestroyed() && event.sender === updateWin.webContents;
  ipcMain.handle('upd:state', (event) => (fromUpdate(event) ? getUpdater().state() : null));
  ipcMain.on('upd:check', (event) => { if (fromUpdate(event)) void checkForUpdate(true); });
  ipcMain.on('upd:download', (event) => { if (fromUpdate(event)) void getUpdater().download(); });
  ipcMain.on('upd:cancel', (event) => { if (fromUpdate(event)) getUpdater().cancel(); });
  ipcMain.on('upd:install', (event) => { if (fromUpdate(event)) void getUpdater().install().catch((error) => console.error('update install failed:', error)); });
  ipcMain.on('upd:skip', (event) => { if (fromUpdate(event)) { getUpdater().skip(); updateWin.close(); } });
  ipcMain.on('upd:page', (event) => { if (fromUpdate(event)) getUpdater().openPage(); });
  ipcMain.on('upd:later', (event) => { if (fromUpdate(event)) updateWin.close(); });
  // The local error page (file://loading.html) may ask for one more start attempt.
  ipcMain.on('core:retry', async (event) => {
    const url = event.senderFrame && event.senderFrame.url;
    if (typeof url !== 'string' || !url.startsWith('file:') || !url.includes('loading.html') || quitting) return;
    coreRestarts = [];
    if (!(await healthy())) { try { spawnCore(); } catch (error) { console.error(error); return; } }
    if (await waitForBridge()) await window.loadURL(consoleURL).catch(() => {});
    else await window.loadFile(path.join(__dirname, 'loading.html'), { query: { error: `核心仍然无法启动。请检查本机 ${consolePort} 端口是否被其他程序占用。`, retry: '1' } }).catch(() => {});
  });
  ipcMain.on('ball:show', (event, route) => {
    if (!fromConsole(event)) return;
    showWindow(typeof route === 'string' ? route : '');
  });
  ipcMain.on('ball:interactive', (event, on) => {
    if (!fromConsole(event) || !ball || ball.isDestroyed()) return;
    if (on) ball.setIgnoreMouseEvents(false); else if (!ballDrag) ball.setIgnoreMouseEvents(true, { forward: true });
  });
  ipcMain.on('ball:drag', (event, phase) => {
    if (!fromConsole(event) || !ball || ball.isDestroyed()) return;
    const cursor = screen.getCursorScreenPoint();
    if (phase === 'start') {
      const [x, y] = ball.getPosition();
      ballDrag = { dx: cursor.x - x, dy: cursor.y - y };
    } else if (phase === 'move' && ballDrag) {
      ball.setPosition(Math.round(cursor.x - ballDrag.dx), Math.round(cursor.y - ballDrag.dy));
    } else if (phase === 'end') {
      ballDrag = null;
      const [x, y] = ball.getPosition();
      saveBallState({ x, y });
    }
  });
}

function setBallEnabled(on) {
  ballEnabled = on;
  saveBallState({ enabled: on });
  if (!on) hideBall();
  else if (window && !window.isDestroyed() && !window.isVisible()) showBall();
  updateTrayMenu();
}

function createWindow() {
  const icon = nativeImage.createFromPath(appIcon());
  window = new BrowserWindow({
    title: appTitle,
    ...windowBounds(),
    resizable: false,
    maximizable: false,
    fullscreenable: false,
    show: false,
    frame: false, // the page draws its own minimise / close buttons
    roundedCorners: true,
    backgroundColor: windowBackground(),
    icon,
    webPreferences: {
      nodeIntegration: false,
      contextIsolation: true,
      sandbox: true,
      webviewTag: false,
      preload: path.join(__dirname, 'preload.cjs'),
    },
  });
  window.setMenuBarVisibility(false);
  // Official download pages open in the system browser; nothing else leaves the app.
  const external = new Set(['https://chatgpt.com/codex', 'https://claude.ai/download', 'https://git-scm.com/download/win']);
  window.webContents.setWindowOpenHandler(({ url }) => {
    if (external.has(url)) shell.openExternal(url);
    return { action: 'deny' };
  });
  window.webContents.on('will-navigate', (event, destination) => {
    if (!destination.startsWith(consoleURL)) event.preventDefault();
  });
  window.on('close', (event) => {
    if (quitting) return;
    event.preventDefault();
    // Before the console has loaded (or when the core failed) there is nothing for the ball to show.
    if (window.webContents.getURL().startsWith(consoleURL)) hideToBall();
    else { closedDuringStart = true; window.hide(); }
  });
  window.on('show', hideBall);
  window.on('move', saveWindowState);
  window.once('ready-to-show', () => { if (!startHidden) showWindow(); });
  window.loadFile(path.join(__dirname, 'loading.html'));
  return window;
}

function createTray() {
  const icon = nativeImage.createFromPath(trayIcon());
  tray = new Tray(icon);
  tray.setToolTip(appTitle + ' · AI 工具工作台与 API 密钥库');
  updateTrayMenu();
  // One click on the tray icon brings the window back (the right-click menu stays as it is).
  tray.on('click', () => showWindow());
}

function updateTrayMenu() {
  if (!tray) return;
  tray.setContextMenu(Menu.buildFromTemplate([
    { label: '打开 ' + appTitle, click: showWindow },
    { label: '显示悬浮球', type: 'checkbox', checked: ballEnabled, click: (item) => setBallEnabled(item.checked) },
    ...updateTrayItems(),
    { type: 'separator' },
    { label: '退出', click: () => app.quit() },
  ]));
}

async function healthy() {
  try {
    const response = await fetch(consoleURL + 'healthz', { signal: AbortSignal.timeout(1500) });
    const body = await response.json();
    return response.ok && body.service === 'salcara-bridge';
  } catch {
    return false;
  }
}

async function waitForBridge() {
  for (let attempt = 0; attempt < 80; attempt++) {
    if (await healthy()) return true;
    await new Promise((resolve) => setTimeout(resolve, 250));
  }
  return false;
}

/* ---------- the Bridge core: start, attach to a running one, restart after a crash ---------- */
function spawnCore() {
  const executable = bridgeExecutable();
  if (!fs.existsSync(executable)) throw new Error('桌面程序缺少 Bridge 核心，请重新打包。');
  const child = spawn(executable, ['--background'], {
    env: {
      ...process.env,
      SALCARA_EMBEDDED_WINDOW: '1',
      // Start at login launches this app (tray + floating ball), not the bare core.
      ...(app.isPackaged ? { SALCARA_DESKTOP_EXE: process.execPath } : {}),
      ...(!app.isPackaged ? { SALCARA_DESKTOP_COMPANION_DIR: path.resolve(__dirname, '../../desktop-companion') } : {}),
    },
    windowsHide: true,
    stdio: 'ignore',
  });
  bridge = child;
  attached = false;
  child.on('error', (error) => console.error('Bridge process error:', error));
  child.on('exit', () => { if (bridge === child) bridge = null; if (!quitting) void coreExited(); });
}

async function coreExited() {
  // Another core already owns the port (for example one started at login by an
  // older version): use it instead of quitting the whole app.
  if (await healthy()) { attachToRunningCore(); return; }
  const now = Date.now();
  coreRestarts = coreRestarts.filter((t) => now - t < 120000);
  if (coreRestarts.length >= 4) {
    if (window && !window.isDestroyed()) {
      await window.loadFile(path.join(__dirname, 'loading.html'), { query: { error: '核心连续意外退出，已停止自动重启。请查看设置里的运行日志，或重新打开 Salcara Bridge。', retry: '1' } }).catch(() => {});
      if (!window.isVisible()) showWindow();
    }
    return;
  }
  coreRestarts.push(now);
  await new Promise((resolve) => setTimeout(resolve, 800 * coreRestarts.length));
  if (quitting) return;
  try {
    spawnCore();
  } catch (error) {
    console.error(error);
    return;
  }
  if (await waitForBridge()) {
    notify('Salcara Bridge 已自动恢复', '核心程序意外退出，已经重新启动。手机连接会自动恢复。');
    if (window && !window.isDestroyed() && window.webContents.getURL().startsWith(consoleURL)) window.webContents.reload();
    else if (window && !window.isDestroyed()) await window.loadURL(consoleURL).catch(() => {});
    reloadBall();
  }
}

// Watch a core we did not start; if it goes away, start our own.
let attachTimer;
function attachToRunningCore() {
  attached = true;
  clearInterval(attachTimer);
  attachTimer = setInterval(async () => {
    if (quitting || !attached) { clearInterval(attachTimer); return; }
    if (!(await healthy())) {
      clearInterval(attachTimer);
      attached = false;
      try { spawnCore(); } catch (error) { console.error(error); return; }
      if (await waitForBridge()) {
        if (window && !window.isDestroyed()) await window.loadURL(consoleURL).catch(() => {});
        reloadBall();
      }
    }
  }, 5000);
}

function reloadBall() {
  if (ball && !ball.isDestroyed()) ball.webContents.reload();
}

function notify(title, body) {
  if (!Notification.isSupported()) return;
  const note = new Notification({ title, body, icon: nativeImage.createFromPath(appIcon()) });
  note.on('click', () => showWindow());
  note.show();
}

/* ---------- automatic updates (desktop/updater.cjs) ---------- */
function updateRepo() {
  try {
    const repo = String(require('./package.json').salcaraUpdateRepo || '').trim();
    return /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(repo) ? repo : '';
  } catch { return ''; }
}
function getUpdater() {
  if (updater) return updater;
  const { createUpdater } = require('./updater.cjs');
  let lastPhase = '';
  updater = createUpdater({
    app, shell, platform: process.platform, arch: process.arch, execPath: process.execPath, isPackaged: app.isPackaged,
    repo: updateRepo(), publicKey: String(require('./package.json').salcaraUpdatePublicKey || ''), userData: app.getPath('userData'),
    onChange(state) {
      if (updateWin && !updateWin.isDestroyed()) updateWin.webContents.send('upd:state', state);
      if (state.phase !== lastPhase) { lastPhase = state.phase; updateTrayMenu(); }
    },
    beforeInstall: stopForUpdate,
  });
  return updater;
}
function updateSummary() {
  const st = getUpdater().state();
  const known = ['available', 'downloading', 'verifying', 'ready'].includes(st.phase) && st.version;
  return { current: app.getVersion(), configured: Boolean(updateRepo()), phase: st.phase, update: known ? { version: st.version } : null };
}
function updateTrayItems() {
  if (!updater) return [];
  const st = updater.state();
  if (st.phase === 'ready') return [{ label: `重启以完成更新 ${st.version}`, click: () => openUpdateWindow(false) }];
  if (st.phase === 'downloading' || st.phase === 'verifying') return [{ label: `正在下载更新 ${st.version}…`, click: () => openUpdateWindow(false) }];
  if (st.phase === 'available') return [{ label: `有新版本 ${st.version}`, click: () => openUpdateWindow(false) }];
  return [];
}
async function checkForUpdate(manual) {
  const st = await getUpdater().check(manual);
  // Found by the periodic check: open the window once per version, without taking focus, unless skipped.
  if (!manual && st.phase === 'available' && st.version && announced !== st.version && !getUpdater().skipped(st.version)) {
    announced = st.version;
    openUpdateWindow(true);
  }
  return st;
}
function openUpdateWindow(quiet) {
  if (updateWin && !updateWin.isDestroyed()) {
    if (!quiet) { updateWin.show(); updateWin.focus(); }
    return;
  }
  updateWin = new BrowserWindow({
    title: '软件更新', width: 440, height: 560, resizable: false, maximizable: false, minimizable: false, fullscreenable: false,
    frame: false, roundedCorners: true, show: false, backgroundColor: windowBackground(), icon: nativeImage.createFromPath(appIcon()),
    webPreferences: { nodeIntegration: false, contextIsolation: true, sandbox: true, webviewTag: false, preload: path.join(__dirname, 'preload.cjs') },
  });
  updateWin.setMenuBarVisibility(false);
  updateWin.webContents.setWindowOpenHandler(() => ({ action: 'deny' }));
  updateWin.webContents.on('will-navigate', (event) => event.preventDefault());
  updateWin.once('ready-to-show', () => { if (quiet) updateWin.showInactive(); else updateWin.show(); });
  updateWin.on('closed', () => { updateWin = null; });
  updateWin.loadFile(path.join(__dirname, 'update.html'));
}
/** Before the folders are swapped: stop the core and let the app quit. */
async function stopForUpdate() {
  quitting = true;
  clearInterval(attachTimer);
  if (bridge && bridge.exitCode === null) bridge.kill();
  else if (await healthy()) {
    // A core this app did not start: ask it to quit through the console (same cookie as the page).
    if (window && !window.isDestroyed() && window.webContents.getURL().startsWith(consoleURL)) {
      await window.webContents.executeJavaScript("fetch('/api/quit', { method: 'POST' }).then(() => 1, () => 0)", true).catch(() => 0);
    }
  }
  for (let i = 0; i < 40 && await healthy(); i++) await new Promise((r) => setTimeout(r, 200));
  for (const w of [window, ball, updateWin]) if (w && !w.isDestroyed()) w.hide();
}
function reportUpdateResult() {
  const done = getUpdater().afterRestart();
  if (!done) return;
  if (done.ok) notify(`已更新到 ${app.getVersion()}`, '新版本已经在运行。');
  else notify('更新没有完成', `仍在使用 ${app.getVersion()}。可以在「设置 → 版本更新」里重试。`);
}

async function start() {
  startHidden = process.argv.includes('--background');
  if (process.platform === 'win32') app.setAppUserModelId(testBuild ? 'top.salcara.bridge.test' : 'top.salcara.bridge');
  ballEnabled = readBallState().enabled !== false;
  applyAppearance(readAppearance());
  registerIpc();
  createWindow();
  createTray();
  if (await healthy()) attachToRunningCore();
  else spawnCore();
  if (!(await waitForBridge())) {
    await window.loadFile(path.join(__dirname, 'loading.html'), { query: { error: `核心启动失败。请检查本机 ${consolePort} 端口是否被占用。`, retry: '1' } });
    if (startHidden) showWindow();
    return;
  }
  // Let the start-up animation finish, then hand over to the console, which
  // carries the orb and the name to their places (?boot=1). Skipped when
  // starting hidden: nobody is watching.
  if (startHidden || closedDuringStart) await window.loadURL(consoleURL);
  else {
    await Promise.race([
      window.webContents.executeJavaScript('window.__bootReady ? window.__bootReady.then(() => window.__settle && window.__settle()) : 0', true).catch(() => {}),
      new Promise((resolve) => setTimeout(resolve, 4000)),
    ]);
    // Closed while the animation was finishing: no hand-over, just the ball.
    if (closedDuringStart || !window.isVisible()) await window.loadURL(consoleURL);
    else await window.loadURL(consoleURL + '?boot=1');
  }
  if (startHidden || closedDuringStart || !window.isVisible()) showBall();
  getUpdater().cleanup();
  reportUpdateResult();
  setTimeout(() => void checkForUpdate(false), 20000);
  setInterval(() => void checkForUpdate(false), 6 * 3600 * 1000);
}

app.on('before-quit', () => {
  quitting = true;
  clearInterval(attachTimer);
  if (bridge && bridge.exitCode === null) bridge.kill();
});
