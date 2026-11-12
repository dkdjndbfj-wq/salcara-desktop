'use strict';
/* Phone connection presentation. Requests and their ownership stay in remote.js. */

function remoteTabMarkup() {
  const active = rmEffectiveMode();
  return `<nav class="rm-tabs" aria-label="手机远程连接方式">${[['station', '中转站', 'server'], ['bot', 'Bot', 'box'], ['direct', '直连', 'link']].map(([id, label, glyph]) => `<button type="button" class="rm-tab ${RM.remoteMode === id ? 'on' : ''}" data-act="rmMode" data-mode="${id}" aria-pressed="${RM.remoteMode === id}">${icon(glyph)}<span>${label}</span>${active === (id === 'station' ? 'hub' : id) ? '<i class="rm-tab-enabled" title="当前启用" aria-label="当前启用"></i>' : ''}</button>`).join('')}</nav>`;
}

function rmScopedHubStatus(st) {
  const hub = st.hub || {}, c = st.config || {};
  if (hub.deviceId && hub.deviceId !== c.deviceId || hub.hubUrl && rmNormalizeHub(hub.hubUrl) !== rmNormalizeHub(c.effectiveHubUrl || c.hubUrl)) return { state: 'connecting' };
  if (hub.pairing?.deviceId && hub.pairing.deviceId !== c.deviceId) return { ...hub, pairing: undefined };
  return hub;
}

function rmNeedsEnrollment(hub) {
  return ['device_auth_required', 'invalid_key'].includes(hub?.state) || /device_|enrollment_|hub_identity_changed/.test(hub?.errorCode || '');
}

function rmConnectionStepsMarkup(st = S.state || {}) {
  const c = st.config || {}, active = rmEffectiveMode(c), hub = rmScopedHubStatus(st), d = RM.direct || {};
  const progress = RM.connectionAction?.mode === 'station' && RM.connectionAction.current() ? RM.connectionProgress : null;
  const enabled = active !== 'none';
  const connected = !progress && (active === 'direct' ? !!d.connected : enabled && hub.state === 'connected');
  const confirmed = active === 'hub' && !!c.remoteAgentSettingsScope && RM.confirmedRegistration?.deviceId === c.deviceId && RM.confirmedRegistration?.hubUrl === rmNormalizeHub(c.effectiveHubUrl) && RM.confirmedRegistration?.settingsScope === c.remoteAgentSettingsScope && RM.confirmedRegistration?.revision === RM.connectionRevision && !rmNeedsEnrollment(hub);
  const ready = progress ? !!progress.registered : active === 'direct' ? !!d.running : connected || confirmed;
  const eligible = progress ? !!progress.eligible : connected || confirmed;
  const discovered = progress ? !!progress.discovered : connected || confirmed;
  const paired = !progress && (active === 'direct' ? !!(d.paired || c.remoteModes?.direct?.paired) : !!hub.pairing?.paired);
  const error = progress ? '' : active === 'direct' ? d.error || (d.enabled && !d.running ? '直连监听未启动，请刷新状态后重试。' : '') : enabled ? hub.error || '' : '';
  const issue = !!error || !progress && enabled && active !== 'direct' && (!!hub.errorCode || ['device_auth_required', 'invalid_key', 'source_unauthorized', 'pair_required', 'remote_blocked'].includes(hub.state));
  const label = st.loading ? '正在读取连接状态' : progress?.state === 'discovering' ? '正在发现中转站' : progress?.state === 'registering' ? '正在验证授权并登记电脑' : !enabled ? '尚未启用远程' : active === 'direct' ? d.error ? '直连暂不可用' : connected ? '手机已连接' : ready ? '直连已就绪' : '正在启动直连' : rmConnectionLabel(hub);
  const step = (title, detail, state) => `<li class="${state}"><i aria-hidden="true">${state === 'done' ? '✓' : state === 'issue' ? '!' : '·'}</i><div><b>${title}</b><span>${esc(detail)}</span></div></li>`;
  return `<div class="rm-connection-heading"><span class="rm-dot ${issue ? 'bad' : connected ? 'ok' : enabled ? 'wait' : ''}"></span><strong>${esc(label)}</strong></div>
    <details class="rm-check-details" ${issue || progress || enabled && !connected ? 'open' : ''}><summary>连接检查</summary><ol class="rm-connection-steps">${active === 'direct' && !progress ? step('电脑监听', ready ? '加密直连已就绪' : '启用后自动检查', ready ? 'done' : issue ? 'issue' : 'pending') : `${step('发现中转站', discovered ? '远程服务已发现' : '等待服务确认', discovered ? 'done' : 'pending')}${step('验证远程资格', eligible ? '服务器已确认授权' : '按站点策略验证', eligible ? 'done' : 'pending')}${step('登记电脑', ready ? '服务器已确认电脑登记' : '等待登记确认', ready ? 'done' : issue ? 'issue' : 'pending')}`}${step('等待在线', connected ? active === 'direct' ? '手机已连接电脑' : '电脑与远程服务已连接' : enabled ? '等待连接恢复' : '连接后自动检查', connected ? 'done' : 'pending')}${step(active === 'bot' ? 'Bot 授权' : '手机绑定', active === 'bot' ? connected ? 'Bot 组授权已确认' : '由 Bot 入口确认组授权' : paired ? '手机绑定已保留' : '在下方完成手机绑定', active === 'bot' ? connected ? 'done' : 'pending' : paired ? 'done' : 'pending')}</ol></details>
    ${active === 'hub' || active === 'bot' ? `<p class="rm-connection-address" id="rmHubConnectionMeta">${esc(host(c.relayRoot || c.effectiveHubUrl))}</p>` : ''}
    <div id="rmHubConnectionError">${error ? `<div class="rm-error" role="status">${esc(error)}</div>` : ''}${issue ? rmConnectionHelpMarkup(active, hub) : ''}</div>`;
}

