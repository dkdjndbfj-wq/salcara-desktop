const { app, BrowserWindow, Menu, Notification, Tray, nativeImage, nativeTheme, shell, ipcMain, screen, dialog } = require('electron');
const { spawn } = require('node:child_process');
const { randomBytes } = require('node:crypto');
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
let pluginSystem = null, pluginCoreLink = null, pluginQuitDone = false, pluginQuitStarted = false;
let updateOwner = null;  // owning child/cookie during a two-phase update
const updatePolicy = require('./update-policy.cjs');
let policyError = '', policySync = null;
// The plugin hook is a desktop-local capability. Generate it per process and
// keep it out of the renderer, command line, and persisted user configuration.
const localToken = randomBytes(32).toString('hex');

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
const ROUTES = new Set(['', 'overview', 'accounts', 'mcp', 'plugins', 'setup', 'login', 'usage', 'settings', 'sessions']);
let ballEnabled = true;
let closedDuringStart = false; // closed on the start-up page: go straight to the ball once the core is up
let ballIpc = null, ballShowRevision = 0;
let ballWanted = false, ballPage = null, ballRecovery = null, ballFailures = [];

function ballStateFile() { return path.join(app.getPath('userData'), 'floating-ball.json'); }
function readBallState() {
  try {
    const state = JSON.parse(fs.readFileSync(ballStateFile(), 'utf8'));
    return state && typeof state === 'object' && !Array.isArray(state) ? state : {};
  } catch { return {}; }
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
  return require('./ball-ipc.cjs').restoredBallBounds(readBallState(), screen, BALL_W, BALL_H);
}

function mainVisible() { return window && !window.isDestroyed() && window.isVisible(); }
function cancelBallRecovery() { if (ballRecovery !== null) clearTimeout(ballRecovery); ballRecovery = null; }

function revealBall(current) {
  if (ball !== current || current.isDestroyed() || ballPage?.state !== 'ready' || current.isVisible()
    || !ballWanted || !ballEnabled || quitting || mainVisible()) return;
  current.setBounds(ballBounds());
  ballIpc.reset(current);
  current.showInactive();
  current.webContents.send('ball:shown', ballIpc.reset(current, true));
}

function failBallPage(current, error) {
  if (ball !== current || current.isDestroyed() || ballPage?.window !== current || ballPage.state === 'failed') return;
  ballPage.state = 'failed';
  ballIpc.reset(current); current.hide();
  console.error('Floating ball page unavailable:', error);
  cancelBallRecovery();
  if (!ballWanted || !ballEnabled || quitting || mainVisible()) return;
  const now = Date.now();
  ballFailures = ballFailures.filter(at => now - at < 60000);
  // A transient load/process failure can recover quietly. Repeated crashes
  // must not create an endless renderer restart loop in the background.
  if (ballFailures.length >= 3) return;
  const delay = [250, 750, 2000][ballFailures.length];
  ballFailures.push(now);
  const revision = ballShowRevision, page = ballPage, version = page.version;
  ballRecovery = setTimeout(() => {
    ballRecovery = null;
    if (ball !== current || current.isDestroyed() || ballPage !== page || page.version !== version
      || revision !== ballShowRevision || !ballWanted || !ballEnabled || quitting || mainVisible()) return;
    loadBallPage(current);
  }, delay);
}

function loadBallPage(current) {
  if (ball !== current || current.isDestroyed() || ballPage?.window !== current) return;
  cancelBallRecovery();
  const page = ballPage, version = ++page.version;
  page.state = 'loading';
  ballIpc.reset(current); current.hide();
  const failed = error => {
    if (ballPage !== page || page.version !== version || error?.code === 'ERR_ABORTED') return;
    failBallPage(current, error?.message || String(error));
  };
  const loaded = () => {
    if (ball !== current || current.isDestroyed() || ballPage !== page || page.version !== version || page.state !== 'loading') return;
    page.state = 'ready'; revealBall(current);
  };
  // Native load events do not identify which loadURL call they belong to. A
  // delayed did-fail-load from a superseded navigation must not fail this page;
  // did-finish-load can also describe Chromium's error document. Settle both
  // outcomes through this call's versioned Promise instead.
  try { Promise.resolve(current.loadURL(consoleURL + 'ball.html')).then(loaded, failed).catch(failed); }
  catch (error) { failed(error); }
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
  const created = ball;
  ballPage = { window: created, state: 'loading', version: 0 };
  ballIpc.watch(created);
  created.on('unresponsive', () => failBallPage(created, 'renderer unresponsive'));
  created.webContents.on('render-process-gone', (_event, details) => failBallPage(created, details?.reason || 'renderer exited'));
  ball.on('closed', () => {
    if (ball === created) { cancelBallRecovery(); ball = null; ballPage = null; ballShowRevision++; }
  });
  loadBallPage(created);
  return ball;
}

