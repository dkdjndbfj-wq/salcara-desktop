'use strict';

// Local plugins execute in disposable workers, so synchronous loops, rejected
// callbacks and plugin timers cannot block or outlive the desktop host. This is
// a responsiveness/lifecycle boundary, not a security sandbox for untrusted code.
const fs = require('node:fs');
const path = require('node:path');
const { randomUUID, createHash } = require('node:crypto');
const { Worker } = require('node:worker_threads');
const { EventEmitter } = require('node:events');
const { LIMITS, PERMISSIONS, EVENTS, safeID, text, jsonCopy, panelConfig, satelliteConfig } = require('./plugin-protocol.cjs');

function validateManifest(raw, dir) {
  if (!raw || typeof raw !== 'object' || Array.isArray(raw)) throw new Error('manifest.json must be an object');
  for (const field of ['id', 'name', 'version']) {
    if (typeof raw[field] !== 'string' || !raw[field].trim()) throw new Error(`plugin "${dir}": manifest missing required string field "${field}"`);
  }
  if (!safeID(raw.id) || /^(con|prn|aux|nul|com[1-9]|lpt[1-9])$/i.test(raw.id)) throw new Error('plugin id must be a safe directory name');
  if (raw.name.length > 128 || raw.version.length > 64) throw new Error('plugin metadata is too long');
  if (raw.permissions !== undefined && !Array.isArray(raw.permissions)) throw new Error('plugin permissions must be an array');
  const permissions = [...new Set(raw.permissions || [])];
  for (const p of permissions) if (!PERMISSIONS.has(p)) throw new Error(`plugin "${raw.id}": unknown permission "${p}"`);
  const entry = raw.entry === undefined ? 'index.js' : raw.entry;
  if (typeof entry !== 'string' || !entry || entry.length > 512) throw new Error('invalid plugin entry');
  const minBridgeVersion = raw.minBridgeVersion === undefined ? '0' : raw.minBridgeVersion;
  parseVersion(minBridgeVersion);
  return { id: raw.id, name: raw.name, version: raw.version, entry, permissions,
    description: text(raw.description, 2048), author: text(raw.author, 128), minBridgeVersion };
}
function parseVersion(value) {
  if (typeof value !== 'string' || value.length > 80 || !/^\d+(?:\.\d+){0,2}(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$/.test(value)) throw new Error('invalid minimum Bridge version');
  const [main, pre] = value.split('+')[0].split('-', 2);
  return { numbers: main.split('.').map(Number), pre: pre?.split('.') };
}
function supportsVersion(current, minimum) {
  const a = parseVersion(current), b = parseVersion(minimum);
  for (let i = 0; i < 3; i++) if ((a.numbers[i] || 0) !== (b.numbers[i] || 0)) return (a.numbers[i] || 0) > (b.numbers[i] || 0);
  if (!a.pre || !b.pre) return !a.pre || Boolean(b.pre);
  for (let i = 0; i < Math.max(a.pre.length, b.pre.length); i++) {
    if (a.pre[i] === b.pre[i]) continue;
    if (a.pre[i] === undefined) return false;
    if (b.pre[i] === undefined) return true;
    const an = /^\d+$/.test(a.pre[i]), bn = /^\d+$/.test(b.pre[i]);
    if (an && bn) return Number(a.pre[i]) > Number(b.pre[i]);
    if (an !== bn) return !an;
    return a.pre[i] > b.pre[i];
  }
  return true;
}
function within(root, target, allowRoot = false) {
  const relative = path.relative(root, target);
  return (allowRoot || Boolean(relative)) && relative !== '..' && !relative.startsWith(`..${path.sep}`) && !path.isAbsolute(relative);
}
function readLimited(filename, limit) {
  const descriptor = fs.openSync(filename, 'r');
  try {
    const size = fs.fstatSync(descriptor).size;
    if (size > limit) throw new Error('plugin file exceeds the size limit');
    const buffer = Buffer.alloc(size);
    let offset = 0;
    while (offset < size) {
      const count = fs.readSync(descriptor, buffer, offset, size - offset, offset);
      if (!count) throw new Error('plugin file changed while reading');
      offset += count;
    }
    if (fs.readSync(descriptor, Buffer.alloc(1), 0, 1, size)) throw new Error('plugin file changed while reading');
    return buffer;
  } finally { fs.closeSync(descriptor); }
}
function readJSON(filename, limit) {
  const stat = fs.lstatSync(filename);
  if (stat.isSymbolicLink()) throw new Error('plugin data files cannot be symbolic links');
  if (!stat.isFile() || stat.size > limit) throw new Error('plugin file exceeds the size limit');
  return JSON.parse(readLimited(filename, limit).toString('utf8'));
}
function resolvePluginEntry(dir, entry) {
  const root = fs.realpathSync(dir), candidate = path.resolve(root, entry);
  if (!within(root, candidate)) throw new Error('plugin entry must be inside its directory');
  const actual = fs.realpathSync(candidate);
  if (!within(root, actual) || !fs.statSync(actual).isFile()) throw new Error('plugin entry must be a file inside its directory');
  if (fs.statSync(actual).size > LIMITS.source) throw new Error('plugin entry exceeds 1 MiB');
  return actual;
}
function atomicJSON(filename, data) {
  const temporary = `${filename}.tmp-${randomUUID()}`;
  try {
    fs.writeFileSync(temporary, JSON.stringify(data) + '\n', { encoding: 'utf8', flag: 'wx' });
    fs.renameSync(temporary, filename);
  } catch (error) {
    try { fs.rmSync(temporary, { force: true }); } catch { /* preserve the original error */ }
    throw error;
  }
}
function storageData(filename) {
  if (!fs.existsSync(filename)) return Object.create(null);
  const parsed = readJSON(filename, LIMITS.storage);
  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) throw new Error('plugin storage is invalid');
  if (Object.keys(parsed).length > LIMITS.storageKeys) throw new Error('plugin storage has too many keys');
  return Object.assign(Object.create(null), parsed);
}