function rmConnectionHelpMarkup(mode, hub) {
  if (mode === 'direct') return '<button class="rm-link" data-act="rmDirectTutorial">检查局域网 / VPN 与直连设置</button><a class="rm-link" href="#settings">查看运行日志</a>';
  const code = hub?.errorCode || '', state = hub?.state || '';
  if (state === 'source_unauthorized' || /api_credentials|api_enrollment|source_/.test(code)) return '<a class="rm-link" href="#accounts">检查 API 与站点授权</a>';
  if (mode === 'hub' && rmNeedsEnrollment(hub)) return '<button class="rm-link" data-act="rmShowRecovery" data-section="enrollment">检查电脑登记</button>';
  if (mode === 'bot') return '<button class="rm-link" data-act="rmShowRecovery" data-section="bot">检查 Bot 地址与绑定</button>';
  return '<a class="rm-link" href="#settings">查看运行日志</a>';
}

function rmComputerMarkup(c) {
  return `<div class="rm-current-computer"><span class="rm-device-icon">${icon('laptop')}</span><div class="rm-grow"><span class="rm-field-label">这台电脑</span><div class="rm-device-name" id="devName">${S.editingName ? `<input class="input" id="devInput" aria-label="电脑名称" value="${esc(c.deviceName)}" maxlength="40"><button class="btn sm primary" data-act="saveName">保存</button><button class="btn sm ghost" data-act="cancelName">取消</button>` : `${esc(c.deviceName || '我的电脑')}<button class="icon-btn" data-act="editName" aria-label="修改电脑名称">${icon('edit')}</button>`}</div></div></div>`;
}

function rmAgentScopeMarkup(c = S.state?.config || {}) {
  const active = rmEffectiveMode(c);
  return active === 'none' ? '尚未启用远程；这里设置手机新任务使用的 API。' : `当前连接：${rmActiveTarget(c).label}。${active === 'direct' ? c.remoteModes?.direct?.paired ? '直连 API 单独保存；站点来源授权不影响直连。' : '完成直连手机配对后可单独设置 API。' : 'API 来源与模型须通过当前站点授权。'}切换待选目标不会应用设置。`;
}

