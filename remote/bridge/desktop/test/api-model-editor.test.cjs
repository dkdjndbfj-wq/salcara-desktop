'use strict';

// Exercise the shipped handlers with only synthetic DOM and network fixtures.
// No Bridge process, credentials, provider requests or native tools are used.
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const web = path.resolve(__dirname, '../../internal/console/web');
const app = fs.readFileSync(path.join(web, 'app.js'), 'utf8').replace(/\r\n/g, '\n');
const workbench = fs.readFileSync(path.join(web, 'workbench.js'), 'utf8').replace(/\r\n/g, '\n');
const section = (source, start, end) => {
  const from = source.indexOf(start), to = source.indexOf(end, from + start.length);
  assert.ok(from >= 0 && to > from, `missing source section: ${start}`);
  return source.slice(from, to);
};
const clone = value => JSON.parse(JSON.stringify(value));

function deferred() {
  let resolve, reject;
  const promise = new Promise((done, fail) => { resolve = done; reject = fail; });
  return { promise, resolve, reject };
}

function node(value = '') {
  return { value, innerHTML: '', dataset: {}, isConnected: true, focus() {}, addEventListener() {} };
}

function setup() {
  const nodes = new Map(), calls = [], pending = [], toasts = [], routes = [];
  const modal = { _html: '', get innerHTML() { return this._html; }, set innerHTML(value) {
    for (const entry of nodes.values()) entry.isConnected = false;
    nodes.clear();
    this._html = String(value);
    for (const match of this._html.matchAll(/<(input|textarea|select|div|button|span)\b([^>]*\bid="([^"]+)"[^>]*)>/g)) {
      const [, tag, attrs, id] = match;
      let val = attrs.match(/\bvalue="([^"]*)"/)?.[1] || '';
      if (tag === 'textarea' || tag === 'select') {
        const contents = this._html.slice(match.index + match[0].length).split(`</${tag}>`, 1)[0];
        if (tag === 'textarea') val = contents;
        else {
          const options = [...contents.matchAll(/<option\b([^>]*)>/g)];
          val = (options.find(item => /\bselected\b/.test(item[1])) || options[0])?.[1].match(/\bvalue="([^"]*)"/)?.[1] || '';
        }
      }
      nodes.set(id, node(val));
    }
  } };
  const context = vm.createContext({
    URL,
    S: { localDraftModels: [], route: 'accounts' },
    $: selector => selector === '#modalRoot' ? modal : nodes.get(selector.slice(1)) || null,
    esc: value => String(value ?? ''), icon: () => '', ico: () => '',
    busy: async (_button, operation) => operation(),
    api: (url, body) => { calls.push({ url, body: clone(body) }); const gate = deferred(); pending.push(gate); return gate.promise; },
    toast: (...args) => toasts.push(args), route: async () => routes.push('route'),
    confirm: async () => true,
    location: { hash: '#accounts' },
  });
  vm.runInContext(section(app, 'function localModelCatalogHTML(', 'function toolForm('), context);
  vm.runInContext('const ACTIONS = {' + section(app, '  localSave: async ', '  localSelect: async ') + '};', context);
  const handlers = vm.runInContext('ACTIONS', context);
  const account = id => ({ id, name: 'Synthetic API', baseUrl: 'https://models.example.test/v1', keyMasked: 'synthetic-mask', models: ['gpt-original'], model: 'gpt-original', authMode: 'bearer', wire: 'auto' });
  const open = (id, overrides = {}) => { context.localEditor({ ...account(id), ...overrides }); return nodes.get('laID'); };
  return { context, handlers, calls, pending, nodes, modal, toasts, routes, open, button: () => ({ dataset: {}, isConnected: true }) };
}

test('late model results and failures cannot populate a replacement editor, even for the same API', async () => {
  for (const nextID of ['api-other', 'api-original']) {
    for (const failure of [false, true]) {
      const f = setup();
      f.open('api-original');
      const request = f.handlers.localModels(f.button());
      const replacement = f.open(nextID);
      f.nodes.get('laModels').value = 'new-editor-model';
      if (failure) f.pending[0].reject(new Error('synthetic stale error'));
      else f.pending[0].resolve({ models: ['stale-provider-model'] });
      await request;
      assert.equal(f.nodes.get('laID'), replacement);
      assert.equal(f.nodes.get('laModels').value, 'new-editor-model');
      assert.equal(f.nodes.get('localProbeResult').innerHTML, '');
      assert.deepEqual(clone(f.context.S.localDraftModels), ['gpt-original']);
      assert.equal(f.toasts.length, 0);
    }
  }
});

