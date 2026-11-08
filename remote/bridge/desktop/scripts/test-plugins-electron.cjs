'use strict';
// Run with the bundled Electron executable. Uses only a temporary userData
// directory, the production preload/IPC/loader/UI, and a synthetic core server.
const { app, BrowserWindow, ipcMain } = require('electron');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const http = require('node:http');
const assert = require('node:assert/strict');
const { createPluginSystem } = require('../plugin-ipc.cjs');
const root = fs.mkdtempSync(path.join(os.tmpdir(), 'salcara-electron-plugins-'));
app.setPath('userData', path.join(root, 'profile'));
app.commandLine.appendSwitch('disable-gpu');
app.commandLine.appendSwitch('force-device-scale-factor','1');
const web = path.resolve(__dirname, '../../internal/console/web');
const example = path.resolve(__dirname, '../examples/activity-monitor');
const output = process.env.SALCARA_PLUGIN_TEST_OUTPUT;
if (!output || !path.isAbsolute(output)) throw new Error('SALCARA_PLUGIN_TEST_OUTPUT must name an absolute output directory');
fs.mkdirSync(output, { recursive: true });
fs.writeFileSync(path.join(output, 'fixture-root.txt'),root);
let win, ball, system, server, exiting = false, ballSession = 0, rootRefreshes = 0, expectedAuthFailure = false, streamAuthFailures = 0;
const errors=[], notifications=[], streams=new Set();
const interactions=[];
const pause = ms => new Promise(resolve=>setTimeout(resolve,ms));
async function waitFor(expression, target=win) {
  let lastError = '';
  for(let i=0;i<150;i++){try { if(await target.webContents.executeJavaScript(expression))return; } catch(error) { lastError=error.message; } await pause(50);}
  console.error('Renderer errors:',JSON.stringify(errors),lastError);
  throw new Error('Timed out: '+expression);
}
async function click(selector, target=win) {
  console.log('Click',selector);
  await waitFor(`!!document.querySelector(${JSON.stringify(selector)})`,target);
  await target.webContents.executeJavaScript(`document.querySelector(${JSON.stringify(selector)}).click()`);
}
async function capture(name,target=win){target.webContents.invalidate();await pause(200);fs.writeFileSync(path.join(output,name),(await target.webContents.capturePage()).toPNG());}
const api=(res,value)=>{res.writeHead(200,{'Content-Type':'application/json','Cache-Control':'no-store'});res.end(JSON.stringify(value));};
async function run() {
  server=http.createServer(async(req,res)=>{
    const url=new URL(req.url,'http://127.0.0.1');
    if (req.headers.referer?.includes('/ball.html') && url.pathname.startsWith('/api/') && ballSession && !req.headers.cookie?.includes('ball_fixture='+ballSession)) {
      if(url.pathname==='/api/stream')streamAuthFailures++;
      res.writeHead(401);res.end('Restarted fixture session');return;
    }
    if(url.pathname==='/api/stream'){res.writeHead(200,{'Content-Type':'text/event-stream'});res.write(': fixture\n\n');streams.add(res);req.on('close',()=>streams.delete(res));return;}
    if(url.pathname==='/api/state')return api(res,{version:'plugin-test',os:'windows',config:{deviceName:'插件隔离验收',remoteDeviceOnly:true},hub:{state:'not_logged_in'}});
    if(url.pathname==='/api/approvals')return api(res,{approvals:[]});
    if(url.pathname==='/api/setup')return api(res,{items:[],discoveryPending:false});
    if(url.pathname==='/api/sessions')return api(res,{sessions:[]});
    if(url.pathname==='/api/activity')return api(res,{revision:0,sessions:[],approvalIds:[]});
    if(url.pathname==='/api/local/accounts')return api(res,{tools:[],accounts:[],bindings:{}});
    if(url.pathname==='/api/usage/overview')return api(res,{days:1,sources:[]});
    if(url.pathname.startsWith('/api/'))return api(res,{});
    const filename=path.resolve(web,'.'+(url.pathname==='/'?'/index.html':decodeURIComponent(url.pathname)));
    if(!filename.startsWith(web+path.sep)){res.writeHead(403);res.end();return;}
    try{const types={'.html':'text/html','.js':'text/javascript','.css':'text/css','.svg':'image/svg+xml','.png':'image/png'};const headers={'Content-Type':types[path.extname(filename)]||'application/octet-stream'};if(url.pathname==='/'){headers['Set-Cookie']='ball_fixture='+ballSession+'; Path=/; HttpOnly; SameSite=Strict';rootRefreshes++;}res.writeHead(200,headers);res.end(fs.readFileSync(filename));}catch{res.writeHead(404);res.end();}
  });
  await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
  const url='http://127.0.0.1:'+server.address().port+'/';
  const makeWindow=(width,height)=>{
    const w=new BrowserWindow({show:false,width,height,frame:false,webPreferences:{preload:path.resolve(__dirname,'../preload.cjs'),nodeIntegration:false,contextIsolation:true,sandbox:true,offscreen:true}});
    w.webContents.on('console-message',event=>{if(event.level==='error'||event.level===3){if(expectedAuthFailure && event.message.includes('401'))return;errors.push(event.message);console.error('Renderer:',event.message);}});return w;
  };
  win=makeWindow(1120,780);
  const hostOptions={app:{getPath:()=>path.join(root,'profile'),getVersion:()=> '1.6.4'},ipcMain,Notification:class{static isSupported(){return true;}constructor(value){this.value=value;}show(){notifications.push(this.value);}},getWindow:()=>win,getBall:()=>ball,consoleURL:url,localToken:'isolated-test-token-'.repeat(3),showWindow:()=>{void win.webContents.executeJavaScript("location.hash='plugins'");}};
  system=await createPluginSystem(hostOptions);system.setConnection({state:'connected'});
  ipcMain.handle('win:choose-plugin-directory',()=>example);
  ipcMain.on('ball:interactive',(_event,on)=>interactions.push(on));
  await win.loadURL(url+'#plugins');
  await waitFor("document.body.innerText.includes('还没有安装插件')");
  await click('[data-act="pluginInstall"]');
  await waitFor("!!document.querySelector('.confirm-back')");
  const confirm = await win.webContents.executeJavaScript("[...document.querySelectorAll('.confirm-back button')].map(b=>({text:b.textContent,act:b.dataset.act}))");
  const approve = await win.webContents.executeJavaScript("[...document.querySelectorAll('.confirm-back button')].find(b=>!['取消','关闭'].includes(b.textContent.trim()))?.outerHTML");
  if(!approve)throw new Error('No confirmation action '+JSON.stringify(confirm));
  await win.webContents.executeJavaScript("[...document.querySelectorAll('.confirm-back button')].find(b=>!['取消','关闭'].includes(b.textContent.trim())).click()");
  await waitFor("!!document.querySelector('[data-plugin=\"salcara-activity-example\"]') && document.body.innerText.includes('运行中')");
  await click('[data-act="pluginPanelOpen"]');await waitFor("document.body.innerText.includes('已保存记录：0')");
  await click('[data-action="record"]');await waitFor("document.body.innerText.includes('已保存记录：1')");
  await click('[data-action="notify"]');await waitFor("!document.querySelector('[data-action=\"notify\"]').disabled");assert.equal(notifications.length,1);
  await capture('plugins-panel.png');
  await win.reload();await waitFor("document.body.innerText.includes('已保存记录：1')");
  ball=makeWindow(400,210);await ball.loadURL(url+'ball.html');
  await waitFor("!!document.querySelector('#sats button[aria-label=\"插件快捷入口\"]:not([hidden])')",ball);
  await ball.webContents.executeJavaScript('setOpen(true)');await click('#sats button[aria-label="插件快捷入口"]',ball);
  ball.webContents.send('ball:layout',{side:'right',maxWidth:174,bubbleMaxWidth:148});await pause(50);
  await capture('plugins-ball.png',ball);
  ball.webContents.send('ball:layout',{side:'left',maxWidth:174,bubbleMaxWidth:148});
  await waitFor("document.querySelector('.plugin-ball-list').classList.contains('opens-left')",ball);
  assert.equal(await ball.webContents.executeJavaScript("document.querySelector('.plugin-ball-list').getBoundingClientRect().right <= innerWidth/2 - 18"),true);
  await capture('plugins-ball-left.png',ball);
  ball.webContents.send('ball:layout',{side:'left',maxWidth:104,bubbleMaxWidth:78});await pause(50);
  assert.equal(await ball.webContents.executeJavaScript("document.querySelector('.plugin-ball-list').getBoundingClientRect().width"),104);
  assert.equal(await ball.webContents.executeJavaScript("document.querySelector('#bubble').getBoundingClientRect().width"),78);
  ball.webContents.send('ball:layout',{side:'right',maxWidth:174,bubbleMaxWidth:148});await pause(50);
  // A stationary pointer over a disappearing menu must release native hit handling.
  await ball.webContents.executeJavaScript("{const r=document.querySelector('.plugin-ball-back').getBoundingClientRect();dispatchEvent(new MouseEvent('mousemove',{clientX:r.x+10,clientY:r.y+10}));}");
  await waitFor('interactive === true',ball);await pause(30);assert.equal(interactions.at(-1),true);
  await ball.webContents.executeJavaScript("document.querySelector('.plugin-ball-list').dispatchEvent(new KeyboardEvent('keydown',{key:'Escape',bubbles:true}));");
  await waitFor('interactive === false && !STATE.open',ball);await pause(30);assert.equal(interactions.at(-1),false);
  await ball.webContents.executeJavaScript('setOpen(true)');await click('#sats button[aria-label="插件快捷入口"]',ball);
  await click('.plugin-ball-list button:not(.plugin-ball-back)',ball);
  await waitFor("document.body.innerText.includes('已保存记录：1')");
  // Rotate a synthetic per-run cookie. EventSource first receives a real HTTP
  // 401, then the renderer refreshes the cookie and opens a new live stream.
  const beforeRefresh = rootRefreshes; expectedAuthFailure=true;ballSession++;
  await ball.webContents.executeJavaScript('stopBallStream(); stream();');
  await waitFor('BALL_AUTH.epoch > 0 && ballStream?.readyState === EventSource.OPEN',ball);
  expectedAuthFailure=false;assert.ok(streamAuthFailures>0);assert.ok(rootRefreshes>beforeRefresh);
  // Exercise the page lifecycle in the real renderer; native display/drag
  // ownership is covered separately by ball-ipc.test.cjs.
  await ball.webContents.executeJavaScript("window.fixtureHidden=true;Object.defineProperty(document,'hidden',{configurable:true,get:()=>window.fixtureHidden});Object.defineProperty(document,'visibilityState',{configurable:true,get:()=>window.fixtureHidden?'hidden':'visible'});document.dispatchEvent(new Event('visibilitychange'));");
  assert.equal(await ball.webContents.executeJavaScript('ballStream === null && ballEyeFrame === 0'),true);
  await ball.webContents.executeJavaScript("window.fixtureHidden=false;document.dispatchEvent(new Event('visibilitychange'));stream();");
  await waitFor('ballStream?.readyState === EventSource.OPEN',ball);
  await click('[data-act="pluginPanelClose"]');await click('[data-plugin-toggle]');
  await waitFor("document.body.innerText.includes('停用') && !document.querySelector('[data-act=\"pluginPanelOpen\"]')");
  await waitFor("document.querySelector('#sats button[aria-label=\"插件快捷入口\"]').hidden",ball);
  await click('[data-plugin-toggle]');await waitFor("!!document.querySelector('.confirm-back')");
  await win.webContents.executeJavaScript("[...document.querySelectorAll('.confirm-back button')].find(b=>!['取消','关闭'].includes(b.textContent.trim())).click()");
  await waitFor("!!document.querySelector('[data-act=\"pluginPanelOpen\"]')");
  await system.close();system=await createPluginSystem(hostOptions);system.setConnection({state:'connected'});
  await win.reload();await waitFor("!!document.querySelector('[data-act=\"pluginPanelOpen\"]')");
  await click('[data-act="pluginPanelOpen"]');await waitFor("document.body.innerText.includes('已保存记录：1')");
  await click('[data-act="pluginPanelClose"]');await capture('plugins-list.png');
  win.setSize(760,720);await pause(100);assert.equal(await win.webContents.executeJavaScript('document.documentElement.scrollWidth <= innerWidth'),true);await capture('plugins-list-small.png');win.setSize(1120,780);
  await click('[data-act="pluginUninstall"]');await waitFor("!!document.querySelector('.confirm-back')");
  await win.webContents.executeJavaScript("[...document.querySelectorAll('.confirm-back button')].find(b=>!['取消','关闭'].includes(b.textContent.trim())).click()");
  await waitFor("document.body.innerText.includes('还没有安装插件')");
  assert.equal(fs.existsSync(path.join(root,'profile','plugins','salcara-activity-example')),false);
  assert.equal(fs.existsSync(path.join(root,'profile','plugin-storage','salcara-activity-example.json')),false);
  assert.deepEqual(errors,[]);
  console.log(JSON.stringify({passed:true,flows:['install-confirm','panel-action','notification-dispatch','renderer-reload','late-ball-shortcut','ball-layout-left-right-narrow','ball-menu-escape-click-through','ball-cookie-recovery','ball-visibility-recovery','disable-cleanup','enable','host-restart-storage','small-window-layout','uninstall'],errors,output}));
}
app.whenReady().then(run).then(()=>finish(0),error=>{console.error(error.stack);fs.writeFileSync(path.join(output,'failure.json'),JSON.stringify({error:error.stack,errors}));void finish(1);});
async function finish(code) {
  if(exiting)return;exiting=true;
  try { await system?.close(); } catch {}
  for(const res of streams)res.end(); server?.close();server?.closeAllConnections();ball?.destroy();win?.destroy();
  // Chromium still holds files here until process exit; the invoking test runner
  // deletes this exact temporary root afterwards (recorded in fixture-root.txt).
  app.exit(code);
}
