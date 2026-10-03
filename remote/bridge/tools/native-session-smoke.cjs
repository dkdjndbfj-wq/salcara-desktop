// Actual installed Codex producer -> same-home actual Bridge -> paired phone
// request/reply and live Hub SSE. All processes/keys/models are explicit fixtures.
'use strict';
const fs = require('node:fs');
const path = require('node:path');
const net = require('node:net');
const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const readline = require('node:readline');
const { spawn } = require('node:child_process');
const bridgeRoot = path.resolve(__dirname, '..');
const [codexExe, gatewayExe, bridgeExe] = process.argv.slice(2);
for (const exe of [codexExe, gatewayExe, bridgeExe]) assert(exe && path.isAbsolute(exe) && fs.existsSync(exe), 'explicit executable paths required');
assert(path.resolve(bridgeExe).startsWith(path.join(bridgeRoot, 'output') + path.sep), 'only a fixture Bridge binary may be used');
assert(path.resolve(gatewayExe) === path.join(bridgeRoot, 'output', 'protocol-smoke.exe'), 'only the loopback synthetic gateway is allowed');
const port = Number(process.env.SALCARA_NATIVE_SESSION_TEST_PORT || 47842);
assert(Number.isInteger(port) && port >= 47840 && port <= 47900, 'explicit fixture-only port range required');
const site = 'http://127.0.0.1:47837';
const consoleURL = `http://127.0.0.1:${port}`;
const model = 'non-openai-fixture', apiKey = 'synthetic-loopback-credential';
const firstPrompt = 'NATIVE_SESSION_FIRST_MARKER: This is a fixture-only first turn, preserve it in the original thread. Reply only.';
const phonePrompt = 'NATIVE_SESSION_PHONE_MARKER: Continue this SAME original thread through the paired-phone fixture. Any supplied synthetic tool is restricted to this fixture project.';
let phase = 'prepare', fixture, gateway, producer, bridge, cookie = '', phoneToken = '', deviceId = '', ownsConsole = false;
let sseAbort, streamTask, streamError;
const phoneEvents = [], producerNotes = [], report = { paidModelRequests: 0, realPhoneCameraUsed: false, steps: [] };
const delay = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
function passed(label) { report.steps.push(label); console.log(`PASS ${label}`); }
function assertLoopback(value) { const url = new URL(value); assert.equal(url.protocol, 'http:'); assert.equal(url.hostname, '127.0.0.1'); return url; }
async function freePort(value) {
  await new Promise((resolve, reject) => { const server = net.createServer(); server.once('error', reject); server.listen(value, '127.0.0.1', () => server.close(resolve)); });
}
async function poll(label, predicate, limit = 25_000) {
  const deadline = Date.now() + limit;
  while (Date.now() < deadline) { if (await predicate()) return; await delay(200); }
  throw new Error(`timed out: ${label}`);
}
async function request(base, route, body, headers = {}) {
  assertLoopback(base);
  const response = await fetch(base + route, { method: body === undefined ? 'GET' : 'POST', headers: { ...(body === undefined ? {} : { 'Content-Type': 'application/json' }), ...headers }, body: body === undefined ? undefined : JSON.stringify(body), signal: AbortSignal.timeout(55_000), redirect: 'error' });
  const bytes = Buffer.from(await response.arrayBuffer());
  const json = response.headers.get('content-type')?.includes('application/json') ? JSON.parse(bytes.toString()) : null;
  return { status: response.status, headers: response.headers, bytes, json };
}
const local = (route, body) => request(consoleURL, route, body, { Cookie: cookie, Origin: consoleURL });
const phone = (route, body) => request(site, '/salcara-hub/v1' + route, body, phoneToken ? { 'X-Salcara-Pair-Token': phoneToken } : {});
async function phoneCommand(command) {
  const response = await phone('/app/commands', { deviceId, command });
  assert.equal(response.status, 200, `${command.type}: HTTP ${response.status}`);
  assert.equal(response.json.ok, true, `${command.type}: ${response.json.error || 'failed'}`);
  return response.json.result;
}
function spawnFixture(exe, args, env = process.env, cwd = bridgeRoot) {
  const child = spawn(exe, args, { env, cwd, windowsHide: true, stdio: ['pipe', 'pipe', 'pipe'] });
  child.stderr.resume();
  return child;
}
async function endOwnChild(child) {
  if (!child || child.exitCode !== null || child.signalCode !== null) return;
  child.stdin.end();
  await Promise.race([new Promise((resolve) => child.once('exit', resolve)), delay(4000)]);
  if (child.exitCode === null && child.signalCode === null) { child.kill(); await Promise.race([new Promise((resolve) => child.once('exit', resolve)), delay(4000)]); }
}
function producerRPC(child) {
  let nextId = 0;
  const pending = new Map(), completed = new Map(), turns = new Map();
  readline.createInterface({ input: child.stdout }).on('line', (line) => {
    let message; try { message = JSON.parse(line); } catch { return; }
    if (message.id !== undefined && pending.has(message.id)) {
      const task = pending.get(message.id); pending.delete(message.id); clearTimeout(task.timer);
      if (message.error) task.reject(new Error(`${task.method}: ${message.error.message}`)); else task.resolve(message.result);
      return;
    }
    producerNotes.push(message);
    if (message.method === 'turn/completed') {
      const turn = message.params.turn; completed.set(turn.id, turn); turns.get(turn.id)?.(turn); turns.delete(turn.id);
    }
    if (message.id !== undefined && message.method) child.stdin.write(JSON.stringify({ jsonrpc: '2.0', id: message.id, result: { decision: 'decline' } }) + '\n');
  });
  const rpc = (method, params) => new Promise((resolve, reject) => {
    const id = ++nextId, timer = setTimeout(() => { pending.delete(id); reject(new Error(`${method} timed out`)); }, 30_000);
    pending.set(id, { resolve, reject, method, timer });
    child.stdin.write(JSON.stringify({ jsonrpc: '2.0', id, method, params }) + '\n');
  });
  return { rpc, waitTurn: (id) => completed.has(id) ? Promise.resolve(completed.get(id)) : new Promise((resolve) => turns.set(id, resolve)) };
}
async function pairActualQR() {
  const started = await local('/api/pair/start', {});
  assert.equal(started.status, 200); assert(started.json.qrUrl);
  const png = await local(started.json.qrUrl); assert.equal(png.status, 200);
  const zxingRoot = path.resolve(bridgeRoot, '..', '..', 'node_modules', 'zxing-wasm');
  const decoder = require(path.join(zxingRoot, 'dist', 'cjs', 'reader', 'index.js'));
  await decoder.prepareZXingModule({ overrides: { wasmBinary: fs.readFileSync(path.join(zxingRoot, 'dist', 'reader', 'zxing_reader.wasm')) }, fireImmediately: true });
  const decoded = await decoder.readBarcodes(new Uint8Array(png.bytes), { formats: ['QRCode'] });
  assert.equal(decoded.length, 1);
  const qr = JSON.parse(decoded[0].text);
  assert.equal(qr.hubUrl, site + '/salcara-hub/v1'); assert.equal(qr.deviceId, deviceId);
  const claim = await phone('/app/pair/qr', { deviceId, ticket: qr.ticket });
  assert.equal(claim.status, 200); assert(/^[a-f0-9]{64}$/.test(claim.json.pair_token));
  phoneToken = claim.json.pair_token;
}
async function startPhoneStream(sessionKey) {
  sseAbort = new AbortController();
  const response = await fetch(site + '/salcara-hub/v1/app/stream?after=0', { headers: { Accept: 'text/event-stream', 'X-Salcara-Pair-Token': phoneToken }, signal: sseAbort.signal, redirect: 'error' });
  assert.equal(response.status, 200); assert(response.headers.get('content-type')?.includes('text/event-stream'));
  const decoder = new TextDecoder(); let buffer = '';
  streamTask = (async () => {
    try {
      for await (const bytes of response.body) {
        buffer += decoder.decode(bytes, { stream: true }).replace(/\r\n/g, '\n');
        while (buffer.includes('\n\n')) {
          const end = buffer.indexOf('\n\n'), frame = buffer.slice(0, end); buffer = buffer.slice(end + 2);
          const eventName = frame.split('\n').find((line) => line.startsWith('event:'))?.slice(6).trim();
          const data = frame.split('\n').filter((line) => line.startsWith('data:')).map((line) => line.slice(5).trimStart()).join('\n');
          if (!data || eventName === 'device') continue;
          let event; try { event = JSON.parse(data); } catch { continue; }
          if (event.deviceId !== deviceId || event.sessionKey !== sessionKey) continue;
          phoneEvents.push(event);
          if (event.type === 'approval.request') {
            // Authorize only this known synthetic fixture operation, never an
            // arbitrary agent request accidentally routed to this test device.
            const expected = (event.kind === 'file_change' && JSON.stringify(event).includes('fixture-result.txt'))
              || (event.kind === 'command' && JSON.stringify(event).includes('Get-Location'));
            if (!expected) throw new Error('unexpected non-fixture approval request');
            await phoneCommand({ type: 'approval.respond', approvalId: event.approvalId, decision: 'allow' });
          }
        }
      }
    } catch (error) { if (!sseAbort.signal.aborted) streamError = error; }
  })();
}

