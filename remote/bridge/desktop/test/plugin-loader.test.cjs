'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { PluginLoader } = require('../plugin-loader.cjs');

function fixture(manifest = {}) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'salcara-plugin-test-'));
  const source = path.join(root, 'source');
  fs.mkdirSync(source);
  fs.writeFileSync(path.join(source, 'manifest.json'), JSON.stringify({
    id: 'fixture', name: 'Fixture', version: '1.0.0', entry: 'index.js', permissions: [], ...manifest,
  }));
  fs.writeFileSync(path.join(source, 'index.js'), 'exports.activate = function () {}; exports.deactivate = function () {};');
  const pluginsDir = path.join(root, 'plugins'), storageDir = path.join(root, 'storage');
  const loaders = [];
  const makeLoader = (options = {}) => {
    const loader = new PluginLoader({ pluginsDir, storageDir, host: {}, bridgeVersion: '1.0.0', ...options });
    loaders.push(loader); return loader;
  };
  const write = code => fs.writeFileSync(path.join(source, 'index.js'), code);
  return { root, source, pluginsDir, storageDir, makeLoader, write, cleanup: async () => {
    await Promise.all(loaders.map(loader => loader.close()));
    assert.ok(path.resolve(root).startsWith(path.resolve(os.tmpdir()) + path.sep));
    fs.rmSync(root, { recursive: true, force: true });
  } };

}

test('inspection validates without executing; install enables, and enable state survives restarts', async t => {
  const f = fixture({ enabled: false, author: 'Test author', description: 'Example plugin' });
  t.after(f.cleanup);
  const loader = f.makeLoader();
  const inspected = loader.inspect(f.source);
  assert.equal(inspected.id, 'fixture');
  assert.equal(inspected.enabled, false);
  assert.equal(loader.list().length, 0, 'inspection does not install or execute plugin code');

  await loader.install(f.source);
  assert.deepEqual(loader.list().map(row => [row.enabled, row.status]), [[true, 'ok']]);
  await loader.disable('fixture');
  assert.equal(JSON.parse(fs.readFileSync(path.join(f.pluginsDir, 'fixture', 'manifest.json'), 'utf8')).enabled, false);

  const restartedDisabled = f.makeLoader();
  await restartedDisabled.loadAll();
  assert.deepEqual(restartedDisabled.list().map(row => [row.enabled, row.status]), [[false, 'disabled']]);
  await restartedDisabled.enable('fixture');
  assert.equal(JSON.parse(fs.readFileSync(path.join(f.pluginsDir, 'fixture', 'manifest.json'), 'utf8')).enabled, true);

  const restarted = f.makeLoader();
  await restarted.loadAll();
  assert.deepEqual(restarted.list().map(row => [row.enabled, row.status]), [[true, 'ok']]);
  await restarted.disable('fixture');
  assert.equal(restarted.list()[0].enabled, false);
});

test('inspection rejects an entry that escapes the selected plugin directory', t => {
  const f = fixture({ entry: '../outside.js' });
  t.after(f.cleanup);
  fs.writeFileSync(path.join(f.root, 'outside.js'), 'exports.activate = function () {};');
  assert.throws(() => f.makeLoader().inspect(f.source), /inside its directory/);
});

test('installation counts saved plugins while asynchronous startup is still scanning', async t => {
  const f = fixture(); t.after(f.cleanup);
  const loader = f.makeLoader();
  for (let index = 0; index < 32; index++) {
    const id = 'saved-' + String(index).padStart(2, '0'), directory = path.join(f.pluginsDir, id);
    fs.mkdirSync(directory);
    fs.writeFileSync(path.join(directory, 'manifest.json'), JSON.stringify({ id, name: id, version: '1.0.0', enabled: index === 0, permissions: [] }));
    fs.writeFileSync(path.join(directory, 'index.js'), 'exports.activate = async () => { await new Promise(resolve => setTimeout(resolve, 100)); };');
  }
  const loading = loader.loadAll();
  assert.ok(loader.list().length < 32, 'the scan has not populated all records yet');
  await assert.rejects(async () => loader.install(f.source), /at most 32 plugins/);
  await loading;
  assert.equal(loader.list().length, 32);
  assert.equal(fs.existsSync(path.join(f.pluginsDir, 'fixture')), false);
});

test('startup refuses a persisted plugin whose entry was changed to escape its directory', async t => {
  const f = fixture({ entry: '../outside.js' });
  t.after(f.cleanup);
  fs.mkdirSync(path.join(f.pluginsDir, 'fixture'), { recursive: true });
  fs.copyFileSync(path.join(f.source, 'manifest.json'), path.join(f.pluginsDir, 'fixture', 'manifest.json'));
  fs.writeFileSync(path.join(f.root, 'outside.js'), 'exports.activate = function () {};');
  const loader = f.makeLoader();
  await loader.loadAll();
  assert.equal(loader.list()[0].status, 'error');
  assert.match(loader.list()[0].error, /inside its directory/);
});