function showBall() {
  if (!ballEnabled || quitting || mainVisible()) return;
  ballWanted = true;
  if (!ball || ball.isDestroyed()) createBall();
  // Repeated close/background requests must not reset an in-flight drag to
  // the position saved before it, or restart the already visible animation.
  if (ball.isVisible()) return;
  ballShowRevision++;
  if (ballPage?.state === 'failed') loadBallPage(ball);
  else revealBall(ball);
}

function hideBall() {
  ballWanted = false; ballShowRevision++; cancelBallRecovery();
  if (ball && !ball.isDestroyed()) { ballIpc?.reset(ball); ball.hide(); }
}

function fromConsole(event) {
  if (!window || window.isDestroyed() || !event || event.sender !== window.webContents
    || event.senderFrame !== window.webContents.mainFrame) return false;
  try {
    const url = new URL(event.senderFrame.url), base = new URL(consoleURL);
    return url.origin === base.origin && (url.pathname === '/' || url.pathname === '/index.html');
  } catch { return false; }
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
  ballIpc = require('./ball-ipc.cjs').registerBallIpc({ ipcMain, screen, getBall: () => ball, consoleURL, showWindow, savePosition: saveBallState, width: BALL_W, height: BALL_H });
  const updateIpc = require('./update-ipc.cjs').registerUpdateIpc({ ipcMain, getWindow: () => updateWin,
    updateURL: require('node:url').pathToFileURL(path.join(__dirname, 'update.html')).href, getUpdater, checkForUpdate,
    showTasks: () => showWindow(), getState: () => ({ ...getUpdater().state(), policyError }),
    copyDiagnostics: value => require('electron').clipboard.writeText(value) });
  require('./window-ipc.cjs').registerWindowIpc({ ipcMain, dialog, getWindow: () => window, consoleURL, fromSplash, hide: hideToBall,
    hideSplash: () => { closedDuringStart = true; if (window && !window.isDestroyed()) window.hide(); }, path,
    closeUpdate: event => { if (!updateIpc.allowed(event)) return false; updateIpc.close(event); return true; } });
  ipcMain.on('win:theme', (event, theme) => {
    if (!fromConsole(event) || !['system', 'light', 'dark'].includes(theme) || theme === readAppearance()) return;
    applyAppearance(theme);
    try { fs.mkdirSync(path.dirname(appearanceFile()), { recursive: true }); fs.writeFileSync(appearanceFile(), JSON.stringify({ theme })); } catch { /* best effort */ }
    if (window && !window.isDestroyed()) window.setBackgroundColor(windowBackground());
  });
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
  // The local error page (file://loading.html) may ask for one more start attempt.
  ipcMain.on('core:retry', async (event) => {
    const url = event.senderFrame && event.senderFrame.url;
    if (typeof url !== 'string' || !url.startsWith('file:') || !url.includes('loading.html') || quitting) return;
    coreRestarts = [];
    if (!(await healthy())) { try { spawnCore(); } catch (error) { console.error(error); return; } }
    if (await waitForBridge()) await window.loadURL(consoleURL).catch(() => {});
    else await window.loadFile(path.join(__dirname, 'loading.html'), { query: { error: `核心仍然无法启动。请检查本机 ${consolePort} 端口是否被其他程序占用。`, retry: '1' } }).catch(() => {});
  });
}