test('upstream editor returns to service after save or cancel but never during route leave', async () => {
  const f = setup(); let returns = 0;
  const open = () => f.context.localEditor({ id: 'upstream', name: 'Synthetic', baseUrl: 'https://models.example.test/v1', models: [] }, 'api', { onReturn: async () => { returns++; } });
  open(); await f.context.closeLocalEditor(); assert.equal(returns, 1);
  open(); const save = f.handlers.localSave(f.button());
  f.pending[0].resolve({ account: { id: 'upstream' } }); await save;
  assert.equal(returns, 2); assert.equal(f.routes.length, 0);
  open(); f.nodes.get('laName').value = 'unsaved';
  const choice = deferred(); f.context.confirm = () => choice.promise;
  const cancel = f.context.closeLocalEditor();
  const leave = f.context.closeLocalEditor({ returnToService: false });
  choice.resolve(true); await Promise.all([cancel, leave]);
  assert.equal(returns, 2); assert.equal(f.nodes.get('laID'), undefined);
});

test('model reads preserve credentials, endpoint and model edits made during the request', async () => {
  for (const [field, value] of [
    ['laBase', 'https://changed.example.test/v1'], ['laKey', 'synthetic-changed-key'],
    ['laModels', 'user-edited-model'],
  ]) {
    for (const failure of [false, true]) {
      const f = setup();
      f.open('api-original');
      const request = f.handlers.localModels(f.button());
      f.nodes.get(field).value = value;
      const expectedModels = f.nodes.get('laModels').value;
      if (failure) f.pending[0].reject(new Error('synthetic old endpoint error'));
      else f.pending[0].resolve({ models: ['old-endpoint-result'] });
      await request;
      assert.equal(f.nodes.get(field).value, value, field);
      assert.equal(f.nodes.get('laModels').value, expectedModels, field);
      assert.equal(f.nodes.get('localProbeResult').innerHTML, '', field);
    }
  }
});

test('a completed model read updates only its unchanged editor', async () => {
  const f = setup();
  f.open('api-original');
  const request = f.handlers.localModels(f.button());
  f.pending[0].resolve({ models: ['claude-test', 'deepseek-test'] });
  await request;
  assert.equal(f.nodes.get('laModels').value, 'claude-test\ndeepseek-test');
  assert.deepEqual(clone(f.context.S.localDraftModels), ['claude-test', 'deepseek-test']);
  assert.equal(f.nodes.get('laModelCount').textContent, '2 个模型');
  assert.equal(f.calls[0].body.draftOnly, true);
});

test('known provider endpoints choose a wire independently of model names and support explicit overrides', () => {
  const f = setup();
  for (const [baseUrl, authMode, wire] of [
    ['https://api.openai.com/v1', 'bearer', 'responses'],
    ['https://api.anthropic.com', 'api-key', 'anthropic'],
    ['https://api.deepseek.com/v1', 'bearer', 'chat'],
    ['https://api.deepseek.com/anthropic', 'bearer', 'anthropic'],
    ['https://openrouter.ai/api/v1', 'bearer', 'chat'],
    ['https://custom.example.test/v1', 'bearer', 'auto'],
  ]) {
    f.open('api-original', { baseUrl, authMode: undefined, wire: 'auto', models: ['gpt-fixture', 'claude-fixture'] });
    assert.equal(f.context.localForm().authMode, authMode, baseUrl);
    assert.equal(f.context.localForm().wire, wire, baseUrl);
  }
  f.nodes.get('laAuth').value = 'api-key';
  f.nodes.get('laWire').value = 'responses';
  assert.equal(f.context.localForm().authMode, 'api-key');
  assert.equal(f.context.localForm().wire, 'responses');
});

