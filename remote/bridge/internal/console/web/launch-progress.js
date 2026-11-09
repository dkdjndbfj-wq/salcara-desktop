'use strict';

let visibleLaunchProgress = null;
function beginLaunchProgress(target, source, labels = [], operation = '启动') {
  if (visibleLaunchProgress) visibleLaunchProgress.hide();
  operation = operation === '应用配置' ? operation : '启动';
  const title = ({ 'codex-desktop': 'Codex', codex: 'Codex CLI', 'claude-desktop': 'Claude Desktop', claude: 'Claude Code', 'deepseek-harness': 'DeepSeek Harness', hermes: 'Hermes' })[target] || '工具';
  const rows = labels.map((label, index) => ({ id: 'local-' + index, label, state: 'waiting', detail: '' }));
  const root = document.createElement('div');
  root.className = 'launch-progress-back';
  const previousFocus = document.activeElement;
  const state = { index: -1, closed: false, terminal: false, message: '', timer: null, outcome: '', dom: null, stepNodes: new Map() };
  const hide = () => {
    if (state.closed) return;
    state.closed = true;
    if (state.timer != null) clearInterval(state.timer);
    root.remove();
    document.removeEventListener('keydown', onKey, true);
    window.removeEventListener('hashchange', hide);
    if (visibleLaunchProgress === handle) visibleLaunchProgress = null;
    if (previousFocus?.isConnected && typeof previousFocus.focus === 'function') previousFocus.focus();
  };
  const onKey = event => {
    if (state.closed) return;
    if (event.key === 'Escape') { event.preventDefault(); event.stopImmediatePropagation(); hide(); }
    if (event.key === 'Tab') { event.preventDefault(); state.dom?.close?.focus(); }
  };
  const elapsed = row => row.started ? `${Math.max(0, Math.floor(((row.ended || Date.now()) - row.started) / 1000))} 秒` : '';
  const labelFor = row => ({waiting:'等待',working:'进行中',done:'已完成',failed:'失败',warning:'已完成 · 提示',skipped:state.outcome === 'done' ? '不适用' : '未继续',unknown:'未确认'})[row.state];
  const make = (tag, className, text) => {
    const node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined) node.textContent = text;
    return node;
  };
  const build = () => {
    const section = make('section', 'launch-progress-modal');
    section.setAttribute('role', 'dialog'); section.setAttribute('aria-modal', 'true'); section.setAttribute('aria-label', operation + ' ' + title);
    const header = make('header');
    const mark = make('div', 'launch-progress-mark');
    mark.innerHTML = typeof accountLaunchToolIcon === 'function' ? accountLaunchToolIcon(target) : icon('external');
    const heading = make('div');
    heading.appendChild(make('h2', '', operation + ' ' + title));
    heading.appendChild(make('p', '', String(source || '当前配置').slice(0, 320)));
    const close = make('button', 'icon-btn', '✕');
    close.setAttribute('type', 'button'); close.setAttribute('aria-label', '关闭' + operation + '进度'); close.setAttribute('title', '收起进度');
    close.addEventListener('click', hide);
    header.appendChild(mark); header.appendChild(heading); header.appendChild(close);
    const summary = make('div', 'launch-progress-summary'); summary.setAttribute('aria-live', 'polite');
    const summaryText = make('span'); const count = make('span');
    summary.appendChild(summaryText); summary.appendChild(count);
    const track = make('div', 'launch-progress-track');
    track.setAttribute('role', 'progressbar'); track.setAttribute('aria-label', '已完成步骤'); track.setAttribute('aria-valuemin', '0');
    const bar = make('i'); track.appendChild(bar);
    const steps = make('ol', 'launch-progress-steps');
    section.appendChild(header); section.appendChild(summary); section.appendChild(track); section.appendChild(steps);
    root.appendChild(section);
    state.dom = { close, summaryText, count, track, bar, steps };
  };
  const stepNode = row => {
    const node = make('li'); node.setAttribute('data-launch-step', row.id);
    const dot = make('span', 'launch-progress-dot'); dot.setAttribute('aria-hidden', 'true');
    const copy = make('div', 'launch-progress-copy');
    const label = make('b'); const detail = make('p');
    copy.appendChild(label); copy.appendChild(detail);
    const status = make('small'); const statusText = make('span'); const time = make('time');
    time.setAttribute('data-launch-time', row.id); status.appendChild(statusText); status.appendChild(time);
    node.appendChild(dot); node.appendChild(copy); node.appendChild(status);
    const refs = { node, dot, label, detail, statusText, time };
    state.stepNodes.set(row.id, refs);
    return refs;
  };
  const draw = () => {
    if (state.closed || visibleLaunchProgress !== handle) return;
    if (!state.dom) build();
    const done = rows.filter(row => row.state === 'done' || row.state === 'warning').length;
    const current = rows[state.index];
    const dom = state.dom;
    dom.summaryText.textContent = state.terminal ? state.message : current?.label || '正在提交' + operation + '请求';
    dom.count.textContent = `${done} / ${rows.length}`;
    dom.track.setAttribute('aria-valuemax', String(rows.length)); dom.track.setAttribute('aria-valuenow', String(done));
    dom.bar.style.width = `${rows.length ? done / rows.length * 100 : 0}%`;
    for (const row of rows) {
      const refs = state.stepNodes.get(row.id) || stepNode(row);
      if (!refs.node.parentNode) dom.steps.appendChild(refs.node);
      refs.node.className = row.state;
      refs.dot.textContent = row.state === 'done' ? '✓' : ['failed','warning'].includes(row.state) ? '!' : row.state === 'skipped' ? '−' : row.state === 'unknown' ? '?' : '';
      refs.label.textContent = row.label;
      refs.detail.textContent = row.detail || '';
      refs.detail.hidden = !row.detail;
      refs.statusText.textContent = labelFor(row);
      refs.time.textContent = elapsed(row);
      refs.time.hidden = !row.started;
    }
  };
  const advance = index => {
    const previous = rows[state.index];
    if (previous && state.index !== index && previous.state === 'working') { previous.state = 'done'; previous.ended = Date.now(); }
    state.index = index;
    const row = rows[index];
    row.state = 'working'; row.started ||= Date.now(); draw();
  };
  const terminal = () => { state.terminal = true; if (state.timer != null) clearInterval(state.timer); };
  const handle = {
    operation,
    hide,
    step(index) { if (!state.terminal && Number.isInteger(index) && index >= state.index && index < labels.length) advance(index); },
    event(event) {
      if (state.terminal || !event) return;
      if (event.type === 'plan' && Array.isArray(event.steps)) {
        for (const item of event.steps.slice(0, 40)) {
          if (!item || typeof item.id !== 'string' || typeof item.label !== 'string' || rows.some(row => row.id === item.id)) continue;
          rows.push({ id: item.id, label: item.label, state: 'waiting', detail: '' });
        }
        draw(); return;
      }
      if (event.type !== 'phase' || typeof event.step !== 'string') return;
      let index = rows.findIndex(row => row.id === event.step);
      if (index < 0) { index = rows.length; rows.push({id:event.step,label:event.label || event.step,state:'waiting',detail:''}); }
      const row = rows[index];
      if (event.label) row.label = event.label;
      row.detail = String(event.detail || '').slice(0, 1600);
      if (state.index !== index || row.state === 'waiting') advance(index);
      if (event.state === 'warning') { row.state = 'warning'; row.ended = Date.now(); }
      draw();
      state.stepNodes.get(row.id)?.node.scrollIntoView?.({block:'nearest'});
    },
    finish() {
      if (state.terminal) return;
      terminal(); state.outcome = 'done';
      rows.forEach(row => { if (row.state === 'working') { row.state = 'done'; row.ended = Date.now(); } else if (row.state === 'waiting') row.state = 'skipped'; });
      state.message = operation + '请求已完成' + (rows.some(row => row.state === 'warning') ? ' · 有提示' : ''); draw();
    },
    fail(error) {
      if (state.terminal) return;
      terminal(); state.outcome = error?.uncertain ? 'unknown' : 'failed';
      const message = String(error?.message || error || operation + '结果尚未确认').slice(0, 1600);
      const row = rows[state.index];
      if (row) { row.state = error?.uncertain ? 'unknown' : 'failed'; row.detail = message; row.ended = Date.now(); }
      rows.forEach(item => { if (item.state === 'waiting' && !error?.uncertain) item.state = 'skipped'; });
      state.message = error?.started ? (operation === '应用配置' ? '配置已应用' : '工具已启动') + ' · 最终状态未保存' : error?.uncertain ? operation + '结果未确认' : operation + '未完成'; draw();
    },
    cancel() {
      if (state.terminal) return;
      terminal(); state.outcome = 'cancelled';
      rows.forEach(row => { if (row.state === 'working' || row.state === 'waiting') row.state = 'skipped'; });
      state.message = '页面或配置已变化，未继续' + operation; draw();
    },
  };
  visibleLaunchProgress = handle;
  document.body.appendChild(root);
  if (rows.length) handle.step(0); else draw();
  document.addEventListener('keydown', onKey, true);
  window.addEventListener('hashchange', hide);
  root.querySelector('button')?.focus();
  if (typeof setInterval === 'function') state.timer = setInterval(() => {
    if (state.closed || state.terminal) return;
    for (const row of rows) if (row.state === 'working') {
      const node = state.stepNodes.get(row.id)?.time;
      if (node) node.textContent = elapsed(row);
    }
  }, 1000);
  return handle;
}