async function startPluginSystem() {
  // The window must exist before plugin callbacks can send UI events. Finish
  // binding the authenticated loopback hook before starting a new core so the
  // first local run receives the same plugin configuration as later restarts.
  pluginSystem = await require('./plugin-ipc.cjs').init({ app, getWindow: () => window, getBall: () => ball, consoleURL, showWindow, localToken });
  const { hookPort } = pluginSystem;
  pluginCoreLink = require('./plugin-core-link.cjs').createCoreLink({ consoleURL, hookPort, token: localToken, onState: state => pluginSystem?.setConnection(state) });
  process.env.SALCARA_PLUGIN_HOOK_PORT = String(hookPort);
  process.env.SALCARA_PLUGIN_HOOK_TOKEN = localToken;
  void pluginCoreLink.start();
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
  // Known download pages and the project repository open in the system browser.
  const external = new Set(['https://chatgpt.com/codex', 'https://claude.ai/download', 'https://git-scm.com/download/win', 'https://github.com/dkdjndbfj-wq/salcara-desktop']);
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
      SALCARA_UPDATE_POLICY: JSON.stringify(updatePolicy.corePolicy(getUpdater().state())),
      // Plugin hook server — set by plugin-ipc.cjs after init(); absent on very
      // first launch (before the hook server is up) but present on restarts.
      ...(process.env.SALCARA_PLUGIN_HOOK_PORT  ? { SALCARA_PLUGIN_HOOK_PORT:  process.env.SALCARA_PLUGIN_HOOK_PORT  } : {}),
      ...(process.env.SALCARA_PLUGIN_HOOK_TOKEN ? { SALCARA_PLUGIN_HOOK_TOKEN: process.env.SALCARA_PLUGIN_HOOK_TOKEN } : {}),
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
  if (ball && !ball.isDestroyed()) loadBallPage(ball);
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
  let lastUpdateMenu = '';
  updater = createUpdater({
    app, shell, platform: process.platform, arch: process.arch, execPath: process.execPath, isPackaged: app.isPackaged,
    repo: updateRepo(), publicKey: String(require('./package.json').salcaraUpdatePublicKey || ''),
    controlURL: String(require('./package.json').salcaraUpdateControlURL || ''), userData: app.getPath('userData'),
    onChange(state) {
      broadcastUpdateState(state);
      void syncUpdatePolicy();
      const menu = `${state.phase}:${state.version || ''}:${state.required}`;
      if (menu !== lastUpdateMenu) { lastUpdateMenu = menu; updateTrayMenu(); }
    },
    beforeInstall: stopForUpdate,
    commitInstall: commitForUpdate,
    installAborted: abortForUpdate,
    recoverAtLogin: true,
  });
  return updater;
}
function updateSummary() {
  return updatePolicy.summary({ ...getUpdater().state(), policyError }, Boolean(updateRepo()));
}
function broadcastUpdateState(state = getUpdater().state()) {
  if (updateWin && !updateWin.isDestroyed()) updateWin.webContents.send('upd:state', { ...state, policyError });
  if (window && !window.isDestroyed()) window.webContents.send('update:state', updatePolicy.summary({ ...state, policyError }, Boolean(updateRepo())));
}
async function syncUpdatePolicy(force = false) {
  // An unknown/offline/unsigned check has no authority to clear a requirement
  // already held by an attached core. Only publish verified release policy.
  if (!getUpdater().state().policyVerified) return;
  if (!policySync) policySync = updatePolicy.createPolicySync({
    async apply(policy) {
      const cookies = await require('electron').session.defaultSession.cookies.get({ url: consoleURL, name: 'salcara_console' });
      if (cookies.length !== 1) throw new Error('Core session unavailable');
      const headers = { 'Content-Type': 'application/json', Origin: consoleURL.slice(0, -1), Cookie: `salcara_console=${cookies[0].value}` };
      const identityResponse = await fetch(consoleURL + 'api/update/policy', { headers, signal: AbortSignal.timeout(5000) });
      const identity = await identityResponse.json();
      if (!identityResponse.ok || !Number.isInteger(identity.pid) || !/^\d+\.\d+\.\d+(?:-test\.\d+)?$/.test(identity.version)) throw new Error('Core version unavailable');
      const coreRequired = Boolean(policy.minimumSupportedVersion && require('./updater.cjs').newer(policy.minimumSupportedVersion, identity.version));
      const response = await fetch(consoleURL + 'api/update/policy', { method: 'POST', headers, signal: AbortSignal.timeout(5000),
        body: JSON.stringify({ pid: identity.pid, version: identity.version, ...policy, required: coreRequired }) });
      if (!response.ok || (await response.json()).ok !== true) throw new Error('Core policy rejected');
    },
    onError(message) { if (policyError !== message) { policyError = message; broadcastUpdateState(); } },
  });
  return policySync.sync(updatePolicy.corePolicy(getUpdater().state()), force);
}
function updateTrayItems() {
  if (!updater) return [];
  const st = updater.state();
  if (st.required) return [{ label: `需要更新至 ${st.version}（当前版本不再支持）`, click: () => openUpdateWindow(false) }];
  if (st.phase === 'waiting') return [{ label: '更新已下载，等待任务结束后自动安装', click: () => openUpdateWindow(false) }];
  if (st.phase === 'ready') return [{ label: `重启以完成更新 ${st.version}`, click: () => openUpdateWindow(false) }];
  if (st.phase === 'downloading' || st.phase === 'verifying') return [{ label: `正在下载更新 ${st.version}…`, click: () => openUpdateWindow(false) }];
  if (st.phase === 'available') return [{ label: `有新版本 ${st.version}`, click: () => openUpdateWindow(false) }];
  return [];
}
async function checkForUpdate(manual) {
  getUpdater().cleanup();
  const st = await getUpdater().check(manual);
  // Found by the periodic check: open the window once per version, without taking focus, unless skipped.
  await syncUpdatePolicy();
  const key = updatePolicy.announcementKey(st);
  if (!manual && (st.phase === 'available' || st.required) && key && announced !== key && !getUpdater().skipped(st.version)) {
    announced = key;
    openUpdateWindow(!st.required);
  }
  if (['available', 'ready', 'waiting'].includes(st.phase) && st.canInstall && !st.autoPaused && !st.autoBlocked && !st.distributionReason && (manual || !getUpdater().skipped(st.version))) void getUpdater().startAutoUpdate({ manual: false });
  return st;
}
function openUpdateWindow(quiet) {
  if (updateWin && !updateWin.isDestroyed()) {
    if (!quiet) { updateWin.show(); updateWin.focus(); }
    return;
  }
  updateWin = new BrowserWindow({
    title: '软件更新', width: 480, height: 650, resizable: false, maximizable: false, minimizable: false, fullscreenable: false,
    frame: false, roundedCorners: true, show: false, backgroundColor: windowBackground(), icon: nativeImage.createFromPath(appIcon()),
    webPreferences: { nodeIntegration: false, contextIsolation: true, sandbox: true, webviewTag: false, preload: path.join(__dirname, 'preload.cjs') },
  });
  updateWin.setMenuBarVisibility(false);
  // Use the very same CSS as the home screen; packaged builds carry that source
  // as a resource, and development reads it directly from the console.
  const orbStyle = fs.readFileSync(app.isPackaged ? path.join(process.resourcesPath, 'home-orb.css') : path.resolve(__dirname, '../internal/console/web/home-orb.css'), 'utf8');
  updateWin.webContents.on('dom-ready', () => { if (updateWin && !updateWin.isDestroyed()) void updateWin.webContents.insertCSS(orbStyle); });
  updateWin.webContents.setWindowOpenHandler(() => ({ action: 'deny' }));
  updateWin.webContents.on('will-navigate', (event) => event.preventDefault());
  updateWin.once('ready-to-show', () => { if (quiet) updateWin.showInactive(); else updateWin.show(); });
  updateWin.on('closed', () => { updateWin = null; });
  updateWin.loadFile(path.join(__dirname, 'update.html'));
  updateWin.webContents.once('did-finish-load', () => broadcastUpdateState());
}
/** Before the folders are swapped: stop the core and let the app quit. */
async function stopForUpdate() {
  // Never kill an attached core, somebody else's process or an active task.
  // The owning core atomically closes admission only after all its tasks finish.
  const child = bridge;
  if (!child || child.exitCode !== null || attached) throw new Error('核心正在由其他进程管理，请退出原程序后再更新');
  const cookies = await require('electron').session.defaultSession.cookies.get({ url: consoleURL, name: 'salcara_console' });
  if (cookies.length !== 1) throw new Error('无法验证核心连接，请稍后重试');
  updateOwner = { child, cookie: cookies[0].value };
  await updateRequest('prepare');
}

