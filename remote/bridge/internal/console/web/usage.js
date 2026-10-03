'use strict';
/* 用量: balance left on the relay, today's spend, a trend curve, usage by model
   and by saved API. Data comes from USAGE (usage-data.js). */

const USAGE_VIEW = { days: 14, metric: 'tokens', data: null, seq: 0 };

function usageSkeleton() {
  const stat = '<div class="us-stat"><div class="skeleton" style="width:50%"></div><div class="skeleton" style="margin-top:14px;height:26px;width:70%"></div></div>';
  return `<div class="us-stats">${stat.repeat(4)}</div><div class="card us-chart-card"><div class="skeleton" style="height:220px"></div></div>`;
}

function usageHead() {
  $('#headActions').innerHTML = `
    <div class="us-seg" role="tablist" aria-label="时间范围">
      ${[7, 14, 30].map((d) => `<button type="button" role="tab" data-days="${d}" aria-selected="${USAGE_VIEW.days === d}">${d} 天</button>`).join('')}
    </div>
    <button class="btn sm icon-only" type="button" id="usRefresh" aria-label="刷新" title="刷新">${icon('refresh')}</button>`;
  $$('#headActions [data-days]').forEach((b) => b.addEventListener('click', () => {
    USAGE_VIEW.days = Number(b.dataset.days);
    $$('#headActions [data-days]').forEach((x) => x.setAttribute('aria-selected', String(x === b)));
    loadUsage(false);
  }));
  $('#usRefresh').addEventListener('click', () => loadUsage(true));
}

RENDER_usage = async function (view) {
  usageHead();
  if (!USAGE_VIEW.data) view.innerHTML = usageSkeleton();
  else drawUsage(view, USAGE_VIEW.data);
  await loadUsage(false);
};

async function loadUsage(refresh) {
  const seq = ++USAGE_VIEW.seq;
  const btn = $('#usRefresh');
  if (btn) btn.classList.add('spinning');
  try {
    const data = await USAGE.load(USAGE_VIEW.days, refresh);
    if (seq !== USAGE_VIEW.seq || S.route !== 'usage') return;
    USAGE_VIEW.data = data;
    drawUsage($('#view'), data);
  } catch (e) {
    if (seq !== USAGE_VIEW.seq || S.route !== 'usage') return;
    $('#view').innerHTML = `<div class="callout bad">${ico('warn')}<div>${esc(e.message)}</div></div>`;
  } finally {
    if (btn && seq === USAGE_VIEW.seq) btn.classList.remove('spinning');
  }
}

