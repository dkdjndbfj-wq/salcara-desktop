import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

const source = await readFile(new URL('../internal/console/web/usage-ledger.js', import.meta.url), 'utf8');
function deferred() {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

// Synthetic UI boundaries: replacing a panel really removes its nodes and
// creates distinct error/content nodes. No native files, credentials or HTTP.
function harness() {
  const nodes = new Map(), requests = [], pending = [];
  const context = vm.createContext({
    OFFICIAL_PANEL: { epoch: 0 }, ACTIONS: {}, usageHead() {},
    $: (selector) => nodes.get(selector) || null,
    officialPanel(_title, _body) {
      context.OFFICIAL_PANEL.epoch++;
      nodes.clear();
      nodes.set('#usageLedgerContent', { innerHTML: '', isConnected: true });
      nodes.set('#officialError', { textContent: '', isConnected: true });
    },
    api: async (path, body) => {
      requests.push({ path, body });
      if (pending.length) return pending.shift().promise;
      return { count: 2, pending: 0, retention: { maxRecords: 1000, days: 30 }, records: [{ id: 'synthetic-row', time: 0, model: 'synthetic-current-model', state: 'completed', httpStatus: 200, durationMs: 1, usageFinal: false }] };
    },
    busy: async (button, fn) => { button.disabled = true; try { return await fn(); } finally { if (button.isConnected) button.disabled = false; } },
    compact: String, esc: (value) => String(value).replace(/[&<>"']/g, (char) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[char]),
    confirm: () => true,
  });
  vm.runInContext(source, context);
  return {
    nodes, requests,
    action: (name) => context.ACTIONS[name]({ disabled: false, isConnected: true }),
    pauseNext() { const wait = deferred(); pending.push(wait); return wait; },
    close() { context.OFFICIAL_PANEL.epoch++; for (const node of nodes.values()) node.isConnected = false; nodes.clear(); },
  };
}

for (const action of ['usageLedger', 'usageLedgerRefresh', 'usageLedgerMore']) {
  test(`old ${action} failure cannot replace the error state of a reopened ledger`, async () => {
    const h = harness();
    if (action !== 'usageLedger') await h.action('usageLedger');
    const oldRead = h.pauseNext();
    const oldAction = h.action(action);
    if (action === 'usageLedgerMore') assert.match(h.requests.at(-1).path, /before=synthetic-row/, 'more must issue a real pagination read');
    h.close();
    await h.action('usageLedger');
    const content = h.nodes.get('#usageLedgerContent'), error = h.nodes.get('#officialError');
    oldRead.reject(new Error('synthetic retired-panel read failure'));
    await oldAction;
    assert.equal(error.textContent, '', 'retired request wrote its error into the new panel');
    assert.match(content.innerHTML, /synthetic-current-model/, 'new panel content was replaced');
  });

  test(`current ${action} read failure remains visible and does not discard existing records`, async () => {
    const h = harness();
    if (action !== 'usageLedger') await h.action('usageLedger');
    const currentRead = h.pauseNext();
    const currentAction = h.action(action);
    currentRead.reject(new Error('synthetic current-panel read failure'));
    await currentAction;
    assert.equal(h.nodes.get('#officialError').textContent, 'synthetic current-panel read failure');
    if (action !== 'usageLedger') assert.match(h.nodes.get('#usageLedgerContent').innerHTML, /synthetic-current-model/);
  });
}

test('failure after closing the ledger does not recreate a panel or throw on detached nodes', async () => {
  const h = harness(), pending = h.pauseNext();
  const opened = h.action('usageLedger');
  h.close();
  pending.reject(new Error('synthetic closed-panel read failure'));
  await opened;
  assert.equal(h.nodes.size, 0);
});

test('failed mutation stays visible after the status reload', async () => {
  const h = harness();
  await h.action('usageLedger');
  const pending = h.pauseNext();
  const mutation = h.action('usageLedgerRetry');
  pending.reject(new Error('synthetic save failure'));
  await mutation;
  assert.equal(h.nodes.get('#officialError').textContent, 'synthetic save failure');
  assert.match(h.nodes.get('#usageLedgerContent').innerHTML, /synthetic-current-model/);
  assert.match(h.requests.at(-1).path, /usage-ledger\?limit=100$/);
});
