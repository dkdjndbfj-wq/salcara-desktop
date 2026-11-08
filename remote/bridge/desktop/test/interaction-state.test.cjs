'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs'), path = require('node:path'), vm = require('node:vm');
const web = path.resolve(__dirname, '../../internal/console/web');
const app = fs.readFileSync(path.join(web, 'app.js'), 'utf8').replace(/\r\n/g, '\n');
const section = (start, end) => app.slice(app.indexOf(start), app.indexOf(end, app.indexOf(start)));
function deferred() { let resolve, reject; const promise = new Promise((a, b) => { resolve = a; reject = b; }); return { promise, resolve, reject }; }
function node(extra = {}) {
  const attributes = new Map();
  return { dataset: {}, innerHTML: '保存', textContent: '保存', disabled: false, isConnected: true,
    getAttribute: name => attributes.get(name) ?? null, setAttribute: (name, value) => attributes.set(name, value), removeAttribute: name => attributes.delete(name),
    ...extra };
}
function confirmationFixture() {
  const elements = new Map(), listeners = new Set();
  const document = { activeElement: null, getElementById: id => elements.get(id), body: { appendChild(root) { elements.set(root.id, root); } } };
  document.createElement = () => {
    const handlers = new Map();
    const root = { id: '', innerHTML: '', addEventListener: (type, handler) => handlers.set(type, handler),
      remove() { if (elements.get(root.id) === root) elements.delete(root.id); for (const button of buttons) button.isConnected = false; },
      querySelector(selector) { return buttons[selector.includes('"1"') ? 1 : 0]; },
      click(value) { const button = buttons[value ? 1 : 0]; handlers.get('click')({ target: { closest: () => button }, stopPropagation() {} }); } };
    const buttons = [0, 1].map(value => node({ dataset: { confirm: String(value) }, focus() { document.activeElement = this; } }));
    return root;
  };
  const anchor = node({ focus() { document.activeElement = this; } }); document.activeElement = anchor;
  const context = vm.createContext({ window: {}, document, icon: () => '', esc: String,
    addEventListener: (_type, fn) => listeners.add(fn), removeEventListener: (_type, fn) => listeners.delete(fn) });
  vm.runInContext(section('const nativeConfirm =', 'const BUTTON_OPERATIONS ='), context);
  return { context, document, anchor, elements, listeners,
    key(key) { let stopped = false; for (const fn of [...listeners]) { fn({ key, preventDefault() {}, stopImmediatePropagation() { stopped = true; } }); if (stopped) break; } } };
}
test('replacement confirmation cancels its predecessor and leaves exactly one keyboard owner', async () => {
  const f = confirmationFixture();
  const first = f.context.confirmDialog('删除第一项？');
  const second = f.context.confirmDialog('切换第二项？');
  assert.equal(await first, false);
  assert.equal(f.listeners.size, 1);
  f.key('Enter');
  assert.equal(await second, true);
  assert.equal(f.listeners.size, 0);
  assert.equal(f.document.activeElement, f.anchor);
});
test('danger confirmation defaults to cancel and Enter follows the focused action', async () => {
  const f = confirmationFixture();
  const result = f.context.confirmDialog('删除当前项目？');
  assert.equal(f.document.activeElement.dataset.confirm, '0');
  f.key('Enter');
  assert.equal(await result, false);
});
test('navigation cancellation settles confirmation and returns focus without keeping handlers', async () => {
  const f = confirmationFixture(), result = f.context.confirmDialog('保存设置？');
  f.context.cancelActiveConfirmation();
  assert.equal(await result, false);
  assert.equal(f.elements.has('confirmRoot'), false);
  assert.equal(f.listeners.size, 0);
  assert.equal(f.document.activeElement, f.anchor);
});
test('confirmation focus stays in its two actions and Escape cancels only that dialog', async () => {
  const f = confirmationFixture(), result = f.context.confirmDialog('保存设置？');
  f.key('Tab'); assert.equal(f.document.activeElement.dataset.confirm, '0');
  f.key('Tab'); assert.equal(f.document.activeElement.dataset.confirm, '1');
  f.key('Escape'); assert.equal(await result, false);
});

