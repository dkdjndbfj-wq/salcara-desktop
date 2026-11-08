'use strict';

// Parse complete SSE frames while retaining split UTF-8/line/event boundaries.
function accountTestParseEvents(buffer, emit, final = false) {
  buffer = buffer.replace(/\r\n/g, '\n');
  if (buffer.length > 262144) throw new Error('响应数据过大，测试已停止');
  let end;
  while ((end = buffer.indexOf('\n\n')) >= 0) {
    const block = buffer.slice(0, end); buffer = buffer.slice(end + 2);
    let event = 'message'; const data = [];
    for (const line of block.split('\n')) {
      if (line.startsWith('event:')) event = line.slice(6).trim();
      if (line.startsWith('data:')) data.push(line.slice(5).trimStart());
    }
    if (data.length) emit(event, data.join('\n'));
  }
  if (final && buffer.trim()) throw new Error('响应流在数据帧中途断开');
  return buffer;
}

(() => {
  let panel = null;
  const current = owner => owner && panel === owner && owner.element?.isConnected;
  const textBytes = value => new TextEncoder().encode(value).length;
  function close(restore = true) {
    const owner = panel;
    if (!owner) return;
    panel = null; owner.generation++; owner.controller?.abort(); owner.element.remove();
    if (restore && owner.previousFocus?.isConnected) owner.previousFocus.focus?.();
  }
  function selectedModel(owner) {
    const select = owner.element.querySelector('#accountTestModel');
    return (select?.value === '__custom' ? owner.element.querySelector('#accountTestCustomModel')?.value : select?.value || '').trim();
  }
  function controls(owner) {
    if (!current(owner)) return;
    const root = owner.element;
    root.querySelector('#accountTestSend').disabled = owner.loading || owner.pending || !owner.revision;
    root.querySelector('#accountTestStop').hidden = !owner.pending;
    root.querySelector('#accountTestSend').hidden = owner.pending;
    for (const id of ['accountTestModel', 'accountTestCustomModel']) {
      const node = root.querySelector('#' + id);
      if (node) node.disabled = owner.pending;
    }
  }
  function reset(owner) {
    owner.generation++; owner.controller?.abort(); owner.controller = null; owner.pending = false;
    owner.history = []; owner.turns = 0;
    owner.element.querySelector('#accountTestConversation').innerHTML = `<div class="account-test-empty">${icon('send')}<b>发送一条消息，检查真实响应</b><span>固定使用当前账号，不自动换账号或模型。</span></div>`;
    owner.element.querySelector('#accountTestStatus').textContent = '';
    controls(owner);
  }
  function scroll(owner) {
    const node = owner.element.querySelector('#accountTestConversation');
    node.scrollTop = node.scrollHeight;
  }
  function addTurn(owner, message) {
    const root = owner.element.querySelector('#accountTestConversation');
    root.querySelector('.account-test-empty')?.remove();
    while (root.children.length >= 20) root.firstElementChild.remove();
    root.insertAdjacentHTML('beforeend', `<article class="account-test-turn"><div class="account-test-message user"><span>你</span><p>${esc(message)}</p></div><div class="account-test-message assistant"><span>${esc(owner.name)}</span><p class="account-test-answer">正在连接…</p><small class="account-test-result" role="status">请求已发送</small></div></article>`);
    scroll(owner); return root.lastElementChild;
  }
  function historyWith(owner, message) {
    const messages = [...owner.history.slice(-20), { role: 'user', content: message }];
    while (messages.length > 1 && messages.reduce((n, row) => n + textBytes(row.content), 0) > 65536) messages.splice(0, 2);
    return messages;
  }
  async function send() {
    const owner = panel;
    if (!current(owner) || owner.pending || owner.loading || !owner.revision) return;
    const input = owner.element.querySelector('#accountTestInput'), model = selectedModel(owner), message = input.value.trim();
    const status = owner.element.querySelector('#accountTestStatus');
    if (!model || model.length > 200 || /[\r\n\x00]/.test(model)) { status.textContent = '请选择或填写模型 ID'; return; }
    if (!message) { input.focus(); return; }
    if (textBytes(message) > 16384) { status.textContent = '测试消息过长，请缩短后发送'; return; }
    const messages = historyWith(owner, message), turn = addTurn(owner, message);
    const answerNode = turn.querySelector('.account-test-answer'), resultNode = turn.querySelector('.account-test-result');
    const generation = ++owner.generation, controller = new AbortController();
    const owns = () => current(owner) && owner.generation === generation;
    owner.pending = true; owner.controller = controller; owner.stopRequested = false; input.value = ''; status.textContent = ''; controls(owner);
    let answer = '', diagnostic = null, upstreamError = '', timedOut = false;
    const timer = setTimeout(() => { timedOut = true; controller.abort(); }, 185000);
    try {
      const response = await fetch('/api/local/accounts/test/send', { method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' }, signal: controller.signal, body: JSON.stringify({ ...owner.target, revision: owner.revision, model, messages }) });
      if (!owns()) return;
      if (!response.ok || !response.headers.get('Content-Type')?.startsWith('text/event-stream')) {
        let data; try { data = await response.json(); } catch (_) { /* bounded local error */ }
        throw new Error(`HTTP ${response.status} · ${typeof data?.error === 'string' ? data.error : data?.error?.message || '没有收到有效的流式响应'}`);
      }
      const reader = response.body.getReader(), decoder = new TextDecoder(); let buffer = '';
      const event = (kind, raw) => {
        if (!owns() || raw === '[DONE]') return;
        let data; try { data = JSON.parse(raw); } catch (_) { throw new Error('响应格式不完整'); }
        if (kind === 'diagnostic') { diagnostic = data; return; }
        if (data.error) { upstreamError = typeof data.error === 'string' ? data.error : data.error.message || '上游返回错误'; return; }
        const content = data.choices?.[0]?.delta?.content;
        if (typeof content === 'string') {
          if (textBytes(answer + content) > 16384) { controller.abort(); throw new Error('回复已达到测试窗口的长度上限，已停止接收'); }
          answer += content; answerNode.textContent = answer; resultNode.textContent = '正在接收…'; scroll(owner);
        }
      };
      while (owns()) {
        const { value, done } = await reader.read();
        buffer += decoder.decode(value, { stream: !done });
        buffer = accountTestParseEvents(buffer, event, done);
        if (done) break;
      }
      if (!owns()) return;
      if (upstreamError) throw new Error(upstreamError);
      if (!diagnostic) throw new Error('连接已结束，但没有收到完成确认；请检查上游');
      if (diagnostic.state !== 'completed' || !answer.trim()) {
        const reason = diagnostic.limited ? '响应超过测试上限' : ({ interrupted: '响应中途断开', incomplete: '上游未完成响应', cancelled: '请求超时或已取消', failed: '上游请求失败' })[diagnostic.state] || '未收到完整的文本回复';
        throw new Error(reason);
      }
      const timing = Number.isFinite(diagnostic.durationMs) ? `${diagnostic.durationMs} ms` : '耗时未记录';
      const first = Number.isFinite(diagnostic.firstByteMs) ? ` · 首包 ${diagnostic.firstByteMs} ms` : '';
      const tokens = Number.isFinite(diagnostic.tokens?.totalTokens) ? ` · ${diagnostic.tokens.totalTokens} Token` : '';
      resultNode.textContent = `连接成功 · HTTP ${diagnostic.upstreamHttpStatus || diagnostic.httpStatus} · ${timing}${first}${tokens}`;
      resultNode.classList.add('ok');
      owner.history = [...messages, { role: 'assistant', content: answer }].slice(-20);
    } catch (error) {
      if (!owns()) return;
      controller.abort();
      if (!answer) answerNode.textContent = '本次未收到回复';
      const reason = owner.stopRequested ? '已停止' : timedOut ? '测试等待超时，未自动重发' : error.name === 'AbortError' ? '连接中断，未自动重发' : error.message;
      resultNode.textContent = reason; resultNode.classList.add('failed');
      if (!input.value) input.value = message;
    } finally {
      clearTimeout(timer);
      if (owns()) { owner.pending = false; owner.controller = null; controls(owner); scroll(owner); input.focus(); }
    }
  }
  async function open(button) {
    const accounts = typeof officialLibraryAccounts === 'function' ? officialLibraryAccounts(S.local?.accounts || []) : S.local?.accounts || [];
    const account = accounts.find(row => row.id === button.dataset.id);
    if (!account || account.officialAccount?.mode === 'chatgpt_web_session') throw new Error('账号已变化，请刷新列表');
    close(false);
    const previousFocus = document.activeElement;
    document.body.insertAdjacentHTML('beforeend', `<div id="accountTestOverlay" class="modal-back account-test-overlay"><section class="modal account-test-modal" role="dialog" aria-modal="true" aria-labelledby="accountTestTitle"><header class="modal-head"><div><h3 id="accountTestTitle">${icon('play')}发送测试请求</h3><p id="accountTestTarget">${esc(account.name)}</p></div><button class="icon-btn" data-act="accountTestClose" title="关闭测试" aria-label="关闭测试">${icon('close')}</button></header><div class="account-test-tools"><div id="accountTestModelBox" class="account-test-model"><span class="muted">正在读取账号配置…</span></div><button class="btn sm" data-act="accountTestClear">${icon('trash')}清空对话</button></div><div id="accountTestConversation" class="account-test-conversation" role="log" aria-label="测试对话"></div><div class="account-test-footer"><p class="account-test-note">真实请求，可能消耗服务商额度。对话仅保留在此窗口。</p><div class="account-test-compose"><textarea id="accountTestInput" class="input" rows="2" maxlength="4000" placeholder="输入测试消息，Enter 发送 · Shift+Enter 换行" aria-label="测试消息">请只回复：连接成功。</textarea><button id="accountTestSend" class="btn primary" data-act="accountTestSend" disabled>${icon('send')}发送</button><button id="accountTestStop" class="btn" data-act="accountTestStop" hidden>${icon('stop')}停止</button></div><div id="accountTestStatus" role="status"></div></div></section></div>`);
    const owner = panel = { element: $('#accountTestOverlay'), name: account.name, target: account.libraryOnly ? { officialId: account.officialAccount.id } : { accountId: account.id }, previousFocus, generation: 0, history: [], pending: false, loading: true, revision: '' };
    reset(owner); owner.element.querySelector('[data-act="accountTestClose"]').focus();
    try {
      const result = await api('/api/local/accounts/test/prepare', owner.target);
      if (!current(owner)) return;
      if (!result.revision || !Array.isArray(result.models)) throw new Error('账号配置响应不完整');
      owner.revision = result.revision; owner.name = result.name;
      owner.element.querySelector('#accountTestTarget').textContent = result.name;
      const models = result.models.filter(row => typeof row === 'string');
      owner.element.querySelector('#accountTestModelBox').innerHTML = `<label for="accountTestModel">模型</label><select id="accountTestModel" class="input">${models.map(model => `<option value="${esc(model)}">${esc(model)}</option>`).join('')}<option value="__custom">填写模型 ID…</option></select><input id="accountTestCustomModel" class="input" maxlength="200" aria-label="自定义模型 ID" placeholder="填写服务商支持的模型 ID" ${models.length ? 'hidden' : ''}>`;
      owner.element.querySelector(models.length ? '#accountTestInput' : '#accountTestCustomModel').focus();
    } catch (error) { if (current(owner)) owner.element.querySelector('#accountTestStatus').textContent = error.message + '；关闭后可重新打开测试。'; }
    finally { if (current(owner)) { owner.loading = false; controls(owner); } }
  }
  Object.assign(ACTIONS, { accountTest: open, accountTestClose: () => close(), accountTestClear: () => { if (panel) reset(panel); }, accountTestSend: send, accountTestStop: () => { if (panel) { panel.stopRequested = true; panel.controller?.abort(); } } });
  document.addEventListener('click', event => { if (panel && event.target === panel.element) close(); });
  document.addEventListener('change', event => {
    if (!panel || !['accountTestModel', 'accountTestCustomModel'].includes(event.target.id)) return;
    if (event.target.id === 'accountTestModel') panel.element.querySelector('#accountTestCustomModel').hidden = event.target.value !== '__custom';
    reset(panel);
  });
  document.addEventListener('keydown', event => {
    if (!panel) return;
    if (document.querySelector('.cs-menu')) return;
    if (event.key === 'Escape') { event.preventDefault(); event.stopImmediatePropagation(); close(); }
    else if (event.target.id === 'accountTestInput' && event.key === 'Enter' && !event.shiftKey && !event.isComposing && event.keyCode !== 229) { event.preventDefault(); void send(); }
    else if (event.key === 'Tab') {
      const nodes = [...panel.element.querySelectorAll('button:not([disabled]),input:not([disabled]),textarea,select:not([disabled])')].filter(node => node.getClientRects().length && !node.closest('[hidden]'));
      const first = nodes[0], last = nodes.at(-1);
      if (event.shiftKey && (document.activeElement === first || !panel.element.contains(document.activeElement))) { event.preventDefault(); last?.focus(); }
      else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first?.focus(); }
    }
  }, true);
  addEventListener('hashchange', () => close(false));
})();