function rmConnectionManageMarkup(entry) {
  if (S.state?.config?.remoteConnectionManagement !== true || !entry.revision) return '';
  const mode = entry.mode === 'bot' || entry.groupId || entry.botGroupId ? 'bot' : 'hub';
  const group = String(entry.groupId || entry.botGroupId || '');
  const current = rmConnectionTarget(mode, entry.hubUrl, group).key === rmActiveTarget().key;
  const attrs = `data-mode="${mode}" data-hub="${esc(entry.hubUrl)}" data-group="${esc(group)}"`;
  return `<details class="rm-connection-menu"><summary aria-label="管理 ${esc(entry.name || host(entry.hubUrl))}" title="管理连接">⋯</summary><div><button class="rm-link" data-act="rmConnectionEdit" data-operation="rename" ${attrs}>重命名</button><button class="rm-link bad" data-act="rmConnectionEdit" data-operation="remove" ${attrs} ${current ? 'disabled title="请先停用或切换当前连接"' : ''}>移除本机保存</button></div></details>`;
}

function renderRemoteDirectPanel(withSelfhost = true) {
  if (withSelfhost && typeof selfhostRememberDraft === 'function') selfhostRememberDraft();
  const d = RM.direct || {}, active = rmEffectiveMode() === 'direct';
  const paired = d.paired || S.state?.config?.remoteModes?.direct?.paired;
  return `<section class="rm-mode-panel" id="rmDirectPanel" aria-label="直连接入">
    <div class="rm-mode-head"><div><h3>电脑直连</h3><p>手机与电脑在同一局域网或 VPN，通过加密连接直接通信。</p></div></div>
    <div class="rm-direct-address"><span class="rm-field-label">连接地址</span><b>${esc(active && d.running ? (d.secureUrls || []).join(', ') || '直连已启动' : '启用后显示')}</b></div>
    <div class="rm-mode-actions">${paired ? active && d.running ? '' : `<button class="btn primary" data-act="rmActivateMode" data-mode="direct" ${RM.connectionAction ? 'disabled' : ''}>${active ? '恢复直连' : '启用直连'}</button>` : active ? '' : `<button class="btn primary" data-act="rmDirectQR" ${RM.connectionAction ? 'disabled' : ''}>${icon('phone')}启用并配对</button>`}<button class="rm-help-link" data-act="rmDirectTutorial">直连说明</button></div>
    ${!active && paired ? '<p class="rm-recovery-note">已配对，未启用。启用后手机可继续连接，无需重新扫码。</p>' : ''}
  </section>${withSelfhost && typeof renderSelfhostHubPanel === 'function' ? renderSelfhostHubPanel() : ''}`;
}

function rmSavedBotsMarkup(c) {
  const hub = rmScopedHubStatus({ config: c, hub: S.state?.hub });
  return rmSavedConnections(c, 'bot').map(entry => {
    const group = String(entry.groupId || entry.botGroupId || '');
    const activeGroup = String(c.remoteModes?.bot?.groupId || '');
    const current = !!group && rmEffectiveMode(c) === 'bot' && activeGroup === group && rmNormalizeHub(entry.hubUrl) === rmNormalizeHub(c.effectiveHubUrl || c.hubUrl);
    const connected = current && c.loggedIn && hub.state === 'connected';
    const status = !current ? '已保存，未启用' : connected ? '已连接' : hub.error || hub.state === 'invalid_key' ? rmConnectionLabel(hub) : '正在连接';
    return `<div class="rm-saved-mode"><div><b>${esc(entry.name || group || 'Bot 中继')}</b><span>Bot · ${esc(host(entry.hubUrl))}${group ? ` · 组 ${esc(group)}` : ''}</span></div><span class="rm-mode-state"><i class="rm-dot ${connected ? 'ok' : current ? 'wait' : ''}"></i>${esc(status)}</span>${current ? '' : `<button class="btn sm" data-act="rmActivateMode" data-mode="bot" data-hub="${esc(entry.hubUrl)}" data-group="${esc(group)}" ${RM.connectionAction ? 'disabled' : ''}>启用 Bot</button>`}${rmConnectionManageMarkup(entry)}</div>`;
  }).join('');
}

