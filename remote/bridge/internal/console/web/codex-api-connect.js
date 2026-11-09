'use strict';

const API_CONNECTION_PROTOCOLS = {
  chat: { name: 'OpenAI Chat', base: '/v1', path: '/v1/chat/completions', auth: 'Authorization: Bearer YOUR_SALCARA_CLIENT_KEY', models: '/v1/models' },
  responses: { name: 'OpenAI Responses', base: '/v1', path: '/v1/responses', auth: 'Authorization: Bearer YOUR_SALCARA_CLIENT_KEY', models: '/v1/models' },
  anthropic: { name: 'Anthropic Messages', base: '', path: '/v1/messages', auth: 'x-api-key: YOUR_SALCARA_CLIENT_KEY', models: '/v1/models' },
  gemini: { name: 'Gemini', base: '', path: '/v1beta/models/', auth: 'x-goog-api-key: YOUR_SALCARA_CLIENT_KEY', models: '/v1beta/models' },
  ollama: { name: 'Ollama', base: '', path: '/api/chat', auth: 'Authorization: Bearer YOUR_SALCARA_CLIENT_KEY', models: '/api/tags' },
};
function apiConnectionSpec(baseUrl, protocol, model = 'YOUR_MODEL_ID') {
  const spec = API_CONNECTION_PROTOCOLS[protocol];
  if (!spec) throw new Error('请选择支持的协议');
  const url = new URL(baseUrl);
  if (!['http:', 'https:'].includes(url.protocol) || url.username || url.password || url.search || url.hash || !url.pathname.endsWith('/v1')) throw new Error('服务地址无效，请刷新状态');
  const root = url.href.replace(/\/v1$/, '');
  model = model.trim() || 'YOUR_MODEL_ID';
  const path = spec.path + (protocol === 'gemini' ? encodeURIComponent(model) + ':generateContent' : '');
  const body = protocol === 'responses' ? { model, input: '你好' }
    : protocol === 'gemini' ? { contents: [{ role: 'user', parts: [{ text: '你好' }] }] }
    : { model, messages: [{ role: 'user', content: '你好' }], ...(protocol === 'anthropic' ? { max_tokens: 1024 } : {}), ...(protocol === 'ollama' ? { stream: false } : {}) };
  const quote = value => "'" + value.replaceAll("'", "'\\''") + "'";
  const example = ['curl ' + quote(root + path), '  -H ' + quote('Content-Type: application/json'), '  -H ' + quote(spec.auth), ...(protocol === 'anthropic' ? ['  -H ' + quote('anthropic-version: 2023-06-01')] : []), '  -d ' + quote(JSON.stringify(body))].join(' \\\n');
  return { baseUrl: root + spec.base, route: root + path, models: root + spec.models, auth: spec.auth, example };
}
function apiConnectionHTML(status) {
  let spec;
  try { spec = apiConnectionSpec(status.baseUrl, 'chat'); }
  catch { return '<p class="muted small">接口地址待确认，请刷新服务状态。</p>'; }
  return `<section class="api-management-section"><h4>连接其他应用</h4><p class="muted small">选择应用使用的协议，填写地址、客户端密钥和模型 ID。这里不需要选择 API 来源。</p><div class="form-grid"><div class="field"><label for="apiConnectProtocol">接口协议</label><select id="apiConnectProtocol" class="input">${Object.entries(API_CONNECTION_PROTOCOLS).map(([id,row]) => `<option value="${id}">${row.name}</option>`).join('')}</select></div><div class="field"><label for="apiConnectModel">模型 ID（含配置的命名空间）</label><input id="apiConnectModel" class="input" maxlength="256" placeholder="从对应模型目录中获取"></div></div><div id="apiConnectDetails">${apiConnectionDetailsHTML(spec, 'chat')}</div><p class="muted small">示例中的 YOUR_SALCARA_CLIENT_KEY 需替换为自己的客户端密钥。密钥只在创建或重置时显示；可在高级设置中添加独立接入配置获取，已有密钥不会被自动重置。</p></section>`;
}
function apiConnectionDetailsHTML(spec, protocol) {
  const note = protocol === 'ollama' ? '兼容 chat、generate、tags；应用须能设置 Authorization 请求头。不提供模型下载、删除或嵌入接口。'
    : protocol === 'gemini' ? '兼容模型目录、generateContent 和 streamGenerateContent。Gemini SDK 的 base_url 使用下方地址，api_version 为 v1beta。'
    : protocol === 'anthropic' ? '支持标准 x-api-key 认证，以及 messages / count_tokens；实际模型能力由已加入的 API 决定。'
    : '接口能力由已加入的 API 决定；不能保留的跨协议参数会报错，不会静默忽略。';
  return `<div class="api-endpoint"><span class="muted small">Base URL / Host</span><code>${esc(spec.baseUrl)}</code><button class="btn sm" data-act="apiConnectCopy" data-kind="address">复制地址</button></div><p class="muted small">${esc(note)}</p><details class="api-pool-options"><summary>调用示例与模型目录</summary><p class="muted small">模型目录：<code class="api-connect-wrap">${esc(spec.models)}</code></p><p class="muted small">以下为 Bash / WSL 的 curl 示例。</p><pre class="api-connect-example">${esc(spec.example)}</pre><button class="btn sm" data-act="apiConnectCopy" data-kind="example">复制调用示例</button></details>`;
}
function currentAPIConnectionSpec() {
  const owner = requireAPIClientPanel('service');
  const status = CODEX_API_OVERVIEW.status;
  if (!status?.baseUrl || CODEX_API_OVERVIEW.error || owner.writeAttempted) throw new Error('请刷新服务状态后重试');
  return apiConnectionSpec(status.baseUrl, $('#apiConnectProtocol')?.value || 'chat', $('#apiConnectModel')?.value);
}
function updateAPIConnection() {
  if (!$('#apiConnectDetails') || !currentAPIClientPanel(apiClientPanel)) return;
  const expanded = $('#apiConnectDetails').querySelector('details')?.open;
  try {
    $('#apiConnectDetails').innerHTML = apiConnectionDetailsHTML(currentAPIConnectionSpec(), $('#apiConnectProtocol').value);
    const details = $('#apiConnectDetails').querySelector('details'); if (details && expanded) details.open = true;
  }
  catch (error) { $('#apiConnectDetails').textContent = error.message; }
}
document.addEventListener('change', event => { if (['apiConnectProtocol','apiConnectModel'].includes(event.target?.id)) updateAPIConnection(); });
document.addEventListener('input', event => { if (event.target?.id === 'apiConnectModel') updateAPIConnection(); });
ACTIONS.apiConnectCopy = async button => {
  const spec = currentAPIConnectionSpec();
  await copyText(button.dataset.kind === 'example' ? spec.example : spec.baseUrl, button.dataset.kind === 'example' ? '调用示例（请替换密钥和模型）' : '接口地址');
};