function operationFixture() {
  const context = vm.createContext({});
  vm.runInContext(section('const BUTTON_OPERATIONS =', 'let localConversationSyncPending ='), context);
  return context;
}
test('keyboard reentry into a busy button shares one mutation and restores the original control state', async () => {
  const c = operationFixture(), button = node(), gate = deferred(); let calls = 0;
  const first = c.busy(button, () => { calls++; return gate.promise; });
  const second = c.busy(button, () => { calls++; return Promise.resolve('wrong'); });
  assert.equal(first, second); assert.equal(calls, 1); assert.equal(button.disabled, true);
  gate.resolve('saved'); assert.equal(await second, 'saved');
  assert.equal(button.innerHTML, '保存'); assert.equal(button.disabled, false); assert.equal(button.getAttribute('aria-busy'), null);
});
test('busy failures restore preexisting disabled state and allow a later retry', async () => {
  const c = operationFixture(), button = node({ disabled: true });
  button.setAttribute('aria-busy', 'false');
  await assert.rejects(c.busy(button, () => { throw new Error('disk unavailable'); }), /disk unavailable/);
  assert.equal(button.disabled, true); assert.equal(button.getAttribute('aria-busy'), 'false');
  assert.equal(await c.busy(button, async () => 2), 2);
});
test('approval delivery failure retains the pending request and opposite actions share one request', async () => {
  const c = operationFixture(), gate = deferred(); let calls = 0;
  Object.assign(c, { S: { approvals: [{ approvalId: 'one' }, { approvalId: 'two' }] },
    api: () => { calls++; return gate.promise; }, toast() {}, setApprovalBadge() {}, renderApprovalsBox() {} });
  vm.runInContext('var ACTIONS = {' + section('  approve: async ', '  send: async ') + '};', c);
  const first = c.ACTIONS.approve(node({ dataset: { id: 'one', d: 'allow' } }));
  const second = c.ACTIONS.approve(node({ dataset: { id: 'one', d: 'deny' } }));
  assert.equal(calls, 1);
  gate.reject(new Error('transport lost'));
  await Promise.all([assert.rejects(first), assert.rejects(second)]);
  assert.deepEqual(c.S.approvals.map(row => row.approvalId), ['one', 'two']);
  c.api = async () => ({ ok: true });
  await c.ACTIONS.approve(node({ dataset: { id: 'one', d: 'allow' } }));
  assert.deepEqual(Array.from(c.S.approvals, row => row.approvalId), ['two']);
});
test('unconfirmed approval response does not remove the request', async () => {
  const c = operationFixture();
  Object.assign(c, { S: { approvals: [{ approvalId: 'one' }] }, api: async () => ({}), toast() {}, setApprovalBadge() {}, renderApprovalsBox() {} });
  vm.runInContext('var ACTIONS = {' + section('  approve: async ', '  send: async ') + '};', c);
  await assert.rejects(c.ACTIONS.approve(node({ dataset: { id: 'one', d: 'allow' } })), /尚未确认/);
  assert.equal(c.S.approvals.length, 1);
});

