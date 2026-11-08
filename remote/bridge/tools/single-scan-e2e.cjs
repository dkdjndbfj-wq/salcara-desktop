// Windows: go build -o output/single-scan-bridge.exe .
// Hub: go build -o ../salcara-desktop/remote/bridge/output/single-scan-hub.exe ./tools/device-smoke
// npm install --prefix output/single-scan-e2e-tools --no-save --package-lock=false zxing-wasm
// node tools/single-scan-e2e.cjs
// Real local Bridge + two independent Hub processes. Isolated fixture only;
// never reads a real API key, launches a model, or modifies a user's profile.
'use strict';
const fs = require('node:fs');
const path = require('node:path');
const net = require('node:net');
const { randomBytes, createHash } = require('node:crypto');
const { spawn } = require('node:child_process');
const assert = require('node:assert/strict');
const root = path.resolve(__dirname, '..');
const bridgeExe = process.env.SALCARA_SCAN_BRIDGE_EXE || path.join(root, 'output/single-scan-bridge.exe');
const hubExe = process.env.SALCARA_SCAN_HUB_EXE || path.join(root, 'output/single-scan-hub.exe');
const report = { ok: false, qrScans: 0, modelRequests: 0, steps: [] };
const children = new Set();
let fixture, phase = 'prepare', bridge, cookie, consoleURL, bridgeDir;
const wait = ms => new Promise(resolve => setTimeout(resolve, ms));
const pass = name => { report.steps.push(name); console.log(`PASS ${name}`); };
async function port() {
  return new Promise((resolve, reject) => { const s = net.createServer(); s.once('error', reject); s.listen(0, '127.0.0.1', () => { const p = s.address().port; s.close(() => resolve(p)); }); });
}
async function request(base, route, body, headers = {}) {
  const response = await fetch(base + route, { method: body === undefined ? 'GET' : 'POST', headers: { ...(body === undefined ? {} : { 'Content-Type': 'application/json' }), ...headers }, body: body === undefined ? undefined : JSON.stringify(body), signal: AbortSignal.timeout(22000), redirect: 'error' });
  const bytes = Buffer.from(await response.arrayBuffer());
  const json = response.headers.get('content-type')?.includes('application/json') ? JSON.parse(bytes.toString()) : undefined;
  return { status: response.status, headers: response.headers, bytes, json };
}
const local = (route, body) => request(consoleURL, route, body, { Cookie: cookie || '', Origin: consoleURL });
const phone = (site, route, token, body) => request(site.url, `/salcara-hub/v1${route}`, body, token ? { 'X-Salcara-Pair-Token': token } : {});
async function poll(name, fn, limit = 25000) {
  const end = Date.now() + limit;
  while (Date.now() < end) { if (await fn().catch(() => false)) return; await wait(250); }
  throw new Error(`timeout: ${name}`);
}
function launch(exe, args, env) {
  const child = spawn(exe, args, { windowsHide: true, stdio: 'ignore', env: env || process.env });
  children.add(child); child.once('error', () => {}); child.once('exit', () => children.delete(child));
  return child;
}
async function stop(child) {
  if (!child || child.exitCode !== null || child.signalCode !== null) return;
  child.kill();
  await poll('owned child exit', async () => child.exitCode !== null || child.signalCode !== null, 5000);
}
async function startHub(site) {
  site.child = launch(hubExe, ['-listen', `127.0.0.1:${site.port}`, '-data-dir', site.dir]);
  await poll('Hub ready', async () => (await phone(site, '/ping')).status === 200);
}
async function createHub(name) {
  const p = await port(), dir = path.join(fixture, name), claim = randomBytes(32).toString('hex');
  fs.mkdirSync(dir);
  fs.writeFileSync(path.join(dir, 'device-enrollment-claims.json'), JSON.stringify({ version: 1, claims: [{ hash: createHash('sha256').update(Buffer.from(claim, 'hex')).digest('hex'), expiresAt: Date.now()+3600000 }] }));
  const site = { port: p, url: `http://127.0.0.1:${p}`, dir, claim };
  await startHub(site); return site;
}
function config() { return JSON.parse(fs.readFileSync(path.join(bridgeDir, 'config.json'), 'utf8')); }
async function startBridge() {
  bridge = launch(bridgeExe, ['-background'], { ...process.env, SALCARA_BRIDGE_PORT: new URL(consoleURL).port, SALCARA_BRIDGE_CONFIG_DIR: bridgeDir, SALCARA_EMBEDDED_WINDOW: '1', CODEX_HOME: path.join(fixture, 'codex-home'), CLAUDE_CONFIG_DIR: path.join(fixture, 'claude-home'), CODEX_ELECTRON_USER_DATA_PATH: path.join(fixture, 'codex-app'), CLAUDE_USER_DATA_DIR: path.join(fixture, 'claude-app') });
  await poll('Bridge ready', async () => (await request(consoleURL, '/healthz')).status === 200);
  cookie = (await request(consoleURL, '/')).headers.get('set-cookie').split(';')[0];
  assert.equal(path.resolve((await local('/api/state')).json.configDir), path.resolve(bridgeDir));
}
async function connect(site) {
  const result = await local('/api/remote/discover', { url: site.url, connect: true, enrollmentClaim: site.claim });
  assert.equal(result.status, 200, 'discover/connect failed');
  await poll('Bridge online at selected Hub', async () => { const state = (await local('/api/state')).json; return state.hub.state === 'connected' && state.hub.hubUrl === `${site.url}/salcara-hub`; });
}
async function scanOnce(site) {
  const start = await local('/api/pair/start', {});
  assert.equal(start.status, 200, 'local QR failed');
  const png = await local(start.json.qrUrl);
  assert.equal(png.status, 200);
  const packageRoot = path.join(root, 'output/single-scan-e2e-tools/node_modules/zxing-wasm');
  const decoder = require(path.join(packageRoot, 'dist/cjs/reader/index.js'));
  await decoder.prepareZXingModule({ overrides: { wasmBinary: fs.readFileSync(path.join(packageRoot, 'dist/reader/zxing_reader.wasm')) }, fireImmediately: true });
  const decoded = await decoder.readBarcodes(new Uint8Array(png.bytes), { formats: ['QRCode'] });
  assert.equal(decoded.length, 1, 'real QR PNG not decoded');
  const qr = JSON.parse(decoded[0].text);
  const phoneId = randomBytes(32).toString('hex');
  const result = await phone(site, '/app/pair/qr', '', { deviceId: qr.deviceId, ticket: qr.ticket, phoneId });
  assert.equal(result.status, 200);
  assert.equal(result.json.computerId, qr.computerId);
  report.qrScans++;
  await poll('physical phone binding accepted by Bridge', async () => (await local('/api/state')).json.hub.pairing?.paired === true);
  return { token: result.json.pair_token, deviceId: qr.deviceId, computerId: qr.computerId };
}
async function sync(site, pair, command = { type: 'remote.connections.sync' }) {
  const result = await phone(site, '/app/commands', pair.token, { deviceId: pair.deviceId, command });
  assert.equal(result.status, 200, 'sync transport failed');
  return result.json;
}
(async () => {
  try {
    assert.ok(fs.existsSync(bridgeExe) && fs.existsSync(hubExe), 'build fixture binaries first');
    fs.mkdirSync(path.join(root, 'output/single-scan-e2e'), { recursive: true });
    fixture = fs.mkdtempSync(path.join(root, 'output/single-scan-e2e/run-'));
    bridgeDir = path.join(fixture, 'bridge');
    for (const name of ['bridge', 'codex-home', 'claude-home', 'codex-app', 'claude-app', 'project']) fs.mkdirSync(path.join(fixture, name));
    fs.writeFileSync(path.join(bridgeDir, 'config.json'), JSON.stringify({ autostart: false, autostartDecided: true, openConsoleOnStart: false, accountKey: '', codexKey: '', claudeKey: '', projects: [{ path: path.join(fixture, 'project'), name: 'Single scan fixture' }] }));
    consoleURL = `http://127.0.0.1:${await port()}`;
    const a = await createHub('hub-a'), b = await createHub('hub-b');
    await startBridge();
    phase = 'save two relay identities';
    await connect(b); const identityB = config();
    await connect(a); const identityA = config();
    assert.notEqual(identityA.deviceSecret, identityB.deviceSecret);
    phase = 'single QR';
    const pair = await scanOnce(a);
    pass('One real PNG scan establishes physical computer/phone binding');
    phase = 'offline relay';
    await stop(b.child);
    const partial = await sync(a, pair);
    assert.equal(partial.ok, true);
    assert.equal(partial.result.computerId, pair.computerId);
    assert.equal(partial.result.connections.length, 1);
    assert.ok(partial.result.connections[0].error);
    assert.equal(partial.result.connections[0].token, undefined);
    assert.equal((await phone(a, '/app/devices', pair.token)).status, 200);
    pass('Offline secondary relay reports retryable failure while initial binding stays usable');
    phase = 'retry without QR';
    await startHub(b);
    const complete = await sync(a, pair);
    assert.equal(complete.ok, true);
    const imported = complete.result.connections[0];
    assert.equal(imported.deviceId, identityB.deviceId);
    assert.equal(imported.hubUrl, b.url+'/salcara-hub/v1');
    assert.match(imported.token, /^[a-f0-9]{64}$/);
    assert.equal((await phone(b, '/app/devices', imported.token)).status, 200);
    assert.equal((await phone(a, '/app/devices', imported.token)).status, 403);
    assert.equal((await phone(b, '/app/devices', pair.token)).status, 403);
    pass('Retry imports secondary relay credential; tokens remain isolated between relays');
    phase = 'same-generation token preservation';
    await connect(b);
    await poll('secondary relay accepts physical phone', async () => (await local('/api/state')).json.hub.pairing?.paired === true);
    const fromB = await sync(b, { token: imported.token, deviceId: imported.deviceId });
    assert.equal(fromB.ok, true);
    assert.equal(fromB.result.connections[0].token, pair.token);
    assert.equal((await phone(a, '/app/devices', pair.token)).status, 200);
    await connect(a);
    await poll('primary relay restored', async () => (await local('/api/state')).json.hub.pairing?.paired === true);
    pass('Switching relays and syncing back preserves the original QR token');
    phase = 'injection and identity checks';
    const injected = await sync(a, pair, { type: 'remote.connections.sync', hubUrl: 'https://injected.invalid' });
    assert.equal(injected.ok, false);
    const wrong = await request(b.url, '/salcara-hub/v1/bridge/pair/sync', { deviceId: identityB.deviceId, computerId: pair.computerId, bindingId: config().phoneBindingId, phoneHash: config().phoneHash, pairHash: 'a'.repeat(64) }, { 'X-Salcara-Device-Id': identityB.deviceId, 'X-Salcara-Device-Secret': identityA.deviceSecret });
    assert.equal(wrong.status, 403);
    pass('Arbitrary relay injection and wrong-computer credentials are rejected');
    phase = 'restart and reconnect';
    assert.equal((await local('/api/quit', {})).status, 200);
    await poll('Bridge shutdown', async () => bridge.exitCode !== null || bridge.signalCode !== null);
    await stop(b.child); await startHub(b); await startBridge();
    await poll('Bridge restores physical phone binding', async () => (await local('/api/state')).json.hub.pairing?.paired === true);
    const repeated = await sync(a, pair);
    assert.equal(repeated.ok, true);
    assert.equal(repeated.result.connections[0].token, imported.token);
    assert.equal((await phone(b, '/app/devices', imported.token)).status, 200);
    assert.equal(report.qrScans, 1);
    pass('Bridge and secondary Hub restart retain binding and the same token without another scan');
    phase = 'credential storage boundaries';
    const inspect = dir => {
      for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
        const file = path.join(dir, entry.name);
        if (entry.isDirectory()) inspect(file);
        else if (/\.(json|jsonl|log)$/.test(entry.name)) {
          const content = fs.readFileSync(file, 'utf8');
          assert.ok(!content.includes(pair.token) && !content.includes(imported.token), 'phone token reached a fixture log/metadata/receipt file');
        }
      }
    };
    for (const dir of [a.dir, b.dir, bridgeDir]) inspect(dir);
    pass('Phone credentials do not appear in Bridge logs or Hub metadata/receipt files');
    phase = 'global revoke';
    assert.equal((await local('/api/pair/revoke', {})).status, 200);
    await poll('all relays revoked', async () => (await phone(a, '/app/devices', pair.token)).status === 403 && (await phone(b, '/app/devices', imported.token)).status === 403);
    pass('Unbinding the computer revokes both saved relay credentials');
    report.ok = true;
  } catch (error) {
    report.failedPhase = phase;
    // Never serialize AssertionError.actual/expected (they may be tokens).
    console.error(`FAIL ${phase}: ${error.message.split('\n')[0]}`);
    process.exitCode = 1;
  } finally {
    for (const child of [...children]) await stop(child).catch(() => { process.exitCode = 1; });
    if (fixture) { fs.writeFileSync(path.join(fixture, 'acceptance-report.json'), JSON.stringify(report, null, 2)); console.log(`REPORT ${path.join(fixture, 'acceptance-report.json')}`); }
  }
})();
