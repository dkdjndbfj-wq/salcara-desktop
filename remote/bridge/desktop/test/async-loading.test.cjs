'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const source = fs.readFileSync(path.resolve(__dirname, '../../internal/console/web/app.js'), 'utf8');

function fixture() {
  const handlers = new Map(); let paints = 0, states = 0, resolveApprovals, rejectApprovals;
  const badgeStates = [];
  const context = vm.createContext({
    S: { route: 'overview', approvals: [], sessions: [] },
    window: { addEventListener() {} }, $$: () => [], icon: () => '',
    route() { paints++; return Promise.resolve(); },
    loadState() { states++; return new Promise(() => undefined); },
    api: endpoint => { assert.equal(endpoint, '/api/approvals'); return new Promise((resolve, reject) => { resolveApprovals = resolve; rejectApprovals = reject; }); },
    EventSource: class { addEventListener(type, handler) { handlers.set(type, handler); } },
    setConn() {}, refreshWorkbenchStatus() {}, setApprovalBadge() { badgeStates.push(context.S.approvalsLoadState); }, renderApprovalsBox() {},
  });
  vm.runInContext(source.slice(source.indexOf('let bootApprovalChanges = null;'), source.indexOf('// Boot after every page script')), context);
  return { context, handlers, badgeStates, paints: () => paints, states: () => states, resolve: value => resolveApprovals(value), reject: error => rejectApprovals(error) };
}

test('startup paints the existing page before approvals or optional local state finish', async () => {
  const f = fixture(); f.context.boot();
  assert.equal(f.paints(), 1); assert.equal(f.states(), 1); assert.ok(f.handlers.has('event'));
  assert.equal(f.context.S.approvalsLoadState, 'loading');
  f.resolve({ approvals: [] });
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(f.context.S.approvalsLoadState, 'ready');
  assert.equal(f.badgeStates.at(-1), 'ready');
});

test('stream rendering coalesces bursts, defers while hidden, and drops an abandoned page', () => {
  const f = fixture(), frames = [], events = new Map();
  let detail = 0, list = 0;
  f.context.requestAnimationFrame = cb => { frames.push(cb); return frames.length; };
  f.context.cancelAnimationFrame = () => {};
  f.context.document = { hidden: false, addEventListener: (type, fn) => events.set(type, fn) };
  f.context.renderDetail = () => detail++;
  f.context.renderSessionList = () => list++;
  f.context.S.route = 'sessions'; f.context.S.routeRevision = 1;
  for (let i = 0; i < 100; i++) f.context.scheduleSessionRender(true, true);
  assert.equal(frames.length, 1);
  frames.shift()(); assert.equal(detail, 1); assert.equal(list, 1);
  f.context.document.hidden = true;
  for (let i = 0; i < 100; i++) f.context.scheduleSessionRender(false, true);
  assert.equal(frames.length, 0);
  f.context.document.hidden = false; f.context.scheduleSessionRender(false, false);
  frames.shift()(); assert.equal(detail, 2);
  f.context.scheduleSessionRender(true, true); f.context.S.routeRevision++;
  frames.shift()(); assert.equal(detail, 2); assert.equal(list, 1);
});

test('an asynchronous startup snapshot cannot erase new questions or resurrect resolved permissions', async () => {
  const f = fixture(); f.context.boot();
  const emit = event => f.handlers.get('event')({ data: JSON.stringify(event) });
  emit({ type: 'approval.request', approvalId: 'new-question', sessionKey: 'codex:fixture' });
  emit({ type: 'approval.resolved', approvalId: 'resolved-permission', sessionKey: 'codex:fixture' });
  f.resolve({ approvals: [{ approvalId: 'existing-permission' }, { approvalId: 'resolved-permission' }] });
  await new Promise(resolve => setImmediate(resolve));
  assert.deepEqual(Array.from(f.context.S.approvals, item => item.approvalId), ['existing-permission', 'new-question']);
  assert.equal(f.context.S.approvalsLoadState, 'ready');
});

test('a failed startup approval read reports an error without deleting questions received live', async () => {
  const f = fixture(); f.context.boot();
  f.handlers.get('event')({ data: JSON.stringify({ type: 'approval.request', approvalId: 'live-question', sessionKey: 'codex:fixture' }) });
  f.reject(new Error('temporarily unavailable'));
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(f.context.S.approvalsLoadState, 'error');
  assert.equal(f.badgeStates.at(-1), 'error');
  assert.deepEqual(Array.from(f.context.S.approvals, item => item.approvalId), ['live-question']);
});
