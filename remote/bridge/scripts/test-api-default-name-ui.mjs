import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

const source = await readFile(new URL('../internal/console/web/app.js', import.meta.url), 'utf8');
const editor = source.slice(source.indexOf('function localEditor('), source.indexOf('function localTransportForDraft('));
const save = source.slice(source.indexOf('  localSave: async (b) => {'), source.indexOf('  localModels: async (b) => {'));

function harness(name = '', id = '') {
  const body = { id, name, baseUrl: 'https://api.example.co.uk/v1', key: id ? '' : 'synthetic' };
  const input = { value: id }, modal = { innerHTML: 'editor' }, posts = [];
  const ctx = vm.createContext({
    localForm: () => ({ ...body }), busy: async (_b, fn) => fn(),
    $: selector => selector === '#laID' ? input : modal,
    api: async (path, data) => { posts.push({ path, data }); return { account: { id: 'saved', name: 'example' } }; },
    S: { route: 'accounts' }, toast: () => {}, location: { hash: '#accounts' }, route: async () => {},
  });
  vm.runInContext('globalThis.action = ({' + save + '}).localSave;', ctx);
  return { body, input, modal, posts, ctx, run: () => ctx.action({ dataset: {}, isConnected: true }) };
}

test('API editor explains optional name without another required field or long description', () => {
  assert.match(editor, /id="laName"[^>]*placeholder="留空使用站点名称"/);
  assert.doesNotMatch(editor, /名称由你决定|id="laName"[^>]*required/);
});

test('blank create and edit submit to the authoritative offline backend name resolver', async () => {
  for (const id of ['', 'existing']) {
    const h = harness('  ', id);
    await h.run();
    assert.equal(h.posts.length, 1);
    assert.equal(h.posts[0].data.name, '  ');
    assert.equal(h.posts[0].data.id, id);
    assert.equal(h.modal.innerHTML, '');
  }
});

test('explicit name stays unchanged and a missing new key is still rejected', async () => {
  const h = harness('我的备用');
  await h.run();
  assert.equal(h.posts[0].data.name, '我的备用');
  const empty = harness(); empty.body.key = '';
  await assert.rejects(empty.run, /请填写 API Key/);
  assert.equal(empty.posts.length, 0);
});

test('default-name response cannot overwrite a name typed while save was pending', async () => {
  const h = harness();
  let resolve;
  h.ctx.api = async () => new Promise(done => { resolve = done; });
  const pending = h.run();
  h.body.name = '保存期间输入的名称';
  resolve({ account: { id: 'saved', name: 'example' } });
  await pending;
  assert.equal(h.input.value, 'saved');
  assert.equal(h.body.name, '保存期间输入的名称');
  assert.equal(h.modal.innerHTML, 'editor');
});
