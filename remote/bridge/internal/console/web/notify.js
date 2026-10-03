'use strict';
/* System notifications from the desktop console: an operation waits for your
   approval, or a task started from the phone / this computer finished. Only
   shown when the window is not in front, and can be turned off in 设置. */

const NOTIFY = (() => {
  const KEY = 'salcara.notify';
  const enabled = () => { try { return localStorage.getItem(KEY) !== 'off'; } catch (_) { return true; } };
  const setEnabled = (on) => { try { localStorage.setItem(KEY, on ? 'on' : 'off'); } catch (_) { /* optional */ } };
  const statuses = new Map();
  const shown = new Set();

  async function inFront() {
    if (document.hidden) return false;
    if (window.salcaraWindow && window.salcaraWindow.state) {
      try { const s = await window.salcaraWindow.state(); return Boolean(s && s.focused); } catch (_) { return false; }
    }
    return document.hasFocus();
  }

  async function post(title, body, route, once) {
    if (!enabled() || (once && shown.has(once)) || await inFront()) return;
    if (once) { shown.add(once); if (shown.size > 500) shown.clear(); }
    if (window.salcaraWindow && window.salcaraWindow.notify) { window.salcaraWindow.notify(title, body, route); return; }
    if (!('Notification' in window)) return;
    if (Notification.permission === 'default') { try { await Notification.requestPermission(); } catch (_) { return; } }
    if (Notification.permission !== 'granted') return;
    const n = new Notification(title, { body, icon: 'logo.svg' });
    n.onclick = () => { window.focus(); if (route) location.hash = '#' + route; n.close(); };
  }

  const toolName = (t) => (t === 'codex' ? 'Codex' : t === 'claude' ? 'Claude Code' : 'Agent');

  function onEvent(ev) {
    if (!ev || typeof ev !== 'object') return;
    if (ev.type === 'approval.request') {
      void post('需要你批准', `${toolName(ev.tool)}：${String(ev.title || '一个操作').slice(0, 120)}`, 'sessions', 'ap:' + ev.approvalId);
      return;
    }
    if (ev.type === 'session.updated' && ev.session) {
      const s = ev.session, key = s.sessionKey;
      const before = statuses.get(key);
      statuses.set(key, s.status);
      if (statuses.size > 2000) statuses.clear();
      const wasBusy = before === 'running' || before === 'waiting_approval';
      if (!wasBusy) return;
      const title = String(s.title || '对话').slice(0, 80);
      if (s.status === 'idle') void post(`${toolName(s.tool)} 完成了任务`, title, 'sessions');
      else if (s.status === 'failed') void post(`${toolName(s.tool)} 任务出错`, title, 'sessions');
    }
  }

  function connect() {
    let es;
    try { es = new EventSource('/api/stream'); } catch (_) { return; }
    es.addEventListener('event', (m) => { try { onEvent(JSON.parse(m.data)); } catch (_) { /* ignore */ } });
  }
  connect();
  return { enabled, setEnabled, onEvent };
})();
