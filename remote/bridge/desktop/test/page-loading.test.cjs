'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.resolve(__dirname, '../../internal/console/web/app.js'), 'utf8');

function assignment(name, endMarker) {
  const start = source.indexOf(`${name} = `);
  assert.notEqual(start, -1, `${name} renderer exists`);
  const end = source.indexOf(endMarker, start);
  assert.notEqual(end, -1, `${name} renderer end marker exists`);
  return source.slice(start, end);
}

function deferred() {
  let resolve;
  const promise = new Promise((done) => { resolve = done; });
  return { promise, resolve };
}

test('overview navigation and session entry render before first state and account reads', async () => {
  const state = deferred(), accounts = deferred();
  const headActions = { innerHTML: '' };
  const context = vm.createContext({
    S: { route: 'overview', routeRevision: 3 },
    $: (selector) => selector === '#headActions' ? headActions : null,
    icon: (name) => name,
    ico: () => '',
    loadState: () => state.promise,
    api: (endpoint) => { assert.equal(endpoint, '/api/local/accounts'); return accounts.promise; },
  });
  vm.runInContext(assignment('RENDER_overview', '\nfunction renderApprovalsBox()'), context);

  const view = { isConnected: true, innerHTML: '' };
  const render = context.RENDER_overview(view);
  assert.match(headActions.innerHTML, /href="#accounts"/);
  assert.match(headActions.innerHTML, /href="#sessions"/);
  assert.match(view.innerHTML, /正在读取本机工具和 API 配置/);

  context.S.route = 'sessions';
  context.S.routeRevision += 1;
  state.resolve({});
  accounts.resolve({ accounts: [] });
  await render;
  assert.match(view.innerHTML, /正在读取本机工具和 API 配置/, 'a stale overview response does not replace the newer route');
});

test('API account creation remains visible while the account library is loading', () => {
  const headActions = { innerHTML: '' };
  const context = vm.createContext({
    S: { route: 'accounts', routeRevision: 1, local: null },
    $: (selector) => selector === '#headActions' ? headActions : null,
    icon: (name) => name,
    loadState: () => new Promise(() => undefined),
    api: (endpoint) => { assert.equal(endpoint, '/api/local/accounts'); return new Promise(() => undefined); },
  });
  vm.runInContext(assignment('RENDER_accounts', '\nfunction localEditor('), context);

  const view = { isConnected: true, innerHTML: '' };
  context.RENDER_accounts(view);
  assert.match(headActions.innerHTML, /添加密钥/);
  assert.match(view.innerHTML, /正在读取本机 API 和账号/);
  assert.match(view.innerHTML, /密钥保存在本机/);
});

test('tools never renders legacy mutation controls from only a partial cache', async () => {
  const toolcfg = deferred();
  const rendered = [];
  const context = vm.createContext({
    S: { route: 'tools', state: { os: 'windows', config: {}, tools: [] }, toolcfg: null },
    loadState: async () => ({ os: 'windows', config: {}, tools: [] }),
    api: (endpoint) => { assert.equal(endpoint, '/api/toolcfg'); return toolcfg.promise; },
    renderToolsPage: (...args) => { rendered.push(args); },
    esc: (value) => String(value), ico: () => '',
  });
  vm.runInContext(assignment('RENDER_tools', '\n/* ---------------- 项目文件夹'), context);

  const view = { isConnected: true, innerHTML: '<div class="page-skeleton">loading</div>' };
  context.RENDER_tools(view);
  assert.deepEqual(rendered, [], 'a partial cache does not paint default-enabled legacy controls');
  toolcfg.resolve({ legacyToolcfg: { managed: true } });
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(rendered.length, 1);
  assert.equal(rendered[0][1].os, 'windows');
  assert.deepEqual(JSON.parse(JSON.stringify(rendered[0][2])), { legacyToolcfg: { managed: true } });
});
