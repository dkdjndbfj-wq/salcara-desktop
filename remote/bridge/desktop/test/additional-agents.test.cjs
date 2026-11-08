'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const vm = require('node:vm');
const { setup } = require('./helpers/workbench.cjs');

test('additional Agent cards preserve configuration without session shortcuts and settings keep history', async () => {
  for (const id of ['deepseek-harness', 'hermes']) {
    const f = setup();
    const tool = {id, name:id, available:true};
    const binding = {accountId:'api-shared', model:'grok-test', protocol:'chat', pending:true};
    f.S.local.tools.push(tool); f.backend.tools.push(tool);
    f.S.local.bindings[id] = {...binding}; f.backend.bindings[id] = {...binding};
    const html = f.wbCard(tool);
    assert.ok(html.includes(`data-family="${id}"`));
    assert.doesNotMatch(html, /data-act="toolRestore"|查看会话/);
    await f.wbOpenAccount(id,'api-shared');
    const button = f.button('wbApplyGo');
    assert.ok(button);
    assert.equal(button.disabled,true);
    f.document.getElementById('wbReady').checked = true;
    f.WB.accountOpen.ready = true;
    await f.ACTIONS.wbApplyGo(button);
    const switches = f.calls.filter(call => call.url === '/api/local/switch');
    assert.equal(switches.length,1);
    assert.equal(switches[0].body.target,id);
    assert.equal(switches[0].body.applyOnly,true);
    assert.equal(switches[0].body.confirmed,true);
    assert.ok(f.toasts.some(x => x.message.includes('未启动工具')));
    f.context.loadLogs = () => {};
    const settings = { innerHTML: '' };
    f.context.renderSettingsPage(settings, { config: {}, version: 'test', os: 'linux' });
    const historyLink = settings.innerHTML.match(/<a\b[^>]*href="#sessions"[^>]*>([\s\S]*?)<\/a>/);
    assert.ok(historyLink, 'settings must retain the conversation history route');
    assert.match(historyLink[1], />对话记录</);
  }
});

test('API cards offer both additional agents only for ordinary API sources', () => {
  const f = setup();
  const html = vm.runInContext('accountLaunchButtonsHTML(S.local.accounts[0])',f.context);
  for (const id of ['deepseek-harness','hermes']) assert.ok(html.includes(`data-target="${id}"`));
  f.S.local.accounts[0].sourceType = 'official_account';
  const official = vm.runInContext('accountLaunchButtonsHTML(S.local.accounts[0])',f.context);
  for (const id of ['deepseek-harness','hermes']) assert.ok(!official.includes(`data-target="${id}"`));
});
