'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const path = require('node:path');
const os = require('node:os');
const { randomUUID } = require('node:crypto');
const { spawn } = require('node:child_process');
const { once } = require('node:events');
const { createCoreLifecycle } = require('../core-lifecycle.cjs');

test('real child launch, exit and simultaneous retry use one replacement process', { timeout: 15000 }, async t => {
  let stopping = false, port = 0, current;
  const children = [], failures = [], ready = [];
  const core = createCoreLifecycle({
    launch() {
      // A synthetic loopback health server only: no Bridge config, Agent,
      // credentials, installation or external request is used by this test.
      const child = spawn(process.execPath, ['-e', `
        const server = require('node:http').createServer((_req,res) => {
          res.writeHead(200, {'content-type':'application/json'});
          res.end(JSON.stringify({service:'synthetic-core'}));
        });
        server.listen(0,'127.0.0.1',() => process.stdout.write(String(server.address().port)+'\\n'));
      `], { windowsHide: true, stdio: ['ignore', 'pipe', 'ignore'] });
      children.push(child);
      child.stdout.once('data', value => { port = Number(String(value).trim()); });
      child.once('exit', () => { port = 0; });
      return child;
    },
    async healthy(timeout) {
      if (!port) return false;
      try {
        const response = await fetch(`http://127.0.0.1:${port}/healthz`, { redirect: 'error', signal: AbortSignal.timeout(timeout) });
        return response.ok && (await response.json()).service === 'synthetic-core';
      } catch { return false; }
    },
    isStopping: () => stopping,
    onOwner(child) { current = child; }, onReady(value) { ready.push(value); }, onFailure(cause) { failures.push(cause); },
    readyTimeout: 5000,
  });
  t.after(async () => {
    stopping = true; core.pause();
    for (const child of children) {
      if (child.exitCode === null && child.signalCode === null) {
        const exited = once(child, 'exit'); child.kill(); await exited;
      }
    }
  });
  assert.deepEqual(await Promise.all([core.start(), core.start(), core.start({ manual: true })]), [true, true, true]);
  assert.equal(children.length, 1); assert.equal(current, children[0]);
  const exited = once(current, 'exit'); current.kill(); await exited;
  assert.equal(await core.start({ manual: true }), true);
  assert.equal(children.length, 2); assert.equal(current, children[1]);
  assert.equal(ready.length, 2); assert.deepEqual(failures, []);
});

test('a real OS spawn failure reaches the retryable failure state without waiting for exit', { timeout: 5000 }, async () => {
  const failures = []; let child, owner;
  const core = createCoreLifecycle({
    launch: () => (child = spawn(path.join(os.tmpdir(), `salcara-missing-${randomUUID()}`, 'missing-core.exe'), [], { windowsHide: true, stdio: 'ignore' })),
    healthy: async () => false, isStopping: () => false,
    onOwner(value) { owner = value; }, onReady() { throw new Error('Missing process cannot be ready'); },
    onFailure(error) { failures.push(error); },
  });
  assert.equal(await core.start(), false);
  assert.equal(child.pid, undefined); assert.equal(owner, null);
  assert.equal(failures.length, 1); assert.equal(failures[0].code, 'ENOENT');
  core.pause();
});
