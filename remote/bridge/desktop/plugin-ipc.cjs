'use strict';

// Desktop-local plugin host. UI state is replayable, so plugins loaded before a
// renderer (or before the floating window exists) do not lose their entries.
const http = require('node:http');
const path = require('node:path');
const { timingSafeEqual } = require('node:crypto');
const { PluginLoader } = require('./plugin-loader.cjs');
const keyOf = (pluginId, id) => JSON.stringify([pluginId, id]);

function trustedFrame(event, win, consoleURL, ball = false) {
  if (!event || !win || win.isDestroyed() || event.sender !== win.webContents || event.senderFrame !== win.webContents.mainFrame) return false;
  try {
    const url = new URL(event.senderFrame.url), base = new URL(consoleURL);
    return url.origin === base.origin && (ball ? url.pathname === '/ball.html' : ['/', '/index.html'].includes(url.pathname));
  } catch { return false; }
}
function send(win, channel, value) {
  if (win && !win.isDestroyed() && !win.webContents.isDestroyed()) win.webContents.send(channel, value);
}
function tokenMatches(value, expected) {
  if (typeof value !== 'string' || typeof expected !== 'string' || !expected) return false;
  const a = Buffer.from(value), b = Buffer.from(expected);
  return a.length === b.length && timingSafeEqual(a, b);
}
function readBody(req) {
  return new Promise((resolve, reject) => {
    const chunks = []; let bytes = 0;
    req.on('data', chunk => {
      bytes += chunk.length;
      if (bytes > 4 * 1024 * 1024) { reject(new Error('Plugin payload is too large')); req.destroy(); return; }
      chunks.push(chunk);
    });
    req.on('end', () => { try { resolve(JSON.parse(Buffer.concat(chunks).toString('utf8'))); } catch { reject(new Error('Invalid JSON')); } });
    req.on('error', reject);
    req.on('aborted', () => reject(new Error('Request aborted')));
  });
}

