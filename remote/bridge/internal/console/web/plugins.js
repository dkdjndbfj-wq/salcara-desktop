'use strict';

PAGES.plugins = { title: '插件', sub: '查看并管理运行在本机的扩展' };

const PLUGIN_PAGE = { records: null, seq: 0, changes: 0, pending: new Set(), installing: false, loadError: false, error: '' };
const PLUGIN_UI = { revision: -1, panels: [], satellites: [], openPanel: null, connection: null, loading: false, pending: new Set() };
const PLUGIN_PERMISSIONS = {
  'api.intercept': ['API 请求与事件', 'risk'],
  'ui.panel': ['插件面板', ''],
  'ui.satellite': ['悬浮球入口', ''],
  storage: ['独立本地存储', ''],
  events: ['会话事件', ''],
  notify: ['系统通知', ''],
};

function pluginPermissionHTML(permissions) {
  const list = Array.isArray(permissions) ? permissions : [];
  if (!list.length) return '<span class="plugin-permission none">未声明权限</span>';
  return list.map(permission => {
    const known = PLUGIN_PERMISSIONS[permission];
    const label = known ? known[0] : String(permission || '未知权限');
    const tone = known ? known[1] : 'risk';
    return `<span class="plugin-permission ${tone}" title="${esc(permission)}">${esc(label)}</span>`;
  }).join('');
}

function pluginStatus(plugin) {
  if (!plugin.enabled) return ['停用', 'off'];
  if (plugin.status === 'error') return ['运行错误', 'error'];
  if (plugin.status === 'loading') return ['启动中', 'loading'];
  if (plugin.status === 'disabled') return ['停用', 'off'];
  return plugin.status === 'ok' ? ['运行中', 'ok'] : ['状态未知', 'off'];
}

function pluginRowHTML(plugin) {
  const id = String(plugin.id || '');
  const pending = PLUGIN_PAGE.pending.has(id);
  const enabled = plugin.enabled === true;
  const [status, tone] = pluginStatus(plugin);
  const author = plugin.author ? ` · ${plugin.author}` : '';
  const error = plugin.status === 'error' && plugin.error
    ? `<div class="plugin-error">${ico('warn')}<span>${esc(plugin.error)}</span></div>` : '';
  return `<article class="plugin-row${plugin.status === 'error' ? ' has-error' : ''}" data-plugin="${esc(id)}">
    <div class="plugin-ident"><span class="plugin-mark">${icon('puzzle')}</span><div class="plugin-copy"><div class="plugin-title"><b title="${esc(plugin.name)}">${esc(plugin.name || id)}</b><span class="plugin-version">v${esc(plugin.version || '—')}</span></div><small>${esc(plugin.id || '')}${esc(author)}</small>${plugin.description ? `<p>${esc(plugin.description)}</p>` : ''}</div></div>
    <div class="plugin-permissions" aria-label="插件声明的权限">${pluginPermissionHTML(plugin.permissions)}</div>
    ${error}
    ${pluginEntriesHTML(plugin)}
    <div class="plugin-controls"><span class="plugin-state ${tone}"><i></i>${status}</span><label class="switch" title="${enabled ? '停用插件' : '启用插件'}"><input type="checkbox" data-plugin-toggle="${esc(id)}" aria-label="${esc((enabled ? '停用 ' : '启用 ') + (plugin.name || id))}" ${enabled ? 'checked' : ''} ${pending ? 'disabled' : ''}><span></span></label><button type="button" class="icon-btn danger-text" data-act="pluginUninstall" data-id="${esc(id)}" aria-label="卸载 ${esc(plugin.name || id)}" title="卸载" ${pending ? 'disabled' : ''}>${icon('trash')}</button></div>
  </article>`;
}

function pluginBridge() {
  return typeof window !== 'undefined' && window.salcaraPlugins ? window.salcaraPlugins : null;
}

