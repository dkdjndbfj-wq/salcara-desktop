'use strict';

// Renewable desktop capability: attaching to an existing core must replace the
// old process's hook endpoint, without putting a token in any renderer or file.
function createCoreLink({ consoleURL, hookPort, token, onState = () => {}, fetchImpl = fetch, intervalMs = 30000 }) {
  let timer = null, cookie = '', stopped = true, closed = false, running = null, controller = null, failedAt = 0;
  const base = new URL(consoleURL);
  if (base.hostname !== '127.0.0.1' || base.protocol !== 'http:') throw new Error('Plugin core link must use loopback HTTP');
  async function request(action, signal) {
    if (!cookie) {
      const response = await fetchImpl(base.href, { signal, redirect: 'error' });
      const cookies = response.headers.getSetCookie?.() || [response.headers.get('set-cookie') || ''];
      cookie = cookies.map(value => value.split(';')[0]).find(value => value.startsWith('salcara_console=')) || '';
      await response.body?.cancel();
      if (!cookie) throw new Error('无法取得本机连接凭据');
    }
    const response = await fetchImpl(new URL('api/local/plugins/' + action, base), {
      method: 'POST', signal, redirect: 'error', headers: { 'Content-Type': 'application/json', Origin: base.origin, Cookie: cookie },
      body: JSON.stringify(action === 'connect' ? { port: hookPort, token } : { token }),
    });
    if (!response.ok) {
      if (response.status === 401 || response.status === 403) cookie = '';
      await response.body?.cancel();
      throw new Error(response.status === 404 ? '当前核心版本不支持插件业务连接' : '本机插件连接暂不可用');
    }
    await response.body?.cancel();
  }
  function connect() {
    if (stopped) return Promise.resolve();
    if (running) return running;
    controller = new AbortController();
    const deadline = setTimeout(() => controller?.abort(), 4000);
    running = request('connect', controller.signal).then(() => {
      failedAt = 0; if (!stopped) onState({ state: 'connected' });
    }).catch(error => {
      if (!failedAt) failedAt = Date.now();
      if (!stopped && Date.now() - failedAt >= 10000) onState({ state: 'offline', error: error.message });
    }).finally(() => {
      clearTimeout(deadline); running = null; controller = null;
      if (!stopped) { timer = setTimeout(connect, failedAt ? 5000 : intervalMs); timer.unref?.(); }
    });
    return running;
  }
  return {
    start() { if (closed) return Promise.resolve(); if (!stopped) return running || Promise.resolve(); stopped = false; return connect(); },
    async stop() {
      closed = true; stopped = true; clearTimeout(timer); controller?.abort();
      if (running) await running;
      try { await request('disconnect', AbortSignal.timeout(1500)); } catch { /* the lease expires if the core is unavailable */ }
    },
  };
}
module.exports = { createCoreLink };
