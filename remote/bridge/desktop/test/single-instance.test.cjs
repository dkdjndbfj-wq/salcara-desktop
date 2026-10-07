'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const source = fs.readFileSync(path.resolve(__dirname, '../main.cjs'), 'utf8');

function fixture(ownsLock) {
  const handlers = new Map(); let quits = 0, starts = 0;
  const app = { requestSingleInstanceLock: () => ownsLock, quit() { quits++; },
    on(event, handler) { handlers.set(event, handler); }, whenReady: () => new Promise(() => undefined) };
  const context = vm.createContext({ process: { env: {}, platform: 'win32' }, __dirname: path.resolve(__dirname, '..'),
    require(name) {
      if (name === 'electron') return { app };
      if (name === './package.json') return { version: '1.6.3' };
      if (name === 'node:child_process') return { spawn() { starts++; throw new Error('Unexpected Agent/core launch'); } };
      return require(name);
    } });
  vm.runInContext(source, context);
  return { handlers, context, quits: () => quits, starts: () => starts };
}

test('a second Salcara process exits before starting another core or Agent', () => {
  const f = fixture(false);
  assert.equal(f.quits(), 1); assert.equal(f.starts(), 0); assert.equal(f.handlers.has('second-instance'), false);
});

test('repeat launch restores and focuses the existing window, leaving other Agents untouched', () => {
  const f = fixture(true), events = [];
  f.context.mainWindowFixture = { isDestroyed: () => false, show: () => events.push('show'), isMinimized: () => true,
    restore: () => events.push('restore'), focus: () => events.push('focus') };
  vm.runInContext('window = mainWindowFixture;', f.context);
  f.handlers.get('second-instance')({}, ['Salcara.exe']);
  assert.deepEqual(events, ['show', 'restore', 'focus']); assert.equal(f.starts(), 0); assert.equal(f.quits(), 0);
  events.length = 0;
  f.handlers.get('second-instance')({}, ['Salcara.exe', '--background']);
  assert.deepEqual(events, []);
});
