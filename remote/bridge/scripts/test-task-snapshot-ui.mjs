import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

const app = await readFile(new URL('../internal/console/web/app.js', import.meta.url), 'utf8');
const helpers = app.slice(app.indexOf('const esc ='), app.indexOf('const ICONS ='));
const timeline = app.slice(app.indexOf('const KIND_ICON ='), app.indexOf('function renderDetail('));
const context = vm.createContext({ ico: () => '', renderDiff: text => text, compact: value => String(value) });
vm.runInContext(`${helpers}\n${timeline}`, context);

const task = { type: 'tool', kind: 'plan', planKind: 'tasks', status: 'done', title: '任务列表 · 已知 0/1', output: '○ #1 等待验证', outputFinal: true };

test('a completed task snapshot is an update, never a claim that its unfinished tasks completed', () => {
  const html = context.tlItem(task);
  assert.match(html, /已更新/);
  assert.match(html, /已知 0\/1/);
  assert.doesNotMatch(html, /chip ok|>完成</);
});

test('tool failures and running reads remain visible while ordinary completed tools retain completion', () => {
  assert.match(context.tlItem({ ...task, status: 'failed' }), /chip bad.*失败/);
  assert.match(context.tlItem({ ...task, status: 'running' }), /运行中/);
  assert.match(context.tlItem({ ...task, kind: 'command' }), /chip ok.*完成/);
  assert.match(context.tlItem({ ...task, planKind: 'proposal' }), /chip ok.*完成/);
});

test('task titles, scope hints and output stay escaped in the desktop timeline', () => {
  const html = context.tlItem({ ...task, title: '<img src=x onerror=alert(1)>', detail: '<script>private()</script>', output: '<b>task</b>' });
  assert.doesNotMatch(html, /<img|<script|<b>/);
  assert.match(html, /&lt;img/);
  assert.match(html, /&lt;script&gt;/);
  assert.match(html, /&lt;b&gt;task/);
});
