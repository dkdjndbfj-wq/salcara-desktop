'use strict';
/* Usage data shared by the 用量 page and the floating ball.
   /api/usage/overview returns one entry per source (the relay account and each
   saved API). Relay payloads differ a little between sub2api versions, so every
   field is read defensively. */

const USAGE = (() => {
  const n = (v) => (typeof v === 'number' && isFinite(v) ? v : (typeof v === 'string' && v.trim() !== '' && isFinite(+v) ? +v : null));
  const first = (...vals) => { for (const v of vals) { const x = n(v); if (x !== null) return x; } return null; };
  const tokens = (d) => first(d.total_tokens, d.tokens, d.totalTokens) ??
    ((n(d.input_tokens) || 0) + (n(d.output_tokens) || 0) + (n(d.cache_creation_tokens) || 0) + (n(d.cache_read_tokens) || 0));
  const cost = (d) => first(d.actual_cost, d.cost, d.total_cost, d.actualCost) || 0;
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
    return arr.map((d) => ({ date: String(d.date || d.day || d.time || '').slice(0, 10), tokens: tokens(d), cost: cost(d), requests: n(d.requests) || 0 }))
      .filter((d) => /^\d{4}-\d{2}-\d{2}$/.test(d.date));
  }
  function models(u) {
    const arr = pick(u, 'model_stats', 'models', 'usage.model_stats', 'usage.models');
    if (!Array.isArray(arr)) return [];
    return arr.map((d) => ({ model: String(d.model || d.name || '未知模型'), tokens: tokens(d), cost: cost(d), requests: n(d.requests) || 0 }));
  }
  function today(u) {
    const t = pick(u, 'usage.today', 'today');
    if (!t || typeof t !== 'object') return null;
    return { tokens: tokens(t), cost: cost(t), requests: n(t.requests) || 0 };
  }
  function remaining(u) {
    return { value: first(u.remaining, u.balance, u.quota && u.quota.remaining), unit: String(u.unit || (u.quota && u.quota.unit) || 'USD') };
  }

  /**
   * Merge every readable source into one picture for the last `days` days.
   * Relay / API sources are the billing truth. The "local" source (Codex and
   * Claude Code records on this computer) counts the same tokens again, so it
   * is used only where the relays report nothing (no daily curve, no models,
   * no today) and is never added on top of them.
   */
  function summarize(resp) {
    const days = Math.max(1, (resp && resp.days) || 14);
    const sources = ((resp && resp.sources) || []).map((s) => {
      const u = s.ok && s.usage ? s.usage : null;
      return { ...s, daily: u ? daily(u) : [], models: u ? models(u) : [], today: u ? today(u) : null, remaining: u ? remaining(u) : { value: null, unit: 'USD' } };
    });
    const axis = [];
    const end = new Date();
    for (let i = days - 1; i >= 0; i--) { const t = new Date(end); t.setDate(end.getDate() - i); axis.push(dayKey(t)); }
    const collect = (list) => {
      const byDay = new Map(axis.map((d) => [d, { date: d, tokens: 0, cost: 0, requests: 0 }]));
      const byModel = new Map();
      let hasDaily = false;
      for (const s of list) {
        let rangeTokens = 0, rangeCost = 0;
        for (const d of s.daily) {
          const slot = byDay.get(d.date);
          if (!slot) continue;
          hasDaily = true;
          slot.tokens += d.tokens; slot.cost += d.cost; slot.requests += d.requests;
          rangeTokens += d.tokens; rangeCost += d.cost;
        }
        s.range = { tokens: rangeTokens, cost: rangeCost, known: s.daily.length > 0 };
        for (const m of s.models) {
          const slot = byModel.get(m.model) || { model: m.model, tokens: 0, cost: 0, requests: 0 };
          slot.tokens += m.tokens; slot.cost += m.cost; slot.requests += m.requests;
          byModel.set(m.model, slot);
        }
      }
      const todays = list.map((s) => s.today).filter(Boolean);
      return {
        series: [...byDay.values()], hasDaily,
        models: [...byModel.values()].filter((m) => m.tokens > 0 || m.cost > 0).sort((a, b) => b.tokens - a.tokens || b.cost - a.cost),
        today: todays.length ? todays.reduce((a, t) => ({ tokens: a.tokens + t.tokens, cost: a.cost + t.cost, requests: a.requests + t.requests }), { tokens: 0, cost: 0, requests: 0 }) : null,
      };
    };
    const remote = sources.filter((s) => s.kind !== 'local');
    const localList = sources.filter((s) => s.kind === 'local');
    const r = collect(remote), l = collect(localList);
    const curveLocal = !r.hasDaily && l.hasDaily;
    const modelsLocal = !r.models.length && l.models.length > 0;
    const todayLocal = !r.today && Boolean(l.today);
    const series = curveLocal ? l.series : r.series;
    const relay = remote.find((s) => s.kind === 'relay' && s.ok) || remote.find((s) => s.ok && s.remaining.value !== null);
    return {
      days,
      sources,
      series,
      hasDaily: r.hasDaily || l.hasDaily,
      curveLocal, modelsLocal, todayLocal,
      models: modelsLocal ? l.models : r.models,
      today: todayLocal ? { ...l.today, cost: null } : r.today,
      rangeCost: r.hasDaily ? r.series.reduce((a, d) => a + d.cost, 0) : null,
      rangeTokens: r.hasDaily ? r.series.reduce((a, d) => a + d.tokens, 0) : l.hasDaily ? l.series.reduce((a, d) => a + d.tokens, 0) : null,
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
    const u = (unit || 'USD').toUpperCase();
    const s = Math.abs(x) >= 100 ? x.toFixed(0) : Math.abs(x) >= 1 ? x.toFixed(2) : x === 0 ? '0.00' : x.toFixed(3);
    return u === 'USD' ? '$' + s : s + ' ' + u;
  }

  async function load(days, refresh) {
    const res = await fetch(`/api/usage/overview?days=${days}${refresh ? '&refresh=1' : ''}`, { credentials: 'same-origin' });
    const body = await res.json().catch(() => null);
    if (!res.ok) throw new Error((body && body.error) || `请求失败 (${res.status})`);
    return summarize(body);
  }

  return { load, summarize, fmtTokens, fmtMoney };
})();