function drawUsage(view, d) {
  if (!d.sources.length) {
    view.innerHTML = `<div class="us-empty">
      <div class="us-empty-ico">${icon('chart')}</div>
      <div class="us-empty-t">还没有可以统计的 API</div>
      <div class="us-empty-d">在「API 密钥」里保存中转站的 Key，或登录中转站后，这里会显示余额、花费和模型用量。</div>
      <a class="btn" href="#accounts">${icon('key')}打开 API 密钥</a></div>`;
    return;
  }
  const bal = d.balance;
  const stats = [
    { k: '中转剩余', v: bal ? USAGE.fmtMoney(bal.value, bal.unit) : '—', f: bal ? esc(bal.host || bal.name) : '中转站未返回余额', main: true },
    { k: '今日 Token', v: d.today ? USAGE.fmtTokens(d.today.tokens) : '—', f: d.today ? `${USAGE.fmtTokens(d.today.requests)} 次请求${d.todayLocal ? ' · 本机记录' : ''}` : '—' },
    { k: '今日花费', v: d.today && d.today.cost !== null ? USAGE.fmtMoney(d.today.cost) : '—', f: d.todayLocal ? '本机记录不含花费' : '按实际扣费' },
    { k: `近 ${d.days} 天花费`, v: d.rangeCost !== null ? USAGE.fmtMoney(d.rangeCost) : '—', f: d.rangeTokens !== null ? `${USAGE.fmtTokens(d.rangeTokens)} tokens${d.curveLocal ? ' · 本机记录' : ''}` : '中转站未返回每日数据' },
  ];
  const maxModel = Math.max(1, ...d.models.map((m) => m.tokens));
  const totalTok = d.sources.filter((s) => s.kind !== 'local').reduce((a, s) => a + (s.range.tokens || 0), 0) || 1;
  view.innerHTML = `
  <div class="us-stats">${stats.map((s) => `<div class="us-stat${s.main ? ' main' : ''}"><div class="us-k">${s.k}</div><div class="us-v">${s.v}</div><div class="us-f">${s.f}</div></div>`).join('')}</div>

  <div class="card us-chart-card">
    <div class="us-card-head">
      <div><div class="us-title">趋势</div><div class="us-sub">近 ${d.days} 天 · ${d.curveLocal ? '来自本机 Codex / Claude Code 记录（中转站未返回每日数据）' : '所有可读取的 API 合计'}</div></div>
      <div class="us-seg small" role="tablist" aria-label="指标">
        <button type="button" data-metric="tokens" aria-selected="${USAGE_VIEW.metric === 'tokens'}">Token</button>
        ${d.curveLocal ? '' : `<button type="button" data-metric="cost" aria-selected="${USAGE_VIEW.metric === 'cost'}">花费</button>`}
      </div>
    </div>
    <div class="us-chart" id="usChart"></div>
  </div>

  <div class="us-cols">
    <div class="card us-list-card">
      <div class="us-card-head"><div><div class="us-title">模型用量</div><div class="us-sub">按 Token 排序${d.modelsLocal ? ' · 本机记录' : ''}</div></div></div>
      ${d.models.length ? `<ol class="us-models">${d.models.slice(0, 8).map((m, i) => `
        <li><div class="us-m-top"><span class="us-m-name" title="${esc(m.model)}">${esc(m.model)}</span><span class="us-m-val">${USAGE.fmtTokens(m.tokens)}${d.modelsLocal ? '' : `<em>${USAGE.fmtMoney(m.cost)}</em>`}</span></div>
        <div class="us-m-bar"><i style="width:${Math.max(2, (m.tokens / maxModel) * 100).toFixed(1)}%;opacity:${(1 - i * 0.08).toFixed(2)}"></i></div></li>`).join('')}</ol>`
        : '<div class="us-none">中转站没有返回按模型的统计</div>'}
    </div>
    <div class="card us-list-card">
      <div class="us-card-head"><div><div class="us-title">API 用量</div><div class="us-sub">每个已保存的 API</div></div></div>
      <ul class="us-apis">${d.sources.map((s) => `
        <li class="${s.ok ? '' : 'off'}">
          <span class="us-a-dot ${s.ok ? 'ok' : ''}"></span>
          <div class="us-a-main"><div class="us-a-name">${esc(s.name)}${s.kind === 'relay' ? '<span class="us-tag">中转站</span>' : s.kind === 'local' ? '<span class="us-tag">本机</span>' : ''}</div>
            <div class="us-a-sub">${s.ok ? `${esc(s.host)} · ${s.range.known ? USAGE.fmtTokens(s.range.tokens) + ' tokens' + (s.kind === 'local' ? ' · 与上面的 API 有重叠，仅供参考' : ' · ' + USAGE.fmtMoney(s.range.cost)) : '今日 ' + (s.today ? USAGE.fmtTokens(s.today.tokens) + ' tokens' : '—')}` : esc(s.error || '读取失败')}</div>
            ${s.ok && s.range.known && s.kind !== 'local' ? `<div class="us-a-share"><i style="width:${((s.range.tokens / totalTok) * 100).toFixed(1)}%"></i></div>` : ''}</div>
          <div class="us-a-bal">${s.ok && s.remaining.value !== null ? `<b>${USAGE.fmtMoney(s.remaining.value, s.remaining.unit)}</b><span>剩余</span>` : ''}</div>
        </li>`).join('')}</ul>
    </div>
  </div>`;
  $$('[data-metric]', view).forEach((b) => b.addEventListener('click', () => {
    USAGE_VIEW.metric = b.dataset.metric;
    $$('[data-metric]', view).forEach((x) => x.setAttribute('aria-selected', String(x === b)));
    drawChart($('#usChart'), d);
  }));
  drawChart($('#usChart'), d);
}

/* ---------- smooth line chart (SVG, no library) ---------- */
function smoothPath(pts) {
  if (pts.length < 2) return pts.length ? `M${pts[0][0]},${pts[0][1]}` : '';
  // Monotone cubic (Fritsch–Carlson): smooth, never overshoots below zero.
  const n = pts.length, dx = [], m = [], t = [];
  for (let i = 0; i < n - 1; i++) { dx[i] = pts[i + 1][0] - pts[i][0]; m[i] = (pts[i + 1][1] - pts[i][1]) / dx[i]; }
  t[0] = m[0]; t[n - 1] = m[n - 2];
  for (let i = 1; i < n - 1; i++) t[i] = m[i - 1] * m[i] <= 0 ? 0 : (m[i - 1] + m[i]) / 2;
  for (let i = 0; i < n - 1; i++) {
    if (m[i] === 0) { t[i] = 0; t[i + 1] = 0; continue; }
    const a = t[i] / m[i], b = t[i + 1] / m[i], h = a * a + b * b;
    if (h > 9) { const k = 3 / Math.sqrt(h); t[i] = k * a * m[i]; t[i + 1] = k * b * m[i]; }
  }
  let p = `M${pts[0][0].toFixed(1)},${pts[0][1].toFixed(1)}`;
  for (let i = 0; i < n - 1; i++) {
    const h = dx[i] / 3;
    p += ` C${(pts[i][0] + h).toFixed(1)},${(pts[i][1] + t[i] * h).toFixed(1)} ${(pts[i + 1][0] - h).toFixed(1)},${(pts[i + 1][1] - t[i + 1] * h).toFixed(1)} ${pts[i + 1][0].toFixed(1)},${pts[i + 1][1].toFixed(1)}`;
  }
  return p;
}

