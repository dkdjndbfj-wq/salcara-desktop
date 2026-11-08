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
  const api = vm.runInContext('({beginLaunchProgress, launchAPI})', context);
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
