'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.resolve(__dirname, '../../internal/console/web/launch-progress.js'), 'utf8');

function setup(fetch) {
  class Element {
    constructor(tag) { this.tag = tag; this.children = []; this.attrs = {}; this.style = {}; this.textContent = ''; }
    appendChild(node) { this.children.push(node); node.parentNode = this; return node; }
    setAttribute(name, value) { this.attrs[name] = value; }
    addEventListener() {}
    focus() {}
    remove() { this.parentNode.children = this.parentNode.children.filter(node => node !== this); }
    querySelector(tag) { return this.children.find(node => node.tag === tag) || this.children.map(node => node.querySelector(tag)).find(Boolean); }
  }
  const body = new Element('body');
  const context = vm.createContext({
    document: { body, createElement: tag => new Element(tag), addEventListener() {}, removeEventListener() {} },
    window: { addEventListener() {}, removeEventListener() {} }, icon: () => '',
    AbortController, TextDecoder, TypeError, SyntaxError, fetch,
    setInterval() {}, clearInterval() {}, setTimeout() {}, clearTimeout() {},
  });
  vm.runInContext(source, context);
  const api = vm.runInContext('({beginLaunchProgress, launchAPI, inlineAgentProgress, requireSafeAgentReceipt})', context);
  return { ...api, body, text: () => {
    const collect = node => [node.textContent, ...node.children.map(collect)].join(' ');
    return collect(body);
  } };
}

for (const [target, name] of [['codex', 'Codex CLI'], ['deepseek-harness', 'DeepSeek Harness'], ['hermes', 'Hermes']]) {
  test(`apply-only progress identifies ${name} without announcing a launch`, () => {
    const ui = setup();
    const progress = ui.beginLaunchProgress(target, 'Fixture API', [], '应用配置');
    assert.equal(progress.operation, '应用配置');
    assert.equal(ui.body.querySelector('section').attrs['aria-label'], '应用配置 ' + name);
    assert.equal(ui.body.querySelector('button').attrs['aria-label'], '关闭应用配置进度');
    assert.match(ui.text(), /正在提交应用配置请求/);
    progress.finish();
    assert.match(ui.text(), /应用配置请求已完成/);
    assert.doesNotMatch(ui.text(), /启动/);
  });
}

test('apply-only warnings, failure, unknown outcome, and cancellation keep their operation', () => {
  const cases = [
    [progress => { progress.event({ type: 'phase', step: 'save', label: '写入配置', state: 'warning' }); progress.finish(); }, '应用配置请求已完成 · 有提示'],
    [progress => progress.fail(new Error('Fixture failure')), '应用配置未完成'],
    [progress => progress.fail({ uncertain: true }), '应用配置结果未确认'],
    [progress => progress.fail({ started: true }), '配置已应用 · 最终状态未保存'],
    [progress => progress.cancel(), '页面或配置已变化，未继续应用配置'],
  ];
  for (const [complete, expected] of cases) {
    const ui = setup();
    complete(ui.beginLaunchProgress('codex', 'Fixture API', ['写入配置'], '应用配置'));
    assert.ok(ui.text().includes(expected), ui.text());
    assert.doesNotMatch(ui.text(), /启动/);
  }
});

test('existing three-argument callers retain launch labels and completion states', () => {
  const ui = setup();
  const progress = ui.beginLaunchProgress('codex', 'Fixture API', []);
  assert.match(ui.text(), /启动 Codex CLI/);
  assert.equal(progress.operation, '启动');
  progress.fail({ started: true });
  assert.match(ui.text(), /工具已启动 · 最终状态未保存/);
});

test('HTTP apply failure retains server status and does not claim the tool was launched', async () => {
  const ui = setup(async () => ({ ok: false, status: 409, headers: new Headers(), json: async () => ({ ok: false }) }));
  const progress = ui.beginLaunchProgress('codex', 'Fixture API', [], '应用配置');
  await assert.rejects(ui.launchAPI('/fixture/apply', {}, progress), error => error.message === '应用配置失败 (409)' && error.started === false && !error.uncertain);
});

test('lost apply connection remains uncertain and is never retried', async () => {
  let calls = 0;
  const ui = setup(async () => { calls++; throw new TypeError('synthetic disconnect'); });
  const progress = ui.beginLaunchProgress('codex', 'Fixture API', ['写入配置'], '应用配置');
  await assert.rejects(ui.launchAPI('/fixture/apply', {}, progress), error => {
    progress.fail(error);
    return error.uncertain === true && error.message === '应用配置连接已中断，结果尚未确认';
  });
  assert.equal(calls, 1);
  assert.match(ui.text(), /应用配置结果未确认/);
  assert.doesNotMatch(ui.text(), /启动/);
});