async function createPluginSystem(options) {
  const { app, ipcMain, Notification, localToken, consoleURL, showWindow = () => {} } = options;
  if (typeof localToken !== 'string' || localToken.length < 24) throw new Error('Plugin host requires a private process token');
  const getWindow = options.getWindow || (() => options.window);
  const getBall = options.getBall || (() => options.ball);
  const panels = new Map(), satellites = new Map(), handlers = [];
  let revision = 0, openPanel = null, closed = false, closing = null, loader, loading = true, ready;
  let connection = { state: 'connecting' };
  const snapshot = () => ({ revision, loading, panels: [...panels.values()], satellites: [...satellites.values()], openPanel, connection });
  const publish = () => {
    if (closed) return;
    revision++;
    const state = snapshot();
    send(getWindow(), 'plugin:ui:changed', state); send(getBall(), 'plugin:ui:changed', state);
  };
  const host = {
    registerPanel(pluginId, panelId, opts = {}) {
      if (closed) return;
      panels.set(keyOf(pluginId, panelId), { pluginId, panelId, label: String(opts.label || panelId), icon: String(opts.icon || 'puzzle'), title: String(opts.title || opts.label || panelId), content: String(opts.content || ''), actions: (Array.isArray(opts.actions) ? opts.actions : []).map(a => ({ id: String(a.id), label: String(a.label || a.id) })) });
      publish();
    },
    openPanel(pluginId, panelId) {
      if (closed || !panels.has(keyOf(pluginId, panelId))) return;
      openPanel = { pluginId, panelId }; publish(); showWindow('plugins');
    },
    closePanel(pluginId, panelId) {
      if (openPanel?.pluginId === pluginId && openPanel.panelId === panelId) { openPanel = null; publish(); }
    },
    addSatellite(pluginId, satId, opts = {}) {
      if (closed) return;
      satellites.set(keyOf(pluginId, satId), { pluginId, satId, label: String(opts.label || satId), icon: String(opts.icon || 'puzzle'), panelId: typeof opts.panelId === 'string' ? opts.panelId : '' }); publish();
    },
    removePlugin(pluginId) {
      for (const [key, panel] of panels) if (panel.pluginId === pluginId) panels.delete(key);
      for (const [key, satellite] of satellites) if (satellite.pluginId === pluginId) satellites.delete(key);
      if (openPanel?.pluginId === pluginId) openPanel = null;
      publish();
    },
    showToast(pluginId, message, opts = {}) {
      if (!closed) send(getWindow(), 'plugin:toast', { pluginId, message: String(message).slice(0, 256), type: ['ok', 'bad'].includes(opts.type) ? opts.type : '' });
    },
    sendNotification(pluginId, title, body) {
      if (!closed && Notification?.isSupported()) new Notification({ title: String(title).slice(0, 128), body: String(body).slice(0, 512), silent: false }).show();
    },
  };
  loader = new (options.Loader || PluginLoader)({ pluginsDir: path.join(app.getPath('userData'), 'plugins'), storageDir: path.join(app.getPath('userData'), 'plugin-storage'), host, bridgeVersion: app.getVersion() });
  loader.on('change', records => { if (!closed) send(getWindow(), 'plugin:list:changed', records); });
  const mainAllowed = event => trustedFrame(event, getWindow(), consoleURL);
  const ballAllowed = event => trustedFrame(event, getBall(), consoleURL, true);
  function handle(channel, action, allowBall = false) {
    ipcMain.handle(channel, (event, input) => {
      if (closed || !(mainAllowed(event) || allowBall && ballAllowed(event))) throw new Error('不允许从此窗口操作插件');
      return action(input || {}, event);
    });
    handlers.push(channel);
  }
  const requireId = value => { if (typeof value !== 'string' || !value || value.length > 128) throw new Error('Invalid plugin identifier'); return value; };
  handle('plugin:list', () => loader.list());
  handle('plugin:inspect', ({ dir }) => loader.inspect(dir));
  handle('plugin:install', async ({ dir, packageDigest }) => ({ id: await loader.install(dir, packageDigest) }));
  for (const operation of ['enable', 'disable', 'uninstall']) handle('plugin:' + operation, ({ id }) => loader[operation](requireId(id)));
  handle('plugin:ui:state', snapshot, true);
  handle('plugin:panel:open', ({ pluginId, panelId }) => {
    requireId(pluginId); requireId(panelId);
    if (!panels.has(keyOf(pluginId, panelId))) throw new Error('插件面板已不可用');
    host.openPanel(pluginId, panelId);
  });
  handle('plugin:panel:close', ({ pluginId, panelId }) => host.closePanel(requireId(pluginId), requireId(panelId)));
  handle('plugin:action', ({ pluginId, kind, targetId, actionId }, event) => {
    requireId(pluginId); requireId(targetId);
    if (kind !== 'panel' && kind !== 'satellite') throw new Error('Invalid plugin action');
    if (ballAllowed(event) && kind !== 'satellite') throw new Error('悬浮球只能操作插件快捷入口');
    const record = (kind === 'panel' ? panels : satellites).get(keyOf(pluginId, targetId));
    if (!record || kind === 'panel' && !record.actions.some(a => a.id === actionId)) throw new Error('插件操作已不可用');
    return loader.invokeUIAction(pluginId, kind, targetId, kind === 'panel' ? requireId(actionId) : '');
  }, true);

  const server = http.createServer(async (req, res) => {
    const reply = (status, value) => { if (!res.destroyed) { res.writeHead(status, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' }); res.end(JSON.stringify(value)); } };
    if (closed || !['127.0.0.1', '::1', '::ffff:127.0.0.1'].includes(req.socket.remoteAddress) || !tokenMatches(req.headers['x-plugin-token'], localToken)) return reply(403, { error: 'forbidden' });
    if (req.method !== 'POST') return reply(405, { error: 'POST required' });
    if (!['/plugin/hook/request', '/plugin/hook/response', '/plugin/event'].includes(req.url)) return reply(404, { error: 'not found' });
    let data;
    try { data = await readBody(req); } catch { return reply(400, { error: 'invalid payload' }); }
    if (!data || typeof data !== 'object' || Array.isArray(data)) return reply(400, { error: 'invalid payload' });
    try {
      if (req.url === '/plugin/hook/request') {
        const result = await loader.runRequestHooks({ tool: String(data.tool || ''), body: data.body || {}, headers: data.headers || {} });
        reply(200, { body: result.body, headers: result.headers });
      } else if (req.url === '/plugin/hook/response') {
        const result = await loader.runResponseHooks({ tool: String(data.tool || ''), body: data.requestBody || {} }, { status: Number(data.responseStatus) || 200, body: data.responseBody || {} });
        reply(200, { status: result.status, body: result.body });
      } else {
        if (typeof data.event !== 'string' || data.event.length > 80) return reply(400, { error: 'invalid event' });
        const run = data.event.startsWith('session:') || data.event === 'tool:detect' ? loader.fireSessionEvent(data.event, data.payload || {}) : loader.fireApiEvent(data.event, data.payload || {});
        Promise.resolve(run).catch(() => {});
        reply(200, {});
      }
    } catch { reply(500, { error: 'plugin execution failed' }); }
  });
  server.requestTimeout = 5000; server.headersTimeout = 5000;
  server.timeout = 6000;
  try {
    await new Promise((resolve, reject) => { server.once('error', reject); server.listen(0, '127.0.0.1', resolve); });
    // Loading plugins must never delay starting the core. Registrations are
    // replayed by uiState() even when a renderer subscribes later.
    ready = loader.loadAll().finally(() => { loading = false; publish(); });
    void ready.catch(() => {});
  } catch (error) {
    closed = true; handlers.forEach(name => ipcMain.removeHandler(name)); server.close(); server.closeAllConnections(); await loader.close(); throw error;
  }
  return {
    loader, ready, hookPort: server.address().port, snapshot,
    setConnection(value) { if (JSON.stringify(connection) !== JSON.stringify(value)) { connection = value; publish(); } },
    close() {
      if (closing) return closing;
      closed = true;
      handlers.forEach(name => ipcMain.removeHandler(name));
      closing = (async () => { server.close(); server.closeAllConnections(); await loader.close(); panels.clear(); satellites.clear(); })();
      return closing;
    },
  };
}
let current = null;
async function init(options) {
  if (current) return current;
  current = await createPluginSystem({ ...require('electron'), ...options });
  return current;
}
async function close() { const previous = current; current = null; if (previous) await previous.close(); }
module.exports = { init, close, createPluginSystem, trustedFrame };