function niceMax(v) {
  if (v <= 0) return 1;
  const p = Math.pow(10, Math.floor(Math.log10(v)));
  const f = v / p;
  return (f <= 1 ? 1 : f <= 2 ? 2 : f <= 2.5 ? 2.5 : f <= 5 ? 5 : 10) * p;
}

function drawChart(box, d) {
  if (!box) return;
  if (!d.hasDaily) {
    box.innerHTML = '<div class="us-none tall">中转站没有返回每日数据，暂时画不出曲线</div>';
    return;
  }
  const metric = d.curveLocal ? 'tokens' : USAGE_VIEW.metric;
  const vals = d.series.map((x) => (metric === 'cost' ? x.cost : x.tokens));
  const fmt = (v) => (metric === 'cost' ? USAGE.fmtMoney(v) : USAGE.fmtTokens(v));
  const W = Math.max(320, box.clientWidth || 640), H = 230;
  const pad = { l: 46, r: 14, t: 16, b: 28 };
  const max = niceMax(Math.max(...vals) * 1.08);
  const x = (i) => pad.l + (vals.length === 1 ? (W - pad.l - pad.r) / 2 : (i * (W - pad.l - pad.r)) / (vals.length - 1));
  const y = (v) => pad.t + (1 - v / max) * (H - pad.t - pad.b);
  const pts = vals.map((v, i) => [x(i), y(v)]);
  const line = smoothPath(pts);
  const area = `${line} L${x(vals.length - 1).toFixed(1)},${H - pad.b} L${x(0).toFixed(1)},${H - pad.b} Z`;
  const grid = [0, 0.5, 1].map((f) => { const gy = y(max * f); return `<line x1="${pad.l}" x2="${W - pad.r}" y1="${gy}" y2="${gy}" class="us-grid"/><text x="${pad.l - 10}" y="${gy + 4}" text-anchor="end" class="us-axis">${fmt(max * f)}</text>`; }).join('');
  const every = Math.ceil(vals.length / 7);
  const labels = d.series.map((s, i) => ((vals.length - 1 - i) % every === 0 ? `<text x="${x(i)}" y="${H - 8}" text-anchor="middle" class="us-axis">${s.date.slice(5).replace('-', '/')}</text>` : '')).join('');
  box.innerHTML = `
    <svg viewBox="0 0 ${W} ${H}" width="${W}" height="${H}" role="img" aria-label="${metric === 'cost' ? '每日花费' : '每日 Token'}趋势">
      <defs><linearGradient id="usFill" x1="0" x2="0" y1="0" y2="1"><stop offset="0" stop-color="var(--us-accent)" stop-opacity=".22"/><stop offset="1" stop-color="var(--us-accent)" stop-opacity="0"/></linearGradient></defs>
      ${grid}${labels}
      <path d="${area}" fill="url(#usFill)" class="us-area"/>
      <path d="${line}" class="us-line" pathLength="1"/>
      <line class="us-guide" y1="${pad.t}" y2="${H - pad.b}" x1="0" x2="0" opacity="0"/>
      <circle class="us-dot" r="4.5" cx="0" cy="0" opacity="0"/>
      <rect x="${pad.l}" y="0" width="${W - pad.l - pad.r}" height="${H}" fill="transparent" class="us-hit"/>
    </svg>
    <div class="us-tip" hidden></div>`;
  const svg = box.querySelector('svg'), tip = box.querySelector('.us-tip');
  const guide = svg.querySelector('.us-guide'), dot = svg.querySelector('.us-dot');
  const hit = svg.querySelector('.us-hit');
  hit.addEventListener('mousemove', (e) => {
    const r = svg.getBoundingClientRect();
    const mx = ((e.clientX - r.left) / r.width) * W;
    let i = Math.round(((mx - pad.l) / (W - pad.l - pad.r)) * (vals.length - 1));
    i = Math.max(0, Math.min(vals.length - 1, i));
    const s = d.series[i];
    guide.setAttribute('x1', pts[i][0]); guide.setAttribute('x2', pts[i][0]); guide.setAttribute('opacity', '1');
    dot.setAttribute('cx', pts[i][0]); dot.setAttribute('cy', pts[i][1]); dot.setAttribute('opacity', '1');
    tip.hidden = false;
    tip.innerHTML = `<b>${s.date.slice(5).replace('-', ' 月 ')} 日</b><span>${USAGE.fmtTokens(s.tokens)} tokens</span><span>${USAGE.fmtMoney(s.cost)} · ${USAGE.fmtTokens(s.requests)} 次</span>`;
    const left = (pts[i][0] / W) * r.width;
    tip.style.left = `${Math.min(r.width - 150, Math.max(0, left + 12))}px`;
    tip.style.top = `${Math.max(0, (pts[i][1] / H) * r.height - 30)}px`;
  });
  hit.addEventListener('mouseleave', () => { tip.hidden = true; guide.setAttribute('opacity', '0'); dot.setAttribute('opacity', '0'); });
}

addEventListener('resize', () => {
  clearTimeout(USAGE_VIEW.rt);
  USAGE_VIEW.rt = setTimeout(() => { if (S.route === 'usage' && USAGE_VIEW.data) drawChart($('#usChart'), USAGE_VIEW.data); }, 120);
});
