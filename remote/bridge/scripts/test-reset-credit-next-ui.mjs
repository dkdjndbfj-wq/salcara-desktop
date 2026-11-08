import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

const source = await readFile(new URL('../internal/console/web/codex-accounts.js', import.meta.url), 'utf8');
const oldID = '11111111-2222-4333-8444-555555555555';
const nextID = '22222222-2222-4333-8444-555555555555';
function deferred() { let resolve; const promise = new Promise(done => { resolve = done; }); return { promise, resolve }; }
function harness(api) {
  const context = vm.createContext({
    URL, TextEncoder, api, ACTIONS: {}, S: { route: 'accounts' },
    $: () => null, icon: () => '', esc: String,
    busy: async (_button, fn) => fn(), confirm: () => true, toast() {}, renderLocalCards() {},
    setTimeout: () => 1, clearTimeout() {},
    MutationObserver: class { observe() {} },
    document: { visibilityState: 'visible', addEventListener() {} },
  });
  vm.runInContext(source, context);
  vm.runInContext('refreshOfficialCards = async () => {}', context);
  return { context, run: () => context.ACTIONS.officialResetCredit({ dataset: { id: 'official-fixture' } }), invalidate: () => vm.runInContext('OFFICIAL_PANEL.mutation++', context) };
}

test('a subsequent reset refreshes usage and binds the new operation to the previous success', async () => {
  const calls = []; let confirmations = 0;
  const h = harness(async (path, body) => {
    calls.push({ path, body });
    if (path.endsWith('/prepare')) return { ok: true, request: body.previousRedeemRequestId
      ? { state: 'prepared', redeemRequestId: nextID } : { state: 'succeeded', redeemRequestId: oldID } };
    return { ok: true };
  });
  h.context.confirm = () => { confirmations++; return true; };
  await h.run();
  assert.deepEqual(calls.map(row => row.path), ['/api/local/codex/reset-credit/prepare', '/api/local/codex/usage/refresh', '/api/local/codex/reset-credit/prepare', '/api/local/codex/reset-credit/consume']);
  assert.equal(calls[2].body.previousRedeemRequestId, oldID);
  assert.equal(calls[3].body.redeemRequestId, nextID);
  assert.equal(confirmations, 1);
  assert.ok(calls.every(row => row.body.id === 'official-fixture'));
});

for (const state of ['unknown', 'consuming']) test(`${state} cannot trigger another confirmation or provider call`, async () => {
  const calls = [];
  const h = harness(async path => { calls.push(path); return { ok: true, request: { state, redeemRequestId: oldID } }; });
  h.context.confirm = () => { assert.fail('uncertain operation must not be presented as a new redemption'); };
  await h.run();
  assert.deepEqual(calls, ['/api/local/codex/reset-credit/prepare']);
});

test('cancelling the next operation does not consume it', async () => {
  const calls = [];
  const h = harness(async (path, body) => {
    calls.push(path);
    return { ok: true, request: { state: body.previousRedeemRequestId ? 'prepared' : 'succeeded', redeemRequestId: body.previousRedeemRequestId ? nextID : oldID } };
  });
  h.context.confirm = () => false;
  await h.run();
  assert.equal(calls.length, 3);
  assert.ok(calls.every(path => !path.endsWith('/consume')));
});

test('unsuccessful usage refresh does not prepare or consume another operation', async () => {
  const calls = [];
  const h = harness(async path => {
    calls.push(path);
    return path.endsWith('/prepare') ? { ok: true, request: { state: 'succeeded', redeemRequestId: oldID } } : { ok: false, error: '额度暂不可用' };
  });
  h.context.confirm = () => { assert.fail('must obtain a new eligible handle first'); };
  await h.run();
  assert.deepEqual(calls, ['/api/local/codex/reset-credit/prepare', '/api/local/codex/usage/refresh']);
});

for (const delayed of ['refresh', 'next']) test(`replaced account panel fences a late ${delayed} result`, async () => {
  const wait = deferred(), reached = deferred(), calls = [];
  const h = harness(async (path, body) => {
    calls.push(path);
    if (delayed === 'refresh' && path.endsWith('/usage/refresh') || delayed === 'next' && body.previousRedeemRequestId) {
      reached.resolve(); return wait.promise;
    }
    return { ok: true, request: { state: 'succeeded', redeemRequestId: oldID } };
  });
  h.context.confirm = () => { assert.fail('stale panel confirmation'); };
  const pending = h.run();
  await reached.promise;
  h.invalidate(); wait.resolve({ ok: true, request: { state: 'prepared', redeemRequestId: nextID } });
  await pending;
  assert.equal(calls.length, delayed === 'refresh' ? 2 : 3);
});

test('a concurrent completed next operation is acknowledged without creating another redemption', async () => {
  const calls = [];
  const h = harness(async path => { calls.push(path); return { ok: true, request: { state: 'succeeded', redeemRequestId: oldID } }; });
  h.context.confirm = () => { assert.fail('already completed operation must not consume again'); };
  await h.run();
  assert.equal(calls.length, 3);
  assert.ok(calls.every(path => !path.endsWith('/consume')));
});