(async () => {
  const deadline = setTimeout(() => { console.error(`TIMEOUT phase=${phase}`); producer?.kill(); if (ownsConsole) void local('/api/quit', {}); gateway?.kill(); process.exitCode = 1; }, 160_000);
  try {
    await freePort(port);
    assert.equal((await request(site, '/salcara-hub/v1/ping')).json.protocolVersion, 1);
    fixture = fs.mkdtempSync(path.join(bridgeRoot, 'output', 'native-session-smoke-'));
    assert(fixture.startsWith(path.join(bridgeRoot, 'output') + path.sep));
    for (const name of ['codex-home', 'claude-home', 'codex-app', 'claude-app', 'project', 'user-profile', 'appdata', 'localappdata', 'bridge-config']) fs.mkdirSync(path.join(fixture, name));
    const home = path.join(fixture, 'codex-home'), project = path.join(fixture, 'project'), configDir = path.join(fixture, 'bridge-config');
    const env = { ...process.env, CODEX_HOME: home, CLAUDE_CONFIG_DIR: path.join(fixture, 'claude-home'), CODEX_ELECTRON_USER_DATA_PATH: path.join(fixture, 'codex-app'), CLAUDE_USER_DATA_DIR: path.join(fixture, 'claude-app'), USERPROFILE: path.join(fixture, 'user-profile'), HOME: path.join(fixture, 'user-profile'), APPDATA: path.join(fixture, 'appdata'), LOCALAPPDATA: path.join(fixture, 'localappdata'), OPENAI_API_KEY: apiKey, OPENAI_BASE_URL: '', SUB2API_API_KEY: '', ANTHROPIC_API_KEY: '', ANTHROPIC_AUTH_TOKEN: '', ANTHROPIC_BASE_URL: '', SALCARA_BRIDGE_PORT: String(port), SALCARA_BRIDGE_CONFIG_DIR: configDir, SALCARA_EMBEDDED_WINDOW: '1', SALCARA_NO_SHELL_PATH: '1' };
    gateway = spawnFixture(gatewayExe, ['chat']);
    const base = await new Promise((resolve, reject) => { const lines = readline.createInterface({ input: gateway.stdout }); lines.once('line', (line) => { lines.close(); resolve(line); }); gateway.once('error', reject); });
    assertLoopback(base);
    const apiRoot = base + '/gateway/codex';
    fs.writeFileSync(path.join(home, 'config.toml'), `model = "${model}"\nmodel_provider = "openai"\nopenai_base_url = "${apiRoot}/v1"\ncli_auth_credentials_store = "file"\n`);
    fs.writeFileSync(path.join(home, 'auth.json'), JSON.stringify({ auth_mode: 'apikey', OPENAI_API_KEY: apiKey }));
    const account = { id: 'native-fixture-api', name: 'Synthetic loopback API', kind: 'api', baseUrl: apiRoot, key: apiKey, model, models: [model], authMode: 'bearer', protocol: 'responses' };
    const fingerprint = crypto.createHash('sha256').update(['codex', apiRoot, apiKey, model, 'bearer', 'responses'].join('\0')).digest('hex');
    fs.writeFileSync(path.join(configDir, 'config.json'), JSON.stringify({
      autostart: false, autostartDecided: true, openConsoleOnStart: false, deviceName: 'Native original-session fixture ' + path.basename(fixture),
      accountKey: '', codexKey: '', claudeKey: '', localAccountsReady: true, localAccounts: [account], activeCodexAccount: account.id,
      localToolPaths: { codex: codexExe, claude: path.join(fixture, 'no-claude-fixture.exe') }, toolAPISelections: { codex: account.id }, toolModels: { codex: model }, toolProtocols: { codex: 'responses' },
      toolApiApplied: { codex: { accountId: account.id, fingerprint, provider: 'openai', model, protocol: 'responses' } },
      projects: [{ path: project, name: 'Native fixture project' }], approval: 'auto_all',
    }, null, 2));
    console.log(`FIXTURE ${fixture}`);

    phase = 'native producer first turn';
    producer = spawnFixture(codexExe, ['app-server', '--listen', 'stdio://'], env, project);
    const { rpc, waitTurn } = producerRPC(producer);
    await rpc('initialize', { clientInfo: { name: 'salcara_external_native_fixture', version: '1.3.0' }, capabilities: { experimentalApi: true } });
    producer.stdin.write(JSON.stringify({ jsonrpc: '2.0', method: 'initialized' }) + '\n');
    const started = await rpc('thread/start', { model, modelProvider: 'openai', cwd: project, approvalPolicy: 'never', sandbox: 'workspace-write', ephemeral: false });
    const threadId = started.thread.id, sessionKey = 'codex:' + threadId;
    await rpc('thread/name/set', { threadId, name: 'Native original-session phone acceptance fixture' });
    const turn = await rpc('turn/start', { threadId, input: [{ type: 'text', text: firstPrompt }] });
    assert.equal((await waitTurn(turn.turn.id)).status, 'completed');
    const initial = await rpc('thread/read', { threadId, includeTurns: true });
    assert(JSON.stringify(initial).includes('NATIVE_SESSION_FIRST_MARKER'));
    assert(JSON.stringify(initial).includes('Synthetic Grok-compatible reply'));
    assert(!producerNotes.some((item) => item.method === 'error'), 'producer transport retry/error');
    await endOwnChild(producer); assert(producer.exitCode !== null || producer.signalCode !== null);
    passed('Actual Codex app-server persisted completed first turn; producer exited; original thread ID recorded');

    phase = 'actual Bridge original history';
    bridge = spawnFixture(bridgeExe, ['--background'], env, project);
    await poll('Bridge health', async () => { try { return (await request(consoleURL, '/healthz')).status === 200; } catch { return false; } });
    const page = await request(consoleURL, '/'); cookie = page.headers.get('set-cookie').split(';')[0];
    const state = await local('/api/state');
    assert.equal(path.resolve(state.json.configDir), path.resolve(configDir)); assert.equal(bridge.exitCode, null); ownsConsole = true;
    const localList = await local('/api/sessions?tool=codex'); assert.equal(localList.status, 200); assert(localList.json.sessions.some((item) => item.sessionKey === sessionKey));
    const localHistory = await local('/api/session?key=' + encodeURIComponent(sessionKey)); assert.equal(localHistory.status, 200);
    assert(JSON.stringify(localHistory.json.events).includes('NATIVE_SESSION_FIRST_MARKER')); assert(JSON.stringify(localHistory.json.events).includes('Synthetic Grok-compatible reply'));
    passed('Actual Bridge read the independently-created original session and both first-turn messages from same HOME');

    phase = 'Hub pairing';
    const connected = await local('/api/remote/discover', { url: site, connect: true }); assert.equal(connected.status, 200);
    await poll('Hub device online', async () => (await local('/api/state')).json.hub.state === 'connected');
    const cfg = JSON.parse(fs.readFileSync(path.join(configDir, 'config.json'))); deviceId = cfg.deviceId;
    assert.equal(cfg.accountKey, ''); assert.equal(cfg.remoteDeviceOnly, true);
    await pairActualQR();
    const phoneList = await phoneCommand({ type: 'sessions.list', tool: 'codex' }); assert(phoneList.sessions.some((item) => item.sessionKey === sessionKey));
    const phoneHistory = await phoneCommand({ type: 'session.open', sessionKey });
    assert(JSON.stringify(phoneHistory.events).includes('NATIVE_SESSION_FIRST_MARKER')); assert(JSON.stringify(phoneHistory.events).includes('Synthetic Grok-compatible reply'));
    await startPhoneStream(sessionKey);
    passed('Simulated phone used only real QR pairing token; actual Hub listed/opened the original session and live SSE');

    phase = 'external-owner protection expires';
    await poll('original external activity protection window', async () => {
      const value = await phoneCommand({ type: 'session.open', sessionKey });
      return value.session.controllable && value.session.status === 'idle';
    }, 45_000);
    phase = 'paired phone continuation';
    await phoneCommand({ type: 'session.send', sessionKey, text: phonePrompt });
    await poll('phone SSE turn completion', async () => {
      if (streamError) throw streamError;
      const failed = phoneEvents.find((event) => event.type === 'turn' && event.status === 'failed'); if (failed) throw new Error('remote turn failed: ' + failed.error);
      return phoneEvents.some((event) => event.type === 'turn' && event.status === 'completed');
    }, 45_000);
    assert(phoneEvents.some((event) => event.type === 'message' && event.role === 'user' && event.text.includes('NATIVE_SESSION_PHONE_MARKER')));
    assert(phoneEvents.some((event) => event.type === 'message' && event.role === 'assistant' && event.final && event.text.includes('Synthetic Grok-compatible reply')));
    assert(!phoneEvents.some((event) => event.type === 'notice' && (event.level === 'error' || event.text.includes('重试'))), 'remote transport retry/error');
    const finalHistory = await phoneCommand({ type: 'session.open', sessionKey });
    assert.equal(finalHistory.session.sessionKey, sessionKey);
    for (const marker of ['NATIVE_SESSION_FIRST_MARKER', 'NATIVE_SESSION_PHONE_MARKER', 'Synthetic Grok-compatible reply']) assert(JSON.stringify(finalHistory.events).includes(marker), 'history lost: ' + marker);
    const finalList = await phoneCommand({ type: 'sessions.list', tool: 'codex' });
    assert.equal(finalList.sessions.length, 1); assert.equal(finalList.sessions[0].sessionKey, sessionKey);
    const statsResponse = await request(base, '/fixture-stats');
    // The deliberately minimal loopback fixture writes valid JSON without a
    // Content-Type header; do not confuse that fixture quirk with a failed turn.
    const stats = statsResponse.json || JSON.parse(statsResponse.bytes.toString()); assert(stats.modelOK); assert(stats.calls >= 2);
    passed('Paired-phone session.send resumed SAME native thread; app SSE delivered user echo, final assistant reply and completed turn');
    passed('Original first-turn history remains intact; exactly one session; no fork or replacement session');
    Object.assign(report, { ok: true, fixture, threadId, sessionKey, deviceId, nativeProducerExitedBeforeBridge: true, bridgeReadOriginalHome: true, phoneTokenOnly: true, liveAssistantReplyReceived: true, originalHistoryPreserved: true, noFork: true, phoneSseEventTypes: [...new Set(phoneEvents.map((event) => event.type))], syntheticModelRequests: stats.calls, noTransportRetries: true });
    fs.writeFileSync(path.join(fixture, 'acceptance-report.json'), JSON.stringify(report, null, 2));
    console.log('RESULT complete: real native Codex -> actual Bridge -> actual Hub -> simulated paired-phone SSE; sanitized acceptance report saved');
  } catch (error) {
    console.error(`FAILED phase=${phase}: ${String(error.message || error).replace(/synthetic-[A-Za-z0-9-]*credential/g, '[fixture credential]')}`);
    process.exitCode = 1;
    if (fixture) fs.writeFileSync(path.join(fixture, 'acceptance-report.json'), JSON.stringify({ ...report, ok: false, phase, fixture }, null, 2));
  } finally {
    clearTimeout(deadline); sseAbort?.abort(); if (streamTask) await streamTask;
    if (phoneToken) { try { await phone('/app/pair/revoke', {}); } catch {} }
    if (bridge && ownsConsole) { try { await local('/api/quit', {}); } catch {} }
    await endOwnChild(bridge); await endOwnChild(producer); await endOwnChild(gateway);
    console.log('CLEANUP only own fixture processes stopped; shared Hub and original user Codex untouched');
  }
})();