test('the API editor sends explicit resets for emptied policies, headers and limits', () => {
  const f = setup();
  f.open('api-original', { modelAllowlist: ['gpt-fixture'], modelDenylist: ['blocked-fixture'],
    extraHeaders: { 'X-Custom-Org': 'synthetic-org' }, systemPromptPrefix: 'synthetic prefix',
    fallbackModel: 'gpt-fixture', priority: 4, weight: 8, maxConcurrency: 3, maxRpm: 12, maxTpm: 1000 });
  for (const id of ['laModelAllowlist', 'laModelDenylist', 'laExtraHeaders', 'laSystemPrompt', 'laFallbackModel',
    'laPriority', 'laWeight', 'laMaxConcurrency', 'laMaxRpm', 'laMaxTpm']) f.nodes.get(id).value = '';
  const body = clone(f.context.localForm());
  assert.deepEqual(body.modelAllowlist, []);
  assert.deepEqual(body.modelDenylist, []);
  assert.deepEqual(body.extraHeaders, {});
  assert.equal(body.systemPromptPrefix, '');
  assert.equal(body.fallbackModel, '');
  for (const field of ['priority', 'weight', 'maxConcurrency', 'maxRpm', 'maxTpm']) assert.equal(body[field], 0, field);
});

test('a provider without model discovery can save a manually entered directory', async () => {
  const f = setup();
  f.open('api-original', { models: [], model: '' });
  const discovery = f.handlers.localModels(f.button());
  f.pending[0].reject(new Error('模型列表返回 HTTP 404'));
  await discovery;
  assert.match(f.modal.innerHTML, /<textarea[^>]*id="laModels"/);
  f.nodes.get('laModels').value = 'custom-model-one\ncustom-model-two';
  const save = f.handlers.localSave(f.button());
  assert.deepEqual(f.calls[1].body.models, ['custom-model-one', 'custom-model-two']);
  assert.equal(f.calls[1].body.modelsManual, true);
  f.pending[1].resolve({ account: { id: 'api-original' } });
  await save;
  assert.equal(f.modal.innerHTML, '');
});

test('late saves do not close a new editor or erase edits made while saving', async () => {
  for (const mutation of ['replacement', 'url', 'key', 'models']) {
    const f = setup();
    f.open('api-original');
    const request = f.handlers.localSave(f.button());
    if (mutation === 'replacement') f.open('api-next');
    else f.nodes.get({ url: 'laBase', key: 'laKey', models: 'laModels' }[mutation]).value = 'synthetic-new-input';
    const editor = f.nodes.get('laID'), html = f.modal.innerHTML;
    f.pending[0].resolve({ account: { id: 'api-original' } });
    await request;
    assert.equal(f.nodes.get('laID'), editor, mutation);
    assert.equal(f.modal.innerHTML, html, mutation);
    assert.equal(f.routes.length, 0, mutation);
    assert.equal(f.toasts.length, 0, mutation);
  }
});

test('unchanged editor saves trimmed model IDs and closes after acknowledgement', async () => {
  const f = setup();
  f.open('api-original');
  f.nodes.get('laModels').value = '  claude-fixture  \n\n deepseek-fixture\r\n';
  const request = f.handlers.localSave(f.button());
  assert.deepEqual(f.calls[0].body.models, ['claude-fixture', 'deepseek-fixture']);
  assert.equal(f.calls[0].body.modelsManual, true);
  assert.notEqual(f.modal.innerHTML, '');
  f.pending[0].resolve({ account: { id: 'api-original' } });
  await request;
  assert.equal(f.modal.innerHTML, '');
  assert.equal(f.routes.length, 1);
});

test('editing during a new API save retains the created ID for the next save', async () => {
  const f = setup();
  f.open('');
  f.nodes.get('laKey').value = 'synthetic-new-key';
  const first = f.handlers.localSave(f.button());
  f.nodes.get('laModels').value = 'newer-draft-model';
  f.pending[0].resolve({ account: { id: 'new-saved-api' } });
  await first;
  assert.equal(f.nodes.get('laID').value, 'new-saved-api');
  assert.equal(f.nodes.get('laModels').value, 'newer-draft-model');
  const next = f.handlers.localSave(f.button());
  assert.equal(f.calls[1].body.id, 'new-saved-api');
  f.pending[1].resolve({ account: { id: 'new-saved-api' } });
  await next;
  assert.equal(f.modal.innerHTML, '');
});

