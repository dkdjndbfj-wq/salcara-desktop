import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

const app = await readFile(new URL('../internal/console/web/app.js', import.meta.url), 'utf8');
const escape = app.slice(app.indexOf('const esc ='), app.indexOf('const ICONS ='));
const textRenderer = app.slice(app.indexOf('function renderText('), app.indexOf('function renderDiff('));
const projection = app.slice(app.indexOf('function tlKey('), app.indexOf('const KIND_ICON ='));
const timeline = app.slice(app.indexOf('const KIND_ICON ='), app.indexOf('function renderDetail('));
const nativeSession = app.slice(app.indexOf('function canOpenNativeSession('), app.indexOf('async function openRestore('));
const detail = app.slice(app.indexOf('function renderDetail('), app.indexOf('async function openSession('));
const open = app.slice(app.indexOf('async function openSession('), app.indexOf('/* ----------------', app.indexOf('async function openSession(')));
const stream = app.slice(app.indexOf('let bootApprovalChanges ='), app.indexOf('function boot()'));

function harness(tool = 'codex') {
  const nodes = { '#sessDetail': { innerHTML: '' }, '#timeline': { scrollHeight: 100, scrollTop: 100, clientHeight: 100 } };
  const listeners = new Map();
  const state = { route: 'sessions', openKey: `${tool}:fixture`, openInfo: { tool, sessionKey: `${tool}:fixture`, controllable: false },
    tl: [], tlIndex: new Map(), approvals: [], sessions: [], sessFilter: '', state: null };
  const fixture = { session: state.openInfo, events: [] };
  const context = vm.createContext({ S: state, $: selector => nodes[selector], $$: () => [],
    ico: () => '', icon: () => '', renderDiff: String, compact: String, toolChip: value => value || '', SESSION_STATUS: {}, OSX: () => 'windows',
    renderSessionList() {}, setApprovalBadge() {}, renderApprovalsBox() {}, setConn() {}, refreshWorkbenchStatus() {},
    api: async () => fixture, EventSource: class { addEventListener(name, callback) { listeners.set(name, callback); } },
  });
  vm.runInContext([escape, textRenderer, projection, timeline, nativeSession, detail, open, stream].join('\n'), context);
  return { context, state, fixture, nodes, live: event => listeners.get('event')({ data: JSON.stringify(event) }) };
}

const delegated = { type: 'message', id: 'delegation-1', role: 'user', text: '继续这项任务。', messageOrigin: { kind: 'delegation' } };

test('a structured delegation displays a compact source above the unchanged message bubble', () => {
  const { context } = harness();
  const html = context.tlItem(delegated, 'codex');
  assert.match(html, /<div class="msg-origin">.*codex-app-official\.png.*来自另一项任务/);
  assert.ok(html.indexOf('msg-origin') < html.indexOf('<div class="msg user">'));
  assert.ok(html.endsWith(`<div class="msg user">${context.renderText(delegated.text)}</div></div>`));
  assert.doesNotMatch(html, /<button|<a\b|data-act=|tabindex=|ChatGPT/);
});

test('the mark comes only from the current conversation tool, never event or claimed sender metadata', () => {
  const { context } = harness('claude');
  const event = { ...delegated, tool: 'codex', messageOrigin: { kind: 'delegation', tool: 'codex', sender: 'ChatGPT', title: '<script>fake</script>', threadId: 'private-thread' } };
  for (const tool of ['claude', 'claude-desktop']) {
    const html = context.tlItem(event, tool);
    assert.match(html, /claude-app-official\.png/);
    assert.doesNotMatch(html, /codex-app-official|ChatGPT|private-thread|script/);
  }
  assert.match(context.tlItem({ ...event, tool: 'claude' }, 'codex-desktop'), /codex-app-official\.png/);
  assert.doesNotMatch(context.tlItem(event, 'unknown-tool'), /agent-brand\//);
  assert.doesNotMatch(context.tlItem(event), /agent-brand\//);
});

test('ordinary messages and copied wrapper text never acquire a delegation source label', () => {
  const { context } = harness();
  const text = '<subagent_notification>来自另一项任务: ChatGPT</subagent_notification>';
  for (const messageOrigin of [undefined, null, 'delegation', [], { kind: 'user' }, { kind: 'Delegation' }, { source: 'delegation' }]) {
    const html = context.tlItem({ ...delegated, text, messageOrigin }, 'codex');
    assert.equal(html, `<div class="msg user">${context.renderText(text)}</div>`);
    assert.doesNotMatch(html, /msg-origin|msg-envelope|agent-brand/);
  }
});

test('only protocol user messages can display delegation provenance', () => {
  const { context } = harness();
  for (const role of ['assistant', 'system', 'tool', 'User', '', undefined, null]) {
    const html = context.tlItem({ ...delegated, role }, 'codex');
    assert.equal(html, `<div class="msg assistant">${context.renderText(delegated.text)}</div>`);
    assert.doesNotMatch(html, /msg-origin|msg-envelope|agent-brand/);
  }
});

test('long delegation bodies retain complete escaped content and code without injected controls', () => {
  const { context } = harness();
  const text = 'BEGIN\n' + '这是原始正文，不推断来源。\n'.repeat(1000) + '\n```text\n<script>synthetic()</script>\n```\nEND';
  const html = context.tlItem({ ...delegated, text }, 'claude');
  assert.ok(html.includes(`<div class="msg user">${context.renderText(text)}</div>`));
  assert.match(html, /BEGIN[\s\S]*END/);
  assert.doesNotMatch(html, /<script|<button|data-act=|ellipsis/);
});

test('history and live updates share the same origin projection and current-conversation branding', async () => {
  const f = harness('claude');
  f.fixture.events = [{ ...delegated, sessionKey: f.state.openKey, tool: 'codex' }];
  await f.context.openSession(f.state.openKey);
  const history = f.nodes['#sessDetail'].innerHTML;
  assert.match(history, /claude-app-official\.png/);
  assert.doesNotMatch(history, /codex-app-official\.png/);
  f.context.connectStream();
  f.live({ type: 'message', id: delegated.id, sessionKey: f.state.openKey, role: 'user', text: '实时补全正文' });
  assert.equal(f.state.tl.length, 1);
  assert.equal(f.state.tl[0].messageOrigin.kind, 'delegation');
  assert.match(f.nodes['#sessDetail'].innerHTML, /来自另一项任务[\s\S]*实时补全正文/);
  assert.equal((f.nodes['#sessDetail'].innerHTML.match(/msg-origin/g) || []).length, 1);
  f.live({ type: 'message', id: 'ordinary-2', sessionKey: f.state.openKey, role: 'user', text: '普通消息' });
  assert.equal((f.nodes['#sessDetail'].innerHTML.match(/msg-origin/g) || []).length, 1);
  assert.match(f.nodes['#sessDetail'].innerHTML, /<div class="msg user">普通消息<\/div>/);
});

test('explicit null replaces metadata under the existing timeline merge', () => {
  const f = harness();
  f.context.tlApply(delegated);
  f.context.tlApply({ type: 'message', id: delegated.id, role: 'user', text: 'ordinary corrected message', messageOrigin: null });
  assert.doesNotMatch(f.context.tlItem(f.state.tl[0], 'codex'), /msg-origin|msg-envelope/);
});