function pluginEntriesHTML(plugin) {
  if (!plugin.enabled || plugin.status !== 'ok') return '';
  const panels = PLUGIN_UI.panels.filter(p => p.pluginId === plugin.id);
  const satellites = PLUGIN_UI.satellites.filter(p => p.pluginId === plugin.id && !panels.some(panel => panel.panelId === p.panelId && panel.label === p.label));
  if (!panels.length && !satellites.length) return '';
  return `<div class="plugin-entries">${panels.map(p => `<button type="button" class="btn sm" data-act="pluginPanelOpen" data-plugin="${esc(p.pluginId)}" data-panel="${esc(p.panelId)}">${icon('puzzle')}${esc(p.label)}</button>`).join('')}${satellites.map(p => `<button type="button" class="btn sm ghost" data-act="pluginUIAction" data-plugin="${esc(p.pluginId)}" data-kind="satellite" data-target="${esc(p.satId)}" ${PLUGIN_UI.pending.has(JSON.stringify([p.pluginId, 'satellite', p.satId, ''])) ? 'disabled' : ''}>${esc(p.label)}</button>`).join('')}</div>`;
}
function pluginCurrentPanel() {
  const open = PLUGIN_UI.openPanel;
  return open && PLUGIN_UI.panels.find(p => p.pluginId === open.pluginId && p.panelId === open.panelId);
}
function pluginPanelHTML(panel) {
  const owner = PLUGIN_PAGE.records?.find(p => p.id === panel.pluginId);
  return `<section class="plugin-panel"><header><span class="plugin-mark">${icon('puzzle')}</span><div><h2>${esc(panel.title || panel.label)}</h2><p>${esc(owner?.name || panel.pluginId)}</p></div></header><div class="plugin-panel-content">${esc(panel.content || '这个插件还没有显示内容。')}</div><div class="plugin-panel-actions">${(panel.actions || []).map(action => {
    const pending = PLUGIN_UI.pending.has(JSON.stringify([panel.pluginId, 'panel', panel.panelId, action.id]));
    return `<button type="button" class="btn" data-act="pluginUIAction" data-plugin="${esc(panel.pluginId)}" data-kind="panel" data-target="${esc(panel.panelId)}" data-action="${esc(action.id)}" ${pending ? 'disabled' : ''}>${pending ? '<span class="spin"></span>' : ''}${esc(action.label)}</button>`;
  }).join('')}</div></section>`;
}
function applyPluginUI(state) {
  if (!state || !Number.isSafeInteger(state.revision) || state.revision <= PLUGIN_UI.revision) return;
  const previous = JSON.stringify(PLUGIN_UI.openPanel);
  PLUGIN_UI.revision = state.revision;
  PLUGIN_UI.panels = Array.isArray(state.panels) ? state.panels : [];
  PLUGIN_UI.satellites = Array.isArray(state.satellites) ? state.satellites : [];
  PLUGIN_UI.openPanel = state.openPanel || null;
  PLUGIN_UI.connection = state.connection || null;
  PLUGIN_UI.loading = state.loading === true;
  if (pluginCurrentPanel() && previous !== JSON.stringify(PLUGIN_UI.openPanel)) location.hash = 'plugins';
  if (S.route === 'plugins') renderPluginPage();
}

function renderPluginPage() {
  if (S.route !== 'plugins') return;
  const view = $('#view');
  if (!view) return;
  const bridge = pluginBridge();
  const canInstall = typeof bridge?.inspect === 'function' && typeof bridge?.install === 'function'
    && typeof window.salcaraWindow?.choosePluginDirectory === 'function';
  const error = PLUGIN_PAGE.error ? `<div class="callout bad">${ico('warn')}<div>${esc(PLUGIN_PAGE.error)}</div></div>` : '';
  if (!bridge || typeof bridge.list !== 'function') {
    view.innerHTML = `<div class="callout warn">${ico('info')}<div>当前窗口不支持桌面插件管理，请在 Salcara Bridge 桌面端打开此页面。</div></div>`;
    return;
  }
  const selected = pluginCurrentPanel();
  const head = $('#headActions');
  if (head) head.innerHTML = selected ? '<button type="button" class="btn" data-act="pluginPanelClose">返回插件列表</button>' : `<button type="button" class="btn primary" data-act="pluginInstall" ${canInstall && !PLUGIN_PAGE.installing ? '' : 'disabled'}>${PLUGIN_PAGE.installing ? '<span class="spin"></span>处理中' : icon('plus') + '安装插件'}</button>`;
  const connection = PLUGIN_UI.connection?.state === 'offline' ? `<div class="callout warn">${ico('info')}<div>${esc(PLUGIN_UI.connection.error || '插件业务连接暂不可用')}，正在自动重连。</div></div>` : '';
  if (selected) { view.innerHTML = `${error}${connection}${pluginPanelHTML(selected)}`; return; }
  const installNote = canInstall ? '' : `<div class="callout warn">${ico('info')}<div>此桌面版本没有插件目录选择能力，请更新 Salcara Bridge 后安装插件。</div></div>`;
  const records = PLUGIN_PAGE.records;
  const content = records === null
    ? PLUGIN_PAGE.loadError ? '<div class="plugin-retry"><span>插件列表暂时无法读取</span><button type="button" class="btn" data-act="pluginRefresh">重试</button></div>' : '<div class="plugin-loading"><span class="spin"></span><span>正在读取插件…</span></div>'
    : records.length
      ? `<section class="plugin-list" aria-label="已安装插件">${records.map(pluginRowHTML).join('')}</section>`
      : PLUGIN_UI.loading ? '<div class="plugin-loading"><span class="spin"></span><span>正在加载已安装插件…</span></div>' : `<section class="plugin-empty"><span class="plugin-empty-mark">${icon('puzzle')}</span><b>还没有安装插件</b><button type="button" class="btn primary" data-act="pluginInstall" ${canInstall && !PLUGIN_PAGE.installing ? '' : 'disabled'}>${icon('plus')}安装插件</button></section>`;
  view.innerHTML = `${error}${connection}${installNote}<div class="callout plugin-trust">${ico('lock')}<div>插件代码会在本机运行。权限为插件自行声明，请只安装信任来源的插件。</div></div>${content}`;
  const installButton = $('#headActions')?.querySelector('[data-act="pluginInstall"]');
  if (installButton) {
    installButton.disabled = !canInstall || PLUGIN_PAGE.installing;
    installButton.innerHTML = `${PLUGIN_PAGE.installing ? '<span class="spin"></span>' : icon('plus')}${PLUGIN_PAGE.installing ? '处理中' : '安装插件'}`;
  }
}

