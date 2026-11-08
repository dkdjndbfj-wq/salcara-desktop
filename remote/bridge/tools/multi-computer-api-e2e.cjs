'use strict';
// Two isolated production Bridges, one real loopback Hub, synthetic API metadata.
// Build Bridge with `go build -o <fixture-bridge.exe> .` and the sibling Hub
// with `go build -o <fixture-hub.exe> ./tools/device-smoke`. Supply a local
// zxing-wasm package directory as the third argument to decode the actual PNG.
// This test creates independent profiles under the OS temp directory, prints
// no credentials and makes no paid model requests. It leaves fixtures for audit.
const fs = require('node:fs');
const path = require('node:path');
const http = require('node:http');
const net = require('node:net');
const crypto = require('node:crypto');
const assert = require('node:assert/strict');
const { spawn } = require('node:child_process');
const temp = require('node:os').tmpdir();
const [bridgeExe, hubExe, zxingRoot] = process.argv.slice(2);
for (const value of [bridgeExe, hubExe, zxingRoot]) assert(value && path.isAbsolute(value) && fs.existsSync(value), 'Usage: node multi-computer-api-e2e.cjs <fixture-bridge.exe> <device-smoke-hub.exe> <zxing-wasm-package-directory>');
const bridgeRoot = path.resolve(__dirname, '..');
const processes = [];
const fixture = fs.mkdtempSync(path.join(temp, 'multi-computer-'));
const delay = ms => new Promise(resolve => setTimeout(resolve, ms));
let proxy;
let phase = 'prepare';
function start(exe, args, env) {
  const child = spawn(exe, args, { windowsHide: true, env: env || process.env, stdio: ['ignore', 'pipe', 'pipe'] });
  child.diagnostics = '';
  child.stdout.on('data', bytes => { child.diagnostics = (child.diagnostics + bytes).slice(-4000); });
  child.stderr.on('data', bytes => { child.diagnostics = (child.diagnostics + bytes).slice(-4000); });
  processes.push(child); return child;
}
async function port() {
  return new Promise((resolve, reject) => { const server = net.createServer(); server.once('error', reject); server.listen(0, '127.0.0.1', () => { const value = server.address().port; server.close(() => resolve(value)); }); });
}
async function request(base, route, body, headers = {}) {
  assert.equal(new URL(base).hostname, '127.0.0.1');
  const response = await fetch(base + route, { method: body === undefined ? 'GET' : 'POST', headers: { ...(body === undefined ? {} : { 'Content-Type': 'application/json' }), ...headers }, body: body === undefined ? undefined : JSON.stringify(body), signal: AbortSignal.timeout(20000), redirect: 'error' });
  const bytes = Buffer.from(await response.arrayBuffer());
  let json; try { json = JSON.parse(bytes.toString()); } catch {}
  return { status: response.status, headers: response.headers, bytes, json };
}
async function poll(label, fn) {
  for (let i = 0; i < 100; i++) { try { if (await fn()) return; } catch {} await delay(200); }
  throw new Error('Timeout: ' + label);
}
const passed = text => console.log('PASS ' + text);
async function main() {
  const hubPort = await port(), frontPort = await port();
  const site = `http://127.0.0.1:${frontPort}`;
  const hubData = path.join(fixture, 'hub'); fs.mkdirSync(hubData);
  const claims = [crypto.randomBytes(32).toString('hex'), crypto.randomBytes(32).toString('hex')];
  fs.writeFileSync(path.join(hubData, 'device-enrollment-claims.json'), JSON.stringify({ version: 1, claims: claims.map(token => ({ hash: crypto.createHash('sha256').update(Buffer.from(token, 'hex')).digest('hex'), expiresAt: Date.now() + 3600000 })) }));
  const hub = start(hubExe, ['-listen', `127.0.0.1:${hubPort}`, '-data-dir', hubData, '-public-url', site + '/salcara-hub']);
  proxy = http.createServer((req, res) => {
    if (req.url.startsWith('/salcara-hub')) {
      const upstream = http.request({ hostname: '127.0.0.1', port: hubPort, path: req.url, method: req.method, headers: req.headers }, reply => { res.writeHead(reply.statusCode, reply.headers); reply.pipe(res); });
      upstream.on('error', () => { if (!res.headersSent) res.writeHead(502); res.end(); }); req.pipe(upstream); res.on('close', () => upstream.destroy());
    } else if (req.url === '/v1/models') {
      res.writeHead(200, { 'Content-Type': 'application/json' }); res.end(JSON.stringify({ data: [{ id: 'fixture-model' }] }));
    } else { res.writeHead(404); res.end(); }
  });
  await new Promise(resolve => proxy.listen(frontPort, '127.0.0.1', resolve));
  await poll('Hub', async () => (await request(site, '/salcara-hub/v1/ping')).status === 200);
  const phoneId = crypto.randomBytes(32).toString('hex');
  const zxing = zxingRoot;
  const decoder = require(path.join(zxing, 'dist/cjs/reader/index.js'));
  await decoder.prepareZXingModule({ overrides: { wasmBinary: fs.readFileSync(path.join(zxing, 'dist/reader/zxing_reader.wasm')) }, fireImmediately: true });
  const computers = [];
  for (const [index, name] of ['A', 'B'].entries()) {
    phase = 'start ' + name;
    const root = path.join(fixture, name); fs.mkdirSync(root);
    for (const dir of ['config', 'codex', 'claude', 'codex-app', 'claude-app', 'profile', 'appdata', 'localappdata', 'project']) fs.mkdirSync(path.join(root, dir));
    const consolePort = await port(), base = `http://127.0.0.1:${consolePort}`;
    const configPath = path.join(root, 'config/config.json');
    const accounts = ['Primary', 'Backup'].map(label => ({ id: name + '-' + label, name: name + ' ' + label, kind: 'api', sourceType: 'api_key', baseUrl: site, key: 'synthetic-' + name + '-' + label, model: 'fixture-model', models: ['fixture-model'], protocol: 'responses', authMode: 'bearer' }));
    fs.writeFileSync(configPath, JSON.stringify({ autostart: false, autostartDecided: true, openConsoleOnStart: false, deviceName: 'Fixture computer ' + name, accountKey: '', localAccountsReady: true, localAccounts: accounts, projects: [{ name: name + ' project', path: path.join(root, 'project') }] }));
    const env = { ...process.env, SALCARA_BRIDGE_PORT: String(consolePort), SALCARA_BRIDGE_CONFIG_DIR: path.join(root, 'config'), SALCARA_EMBEDDED_WINDOW: '1', SALCARA_NO_SHELL_PATH: '1', CODEX_HOME: path.join(root, 'codex'), CLAUDE_CONFIG_DIR: path.join(root, 'claude'), CODEX_ELECTRON_USER_DATA_PATH: path.join(root, 'codex-app'), CLAUDE_USER_DATA_DIR: path.join(root, 'claude-app'), USERPROFILE: path.join(root, 'profile'), HOME: path.join(root, 'profile'), APPDATA: path.join(root, 'appdata'), LOCALAPPDATA: path.join(root, 'localappdata'), OPENAI_API_KEY: '', OPENAI_BASE_URL: '', SUB2API_API_KEY: '', ANTHROPIC_API_KEY: '', ANTHROPIC_AUTH_TOKEN: '', ANTHROPIC_BASE_URL: '' };
    let child = start(bridgeExe, ['--background'], env);
    await poll('Bridge ' + name, async () => (await request(base, '/healthz')).status === 200);
    const page = await request(base, '/'); let cookie = page.headers.get('set-cookie').split(';')[0];
    const local = (route, body) => request(base, route, body, { Cookie: cookie, Origin: base });
    assert.equal(path.resolve((await local('/api/state')).json.configDir), path.resolve(root, 'config'));
    const connected = await local('/api/remote/discover', { url: site, connect: true, enrollmentClaim: claims[index] });
    assert.equal(connected.status, 200, JSON.stringify(connected.json));
    await poll('Bridge registered ' + name, async () => (await local('/api/state')).json.hub.state === 'connected');
    const qrStarted = await local('/api/pair/start', {}); assert.equal(qrStarted.status, 200, JSON.stringify(qrStarted.json));
    const png = await local(qrStarted.json.qrUrl); assert.equal(png.status, 200);
    const decoded = await decoder.readBarcodes(new Uint8Array(png.bytes), { formats: ['QRCode'] }); assert.equal(decoded.length, 1);
    const qr = JSON.parse(decoded[0].text);
    const paired = await request(site, '/salcara-hub/v1/app/pair/qr', { deviceId: qr.deviceId, ticket: qr.ticket, phoneId });
    assert.equal(paired.status, 200, JSON.stringify(paired.json));
    const token = paired.json.pair_token;
    const phone = (route, body) => request(site, '/salcara-hub/v1' + route, body, { 'X-Salcara-Pair-Token': token });
    const command = async command => { const response = await phone('/app/commands', { deviceId: qr.deviceId, command }); assert.equal(response.status, 200, command.type + ': ' + JSON.stringify(response.json)); assert.equal(response.json.ok, true, JSON.stringify(response.json)); return response.json.result; };
    await poll('phone identity ' + name, async () => { const result = await command({ type: 'agents.status' }); return result.apis?.length === 2; });
    computers.push({ name, root, base, configPath, env, child, token, deviceId: qr.deviceId, computerId: qr.computerId, phone, command, local, setCookie: value => { cookie = value; } });
  }
  passed('Two independent Bridge processes paired from real QR PNG with the same phone identity');
  const [a, b] = computers;
  assert.notEqual(a.deviceId, b.deviceId); assert.notEqual(a.computerId, b.computerId);
  for (const pc of computers) {
    const listed = await pc.phone('/app/devices'); assert.equal(listed.status, 200); assert.equal(listed.json.devices.length, 1); assert.equal(listed.json.devices[0].deviceId, pc.deviceId);
    const projects = await pc.command({ type: 'projects.list' }); assert.equal(projects.projects.length, 1); assert.equal(projects.projects[0].path, path.join(pc.root, 'project'));
    const status = await pc.command({ type: 'agents.status' }); assert.deepEqual(status.apis.map(item => item.name).sort(), [pc.name + ' Backup', pc.name + ' Primary']);
    const raw = JSON.stringify(status); assert(!raw.includes('synthetic-'));
  }
  passed('Phone credentials isolate device lists, projects and API handles; no model secret is returned');
  phase = 'API switches';
  const bBefore = await b.command({ type: 'agents.status' });
  const aStatus = await a.command({ type: 'agents.status' });
  const selected = aStatus.apis.find(item => item.name === 'A Backup');
  const result = await a.command({ type: 'agents.api.set', agent: 'codex', accountId: selected.id });
  const fresh = await a.command({ type: 'agents.status' });
  const codex = fresh.agents.find(item => item.id === 'codex');
  assert.equal(codex.api.accountId, selected.id); assert.equal(codex.api.source, 'phone'); assert.equal(codex.api.pending, false);
  assert.deepEqual((await b.command({ type: 'agents.status' })).agents.map(item => item.api), bBefore.agents.map(item => item.api));
  const rejected = await b.phone('/app/commands', { deviceId: b.deviceId, command: { type: 'agents.api.set', agent: 'codex', accountId: selected.id } });
  assert(rejected.status !== 200 || rejected.json.ok !== true);
  passed('Real agents.api.set changes only computer A; A API handle cannot be applied on B');
  phase = 'reconnect';
  a.child.kill(); await new Promise(resolve => a.child.once('exit', resolve));
  await poll('offline', async () => !(await a.phone('/app/devices')).json.devices[0].online);
  a.child = start(bridgeExe, ['--background'], a.env);
  await poll('reconnected', async () => (await a.phone('/app/devices')).json.devices[0].online);
  const after = await a.command({ type: 'agents.status' });
  assert.equal(after.agents.find(item => item.id === 'codex').api.accountId, selected.id);
  assert.equal((await b.phone('/app/devices')).json.devices[0].online, true);
  passed('Restarted A reconnects using the existing phone token and selected API; B stays online');
  const rejectedTarget = await a.phone('/app/commands', { deviceId: b.deviceId, command: { type: 'projects.list' } });
  assert(rejectedTarget.status !== 200 || rejectedTarget.json.ok !== true);
  passed('Cross-computer command rejected after reconnect');
  console.log(JSON.stringify({ ok: true, fixture, realBridges: 2, realHub: true, physicalComputers: 1, paidModelRequests: 0 }));
}
main().catch(error => { console.error('FAIL phase=' + phase, error.message); process.exitCode = 1; }).finally(async () => {
  for (const child of processes) if (child.exitCode === null && child.signalCode === null) child.kill();
  if (proxy) { proxy.closeAllConnections(); proxy.close(); }
});