async function updateRequest(action) {
  if (!updateOwner) throw new Error('没有安全的更新准备');
  const response = await fetch(consoleURL + 'api/update/' + action, {
      method: 'POST', signal: AbortSignal.timeout(5000),
      headers: { 'Content-Type': 'application/json', Origin: consoleURL.slice(0, -1), Cookie: `salcara_console=${updateOwner.cookie}` },
      body: JSON.stringify({ pid: updateOwner.child.pid, version: app.getVersion() }),
    });
  const body = await response.json();
  if (!response.ok || body.ok !== true) {
    const error = new Error(body.error || '核心未允许更新');
    error.updateBusy = action === 'prepare' && response.status === 409 && body.code === 'update_busy';
    throw error;
  }
}

async function commitForUpdate() {
  const child = updateOwner && updateOwner.child;
  if (!child || child !== bridge) throw new Error('核心身份已改变，未安装更新');
  quitting = true;
  try {
    await updateRequest('commit');
    for (let i = 0; i < 75 && child.exitCode === null; i++) await new Promise((r) => setTimeout(r, 200));
    if (child.exitCode === null) throw new Error('核心仍未安全退出，未安装更新');
    clearInterval(attachTimer);
  } catch (error) {
    quitting = false;
    throw error;
  }
}

async function abortForUpdate() {
  const owner = updateOwner;
  if (owner && owner.child.exitCode === null) await updateRequest('cancel').catch(() => {});
  updateOwner = null;
  quitting = false;
  if ((!bridge || bridge.exitCode !== null) && !(await healthy())) spawnCore();
}
function reportUpdateResult() {
  const done = getUpdater().afterRestart();
  if (!done) return;
  if (done.ok) notify(`已更新到 ${app.getVersion()}`, '新版本已经在运行。');
  else notify('更新已回退', `仍在使用 ${app.getVersion()}，已停止对失败版本的自动重试。可以在「设置 → 版本更新」中查看详情。`);
}

