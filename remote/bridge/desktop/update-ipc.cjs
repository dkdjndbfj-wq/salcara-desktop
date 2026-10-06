'use strict';

function fromUpdate(event, win, updateURL) {
  if (!event || !win || win.isDestroyed() || event.sender !== win.webContents || event.senderFrame !== win.webContents.mainFrame) return false;
  try {
    const actual = new URL(event.senderFrame.url), expected = new URL(updateURL);
    return actual.protocol === 'file:' && actual.href === expected.href;
  } catch { return false; }
}

// Closing the update window only closes that window. It must not hide the
// main window, cancel an Agent task, or initiate installation.
function registerUpdateIpc({ ipcMain, getWindow, updateURL, getUpdater, checkForUpdate }) {
  const allowed = event => fromUpdate(event, getWindow(), updateURL);
  const close = event => { if (allowed(event)) getWindow().close(); };
  ipcMain.handle('upd:state', event => allowed(event) ? getUpdater().state() : null);
  ipcMain.on('upd:check', event => { if (allowed(event)) void checkForUpdate(true); });
  ipcMain.on('upd:download', event => { if (allowed(event)) void getUpdater().download(); });
  ipcMain.on('upd:cancel', event => { if (allowed(event)) getUpdater().cancel(); });
  ipcMain.on('upd:install', event => { if (allowed(event)) void getUpdater().install().catch(error => console.error('update install failed:', error)); });
  ipcMain.on('upd:skip', event => { if (allowed(event)) { getUpdater().skip(); close(event); } });
  ipcMain.on('upd:page', event => { if (allowed(event)) getUpdater().openPage(); });
  ipcMain.on('upd:later', close);
  return { allowed, close };
}

module.exports = { fromUpdate, registerUpdateIpc };