// Safe changes stay in the selection panel and carry a unique, echoed receipt.
const SAFE_AGENT_CHANGE_PROTOCOL = 'safe-agent-change.v1';
function safeAgentOperationId() {
  if (typeof crypto === 'undefined' || typeof crypto.randomUUID !== 'function') throw new Error('当前页面无法安全提交，请刷新或更新电脑端');
  return crypto.randomUUID();
}
function requireSafeAgentReceipt(data, operationId) {
  if (data?.ok !== true || data.protocol !== SAFE_AGENT_CHANGE_PROTOCOL || data.operationId !== operationId) {
    throw Object.assign(new Error('操作结果未确认，请检查原工具状态；不会自动重试'), { uncertain: true });
  }
  if (data.uncertain === true || data.result?.uncertain === true) {
    throw Object.assign(new Error('配置/启动已提交，状态未能确认，请在原工具核对后重新打开；不会自动重试'), { uncertain: true, started: data.started === true, applied: data.applied === true, submitted: data.result?.submitted === true, result: data.result });
  }
  return data;
}
function inlineAgentProgress(owner, paint, applyOnly) {
  let terminal = false;
  const update = message => { owner.operationStage = message; paint(); };
  return {
    operation: applyOnly ? '应用配置' : '启动',
    step() { if (!terminal) update('正在检查账号与工具…'); },
    event(event) {
      if (terminal || event?.type !== 'phase') return;
      const label = ({ stop: '正在停止该 Agent 的当前任务…', catalog: '正在读取模型目录…', profile: '正在应用所选 API…', start: '正在打开 Agent…', record: '正在保存操作结果…' })[event.step];
      if (label) update(label);
    },
    finish() { terminal = true; update(applyOnly ? '配置已应用' : '已应用并打开 Agent'); },
    fail(error) { terminal = true; update(error?.uncertain ? '操作结果未确认' : '操作未完成'); },
    cancel() { terminal = true; update('操作已取消'); },
  };
}

