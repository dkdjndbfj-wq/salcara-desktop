'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const http = require('node:http');
const { createPluginSystem } = require('../plugin-ipc.cjs');
const { createCoreLink } = require('../plugin-core-link.cjs');

async function fixture(t) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'salcara-plugin-host-'));
  const handlers = new Map(), sent = [], shown = [];
  const url = 'http://127.0.0.1:47831/';
  const makeWindow = route => { const mainFrame = { url: url + route }; return { isDestroyed: () => false, webContents: { mainFrame, isDestroyed: () => false, send: (channel, value) => sent.push([route, channel, value]) } }; };
  const win = makeWindow(''); let ball = null;
  const system = await createPluginSystem({ app: { getPath: () => root, getVersion: () => '1.6.4' }, ipcMain: { handle: (name, handler) => handlers.set(name, handler), removeHandler: name => handlers.delete(name) }, getWindow: () => win, getBall: () => ball, consoleURL: url, localToken: 'fixture-token-'.repeat(4), showWindow: route => shown.push(route) });
  t.after(async () => { await system.close(); if (!path.resolve(root).startsWith(path.resolve(os.tmpdir()) + path.sep)) throw new Error('Unsafe fixture cleanup'); fs.rmSync(root, { recursive: true, force: true }); });
  const source = path.join(root, 'source'); fs.mkdirSync(source);
  fs.writeFileSync(path.join(source, 'manifest.json'), JSON.stringify({ id: 'example', name: 'Example', version: '1.0.0', permissions: ['ui.panel', 'ui.satellite', 'api.intercept'], entry: 'index.js' }));
  fs.writeFileSync(path.join(source, 'index.js'), `exports.activate = api => { let count=0; const panel=api.ui.registerPanel('summary',{label:'Summary',content:'Ready',actions:[{id:'count',label:'Count',onClick:()=>panel.update({content:'Count '+(++count)})}]}); api.ui.addSatellite('shortcut',{label:'Example shortcut',panelId:'summary'}); api.api.intercept('request',(req,next)=>next({...req,body:{...req.body,instructions:'plugin'}})); };`);
  const event = window => ({ sender: window.webContents, senderFrame: window.webContents.mainFrame });
  const call = (name, data) => handlers.get(name)(event(win), data);
  return { system, root, source, sent, shown, win, handlers, call, event, makeBall: () => { ball = makeWindow('ball.html'); return ball; } };
}

test('real plugin registrations replay to late windows, actions update panels and stopping removes entries', async t => {
  const f = await fixture(t);
  await f.call('plugin:install', { dir: f.source });
  const state = f.call('plugin:ui:state');
  assert.equal(state.panels[0].content, 'Ready');
  const ball = f.makeBall();
  assert.equal(f.handlers.get('plugin:ui:state')(f.event(ball)).satellites.length, 1);
  await f.handlers.get('plugin:action')(f.event(ball), { pluginId: 'example', kind: 'satellite', targetId: 'shortcut' });
  assert.equal(f.system.snapshot().openPanel.panelId, 'summary');
  assert.deepEqual(f.shown, ['plugins']);
  await f.call('plugin:action', { pluginId: 'example', kind: 'panel', targetId: 'summary', actionId: 'count' });
  assert.equal(f.system.snapshot().panels[0].content, 'Count 1');
  await f.call('plugin:disable', { id: 'example' });
  assert.equal(f.system.snapshot().panels.length, 0);
  assert.equal(f.system.snapshot().satellites.length, 0);
  assert.throws(() => f.call('plugin:action', { pluginId: 'example', kind: 'panel', targetId: 'summary', actionId: 'count' }), /不可用/);
});

test('plugin IPC rejects foreign frames and restricts floating windows to shortcut actions', async t => {
  const f = await fixture(t), ball = f.makeBall();
  for (const event of [{}, { ...f.event(f.win), sender: {} }, { ...f.event(f.win), senderFrame: { url: f.win.webContents.mainFrame.url } }]) assert.throws(() => f.handlers.get('plugin:list')(event), /不允许/);
  assert.throws(() => f.handlers.get('plugin:install')(f.event(ball), { dir: f.source }), /不允许/);
  assert.throws(() => f.handlers.get('plugin:action')(f.event(ball), { pluginId: 'example', kind: 'panel', targetId: 'summary', actionId: 'count' }), /悬浮球/);
  f.win.webContents.mainFrame.url = 'http://127.0.0.1:47831/foreign.html';
  assert.throws(() => f.call('plugin:list'), /不允许/);
});

test('authenticated loopback hooks execute a real plugin and close keeps saved enable state', async t => {
  const f = await fixture(t); await f.call('plugin:install', { dir: f.source });
  const url = 'http://127.0.0.1:' + f.system.hookPort + '/plugin/hook/request';
  assert.equal((await fetch(url, { method: 'POST', body: '{}' })).status, 403);
  const result = await fetch(url, { method: 'POST', headers: { 'X-Plugin-Token': 'fixture-token-'.repeat(4) }, body: JSON.stringify({ tool: 'codex', body: { input: 'hello' } }) }).then(r => r.json());
  assert.equal(result.body.instructions, 'plugin');
  assert.equal(result.body.input, 'hello');
  await f.system.close();
  assert.equal(f.handlers.size, 0);
  assert.equal(JSON.parse(fs.readFileSync(path.join(f.root, 'plugins', 'example', 'manifest.json'), 'utf8')).enabled, true);
});

