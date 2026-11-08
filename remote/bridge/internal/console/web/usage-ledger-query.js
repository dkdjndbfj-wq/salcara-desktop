'use strict';
Object.assign(LOCAL_LEDGER, { query: null, filter: {}, appliedFilter: {}, pendingFilter: null, readSeq: 0, dimension: 'model', accountLimit: 12, accountIDs: [] });
function ledgerNumber(value) { return Number.isFinite(value) && value >= 0 ? value !== 0 && value < 1e-8 ? value.toPrecision(8) : value.toLocaleString(undefined, { maximumFractionDigits: 8 }) : '未知'; }
function ledgerSum(value) { return value?.sum == null ? '未知' : `${ledgerToken(value.sum)}${value.unknownRecords ? '（已知部分）' : ''}`; }
function ledgerOptions(values, selected) { return values.map(([value, label]) => `<option value="${esc(value)}" ${value === String(selected || '') ? 'selected' : ''}>${esc(label)}</option>`).join(''); }
function ledgerField(id, label, value, type = 'text', extra = '') { return `<div class="field"><label for="${id}">${label}</label><input class="input" id="${id}" type="${type}" value="${esc(value ?? '')}" ${extra}></div>`; }
function ledgerSelect(id, label, values, value) { return `<div class="field"><label for="${id}">${label}</label><select class="input" id="${id}">${ledgerOptions(values, value)}</select></div>`; }
function ledgerLocalDate(ms) { const date = new Date(ms); return new Date(ms - date.getTimezoneOffset()*60000).toISOString().slice(0,16); }
function ledgerAccountLabel(id, data=LOCAL_LEDGER.query||{}) {
  const name=data.accountNames?.[id],source=data.accountSources?.[id];
  return [typeof name==='string'&&name.trim()?name:'历史 / 未命名账号',source?.host||'',id?String(id).slice(-8):'未记录来源'].filter(Boolean).join(' · ');
}
function ledgerAccountOptions() {
  const q=LOCAL_LEDGER.query||{},ids=new Set([...(LOCAL_LEDGER.accountIDs||[]),...Object.keys(q.accountNames||{}),...(q.byAccount||[]).map((g)=>g.key),LOCAL_LEDGER.appliedFilter?.accountId,LOCAL_LEDGER.pendingFilter?.filter.accountId].filter((id)=>typeof id==='string'&&/^acct-[a-fA-F0-9]{24}$/.test(id)));
  return [['','全部 API / 账号'],...[...ids].map((id)=>[id,ledgerAccountLabel(id,q)])];
}
function ledgerAccountStats(summary={}) {
  return [['输入',summary.inputTokens],['输出',summary.outputTokens],['读缓存',summary.cachedInputTokens],['建缓存',summary.cacheCreationInputTokens]].map(([label,value])=>`<div><span>${label}</span><b>${esc(ledgerSum(value))}</b></div>`).join('');
}
function ledgerAccountsHTML() {
  const q=LOCAL_LEDGER.query||{},selected=LOCAL_LEDGER.appliedFilter?.accountId||'',groups=q.byAccount||[],shown=groups.slice(0,LOCAL_LEDGER.accountLimit);
  const scope=selected?ledgerAccountLabel(selected,q):'全部 API / 账号';
  return `<section class="ledger-accounts"><div class="ledger-account-heading">${ledgerSelect('ledgerAccount','API / 账号',ledgerAccountOptions(),selected)}<p class="small muted" title="名称与地址来自当前本机配置；统计按独立匿名账号 ID 区分，不按中转站或名称合并。">统计范围：${esc(scope)}<br>仅本机记录，不是账号额度或账单</p></div>${selected?'':`<div class="ledger-account-grid">${shown.map((g)=>{const valid=/^acct-[a-fA-F0-9]{24}$/.test(g.key||''),source=q.accountSources?.[g.key];return `<article class="card ledger-account-card" data-account-id="${esc(g.key||'')}"><div class="ledger-account-title"><b title="${esc(g.key||'未记录来源')}">${esc(ledgerAccountLabel(g.key,q))}</b><span class="chip">${source?.kind==='official_account'?'官方账号':source?.kind==='api_key'?'API':'本机记录'}</span></div><div class="ledger-account-stats">${ledgerAccountStats(g.summary)}</div><div class="ledger-account-foot"><span class="small muted">${esc(ledgerToken(g.summary?.records))} 次请求</span><button class="btn sm" data-act="usageLedgerAccount" data-account-id="${esc(g.key||'')}" ${!valid||selected===g.key?'disabled':''}>${selected===g.key?'已选中':'查看记录'}</button></div></article>`;}).join('')||`<p class="small muted">${q.available===false?'暂时无法读取账号统计':'当前范围暂无本机用量'}</p>`}</div>${groups.length>shown.length?'<button class="btn sm" data-act="usageLedgerAccountsMore">显示更多账号</button>':''}`}</section>`;
}
function ledgerFilterDraft() {
  const f=LOCAL_LEDGER.filter,read=(id,fallback)=>$('#'+id)?.value ?? fallback ?? '';
  return {from:read('ledgerFrom',f.from?ledgerLocalDate(f.from):''),to:read('ledgerTo',f.to?ledgerLocalDate(f.to):''),model:read('ledgerModel',f.model),modelContains:read('ledgerContains',f.modelContains),accountId:read('ledgerAccount',f.accountId),clientId:read('ledgerClient',f.clientId),routeId:read('ledgerRoute',f.routeId),state:read('ledgerState',f.state),httpStatus:read('ledgerHTTP',f.httpStatus),serviceTier:read('ledgerTier',f.serviceTier),sort:read('ledgerSort',f.sort||'time'),direction:read('ledgerDirection',f.direction||'desc')};
}
function ledgerFilterHTML() {
  const f=ledgerFilterDraft(),s=LOCAL_LEDGER.status||{},names=(values)=>[['','全部'],...Object.entries(values||{}).map(([id,name])=>[id,name||id])];
  return `${ledgerAccountsHTML()}<details class="adv"><summary>筛选与排序</summary><div class="form-grid">${ledgerField('ledgerFrom','开始时间（本地）',f.from,'datetime-local')}${ledgerField('ledgerTo','结束时间（本地）',f.to,'datetime-local')}${ledgerField('ledgerModel','精确模型',f.model,'text','maxlength="200"')}${ledgerField('ledgerContains','模型包含',f.modelContains,'text','maxlength="200"')}${ledgerSelect('ledgerClient','客户端',names(s.clientNames),f.clientId)}${ledgerField('ledgerRoute','匿名路由 ID',f.routeId)}${ledgerSelect('ledgerState','结果',[['','全部'],...['completed','incomplete','failed','interrupted','cancelled','rejected','unknown','read_only','local_estimate'].map((state)=>[state,ledgerState(state)])],f.state)}${ledgerField('ledgerHTTP','HTTP 状态（留空为全部）',f.httpStatus,'number','min="0" max="599"')}${ledgerSelect('ledgerTier','上游服务层级',[['','全部'],['default','default'],['standard','standard'],['priority','priority'],['flex','flex'],['auto','auto']],f.serviceTier)}${ledgerSelect('ledgerSort','排序',[['time','时间'],['duration','耗时'],['model','模型'],['state','结果']],f.sort)}${ledgerSelect('ledgerDirection','方向',[['desc','降序'],['asc','升序']],f.direction)}</div><div class="row"><button class="btn sm primary" data-act="usageLedgerFilter">应用筛选</button><button class="btn sm" data-act="usageLedgerReset">重置筛选</button></div><p class="small muted">统计与列表按已应用的筛选范围；修改输入后点击应用筛选。已删除账号或客户端可使用匿名 ID。</p>${ledgerField('ledgerAccountID','匿名账号 ID（优先于账号选择）',$('#ledgerAccountID')?.value||'')}${ledgerField('ledgerClientID','匿名客户端 ID（优先于客户端选择）',$('#ledgerClientID')?.value||'')}</details>`;
}
function ledgerSummaryHTML(summary={}) {
  const stats=[['输入 token',ledgerSum(summary.inputTokens)],['输出 token',ledgerSum(summary.outputTokens)],['读缓存 token',ledgerSum(summary.cachedInputTokens)],['建缓存 token',ledgerSum(summary.cacheCreationInputTokens)],['推理 token',ledgerSum(summary.reasoningOutputTokens)],['总 token',ledgerSum(summary.totalTokens)]];
  return `<div class="ledger-token-grid">${stats.map(([label,value])=>`<div class="card stat"><div class="stat-label">${label}</div><div class="stat-value">${esc(value)}</div></div>`).join('')}</div><p class="small muted">未知 Token 保留为未知，统计仅包含已知部分。</p>`;
}
function ledgerGroupsHTML() {
  const q=LOCAL_LEDGER.query||{},dimension=LOCAL_LEDGER.dimension,groups=({model:q.byModel,account:q.byAccount,client:q.byClient,route:q.byRoute,date:q.trend})[dimension]||[];
  const label=(g)=>dimension==='date'?new Date(g.time).toISOString().slice(0,10):dimension==='account'?ledgerLabel(q.accountNames,g.key,'未知账号'):dimension==='client'?ledgerLabel(q.clientNames,g.key,'未知客户端'):g.key||'未知';
  return `${ledgerSelect('ledgerDimension','汇总维度',[['model','模型'],['account','账号'],['client','客户端'],['route','匿名路由'],['date','日期（UTC）']],dimension)}<div class="table-wrap"><table class="table"><thead><tr><th>分组</th><th>请求 / 结果</th><th>输入 / 输出</th></tr></thead><tbody>${groups.map((g)=>`<tr><td>${esc(label(g))}</td><td>${g.summary.records} 条<br>${esc(Object.entries(g.summary.states||{}).map(([state,count])=>`${ledgerState(state)} ${count}`).join(' · '))}</td><td>${esc(ledgerSum(g.summary.inputTokens))} / ${esc(ledgerSum(g.summary.outputTokens))}</td></tr>`).join('')||'<tr><td colspan="3" class="muted">此筛选范围暂无记录</td></tr>'}</tbody></table></div>`;
}
drawLocalLedger=function(){
  if(!ledgerIsCurrent(LOCAL_LEDGER.epoch))return;
  const content=$('#usageLedgerContent');
  if(LOCAL_LEDGER.pendingFilter){
    const height=content.getBoundingClientRect?.().height;
    if(!content.style.minHeight&&Number.isFinite(height)&&height>0)content.style.minHeight=Math.ceil(height)+'px';
    content.innerHTML=`<div class="stack">${ledgerSelect('ledgerAccount','API / 账号',ledgerAccountOptions(),LOCAL_LEDGER.pendingFilter.filter.accountId)}<div class="muted small" role="status">正在读取…</div></div>`;
    // The previous account may have been selected from cards farther down the
    // modal. Keep the dialog's height, but expose the new scope and its status.
    const scroller=content.closest('.modal-body');
    if(scroller)scroller.scrollTop=0;
    return;
  }
  content.style.minHeight='';
  const status=LOCAL_LEDGER.status,q=LOCAL_LEDGER.query||{},filters=ledgerFilterHTML();
  const issues=[status.readError,status.saveError,status.dropped?`记录队列曾丢弃 ${status.dropped} 条；不能将记录当作完整账单。`:'',status.unsaved?'含未保存记录。统计仅使用已保存数据；重试保存不会重发推理。':''].filter(Boolean);
  $('#usageLedgerContent').innerHTML=`<div class="stack"><p class="small muted">记录本机请求和 Token 用量，不保存提示词、正文或密钥。</p>${issues.length?`<div class="card small" role="alert">${issues.map(esc).join('<br>')}</div>`:''}<div class="row"><span class="chip">已保存筛选结果 ${q.total == null ? '未知' : q.total} 条 · 本机 ${status.count} 条 · 待处理 ${status.pending}</span><button class="btn sm" data-act="usageLedgerRefresh">刷新</button><button class="btn sm" data-act="usageLedgerRetry">重试保存</button><button class="btn sm ghost danger-text" data-act="usageLedgerClear">清空记录</button></div>${filters}${ledgerSummaryHTML(q.summary)}<details class="adv"><summary>分组统计</summary>${ledgerGroupsHTML()}</details><div class="form-grid">${ledgerField('ledgerRecords','最多保留条数',status.retention.maxRecords,'number','min="1" max="4096"')}${ledgerField('ledgerDays','保留天数',status.retention.days,'number','min="1" max="365"')}</div><button class="btn sm" data-act="usageLedgerRetention">保存保留设置</button><div class="table-wrap"><table class="table"><thead><tr><th>时间 / 模型</th><th>结果 / HTTP</th><th>输入 / 输出</th><th>合计 / 耗时</th></tr></thead><tbody>${LOCAL_LEDGER.rows.map((row)=>`<tr><td>${esc(new Date(row.time).toLocaleString())}<br>${esc(row.model||'未知模型')}<br><span class="muted small">${esc(ledgerLabel(status.accountNames,row.accountId,'未知账号'))}<br>${esc(ledgerLabel(status.clientNames,row.clientId,row.routeId))}${row.serviceTier?'<br>层级 '+esc(row.serviceTier):''}</span></td><td>${esc(ledgerState(row.state))}<br>${row.httpStatus||'未知'}${row.upstreamHttpStatus?' / 上游 '+row.upstreamHttpStatus:''}${!row.usageFinal?'<br><span class="muted small">非最终用量或未知</span>':''}</td><td>${esc(ledgerToken(row.tokens?.inputTokens))} / ${esc(ledgerToken(row.tokens?.outputTokens))}<br><span class="small muted">缓存 ${esc(ledgerToken(row.tokens?.cachedInputTokens))} · 建缓存 ${esc(ledgerToken(row.tokens?.cacheCreationInputTokens))} · 推理 ${esc(ledgerToken(row.tokens?.reasoningOutputTokens))}</span></td><td>${esc(ledgerToken(row.tokens?.totalTokens))}<br>${Number.isSafeInteger(row.durationMs)?row.durationMs:'未知'} ms</td></tr>`).join('')||'<tr><td colspan="4" class="muted">'+(q.available === false ? '数据库读取失败，记录数未知；可重试保存或明确清空恢复。' : '暂无本机请求记录')+'</td></tr>'}</tbody></table></div>${LOCAL_LEDGER.rows.length<(q.total||0)?'<button class="btn sm" data-act="usageLedgerMore">读取更早记录</button>':''}</div>`;
};
loadLocalLedger=async function(more=false){
  // Pagination belongs to the last applied scope. It cannot supersede an
  // explicit account/filter switch that is still being read.
  if(more&&LOCAL_LEDGER.pendingFilter)return false;
  const epoch=LOCAL_LEDGER.epoch,seq=++LOCAL_LEDGER.readSeq,params=new URLSearchParams({limit:'100'}),filter={...(more?LOCAL_LEDGER.appliedFilter:LOCAL_LEDGER.filter)};
  if(!more&&(LOCAL_LEDGER.pendingFilter||[...new Set([...Object.keys(filter),...Object.keys(LOCAL_LEDGER.appliedFilter)])].some(key=>filter[key]!==LOCAL_LEDGER.appliedFilter[key]))){
    LOCAL_LEDGER.pendingFilter={seq,filter};
    drawLocalLedger();
  }
  for(const [key,value]of Object.entries(filter))if(value!==''&&value!=null)params.set(key,String(value));
  if(more&&LOCAL_LEDGER.rows.length){params.set('offset',String(LOCAL_LEDGER.rows.length));params.set('revision',LOCAL_LEDGER.query.revision);}
  let out;try{out=await api('/api/local/usage-ledger/query?'+params);}catch(error){
    if(ledgerIsCurrent(epoch)&&LOCAL_LEDGER.readSeq===seq&&!LOCAL_LEDGER.status){try{const status=await api('/api/local/usage-ledger?limit=100');if(ledgerIsCurrent(epoch)&&LOCAL_LEDGER.readSeq===seq){LOCAL_LEDGER.status=status;LOCAL_LEDGER.query={available:false,total:null};LOCAL_LEDGER.rows=[];drawLocalLedger();}}catch(_){/* Keep original query failure visible. */}}
    if(ledgerIsCurrent(epoch)&&LOCAL_LEDGER.readSeq===seq){LOCAL_LEDGER.pendingFilter=null;LOCAL_LEDGER.filter={...LOCAL_LEDGER.appliedFilter};if(LOCAL_LEDGER.status)drawLocalLedger();throw error;}return false;
  }
  if(!ledgerIsCurrent(epoch)||LOCAL_LEDGER.readSeq!==seq)return false;
  LOCAL_LEDGER.pendingFilter=null;
  LOCAL_LEDGER.rows=more?[...LOCAL_LEDGER.rows,...out.records]:out.records;LOCAL_LEDGER.query=out;LOCAL_LEDGER.appliedFilter=filter;LOCAL_LEDGER.filter={...filter};LOCAL_LEDGER.accountIDs=[...new Set([...LOCAL_LEDGER.accountIDs,...(out.byAccount||[]).map((g)=>g.key)])];LOCAL_LEDGER.status={...out.status,accountNames:out.accountNames,accountSources:out.accountSources,clientNames:out.clientNames};drawLocalLedger();return true;
};
openLocalLedger=async function(){
  officialPanel('本机请求记录','<div id="usageLedgerContent" aria-live="polite">正在读取…</div>');const epoch=OFFICIAL_PANEL.epoch;
  Object.assign(LOCAL_LEDGER,{epoch,rows:[],status:null,query:null,filter:{},appliedFilter:{},pendingFilter:null,readSeq:0,dimension:'model',accountLimit:12,accountIDs:[]});
  try{await loadLocalLedger();}catch(error){if(ledgerIsCurrent(epoch))$('#officialError').textContent=error.message;}
};
reloadLocalLedger=async function(button,more=false){const epoch=LOCAL_LEDGER.epoch;try{await busy(button,async()=>{if(await loadLocalLedger(more)&&ledgerIsCurrent(epoch))$('#officialError').textContent='';});}catch(error){if(ledgerIsCurrent(epoch))$('#officialError').textContent=error.message;}};
function ledgerReadFilter(){
  const raw=ledgerFilterDraft(),f={};for(const key of ['from','to'])if(raw[key]){const ms=new Date(raw[key]).getTime();if(!Number.isSafeInteger(ms)||ms<=0)throw new Error('时间筛选无效');f[key]=ms;}if(f.from&&f.to&&f.from>f.to)throw new Error('开始时间不能晚于结束时间');
  const validModel=(v)=>/^[A-Za-z0-9][A-Za-z0-9._:/-]{0,199}$/.test(v)&&!v.includes('://')&&!/^(sk-|bearer)/i.test(v);for(const key of ['model','modelContains'])if(raw[key]){if(!validModel(raw[key]))throw new Error('模型筛选值无效');f[key]=raw[key];}
  const account=$('#ledgerAccountID')?.value||raw.accountId,client=$('#ledgerClientID')?.value||raw.clientId;
  for(const [key,value,prefix]of [['accountId',account,'acct'],['clientId',client,'client'],['routeId',raw.routeId,'route']])if(value){if(!new RegExp('^'+prefix+'-[a-fA-F0-9]{24}$').test(value))throw new Error('匿名 ID 无效');f[key]=value;}
  if(raw.httpStatus!==''){if(!/^\d{1,3}$/.test(raw.httpStatus)||Number(raw.httpStatus)>599)throw new Error('HTTP 状态为 0–599');f.httpStatus=Number(raw.httpStatus);}
  if(raw.state)f.state=raw.state;if(raw.serviceTier)f.serviceTier=raw.serviceTier;f.sort=raw.sort;f.direction=raw.direction;
  if(!['time','duration','model','state'].includes(f.sort)||!['asc','desc'].includes(f.direction)||f.state&&!['completed','incomplete','failed','interrupted','cancelled','rejected','unknown','read_only','local_estimate'].includes(f.state)||f.serviceTier&&!['standard','default','priority','flex','auto'].includes(f.serviceTier))throw new Error('筛选选择无效');return f;
}
Object.assign(ACTIONS,{
  usageLedgerAccount:(button)=>{
    if(!ledgerIsCurrent(LOCAL_LEDGER.epoch)||button?.isConnected===false)return;
    const id=button?.dataset?.accountId??$('#ledgerAccount')?.value??'';
    if(id&&!/^acct-[a-fA-F0-9]{24}$/.test(id))return;
    // Account selection keeps the applied time/model scope, not unsaved drafts.
    // A previous manual ID must not silently override this explicit selection.
    if($('#ledgerAccountID'))$('#ledgerAccountID').value='';
    LOCAL_LEDGER.filter={...LOCAL_LEDGER.appliedFilter};
    if(id)LOCAL_LEDGER.filter.accountId=id;else delete LOCAL_LEDGER.filter.accountId;
    return reloadLocalLedger(button);
  },
  usageLedgerAccountsMore:()=>{if(ledgerIsCurrent(LOCAL_LEDGER.epoch)){LOCAL_LEDGER.accountLimit+=12;drawLocalLedger();}},
  usageLedger:openLocalLedger,usageLedgerRefresh:(button)=>reloadLocalLedger(button),usageLedgerMore:(button)=>reloadLocalLedger(button,true),
  usageLedgerFilter:(button)=>{try{LOCAL_LEDGER.filter=ledgerReadFilter();return reloadLocalLedger(button);}catch(error){$('#officialError').textContent=error.message;}},
  usageLedgerReset:(button)=>{LOCAL_LEDGER.filter={};for(const id of ['ledgerFrom','ledgerTo','ledgerModel','ledgerContains','ledgerAccount','ledgerClient','ledgerRoute','ledgerState','ledgerHTTP','ledgerTier','ledgerAccountID','ledgerClientID'])if($('#'+id))$('#'+id).value='';if($('#ledgerSort'))$('#ledgerSort').value='time';if($('#ledgerDirection'))$('#ledgerDirection').value='desc';return reloadLocalLedger(button);},
  usageLedgerClear:async(button)=>{if(await confirm('清空本机请求记录？保留设置不变，新请求仍会记录。'))return mutateLocalLedger(button,'/api/local/usage-ledger/clear',{confirmed:true});},
});
if(typeof document!=='undefined')document.addEventListener('change',(event)=>{
  if(!ledgerIsCurrent(LOCAL_LEDGER.epoch))return;
  if(event.target?.id==='ledgerAccount'&&event.target.isConnected!==false){void ACTIONS.usageLedgerAccount(event.target);return;}
  if(event.target?.id==='ledgerDimension'&&['model','account','client','route','date'].includes(event.target.value)){LOCAL_LEDGER.dimension=event.target.value;drawLocalLedger();}
});