test('stream integrity failures retain apply-only wording and release the reader', async () => {
  let released = false;
  const ui = setup(async () => ({ ok: true, headers: new Headers({ 'Content-Type': 'application/x-ndjson' }), body: { getReader: () => ({
    read: async () => ({ done: false, value: new TextEncoder().encode('{"sequence":2,"event":{"type":"plan","steps":[]}}\n') }),
    releaseLock() { released = true; },
  }) } }));
  await assert.rejects(ui.launchAPI('/fixture/apply', {}, { operation: '应用配置' }), error => error.uncertain === true && error.message === '应用配置进度顺序异常');
  assert.equal(released, true);
});

test('launchAPI callers without progress keep the existing default', async () => {
  const ui = setup(async () => ({ ok: false, status: 500, headers: new Headers(), json: async () => ({ ok: false }) }));
  await assert.rejects(ui.launchAPI('/fixture/switch', {}), /启动失败 \(500\)/);
});

test('safe routes reject old JSON and plain-text backends without ambiguous status or fallback', async () => {
  for (const status of [404,405]) for (const json of [true,false]) {
    let calls=0;
    const ui=setup(async()=>{calls++;return {ok:false,status,headers:new Headers({'Content-Type':json?'application/json':'text/plain'}),json:async()=>{if(!json)throw new SyntaxError('plain text');return {ok:false,error:'Unknown route'};}};});
    await assert.rejects(ui.launchAPI('/api/local/agent-change/v1',{protocol:'safe-agent-change.v1'}),error=>error.status===status&&!error.uncertain&&/更新电脑端/.test(error.message));
    assert.equal(calls,1);
  }
});

test('external cancellation aborts the same POST, removes its listener and never retries', async () => {
  let calls=0,removed=0,received;
  const controller=new AbortController(),signal=controller.signal;
  const remove=signal.removeEventListener.bind(signal);
  signal.removeEventListener=(...args)=>{removed++;remove(...args);};
  const ui=setup(async(_path,options)=>{calls++;received=options.signal;return await new Promise((_,reject)=>options.signal.addEventListener('abort',()=>reject(Error('aborted'))));});
  const pending=ui.launchAPI('/api/local/agent-change/v1',{protocol:'safe-agent-change.v1'},null,{signal});
  controller.abort();
  await assert.rejects(pending,error=>error.cancelled===true&&error.uncertain===true);
  assert.equal(received.aborted,true);assert.equal(calls,1);assert.equal(removed,1);
});

test('an already cancelled operation cannot dispatch a POST', async () => {
  let calls=0;const controller=new AbortController();controller.abort();
  const ui=setup(async()=>{calls++;throw Error('must not fetch');});
  await assert.rejects(ui.launchAPI('/safe',{},null,{signal:controller.signal}),error=>error.cancelled===true);
  assert.equal(calls,0);
});

test('known stop rejection preserves server code and status while lost receipt stays unknown', async () => {
  const ui=setup(async()=>({ok:false,status:409,headers:new Headers(),json:async()=>({ok:false,error:'stop timed out',code:'safe_agent_change_stop_timeout'})}));
  await assert.rejects(ui.launchAPI('/safe',{}),error=>error.status===409&&error.code==='safe_agent_change_stop_timeout'&&!error.uncertain);
  assert.throws(()=>ui.requireSafeAgentReceipt({ok:true,protocol:'safe-agent-change.v1',operationId:'other'},'expected'),error=>error.uncertain===true);
  assert.equal(ui.requireSafeAgentReceipt({ok:true,protocol:'safe-agent-change.v1',operationId:'expected'},'expected').ok,true);
});

test('submitted or applied outcomes preserve the authoritative uncertainty envelope', async () => {
  for(const started of [false,true]){
    const ui=setup(async()=>({ok:false,status:500,headers:new Headers(),json:async()=>({ok:false,error:'submitted',started,applied:true,uncertain:true,result:{submitted:true,uncertain:true}})}));
    await assert.rejects(ui.launchAPI('/safe',{}),error=>error.applied===true&&error.started===started&&error.uncertain===true&&error.result.submitted===true);
    assert.throws(()=>ui.requireSafeAgentReceipt({ok:true,protocol:'safe-agent-change.v1',operationId:'expected',result:{submitted:true,uncertain:true}},'expected'),error=>error.uncertain===true&&error.submitted===true);
  }
});

test('inline phases update the owner without creating any progress overlay', () => {
  const ui=setup(),owner={},stages=[];
  const progress=ui.inlineAgentProgress(owner,()=>stages.push(owner.operationStage),false);
  for(const step of ['stop','catalog','profile','start','record'])progress.event({type:'phase',step});
  assert.deepEqual(stages,['正在停止该 Agent 的当前任务…','正在读取模型目录…','正在应用所选 API…','正在打开 Agent…','正在保存操作结果…']);
  assert.equal(ui.body.children.length,0);
});