test('manual catalog ownership survives reopening and resets only after an accepted explicit refresh', async () => {
  const f = setup();
  f.open('api-original');
  assert.equal(f.context.localForm().modelsManual, false);
  f.nodes.get('laModels').value = '  gpt-original \n';
  assert.equal(f.context.localForm().modelsManual, false, 'whitespace is not a model-directory edit');
  f.nodes.get('laModels').value = 'gpt-original\ncustom-upstream-alias';
  assert.equal(f.context.localForm().modelsManual, true);
  f.nodes.get('laModels').value = '';
  assert.equal(f.context.localForm().modelsManual, true, 'an intentionally cleared directory is still manual');

  f.open('api-manual', { modelsManual: true, models: ['custom-upstream-alias'] });
  assert.equal(f.context.localForm().modelsManual, true, 'saved manual ownership must survive reopening');
  const request = f.handlers.localModels(f.button());
  assert.equal(f.calls[0].body.modelsManual, true);
  f.pending[0].resolve({ models: ['gpt-refreshed'] });
  await request;
  assert.equal(f.context.localForm().modelsManual, false, 'an accepted explicit refresh restores upstream ownership');
  assert.deepEqual(clone(f.context.localForm().models), ['gpt-refreshed']);

  f.open('api-auto');
  assert.equal(f.context.localForm().modelsManual, false, 'opening another API must not inherit manual state');
});

test('stale model refresh cannot clear a newly opened manual directory flag', async () => {
  const f = setup();
  f.open('api-original');
  const request = f.handlers.localModels(f.button());
  f.open('api-manual', { modelsManual: true, models: ['custom-upstream-alias'] });
  f.pending[0].resolve({ models: ['stale-upstream-result'] });
  await request;
  assert.equal(f.context.localForm().modelsManual, true);
  assert.deepEqual(clone(f.context.localForm().models), ['custom-upstream-alias']);
});

test('editor speed choices and deduplicated model IDs are included in the saved draft', () => {
  const f = setup();
  f.open('api-original', { speedMode: 'ultrafast' });
  assert.equal(f.context.localForm().speedMode, 'ultrafast');
  f.nodes.get('laSpeed').value = 'fast';
  f.nodes.get('laModels').value = ' first-model \nsecond-model\nfirst-model\n';
  assert.equal(f.context.localForm().speedMode, 'fast');
  assert.deepEqual(clone(f.context.localForm().models), ['first-model', 'second-model']);
  f.context.localEditorDraftSync();
  assert.equal(f.nodes.get('laModelCount').textContent, '2 个模型');
  assert.equal(f.nodes.get('laDraftNote').textContent, '有未保存的更改');
});

test('cancelled discard preserves the draft and a later accepted discard closes it', async () => {
  const f = setup(); f.open('api-original');
  let confirms = 0;
  f.context.confirm = async () => { confirms++; return false; };
  f.nodes.get('laName').value = '尚未保存的名字';
  assert.equal(await f.context.closeLocalEditor(), false);
  assert.equal(f.nodes.get('laName').value, '尚未保存的名字');
  f.context.confirm = async () => { confirms++; return true; };
  assert.equal(await f.context.closeLocalEditor(), true);
  assert.equal(f.modal.innerHTML, ''); assert.equal(confirms, 2);
  assert.equal(f.calls.length, 0);
});

test('clean close does not ask and an old discard confirmation cannot close another editor', async () => {
  const f = setup(); f.open('api-original');
  f.context.confirm = async () => { throw new Error('clean editor must not confirm'); };
  assert.equal(await f.context.closeLocalEditor(), true);
  f.open('api-original'); f.nodes.get('laName').value = 'changed';
  const gate = deferred(); f.context.confirm = () => gate.promise;
  const closing = f.context.closeLocalEditor();
  f.open('api-next'); gate.resolve(true);
  assert.equal(await closing, false); assert.equal(f.nodes.get('laID').value, 'api-next');
});

test('a declined close during save retains the editor and a failed save permits retry', async () => {
  const f = setup(); f.open('api-original'); f.nodes.get('laName').value = 'changed';
  const pending = f.handlers.localSave(f.button());
  f.context.confirm = async message => { assert.match(message, /关闭不会撤销/); return false; };
  assert.equal(await f.context.closeLocalEditor(), false);
  assert.equal(f.context.S.localEditorDraft.saving, true);
  f.pending[0].reject(new Error('synthetic unavailable'));
  await assert.rejects(pending, /synthetic unavailable/);
  assert.equal(f.context.S.localEditorDraft.saving, false);
  assert.equal(f.nodes.get('laName').value, 'changed');
  assert.match(f.nodes.get('localSaveError').textContent, /synthetic unavailable/);
  const retry = f.handlers.localSave(f.button());
  f.pending[1].resolve({ account: { id: 'api-original' } }); await retry;
  assert.equal(f.modal.innerHTML, '');
});

