'use strict';

// Keep native controls independent of console rendering and restrict dialogs to
// the main, top-level local page. Never expose Electron objects to the renderer.
function fromMain(event, win, consoleURL) {
  if (!win || win.isDestroyed() || event.sender !== win.webContents || event.senderFrame !== win.webContents.mainFrame) return false;
  try {
    const url = new URL(event.senderFrame.url), base = new URL(consoleURL);
    return url.origin === base.origin && (url.pathname === '/' || url.pathname === '/index.html');
  } catch { return false; }
}

function registerWindowIpc({ ipcMain, dialog, getWindow, consoleURL, fromSplash, hide, hideSplash, path, closeUpdate = () => false }) {
  let choosing = null;
  ipcMain.on('win:minimize', event => {
    const win = getWindow();
    if ((fromMain(event, win, consoleURL) || fromSplash(event)) && win && !win.isDestroyed()) win.minimize();
  });
  ipcMain.on('win:close', event => {
    if (closeUpdate(event)) return;
    if (fromMain(event, getWindow(), consoleURL)) hide();
    else if (fromSplash(event)) hideSplash();
  });
  ipcMain.handle('win:choose-directory', async (event, initial) => {
    const win = getWindow();
    if (!fromMain(event, win, consoleURL)) throw new Error('不允许从此页面选择文件夹');
    if (!choosing) {
      const defaultPath = typeof initial === 'string' && initial.length <= 4096 && !/[\r\n\0]/.test(initial) && path.isAbsolute(initial) ? initial : undefined;
      choosing = dialog.showOpenDialog(win, { title: '选择项目文件夹', properties: ['openDirectory'], ...(defaultPath ? { defaultPath } : {}) })
        .then(result => result.canceled ? null : result.filePaths[0] || null).finally(() => { choosing = null; });
    }
    return choosing;
  });
}

module.exports = { fromMain, registerWindowIpc };
