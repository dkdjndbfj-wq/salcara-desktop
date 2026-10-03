// Bridges the console pages to the desktop shell. Only small, fixed commands
// cross; the page never gets Node or Electron objects.
const { contextBridge, ipcRenderer } = require('electron');

contextBridge.exposeInMainWorld('salcaraWindow', {
  minimize: () => ipcRenderer.send('win:minimize'),
  close: () => ipcRenderer.send('win:close'), // hides to the floating ball; the Bridge keeps running
  show: (route) => ipcRenderer.send('win:show', typeof route === 'string' ? route : ''),
  state: () => ipcRenderer.invoke('win:state'),
  notify: (title, body, route) => ipcRenderer.send('notify', { title: String(title || ''), body: String(body || ''), route: typeof route === 'string' ? route : '' }),
  updateStatus: () => ipcRenderer.invoke('update:status'),
  checkUpdate: () => ipcRenderer.invoke('update:check'),
  openUpdate: () => ipcRenderer.send('update:open'),
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
  onShown: (fn) => ipcRenderer.on('ball:shown', () => fn()),
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
});