class PluginLoader extends EventEmitter {
  constructor({ pluginsDir, storageDir, host = {}, bridgeVersion = '0', timeouts = {} }) {
    super();
    this._pluginsDir = path.resolve(pluginsDir); this._storageDir = path.resolve(storageDir);
    this._host = host; this._bridgeVersion = bridgeVersion;
    this._plugins = new Map(); this._queues = new Map(); this._queueCounts = new Map(); this._closed = false; this._closePromise = null;
    this._timeouts = { activation: 5000, hook: 1000, action: 2000, event: 1000, deactivate: 1000, heartbeat: 3000, chain: 2500, ...timeouts };
    // Kept as serialisable capability indices for callers/tests, never functions.
    this._hooks = { request: [], response: [], apiEvents: Object.create(null), sessionEvents: Object.create(null) };
    fs.mkdirSync(this._pluginsDir, { recursive: true }); fs.mkdirSync(this._storageDir, { recursive: true });
    this._pluginsDir = fs.realpathSync(this._pluginsDir); this._storageDir = fs.realpathSync(this._storageDir);
  }
  _assertOpen() { if (this._closed) throw new Error('plugin loader is closed'); }
  _enqueue(id, task) {
    this._assertOpen();
    const count = this._queueCounts.get(id) || 0;
    if (count >= LIMITS.pending) return Promise.reject(new Error('too many pending plugin operations'));
    this._queueCounts.set(id, count + 1);
    const previous = this._queues.get(id) || Promise.resolve();
    const next = previous.catch(() => {}).then(task);
    this._queues.set(id, next);
    next.finally(() => {
      const count = this._queueCounts.get(id) - 1;
      if (count) this._queueCounts.set(id, count); else this._queueCounts.delete(id);
      if (this._queues.get(id) === next) this._queues.delete(id);
    }).catch(() => {});
    return next;
  }
  _checkVersion(manifest) {
    if (!supportsVersion(this._bridgeVersion, manifest.minBridgeVersion)) throw new Error(`plugin requires Salcara Bridge ${manifest.minBridgeVersion} or newer`);
  }
  async loadAll() {
    this._assertOpen();
    const entries = fs.readdirSync(this._pluginsDir, { withFileTypes: true }).filter(e => e.isDirectory() && !e.name.startsWith('.')).slice(0, LIMITS.plugins);
    for (const entry of entries) {
      if (this._closed) return;
      await this._enqueue(entry.name, async () => {
        if (this._plugins.has(entry.name)) return;
        const dir = path.join(this._pluginsDir, entry.name);
        let raw, manifest;
        try {
          if (!within(this._pluginsDir, fs.realpathSync(dir))) throw new Error('plugin directory is outside plugin storage');
          raw = readJSON(path.join(dir, 'manifest.json'), 65536);
          manifest = validateManifest(raw, dir);
          if (manifest.id !== entry.name) throw new Error('plugin directory does not match its id');
        } catch (error) {
          if (safeID(entry.name)) this._plugins.set(entry.name, { manifest: { id: entry.name, name: entry.name, version: '', permissions: [], description: '', author: '' }, dir, enabled: false, status: 'error', error: error.message });
          return;
        }
        const rec = { manifest, dir, enabled: raw.enabled !== false, status: 'disabled', error: undefined, runtime: null };
        this._plugins.set(manifest.id, rec);
        if (rec.enabled) await this._activate(rec);
      });
    }
    this._changed();
  }
  inspect(srcDir) {
    this._assertOpen();
    if (typeof srcDir !== 'string' || !path.isAbsolute(srcDir)) throw new Error('srcDir must be an absolute path');
    const root = fs.realpathSync(srcDir);
    if (!fs.statSync(root).isDirectory()) throw new Error('plugin source must be a directory');
    if (within(root, this._pluginsDir, true)) throw new Error('plugin source must not contain the installation directory');
    const raw = readJSON(path.join(root, 'manifest.json'), 65536), manifest = validateManifest(raw, root);
    this._checkVersion(manifest);
    // Startup activates plugins asynchronously; count saved packages that the
    // startup scan has not reached yet as well as records already loaded.
    const installed = new Set(this._plugins.keys());
    for (const entry of fs.readdirSync(this._pluginsDir, { withFileTypes: true })) {
      if (entry.isDirectory() && !entry.name.startsWith('.')) installed.add(entry.name);
    }
    if (installed.size >= LIMITS.plugins) throw new Error('at most 32 plugins can be installed');
    if (this._plugins.has(manifest.id) || fs.existsSync(path.join(this._pluginsDir, manifest.id))) throw new Error(`plugin "${manifest.id}" is already installed`);
    resolvePluginEntry(root, manifest.entry);
    const packageDigest = this._packageDigest(root);
    return { ...manifest, enabled: raw.enabled !== false, packageDigest };
  }
  install(srcDir, expectedDigest) {
    if (expectedDigest !== undefined && (typeof expectedDigest !== 'string' || !/^[a-f0-9]{64}$/.test(expectedDigest))) throw new Error('invalid inspected plugin digest');
    const selected = this.inspect(srcDir);
    if (expectedDigest && selected.packageDigest !== expectedDigest) throw new Error('插件文件已改变，请重新检查并确认安装');
    return this._enqueue(selected.id, async () => {
      const inspected = this.inspect(srcDir), root = fs.realpathSync(srcDir);
      if (inspected.packageDigest !== (expectedDigest || selected.packageDigest)) throw new Error('插件文件已改变，请重新检查并确认安装');
      const manifest = validateManifest(inspected, root);
      const dest = path.join(this._pluginsDir, manifest.id), stage = path.join(this._pluginsDir, `.install-${manifest.id}-${randomUUID()}`);
      try {
        fs.mkdirSync(stage);
        for (const relative of this._sourceFiles(root)) {
          const source = path.join(root, relative), destination = path.join(stage, relative);
          if (fs.lstatSync(source).isSymbolicLink() || !within(root, fs.realpathSync(source))) throw new Error('plugin files must stay within their directory');
          fs.mkdirSync(path.dirname(destination), { recursive: true });
          fs.copyFileSync(source, destination, fs.constants.COPYFILE_EXCL);
        }
        const copied = readJSON(path.join(stage, 'manifest.json'), 65536), copiedManifest = validateManifest(copied, stage);
        if (JSON.stringify(copiedManifest) !== JSON.stringify(validateManifest(manifest, root))) throw new Error('plugin manifest changed during installation');
        resolvePluginEntry(stage, copied.entry === undefined ? 'index.js' : copied.entry);
        if (this._packageDigest(stage) !== inspected.packageDigest) throw new Error('插件文件在安装期间改变，请重新检查');
        atomicJSON(path.join(stage, 'manifest.json'), { ...copied, enabled: true });
        fs.renameSync(stage, dest);
      } catch (error) {
        try { fs.rmSync(stage, { recursive: true, force: true }); } catch { /* hidden staging directory cannot execute */ }
        throw error;
      }
      const rec = { manifest, dir: dest, enabled: true, status: 'loading', error: undefined, runtime: null };
      this._plugins.set(manifest.id, rec);
      await this._activate(rec); this._changed();
      return manifest.id;
    });
  }
  enable(id) {
    return this._enqueue(id, async () => {
      const rec = this._record(id);
      if (rec.enabled && rec.status === 'ok') return;
      const raw = readJSON(path.join(rec.dir, 'manifest.json'), 65536), manifest = validateManifest(raw, rec.dir);
      if (manifest.id !== id) throw new Error('plugin manifest identity changed');
      this._checkVersion(manifest); rec.manifest = manifest;
      this._persistEnabled(rec, true); rec.enabled = true;
      await this._activate(rec); this._changed();
    });
  }
  disable(id) {
    return this._enqueue(id, async () => {
      const rec = this._record(id);
      if (!rec.enabled && !rec.runtime) return;
      this._persistEnabled(rec, false); rec.enabled = false;
      await this._stop(rec);
      rec.status = 'disabled'; rec.error = undefined; this._changed();
    });
  }
  uninstall(id) {
    return this._enqueue(id, async () => {
      const rec = this._record(id);
      // Persist the stopped state first. An interrupted uninstall must not load
      // this plugin again, even when Windows still holds one of its files open.
      if (rec.enabled) this._persistEnabled(rec, false);
      rec.enabled = false; await this._stop(rec);
      try {
        const actual = fs.realpathSync(rec.dir);
        if (!within(this._pluginsDir, actual) || path.basename(actual) !== id) throw new Error('plugin removal path is outside plugin storage');
        fs.rmSync(path.join(this._storageDir, `${id}.json`), { force: true });
        fs.rmSync(rec.dir, { recursive: true, force: true });
        this._plugins.delete(id);
      } catch (error) {
        rec.status = 'error'; rec.error = `卸载未完成：${error.message}`; this._changed(); throw error;
      }
      this._changed();
    });
  }
  close() {
    if (this._closePromise) return this._closePromise;
    this._closed = true;
    this._closePromise = (async () => {
      await Promise.allSettled([...this._queues.values()]);
      await Promise.all([...this._plugins.values()].map(rec => this._stop(rec)));
      // Enabled flags are user preferences, not process lifetime state.
    })();
    return this._closePromise;
  }
  list() {
    return [...this._plugins.values()].map(rec => ({ ...rec.manifest, permissions: [...rec.manifest.permissions], enabled: rec.enabled, status: rec.status, error: rec.error }));
  }
  async runRequestHooks(req) { return this._runChain('request', req); }
  async runResponseHooks(req, res) { return this._runChain('response', res, req); }
  async _runChain(phase, value, request) {
    let current = jsonCopy(value), deadline = Date.now() + this._timeouts.chain;
    for (const { pluginId } of [...this._hooks[phase]]) {
      const rec = this._plugins.get(pluginId), runtime = rec?.runtime;
      if (rec?.status !== 'ok' || !runtime || runtime.stopping) continue;
      const remaining = deadline - Date.now(); if (remaining <= 0) break;
      try {
        const args = phase === 'response' ? [request, current] : [current];
        const next = await this._call(runtime, phase, args, Math.min(remaining, this._timeouts.hook));
        if (rec.runtime === runtime && rec.status === 'ok' && !runtime.stopping) {
          if (!next || typeof next !== 'object' || Array.isArray(next)) throw new Error('plugin hook must return an object');
          if (phase === 'request' && (!next.body || typeof next.body !== 'object' || Array.isArray(next.body))) throw new Error('plugin request body must be an object');
          current = jsonCopy(next);
        }
      } catch (error) { await this._fail(rec, runtime, error); }
    }
    return current;
  }
  fireSessionEvent(event, payload) { if (EVENTS.has(event)) return this._fire('sessionEvent', 'sessionEvents', event, payload); return Promise.resolve(); }
  fireApiEvent(event, payload) { return this._fire('apiEvent', 'apiEvents', event, payload); }
  async _fire(method, group, event, payload) {
    let value; try { value = jsonCopy(payload); } catch { return; }
    const records = (this._hooks[group][event] || []).map(({ pluginId }) => this._plugins.get(pluginId));
    await Promise.all(records.map(async rec => {
      const runtime = rec?.runtime;
      if (!runtime || runtime.stopping || rec.status !== 'ok') return;
      try { await this._call(runtime, method, [event, value], this._timeouts.event); }
      catch (error) { await this._fail(rec, runtime, error); }
    }));
  }
  async invokeUIAction(id, kind, target, action = '') {
    const rec = this._record(id), runtime = rec.runtime;
    if (!runtime || runtime.stopping || rec.status !== 'ok') throw new Error('插件尚未运行');
    if (!safeID(target) || !['panel', 'satellite'].includes(kind) || (kind === 'panel' && !safeID(action))) throw new Error('invalid plugin action');
    try { await this._call(runtime, 'action', [kind, target, action], this._timeouts.action); }
    catch (error) { await this._fail(rec, runtime, error); throw error; }
  }
  _record(id) { const rec = this._plugins.get(id); if (!rec) throw new Error(`plugin "${id}" not found`); return rec; }
  _changed() { this.emit('change', this.list()); }
  _persistEnabled(rec, enabled) {
    const filename = path.join(rec.dir, 'manifest.json'), raw = readJSON(filename, 65536);
    if (validateManifest(raw, rec.dir).id !== rec.manifest.id) throw new Error('plugin manifest identity changed');
    atomicJSON(filename, { ...raw, enabled });
  }
  _sourceFiles(root) {
    const files = []; let total = 0, entries = 0;
    const scan = (dir, depth) => {
      if (depth > 16) throw new Error('plugin directory is too deep');
      for (const item of fs.readdirSync(dir, { withFileTypes: true })) {
        if (++entries > LIMITS.installFiles) throw new Error('plugin package exceeds 512 entries');
        const full = path.join(dir, item.name);
        if (item.isSymbolicLink()) throw new Error('plugin packages cannot contain symbolic links');
        if (item.isDirectory()) scan(full, depth + 1);
        else if (item.isFile()) {
          total += fs.statSync(full).size; files.push(path.relative(root, full));
          if (total > LIMITS.installBytes || files.length > LIMITS.installFiles) throw new Error('plugin package exceeds 16 MiB or 512 files');
        } else throw new Error('plugin package contains an unsupported file');
      }
    };
    scan(root, 0); return files;
  }
  _packageDigest(root) {
    const digest = createHash('sha256');
    for (const relative of this._sourceFiles(root).sort()) {
      const filename = path.join(root, relative);
      if (fs.lstatSync(filename).isSymbolicLink() || !within(root, fs.realpathSync(filename))) throw new Error('plugin files must stay within their directory');
      const bytes = readLimited(filename, LIMITS.installBytes);
      const fileHash = createHash('sha256').update(bytes).digest('hex');
      digest.update(JSON.stringify([relative.split(path.sep).join('/'), bytes.length, fileHash]) + '\n');
    }
    return digest.digest('hex');
  }
  async _activate(rec) {
    await this._stop(rec); rec.status = 'loading'; rec.error = undefined;
    let runtime;
    try {
      this._checkVersion(rec.manifest);
      const filename = resolvePluginEntry(rec.dir, rec.manifest.entry), source = readLimited(filename, LIMITS.source).toString('utf8');
      const data = storageData(path.join(this._storageDir, `${rec.manifest.id}.json`));
      const worker = new Worker(path.join(__dirname, 'plugin-worker.cjs'), {
        workerData: { manifest: rec.manifest, filename, source, activationTimeout: this._timeouts.activation },
        resourceLimits: { maxOldGenerationSizeMb: 64, maxYoungGenerationSizeMb: 16, stackSizeMb: 4 },
      });
      runtime = { worker, rec, waiting: new Map(), sequence: 0, lastBeat: Date.now(), dead: false, stopping: false, data,
        panels: new Set(), satellites: new Set(), budgetAt: Date.now(), budget: 0 };
      rec.runtime = runtime;
      worker.on('message', message => this._message(runtime, message));
      worker.on('error', error => { void this._fail(rec, runtime, error); });
      worker.on('exit', code => { if (!runtime.dead && !runtime.stopping) void this._fail(rec, runtime, new Error(`plugin worker exited (${code})`)); });
      worker.unref();
      runtime.heartbeat = setInterval(() => {
        if (rec.status === 'ok' && !runtime.stopping && Date.now() - runtime.lastBeat > this._timeouts.heartbeat) void this._fail(rec, runtime, new Error('plugin stopped responding'));
      }, Math.min(1000, this._timeouts.heartbeat));
      runtime.heartbeat.unref();
      const capabilities = await this._call(runtime, 'activate', [], this._timeouts.activation);
      if (rec.runtime !== runtime || runtime.dead) throw new Error('plugin stopped during activation');
      this._setCapabilities(runtime, capabilities);
      rec.status = 'ok';
    } catch (error) {
      if (runtime) await this._fail(rec, runtime, error);
      else { rec.status = 'error'; rec.error = text(error.message, 2048); }
    }
  }
  _call(runtime, method, args, timeout) {
    if (runtime.dead || runtime.waiting.size >= LIMITS.pending) return Promise.reject(new Error('plugin callback queue is unavailable'));
    const safeArgs = jsonCopy(args);
    return new Promise((resolve, reject) => {
      const requestId = ++runtime.sequence;
      const timer = setTimeout(() => {
        runtime.waiting.delete(requestId); reject(new Error(`${method} timed out (${timeout} ms)`));
      }, timeout);
      runtime.waiting.set(requestId, { resolve, reject, timer });
      try { runtime.worker.postMessage({ type: 'call', requestId, method, args: safeArgs }); }
      catch (error) { clearTimeout(timer); runtime.waiting.delete(requestId); reject(error); }
    });
  }
  _message(runtime, message) {
    if (runtime.dead || runtime.rec.runtime !== runtime || !message || typeof message !== 'object') return;
    if (message.type === 'heartbeat') { runtime.lastBeat = Date.now(); return; }
    if (message.type === 'capabilities') {
      try { this._setCapabilities(runtime, message.value); } catch (error) { void this._fail(runtime.rec, runtime, error); }
      return;
    }
    if (message.type === 'result') {
      const request = runtime.waiting.get(message.requestId); if (!request) return;
      runtime.waiting.delete(message.requestId); clearTimeout(request.timer);
      if (message.error) request.reject(new Error(text(message.error, 2048)));
      else { try { request.resolve(jsonCopy(message.value)); } catch (error) { request.reject(error); } }
      return;
    }
    if (message.type === 'fatal') { void this._fail(runtime.rec, runtime, new Error(text(message.error, 2048))); return; }
    if (Date.now() - runtime.budgetAt >= 1000) { runtime.budgetAt = Date.now(); runtime.budget = 0; }
    if (++runtime.budget > 120) { void this._fail(runtime.rec, runtime, new Error('plugin host message rate exceeded')); return; }
    if (message.type === 'log') {
      this.emit('log', { pluginId: runtime.rec.manifest.id, level: text(message.level, 12), message: text(message.message, 2048) }); return;
    }
    if (message.type !== 'host') return;
    Promise.resolve().then(() => this._hostCall(runtime, message.operation, message.args)).then(value => {
      if (!runtime.dead) runtime.worker.postMessage({ type: 'hostResult', requestId: message.requestId, value });
    }).catch(error => {
      if (!runtime.dead) runtime.worker.postMessage({ type: 'hostResult', requestId: message.requestId, error: text(error.message, 2048) });
      if (!message.wait) void this._fail(runtime.rec, runtime, error);
    });
  }
  async _hostCall(runtime, operation, args) {
    if (runtime.dead || runtime.rec.runtime !== runtime) throw new Error('plugin has stopped');
    const rec = runtime.rec, id = rec.manifest.id;
    const permission = { registerPanel: 'ui.panel', openPanel: 'ui.panel', closePanel: 'ui.panel', addSatellite: 'ui.satellite', sendNotification: 'notify' }[operation];
    if (permission && !rec.manifest.permissions.includes(permission)) throw new Error(`missing permission ${permission}`);
    if (!Array.isArray(args)) throw new Error('invalid plugin host arguments');
    if (operation.startsWith('storage.')) {
      if (!rec.manifest.permissions.includes('storage')) throw new Error('missing permission storage');
      const key = args[0];
      if (operation !== 'storage.clear' && (typeof key !== 'string' || !key || key.length > 128)) throw new Error('invalid storage key');
      if (operation === 'storage.get') return runtime.data[key];
      const next = Object.assign(Object.create(null), runtime.data);
      if (operation === 'storage.set') next[key] = jsonCopy(args[1], LIMITS.storage);
      else if (operation === 'storage.delete') delete next[key];
      else if (operation === 'storage.clear') for (const existing of Object.keys(next)) delete next[existing];
      else throw new Error('unknown storage operation');
      if (Object.keys(next).length > LIMITS.storageKeys) throw new Error('plugin storage has too many keys');
      jsonCopy(next, LIMITS.storage - 1);
      atomicJSON(path.join(this._storageDir, `${id}.json`), next);
      runtime.data = next; return null;
    }
    if (runtime.stopping) throw new Error('plugin is stopping');
    if (operation === 'registerPanel') {
      if (!safeID(args[0])) throw new Error('invalid panel id');
      if (!runtime.panels.has(args[0]) && runtime.panels.size >= LIMITS.panels) throw new Error('too many panels');
      runtime.panels.add(args[0]); return this._host.registerPanel?.(id, args[0], panelConfig(args[1]));
    }
    if (operation === 'openPanel' || operation === 'closePanel') {
      if (!runtime.panels.has(args[0])) throw new Error('plugin panel is not registered');
      return this._host[operation]?.(id, args[0]);
    }
    if (operation === 'addSatellite') {
      if (!safeID(args[0])) throw new Error('invalid satellite id');
      if (!runtime.satellites.has(args[0]) && runtime.satellites.size >= LIMITS.satellites) throw new Error('too many satellite entries');
      runtime.satellites.add(args[0]); return this._host.addSatellite?.(id, args[0], satelliteConfig(args[1]));
    }
    if (operation === 'showToast') return this._host.showToast?.(id, text(args[0], 256), { type: ['ok', 'bad', 'warn'].includes(args[1]?.type) ? args[1].type : '' });
    if (operation === 'sendNotification') return this._host.sendNotification?.(id, text(args[0], 128), text(args[1], 512));
    throw new Error('unknown plugin host operation');
  }
  _setCapabilities(runtime, capabilities) {
    if (runtime.dead || runtime.stopping) return;
    if (!capabilities || typeof capabilities !== 'object') throw new Error('invalid plugin capabilities');
    const id = runtime.rec.manifest.id;
    this._removeHooks(id);
    for (const phase of ['request', 'response']) if (capabilities[phase]) {
      if (!Number.isInteger(capabilities[phase]) || capabilities[phase] > LIMITS.callbacks) throw new Error('too many plugin hooks');
      this._hooks[phase].push({ pluginId: id });
    }
    for (const group of ['apiEvents', 'sessionEvents']) {
      const names = capabilities[group];
      if (!Array.isArray(names) || names.length > LIMITS.callbacks) throw new Error('invalid plugin subscriptions');
      for (const name of names) {
        if (typeof name !== 'string' || !name || name.length > 80 || group === 'sessionEvents' && !EVENTS.has(name)) throw new Error('invalid plugin subscription');
        (this._hooks[group][name] ||= []).push({ pluginId: id });
      }
    }
  }
  _removeHooks(id) {
    for (const phase of ['request', 'response']) this._hooks[phase] = this._hooks[phase].filter(h => h.pluginId !== id);
    for (const group of ['apiEvents', 'sessionEvents']) for (const key of Object.keys(this._hooks[group])) {
      this._hooks[group][key] = this._hooks[group][key].filter(h => h.pluginId !== id);
      if (!this._hooks[group][key].length) delete this._hooks[group][key];
    }
  }
  _detach(runtime) {
    if (runtime.dead) return;
    runtime.dead = true; clearInterval(runtime.heartbeat);
    this._removeHooks(runtime.rec.manifest.id);
    for (const request of runtime.waiting.values()) { clearTimeout(request.timer); request.reject(new Error('plugin has stopped')); }
    runtime.waiting.clear();
    if (runtime.rec.runtime === runtime) runtime.rec.runtime = null;
    try { this._host.removePlugin?.(runtime.rec.manifest.id); } catch { /* lifecycle cleanup cannot retain worker callbacks */ }
  }
  async _fail(rec, runtime, error) {
    if (runtime.dead || rec.runtime !== runtime) return;
    this._detach(runtime); rec.status = 'error'; rec.error = text(error?.message || String(error), 2048); this._changed();
    await runtime.worker.terminate().catch(() => {});
  }
  async _stop(rec) {
    const runtime = rec.runtime;
    this._removeHooks(rec.manifest.id);
    if (!runtime || runtime.dead) { try { this._host.removePlugin?.(rec.manifest.id); } catch {} return; }
    if (runtime.stopPromise) return runtime.stopPromise;
    runtime.stopping = true;
    runtime.stopPromise = (async () => {
      try { this._host.removePlugin?.(rec.manifest.id); } catch {}
      try { await this._call(runtime, 'deactivate', [], this._timeouts.deactivate); } catch { /* termination guarantees cleanup even when deactivate hangs */ }
      this._detach(runtime);
      await runtime.worker.terminate().catch(() => {});
    })();
    return runtime.stopPromise;
  }
}
module.exports = { PluginLoader, validateManifest };