async function start() {
  startHidden = process.argv.includes('--background');
  if (process.platform === 'win32') app.setAppUserModelId(testBuild ? 'top.salcara.bridge.test' : 'top.salcara.bridge');
  ballEnabled = readBallState().enabled !== false;
  applyAppearance(readAppearance());
  registerIpc();
  createWindow();
  createTray();
  getUpdater().cleanup();
  if (getUpdater().state().required) { announced = updatePolicy.announcementKey(getUpdater().state()); openUpdateWindow(false); }
  setTimeout(() => void checkForUpdate(false), 20000);
  setInterval(() => void checkForUpdate(false), 2 * 3600 * 1000);
  setInterval(() => { if (policyError || getUpdater().state().required) void syncUpdatePolicy(true); }, 10000);
  try { await startPluginSystem(); }
  catch (error) { console.error('[plugins] init failed:', error); }
  if (await healthy()) attachToRunningCore();
  else spawnCore();
  if (!(await waitForBridge())) {
    await window.loadFile(path.join(__dirname, 'loading.html'), { query: { error: `核心启动失败。请检查本机 ${consolePort} 端口是否被占用。`, retry: '1' } });
    if (startHidden) showWindow();
    return;
  }
  void pluginCoreLink?.start();
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
  await syncUpdatePolicy(true);
  getUpdater().cleanup();
  reportUpdateResult();
  setTimeout(() => getUpdater().cleanup(), 5000);
}

app.on('before-quit', event => {
  if (!pluginQuitDone && pluginSystem) {
    event.preventDefault();
    if (pluginQuitStarted) return;
    pluginQuitStarted = true; quitting = true;
    void (async () => {
      try { await pluginCoreLink?.stop(); await require('./plugin-ipc.cjs').close(); }
      catch (error) { console.error('[plugins] shutdown failed:', error); }
      finally { pluginQuitDone = true; app.quit(); }
    })();
    return;
  }
  quitting = true;
  cancelBallRecovery();
  clearInterval(attachTimer);
  if (bridge && bridge.exitCode === null) bridge.kill();
});