function settingsFixture() {
  const elements = new Map(), calls = [], requests = [];
  for (const id of ['swAuto', 'swOpen', 'swPhoneAuto']) elements.set(id, node({ id, checked: false }));
  const radios = ['ask', 'auto_edits', 'auto_all'].map(value => node({ name: 'pol', value, checked: value === 'ask', closest: () => ({ classList: { toggle() {} } }) }));
  for (const key of ['autostart', 'openConsoleOnStart', 'allowPhoneAutoAll', 'approval']) elements.set('settingStatus-' + key, node({ textContent: '' }));
  elements.set('view', node()); elements.set('approvalPolicyWarning', node({ hidden: true }));
  const initial = { config: { autostart: false, openConsoleOnStart: false, allowPhoneAutoAll: false, approval: 'ask' } };
  const context = vm.createContext({ S: { state: initial, route: 'settings' }, document: { getElementById: id => elements.get(id) },
    $: selector => elements.get(selector.slice(1)), $$: () => radios, confirm: async () => true,
    api: (_url, body) => { calls.push(body); const d = deferred(); requests.push(d); return d.promise; } });
  vm.runInContext(section('const SETTINGS_FIELDS =', 'function renderSettingsPage('), context);
  context.settingsSnapshot(initial); context.projectSettingsState();
  return { context, elements, radios, calls, requests };
}
test('settings lock the changed field while allowing independent settings to save', async () => {
  const f = settingsFixture(), a = f.elements.get('swAuto'), b = f.elements.get('swOpen');
  a.checked = true; const first = f.context.saveSetting(a);
  a.checked = false; const duplicate = f.context.saveSetting(a);
  assert.equal(first, duplicate); assert.equal(a.checked, true); assert.equal(a.disabled, true);
  b.checked = true; const other = f.context.saveSetting(b);
  assert.equal(f.calls.length, 2);
  f.requests[1].resolve({ ok: true }); await other;
  f.requests[0].reject(new Error('permission rejected')); await first;
  assert.equal(a.checked, false); assert.equal(a.disabled, false);
  assert.equal(b.checked, true); assert.equal(f.context.S.state.config.openConsoleOnStart, true);
  assert.match(f.elements.get('settingStatus-autostart').textContent, /permission rejected/);
});
test('late settings snapshot cannot overwrite a setting saved after its read began', async () => {
  const f = settingsFixture(), control = f.elements.get('swAuto');
  control.checked = true; const saving = f.context.saveSetting(control);
  f.requests[0].resolve({ ok: true }); await saving;
  const stale = f.context.settingsSnapshot({ config: { autostart: false } }, 0);
  assert.equal(stale.config.autostart, true);
  const fresh = f.context.settingsSnapshot({ config: { autostart: false } });
  assert.equal(fresh.config.autostart, false, 'a new authoritative read can observe an external settings change');
});
test('a settings read started during a save cannot undo the later acknowledgement', async () => {
  const f = settingsFixture(), control = f.elements.get('swAuto');
  control.checked = true; const saving = f.context.saveSetting(control);
  const readRevision = vm.runInContext('SETTINGS_UI.revision', f.context);
  f.requests[0].resolve({ ok: true }); await saving;
  const stale = f.context.settingsSnapshot({ config: { autostart: false } }, readRevision);
  assert.equal(stale.config.autostart, true);
});
test('cancelling automatic approval restores the previous selection without saving', async () => {
  const f = settingsFixture(); f.context.confirm = async () => false;
  const auto = f.radios[2]; auto.checked = true;
  await f.context.saveSetting(auto);
  assert.equal(f.calls.length, 0); assert.equal(f.radios[0].checked, true); assert.equal(auto.checked, false);
  assert.equal(f.radios.some(r => r.disabled), false);
});
test('an accepted settings confirmation cannot write through a replaced page', async () => {
  const f = settingsFixture(), gate = deferred(); f.context.confirm = () => gate.promise;
  const saving = f.context.saveSetting(f.radios[2]);
  f.elements.set('view', node()); gate.resolve(true); await saving;
  assert.equal(f.calls.length, 0);
});

