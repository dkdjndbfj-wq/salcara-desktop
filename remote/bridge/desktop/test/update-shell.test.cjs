'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const updatePolicy = require('../update-policy.cjs');
const source = fs.readFileSync(path.join(__dirname, '../main.cjs'), 'utf8');

test('the shell applies verified policy to the actual attached core version and never publishes an unknown policy', async () => {
  for (const [verified, version, required] of [[false, '1.6.4', null], [true, '1.6.3', true], [true, '1.6.5', false]]) {
    const posts = [];
    const state = { policyVerified: verified, required: true, version: '1.7.0', minimumSupportedVersion: '1.6.5' };
    const context = vm.createContext({ updatePolicy, policySync: null, policyError: '', consoleURL: 'http://127.0.0.1:47831/',
      getUpdater: () => ({ state: () => state }), broadcastUpdateState() {}, AbortSignal,
      require: name => name === 'electron' ? { session: { defaultSession: { cookies: { get: async () => [{ value: 'fixture-cookie' }] } } } } : require('../updater.cjs'),
      fetch: async (_url, options) => {
        assert.equal(options.headers.Cookie, 'salcara_console=fixture-cookie');
        if (options.method === 'POST') { posts.push(JSON.parse(options.body)); return { ok: true, json: async () => ({ ok: true }) }; }
        return { ok: true, json: async () => ({ pid: 123, version }) };
      },
    });
    vm.runInContext(source.slice(source.indexOf('async function syncUpdatePolicy('), source.indexOf('function updateTrayItems()')), context);
    await context.syncUpdatePolicy();
    if (!verified) assert.equal(posts.length, 0);
    else { assert.equal(posts.length, 1); assert.equal(posts[0].version, version); assert.equal(posts[0].required, required); }
  }
});

test('only explicit busy responses permit automatic install retries', async () => {
  for (const [status, code, retry] of [[409, 'update_busy', true], [409, '', false], [401, 'update_busy', false]]) {
    const context = vm.createContext({ updateOwner: { child: { pid: 123 }, cookie: 'fixture' }, consoleURL: 'http://127.0.0.1:47831/',
      app: { getVersion: () => '1.6.4' }, AbortSignal,
      fetch: async () => ({ ok: false, status, json: async () => ({ code, error: 'fixture' }) }),
    });
    vm.runInContext(source.slice(source.indexOf('async function updateRequest('), source.indexOf('async function commitForUpdate(')), context);
    await assert.rejects(context.updateRequest('prepare'), error => error.updateBusy === retry);
    await assert.rejects(context.updateRequest('commit'), error => error.updateBusy === false);
  }
});

test('discovery starts automatic updating and respects an explicit cancellation or skipped version', async () => {
  for (const [autoPaused, skipped, expected] of [[false, false, 1], [true, false, 0], [false, true, 0]]) {
    let starts = 0;
    const state = { phase: 'available', version: '1.7.0', canInstall: true, autoPaused };
    const context = vm.createContext({ announced: '', updatePolicy, syncUpdatePolicy: async () => {}, openUpdateWindow() {},
      getUpdater: () => ({ cleanup() {}, check: async () => state, skipped: () => skipped, startAutoUpdate: () => { starts++; } }),
    });
    vm.runInContext(source.slice(source.indexOf('async function checkForUpdate('), source.indexOf('function openUpdateWindow(')), context);
    await context.checkForUpdate(false); assert.equal(starts, expected);
  }
});
