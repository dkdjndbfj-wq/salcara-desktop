import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

const app = await readFile(new URL('../internal/console/web/app.js', import.meta.url), 'utf8');
const shell = await readFile(new URL('../internal/console/web/shell.js', import.meta.url), 'utf8');
const appHandler = app.slice(app.indexOf("document.addEventListener('keydown', (e) => {"), app.indexOf('/* ---------------- live stream ---------------- */'));
const shellStart = shell.indexOf("addEventListener('keydown', (e) => {", shell.indexOf('function goHome()'));
const shellHandler = shell.slice(shellStart, shell.indexOf("addEventListener('popstate'", shellStart));

function fixture() {
  let documentKey, windowKey, closedPages = 0;
  const modal = { children: [{}], get innerHTML() { return ''; }, set innerHTML(value) { assert.equal(value, ''); this.children = []; } };
  const context = vm.createContext({
    document: { addEventListener: (_name, handler) => { documentKey = handler; }, body: { classList: { contains: () => true } } },
    addEventListener: (_name, handler) => { windowKey = handler; },
    $: () => modal, goHome: () => { closedPages++; }, setMenu() {}, ACTIONS: {},
  });
  vm.runInContext(appHandler, context); vm.runInContext(shellHandler, context);
  return { modal, pages: () => closedPages, key(key) {
    let stopped = false, prevented = false;
    const event = { target: { id: 'apiClientName' }, key, preventDefault() { prevented = true; }, stopPropagation() { stopped = true; } };
    documentKey(event); if (!stopped) windowKey(event); return { stopped, prevented };
  } };
}

test('one Escape closes the nested API editor and preserves the underlying account page', () => {
  const f = fixture();
  assert.deepEqual(f.key('Escape'), { stopped: true, prevented: true });
  assert.equal(f.modal.children.length, 0); assert.equal(f.pages(), 0);
  assert.deepEqual(f.key('Escape'), { stopped: false, prevented: false });
  assert.equal(f.pages(), 1);
});

test('other keys retain the editor and do not swallow page keyboard handling', () => {
  const f = fixture();
  assert.deepEqual(f.key('Tab'), { stopped: false, prevented: false });
  assert.equal(f.modal.children.length, 1); assert.equal(f.pages(), 0);
});