function mcpFixture() {
  const elements = new Map(), calls = [], requests = [], messages = [];
  const view = node(); elements.set('view', view);
  const root = node();
  Object.defineProperty(root, 'innerHTML', { get() { return this.markup || ''; }, set(value) {
    for (const [id, item] of elements) if (id.startsWith('mcp')) { item.isConnected = false; elements.delete(id); }
    this.markup = value;
    for (const match of value.matchAll(/<(?:input|textarea|div)\b([^>]*\bid="([^"]+)"[^>]*)>/g)) {
      elements.set(match[2], node({ id: match[2], value: match[1].match(/\bvalue="([^"]*)"/)?.[1] || '', hidden: /\bhidden\b/.test(match[1]), focus() {} }));
    }
  } }); elements.set('modalRoot', root);
  const context = vm.createContext({ PAGES: {}, RENDER: {}, ACTIONS: {}, S: { route: 'mcp' },
    $: selector => elements.get(selector.slice(1)), $$: () => [], icon: () => '', ico: () => '', esc: String,
    document: { addEventListener() {} }, confirm: async () => true, toast: (...args) => messages.push(args),
    busy: async (_b, fn) => fn(), api: (_url, body) => { calls.push(body); const d = deferred(); requests.push(d); return d.promise; } });
  vm.runInContext(fs.readFileSync(path.join(web, 'mcp.js'), 'utf8'), context);
  let form;
  context.mcpFormServer = () => ({ ...form });
  const open = name => { form = { name, command: 'fixture-command', args: [], env: {}, agents: {}, enabled: true }; context.mcpEditor(form); return elements.get('mcpPrev'); };
  return { context, elements, root, calls, requests, messages, open, edit: value => { form = { ...form, ...value }; } };
}
test('late MCP save never closes or fills a replacement editor', async () => {
  const f = mcpFixture(); f.open('first'); const saving = f.context.ACTIONS.mcpSave(node());
  const replacement = f.open('second');
  f.requests[0].resolve({ servers: [{ name: 'first' }], discovered: [] }); await saving;
  assert.equal(f.elements.get('mcpPrev'), replacement); assert.equal(replacement.value, 'second');
  assert.equal(f.elements.get('mcpEditStatus').textContent, '保存');
});
test('MCP edits made during save remain editable after the earlier snapshot commits', async () => {
  const f = mcpFixture(), owner = f.open('original');
  f.edit({ name: 'renamed' }); const saving = f.context.ACTIONS.mcpSave(node());
  f.edit({ command: 'newer command' });
  f.requests[0].resolve({ servers: [{ name: 'renamed' }], discovered: [] }); await saving;
  assert.equal(f.elements.get('mcpPrev'), owner); assert.equal(owner.value, 'renamed');
  assert.match(f.elements.get('mcpEditStatus').textContent, /当前修改尚未保存/);
});
test('MCP synchronization failure keeps the saved configuration open with its real rename identity', async () => {
  const f = mcpFixture(), owner = f.open('original'); f.edit({ name: 'renamed' });
  const saving = f.context.ACTIONS.mcpSave(node());
  f.requests[0].resolve({ servers: [{ name: 'renamed' }], discovered: [], errors: { sync: 'Agent configuration busy' } }); await saving;
  assert.equal(owner.value, 'renamed'); assert.match(f.elements.get('mcpEditStatus').textContent, /同步未完成/);
});
test('MCP save and removal cannot overlap within the same editor', async () => {
  const f = mcpFixture(); f.open('original');
  const saving = f.context.ACTIONS.mcpSave(node());
  await f.context.ACTIONS.mcpRemove(node({ dataset: { name: 'original' } }));
  await f.context.ACTIONS.mcpSave(node());
  assert.equal(f.calls.length, 1);
  f.requests[0].reject(new Error('offline')); await saving;
  const retry = f.context.ACTIONS.mcpSave(node());
  assert.equal(f.calls.length, 2);
  f.requests[1].resolve({ servers: [{ name: 'original' }], discovered: [] }); await retry;
});

test('HTTP failures preserve status and code without classifying successful JSON responses', async () => {
  const context = vm.createContext({ AbortController, setTimeout, clearTimeout,
    fetch: async () => ({ ok: false, status: 409, json: async () => ({ error: 'Please check the original task', code: 'command_delivery_uncertain' }) }) });
  vm.runInContext(section('async function api(', '/* 系统相关的文案'), context);
  await assert.rejects(context.api('/synthetic'), error => error.status === 409 && error.code === 'command_delivery_uncertain');
  context.fetch = async () => ({ ok: true, status: 200, json: async () => ({ ok: false, error: 'business error' }) });
  assert.equal((await context.api('/synthetic')).ok, false);
});
test('MCP removal checks dialog ownership after confirmation and after response', async () => {
  const f = mcpFixture(), confirmation = deferred(); f.open('first'); f.context.confirm = () => confirmation.promise;
  const stopped = f.context.ACTIONS.mcpRemove(node({ dataset: { name: 'first' } }));
  f.open('replacement'); confirmation.resolve(true); await stopped; assert.equal(f.calls.length, 0);
  f.context.confirm = async () => true; f.open('first');
  const removing = f.context.ACTIONS.mcpRemove(node({ dataset: { name: 'first' } }));
  await new Promise(resolve => setImmediate(resolve)); const replacement = f.open('second');
  f.requests[0].resolve({ servers: [], discovered: [] }); await removing;
  assert.equal(f.elements.get('mcpPrev'), replacement);
});

test('late desktop update status cannot bind actions onto the replacement settings row', async () => {
  const source = fs.readFileSync(path.join(web, 'prefs.js'), 'utf8');
  const oldRead = deferred(), newRead = deferred(); let calls = 0, bound = 0;
  const first = node({ querySelector: () => ({ addEventListener() { bound++; } }) });
  const second = node({ querySelector: () => ({ addEventListener() { bound++; } }) });
  let row = first;
  const context = vm.createContext({ $: () => row, S: { route: 'settings' }, esc: String, icon: () => '',
    window: { salcaraWindow: { updateStatus() { return ++calls === 1 ? oldRead.promise : newRead.promise; } } } });
  vm.runInContext(source.slice(source.indexOf('let updateRowRevision ='), source.indexOf('(function wrapSettings()')), context);
  const old = context.renderUpdateRow(); row = second; first.isConnected = false;
  const next = context.renderUpdateRow();
  newRead.resolve({ configured: true, current: 'new' }); await next;
  oldRead.resolve({ configured: true, current: 'old' }); await old;
  assert.match(second.innerHTML, /new/); assert.doesNotMatch(second.innerHTML, /old/);
  assert.equal(first.innerHTML, '保存'); assert.equal(bound, 2);
});
