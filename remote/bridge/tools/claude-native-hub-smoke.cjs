// Real installed Claude -> production Bridge gateway/worker -> production Hub
// -> QR-paired simulated phone. Inference and credentials are loopback fixtures.
'use strict';
const fs = require('node:fs');
const path = require('node:path');
const http = require('node:http');
const net = require('node:net');
const crypto = require('node:crypto');
const assert = require('node:assert/strict');
const { spawn } = require('node:child_process');
const root = path.resolve(__dirname, '..');
const [claudeExe, bridgeExe, hubExe, hubFixtureRoot] = process.argv.slice(2);
for (const exe of [claudeExe, bridgeExe, hubExe]) assert(exe && path.isAbsolute(exe) && fs.statSync(exe).isFile(), 'explicit executable paths required');
for (const exe of [bridgeExe, hubExe]) assert(path.resolve(exe).startsWith(path.join(root, 'output') + path.sep), 'only fixture service binaries are allowed');
assert(hubFixtureRoot && path.isAbsolute(hubFixtureRoot) && fs.statSync(hubFixtureRoot).isDirectory(), 'explicit existing Hub fixture root required');
const consolePort = 47861, hubPort = 47862;
const consoleURL = `http://127.0.0.1:${consolePort}`, site = `http://127.0.0.1:${hubPort}`;
const model = 'claude-sonnet-4-6', key = 'synthetic-claude-hub-provider';
const events = [], bodies = [], report = { paidModelRequests: 0, realPhoneCameraUsed: false, steps: [] };
let phase = 'prepare', fixture, project, bridge, hub, upstream, cookie = '', token = '', deviceId = '', accountId = '', sessionKey = '', ownsConsole = false;
let streamAbort, streamTask, streamError, stopStarted = false, stopCanceled = false;
const delay = ms => new Promise(resolve => setTimeout(resolve, ms));
function passed(step) { report.steps.push(step); console.log(`PASS ${step}`); }
async function poll(label, predicate, limit = 45000) {
  const deadline = Date.now() + limit;
  while (Date.now() < deadline) { if (streamError) throw streamError; if (await predicate()) return; await delay(100); }
  throw new Error(`timed out: ${label}`);
}
async function freePort(port) {
  await new Promise((resolve, reject) => { const server = net.createServer(); server.once('error', reject); server.listen(port, '127.0.0.1', () => server.close(resolve)); });
}
async function request(base, route, body, headers = {}) {
  const url = new URL(base); assert.equal(url.protocol, 'http:'); assert.equal(url.hostname, '127.0.0.1');
  const response = await fetch(base + route, { method: body === undefined ? 'GET' : 'POST', headers: { ...(body === undefined ? {} : { 'Content-Type': 'application/json' }), ...headers }, body: body === undefined ? undefined : JSON.stringify(body), signal: AbortSignal.timeout(55000), redirect: 'error' });
  const bytes = Buffer.from(await response.arrayBuffer());
  return { status: response.status, headers: response.headers, bytes, json: response.headers.get('content-type')?.includes('application/json') ? JSON.parse(bytes) : null };
}
const local = (route, body) => request(consoleURL, route, body, { Cookie: cookie, Origin: consoleURL });
const phone = (route, body) => request(site, '/salcara-hub/v1' + route, body, token ? { 'X-Salcara-Pair-Token': token } : {});
async function command(cmd) {
  const response = await phone('/app/commands', { deviceId, command: cmd, ...(cmd.operationId ? { requestId: cmd.operationId } : {}) });
  assert.equal(response.status, 200, `${cmd.type}: HTTP ${response.status}`);
  assert.equal(response.json.ok, true, `${cmd.type}: ${response.json.error || 'failed'}`);
  return response.json.result;
}
function child(exe, args, env = process.env) {
  const proc = spawn(exe, args, { cwd: project, env, windowsHide: true, stdio: ['pipe', 'pipe', 'pipe'] });
  proc.fixtureStderr = ''; proc.stdout.resume();
  proc.stderr.on('data', bytes => { proc.fixtureStderr = (proc.fixtureStderr + bytes).slice(-16000); });
  proc.on('error', error => { streamError = error; });
  return proc;
}
async function endOwnChild(proc) {
  if (!proc || proc.exitCode !== null || proc.signalCode !== null) return;
  proc.stdin.end();
  await Promise.race([new Promise(resolve => proc.once('exit', resolve)), delay(3000)]);
  if (proc.exitCode === null && proc.signalCode === null) { proc.kill(); await Promise.race([new Promise(resolve => proc.once('exit', resolve)), delay(3000)]); }
}
async function startStream() {
  streamAbort = new AbortController();
  const response = await fetch(site + '/salcara-hub/v1/app/stream?after=0', { headers: { Accept: 'text/event-stream', 'X-Salcara-Pair-Token': token }, signal: streamAbort.signal, redirect: 'error' });
  assert.equal(response.status, 200);
  const decoder = new TextDecoder(); let buffer = '';
  streamTask = (async () => {
    try {
      for await (const bytes of response.body) {
        buffer += decoder.decode(bytes, { stream: true }).replace(/\r\n/g, '\n');
        while (buffer.includes('\n\n')) {
          const end = buffer.indexOf('\n\n'), frame = buffer.slice(0, end); buffer = buffer.slice(end + 2);
          const data = frame.split('\n').filter(line => line.startsWith('data:')).map(line => line.slice(5).trimStart()).join('\n');
          if (!data) continue;
          let event; try { event = JSON.parse(data); } catch { continue; }
          if (event.deviceId === deviceId && event.sessionKey) events.push(event);
        }
      }
    } catch (error) { if (!streamAbort.signal.aborted) streamError = error; }
  })();
}
async function awaitTurn(key, from, text) {
  await poll(text, () => {
    const current = events.slice(from).filter(event => event.sessionKey === key);
    const failed = current.find(event => event.type === 'turn' && event.status === 'failed');
    if (failed) throw new Error(`turn failed: ${failed.error || failed.errorCode}`);
    return current.some(event => event.type === 'message' && event.role === 'assistant' && event.final && event.text?.includes(text))
      && current.some(event => event.type === 'turn' && event.status === 'completed');
  });
}
async function pair() {
  const started = await local('/api/pair/start', {}); assert.equal(started.status, 200);
  const png = await local(started.json.qrUrl); assert.equal(png.status, 200);
  const zxingRoot = path.join(root, 'output', 'single-scan-e2e-tools', 'node_modules', 'zxing-wasm');
  const decoder = require(path.join(zxingRoot, 'dist', 'cjs', 'reader', 'index.js'));
  await decoder.prepareZXingModule({ overrides: { wasmBinary: fs.readFileSync(path.join(zxingRoot, 'dist', 'reader', 'zxing_reader.wasm')) }, fireImmediately: true });
  const decoded = await decoder.readBarcodes(new Uint8Array(png.bytes), { formats: ['QRCode'] }); assert.equal(decoded.length, 1);
  const qr = JSON.parse(decoded[0].text); assert.equal(qr.hubUrl, site + '/salcara-hub/v1'); assert.equal(qr.deviceId, deviceId);
  const result = await phone('/app/pair/qr', { deviceId, ticket: qr.ticket, phoneId: crypto.randomBytes(32).toString('hex') });
  assert.equal(result.status, 200); token = result.json.pair_token; assert(/^[a-f0-9]{64}$/.test(token));
  await poll('Bridge confirms phone pairing', async () => (await local('/api/state')).json.hub.pairing?.paired === true);
}
async function startUpstream() {
  upstream = http.createServer(async (req, res) => {
    try {
      assert.equal(req.headers.authorization, `Bearer ${key}`, 'selected API must supply the upstream credential');
      const pathname = new URL(req.url, 'http://127.0.0.1').pathname;
      if (pathname === '/v1/models') { res.writeHead(200, { 'Content-Type': 'application/json' }); res.end(JSON.stringify({ data: [{ id: model }] })); return; }
      let raw = ''; for await (const chunk of req) { raw += chunk; assert(raw.length < 4 * 1024 * 1024, 'bounded fixture request'); }
      if (pathname === '/v1/messages/count_tokens') { res.writeHead(200, { 'Content-Type': 'application/json' }); res.end('{"input_tokens":100}'); return; }
      assert.equal(pathname, '/v1/messages');
      const body = JSON.parse(raw); assert.equal(body.model, model); bodies.push(body);
      const last = JSON.stringify(body.messages?.findLast(item => item.role === 'user')?.content || '');
      const data = value => res.write(`event: ${value.type}\ndata: ${JSON.stringify(value)}\n\n`);
      res.writeHead(200, { 'Content-Type': 'text/event-stream' });
      data({ type: 'message_start', message: { id: `msg_fixture_${bodies.length}`, type: 'message', role: 'assistant', model, content: [], stop_reason: null, stop_sequence: null, usage: { input_tokens: 100, output_tokens: 0 } } });
      if (last.includes('HUB_FIXTURE_STOP')) { stopStarted = true; res.on('close', () => { if (!res.writableEnded) stopCanceled = true; }); return; }
      const write = last.includes('HUB_FIXTURE_WRITE') && !last.includes('tool_result');
      if (write) {
        data({ type: 'content_block_start', index: 0, content_block: { type: 'tool_use', id: 'tool_hub_fixture_write', name: 'Write', input: {} } });
        data({ type: 'content_block_delta', index: 0, delta: { type: 'input_json_delta', partial_json: JSON.stringify({ file_path: path.join(project, 'phone-approved.txt'), content: 'claude-hub-phone-approved\n' }) } });
      } else {
        let text = 'claude-hub-first-reply';
        if (last.includes('tool_result')) text = 'claude-hub-write-done';
        else if (last.includes('HUB_FIXTURE_CONTINUE')) text = 'claude-hub-continued';
        else if (last.includes('HUB_FIXTURE_NEW')) text = 'claude-hub-new-task';
        else if (last.includes('HUB_FIXTURE_SECOND')) { assert(raw.includes('HUB_FIXTURE_FIRST') && raw.includes('claude-hub-first-reply'), 'same conversation history required'); text = 'claude-hub-second-reply'; }
        data({ type: 'content_block_start', index: 0, content_block: { type: 'text', text: '' } });
        data({ type: 'content_block_delta', index: 0, delta: { type: 'text_delta', text } });
      }
      data({ type: 'content_block_stop', index: 0 });
      data({ type: 'message_delta', delta: { stop_reason: write ? 'tool_use' : 'end_turn', stop_sequence: null }, usage: { output_tokens: 10 } });
      data({ type: 'message_stop' }); res.end();
    } catch (error) { streamError = error; res.destroy(error); }
  });
  await new Promise(resolve => upstream.listen(0, '127.0.0.1', resolve));
  return `http://127.0.0.1:${upstream.address().port}`;
}
(async () => {
  try {
    await freePort(consolePort); await freePort(hubPort);
    fixture = fs.mkdtempSync(path.join(root, 'output', 'claude-native-hub-smoke-'));
    for (const dir of ['codex-home', 'claude-home', 'codex-app', 'claude-app', 'project', 'profile', 'appdata', 'localappdata', 'bridge-config']) fs.mkdirSync(path.join(fixture, dir));
    project = path.join(fixture, 'project');
    const hubDir = fs.mkdtempSync(path.join(hubFixtureRoot, 'claude-hub-'));
    const disk = fs.statfsSync(hubDir); assert(disk.bavail * disk.bsize > Math.max(disk.blocks * disk.bsize / 20, 2 * 1024 ** 3) + 32 * 1024 ** 2, 'Hub fixture volume must exceed production storage reserve');
    report.hubFixture = hubDir;
    const enrollmentClaim = crypto.randomBytes(32).toString('hex');
    fs.writeFileSync(path.join(hubDir, 'device-enrollment-claims.json'), JSON.stringify({ version: 1, claims: [{ hash: crypto.createHash('sha256').update(Buffer.from(enrollmentClaim, 'hex')).digest('hex'), expiresAt: Date.now() + 3600000 }] }));
    const configDir = path.join(fixture, 'bridge-config'), base = await startUpstream();
    const env = { ...process.env, CODEX_HOME: path.join(fixture, 'codex-home'), CLAUDE_CONFIG_DIR: path.join(fixture, 'claude-home'), CODEX_ELECTRON_USER_DATA_PATH: path.join(fixture, 'codex-app'), CLAUDE_USER_DATA_DIR: path.join(fixture, 'claude-app'), USERPROFILE: path.join(fixture, 'profile'), HOME: path.join(fixture, 'profile'), APPDATA: path.join(fixture, 'appdata'), LOCALAPPDATA: path.join(fixture, 'localappdata'), SALCARA_BRIDGE_PORT: String(consolePort), SALCARA_BRIDGE_CONFIG_DIR: configDir, SALCARA_EMBEDDED_WINDOW: '1', SALCARA_NO_SHELL_PATH: '1', CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: '1', ANTHROPIC_API_KEY: 'synthetic-stale-key', ANTHROPIC_BASE_URL: 'http://127.0.0.1:1', CLAUDE_CODE_USE_BEDROCK: '1' };
    for (const name of ['CODEX_TOOL_PIPE', 'CODEX_APP_TOOLS_PIPE_PATH', 'CODEX_TOOL_CALL_ID', 'CODEX_THREAD_ID', 'CODEX_TURN_ID']) delete env[name];
    const projectSettings = '{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:2","ANTHROPIC_AUTH_TOKEN":"synthetic-project-key","CLAUDE_CODE_USE_VERTEX":"1"}}';
    fs.mkdirSync(path.join(project, '.claude')); fs.writeFileSync(path.join(project, '.claude', 'settings.local.json'), projectSettings);
    const account = { id: 'claude-hub-fixture', name: 'Claude synthetic loopback API', kind: 'api', sourceType: 'api_key', wire: 'anthropic', baseUrl: base, key, model, models: [model], authMode: 'bearer', protocol: 'anthropic' };
    fs.writeFileSync(path.join(configDir, 'config.json'), JSON.stringify({ autostart: false, autostartDecided: true, openConsoleOnStart: false, deviceName: 'Claude Hub fixture ' + path.basename(fixture), accountKey: '', codexKey: '', claudeKey: '', localAccountsReady: true, localAccounts: [account], activeClaudeAccount: account.id, localToolPaths: { claude: claudeExe, codex: path.join(fixture, 'absent-codex.exe') }, toolAPISelections: { claude: account.id }, toolModels: { claude: model }, toolProtocols: { claude: 'anthropic' }, projects: [{ path: project, name: 'Claude Hub fixture' }], approval: 'ask' }, null, 2));
    console.log(`FIXTURE ${fixture}`);
    phase = 'Hub and Bridge startup';
    hub = child(hubExe, ['-listen', `127.0.0.1:${hubPort}`, '-data-dir', hubDir, '-public-url', base + '/salcara-hub']);
    await poll('Hub health', async () => { try { return (await request(site, '/salcara-hub/v1/ping')).json.protocolVersion === 1; } catch { return false; } });
    bridge = child(bridgeExe, ['--background'], env);
    await poll('Bridge health', async () => { try { return (await request(consoleURL, '/healthz')).status === 200; } catch { return false; } });
    const page = await request(consoleURL, '/'); cookie = page.headers.get('set-cookie').split(';')[0];
    assert.equal(path.resolve((await local('/api/state')).json.configDir), path.resolve(configDir)); ownsConsole = true;
    const connected = await local('/api/remote/discover', { url: site, connect: true, enrollmentClaim }); assert.equal(connected.status, 200);
    await poll('Hub connected', async () => (await local('/api/state')).json.hub.state === 'connected');
    const cfg = JSON.parse(fs.readFileSync(path.join(configDir, 'config.json'))); deviceId = cfg.deviceId; assert.equal(cfg.remoteDeviceOnly, true); assert.equal(cfg.accountKey, '');
    phase = 'QR pairing and Claude API selection'; await pair();
    const status = await command({ type: 'agents.status' }); assert.equal(status.apis.length, 1); accountId = status.apis[0].id;
    await command({ type: 'agents.api.set', agent: 'claude', accountId, model }); await startStream();
    passed('Real QR pairing and opaque API selection through actual Hub');
    phase = 'new Claude task';
    let from = events.length;
    const started = await command({ type: 'session.start', tool: 'claude', controlSurface: 'cli', cwd: project, prompt: 'HUB_FIXTURE_FIRST: reply only.', accountId, model, approval: 'ask', operationId: crypto.randomUUID() });
    sessionKey = started.sessionKey; assert(sessionKey.startsWith('claude:'));
    await awaitTurn(sessionKey, from, 'claude-hub-first-reply');
    from = events.length;
    await command({ type: 'session.send', sessionKey, accountId, model, approval: 'ask', text: 'HUB_FIXTURE_SECOND: continue same conversation.', operationId: crypto.randomUUID() });
    await awaitTurn(sessionKey, from, 'claude-hub-second-reply');
    const history = await command({ type: 'session.open', sessionKey });
    for (const marker of ['HUB_FIXTURE_FIRST', 'claude-hub-first-reply', 'HUB_FIXTURE_SECOND', 'claude-hub-second-reply']) assert(JSON.stringify(history.events).includes(marker));
    passed('Real Claude start/send retained selected API, model and original history; phone SSE delivered replies and completion');
    phase = 'phone approval'; from = events.length;
    await command({ type: 'session.send', sessionKey, accountId, model, approval: 'ask', text: 'HUB_FIXTURE_WRITE: create only the supplied fixture file after approval.', operationId: crypto.randomUUID() });
    await poll('phone Write approval', () => events.slice(from).some(event => event.sessionKey === sessionKey && event.type === 'approval.request'));
    const approval = events.slice(from).find(event => event.sessionKey === sessionKey && event.type === 'approval.request');
    assert(JSON.stringify(approval).includes('phone-approved.txt')); assert(!fs.existsSync(path.join(project, 'phone-approved.txt')));
    await command({ type: 'approval.respond', sessionKey, accountId, approvalId: approval.approvalId, decision: 'allow' });
    await awaitTurn(sessionKey, from, 'claude-hub-write-done');
    assert.equal(fs.readFileSync(path.join(project, 'phone-approved.txt'), 'utf8'), 'claude-hub-phone-approved\n');
    passed('Native Write waited for paired-phone approval, then created exactly the fixture file');
    phase = 'exact turn interruption'; from = events.length;
    await command({ type: 'session.send', sessionKey, accountId, model, approval: 'ask', text: 'HUB_FIXTURE_STOP: synthetic long-running request.', operationId: crypto.randomUUID() });
    await poll('long-running upstream request', () => stopStarted);
    const active = await command({ type: 'session.open', sessionKey }), expectedTurnId = active.session.activeTurnId; assert(expectedTurnId);
    const stopped = await command({ type: 'session.interrupt', sessionKey, expectedTurnId, operationId: crypto.randomUUID() }); report.interruptReceipt = stopped; assert.equal(stopped.turnId, expectedTurnId);
    await poll('interrupted terminal state and upstream cancellation', () => stopCanceled && events.slice(from).some(event => event.sessionKey === sessionKey && event.type === 'turn' && event.turnId === expectedTurnId && event.status === 'interrupted'));
    streamAbort.abort(); await streamTask; await startStream();
    const recovered = await command({ type: 'session.open', sessionKey }); report.recoveredSession = recovered.session;
    assert.equal(stopped.status, 'stopped'); assert.equal(recovered.session.lastTerminalTurnId, expectedTurnId); assert(!recovered.session.activeTurnId);
    assert(JSON.stringify(recovered.events).includes('HUB_FIXTURE_FIRST'));
    passed('Exact-turn stop canceled native upstream; reconnect restored interruption and original history');
    phase = 'continue and idempotent replay'; from = events.length;
    const continuedCommand = { type: 'session.continue', sessionKey, accountId, model, approval: 'ask', expectedTurnId, operationId: crypto.randomUUID(), text: 'HUB_FIXTURE_CONTINUE: continue this same conversation.' };
    const continued = await command(continuedCommand); assert.equal(continued.accepted, true); assert.equal(continued.sessionKey, sessionKey); assert.equal(continued.previousTurnId, expectedTurnId);
    await awaitTurn(sessionKey, from, 'claude-hub-continued');
    const calls = bodies.length; assert.deepEqual(await command(continuedCommand), continued); assert.equal(bodies.length, calls);
    passed('Terminal continuation retained session identity; receipt replay executed no extra model turn');
    phase = 'workspace release'; from = events.length;
    const fresh = await command({ type: 'session.start', tool: 'claude', controlSurface: 'cli', cwd: project, prompt: 'HUB_FIXTURE_NEW: a new task in the same workspace.', accountId, model, approval: 'ask', operationId: crypto.randomUUID() }); assert.notEqual(fresh.sessionKey, sessionKey);
    await awaitTurn(fresh.sessionKey, from, 'claude-hub-new-task');
    assert.equal(fs.readFileSync(path.join(project, '.claude', 'settings.local.json'), 'utf8'), projectSettings);
    passed('Completed task released workspace for another Claude task; conflicting project settings remained unchanged');
    Object.assign(report, { ok: true, fixture, sessionKey, syntheticModelRequests: bodies.length, phoneSseEventTypes: [...new Set(events.map(event => event.type))] });
    console.log('RESULT real Claude -> actual Bridge -> actual Hub -> paired-phone SSE passed');
  } catch (error) {
    process.exitCode = 1; console.error(`FAILED phase=${phase}: ${error.message || error}`);
    Object.assign(report, { ok: false, phase, error: String(error.stack || error), events: events.map(({ type, status, turnId, approvalId, errorCode, error }) => ({ type, status, turnId, approvalId, errorCode, error })), syntheticModelRequests: bodies.length, bridgeStderr: bridge?.fixtureStderr, hubStderr: hub?.fixtureStderr });
  } finally {
    if (fixture) fs.writeFileSync(path.join(fixture, 'acceptance-report.json'), JSON.stringify(report, null, 2));
    streamAbort?.abort(); if (streamTask) await streamTask;
    if (token) { try { await phone('/app/pair/revoke', {}); } catch {} }
    if (bridge && ownsConsole) { try { await local('/api/quit', {}); } catch {} }
    await endOwnChild(bridge); await endOwnChild(hub);
    if (upstream) { upstream.closeAllConnections(); await new Promise(resolve => upstream.close(resolve)); }
    console.log('CLEANUP own fixture services stopped');
  }
})();