function renderRemoteBotPanel(c) {
  const entries = rmSavedConnections(c, 'bot');
  return `<section class="rm-mode-panel" id="rmBotPanel" aria-label="Bot 接入"><div class="rm-mode-head"><div><h3>Bot 中继</h3><p>导入管理员提供的绑定信息，Agent 继续运行在这台电脑上。</p></div></div>
    ${entries.length ? `<details class="rm-new-connection" ${RM.botDraft ? 'open' : ''}><summary>添加其他 Bot</summary>` : ''}
    <label class="field"><span>Bot 绑定信息</span><textarea class="input rm-bot-input" id="rmBotBinding" rows="5" placeholder="粘贴 Bot 绑定 JSON" aria-describedby="rmBotSecretNote">${esc(RM.botDraft)}</textarea></label>
    <p class="rm-recovery-note" id="rmBotSecretNote">绑定信息含授权邀请，请勿公开分享。导入后，以连接状态确认是否已上线。</p>
    <div class="rm-mode-actions"><button class="btn primary" data-act="rmBotBind">导入并连接</button></div><div id="rmBotError" aria-live="polite"></div>${entries.length ? '</details>' : ''}</section>`;
}

function rmBotIngressSettings(c) {
  const entries = rmSavedConnections(c, 'bot');
  if (!entries.length) return '';
  return `<details class="rm-adv" id="rmBotRecovery"><summary>更新 Bot 连接地址</summary><p class="rm-recovery-note">旧地址无法连接时，验证新地址是否属于原来的 Bot。验证通过才保存，原手机绑定和电脑身份会保留。</p>${entries.map((entry, index) => {
    const group = String(entry.groupId || entry.botGroupId || ''), key = JSON.stringify([rmNormalizeHub(entry.hubUrl), group]);
    return `<div class="rm-ingress-row"><label class="field"><span>${esc(entry.name || host(entry.hubUrl))} · 组 ${esc(group)}</span><input class="input" type="url" id="rmBotIngress-${index}" data-rm-ingress="${esc(key)}" value="${esc(RM.botIngressDrafts?.[key] || '')}" placeholder="新的 HTTPS 服务地址" autocomplete="url"></label><button class="btn sm" data-act="rmBotIngress" data-field="rmBotIngress-${index}" data-hub="${esc(entry.hubUrl)}" data-group="${esc(group)}" ${RM.connectionAction ? 'disabled' : ''}>验证并更新</button><div id="rmBotIngressError-${index}" aria-live="polite"></div></div>`;
  }).join('')}</details>`;
}

function rmEnrollmentRecoveryMarkup() {
  return `<div class="rm-enrollment-recovery"><p class="rm-recovery-note">先检查同一台电脑是否已完成登记。仅在服务器确认原登记未完成后，才允许重新尝试。</p><div class="rm-mode-actions"><button class="btn" data-act="rmConnect">检查登记结果</button><button class="btn" data-act="rmRetryEnrollment">重新尝试登记</button></div></div>`;
}

