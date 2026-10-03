const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const source = fs.readFileSync(path.resolve(__dirname, '../main.cjs'), 'utf8');

function inspect(version, initial = {}) {
  const env = { ...initial }, paths = [];
  const app = { getPath: () => path.resolve('fixture-appdata'), setPath: (...args) => paths.push(args),
    requestSingleInstanceLock: () => false, quit() {}, on() {} };
  const context = vm.createContext({ process: { env, platform: 'win32' }, __dirname: path.resolve(__dirname, '..'),
    require(name) {
      if (name === 'electron') return { app };
      if (name === './package.json') return { version };
      if (name === 'node:child_process') return { spawn() { throw new Error('Unexpected tool launch'); } };
      return require(name);
    } });
  vm.runInContext(source, context);
  return { env, paths, port: vm.runInContext('consolePort', context), title: vm.runInContext('appTitle', context) };
}

test('test desktop automatically separates vault, window state, port and application name', () => {
  const got = inspect('1.5.1-test.1');
  assert.match(got.env.SALCARA_BRIDGE_CONFIG_DIR, /SalcaraBridge-Test-1\.5$/);
  assert.equal(got.port, 47841); assert.equal(got.title, 'Salcara Bridge Test');
  assert.equal(got.paths[0][0], 'userData'); assert.match(got.paths[0][1], /desktop-ui$/);
});

test('production defaults and explicit isolated test overrides are unchanged', () => {
  const prod = inspect('1.5.1');
  assert.equal(prod.port, 47831); assert.equal(prod.env.SALCARA_BRIDGE_CONFIG_DIR, undefined); assert.equal(prod.paths.length, 0);
  const explicit = inspect('1.5.1-test.1', { SALCARA_BRIDGE_CONFIG_DIR: path.resolve('fixture-isolated'), SALCARA_BRIDGE_PORT: '47849' });
  assert.equal(explicit.port, 47849); assert.equal(explicit.env.SALCARA_BRIDGE_CONFIG_DIR, path.resolve('fixture-isolated'));
  const invalid = inspect('1.5.1-test.1', { SALCARA_BRIDGE_PORT: 'invalid' });
  assert.equal(invalid.port, 47841); assert.equal(invalid.env.SALCARA_BRIDGE_PORT, '47841');
});