test('failed activation removes hooks registered before the plugin throws', async t => {
  const f = fixture({ permissions: ['api.intercept'] });
  t.after(f.cleanup);
  fs.writeFileSync(path.join(f.source, 'index.js'), "exports.activate = function (api) { api.api.intercept('request', function () {}); throw new Error('broken'); };");
  const loader = f.makeLoader();
  await loader.install(f.source);
  assert.equal(loader.list()[0].status, 'error');
  assert.equal(loader._hooks.request.length, 0);
  await loader.disable('fixture');
  assert.equal(loader.list()[0].enabled, false);
});

test('panel actions, satellite navigation, updates, JSON and Math work through the real worker', async t => {
  const f = fixture({ permissions: ['ui.panel', 'ui.satellite', 'storage', 'notify'] }); t.after(f.cleanup);
  const panels = [], opens = [], closes = [], satellites = [], toasts = [], notifications = [], removed = [];
  f.write(`module.exports = { activate: async function (api) {
    const answer = JSON.parse('{"n":41}').n + Math.round(0.8);
    const panel = api.ui.registerPanel('stats', {label:'Stats', content:String(answer), actions:[{id:'increment',label:'Increment',onClick:async function(){
      const next = (await api.storage.get('count') || 0) + 1;
      await api.storage.set('count', next); panel.update({content:String(next)}); api.ui.toast('saved', {type:'ok'});
    }}]});
    api.ui.addSatellite('open', {label:'Open stats', panelId:'stats'});
    api.ui.addSatellite('notify', {label:'Notify',onClick:function(){api.notify.send('Title','Body'); panel.close();}});
  }};`);
  const loader = f.makeLoader({ host: {
    registerPanel: (...args) => panels.push(args), openPanel: (...args) => opens.push(args), closePanel: (...args) => closes.push(args),
    addSatellite: (...args) => satellites.push(args), showToast: (...args) => toasts.push(args), sendNotification: (...args) => notifications.push(args), removePlugin: id => removed.push(id),
  } });
  await loader.install(f.source);
  assert.equal(loader.list()[0].status, 'ok');
  assert.equal(panels[0][2].content, '42');
  assert.deepEqual(panels[0][2].actions, [{id:'increment',label:'Increment'}]);
  assert.equal(satellites.length, 2);
  assert.equal(Object.isFrozen(Object), false, 'plugin context must not freeze host builtins');
  await loader.invokeUIAction('fixture','panel','stats','increment');
  await loader.invokeUIAction('fixture','panel','stats','increment');
  assert.equal(panels.at(-1)[2].content, '2');
  assert.equal(toasts.length, 2);
  await loader.invokeUIAction('fixture','satellite','open');
  await loader.invokeUIAction('fixture','satellite','notify');
  assert.deepEqual(opens, [['fixture','stats']]);
  assert.deepEqual(closes, [['fixture','stats']]);
  assert.deepEqual(notifications, [['fixture','Title','Body']]);
  await loader.disable('fixture');
  assert.ok(removed.includes('fixture'));
  await assert.rejects(loader.invokeUIAction('fixture','panel','stats','increment'), /尚未运行/);
});

test('request and response hooks are chained with real data and leave caller objects unchanged', async t => {
  const f = fixture({ permissions:['api.intercept'] }); t.after(f.cleanup);
  f.write(`exports.activate = function(api) {
    api.api.intercept('request', async function(req,next) { req.body.first = true; return next(req); });
    api.api.intercept('request', function(req) { req.headers['x-plugin'] = 'ok'; return req; });
    api.api.intercept('response', function(res,next,req) { res.body.requestModel = req.body.model; res.status = 201; return next(res); });
  };`);
  const loader=f.makeLoader(); await loader.install(f.source);
  const request={tool:'codex',body:{model:'test'},headers:{}};
  const changed=await loader.runRequestHooks(request);
  assert.deepEqual(changed,{tool:'codex',body:{model:'test',first:true},headers:{'x-plugin':'ok'}});
  assert.deepEqual(request,{tool:'codex',body:{model:'test'},headers:{}});
  assert.deepEqual(await loader.runResponseHooks(request,{status:200,body:{ok:true}}),{status:201,body:{ok:true,requestModel:'test'}});
});