// One POST carries actual stages and the final response. Callers may cancel when
// their panel closes; a lost acknowledgement must never trigger a second POST.
async function launchAPI(path, body, progress, options = {}) {
  const operation = progress?.operation === '应用配置' ? '应用配置' : '启动';
  const controller = new AbortController();
  const abort = () => controller.abort();
  if (options.signal?.aborted) abort();
  else options.signal?.addEventListener('abort', abort, { once: true });
  const timer = setTimeout(() => controller.abort(), 65000);
  const uncertain = message => Object.assign(new Error(message), {uncertain:true});
  const result = (status, data) => {
    if (!Number.isInteger(status) || !data || typeof data !== 'object') throw uncertain(operation + '响应不完整');
    if (status < 200 || status >= 300 || data.ok !== true) throw Object.assign(new Error(data.error || `${operation}失败 (${status})`), {status,code:data.code,uncertain:data.uncertain === true || data.result?.uncertain === true,started:data.started === true,applied:data.applied === true,result:data.result});
    return data;
  };
  try {
    if (controller.signal.aborted) throw Object.assign(new Error('操作已取消'), { cancelled: true });
    const res = await fetch(path, {method:'POST',credentials:'same-origin',signal:controller.signal,headers:{'Content-Type':'application/json','Accept':'application/x-ndjson'},body:JSON.stringify(body)});
    if (body?.protocol === SAFE_AGENT_CHANGE_PROTOCOL && [404, 405].includes(res.status)) {
      throw Object.assign(new Error('电脑端不支持安全应用功能，请更新电脑端后重新打开'), { status: res.status, code: 'safe_agent_change_not_supported' });
    }
    if (!res.ok || !res.headers.get('Content-Type')?.includes('application/x-ndjson')) return result(res.status, await res.json());
    if (!res.body?.getReader) throw uncertain(operation + '进度连接不可用');
    const reader = res.body.getReader(), decoder = new TextDecoder();
    let pending = '', sequence = 0;
    try {
      while (true) {
        const {done,value} = await reader.read();
        pending += decoder.decode(value, {stream:!done});
        if (pending.length > 1024 * 1024) throw uncertain(operation + '进度响应异常');
        let end;
        while ((end = pending.indexOf('\n')) >= 0) {
          const line = pending.slice(0,end); pending = pending.slice(end+1);
          if (!line.trim()) continue;
          let record; try { record = JSON.parse(line); } catch (_) { throw uncertain(operation + '进度响应不完整'); }
          if (record.sequence !== ++sequence || !record.event) throw uncertain(operation + '进度顺序异常');
          if (record.event.type === 'result') return result(record.event.status, record.event.data);
          progress?.event(record.event);
        }
        if (done) throw uncertain(operation + '连接已中断，结果尚未确认');
      }
    } finally { reader.releaseLock(); }
  } catch (error) {
    if (controller.signal.aborted) throw options.signal?.aborted ? Object.assign(uncertain('操作已取消，请检查原工具的最终状态'), { cancelled: true }) : uncertain('等待' + operation + '结果超时');
    if (error instanceof TypeError || error instanceof SyntaxError) throw uncertain(operation + '连接已中断，结果尚未确认');
    throw error;
  } finally { clearTimeout(timer); options.signal?.removeEventListener('abort', abort); }
}
