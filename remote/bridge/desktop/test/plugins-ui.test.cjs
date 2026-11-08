'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.resolve(__dirname, '../../internal/console/web/plugins.js'), 'utf8');
const indexSource = fs.readFileSync(path.resolve(__dirname, '../../internal/console/web/index.html'), 'utf8');

function deferred() {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

function fixture(pluginAPI, { choosePluginDirectory = async () => 'C:\\plugins\\fixture', confirm = async () => true } = {}) {
  const view = { innerHTML: '', isConnected: true };
  const installButton = { disabled: false, innerHTML: '' };
  const headActions = { innerHTML: '', querySelector: () => installButton };
  const listeners = new Map(), notices = [], confirmations = [];
  const context = vm.createContext({
    PAGES: {}, RENDER: {}, ACTIONS: {}, S: { route: 'plugins' },
    window: { salcaraPlugins: pluginAPI, salcaraWindow: { choosePluginDirectory } },
    document: { addEventListener: (name, fn) => listeners.set(name, fn) },
    $: selector => selector === '#view' ? view : selector === '#headActions' ? headActions : null,
    icon: name => `<svg data-icon="${name}"></svg>`, ico: name => `<i data-ico="${name}"></i>`,
    esc: value => String(value ?? '').replace(/[&<>"']/g, ch => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[ch])),
    confirm: async message => { confirmations.push(message); return confirm(message); },
    toast: (message, kind) => notices.push([message, kind]),
  });
  vm.runInContext(source, context, { filename: 'plugins.js' });
  return { context, view, installButton, headActions, listeners, notices, confirmations };
}

test('plugin page renders metadata, declared permissions, disabled state, and runtime errors safely', async () => {
  const f = fixture({ list: async () => [{
    id: 'local-plugin', name: '<script>bad</script>', version: '2.1', author: 'Author',
    description: 'A local extension', permissions: ['api.intercept', 'storage'], enabled: true,
    status: 'error', error: '<img src=x onerror=alert(1)>',
  }] });
  await f.context.RENDER.plugins(f.view);
  assert.match(f.view.innerHTML, /&lt;script&gt;bad&lt;\/script&gt;/);
  assert.match(f.view.innerHTML, /API 请求与事件/);
  assert.match(f.view.innerHTML, /独立本地存储/);
  assert.match(f.view.innerHTML, /运行错误/);
  assert.match(f.view.innerHTML, /&lt;img src=x onerror=alert\(1\)&gt;/);
  assert.match(f.headActions.innerHTML, /安装插件/);
});

test('plugin install inspects selected metadata and requires confirmation before executing install', async () => {
  const calls = [];
  const f = fixture({
    list: async () => [],
    inspect: async dir => { calls.push(['inspect', dir]); return { id: 'fixture', name: 'Fixture', version: '1.0', author: 'Author', permissions: ['notify'] }; },
    install: async dir => { calls.push(['install', dir]); return { id: 'fixture' }; },
  }, { confirm: async () => false });
  await f.context.ACTIONS.pluginInstall();
  assert.deepEqual(calls, [['inspect', 'C:\\plugins\\fixture']]);
  assert.match(f.confirmations[0], /系统通知/);
  assert.equal(f.notices.length, 0);
});

test('enabling a plugin shows its permissions before activation and refreshes the row', async () => {
  const plugin = { id: 'fixture', name: 'Fixture', version: '1.0', permissions: ['api.intercept'], enabled: false, status: 'disabled' };
  const f = fixture({
    list: async () => [plugin],
    enable: async id => { assert.equal(id, 'fixture'); plugin.enabled = true; plugin.status = 'ok'; },
  });
  await f.context.RENDER.plugins(f.view);
  await f.context.ACTIONS.pluginToggle({ dataset: { pluginToggle: 'fixture' }, checked: true });
  assert.match(f.confirmations[0], /API 请求与事件/);
  assert.match(f.view.innerHTML, /运行中/);
  assert.match(f.view.innerHTML, /checked/);
});

test('enabling a plugin whose activation fails reports failure instead of success', async () => {
  const plugin = { id: 'fixture', name: 'Fixture', version: '1.0', permissions: [], enabled: false, status: 'disabled' };
  const f = fixture({
    list: async () => [plugin],
    enable: async () => { plugin.enabled = true; plugin.status = 'error'; plugin.error = 'activate timed out (5000 ms)'; },
  });
  await f.context.RENDER.plugins(f.view);
  await f.context.ACTIONS.pluginToggle({ dataset: { pluginToggle: 'fixture' }, checked: true });
  assert.deepEqual(f.notices, [['插件启动失败', 'bad']]);
  assert.match(f.view.innerHTML, /运行错误/);
  assert.match(f.view.innerHTML, /activate timed out/);
  assert.doesNotMatch(f.view.innerHTML, /data-plugin-toggle="fixture"[^>]*\bdisabled/, 'a failed activation leaves the stop control usable');
});

test('a pushed plugin change wins over an older pending list response', async () => {
  const list = deferred();
  let onChanged;
  const f = fixture({ list: () => list.promise, onListChanged: fn => { onChanged = fn; } });
  const rendering = f.context.RENDER.plugins(f.view);
  onChanged([{ id: 'fresh', name: 'Fresh', version: '1', permissions: [], enabled: false, status: 'disabled' }]);
  list.resolve([{ id: 'stale', name: 'Stale', version: '1', permissions: [], enabled: true, status: 'ok' }]);
  await rendering;
  assert.match(f.view.innerHTML, /Fresh/);
  assert.doesNotMatch(f.view.innerHTML, /Stale/);
  assert.match(f.view.innerHTML, /停用/);
});

test('plugin management stays explicit when the desktop bridge is unavailable', async () => {
  const f = fixture(null);
  await f.context.RENDER.plugins(f.view);
  assert.match(f.view.innerHTML, /当前窗口不支持桌面插件管理/);
  assert.doesNotMatch(f.view.innerHTML, /data-plugin-toggle/);
});

test('console navigation loads the plugin route and its stylesheet', () => {
  assert.match(indexSource, /href="#plugins" data-route="plugins"/);
  assert.match(indexSource, /<script src="plugins\.js"><\/script>/);
  assert.match(indexSource, /<link rel="stylesheet" href="plugins\.css">/);
});

test('plugin UI snapshot restores panels and old reads cannot resurrect removed entries', async () => {
  let push; const initial = deferred();
  const f = fixture({list:async()=>[],uiState:()=>initial.promise,onUIChanged:fn=>{push=fn;}});
  f.context.location = {hash:''};
  push({revision:2,panels:[{pluginId:'demo',panelId:'one',label:'Example',title:'Example panel',content:'<script>text only</script>',actions:[{id:'go',label:'Run'}]}],satellites:[],openPanel:{pluginId:'demo',panelId:'one'}});
  assert.equal(f.context.location.hash,'plugins');
  assert.match(f.view.innerHTML,/Example panel/);
  assert.match(f.view.innerHTML,/&lt;script&gt;/);
  initial.resolve({revision:1,panels:[],satellites:[],openPanel:null});await Promise.resolve();await Promise.resolve();
  assert.match(f.view.innerHTML,/Example panel/);
  push({revision:3,panels:[],satellites:[],openPanel:null});
  assert.doesNotMatch(f.view.innerHTML,/Example panel/);
});

test('leaving plugins while choosing a directory cancels the unconfirmed install', async () => {
  const chosen=deferred(), calls=[];
  const f=fixture({list:async()=>[],inspect:async()=>{calls.push('inspect');return{};},install:async()=>calls.push('install')},{choosePluginDirectory:()=>chosen.promise});
  const install=f.context.ACTIONS.pluginInstall();
  f.context.S.route='settings';chosen.resolve('C:\\plugins\\fixture');await install;
  assert.deepEqual(calls,[]);assert.equal(f.confirmations.length,0);
});