test('session and usage events persist storage, and host close preserves the enabled preference', async t => {
  const f=fixture({permissions:['events','api.intercept','storage']}); t.after(f.cleanup);
  f.write(`exports.activate = async function(api) {
    await api.storage.set('starts',(await api.storage.get('starts') || 0)+1);
    api.events.on('session:update', async payload => api.storage.set('session',payload));
    api.api.on('usage', async payload => api.storage.set('usage',payload));
  };`);
  const loader=f.makeLoader(); await loader.install(f.source);
  await loader.fireSessionEvent('session:update',{id:'session-one'});
  await loader.fireApiEvent('usage',{tokens:42});
  await loader.close();
  const saved=JSON.parse(fs.readFileSync(path.join(f.storageDir,'fixture.json'),'utf8'));
  assert.deepEqual(saved,{starts:1,session:{id:'session-one'},usage:{tokens:42}});
  assert.equal(JSON.parse(fs.readFileSync(path.join(f.pluginsDir,'fixture','manifest.json'),'utf8')).enabled,true);
  const restarted=f.makeLoader(); await restarted.loadAll();
  assert.equal(restarted.list()[0].status,'ok');
  assert.equal(JSON.parse(fs.readFileSync(path.join(f.storageDir,'fixture.json'),'utf8')).starts,2);
});

test('a synchronous activation loop times out without blocking the host event loop', async t => {
  const f=fixture(); t.after(f.cleanup); f.write('exports.activate = function(){while(true){}};');
  const loader=f.makeLoader({timeouts:{activation:200}});
  let responsive=false; const tick=setTimeout(()=>{responsive=true;},20); t.after(()=>clearTimeout(tick));
  await loader.install(f.source);
  assert.equal(responsive,true);
  assert.equal(loader.list()[0].status,'error');
  assert.match(loader.list()[0].error,/timed out|Script execution timed out/);
  assert.equal(loader._plugins.get('fixture').runtime,null);
});

test('a looping hook fails open, is marked failed and cannot retain hooks', async t => {
  const f=fixture({permissions:['api.intercept']}); t.after(f.cleanup);
  f.write(`exports.activate = function(api){api.api.intercept('request',function(req){req.body.changed=true;while(true){}});};`);
  const loader=f.makeLoader({timeouts:{hook:150}}); await loader.install(f.source);
  const req={tool:'claude',body:{model:'test'},headers:{}};
  assert.deepEqual(await loader.runRequestHooks(req),req);
  assert.equal(loader.list()[0].status,'error');
  assert.match(loader.list()[0].error,/timed out/);
  assert.equal(loader._hooks.request.length,0);
});

test('failed asynchronous events are visible and remove the plugin UI', async t => {
  const f=fixture({permissions:['events']}); t.after(f.cleanup); const removed=[];
  f.write(`exports.activate = function(api){api.events.on('session:start',async function(){throw new Error('event failed');});};`);
  const loader=f.makeLoader({host:{removePlugin:id=>removed.push(id)}}); await loader.install(f.source);
  await loader.fireSessionEvent('session:start',{});
  assert.equal(loader.list()[0].status,'error'); assert.match(loader.list()[0].error,/event failed/);
  assert.ok(removed.includes('fixture')); assert.equal(loader._plugins.get('fixture').runtime,null);
});

test('disable terminates timers and hung deactivation without changing future enabled state on close', async t => {
  const f=fixture({permissions:[]}); t.after(f.cleanup); const messages=[];
  f.write(`exports.activate = function(api){setInterval(function(){api.ui.toast('tick');},500);}; exports.deactivate = function(){while(true){}};`);
  const loader=f.makeLoader({timeouts:{deactivate:100},host:{showToast:(_id,message)=>messages.push(message)}});
  await loader.install(f.source);
  await new Promise(resolve=>setTimeout(resolve,550));
  assert.ok(messages.length>0);
  await loader.disable('fixture'); const count=messages.length;
  await new Promise(resolve=>setTimeout(resolve,550));
  assert.equal(messages.length,count); assert.equal(loader.list()[0].enabled,false);
  assert.equal(loader._plugins.get('fixture').runtime,null);
});

test('idle timer loops are stopped by the host heartbeat watchdog', async t => {
  const f=fixture(); t.after(f.cleanup);
  f.write(`exports.activate=function(){setTimeout(function(){while(true){}},30);};`);
  const loader=f.makeLoader({timeouts:{heartbeat:250}}); await loader.install(f.source);
  await new Promise(resolve=>setTimeout(resolve,650));
  assert.equal(loader.list()[0].status,'error'); assert.match(loader.list()[0].error,/stopped responding/);
  assert.equal(loader._plugins.get('fixture').runtime,null);
});

test('storage limits fail explicitly without replacing committed data', async t => {
  const f=fixture({permissions:['storage']}); t.after(f.cleanup);
  f.write(`exports.activate = async function(api){
    await api.storage.set('valid','saved');
    await api.storage.set('oversized','x'.repeat(1024*1024));
  };`);
  const loader=f.makeLoader(); await loader.install(f.source);
  assert.equal(loader.list()[0].status,'error'); assert.match(loader.list()[0].error,/size limit/);
  assert.deepEqual(JSON.parse(fs.readFileSync(path.join(f.storageDir,'fixture.json'),'utf8')),{valid:'saved'});
});

