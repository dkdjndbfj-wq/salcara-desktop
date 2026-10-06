'use strict';
const { test } = require('node:test');
const assert = require('node:assert/strict');
const path = require('node:path');
const { fromMain, registerWindowIpc } = require('../window-ipc.cjs');

function fixture() {
  const handlers = new Map(), calls = [];
  const frame = { url: 'http://127.0.0.1:47831/' };
  const win = { webContents: { mainFrame: frame }, isDestroyed: () => false, minimize: () => calls.push('min') };
  const event = { sender: win.webContents, senderFrame: frame };
  const dialog = { showOpenDialog: async (_win, options) => { calls.push(options); return { canceled: false, filePaths: ['C:\\fixture\\project'] }; } };
  registerWindowIpc({ ipcMain: { on: (name, fn) => handlers.set(name, fn), handle: (name, fn) => handlers.set(name, fn) },
    dialog, getWindow: () => win, consoleURL: frame.url, fromSplash: ev => ev.splash === true,
    hide: () => calls.push('hide'), hideSplash: () => calls.push('hideSplash'), path: path.win32 });
  return { handlers, calls, win, event, dialog };
}
test('native minimize and close work independently of console page renderers', () => {
  const f = fixture(); f.handlers.get('win:minimize')(f.event); f.handlers.get('win:close')(f.event);
  assert.deepEqual(f.calls, ['min', 'hide']); f.handlers.get('win:close')({ splash: true }); assert.equal(f.calls.at(-1), 'hideSplash');
});
test('controls and directory picker reject other windows, frames, ports and paths', async () => {
  for (const url of ['https://evil.example/', 'http://127.0.0.1:47832/', 'http://127.0.0.1:47831/subpage']) {
    const f = fixture(); f.win.webContents.mainFrame.url = url;
    assert.equal(fromMain(f.event, f.win, 'http://127.0.0.1:47831/'), false);
  }
  const f = fixture();
  for (const ev of [{ ...f.event, sender: {} }, { ...f.event, senderFrame: { url: f.event.senderFrame.url } }]) {
    f.handlers.get('win:minimize')(ev); f.handlers.get('win:close')(ev);
    await assert.rejects(f.handlers.get('win:choose-directory')(ev, 'C:\\safe'));
  }
  assert.equal(f.calls.length, 0);
});
test('Windows chooser selects directories only, honors safe absolute default and cancellation', async () => {
  const f = fixture();
  assert.equal(await f.handlers.get('win:choose-directory')(f.event, 'C:\\fixture'), 'C:\\fixture\\project');
  assert.deepEqual(f.calls[0].properties, ['openDirectory']); assert.equal(f.calls[0].defaultPath, 'C:\\fixture');
  f.dialog.showOpenDialog = async (_win, options) => { assert.equal(options.defaultPath, undefined); return { canceled: true, filePaths: [] }; };
  assert.equal(await f.handlers.get('win:choose-directory')(f.event, 'relative/path'), null);
});
test('double click shares one native chooser instead of opening two dialogs', async () => {
  const f = fixture(); let resolve, count = 0;
  f.dialog.showOpenDialog = () => { count++; return new Promise(r => { resolve = r; }); };
  const a = f.handlers.get('win:choose-directory')(f.event), b = f.handlers.get('win:choose-directory')(f.event);
  assert.equal(count, 1); resolve({ canceled: true, filePaths: [] }); assert.equal(await a, null); assert.equal(await b, null);
});