function renderRemoteStationPanel(_view, _st, c, linked) {
  const draft = RM.stationDraft;
  const apis = (S.local?.accounts || []).filter(a => a.baseUrl && !a.credentialRef && a.sourceType === 'api_key');
  const chosen = draft ? apis.find(a => a.id === draft.accountId) : apis.find(a => a.id === S.local?.activeCodexAccount) || apis[0];
  const url = draft ? draft.url : chosen?.baseUrl || c.relayRoot || '';
  const recovery = RM.pendingEnrollment?.key === rmEnrollmentTargetKey(chosen?.id, chosen?.sourceRevision, url, draft?.hubUrl) ? rmEnrollmentRecoveryMarkup() : '';
  const saved = rmSavedConnections(c, 'hub');
  const collapse = linked || saved.length > 0;
  return `<section class="rm-mode-panel" aria-label="中转站接入"><div class="rm-mode-head"><div><h3>中转站</h3><p>${apis.length ? '选择已保存的 API，验证本站远程授权后连接这台电脑。' : '先在「API 密钥」添加 API，或填写管理员提供的中转站地址。'}</p></div></div>
    ${collapse ? `<details class="rm-new-connection" ${draft?.url || draft?.claim ? 'open' : ''}><summary>添加其他中转站</summary>` : ''}
    ${apis.length ? `<label class="field"><span>使用的 API</span><select class="input" id="rmSourceAPI">${apis.map(a => `<option value="${esc(a.id)}" ${a.id === chosen?.id ? 'selected' : ''}>${esc(a.name || host(a.baseUrl))}</option>`).join('')}<option value="" ${!chosen ? 'selected' : ''}>其他地址</option></select></label>` : ''}
    <label class="field"><span>API 或中转站地址</span><input class="input" id="rmStation" placeholder="https://api.example.com/v1" value="${esc(url)}" autocomplete="url"></label>
    <details class="rm-adv"><summary>自定义接入地址</summary><label class="field"><span>Hub 地址（可选）</span><input class="input" id="rmHub" placeholder="自动发现，无需填写" value="${esc(draft?.hubUrl || '')}" autocomplete="url"></label><p class="rm-recovery-note">仅在站点管理员提供独立地址时填写。</p></details>
    <div class="rm-enrollment-fallback" id="rmEnrollmentFallback" ${RM.enrollmentRequired || draft?.claim ? '' : 'hidden'}><p class="rm-recovery-note">这个独立站点要求电脑登记码，请向站点管理员获取。</p><label class="field"><span>电脑登记码</span><input class="input" id="rmEnrollmentClaim" type="password" maxlength="64" autocomplete="off" autocapitalize="none" spellcheck="false" value="${esc(draft?.claim || '')}"></label></div>
    <div class="rm-mode-actions"><button class="btn primary" data-act="rmConnect">${collapse ? '连接其他站点' : '验证并连接'}</button></div><div id="rmConnectError" aria-live="polite">${recovery}</div>${collapse ? '</details>' : ''}</section>`;
}

function rmHubPhoneQRMarkup(st = S.state || {}) {
  const c = st.config || {}, hub = rmScopedHubStatus(st);
  const connected = rmEffectiveMode(c) === 'hub' && c.loggedIn && hub.state === 'connected';
  if (connected && S.pairQR && S.pairExpires > Date.now()) return `<img src="${esc(S.pairQR)}" alt="手机绑定二维码"><span class="rm-timer" id="rmTimer"></span>`;
  if (hub.pairing?.paired === true) return '';
  return `<button class="btn primary sm" data-act="rmShowQR" ${connected && !RM.connectionAction ? '' : 'disabled'}>${icon('scan')}扫码绑定手机</button>${connected ? '' : '<span>电脑连接成功后可扫码</span>'}`;
}

