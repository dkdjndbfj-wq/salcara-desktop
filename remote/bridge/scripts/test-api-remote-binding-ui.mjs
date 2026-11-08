import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

const source = await readFile(new URL('../internal/console/web/app.js', import.meta.url), 'utf8');
const formSource = source.slice(source.indexOf('function localForm() {'), source.indexOf('\nfunction toolForm('));
const editorSource = source.slice(source.indexOf('function localModelCatalogHTML('), source.indexOf('\nfunction localTransportForDraft('));
const modelCatalogSource = source.slice(source.indexOf('function localModelCatalogHTML('), source.indexOf('\nfunction localEditor('));
const localModelsStart = source.indexOf('  localModels: async (b) => {');
const localModelsEnd = source.indexOf('  localSelect: async', localModelsStart);
const localModelsAction = source.slice(localModelsStart, localModelsEnd).trim().replace(/,$/, '').replace(/^localModels:\s*/, '');

function formHarness({ legacy = false, touched = false, enabled = true, selected = [] } = {}) {
  const values = new Map();
  for (const id of ['laID', 'laName', 'laBase', 'laKey', 'laModel', 'laWorkspace', 'laModels', 'laPriority', 'laWeight', 'laMaxConcurrency', 'laMaxRpm', 'laMaxTpm', 'laSystemPrompt', 'laFallbackModel', 'laModelAllowlist', 'laModelDenylist', 'laExtraHeaders']) values.set('#' + id, { value: '' });
  const root = { dataset: { legacy: String(legacy), touched: String(touched) } };
  values.set('#laRemoteEnabled', { checked: enabled, closest: () => root });
  const context = vm.createContext({
    S: { localDraftModels: [], localDraftModelsManual: false },
    $: selector => values.get(selector),
    document: { querySelectorAll: selector => selector === '[data-remote-hub]:checked' ? selected.map(remoteHub => ({ dataset: { remoteHub } })) : [] },
    localTransportForDraft: () => ({ authMode: 'bearer', wire: 'auto' }),
  });
  vm.runInContext(formSource, context);
  return context.localForm();
}

test('new API form scopes the default grant to the chosen saved station', () => {
  const value = formHarness({ selected: ['https://current.example/salcara-hub'] });
  assert.deepEqual(Array.from(value.remoteHubUrls), ['https://current.example/salcara-hub']);
  assert.equal(value.remoteHubBindingsSet, true);
});

test('explicit switch off persists an empty allowlist', () => {
  const value = formHarness({ enabled: false, selected: ['https://current.example/salcara-hub'] });
  assert.deepEqual(Array.from(value.remoteHubUrls), []);
  assert.equal(value.remoteHubBindingsSet, true);
});

test('untouched legacy accounts omit the new policy so saving does not change behavior', () => {
  const value = formHarness({ legacy: true, enabled: true, selected: ['https://current.example/salcara-hub'] });
  assert.equal(Object.hasOwn(value, 'remoteHubUrls'), false);
  assert.equal(Object.hasOwn(value, 'remoteHubBindingsSet'), false);
});

test('interacting with a legacy account writes an explicit station policy', () => {
  const enabled = formHarness({ legacy: true, touched: true, enabled: true, selected: ['https://one.example/salcara-hub'] });
  assert.deepEqual(Array.from(enabled.remoteHubUrls), ['https://one.example/salcara-hub']);
  const disabled = formHarness({ legacy: true, touched: true, enabled: false, selected: ['https://one.example/salcara-hub'] });
  assert.deepEqual(Array.from(disabled.remoteHubUrls), []);
});