test('the editor freezes its source revision and adopts only its acknowledged next revision', async () => {
  const f = setup();
  f.open('api-original', { sourceRevision: 'source-at-open' });
  const request = f.handlers.localSave(f.button());
  assert.equal(f.calls[0].body.sourceRevision, 'source-at-open');
  f.nodes.get('laName').value = 'newer input';
  f.pending[0].resolve({ account: { id: 'api-original', sourceRevision: 'acknowledged-source' } });
  await request;
  assert.equal(f.context.localForm().sourceRevision, 'acknowledged-source');
  assert.equal(f.nodes.get('laName').value, 'newer input');
  const retry = f.handlers.localSave(f.button());
  assert.equal(f.calls[1].body.sourceRevision, 'acknowledged-source');
  f.pending[1].resolve({ account: { id: 'api-original', sourceRevision: 'final-source' } }); await retry;
});

test('invalid endpoint, header and numeric limits fail before sending a save', async () => {
  for (const [id, value, message] of [
    ['laBase', 'https://user:secret@example.test', /服务地址/],
    ['laBase', 'https://example.test?secret=x', /服务地址/],
    ['laExtraHeaders', 'Authorization: replacement', /请求头/],
    ['laExtraHeaders', 'Malformed line', /请求头/],
    ['laMaxConcurrency', '10001', /最大并发/],
    ['laMaxRpm', '1.5', /请求上限/],
  ]) {
    const f = setup(); f.open('api-original'); f.nodes.get(id).value = value;
    await assert.rejects(f.handlers.localSave(f.button()), message);
    assert.equal(f.calls.length, 0); assert.notEqual(f.modal.innerHTML, '');
    assert.equal(f.context.S.localEditorDraft.saving, false);
  }
});

test('a save without an acknowledged account keeps the editor and exposes retryable feedback', async () => {
  const f = setup(); f.open('api-original');
  const request = f.handlers.localSave(f.button()); f.pending[0].resolve({});
  await assert.rejects(request, /尚未确认保存/);
  assert.equal(f.nodes.get('laID').value, 'api-original');
  assert.match(f.nodes.get('localSaveError').textContent, /尚未确认保存/);
});

test('late save failure stays with the discarded editor and cannot reject onto a replacement', async () => {
  const f = setup(); f.open('api-original');
  const request = f.handlers.localSave(f.button());
  f.open('api-replacement');
  f.pending[0].reject(new Error('stale failure'));
  await request;
  assert.equal(f.nodes.get('laID').value, 'api-replacement');
  assert.equal(f.nodes.get('localSaveError').textContent, undefined);
  assert.equal(f.toasts.length, 0);
});

test('repeated discard attempts share one confirmation and saved source changes report cleared models', async () => {
  const f = setup(); f.open('api-original'); f.nodes.get('laName').value = 'changed';
  const choice = deferred(); let confirms = 0;
  f.context.confirm = () => { confirms++; return choice.promise; };
  const first = f.context.closeLocalEditor(), second = f.context.closeLocalEditor();
  assert.equal(confirms, 1); choice.resolve(false);
  assert.deepEqual(await Promise.all([first, second]), [false, false]);
  const save = f.handlers.localSave(f.button());
  f.pending[0].resolve({ account: { id: 'api-original', models: null } }); await save;
  assert.match(f.toasts.at(-1)[0], /请重新读取模型目录/);
});

test('refreshing a mixed catalog preserves the staged native-picker choice without changing saved selection', async () => {
  const { setup: workbenchFixture } = require('./helpers/workbench.cjs');
  for (const target of ['codex', 'codex-desktop']) {
    for (const override of [false, true]) {
      const f = workbenchFixture();
      f.backend.bindings[target] = { accountId: 'api-empty', model: 'old-model', override };
      await f.change(target, 'api-shared');
      f.WB.accountOpen.override = override;
      await f.wbAccountModels(f.WB.accountOpen, false);
      assert.equal(f.WB.accountOpen.override, override);
      assert.equal(f.backend.bindings[target].accountId, 'api-empty');
      assert.equal(f.backend.bindings[target].model, 'old-model');
      assert.equal(f.calls.filter(call => call.url === '/api/local/bind').length, 0);
    }
  }
});
