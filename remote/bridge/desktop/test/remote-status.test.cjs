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
  return { context, node, label: () => node('.conn-label').textContent.replace(/^远程 · /, '') };
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

test('remote authorization failures remain in the remote entry without declaring API keys invalid', () => {
  const f = fixture('hub');
  for (const [state, label] of [['device_auth_required', '设备需重新登记'], ['source_unauthorized', '远程来源未授权'], ['pair_required', '手机需重新绑定'], ['remote_blocked', '远程授权待处理']]) {
    f.context.setConn({ state, error: 'safe remote error' });
    assert.equal(f.label(), label);
    assert.equal(f.node('#connPill').className, 'conn st-error');
    assert.match(f.node('.conn-label').textContent, /^远程 · /);
    assert.doesNotMatch(f.label(), /Key 无效|API.*失效/);
  }
});

test('Agent cards keep local API readiness when remote device authentication fails', () => {
  const { setup } = require('./helpers/workbench.cjs');
  const f = setup();
  f.S.state = { config: { effectiveRemoteMode: 'hub' }, hub: { state: 'device_auth_required', error: '设备验证失败' } };
  for (const tool of f.S.local.tools) {
    const binding = f.S.local.bindings[tool.id];
    Object.assign(binding, { accountId: 'api-shared', appliedId: 'api-shared', pending: false, remote: { state: 'error', label: '设备验证失败', detail: '设备登记凭据失效' } });
    const html = f.wbCard(tool);
    assert.match(html, /API 已应用/);
    assert.doesNotMatch(html, /设备验证失败|设备登记凭据失效|手机远程未连接|lamp-connected/);
    assert.match(html, /data-remote-capability="agent"/);
  }
  const codex = f.S.local.tools.find(tool => tool.id === 'codex');
  f.S.local.bindings.codex.pending = true;
  assert.match(f.wbCard(codex), /API 待应用/);
  assert.match(f.wbCard({ ...codex, available: false }), /工具未安装/);
});
