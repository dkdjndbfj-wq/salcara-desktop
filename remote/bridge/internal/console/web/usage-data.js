'use strict';
/* Usage data shared by the 用量 page and the floating ball.
   /api/usage/overview returns one entry per source (the relay account and each
   saved API). Relay payloads differ a little between sub2api versions, so every
   field is read defensively. */

const USAGE = (() => {
  const n = (v) => (typeof v === 'number' && isFinite(v) ? v : (typeof v === 'string' && v.trim() !== '' && isFinite(+v) ? +v : null));
  const first = (...vals) => { for (const v of vals) { const x = n(v); if (x !== null) return x; } return null; };
  const tokens = (d) => {
    for (const key of ['total_tokens', 'tokens', 'totalTokens']) {
      if (d[key] !== undefined) return n(d[key]);
    }
    const input = n(d.input_tokens), output = n(d.output_tokens);
    // Cache/reasoning counts can already be included in input/output. A
    // partial base count is not a total, and an explicit null stays unknown.
    return input !== null && output !== null ? input + output : null;
  };
  const add = (a, b) => a === null || b === null ? null : a + b;
  const pick = (u, ...keys) => {
    for (const k of keys) {
      const v = k.split('.').reduce((o, p) => (o && typeof o === 'object' ? o[p] : undefined), u);
      if (v !== undefined && v !== null) return v;
    }
    return undefined;
  };
  const dayKey = (t) => `${t.getFullYear()}-${String(t.getMonth() + 1).padStart(2, '0')}-${String(t.getDate()).padStart(2, '0')}`;

  function daily(u) {
    const arr = pick(u, 'daily_usage', 'daily', 'trend', 'usage.daily', 'usage.daily_usage', 'usage.trend');
    if (!Array.isArray(arr)) return [];
    return arr.filter((d) => d && typeof d === 'object').map((d) => ({ date: String(d.date || d.day || d.time || '').slice(0, 10), tokens: tokens(d), requests: n(d.requests) }))
      .filter((d) => /^\d{4}-\d{2}-\d{2}$/.test(d.date));
  }
  function models(u) {
    const arr = pick(u, 'model_stats', 'models', 'usage.model_stats', 'usage.models');
    if (!Array.isArray(arr)) return [];
    return arr.filter((d) => d && typeof d === 'object').map((d) => ({ model: String(d.model || d.name || '未知模型'), tokens: tokens(d), requests: n(d.requests) }));
  }
  function today(u) {
    const t = pick(u, 'usage.today', 'today');
    if (!t || typeof t !== 'object') return null;
    return { tokens: tokens(t), requests: n(t.requests) };
  }
  function remaining(u) {
    return { value: first(u.remaining, u.balance, u.quota && u.quota.remaining), unit: String(u.unit || (u.quota && u.quota.unit) || 'USD') };
  }

  /**
   * Merge every readable source into one picture for the last `days` days.
   * Relay / API sources report upstream activity. The "local" source (Codex and
   * Claude Code records on this computer) counts the same tokens again, so it
   * is used only where the relays report nothing (no daily curve, no models,
   * no today) and is never added on top of them.
   */
  function summarize(resp) {
    const days = Math.max(1, (resp && resp.days) || 14);
    const selectedSource = String(resp && resp.selectedSource || '');
    const options = Array.isArray(resp && resp.options) ? resp.options : ((resp && resp.sources) || []).filter((s) => s.kind !== 'local');
    const axis = [];
    const end = /^\d{4}-\d{2}-\d{2}$/.test(resp && resp.rangeEnd || '') ? new Date(resp.rangeEnd + 'T12:00:00') : new Date();
    for (let i = days - 1; i >= 0; i--) { const t = new Date(end); t.setDate(end.getDate() - i); axis.push(dayKey(t)); }
    const dates = new Set(axis);
    const inRange = (u) => u ? daily(u).filter((row) => dates.has(row.date)) : [];
    // An account view must never consume the unbound, computer-wide scanner.
    const sources = ((resp && resp.sources) || []).filter((s) => !selectedSource || s.id === selectedSource && s.kind !== 'local').map((s) => {
      const u = s.ok && s.usage ? s.usage : null;
      const local = selectedSource && s.localUsage ? s.localUsage : null;
      const upstreamDaily = inRange(u), upstreamModels = u ? models(u) : [], upstreamToday = u ? today(u) : null;
      const localDaily = inRange(local), localModels = local ? models(local) : [], localToday = local ? today(local) : null;
      return { ...s, ok: s.ok || Boolean(local),
        daily: upstreamDaily.length ? upstreamDaily : localDaily,
        models: upstreamModels.length ? upstreamModels : localModels,
        today: upstreamToday || localToday,
        dailyLocal: !upstreamDaily.length && localDaily.length > 0,
        modelsLocal: !upstreamModels.length && localModels.length > 0,
        todayLocal: !upstreamToday && Boolean(localToday),
        partial: Boolean(u && u.partial || local && local.partial && (!upstreamDaily.length || !upstreamModels.length || !upstreamToday)),
        remaining: u ? remaining(u) : { value: null, unit: 'USD' } };
    });
    const collect = (list) => {
      const byDay = new Map(axis.map((d) => [d, { date: d, tokens: 0, requests: 0 }]));
      const byModel = new Map();
      let hasDaily = false;
      for (const s of list) {
        let rangeTokens = 0;
        for (const d of s.daily) {
          const slot = byDay.get(d.date);
          if (!slot) continue;
          hasDaily = true;
          slot.tokens = add(slot.tokens, d.tokens); slot.requests = add(slot.requests, d.requests);
          rangeTokens = add(rangeTokens, d.tokens);
        }
        s.range = { tokens: s.daily.length ? rangeTokens : null, known: s.daily.length > 0 && rangeTokens !== null };
        for (const m of s.models) {
          const slot = byModel.get(m.model) || { model: m.model, tokens: 0, requests: 0 };
          slot.tokens = add(slot.tokens, m.tokens); slot.requests = add(slot.requests, m.requests);
          byModel.set(m.model, slot);
        }
      }
      const todays = list.map((s) => s.today).filter(Boolean);
      return {
        series: [...byDay.values()], hasDaily,
        models: [...byModel.values()].filter((m) => m.tokens === null || m.tokens > 0 || m.requests > 0).sort((a, b) => (b.tokens || 0) - (a.tokens || 0)),
        today: todays.length ? todays.reduce((a, t) => ({ tokens: add(a.tokens, t.tokens), requests: add(a.requests, t.requests) }), { tokens: 0, requests: 0 }) : null,
      };
    };
    const remote = sources.filter((s) => s.kind !== 'local');
    const localList = sources.filter((s) => s.kind === 'local');
    const r = collect(remote), l = collect(localList);
    const curveLocal = !r.hasDaily && l.hasDaily;
    const modelsLocal = !r.models.length && l.models.length > 0;
    const todayLocal = !r.today && Boolean(l.today);
    const series = curveLocal ? l.series : r.series;
    const relay = remote.find((s) => s.kind === 'relay' && s.ok && s.remaining.value !== null) || remote.find((s) => s.ok && s.remaining.value !== null);
    return {
      days, selectedSource, options, partial: sources.some((s) => s.partial),
      sources,
      series,
      hasDaily: r.hasDaily || l.hasDaily,
      curveLocal: curveLocal || remote.some((s) => s.dailyLocal),
      modelsLocal: modelsLocal || remote.some((s) => s.modelsLocal),
      todayLocal: todayLocal || remote.some((s) => s.todayLocal),
      models: modelsLocal ? l.models : r.models,
      today: todayLocal ? l.today : r.today,
      rangeTokens: r.hasDaily || l.hasDaily ? series.reduce((a, d) => add(a, d.tokens), 0) : null,
      balance: relay && relay.remaining.value !== null ? { ...relay.remaining, name: relay.name, host: relay.host } : null,
    };
  }

  function fmtTokens(v) {
    const x = n(v);
    if (x === null) return '—';
    if (x >= 1e9) return (x / 1e9).toFixed(2) + 'B';
    if (x >= 1e6) return (x / 1e6).toFixed(2) + 'M';
    if (x >= 1e3) return (x / 1e3).toFixed(1) + 'K';
    return String(Math.round(x));
  }
  function fmtMoney(v, unit) {
    const x = n(v);
    if (x === null) return '—';
    const u = String(unit || 'USD').trim().toUpperCase() || 'USD';
    const s = Math.abs(x) >= 1 ? x.toFixed(2) : x === 0 ? '0.00' : x.toFixed(3);
    return u === 'USD' ? '$' + s : s + ' ' + u;
  }

  async function load(days, refresh, source = '', signal) {
    const res = await fetch(`/api/usage/overview?days=${days}${refresh ? '&refresh=1' : ''}${source ? '&source=' + encodeURIComponent(source) : ''}`, { credentials: 'same-origin', signal });
    const body = await res.json().catch(() => null);
    if (!res.ok) {
      const error = new Error((body && body.error) || `请求失败 (${res.status})`);
      if (Array.isArray(body && body.options)) error.options = body.options;
      throw error;
    }
    if (!body || !Array.isArray(body.sources) || String(body.selectedSource || '') !== source || source && !body.sources.some((s) => s.id === source && s.kind !== 'local')) {
      throw new Error('返回的用量来源与当前选择不一致');
    }
    if (Number(body.days) !== Number(days)) throw new Error('返回的用量时间范围与当前选择不一致');
    return summarize(body);
  }

  return { load, summarize, fmtTokens, fmtMoney };
})();
