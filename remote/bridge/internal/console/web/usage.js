'use strict';
/* Provider balances and Token activity, by model and saved API. */

const USAGE_VIEW = { days: 14, metric: 'tokens', source: '', options: [], data: null, seq: 0, controller: null, dayTimer: 0 };

function usageVisible() {
  return S.route === 'usage' && (typeof document === 'undefined' || document.hidden !== true);
}

function suspendUsage() {
  clearTimeout(USAGE_VIEW.dayTimer); USAGE_VIEW.dayTimer = 0;
  ++USAGE_VIEW.seq;
  USAGE_VIEW.controller?.abort(); USAGE_VIEW.controller = null;
  USAGE_VIEW.data = null;
  if (S.route === 'usage') {
    $('#usRefresh')?.classList.remove('spinning');
    const view = $('#view');
    if (view) view.innerHTML = usageSkeleton();
  }
}

function scheduleUsageDayChange() {
  clearTimeout(USAGE_VIEW.dayTimer); USAGE_VIEW.dayTimer = 0;
  if (typeof document === 'undefined' || !usageVisible()) return;
  const revision = S.routeRevision, now = new Date(), next = new Date(now);
  next.setHours(24, 0, 0, 0);
  USAGE_VIEW.dayTimer = setTimeout(() => {
    USAGE_VIEW.dayTimer = 0;
    if (usageVisible() && S.routeRevision === revision) void loadUsage(false);
  }, Math.max(1, next.getTime() - now.getTime() + 50));
}

if (typeof document !== 'undefined') document.addEventListener('visibilitychange', () => {
  // Returning to the window must recheck the selected credentials and date,
  // rather than briefly presenting the previous account's cached numbers.
  if (document.hidden === true) suspendUsage();
  else if (S.route === 'usage') void loadUsage(false);
});
addEventListener('pagehide', suspendUsage);
addEventListener('pageshow', (event) => {
  if (event.persisted && usageVisible()) void loadUsage(false);
});

function usageSkeleton() {
  if (typeof pageSkeleton === 'function') return pageSkeleton('usage');
  const stat = '<div class="us-stat"><div class="skeleton" style="width:50%"></div><div class="skeleton" style="margin-top:14px;height:26px;width:70%"></div></div>';
  return `<div class="us-stats">${stat.repeat(4)}</div><div class="card us-chart-card"><div class="skeleton" style="height:220px"></div></div>`;
}

function usageHead() {
  $('#headActions').innerHTML = `
    <div class="us-controls">
    <select class="input us-source" id="usSource" aria-label="用量来源"></select>
    <div class="us-seg" role="tablist" aria-label="时间范围">
      ${[7, 14, 30].map((d) => `<button type="button" role="tab" data-days="${d}" aria-selected="${USAGE_VIEW.days === d}">${d} 天</button>`).join('')}
    </div>
    <button class="btn sm icon-only" type="button" id="usRefresh" aria-label="刷新" title="刷新">${icon('refresh')}</button></div>`;
  usageOptions();
  $('#usSource').addEventListener('change', (event) => {
    USAGE_VIEW.source = event.target.value;
    return loadUsage(false);
  });
  $$('#headActions [data-days]').forEach((b) => b.addEventListener('click', () => {
    USAGE_VIEW.days = Number(b.dataset.days);
    $$('#headActions [data-days]').forEach((x) => x.setAttribute('aria-selected', String(x === b)));
    return loadUsage(false);
  }));
  $('#usRefresh').addEventListener('click', () => loadUsage(true));
}

function usageOptions() {
  const select = $('#usSource');
  if (!select) return;
  select.disabled = !USAGE_VIEW.options.length && !USAGE_VIEW.source;
  const label = (s) => `${s.kind === 'official' || s.kind === 'official_account' ? '账号' : s.kind === 'relay' ? '中转站' : 'API'} · ${s.name || s.host || '未命名'}${s.host && s.host !== s.name ? ' · ' + s.host : ''}`;
  const names = new Map();
  for (const option of USAGE_VIEW.options) names.set(label(option), (names.get(label(option)) || 0) + 1);
  const choices = USAGE_VIEW.options.map((option) => {
    const name = label(option);
    let display = name;
    if (names.get(name) > 1) {
      const shortID = option.id.slice(-8);
      const unique = USAGE_VIEW.options.filter((s) => label(s) === name && s.id.slice(-8) === shortID).length === 1;
      // Put the distinction before a long host so a compact closed control
      // still tells same-name accounts apart. The option value stays exact.
      display = label({ ...option, name: `${option.name || option.host || '未命名'} · ${unique ? shortID : option.id}` });
    }
    return `<option value="${esc(option.id)}"${option.id === USAGE_VIEW.source ? ' selected' : ''}>${esc(display)}</option>`;
  });
  if (USAGE_VIEW.source && !USAGE_VIEW.options.some((s) => s.id === USAGE_VIEW.source)) choices.unshift(`<option value="${esc(USAGE_VIEW.source)}" selected>来源已不可用</option>`);
  const html = `<option value=""${!USAGE_VIEW.source ? ' selected' : ''}>用量总览</option>${choices.join('')}`;
  // Keep the select and its styled button alive so keyboard focus survives.
  if (select.innerHTML !== html) select.innerHTML = html;
}

