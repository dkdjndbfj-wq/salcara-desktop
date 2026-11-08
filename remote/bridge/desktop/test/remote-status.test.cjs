'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const source = fs.readFileSync(path.resolve(__dirname, '../../internal/console/web/app.js'), 'utf8');

function fixture(mode) {
  const nodes = new Map();
  const node = id => { if (!nodes.has(id)) nodes.set(id, { textContent: '', title: '', className: '' }); return nodes.get(id); };
  const context = vm.createContext({
    S: { route: 'login', state: { config: { effectiveRemoteMode: mode } } }, RM: { direct: null },
    rmEffectiveMode: config => config?.effectiveRemoteMode,
    $: selector => node(selector), $$: () => [],
  });
  vm.runInContext(source.slice(source.indexOf('const STATUS ='), source.indexOf('const TOOL_LABEL =')), context);
  vm.runInContext(source.slice(source.indexOf('function connectionPresentation('), source.indexOf('function setApprovalBadge(')), context);
  return { context, node, label: () => node('.conn-label').textContent };
}

test('global direct indicator uses the selected mode and listener status without claiming phone connectivity', () => {
  const f = fixture('direct');
  f.context.setConn({ state: 'not_logged_in' });
  assert.equal(f.label(), '直连模式');
  assert.equal(f.node('#connPill').className, 'conn st-direct');
  f.context.RM.direct = { enabled: true, running: true, connected: false };
  f.context.setConn({ state: 'connected', error: 'old Hub error' });
  assert.equal(f.label(), '直连已启用');
  assert.equal(f.node('#connPill').className, 'conn st-direct', 'listener availability is not phone-online status');
  assert.doesNotMatch(f.node('#connPill').title, /old Hub error/);
  assert.equal(f.node('#remoteStatusLabel').textContent, '直连已启用');
  assert.equal(f.node('.lbl').textContent, '直连已启用');
});

test('a direct listener error is distinct from inactive Hub credentials', () => {
  const f = fixture('direct');
  f.context.RM.direct = { enabled: true, running: false, error: 'port unavailable' };
  f.context.setConn({ state: 'invalid_key', error: 'saved Hub key rejected' });
  assert.equal(f.label(), '直连待恢复');
  assert.equal(f.node('#connPill').className, 'conn st-error');
  assert.equal(f.node('#connPill').title, 'port unavailable');
  assert.equal(f.node('#heroErr').textContent, 'port unavailable');
});

test('disabled mode ignores old online snapshots and Bot mode is named explicitly', () => {
  const f = fixture('none');
  f.context.RM.direct = { enabled: true, running: true, connected: true };
  f.context.setConn({ state: 'connected' });
  assert.equal(f.label(), '本地模式');
  f.context.S.state.config.effectiveRemoteMode = 'bot';
  f.context.setConn({ state: 'connecting' });
  assert.equal(f.label(), 'Bot · 连接中');
  f.context.setConn({ state: 'connected' });
  assert.equal(f.label(), 'Bot · 已连接');
});

test('loss of the local status stream remains visible even when direct mode was enabled', () => {
  const f = fixture('direct');
  f.context.RM.direct = { enabled: true, running: true };
  f.context.setConn({ state: 'connecting', error: '与本机程序的连接断开', consoleDisconnected: true });
  assert.equal(f.label(), '状态连接中断');
  assert.equal(f.node('#connPill').className, 'conn st-connecting');
  assert.match(f.node('#connPill').title, /本机程序/);
});
