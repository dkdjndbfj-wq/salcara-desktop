'use strict';
/* One system reminder for a genuinely completed Codex turn with a Chinese
   final answer. Uses the console's existing SSE stream; never infer completion
   from an idle session, tool update, approval, reasoning or history replay. */
const NOTIFY = (() => {
  const KEY = 'salcara.notify';
  const enabled = () => { try { return localStorage.getItem(KEY) !== 'off'; } catch (_) { return true; } };
  const setEnabled = on => { try { localStorage.setItem(KEY, on ? 'on' : 'off'); } catch (_) {} };
  const turns = new Map(), sessions = new Map(), shown = new Set();
  const trim = (map, size) => { while (map.size > size) map.delete(map.keys().next().value); };
  async function inFront() {
    if (document.hidden) return false;
    if (window.salcaraWindow && window.salcaraWindow.state) {
      try { const s = await window.salcaraWindow.state(); return Boolean(s && s.focused); } catch (_) { return false; }
    }
    return document.hasFocus();
  }
  async function post(body, once) {
    // Claim synchronously before checking focus; concurrent terminal updates
    // cannot each pass the asynchronous check and show duplicate reminders.
    if (shown.has(once)) return;
    shown.add(once); trim(shown, 500);
    if (!enabled() || await inFront()) return;
    const title = 'Codex 已完成本轮任务', route = 'sessions';
    if (window.salcaraWindow && window.salcaraWindow.notify) { window.salcaraWindow.notify(title, body, route); return; }
    if (!('Notification' in window)) return;
    if (Notification.permission === 'default') { try { await Notification.requestPermission(); } catch (_) { return; } }
    if (Notification.permission !== 'granted') return;
    const n = new Notification(title, { body, icon: 'logo.svg' });
    n.onclick = () => { window.focus(); location.hash = '#' + route; n.close(); };
  }
  function onEvent(ev) {
    if (!ev || ev.tool !== 'codex' || !ev.sessionKey) return;
    if (ev.type === 'session.updated' && ev.session) {
      sessions.set(ev.sessionKey, { parent: ev.session.parentSessionKey || '' }); trim(sessions, 1000);
      return;
    }
    if (!ev.turnId) return;
    const key = ev.sessionKey + ':' + ev.turnId;
    if (ev.type === 'turn' && ev.status === 'started') {
      if (!shown.has(key) && !turns.has(key)) turns.set(key, { text: '' });
      trim(turns, 256); return;
    }
    const turn = turns.get(key);
    if (!turn) return; // no observed live start: no historical/replayed notice
    if (ev.type === 'message' && ev.role === 'assistant' && ev.final === true) {
      turn.text = String(ev.text || '').trim().slice(0, 2000); return;
    }
    if (ev.type !== 'turn' || !['completed', 'failed', 'interrupted'].includes(ev.status)) return;
    turns.delete(key);
    const session = sessions.get(ev.sessionKey);
    if (ev.status !== 'completed' || ev.error || !session || session.parent || !/[\u3400-\u9fff]/.test(turn.text)) {
      shown.add(key); trim(shown, 500); return;
    }
    void post(turn.text.replace(/\s+/g, ' ').slice(0, 120), key);
  }
  return { enabled, setEnabled, onEvent };
})();