RENDER_usage = async function (view) {
  usageHead();
  // Credentials or an account can change while another page is open.
  USAGE_VIEW.data = null;
  view.innerHTML = usageSkeleton();
  await loadUsage(false);
};

async function loadUsage(refresh) {
  clearTimeout(USAGE_VIEW.dayTimer); USAGE_VIEW.dayTimer = 0;
  if (!usageVisible()) { suspendUsage(); return; }
  const seq = ++USAGE_VIEW.seq;
  const source = USAGE_VIEW.source, days = USAGE_VIEW.days, revision = S.routeRevision;
  USAGE_VIEW.controller?.abort();
  const controller = new AbortController();
  USAGE_VIEW.controller = controller;
  const timeout = setTimeout(() => controller.abort(), 30000);
  const current = () => seq === USAGE_VIEW.seq && usageVisible() && S.routeRevision === revision && USAGE_VIEW.source === source && USAGE_VIEW.days === days;
  const btn = $('#usRefresh');
  if (btn) btn.classList.add('spinning');
  // Never leave A's numbers under B's selected label, including on failures.
  USAGE_VIEW.data = null;
  $('#view').innerHTML = usageSkeleton();
  try {
    const data = await USAGE.load(days, refresh, source, controller.signal);
    if (!current()) return;
    if (controller.signal.aborted) throw new Error('用量读取超时');
    USAGE_VIEW.options = data.options;
    usageOptions();
    USAGE_VIEW.data = data;
    drawUsage($('#view'), data);
  } catch (e) {
    if (!current()) return;
    if (Array.isArray(e.options)) { USAGE_VIEW.options = e.options; usageOptions(); }
    $('#view').innerHTML = `<div class="callout bad" role="status">${ico('warn')}<div>${esc(controller.signal.aborted ? '用量读取超时' : e.message)}</div></div>`;
  } finally {
    clearTimeout(timeout);
    if (btn && seq === USAGE_VIEW.seq) btn.classList.remove('spinning');
    if (seq === USAGE_VIEW.seq) {
      USAGE_VIEW.controller = null;
      scheduleUsageDayChange();
    }
  }
}

