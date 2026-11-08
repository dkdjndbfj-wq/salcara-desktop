// Bridges the console pages to the desktop shell. Only small, fixed commands
// cross; the page never gets Node or Electron objects.
const { contextBridge, ipcRenderer } = require('electron');

contextBridge.exposeInMainWorld('salcaraWindow', {
  minimize: () => ipcRenderer.send('win:minimize'),
  close: () => ipcRenderer.send('win:close'), // hides to the floating ball; the Bridge keeps running
  show: (route) => ipcRenderer.send('win:show', typeof route === 'string' ? route : ''),
  state: () => ipcRenderer.invoke('win:state'),
  chooseDirectory: (initial) => ipcRenderer.invoke('win:choose-directory', typeof initial === 'string' ? initial : ''),
  choosePluginDirectory: () => ipcRenderer.invoke('win:choose-plugin-directory'),
  notify: (title, body, route) => ipcRenderer.send('notify', { title: String(title || ''), body: String(body || ''), route: typeof route === 'string' ? route : '' }),
  updateStatus: () => ipcRenderer.invoke('update:status'),
  checkUpdate: () => ipcRenderer.invoke('update:check'),
  openUpdate: () => ipcRenderer.send('update:open'),
  onUpdateState: fn => pluginSubscription('update:state', fn),
  setTheme: (theme) => ipcRenderer.send('win:theme', ['system', 'light', 'dark'].includes(theme) ? theme : 'system'),
});

contextBridge.exposeInMainWorld('salcaraCore', {
  retry: () => ipcRenderer.send('core:retry'),
});

contextBridge.exposeInMainWorld('salcaraBall', {
  show: (route) => ipcRenderer.send('ball:show', typeof route === 'string' ? route : ''),
  setInteractive: (on) => ipcRenderer.send('ball:interactive', !!on),
  dragStart: () => ipcRenderer.send('ball:drag', 'start'),
  drag: () => ipcRenderer.send('ball:drag', 'move'),
  dragEnd: () => ipcRenderer.send('ball:drag', 'end'),
  dragCancel: () => ipcRenderer.send('ball:drag', 'cancel'),
  onDragFinished: (fn) => {
    if (typeof fn !== 'function') return () => {};
    const listener = (_event, interactive) => fn(interactive === true);
    ipcRenderer.on('ball:drag-finished', listener);
    return () => ipcRenderer.removeListener('ball:drag-finished', listener);
  },
  onLayout: (fn) => pluginSubscription('ball:layout', fn),
  onShown: (fn) => {
    if (typeof fn !== 'function') return () => {};
    const listener = (_event, interactive) => fn(interactive === true);
    ipcRenderer.on('ball:shown', listener);
    return () => ipcRenderer.removeListener('ball:shown', listener);
  },
});

// Every subscription returns a disposer; reloading a view cannot stack listeners.
function pluginSubscription(channel, callback) {
  if (typeof callback !== 'function') return () => {};
  const listener = (_event, value) => callback(value);
  ipcRenderer.on(channel, listener);
  return () => ipcRenderer.removeListener(channel, listener);
}
contextBridge.exposeInMainWorld('salcaraPlugins', {
  list: () => ipcRenderer.invoke('plugin:list'),
  inspect: dir => ipcRenderer.invoke('plugin:inspect', { dir: String(dir) }),
  install: (dir, packageDigest) => ipcRenderer.invoke('plugin:install', { dir: String(dir), packageDigest }),
  enable: id => ipcRenderer.invoke('plugin:enable', { id: String(id) }),
  disable: id => ipcRenderer.invoke('plugin:disable', { id: String(id) }),
  uninstall: id => ipcRenderer.invoke('plugin:uninstall', { id: String(id) }),
  uiState: () => ipcRenderer.invoke('plugin:ui:state'),
  openPanel: (pluginId, panelId) => ipcRenderer.invoke('plugin:panel:open', { pluginId: String(pluginId), panelId: String(panelId) }),
  closePanel: (pluginId, panelId) => ipcRenderer.invoke('plugin:panel:close', { pluginId: String(pluginId), panelId: String(panelId) }),
  invokeAction: (pluginId, kind, targetId, actionId = '') => ipcRenderer.invoke('plugin:action', { pluginId: String(pluginId), kind: String(kind), targetId: String(targetId), actionId: String(actionId) }),
  onListChanged: fn => pluginSubscription('plugin:list:changed', fn),
  onUIChanged: fn => pluginSubscription('plugin:ui:changed', fn),
  onToast: fn => pluginSubscription('plugin:toast', fn),
});

// The update window (desktop/update.html). The shell checks that calls come from that window.
contextBridge.exposeInMainWorld('salcaraUpdate', {
  state: () => ipcRenderer.invoke('upd:state'),
  onState: (fn) => ipcRenderer.on('upd:state', (_event, value) => fn(value && typeof value === 'object' ? value : null)),
  check: () => ipcRenderer.send('upd:check'),
  download: () => ipcRenderer.send('upd:download'),
  cancel: () => ipcRenderer.send('upd:cancel'),
  install: () => ipcRenderer.send('upd:install'),
  later: () => ipcRenderer.send('upd:later'),
  skip: () => ipcRenderer.send('upd:skip'),
  openPage: () => ipcRenderer.send('upd:page'),
  copyDiagnostics: () => ipcRenderer.invoke('upd:diagnostics'),
});
