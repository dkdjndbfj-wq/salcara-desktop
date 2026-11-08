'use strict';

// Run the production panel event/route code with a disposable DOM contract.
// This is not a Windows pointer hit-test and does not touch a user's app.
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs'), path = require('node:path'), vm = require('node:vm');

function fixture() {
  const elements = new Map(), timers = [], listeners = new Map();
  function node(id) {
    const classes = new Set(), events = new Map();
    return { id, hidden: false, innerHTML: '', style: { setProperty() {} },
      classList: { add: n => classes.add(n), remove: n => classes.delete(n), contains: n => classes.has(n) },
      addEventListener: (name, fn) => events.set(name, fn), dispatch: (name, event) => events.get(name)?.(event),
      cloneNode() { return node(id); }, replaceWith(replacement) { elements.set(id, replacement); },
    };
  }
  for (const id of ['panelBack','panel','panelClose','view','pageIco','pageTitle','pageSub','headActions','modalRoot']) elements.set(id, node(id));
  const body = node('body'); body.classList.add('panel-open');
  elements.get('panelBack').classList.add('enter');
  elements.get('view').innerHTML = 'synthetic existing page';
  elements.get('modalRoot').children = [];
  const location = { hash: '#overview', pathname: '/', search: '' }, state = { route: 'overview' };
  const pages = Object.fromEntries(['setup','overview','accounts','login','sessions','settings','usage'].map(key => [key, { title: key }]));
  const context = vm.createContext({
    $: selector => elements.get(selector.slice(1)), $$: () => [],
    document: { body }, location, PAGES: pages, S: state, RENDER: {}, ROUTE_ICON: {}, SHELL: {},
    innerWidth: 1000, innerHeight: 900, icon: () => '', setMenu() {},
    history: { pushState() { location.hash = ''; }, replaceState(_state, _title, url) { location.hash = url.includes('#') ? '#' + url.split('#')[1] : ''; } },
    setTimeout(fn, delay) { timers.push({ fn, delay }); }, addEventListener: (name, fn) => listeners.set(name, fn),
  });
  const source = fs.readFileSync(path.resolve(__dirname, '../../internal/console/web/shell.js'), 'utf8');
  const panel = source.slice(source.indexOf('/* ---------- pages open in a panel'), source.indexOf('/* ---------- small signals'));
  vm.runInContext(panel, context);
  return { elements, timers, listeners, state, location, body, context };
}

test('every page returns home immediately: pages are inline, so nothing waits on an animation', () => {
  for (const route of ['overview','accounts','setup','login','usage','settings']) {
    const f = fixture(); f.location.hash = '#' + route; f.state.route = route;
    f.elements.get('panelClose').dispatch('click', {});
    assert.equal(f.location.hash, '');
    assert.equal(f.state.route, 'home');
    assert.equal(f.body.classList.contains('panel-open'), false);
    assert.equal(f.elements.get('panelBack').hidden, true);
    assert.equal(f.elements.get('view').innerHTML, '');
    assert.equal(f.timers.length, 0);
  }
});

test('reopening after going home shows the page area again', () => {
  const f = fixture(); f.elements.get('panelClose').dispatch('click', {});
  vm.runInContext('openPanel()', f.context);
  assert.equal(f.elements.get('panelBack').hidden, false);
  assert.equal(f.body.classList.contains('panel-open'), true);
});

test('refreshing the current route closes its confirmation before replacing the page', async () => {
  const f = fixture(), oldView = f.elements.get('view');
  const draft = { route: 'overview', routeRevision: 7, generation: 4, ready: true };
  f.state.routeRevision = 7;
  f.context.WB = { launchGeneration: 4, accountOpen: draft, launchPending: false };
  f.elements.get('modalRoot').innerHTML = '<div>confirmed launch draft</div>';
  let resolveConfirmation, finishRender, cancellations = 0;
  const confirmation = new Promise(resolve => { resolveConfirmation = resolve; });
  const rendering = new Promise(resolve => { finishRender = resolve; });
  f.context.cancelActiveConfirmation = () => { cancellations++; resolveConfirmation(false); };
  f.context.RENDER.overview = async view => {
    assert.equal(f.elements.get('modalRoot').innerHTML, '');
    assert.equal(f.context.WB.accountOpen, null);
    assert.equal(cancellations, 1);
    await rendering;
    view.innerHTML = 'refreshed Agent page';
  };

  const refresh = vm.runInContext('route()', f.context);
  assert.equal(await confirmation, false);
  assert.equal(f.state.route, 'overview');
  assert.equal(f.state.routeRevision, 8);
  assert.equal(f.context.WB.launchGeneration, 5);
  assert.notEqual(f.elements.get('view'), oldView);
  assert.equal(f.elements.get('modalRoot').innerHTML, '');
  finishRender();
  await refresh;
  assert.equal(f.elements.get('view').innerHTML, 'refreshed Agent page');
  assert.equal(f.context.WB.accountOpen, null);
});

test('cancelled navigation preserves the API editor and restores the current URL without rendering', async () => {
  const f = fixture(), input = {}, view = f.elements.get('view');
  f.state.route = 'accounts'; f.state.routeRevision = 5;
  f.elements.set('laID', input); f.state.localEditorDraft = { input };
  f.elements.get('modalRoot').innerHTML = 'unsaved API draft';
  f.context.closeLocalEditor = async () => false;
  f.location.hash = '#settings';
  await f.context.route();
  assert.equal(f.location.hash, '#accounts'); assert.equal(f.state.route, 'accounts');
  assert.equal(f.state.routeRevision, 5); assert.equal(f.elements.get('view'), view);
  assert.equal(f.elements.get('modalRoot').innerHTML, 'unsaved API draft');
});

test('navigation attempts sharing a draft decision only render the latest requested destination', async () => {
  const f = fixture(), input = {}, renders = [];
  f.state.route = 'accounts'; f.elements.set('laID', input); f.state.localEditorDraft = { input };
  let decide;
  const confirmation = new Promise(resolve => { decide = resolve; });
  f.context.closeLocalEditor = () => confirmation;
  f.context.RENDER.settings = () => renders.push('settings');
  f.context.RENDER.usage = () => renders.push('usage');
  f.location.hash = '#settings'; const first = f.context.route();
  f.location.hash = '#usage'; const second = f.context.route();
  decide(true); await Promise.all([first, second]);
  assert.deepEqual(renders, ['usage']); assert.equal(f.state.route, 'usage');
  assert.equal(f.state.routeRevision, 1);
});