test('API editor presents a remote access switch and the registered station list', () => {
  const modal = { innerHTML: '' };
  const local = { remoteStations: [{ hubUrl: 'https://current.example/salcara-hub', name: '当前站点' }] };
  const context = vm.createContext({
    S: { local, state: { config: { effectiveHubUrl: 'https://current.example/salcara-hub' } }, localDraftModels: [], localDraftTransport: null },
    $: selector => selector === '#modalRoot' ? modal : { focus() {}, addEventListener() {} },
    localForm: () => ({}),
    esc: value => String(value), icon: () => '', keyCardFace: () => 'blue',
    localHubKey: value => String(value || '').replace(/\/+$/, '').replace(/\/v1$/i, ''),
    localRemoteStationRows: () => local.remoteStations,
  });
  vm.runInContext(editorSource, context);
  context.localEditor(null);
  assert.match(modal.innerHTML, /class="modal account-modal key-editor account-add-modal"/);
  assert.match(modal.innerHTML, /class="api-editor-section api-editor-models" aria-label="模型列表"/);
  assert.match(modal.innerHTML, /data-act="localModels" aria-label="从上游获取模型"/);
  assert.match(modal.innerHTML, /<textarea[^>]*id="laModels"/);
  assert.match(modal.innerHTML, /id="laModelCount"/);
  assert.match(modal.innerHTML, /获取结果只填入当前草稿/);
  assert.match(modal.innerHTML, /class="adv key-editor-more api-editor-advanced"/);
  assert.match(modal.innerHTML, /允许中转站 \/ Bot 使用此 API/);
  assert.match(modal.innerHTML, /无需重复绑定手机/);
  assert.doesNotMatch(modal.innerHTML, /先在「手机远程」|首次在|仍需单独授权/);
  assert.match(modal.innerHTML, /当前站点/);
  assert.match(modal.innerHTML, /data-remote-hub="https:\/\/current\.example\/salcara-hub" checked/);
});

test('model read fills the editable draft without submitting a persistent refresh', async () => {
  let submitted;
  const nodes = new Map([
    ['#laID', { value: '' }],
    ['#laModels', { value: 'old-model' }],
    ['#laModelCatalog', { innerHTML: '<code>old-model</code>' }],
    ['#laModelSyncState', { textContent: '1 个已保存模型' }],
    ['#localProbeResult', { innerHTML: '' }],
  ]);
  const context = vm.createContext({
    S: { localDraftModels: ['old-model'], localDraftModelsManual: true },
    $: selector => nodes.get(selector) || null,
    localForm: () => ({ key: 'synthetic-key', baseUrl: 'https://api.example.test', models: nodes.get('#laModels').value }),
    busy: async (_button, run) => run(),
    api: async (_path, body) => { submitted = body; return { models: ['new-model', '<img src=x>'] }; },
    esc: value => String(value).replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;').replaceAll('"', '&quot;').replaceAll("'", '&#39;'),
    ico: () => '',
    toast() {},
    route: async () => {},
  });
  vm.runInContext(modelCatalogSource, context);
  vm.runInContext('globalThis.syncModels = (' + localModelsAction + ')', context);
  await context.syncModels({ dataset: {} });
  assert.equal(nodes.get('#laModels').value, 'new-model\n<img src=x>');
  assert.match(nodes.get('#laModelCatalog').innerHTML, /new-model/);
  assert.match(nodes.get('#laModelCatalog').innerHTML, /&lt;img src=x&gt;/);
  assert.equal(nodes.get('#laModelSyncState').textContent, '2 个模型 · 已填入草稿');
  assert.equal(submitted.draftOnly, true);
  assert.equal(context.S.localDraftModelsManual, false);
});

