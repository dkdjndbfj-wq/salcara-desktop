'use strict';

// Shared, serialisable plugin contract. Plugins are trusted local code; the worker
// protects app responsiveness and lifetime, not against a hostile OS-level actor.
const LIMITS = Object.freeze({
  payload: 4 * 1024 * 1024, storage: 1024 * 1024, source: 1024 * 1024,
  installBytes: 16 * 1024 * 1024, installFiles: 512, plugins: 32,
  panels: 12, satellites: 8, actions: 12, callbacks: 64, timers: 128,
  pending: 64, storageKeys: 1024,
});
const PERMISSIONS = new Set(['api.intercept', 'ui.panel', 'ui.satellite', 'storage', 'events', 'notify']);
const EVENTS = new Set(['session:start', 'session:end', 'session:update', 'tool:detect']);
const safeID = value => typeof value === 'string' && /^[a-zA-Z0-9_-]{1,64}$/.test(value);
function text(value, max, fallback = '') {
  return typeof value === 'string' ? value.slice(0, max) : fallback;
}
function jsonCopy(value, maxBytes = LIMITS.payload) {
  const raw = JSON.stringify(value);
  if (raw === undefined) throw new TypeError('value must be JSON serialisable');
  if (Buffer.byteLength(raw, 'utf8') > maxBytes) throw new Error('plugin data exceeds the size limit');
  return JSON.parse(raw);
}
function panelConfig(value = {}) {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new TypeError('panel options must be an object');
  const actions = Array.isArray(value.actions) ? value.actions : [];
  if (actions.length > LIMITS.actions) throw new Error('too many panel actions');
  const seen = new Set();
  return {
    label: text(value.label, 80), icon: text(value.icon, 32, 'puzzle'),
    title: text(value.title, 160), content: text(value.content, 32768),
    actions: actions.map(action => {
      if (!action || !safeID(action.id) || seen.has(action.id)) throw new TypeError('panel action ids must be unique safe ids');
      seen.add(action.id);
      return { id: action.id, label: text(action.label, 80, action.id) };
    }),
  };
}
function satelliteConfig(value = {}) {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new TypeError('satellite options must be an object');
  if (value.panelId !== undefined && !safeID(value.panelId)) throw new TypeError('panelId must be a safe id');
  return { label: text(value.label, 80), icon: text(value.icon, 32, 'puzzle'), ...(value.panelId ? { panelId: value.panelId } : {}) };
}
module.exports = { LIMITS, PERMISSIONS, EVENTS, safeID, text, jsonCopy, panelConfig, satelliteConfig };