function rmPhoneBindingMarkup(st = S.state || {}) {
  const c = st.config || {}, hub = rmScopedHubStatus(st), active = rmEffectiveMode(c), mode = active, d = RM.direct || {};
  if (active === 'none') return '<p class="rm-recovery-note">尚未启用远程。已保存的手机配对会保留，启用连接后可查看或管理绑定。</p>';
  if (mode === 'direct') {
    const paired = d.paired || c.remoteModes?.direct?.paired, enabled = active === 'direct';
    const qrLive = enabled && !paired && d.qrDataURL && Number(d.expiresAt) > Date.now();
    return `<p class="rm-binding-state"><span class="rm-dot ${enabled && d.connected ? 'ok' : paired ? 'wait' : ''}"></span><strong>${enabled && d.connected ? '手机已连接' : paired ? '已配对' : '尚未绑定手机'}</strong></p>${qrLive ? `<div class="rm-direct-qr"><img src="${esc(d.qrDataURL)}" alt="直连配对二维码"><span>用手机「我的电脑」扫码，核对并确认连接。<br>有效至 ${esc(new Date(Number(d.expiresAt)).toLocaleTimeString())}</span></div>` : `<p class="rm-recovery-note">${paired ? '这台电脑保留一个直连手机配对。' : '用手机扫描配对二维码，核对并确认授权。'}</p>`}${paired ? `<button class="rm-link bad" data-act="rmDirectRevoke" ${RM.connectionAction ? 'disabled' : ''}>撤销手机配对</button>` : !qrLive ? `<button class="btn primary sm" data-act="rmDirectQR" ${RM.connectionAction ? 'disabled' : ''}>生成配对二维码</button>` : ''}`;
  }
  if (mode === 'bot') return `<p class="rm-recovery-note">在手机「我的电脑」导入同一份 Bot 绑定信息。电脑列表由该 Bot 的授权范围决定。</p><button class="rm-help-link" data-act="rmBotTutorial">查看手机绑定说明</button>`;
  const linked = active === 'hub' && c.loggedIn, connected = linked && hub.state === 'connected', paired = linked && hub.pairing?.paired === true;
  const unpaired = linked && hub.pairing?.paired === false;
  const qrLive = connected && S.pairQR && S.pairExpires > Date.now();
  return `<p class="rm-binding-state"><span class="rm-dot ${paired ? 'ok' : ''}"></span><strong>${paired ? '手机已绑定' : unpaired ? '需要扫码绑定手机' : '手机绑定待确认'}</strong></p><div class="rm-qr ${qrLive ? 'live' : ''}" id="pairQR" ${paired && !qrLive ? 'hidden' : ''}>${rmHubPhoneQRMarkup(st)}</div><p class="rm-recovery-note">${paired ? '手机已获授权，可使用这台电脑上的 Agent。更换手机前请先解除绑定。' : unpaired ? `${connected ? '' : rmNeedsEnrollment(hub) ? '先完成电脑登记并等待连接成功。' : '先完成电脑连接。'}中转站尚未确认手机绑定；如果这部手机以前连接过，请重新扫码。手机打开「编程 → 我的电脑」，扫码并确认授权。` : '连接中转站后核对手机绑定状态；需要绑定时，请在手机「编程 → 我的电脑」扫码。'}</p>${paired ? `<button class="rm-link bad" id="rmPairRevoke" data-act="pairRevoke" ${connected && !RM.connectionAction ? '' : 'disabled'}>解除手机绑定</button>` : ''}`;
}

function renderRemoteAdvanced(st) {
  const c = st.config || {}, linked = c.loggedIn && rmEffectiveMode(c) === 'hub';
  return `<details class="rm-sec rm-advanced" id="rmAdvanced"><summary><span>${icon('gear')}<b>高级设置</b></span><small>恢复、诊断与本机服务</small></summary><div class="rm-advanced-body">
    ${linked ? `<details class="rm-adv" id="rmEnrollmentRecovery"><summary>重新登记这台电脑</summary><p class="rm-recovery-note">设备凭据失效或站点重置后重新登记。完成后手机需要重新扫码；API、项目和聊天记录会保留。</p><label class="field"><span>设备登记码（按站点要求填写）</span><input class="input" id="rmRecoveryClaim" type="password" maxlength="64" autocomplete="off" placeholder="已有授权的站点可留空"></label><button class="btn" data-act="rmReenroll">重新登记</button><div id="rmReenrollError"></div></details>` : ''}
    ${rmBotIngressSettings(c)}${RM.remoteMode === 'direct' && typeof renderSelfhostHubPanel === 'function' ? renderSelfhostHubPanel() : ''}<section id="rmRecovery" aria-label="消息与恢复">${rmRecoveryMarkup(st.hub)}</section><nav class="rm-settings-links" aria-label="诊断操作"><button class="rm-link" data-act="rmCopyDiagnostics">复制诊断摘要</button><a class="rm-link" href="#settings">查看运行日志</a></nav><div id="rmLive"></div></div></details>`;
}

