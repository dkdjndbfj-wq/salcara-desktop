// Local cross-process pairing acceptance. This creates a unique fixture only,
// never invokes a model, never logs credentials and exits only its own Bridge.
'use strict';
const fs = require('node:fs');
const path = require('node:path');
const net = require('node:net');
const { spawn } = require('node:child_process');
const assert = require('node:assert/strict');

const root = path.resolve(__dirname, '..');
const bridgeExe = path.join(root, 'desktop', 'bin', 'SalcaraBridge-device-test.exe');
const hubExe = path.join(root, 'output', 'device-pair-e2e-hub.exe');
const port = Number(process.env.SALCARA_PAIR_E2E_PORT || 47836);
const secondPort = Number(process.env.SALCARA_PAIR_E2E_HUB_PORT || 47838);
const consoleURL = `http://127.0.0.1:${port}`;
const siteA = 'http://127.0.0.1:47837';
const siteB = `http://127.0.0.1:${secondPort}`;
let phase = 'prepare';
let cookie = '';
let bridge;
let ownHub;
let fixture;
let ownsConsole = false;
const report = { qrDecodedFromActualPNG: false, paidModelRequests: 0, steps: [] };

function done(name) { report.steps.push(name); console.log(`PASS ${name}`); }
const delay = (ms) => new Promise(resolve => setTimeout(resolve, ms));
async function freePort(p) {
  await new Promise((resolve, reject) => {
    const server = net.createServer();
    server.once('error', reject);
    server.listen(p, '127.0.0.1', () => server.close(resolve));
  });
}
async function request(base, route, body, headers = {}) {
  const response = await fetch(base + route, {
    method: body === undefined ? 'GET' : 'POST',
    headers: { ...(body === undefined ? {} : { 'Content-Type': 'application/json' }), ...headers },
    body: body === undefined ? undefined : JSON.stringify(body),
    signal: AbortSignal.timeout(15000), redirect: 'error',
  });
  const bytes = Buffer.from(await response.arrayBuffer());
  let json;
  if (response.headers.get('content-type')?.includes('application/json')) json = JSON.parse(bytes.toString());
  return { status: response.status, headers: response.headers, bytes, json };
}
const local = (route, body) => request(consoleURL, route, body, { Cookie: cookie, Origin: consoleURL });
const phone = (site, route, token, body) => request(site, `/salcara-hub/v1${route}`, body, token ? { 'X-Salcara-Pair-Token': token } : {});
async function poll(name, fn, limit = 15000) {
  const end = Date.now() + limit;
  while (Date.now() < end) { try { if (await fn()) return; } catch {} await delay(200); }
  throw new Error(`timed out at ${name}`);
}
function fixtureConfig() { return JSON.parse(fs.readFileSync(path.join(fixture, 'config.json'), 'utf8')); }
async function connect(site) {
  const response = await local('/api/remote/discover', { url: site, connect: true });
  assert.equal(response.status, 200);
  assert.equal(response.json.plugin.protocolVersion, 1);
  await poll('Bridge registered/SSE connected', async () => (await local('/api/state')).json.hub.state === 'connected');
}
async function pairingPNG() {
  const started = await local('/api/pair/start', {});
  assert.equal(started.status, 200);
  assert.ok(started.json.qrUrl.startsWith('/api/pair/qr'));
  assert.equal(started.json.ticket, undefined, 'ticket not exposed in local API JSON');
  const protectedPNG = await request(consoleURL, started.json.qrUrl);
  assert.equal(protectedPNG.status, 401);
  const qr = await local(started.json.qrUrl);
  assert.equal(qr.status, 200);
  assert.equal(qr.headers.get('content-type'), 'image/png');
  assert.equal(qr.bytes.subarray(0, 8).toString('hex'), '89504e470d0a1a0a');
  const zxingRoot = path.resolve(root, '..', '..', 'node_modules', 'zxing-wasm');
  const decoder = require(path.join(zxingRoot, 'dist', 'cjs', 'reader', 'index.js'));
  await decoder.prepareZXingModule({ overrides: { wasmBinary: fs.readFileSync(path.join(zxingRoot, 'dist', 'reader', 'zxing_reader.wasm')) }, fireImmediately: true });
  const decoded = await decoder.readBarcodes(new Uint8Array(qr.bytes), { formats: ['QRCode'] });
  assert.equal(decoded.length, 1);
  const payload = JSON.parse(decoded[0].text);
  assert.equal(payload.type, 'salcara-remote-pair');
  assert.equal(payload.version, 1);
  assert.equal(payload.ticket.length, 64);
  assert.ok(payload.expiresAt > Date.now());
  report.qrDecodedFromActualPNG = true;
  return payload;
}
async function claim(site, qr) {
  const response = await phone(site, '/app/pair/qr', '', { deviceId: qr.deviceId, ticket: qr.ticket });
  assert.equal(response.status, 200);
  assert.equal(response.json.pair_token.length, 64);
  return response.json.pair_token;
}