test('storage write failure reaches the plugin instead of reporting success', async t => {
  const f=fixture({permissions:['storage','ui.panel']}); t.after(f.cleanup);
  f.write(`exports.activate=function(api){api.ui.registerPanel('save',{actions:[{id:'save',label:'Save',onClick:async function(){await api.storage.set('value',42);}}]});};`);
  const loader=f.makeLoader(); await loader.install(f.source);
  const badTarget=path.join(f.storageDir,'fixture.json'); fs.mkdirSync(badTarget);
  await assert.rejects(loader.invokeUIAction('fixture','panel','save','save'));
  assert.equal(loader.list()[0].status,'error'); assert.ok(fs.statSync(badTarget).isDirectory());
  assert.deepEqual(fs.readdirSync(f.storageDir),['fixture.json'],'failed atomic writes leave no temp files');
});

test('minimum app version and symbolic-link packages are rejected before installation', async t => {
  const f=fixture({minBridgeVersion:'2.0.0'}); t.after(f.cleanup);
  const loader=f.makeLoader(); assert.throws(()=>loader.inspect(f.source),/requires Salcara Bridge 2.0.0/);
  const manifest=JSON.parse(fs.readFileSync(path.join(f.source,'manifest.json'),'utf8')); manifest.minBridgeVersion='0';
  fs.writeFileSync(path.join(f.source,'manifest.json'),JSON.stringify(manifest));
  const linked=path.join(f.source,'linked');
  try { fs.symlinkSync(f.storageDir,linked,'junction'); } catch(error){ if(['EPERM','EACCES'].includes(error.code)) {t.skip('symlinks unavailable');return;} throw error; }
  assert.throws(()=>loader.inspect(f.source),/symbolic links/);
});

test('simultaneous lifecycle mutations serialize and uninstall removes both code and storage', async t => {
  const f=fixture({permissions:['storage']}); t.after(f.cleanup);
  f.write(`exports.activate=async function(api){await api.storage.set('value','saved');};`);
  const loader=f.makeLoader(); await loader.install(f.source);
  await Promise.all([loader.disable('fixture'),loader.enable('fixture'),loader.disable('fixture')]);
  assert.equal(loader.list()[0].enabled,false); assert.equal(loader._plugins.get('fixture').runtime,null);
  await loader.uninstall('fixture'); assert.deepEqual(loader.list(),[]);
  assert.equal(fs.existsSync(path.join(f.pluginsDir,'fixture')),false);
  assert.equal(fs.existsSync(path.join(f.storageDir,'fixture.json')),false);
});

test('uninstall failures remain visible and retryable instead of dropping the record', async t => {
  const f=fixture(); t.after(f.cleanup);
  const loader=f.makeLoader(); await loader.install(f.source);
  fs.mkdirSync(path.join(f.storageDir,'fixture.json'));
  await assert.rejects(loader.uninstall('fixture'));
  assert.equal(loader.list().length,1); assert.equal(loader.list()[0].enabled,false);
  assert.equal(loader.list()[0].status,'error'); assert.match(loader.list()[0].error,/卸载未完成/);
  fs.rmdirSync(path.join(f.storageDir,'fixture.json'));
  await loader.uninstall('fixture'); assert.deepEqual(loader.list(),[]);
});

test('install binds confirmation to the inspected package and rejects changed source without executing it', async t => {
  const f=fixture(); t.after(f.cleanup);
  const loader=f.makeLoader(), inspected=loader.inspect(f.source);
  assert.match(inspected.packageDigest,/^[a-f0-9]{64}$/);
  f.write(`exports.activate=function(){throw new Error('changed code must not execute');};`);
  assert.throws(()=>loader.install(f.source,inspected.packageDigest),/文件已改变/);
  assert.deepEqual(loader.list(),[]); assert.deepEqual(fs.readdirSync(f.pluginsDir),[]);
  const current=loader.inspect(f.source);
  await loader.install(f.source,current.packageDigest);
  assert.match(loader.list()[0].error,/changed code must not execute/);
});

test('hooks registered after activation become callable through the host', async t => {
  const f=fixture({permissions:['api.intercept','ui.panel']}); t.after(f.cleanup);
  f.write(`exports.activate=function(api){api.ui.registerPanel('late',{actions:[{id:'add',label:'Add',onClick:function(){
    api.api.intercept('request',req=>{req.body.later=true;return req;});
  }}]});};`);
  const loader=f.makeLoader(); await loader.install(f.source);
  const request={tool:'codex',body:{},headers:{}};
  assert.deepEqual(await loader.runRequestHooks(request),request);
  await loader.invokeUIAction('fixture','panel','late','add');
  assert.equal((await loader.runRequestHooks(request)).body.later,true);
});