function rmConnectionSwitchMarkup(config) {
  const state = rmSwitchState(config), { draft, target } = state;
  return `<div class="rm-connection-switch"><label class="field"><span>添加 / 切换连接方式</span><select class="input" id="rmSwitchMode" aria-describedby="rmSwitchNote">${[['hub', '中转站'], ['bot', 'Bot'], ['direct', '直连']].map(([mode, label]) => `<option value="${mode}" ${state.mode === mode ? 'selected' : ''}>${label}</option>`).join('')}</select></label>
    <label class="field"><span>已保存目标</span><select class="input" id="rmSwitchTarget" aria-describedby="rmSwitchNote" ${state.targets.length ? '' : 'disabled'}>${!draft.key ? '<option value="" selected>请先在左侧添加连接</option>' : !target ? `<option value="${esc(draft.key)}" selected>${esc(draft.label || '原选择')}（已不可用）</option>` : ''}${state.targets.map(row => `<option value="${esc(row.key)}" ${row.key === draft.key ? 'selected' : ''}>${esc(row.label)}</option>`).join('')}</select></label>
    <p class="rm-recovery-note" id="rmSwitchNote" aria-live="polite">${esc(state.note)}</p><button class="btn sm" id="rmSwitchApply" data-act="rmActivateMode" data-target="switch" ${state.disabled ? 'disabled' : ''}>${state.pending ? '正在切换…' : '切换连接'}</button><div id="rmSwitchError" aria-live="polite">${draft.error ? `<div class="rm-error">${esc(draft.error)}</div>` : ''}</div></div>`;
}

function renderRemoteConnectionLayout(view, st) {
  const c = st.config || {}, linked = c.loggedIn && rmEffectiveMode(c) === 'hub';
  const saved = rmSavedConnections(c, 'hub').length + rmSavedConnections(c, 'bot').length;
  const stations = rmSavedConnections(c, 'hub').length && typeof remoteSavedStationsHTML === 'function' ? remoteSavedStationsHTML(c) : '';
  return `<div class="rm-page rm-connect-page"><div id="rmModeError" aria-live="polite"></div><div id="approvalsBox"></div>${rmComputerMarkup(c)}<div class="rm-connect-grid"><section class="rm-sec rm-connect-main" aria-label="所选连接方式的配置">${RM.remoteMode === 'station' ? renderRemoteStationPanel(view, st, c, linked) : RM.remoteMode === 'bot' ? renderRemoteBotPanel(c) : renderRemoteDirectPanel(false)}${saved ? `<details class="rm-saved-connections" open><summary>已保存连接 · ${saved}</summary>${stations}<div id="rmSavedBots" class="rm-saved-modes">${rmSavedBotsMarkup(c)}</div></details>` : ''}</section>
    <aside class="rm-connect-aside"><section class="rm-sec" aria-labelledby="rmStatusTitle"><div class="rm-sec-head"><h3 id="rmStatusTitle">连接状态</h3><button class="rm-link" data-act="rmRecoveryRefresh" aria-label="刷新连接状态">${icon('refresh')}刷新</button></div><div id="rmConnectionSteps" aria-live="polite">${rmConnectionStepsMarkup(st)}</div><div id="rmActiveMode">${rmModeStatusMarkup(c)}</div>${rmConnectionSwitchMarkup(c)}</section><section class="rm-sec" id="rmPairSection" aria-labelledby="rmBindingTitle"><div class="rm-sec-head"><h3 id="rmBindingTitle">已绑定手机</h3></div><div id="rmPhoneBinding">${rmPhoneBindingMarkup(st)}</div></section></aside></div>
    <details class="rm-sec rm-advanced" id="rmAgentSettings" ${RM.agentSettingsOpen ? 'open' : ''}><summary><span>${icon('code')}<b>手机任务使用的 API</b></span><small>跟随电脑或单独设置</small></summary><p class="rm-recovery-note rm-agent-scope" id="rmAgentScope">${esc(rmAgentScopeMarkup(c))}</p><div class="rm-agents" id="rmAgents"><p class="rm-recovery-note">展开后读取 Agent 与 API 配置。</p></div></details>
    <nav class="rm-settings-links" aria-label="远程权限设置"><a class="rm-link" href="#settings">目录授权 · ${c.projects?.length || 0} 个 / 审批 · ${esc({ ask: '每次询问', auto_edits: '自动改文件', auto_all: '全部自动' }[c.approval] || '每次询问')}</a><a class="rm-link" href="#accounts">API 来源权限</a></nav>${renderRemoteAdvanced(st)}</div>`;
}
