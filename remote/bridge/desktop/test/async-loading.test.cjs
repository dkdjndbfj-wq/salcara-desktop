'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const source = fs.readFileSync(path.resolve(__dirname, '../../internal/console/web/app.js'), 'utf8');

function fixture() {
  const handlers = new Map(); let paints = 0, states = 0, resolveApprovals;
  const context = vm.createContext({
    S: { route: 'overview', approvals: [], sessions: [] },
    window: { addEventListener() {} }, $$: () => [], icon: () => '',
    route() { paints++; return Promise.resolve(); },
    loadState() { states++; return new Promise(() => undefined); },
    api: endpoint => { assert.equal(endpoint, '/api/approvals'); return new Promise(resolve => { resolveApprovals = resolve; }); },
    EventSource: class { addEventListener(type, handler) { handlers.set(type, handler); } },
    setConn() {}, refreshWorkbenchStatus() {}, setApprovalBadge() {}, renderApprovalsBox() {},
  });
  vm.runInContext(source.slice(source.indexOf('let bootApprovalChanges = null;'), source.indexOf('// Boot after every page script')), context);
  return { context, handlers, paints: () => paints, states: () => states, resolve: value => resolveApprovals(value) };
}

test('startup paints the existing page before approvals or optional local state finish', async () => {
  const f = fixture(); f.context.boot();
  assert.equal(f.paints(), 1); assert.equal(f.states(), 1); assert.ok(f.handlers.has('event'));
  f.resolve({ approvals: [] });
  await new Promise(resolve => setImmediate(resolve));
});

test('an asynchronous startup snapshot cannot erase new questions or resurrect resolved permissions', async () => {
  const f = fixture(); f.context.boot();
  const emit = event => f.handlers.get('event')({ data: JSON.stringify(event) });
  emit({ type: 'approval.request', approvalId: 'new-question', sessionKey: 'codex:fixture' });
  emit({ type: 'approval.resolved', approvalId: 'resolved-permission', sessionKey: 'codex:fixture' });
  f.resolve({ approvals: [{ approvalId: 'existing-permission' }, { approvalId: 'resolved-permission' }] });
  await new Promise(resolve => setImmediate(resolve));
  assert.deepEqual(Array.from(f.context.S.approvals, item => item.approvalId), ['existing-permission', 'new-question']);
});