(async () => {
  try {
    await freePort(port); await freePort(secondPort);
    assert.ok(fs.existsSync(bridgeExe)); assert.ok(fs.existsSync(hubExe));
    fs.mkdirSync(path.join(root, 'output', 'device-pair-e2e'), { recursive: true });
    fixture = fs.mkdtempSync(path.join(root, 'output', 'device-pair-e2e', 'run-'));
    for (const name of ['codex-home', 'claude-home', 'codex-app', 'claude-app', 'project']) fs.mkdirSync(path.join(fixture, name));
    fs.writeFileSync(path.join(fixture, 'config.json'), JSON.stringify({
      autostart: false, autostartDecided: true, openConsoleOnStart: false,
      accountKey: '', codexKey: '', claudeKey: '', projects: [{ path: path.join(fixture, 'project'), name: 'Device pairing fixture project' }],
    }, null, 2));
    ownHub = spawn(hubExe, ['-listen', `127.0.0.1:${secondPort}`], { windowsHide: true, stdio: 'ignore' });
    ownHub.once('error', () => {});
    bridge = spawn(bridgeExe, [], { windowsHide: true, stdio: 'ignore', env: {
      ...process.env, SALCARA_BRIDGE_PORT: String(port), SALCARA_BRIDGE_CONFIG_DIR: fixture, SALCARA_EMBEDDED_WINDOW: '1',
      CODEX_HOME: path.join(fixture, 'codex-home'), CLAUDE_CONFIG_DIR: path.join(fixture, 'claude-home'),
      CODEX_ELECTRON_USER_DATA_PATH: path.join(fixture, 'codex-app'), CLAUDE_USER_DATA_DIR: path.join(fixture, 'claude-app'),
    } });
    bridge.once('error', () => {});
    console.log(`FIXTURE ${fixture}`);
    console.log(`BRIDGE ${consoleURL}/ PID ${bridge.pid}`);
    await poll('console ready', async () => (await request(consoleURL, '/healthz')).status === 200);
    await poll('second independent Hub ready', async () => (await request(siteB, '/salcara-hub/v1/ping')).status === 200);
    const page = await request(consoleURL, '/');
    cookie = page.headers.get('set-cookie').split(';')[0];
    const ownState = await local('/api/state');
    assert.equal(bridge.exitCode, null);
    assert.equal(ownHub.exitCode, null);
    assert.equal(path.resolve(ownState.json.configDir), path.resolve(fixture));
    ownsConsole = true; // Never mutate or quit another server after a port race.

    phase = 'discover and connect';
    const initial = fixtureConfig();
    assert.equal(initial.accountKey, ''); assert.equal(initial.codexKey || '', ''); assert.equal(initial.claudeKey || '', '');
    await connect(siteA);
    const identityA = fixtureConfig();
    assert.equal(identityA.remoteDeviceOnly, true);
    assert.equal(identityA.accountKey, '');
    done('No station account/model Key: discovery + real Bridge register/SSE online');

    phase = 'QR image and scan';
    const qrA = await pairingPNG();
    assert.equal(qrA.hubUrl, siteA + '/salcara-hub/v1');
    assert.equal(qrA.deviceId, identityA.deviceId);
    const tokenA = await claim(siteA, qrA);
    done('Cookie-protected real PNG decoded using installed local ZXing; QR claim successful');
    const devicesA = await phone(siteA, '/app/devices', tokenA);
    assert.equal(devicesA.status, 200);
    assert.equal(devicesA.json.devices.length, 1);
    assert.equal(devicesA.json.devices[0].deviceId, identityA.deviceId);
    assert.equal(devicesA.json.devices[0].online, true);
    const projects = await phone(siteA, '/app/commands', tokenA, { deviceId: identityA.deviceId, command: { type: 'projects.list' } });
    assert.equal(projects.status, 200); assert.equal(projects.json.ok, true);
    assert.equal(projects.json.result.projects[0].path, path.join(fixture, 'project'));
    done('Phone token-only device list + projects.list reached actual Bridge and returned fixture result');
    assert.equal((await phone(siteA, '/app/pair/qr', '', { deviceId: qrA.deviceId, ticket: qrA.ticket })).status, 403);
    assert.equal((await phone(siteA, '/bridge/register', tokenA, { deviceId: identityA.deviceId })).status, 403);
    done('QR replay denied; phone token cannot authenticate a bridge');

    phase = 'independent sites';
    await connect(siteB);
    const identityB = fixtureConfig();
    assert.notEqual(identityB.deviceId, identityA.deviceId); assert.notEqual(identityB.deviceSecret, identityA.deviceSecret);
    assert.equal((await local('/api/pair/qr')).status, 410);
    assert.equal((await phone(siteB, '/app/devices', tokenA)).status, 403);
    const qrB = await pairingPNG();
    const tokenB = await claim(siteB, qrB);
    assert.equal((await phone(siteA, '/app/devices', tokenB)).status, 403);
    await connect(siteA);
    const backA = fixtureConfig();
    assert.equal(backA.deviceId, identityA.deviceId); assert.equal(backA.deviceSecret, identityA.deviceSecret);
    assert.equal(backA.remoteConnections.length, 2);
    assert.equal((await phone(siteA, '/app/devices', tokenA)).status, 200);
    assert.equal(backA.accountKey, ''); assert.equal(backA.codexKey || '', ''); assert.equal(backA.claudeKey || '', '');
    done('Second independent Hub requires separate identity/pairing; tokens cross-site denied; returning restores only site A identity');

    phase = 'revoke';
    const freshQR = await pairingPNG();
    assert.equal((await local('/api/pair/revoke', {})).status, 200);
    assert.equal((await phone(siteA, '/app/devices', tokenA)).status, 403);
    assert.equal((await phone(siteA, '/app/pair/qr', '', { deviceId: freshQR.deviceId, ticket: freshQR.ticket })).status, 403);
    assert.equal((await local('/api/pair/qr')).status, 410);
    assert.equal((await phone(siteB, '/app/devices', tokenB)).status, 200);
    done('Desktop revoke blocks prior phone token/pending QR, clears local PNG, leaves other station untouched');
    assert.equal((await phone(siteB, '/app/pair/revoke', tokenB, {})).status, 200);

    report.ok = true;
    report.fixture = fixture;
    report.bridgeURL = consoleURL;
    report.stationCredentialsIndependent = true;
    report.noRealPhoneCameraUsed = true;
    fs.writeFileSync(path.join(fixture, 'acceptance-report.json'), JSON.stringify(report, null, 2));
    console.log('RESULT complete; sanitized report saved; no real phone camera, no model inference');
    if (process.env.SALCARA_PAIR_E2E_HOLD_FOR_UI === '1') {
      console.log('INSPECTION waiting for explicit stdin "UI done" before quitting own fixture');
      await new Promise(resolve => {
        process.stdin.resume();
        const allowQuit = data => {
          if (!data.toString().includes('UI done')) return;
          process.stdin.removeListener('data', allowQuit);
          process.stdin.pause();
          resolve();
        };
        process.stdin.on('data', allowQuit);
      });
    } else if (process.env.SALCARA_PAIR_E2E_INSPECT_MS) {
      await delay(Math.min(Number(process.env.SALCARA_PAIR_E2E_INSPECT_MS), 180000));
    }
  } catch (_) {
    console.error(`FAILED phase=${phase}; details withheld to protect fixture credentials`);
    process.exitCode = 1;
  } finally {
    if (bridge) {
      if (ownsConsole) { try { await local('/api/quit', {}); } catch {} }
      await poll('own Bridge exit', async () => bridge.exitCode !== null || bridge.signalCode !== null, 7000).catch(() => {});
      if (bridge.exitCode === null && bridge.signalCode === null) {
        console.error('Own fixture did not exit via local API; left running for inspection (no other processes touched)');
        process.exitCode = 1;
      } else console.log('CLEANUP own fixture Bridge exited via local API');
    }
    if (ownHub && ownHub.exitCode === null) ownHub.kill(); // Exact child fixture only; no persistence.
  }
})();
