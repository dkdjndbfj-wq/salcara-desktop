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
  const location = { hash: '#overview', pathname: '/' }, state = { route: 'overview' };
  const pages = Object.fromEntries(['setup','overview','accounts','login','sessions','settings','usage'].map(key => [key, { title: key }]));
  const context = vm.createContext({
    $: selector => elements.get(selector.slice(1)), $$: () => [],
    document: { body }, location, PAGES: pages, S: state, RENDER: {}, ROUTE_ICON: {}, SHELL: {},
    innerWidth: 1000, innerHeight: 900, icon: () => '', setMenu() {},
    history: { pushState() { location.hash = ''; } },
    setTimeout(fn, delay) { timers.push({ fn, delay }); }, addEventListener: (name, fn) => listeners.set(name, fn),
  });
  const source = fs.readFileSync(path.resolve(__dirname, '../../internal/console/web/shell.js'), 'utf8');
  const panel = source.slice(source.indexOf('/* ---------- pages open in a panel'), source.indexOf('/* ---------- small signals'));
  vm.runInContext(panel, context);
  return { elements, timers, listeners, state, location, body, context };
}

test('all page panel close controls return home immediately and retain the existing leave animation', () => {
  for (const route of ['overview','accounts','setup','login','usage','settings']) {
    const f = fixture(); f.location.hash = '#' + route; f.state.route = route;
    f.elements.get('panelClose').dispatch('click', {});
    assert.equal(f.location.hash, '');
    assert.equal(f.state.route, 'home');
    assert.equal(f.body.classList.contains('panel-open'), false);
    assert.equal(f.elements.get('panelBack').classList.contains('leave'), true);
    assert.equal(f.elements.get('panelBack').hidden, false);
    const timer = f.timers.find(item => item.delay === 220); assert.ok(timer);
    timer.fn();
    assert.equal(f.elements.get('panelBack').hidden, true);
    assert.equal(f.elements.get('view').innerHTML, '');
  }
});

test('an older close animation cannot hide a newly reopened panel', () => {
  const f = fixture(); f.elements.get('panelClose').dispatch('click', {});
  const timer = f.timers.find(item => item.delay === 220);
  vm.runInContext('openPanel()', f.context);
  timer.fn();
  assert.equal(f.elements.get('panelBack').hidden, false);
  assert.equal(f.body.classList.contains('panel-open'), true);
});