function applyPluginSnapshot(records) {
  if (!Array.isArray(records)) return;
  PLUGIN_PAGE.records = records.filter(record => record && typeof record === 'object');
  PLUGIN_PAGE.loadError = false;
  PLUGIN_PAGE.error = '';
  if (S.route === 'plugins') renderPluginPage();
}

async function loadPluginList(view) {
  const bridge = pluginBridge();
  if (!bridge || typeof bridge.list !== 'function') return renderPluginPage();
  const seq = ++PLUGIN_PAGE.seq;
  const changeRevision = PLUGIN_PAGE.changes;
  try {
    const records = await bridge.list();
    if (seq !== PLUGIN_PAGE.seq || changeRevision !== PLUGIN_PAGE.changes || S.route !== 'plugins' || view.isConnected === false) return;
    applyPluginSnapshot(records);
  } catch (error) {
    if (seq !== PLUGIN_PAGE.seq || changeRevision !== PLUGIN_PAGE.changes || S.route !== 'plugins' || view.isConnected === false) return;
    PLUGIN_PAGE.error = error?.message || String(error);
    PLUGIN_PAGE.loadError = true;
    renderPluginPage();
  }
}

RENDER.plugins = function (view) {
  const bridge = pluginBridge();
  const canInstall = typeof bridge?.inspect === 'function' && typeof bridge?.install === 'function'
    && typeof window.salcaraWindow?.choosePluginDirectory === 'function';
  $('#headActions').innerHTML = `<button type="button" class="btn primary" data-act="pluginInstall" ${canInstall && !PLUGIN_PAGE.installing ? '' : 'disabled'}>${icon('plus')}安装插件</button>`;
  renderPluginPage();
  return loadPluginList(view);
};

async function refreshPluginList() {
  const bridge = pluginBridge();
  if (!bridge || typeof bridge.list !== 'function') return;
  const seq = ++PLUGIN_PAGE.seq, changes = PLUGIN_PAGE.changes;
  const records = await bridge.list();
  if (seq === PLUGIN_PAGE.seq && changes === PLUGIN_PAGE.changes) applyPluginSnapshot(records);
}

async function changePlugin(id, operation) {
  const bridge = pluginBridge();
  if (!bridge || PLUGIN_PAGE.pending.has(id)) return;
  const plugin = (PLUGIN_PAGE.records || []).find(record => record.id === id);
  if (!plugin) return;
  PLUGIN_PAGE.pending.add(id);
  renderPluginPage();
  try {
    if (operation === 'enable') {
      const permissions = (Array.isArray(plugin.permissions) ? plugin.permissions : []).map(permission => PLUGIN_PERMISSIONS[permission]?.[0] || permission).join('、') || '未声明权限';
      if (!await confirm(`启用「${plugin.name || id}」？\n\n版本：${plugin.version || '未知'}\n声明权限：${permissions}\n\n启用后插件代码会在本机运行；仅凭权限声明不能验证插件来源或行为。`)) return;
    }
    if (operation === 'uninstall' && !await confirm(`卸载「${plugin.name || id}」？\n\n这会删除插件文件和该插件的本地存储。`)) return;
    if (operation === 'enable') await bridge.enable(id);
    else if (operation === 'disable') await bridge.disable(id);
    else await bridge.uninstall(id);
    await refreshPluginList();
    const updated = PLUGIN_PAGE.records?.find(record => record.id === id);
    if (operation === 'enable' && updated?.status === 'error') {
      PLUGIN_PAGE.error = updated.error || '插件启动失败。';
      toast('插件启动失败', 'bad');
    } else {
      toast(operation === 'uninstall' ? '插件已卸载' : operation === 'enable' ? '插件已启用' : '插件已停用', 'ok');
    }
  } catch (error) {
    PLUGIN_PAGE.error = error?.message || String(error);
    toast(PLUGIN_PAGE.error, 'bad');
    try { await refreshPluginList(); } catch { renderPluginPage(); }
  } finally {
    PLUGIN_PAGE.pending.delete(id);
    renderPluginPage();
  }
}

