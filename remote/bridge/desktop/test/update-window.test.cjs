'use strict';
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs'), path = require('node:path'), vm = require('node:vm');
const { registerUpdateIpc } = require('../update-ipc.cjs');
const { registerWindowIpc } = require('../window-ipc.cjs');

function fixture() {
  const handlers = new Map(), calls = [];
  const frame = { url:'file:///fixture/desktop/update.html' };
  const win = { webContents:{mainFrame:frame}, isDestroyed:()=>false, close:()=>calls.push('popup closed') };
  const event = { sender:win.webContents, senderFrame:frame };
  const ipcMain = { on:(key,handler)=>handlers.set(key,handler), handle:(key,handler)=>handlers.set(key,handler) };
  const updater = Object.fromEntries(['state','download','cancel','install','skip','openPage'].map(name=>[name,()=>{calls.push(name);return Promise.resolve();}]));
  updater.state = () => null;
  const api = registerUpdateIpc({ ipcMain, getWindow:()=>win, updateURL:frame.url, getUpdater:()=>updater, checkForUpdate:()=>calls.push('check') });
  return { handlers, calls, win, event, ipcMain, api };
}

test('popup close and later do not quit, cancel or install anything', () => {
  const f = fixture(); f.handlers.get('upd:later')(f.event);
  assert.deepEqual(f.calls, ['popup closed']);
});

test('generic close targets the update window rather than hiding the main window', () => {
  const f = fixture();
  registerWindowIpc({ipcMain:f.ipcMain,dialog:{},getWindow:()=>null,consoleURL:'http://127.0.0.1:47831/',fromSplash:()=>false,
    hide:()=>f.calls.push('main hidden'),hideSplash:()=>f.calls.push('splash hidden'),path,
    closeUpdate:event=>{if(!f.api.allowed(event))return false;f.api.close(event);return true;}});
  f.handlers.get('win:close')(f.event); assert.deepEqual(f.calls,['popup closed']);
});

test('popup commands reject other windows, subframes and navigated pages', () => {
  for (const change of ['sender','subframe','url','destroyed']) {
    const f=fixture(), event={...f.event};
    if(change==='sender')event.sender={};
    if(change==='subframe')event.senderFrame={url:f.event.senderFrame.url};
    if(change==='url')f.win.webContents.mainFrame.url='file:///fixture/other.html';
    if(change==='destroyed')f.win.isDestroyed=()=>true;
    for(const name of ['upd:later','upd:skip','upd:download','upd:cancel','upd:install'])f.handlers.get(name)(event);
    assert.deepEqual(f.calls,[],change);
  }
});

test('actual popup close button and Escape traverse preload and native IPC without touching tasks', async () => {
  const f=fixture(), elements=new Map(), listeners=new Map(), exposed={};
  const context=vm.createContext({
    require:name=>{assert.equal(name,'electron');return {
      contextBridge:{exposeInMainWorld:(name,api)=>{exposed[name]=api;}},
      ipcRenderer:{send:(name)=>f.handlers.get(name)?.(f.event),invoke:async(name)=>f.handlers.get(name)?.(f.event),on(){}},
    };},
    document:{getElementById:id=>{if(!elements.has(id))elements.set(id,{classList:{add(){},remove(){},contains:()=>false},click(){this.onclick?.();},style:{setProperty(){},removeProperty(){}},offsetWidth:0});return elements.get(id);}},
    window:exposed,setTimeout(){},addEventListener:(name,fn)=>listeners.set(name,fn),HTMLButtonElement:class{},
  });
  vm.runInContext(fs.readFileSync(path.join(__dirname,'../preload.cjs'),'utf8'),context);
  const html=fs.readFileSync(path.join(__dirname,'../update.html'),'utf8');
  vm.runInContext(html.match(/<script>([\s\S]*?)<\/script>/)[1],context);
  elements.get('close').click();
  listeners.get('keydown')({key:'Escape'});
  await Promise.resolve();
  assert.deepEqual(f.calls,['popup closed','popup closed']);
  assert.match(html,/button, a, \.notes \{ -webkit-app-region: no-drag;/);
});