test('a late model sync cannot overwrite a changed provider draft', async () => {
  let resolve;
  const nodes = new Map([
    ['#laID', { value: '' }],
    ['#laModels', { value: 'saved-model' }],
    ['#laModelCatalog', { innerHTML: '<code>saved-model</code>' }],
    ['#laModelSyncState', { textContent: '1 个已保存模型' }],
    ['#localProbeResult', { innerHTML: '' }],
  ]);
  let baseUrl = 'https://api.example.test';
  const context = vm.createContext({
    S: { localDraftModels: ['saved-model'], localDraftModelsManual: false },
    $: selector => nodes.get(selector) || null,
    localForm: () => ({ key: 'synthetic-key', baseUrl, models: nodes.get('#laModels').value }),
    busy: async (_button, run) => run(),
    api: () => new Promise(done => { resolve = done; }),
    esc: value => String(value),
    ico: () => '',
    toast() {},
    route: async () => {},
  });
  vm.runInContext(modelCatalogSource, context);
  vm.runInContext('globalThis.syncModels = (' + localModelsAction + ')', context);
  const pending = context.syncModels({ dataset: {} });
  baseUrl = 'https://new.example.test';
  resolve({ models: ['stale-model'] });
  await pending;
  assert.equal(nodes.get('#laModels').value, 'saved-model');
  assert.equal(nodes.get('#laModelCatalog').innerHTML, '<code>saved-model</code>');
  assert.equal(nodes.get('#laModelSyncState').textContent, '配置已更改，请重新同步');
});

test('a new API cannot be remotely enabled before a station is registered', () => {
  const modal = { innerHTML: '' };
  const local = { remoteStations: [] };
  const context = vm.createContext({
    S: { local, state: { config: { effectiveHubUrl: '' } }, localDraftModels: [], localDraftTransport: null },
    $: selector => selector === '#modalRoot' ? modal : { focus() {}, addEventListener() {} },
    localForm: () => ({}),
    esc: value => String(value), icon: () => '', keyCardFace: () => 'blue',
    localHubKey: value => String(value || '').replace(/\/+$/, '').replace(/\/v1$/i, ''),
    localRemoteStationRows: () => local.remoteStations,
  });
  vm.runInContext(editorSource, context);
  context.localEditor(null);
  assert.match(modal.innerHTML, /id="laRemoteEnabled" type="checkbox"\s+disabled/);
  assert.match(modal.innerHTML, /首次在「手机远程」绑定，之后在这里管理 API/);
  assert.match(modal.innerHTML, /电脑直连无需登记站点/);
});

test('saved stations remain available after restart without an active connection', () => {
  const helper = source.slice(source.indexOf('function localHubKey('), source.indexOf('function localForm('));
  const context = vm.createContext({ S: { local: { remoteStations: [{ hubUrl: 'https://saved.example/hub/v1/', name: '已绑定站点' }] }, state: { config: { loggedIn: false, remoteConnections: [{ hubUrl: 'https://saved.example/hub' }] } } } });
  vm.runInContext(helper, context);
  const rows = context.localRemoteStationRows();
  assert.equal(rows.length, 1); assert.equal(rows[0].hubUrl, 'https://saved.example/hub'); assert.equal(rows[0].name, '已绑定站点');
  context.S.local.remoteStations = [];
  assert.equal(context.localRemoteStationRows().length, 1, 'saved config is still usable while the cached account list refreshes');
  context.S.state.config = { effectiveHubUrl: 'https://unregistered.example/hub', remoteConnections: [] };
  assert.equal(context.localRemoteStationRows().length, 0, 'a URL alone is never treated as a completed binding');
});

test('an already paired direct phone is not instructed to bind a station', () => {
  const modal = { innerHTML: '' };
  const context = vm.createContext({ S: { local: { remoteStations: [] }, state: { config: { remoteModes: { direct: { configured: true, paired: true } } } } },
    $: selector => selector === '#modalRoot' ? modal : { focus() {}, addEventListener() {} },
    localForm: () => ({}), esc: String, icon: () => '', keyCardFace: () => 'blue', localHubKey: String, localRemoteStationRows: () => [] });
  vm.runInContext(editorSource, context); context.localEditor(null);
  assert.match(modal.innerHTML, /已完成直连绑定，无需登记中转站/);
  assert.match(modal.innerHTML, /此处的站点授权不影响直连/);
  assert.doesNotMatch(modal.innerHTML, /首次在「手机远程」|先在「手机远程」/);
});