test('closing during real asynchronous startup drains the active worker and skips later plugins', async t => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'salcara-plugin-startup-'));
  const handlers = new Map(), sent = [];
  let system;
  t.after(async () => {
    await system?.close();
    if (!path.resolve(root).startsWith(path.resolve(os.tmpdir()) + path.sep)) throw new Error('Unsafe fixture cleanup');
    fs.rmSync(root, { recursive: true, force: true });
  });
  for (const id of ['a-starting', 'b-later']) {
    const directory = path.join(root, 'plugins', id);
    fs.mkdirSync(directory, { recursive: true });
    fs.writeFileSync(path.join(directory, 'manifest.json'), JSON.stringify({ id, name: id, version: '1.0.0', enabled: true, permissions: ['storage', 'ui.panel'] }));
    fs.writeFileSync(path.join(directory, 'index.js'), `exports.activate = async api => {
      await api.storage.set('started', true);
      await new Promise(resolve => setTimeout(resolve, 250));
      api.ui.registerPanel('late', { label: 'Late registration' });
    };`);
  }
  const window = { isDestroyed: () => false, webContents: { isDestroyed: () => false, send: (...args) => sent.push(args) } };
  system = await createPluginSystem({ app: { getPath: () => root, getVersion: () => '1.6.4' },
    ipcMain: { handle: (name, fn) => handlers.set(name, fn), removeHandler: name => handlers.delete(name) },
    getWindow: () => window, consoleURL: 'http://127.0.0.1:47831/', localToken: 'fixture-token-'.repeat(4) });
  const started = path.join(root, 'plugin-storage', 'a-starting.json');
  for (let i = 0; i < 100 && !fs.existsSync(started); i++) await new Promise(resolve => setTimeout(resolve, 10));
  assert.equal(fs.existsSync(started), true, 'the production worker has entered async activation');
  assert.equal(system.snapshot().loading, true, 'creating the host does not await plugin activation');
  const published = sent.length, closing = system.close();
  assert.equal(system.close(), closing, 'concurrent closes share one shutdown');
  await closing;
  await system.ready;
  assert.equal(handlers.size, 0);
  assert.equal(sent.length, published, 'late activation cannot publish UI after close begins');
  assert.deepEqual(system.snapshot().panels, []);
  assert.equal(system.loader._queues.size, 0);
  assert.ok([...system.loader._plugins.values()].every(record => record.runtime === null));
  assert.equal(fs.existsSync(path.join(root, 'plugin-storage', 'b-later.json')), false, 'shutdown never starts the next saved plugin');
  assert.equal(JSON.parse(fs.readFileSync(path.join(root, 'plugins', 'a-starting', 'manifest.json'), 'utf8')).enabled, true);
  await assert.rejects(fetch('http://127.0.0.1:' + system.hookPort + '/plugin/event', { signal: AbortSignal.timeout(1000) }));
});

test('core link obtains its private cookie, renews the lease and conditionally disconnects', async t => {
  const calls = [], states = [];
  const server = http.createServer(async (req, res) => {
    if (req.url === '/') { res.setHeader('Set-Cookie', 'salcara_console=fixture; HttpOnly'); res.end('shell'); return; }
    assert.equal(req.headers.cookie, 'salcara_console=fixture');
    assert.equal(req.headers.origin, 'http://127.0.0.1:' + server.address().port);
    let body=''; for await(const chunk of req)body+=chunk;
    calls.push([req.url,JSON.parse(body)]); res.end('{"ok":true}');
  });
  await new Promise(resolve => server.listen(0,'127.0.0.1',resolve));
  const link = createCoreLink({ consoleURL:'http://127.0.0.1:'+server.address().port+'/',hookPort:12345,token:'token-not-in-ui',onState:state=>states.push(state),intervalMs:15 });
  t.after(async()=>{await link.stop();server.close();server.closeAllConnections();});
  await link.start();
  await new Promise(resolve=>setTimeout(resolve,50));
  await link.stop();
  assert.ok(calls.filter(([url])=>url.endsWith('/connect')).length>=2);
  assert.equal(calls.at(-1)[0],'/api/local/plugins/disconnect');
  assert.deepEqual(calls.at(-1)[1],{token:'token-not-in-ui'});
  assert.ok(states.some(state=>state.state==='connected'));
});

test('stopping the core link aborts an in-flight connection and never renews after shutdown', async t => {
  const calls = [], states = [];
  let connected;
  const seenConnect = new Promise(resolve => { connected = resolve; });
  const server = http.createServer(async (req, res) => {
    if (req.url === '/') { res.setHeader('Set-Cookie', 'salcara_console=fixture; HttpOnly'); res.end('shell'); return; }
    let body = ''; for await (const chunk of req) body += chunk;
    calls.push([req.url, JSON.parse(body)]);
    if (req.url.endsWith('/connect')) { connected(); return; }
    res.end('{"ok":true}');
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const link = createCoreLink({ consoleURL: 'http://127.0.0.1:' + server.address().port + '/', hookPort: 12345,
    token: 'fixture-stop-token', onState: state => states.push(state), intervalMs: 10 });
  t.after(async () => { await link.stop(); server.close(); server.closeAllConnections(); });
  const starting = link.start();
  await seenConnect;
  await link.stop();
  await starting;
  await link.start();
  await new Promise(resolve => setTimeout(resolve, 30));
  assert.deepEqual(calls.map(([url]) => url), ['/api/local/plugins/connect', '/api/local/plugins/disconnect']);
  assert.deepEqual(calls[1][1], { token: 'fixture-stop-token' });
  assert.deepEqual(states, [], 'an aborted startup does not publish a stale connected/offline state');
});