function drawUsage(view, d) {
  if (!d.sources.length) {
    view.innerHTML = `<div class="us-empty">
      <div class="us-empty-ico">${icon('chart')}</div>
      <div class="us-empty-t">暂无可读取的用量</div>
      <div class="us-empty-d">${d.options?.length ? '选择 API 或账号查看独立用量。' : '还没有可统计的 API 或账号。'}</div>
      <a class="btn" href="#accounts">${icon('key')}打开 API 密钥</a></div>`;
    return;
  }
  const bal = d.balance;
  const selected = d.selectedSource ? d.sources.find((s) => s.id === d.selectedSource) : null;
  const scopeName = selected ? selected.name || selected.host || '当前来源' : '可读取来源合计';
  const localLabel = (selected ? '该账号的本机记录' : '本机记录') + (d.partial ? ' · 部分记录' : '');
  const errors = selected ? [selected.error, selected.localUsageError].filter(Boolean) : [];
  const stats = [
    { k: !selected && d.sources.filter((s) => s.kind !== 'local').length > 1 ? '来源余额' : '剩余金额', v: bal ? USAGE.fmtMoney(bal.value, bal.unit) : '—', f: bal ? bal.name || bal.host || '当前来源' : '服务商未返回余额', title: bal ? [bal.name, bal.host].filter(Boolean).join(' · ') : '服务商未返回余额', main: true },
    { k: '今日 Token', v: d.today ? USAGE.fmtTokens(d.today.tokens) : '—', f: d.today ? `${USAGE.fmtTokens(d.today.requests)} 次请求` : '暂无数据' },
    { k: '今日请求', v: d.today ? USAGE.fmtTokens(d.today.requests) : '—', f: d.todayLocal ? localLabel : '服务商返回' },
    { k: `近 ${d.days} 天 Token`, v: d.rangeTokens !== null ? USAGE.fmtTokens(d.rangeTokens) : '—', f: d.curveLocal ? localLabel : '服务商返回' },
  ];
  const maxModel = Math.max(1, ...d.models.map((m) => m.tokens));
  const totalTok = d.sources.filter((s) => s.kind !== 'local').reduce((a, s) => a + (s.range.tokens || 0), 0) || 1;
  view.innerHTML = `
  ${errors.length ? `<div class="callout warn us-source-error" role="status">${ico('warn')}<div>${errors.map(esc).join(' · ')}</div></div>` : ''}
  <div class="us-stats">${stats.map((s) => `<div class="us-stat${s.main ? ' us-stat-balance' : ''}"><div class="us-k">${esc(s.k)}</div><div class="us-v" title="${esc(s.v)}">${esc(s.v)}</div><div class="us-f" title="${esc(s.title || s.f)}">${esc(s.f)}</div></div>`).join('')}</div>

  <div class="card us-chart-card">
    <div class="us-card-head">
      <div><div class="us-title">趋势</div><div class="us-sub">近 ${d.days} 天 · ${esc(scopeName)}${d.curveLocal ? ' · 本机记录' : ''}</div></div>
      <span class="us-tag">Token</span>
    </div>
    <div class="us-chart" id="usChart"></div>
  </div>

  <div class="us-cols">
    <div class="card us-list-card">
      <div class="us-card-head"><div><div class="us-title">模型用量</div><div class="us-sub">按 Token 排序${d.modelsLocal ? ' · 本机记录' : ''}${d.partial ? ' · 部分记录' : ''}</div></div></div>
      ${d.models.length ? `<ol class="us-models">${d.models.slice(0, 8).map((m, i) => `
        <li><div class="us-m-top"><span class="us-m-name" title="${esc(m.model)}">${esc(m.model)}</span><span class="us-m-val">${USAGE.fmtTokens(m.tokens)}</span></div>
        ${m.tokens !== null ? `<div class="us-m-bar"><i style="width:${Math.max(2, (m.tokens / maxModel) * 100).toFixed(1)}%;opacity:${(1 - i * 0.08).toFixed(2)}"></i></div>` : ''}</li>`).join('')}</ol>`
        : '<div class="us-none">暂无模型用量</div>'}
    </div>
    <div class="card us-list-card">
      <div class="us-card-head"><div><div class="us-title">来源用量</div><div class="us-sub">${selected ? '当前 API / 账号' : '可读取的来源'}</div></div></div>
      <ul class="us-apis">${d.sources.map((s) => `
        <li class="${s.ok ? '' : 'off'}">
          <span class="us-a-dot ${s.ok ? 'ok' : ''}"></span>
          <div class="us-a-main"><div class="us-a-name">${esc(s.name)}${s.kind === 'relay' ? '<span class="us-tag">中转站</span>' : s.kind === 'local' ? '<span class="us-tag">本机</span>' : s.kind === 'official' || s.kind === 'official_account' ? '<span class="us-tag">账号</span>' : ''}</div>
            <div class="us-a-sub">${s.ok ? `${s.host ? esc(s.host) + ' · ' : ''}${s.range.known ? USAGE.fmtTokens(s.range.tokens) + ' tokens' + (s.kind === 'local' ? ' · 与上面的 API 有重叠，仅供参考' : '') : '今日 ' + (s.today ? USAGE.fmtTokens(s.today.tokens) + ' tokens' : '—')}` : esc(s.error || '读取失败')}</div>
            ${s.ok && s.range.known && s.kind !== 'local' ? `<div class="us-a-share"><i style="width:${((s.range.tokens / totalTok) * 100).toFixed(1)}%"></i></div>` : ''}</div>
          <div class="us-a-bal">${s.ok && s.remaining.value !== null ? `<b>${esc(USAGE.fmtMoney(s.remaining.value, s.remaining.unit))}</b><span>剩余</span>` : ''}</div>
        </li>`).join('')}</ul>
    </div>
  </div>`;

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
    box.innerHTML = '<div class="us-none tall">暂无每日 Token 数据</div>';
    return;
  }
  const vals = d.series.map((x) => x.tokens);
  if (!vals.some((value) => value !== null)) {
    box.innerHTML = '<div class="us-none tall">服务商未提供每日 Token 数据</div>';
    return;
  }
  const fmt = USAGE.fmtTokens;
  const W = Math.max(320, box.clientWidth || 640), H = 230;
  const pad = { l: 46, r: 14, t: 16, b: 28 };
  const max = niceMax(Math.max(...vals.filter((value) => value !== null)) * 1.08);
  const x = (i) => pad.l + (vals.length === 1 ? (W - pad.l - pad.r) / 2 : (i * (W - pad.l - pad.r)) / (vals.length - 1));
  const y = (v) => pad.t + (1 - v / max) * (H - pad.t - pad.b);
  const pts = vals.map((v, i) => v === null ? null : [x(i), y(v)]);
  const segments = [];
  let segment = [];
  for (const point of pts) {
    if (point) segment.push(point);
    else if (segment.length) { segments.push(segment); segment = []; }
  }
  if (segment.length) segments.push(segment);
  const line = segments.map(smoothPath).join(' ');
  const area = segments.map((points) => `${smoothPath(points)} L${points[points.length - 1][0].toFixed(1)},${H - pad.b} L${points[0][0].toFixed(1)},${H - pad.b} Z`).join(' ');
  const grid = (max <= 1 ? [0, 1] : [0, 0.5, 1]).map((f) => { const gy = y(max * f); return `<line x1="${pad.l}" x2="${W - pad.r}" y1="${gy}" y2="${gy}" class="us-grid"/><text x="${pad.l - 10}" y="${gy + 4}" text-anchor="end" class="us-axis">${fmt(max * f)}</text>`; }).join('');
  const every = Math.ceil(vals.length / 7);
  const labels = d.series.map((s, i) => ((vals.length - 1 - i) % every === 0 ? `<text x="${x(i)}" y="${H - 8}" text-anchor="middle" class="us-axis">${s.date.slice(5).replace('-', '/')}</text>` : '')).join('');
  box.innerHTML = `
    <svg viewBox="0 0 ${W} ${H}" width="${W}" height="${H}" role="img" aria-label="每日 Token 趋势">
      <defs><linearGradient id="usFill" x1="0" x2="0" y1="0" y2="1"><stop offset="0" stop-color="#8A7BF6" stop-opacity=".30"/><stop offset="1" stop-color="#7CC6FF" stop-opacity="0"/></linearGradient><linearGradient id="usLine" x1="0" x2="1" y1="0" y2="0"><stop offset="0" stop-color="#7CC6FF"/><stop offset=".5" stop-color="#3D7BFA"/><stop offset=".85" stop-color="#A68BF7"/><stop offset="1" stop-color="#F4A6CE"/></linearGradient></defs>
      ${grid}${labels}
      <path d="${area}" fill="url(#usFill)" class="us-area"/>
      <path d="${line}" class="us-line" stroke="url(#usLine)" pathLength="1"/>
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
    const point = pts[i];
    if (point) {
      guide.setAttribute('x1', point[0]); guide.setAttribute('x2', point[0]); guide.setAttribute('opacity', '1');
      dot.setAttribute('cx', point[0]); dot.setAttribute('cy', point[1]); dot.setAttribute('opacity', '1');
    } else { guide.setAttribute('opacity', '0'); dot.setAttribute('opacity', '0'); }
    tip.hidden = false;
    tip.innerHTML = `<b>${s.date.slice(5).replace('-', ' 月 ')} 日</b><span>${USAGE.fmtTokens(s.tokens)} tokens</span><span>${USAGE.fmtTokens(s.requests)} 次</span>`;
    const left = (x(i) / W) * r.width;
    tip.style.left = `${Math.min(r.width - 150, Math.max(0, left + 12))}px`;
    tip.style.top = `${Math.max(0, ((point ? point[1] : H / 2) / H) * r.height - 30)}px`;
  });
  hit.addEventListener('mouseleave', () => { tip.hidden = true; guide.setAttribute('opacity', '0'); dot.setAttribute('opacity', '0'); });
}

addEventListener('resize', () => {
  clearTimeout(USAGE_VIEW.rt);
  USAGE_VIEW.rt = setTimeout(() => { if (S.route === 'usage' && USAGE_VIEW.data) drawChart($('#usChart'), USAGE_VIEW.data); }, 120);
});
