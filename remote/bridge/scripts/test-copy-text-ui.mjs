import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

const app = await readFile(new URL('../internal/console/web/app.js', import.meta.url), 'utf8');
const source = app.slice(app.indexOf('async function copyText(text, what) {'), app.indexOf('function relTime('));
assert.ok(source.startsWith('async function copyText('), 'test must execute the production clipboard helper');
const syntheticSecret = 'synthetic-clipboard-only-fixture';

function harness({ modern = 'reject', legacy = 'success', failure = '' } = {}) {
  const state = { attached: [], created: [], toasts: [], modernCopies: [], legacyCopies: [], selected: 0 };
  const document = {
    createElement(tag) {
      assert.equal(tag, 'textarea');
      const node = {
        value: '', style: {}, attributes: {},
        setAttribute(name, value) { this.attributes[name] = value; },
        select() {
          state.selected++;
          if (failure === 'select') throw new Error('synthetic selection failure');
        },
        remove() { state.attached = state.attached.filter(row => row !== this); },
      };
      state.created.push(node);
      return node;
    },
    body: {
      appendChild(node) {
        if (failure === 'append') throw new Error('synthetic append failure');
        state.attached.push(node);
      },
    },
    execCommand(command) {
      assert.equal(command, 'copy');
      const node = state.attached[0];
      assert.equal(node.value, syntheticSecret);
      assert.equal(node.readOnly, true);
      assert.equal(node.tabIndex, -1);
      assert.equal(node.attributes['aria-hidden'], 'true');
      assert.match(node.style.cssText, /left:-10000px/);
      assert.match(node.style.cssText, /opacity:0/);
      if (legacy === 'throw') throw new Error('synthetic copy rejection');
      if (legacy === 'false') return false;
      state.legacyCopies.push(node.value);
      return true;
    },
  };
  const navigator = modern === 'missing' ? {} : {
    clipboard: {
      async writeText(value) {
        if (modern === 'reject') throw new Error('synthetic clipboard permission denied');
        state.modernCopies.push(value);
      },
    },
  };
  const context = vm.createContext({ document, navigator, toast: (message, kind) => state.toasts.push({ message, kind }) });
  vm.runInContext(source, context);
  return { state, copy: () => context.copyText(syntheticSecret, '客户端密钥') };
}

function assertNoSecretNodesOrToasts(state) {
  assert.equal(state.attached.length, 0);
  for (const node of state.created) assert.equal(node.value, '', 'detached nodes must also lose plaintext');
  for (const toast of state.toasts) assert.ok(!toast.message.includes(syntheticSecret));
}

test('modern clipboard success avoids creating a plaintext DOM fallback', async () => {
  const { state, copy } = harness({ modern: 'success' });
  assert.equal(await copy(), true);
  assert.deepEqual(state.modernCopies, [syntheticSecret]);
  assert.equal(state.created.length, 0);
  assert.equal(state.toasts[0].kind, 'ok');
  assertNoSecretNodesOrToasts(state);
});

for (const modern of ['reject', 'missing']) {
  test(`modern clipboard ${modern} uses a hidden fallback and clears it after success`, async () => {
    const { state, copy } = harness({ modern });
    assert.equal(await copy(), true);
    assert.deepEqual(state.legacyCopies, [syntheticSecret]);
    assert.equal(state.toasts[0].kind, 'ok');
    assertNoSecretNodesOrToasts(state);
  });
}

for (const legacy of ['false', 'throw']) {
  test(`legacy copy ${legacy} is reported as failure and never retains a secret node`, async () => {
    const { state, copy } = harness({ legacy });
    assert.equal(await copy(), false);
    assert.deepEqual(state.toasts, [{ message: '复制失败，请重试或手动复制', kind: 'bad' }]);
    assertNoSecretNodesOrToasts(state);
  });
}

for (const failure of ['append', 'select']) {
  test(`fallback ${failure} failure still clears the temporary value and node`, async () => {
    const { state, copy } = harness({ failure });
    assert.equal(await copy(), false);
    assert.equal(state.toasts[0].kind, 'bad');
    assertNoSecretNodesOrToasts(state);
  });
}