Object.assign(ACTIONS, {
  pluginInstall: async () => {
    const bridge = pluginBridge();
    const chooser = window.salcaraWindow?.choosePluginDirectory;
    if (PLUGIN_PAGE.installing) return;
    if (!bridge?.inspect || !bridge?.install || typeof chooser !== 'function') {
      PLUGIN_PAGE.error = '当前桌面版本不支持安装插件，请更新后重试。';
      renderPluginPage();
      return;
    }
    const owner = S.routeRevision;
    const current = () => S.route === 'plugins' && S.routeRevision === owner;
    PLUGIN_PAGE.installing = true;
    PLUGIN_PAGE.error = '';
    renderPluginPage();
    try {
      const dir = await chooser();
      if (!dir || !current()) return;
      const manifest = await bridge.inspect(dir);
      if (!current()) return;
      if (!manifest || typeof manifest !== 'object') throw new Error('无法读取插件清单');
      const permissions = (Array.isArray(manifest.permissions) ? manifest.permissions : []).map(permission => PLUGIN_PERMISSIONS[permission]?.[0] || permission).join('、') || '未声明权限';
      const description = String(manifest.description || '').replace(/[\r\n]+/g, ' ').slice(0, 160);
      const details = [`版本：${manifest.version}`, `作者：${manifest.author || '未提供'}`, ...(description ? [`说明：${description}`] : []), `声明权限：${permissions}`].join('\n');
      if (!await confirm(`安装并启用「${manifest.name}」？\n\n${details}\n\n插件代码会在本机运行。权限由插件自行声明，只安装信任来源的插件。`)) return;
      const result = await bridge.install(dir, manifest.packageDigest);
      await refreshPluginList();
      const installed = PLUGIN_PAGE.records?.find(record => record.id === result?.id);
      if (installed?.status === 'error') {
        PLUGIN_PAGE.error = installed.error || '插件已安装，但启动失败。';
        toast('插件已安装，但启动失败', 'bad');
      } else toast('插件已安装并启用', 'ok');
    } catch (error) {
      PLUGIN_PAGE.error = error?.message || String(error);
      toast(PLUGIN_PAGE.error, 'bad');
    } finally {
      PLUGIN_PAGE.installing = false;
      renderPluginPage();
    }
  },
  pluginPanelOpen: async button => {
    try { await pluginBridge()?.openPanel(button.dataset.plugin, button.dataset.panel); }
    catch (error) { toast(error.message || String(error), 'bad'); }
  },
  pluginPanelClose: async () => {
    const panel = pluginCurrentPanel();
    if (!panel) return;
    try { await pluginBridge()?.closePanel(panel.pluginId, panel.panelId); }
    catch (error) { toast(error.message || String(error), 'bad'); }
  },
  pluginUIAction: async button => {
    const { plugin, kind, target, action = '' } = button.dataset;
    const key = JSON.stringify([plugin, kind, target, action]);
    if (PLUGIN_UI.pending.has(key)) return;
    PLUGIN_UI.pending.add(key); renderPluginPage();
    try { await pluginBridge()?.invokeAction(plugin, kind, target, action); }
    catch (error) { toast(error.message || String(error), 'bad'); }
    finally { PLUGIN_UI.pending.delete(key); renderPluginPage(); }
  },
  pluginRefresh: () => {
    const view = $('#view');
    if (!view) return;
    PLUGIN_PAGE.error = '';
    PLUGIN_PAGE.loadError = false;
    PLUGIN_PAGE.records = null;
    renderPluginPage();
    return loadPluginList(view);
  },
  pluginToggle: input => changePlugin(input.dataset.pluginToggle, input.checked ? 'enable' : 'disable'),
  pluginUninstall: button => changePlugin(button.dataset.id, 'uninstall'),
});

document.addEventListener('change', event => {
  const input = event.target?.dataset?.pluginToggle;
  if (input === undefined) return;
  void ACTIONS.pluginToggle(event.target);
});

try {
  const bridge = pluginBridge();
  bridge?.onUIChanged?.(applyPluginUI);
  bridge?.onToast?.(event => { if (event && typeof event.message === 'string') toast(event.message, ['ok', 'bad'].includes(event.type) ? event.type : ''); });
  if (bridge?.uiState) Promise.resolve(bridge.uiState()).then(applyPluginUI).catch(() => {});
  bridge?.onListChanged?.(records => {
    PLUGIN_PAGE.changes++;
    applyPluginSnapshot(records);
  });
} catch { /* the management page still refreshes through list() */ }
