'use strict';

const vm = require('node:vm');
const { parentPort, workerData } = require('node:worker_threads');
const { LIMITS, EVENTS, safeID, text, jsonCopy, panelConfig, satelliteConfig } = require('./plugin-protocol.cjs');
const { manifest, source, filename } = workerData;
const hooks = { request: [], response: [], apiEvents: new Map(), sessionEvents: new Map() };
const panels = new Map(), satellites = new Map(), timers = new Map(), waiting = new Map();
let nextHost = 0, nextTimer = 0, callbackCount = 0, stopped = false, pluginModule;

function send(value) { if (!stopped) parentPort.postMessage(value); }
function fatal(error) {
  send({ type: 'fatal', error: text(error?.message || String(error), 2048) });
  clearPluginTimers();
  stopped = true;
}
function requirePerm(permission) {
  if (stopped) throw new Error('plugin has stopped');
  if (!manifest.permissions.includes(permission)) throw new Error(`plugin "${manifest.id}" requires permission "${permission}"`);
}
function host(operation, args, wait = false) {
  if (stopped) return wait ? Promise.reject(new Error('plugin has stopped')) : undefined;
  if (waiting.size >= LIMITS.pending) throw new Error('too many outstanding host calls');
  const requestId = ++nextHost;
  const message = { type: 'host', requestId, operation, args: jsonCopy(args), wait };
  if (!wait) { send(message); return undefined; }
  return new Promise((resolve, reject) => { waiting.set(requestId, { resolve, reject }); send(message); });
}
function validateKey(key) {
  if (typeof key !== 'string' || !key || key.length > 128) throw new TypeError('storage key must be a non-empty string of at most 128 characters');
}
function addCallback(list, fn) {
  if (typeof fn !== 'function') throw new TypeError('callback must be a function');
  if (callbackCount >= LIMITS.callbacks) throw new Error('too many plugin callbacks');
  list.push(fn); callbackCount++;
  send({ type: 'capabilities', value: capabilities() });
}
function capabilities() {
  return { request: hooks.request.length, response: hooks.response.length, apiEvents: [...hooks.apiEvents.keys()], sessionEvents: [...hooks.sessionEvents.keys()] };
}
function updatePanel(id, opts) {
  const current = panels.get(id);
  const merged = { ...(current?.options || {}), ...opts };
  const config = panelConfig(merged), handlers = new Map();
  for (const action of merged.actions || []) {
    if (typeof action.onClick !== 'function') throw new TypeError('panel actions require an onClick function');
    handlers.set(action.id, action.onClick);
  }
  panels.set(id, { options: merged, handlers });
  host('registerPanel', [id, config]);
}
const api = Object.freeze({
  ui: Object.freeze({
    registerPanel(id, opts = {}) {
      requirePerm('ui.panel');
      if (!safeID(id)) throw new TypeError('panelId must be a safe id');
      if (!panels.has(id) && panels.size >= LIMITS.panels) throw new Error('too many plugin panels');
      updatePanel(id, opts);
      return Object.freeze({
        open() { requirePerm('ui.panel'); host('openPanel', [id]); },
        close() { requirePerm('ui.panel'); host('closePanel', [id]); },
        update(next) { requirePerm('ui.panel'); updatePanel(id, next); },
      });
    },
    addSatellite(id, opts = {}) {
      requirePerm('ui.satellite');
      if (!safeID(id)) throw new TypeError('satellite id must be a safe id');
      if (!satellites.has(id) && satellites.size >= LIMITS.satellites) throw new Error('too many plugin satellite entries');
      if (opts.onClick !== undefined && typeof opts.onClick !== 'function') throw new TypeError('onClick must be a function');
      const config = satelliteConfig(opts);
      if (!opts.onClick && !config.panelId) throw new Error('a satellite needs panelId or onClick');
      satellites.set(id, { ...config, onClick: opts.onClick });
      host('addSatellite', [id, config]);
    },
    toast(message, opts = {}) {
      if (typeof message !== 'string') throw new TypeError('message must be a string');
      host('showToast', [message.slice(0, 256), { type: ['ok', 'bad', 'warn'].includes(opts.type) ? opts.type : '' }]);
    },
  }),
  api: Object.freeze({
    intercept(phase, fn) {
      requirePerm('api.intercept');
      if (phase !== 'request' && phase !== 'response') throw new TypeError('phase must be request or response');
      addCallback(hooks[phase], fn);
    },
    on(event, fn) {
      requirePerm('api.intercept');
      if (typeof event !== 'string' || !event || event.length > 80) throw new TypeError('invalid API event');
      if (!hooks.apiEvents.has(event)) hooks.apiEvents.set(event, []);
      addCallback(hooks.apiEvents.get(event), fn);
    },
  }),
  storage: Object.freeze({
    get(key) { requirePerm('storage'); validateKey(key); return host('storage.get', [key], true); },
    set(key, value) { requirePerm('storage'); validateKey(key); return host('storage.set', [key, jsonCopy(value, LIMITS.storage)], true); },
    delete(key) { requirePerm('storage'); validateKey(key); return host('storage.delete', [key], true); },
    clear() { requirePerm('storage'); return host('storage.clear', [], true); },
  }),
  events: Object.freeze({
    on(event, fn) {
      requirePerm('events');
      if (!EVENTS.has(event)) throw new TypeError(`unknown event "${event}"`);
      if (!hooks.sessionEvents.has(event)) hooks.sessionEvents.set(event, []);
      addCallback(hooks.sessionEvents.get(event), fn);
    },
  }),
  notify: Object.freeze({
    send(title, body) { requirePerm('notify'); host('sendNotification', [String(title).slice(0, 128), String(body).slice(0, 512)]); },
  }),
  id: Object.freeze({ id: manifest.id, name: manifest.name, version: manifest.version }),
});

function schedule(fn, delay, repeat, args) {
  if (typeof fn !== 'function') throw new TypeError('timer callback must be a function');
  if (stopped) throw new Error('plugin has stopped');
  if (timers.size >= LIMITS.timers) throw new Error('too many plugin timers');
  const id = ++nextTimer;
  let busy = false;
  const tick = () => {
    if (stopped || busy) return;
    if (!repeat) timers.delete(id);
    busy = true;
    Promise.resolve().then(() => fn(...args)).catch(fatal).finally(() => { busy = false; });
  };
  const ms = Math.min(86400000, Math.max(repeat ? 500 : 0, Number(delay) || 0));
  timers.set(id, repeat ? setInterval(tick, ms) : setTimeout(tick, ms));
  return id;
}
function clearTimer(id) { const timer = timers.get(id); if (timer) { clearTimeout(timer); clearInterval(timer); timers.delete(id); } }
function clearPluginTimers() { for (const id of timers.keys()) clearTimer(id); }

async function chain(handlers, input, extra) {
  let position = -1;
  const next = async (value, i = 0) => {
    if (i <= position) throw new Error('next() may only be called once per hook');
    position = i;
    if (i >= handlers.length) return value;
    let advanced;
    const result = await handlers[i](value, updated => {
      if (advanced) throw new Error('next() may only be called once per hook');
      advanced = next(updated === undefined ? value : updated, i + 1);
      return advanced;
    }, extra);
    return result === undefined ? advanced ? await advanced : value : result;
  };
  return next(input);
}
async function dispatch(method, args) {
  if (method === 'activate') {
    const module = { exports: {} };
    const context = vm.createContext({
      module, exports: module.exports, salcara: api,
      console: Object.freeze(Object.fromEntries(['log', 'warn', 'error'].map(level => [level, (...values) => {
        send({ type: 'log', level, message: values.map(v => String(v).slice(0, 512)).slice(0, 8).join(' ').slice(0, 2048) });
      }]))),
      setTimeout: (fn, ms, ...values) => schedule(fn, ms, false, values), clearTimeout: clearTimer,
      setInterval: (fn, ms, ...values) => schedule(fn, ms, true, values), clearInterval: clearTimer,
    });
    new vm.Script(source, { filename }).runInContext(context, { timeout: workerData.activationTimeout });
    pluginModule = module.exports;
    if (!pluginModule || typeof pluginModule.activate !== 'function') throw new Error('plugin must export activate(salcara)');
    await pluginModule.activate(api);
    return capabilities();
  }
  if (method === 'deactivate') {
    clearPluginTimers();
    if (typeof pluginModule?.deactivate === 'function') await pluginModule.deactivate();
    clearPluginTimers();
    return null;
  }
  if (method === 'request') return chain(hooks.request, args[0]);
  if (method === 'response') return chain(hooks.response, args[1], args[0]);
  if (method === 'sessionEvent' || method === 'apiEvent') {
    const map = method === 'sessionEvent' ? hooks.sessionEvents : hooks.apiEvents;
    for (const handler of map.get(args[0]) || []) await handler(args[1]);
    return null;
  }
  if (method === 'action') {
    const [kind, target, action] = args;
    if (kind === 'panel') {
      const fn = panels.get(target)?.handlers.get(action);
      if (!fn) throw new Error('plugin action is unavailable');
      await fn();
    } else if (kind === 'satellite') {
      const satellite = satellites.get(target);
      if (!satellite) throw new Error('plugin entry is unavailable');
      if (satellite.onClick) await satellite.onClick();
      else host('openPanel', [satellite.panelId]);
    } else throw new Error('unknown UI action kind');
    return null;
  }
  throw new Error('unknown plugin operation');
}
let queue = Promise.resolve(), queued = 0;
parentPort.on('message', message => {
  if (message.type === 'hostResult') {
    const promise = waiting.get(message.requestId);
    if (!promise) { if (message.error) fatal(new Error(message.error)); return; }
    waiting.delete(message.requestId);
    if (message.error) promise.reject(new Error(message.error)); else promise.resolve(message.value);
    return;
  }
  if (message.type !== 'call' || stopped) return;
  if (++queued > LIMITS.pending) { fatal(new Error('plugin callback queue is full')); return; }
  queue = queue.then(async () => {
    if (stopped) return;
    try {
      const result = await dispatch(message.method, message.args || []);
      send({ type: 'result', requestId: message.requestId, value: jsonCopy(result === undefined ? null : result) });
    } catch (error) { send({ type: 'result', requestId: message.requestId, error: text(error?.message || String(error), 2048) }); }
  }).finally(() => { queued--; });
});
const heartbeat = setInterval(() => send({ type: 'heartbeat' }), 400);
heartbeat.unref();
process.on('unhandledRejection', fatal);
process.on('uncaughtException', fatal);
