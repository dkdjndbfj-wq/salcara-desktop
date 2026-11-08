'use strict';
const LOCAL_PRICE={epoch:0,book:null,entries:[],revision:0,request:0,dirty:false,open:false,note:''};
function resetLedgerPricing(epoch){Object.assign(LOCAL_PRICE,{epoch,book:null,entries:[],revision:0,request:0,dirty:false,open:false,note:''});}
function ledgerPriceCurrent(epoch,request){return ledgerIsCurrent(epoch)&&LOCAL_PRICE.epoch===epoch&&(request==null||LOCAL_PRICE.request===request);}
function ledgerPriceDraft(p={}){const str=(value)=>value==null?'':String(value),r=p.rates||{},l=p.longRates||{};return {model:p.model||'',tier:p.tier||'',basis:p.inputTokenBasis||'includes_cache',creationMode:p.cacheCreationMode||'included_input',input:str(r.inputUsdPerMillion),cached:str(r.cachedInputUsdPerMillion),creation:str(r.cacheCreationUsdPerMillion),output:str(r.outputUsdPerMillion),threshold:str(p.longContextThreshold),longInput:str(l.inputUsdPerMillion),longCached:str(l.cachedInputUsdPerMillion),longCreation:str(l.cacheCreationUsdPerMillion),longOutput:str(l.outputUsdPerMillion)};}
function ledgerPriceInput(index,key,label,value,type='number'){return `<div class="field"><label for="ledgerPrice-${index}-${key}">${label}</label><input class="input" id="ledgerPrice-${index}-${key}" data-price-row="${index}" data-price-field="${key}" type="${type}" value="${esc(value)}" ${type==='number'?key==='threshold'?'min="1" max="1000000000000" step="1"':'min="0" max="1000000" step="any"':'maxlength="200"'}></div>`;}
function ledgerPriceSelect(index,key,label,values,value){return `<div class="field"><label for="ledgerPrice-${index}-${key}">${label}</label><select class="input" id="ledgerPrice-${index}-${key}" data-price-row="${index}" data-price-field="${key}">${ledgerOptions(values,value)}</select></div>`;}
function ledgerPriceBody(){
  if(!LOCAL_PRICE.open)return '<button class="btn sm" data-act="usageLedgerPricing">编辑模型价表</button>';
  if(!LOCAL_PRICE.book)return '<p class="muted small">正在读取本机价表…</p><button class="btn sm" data-act="usageLedgerPricingReload">重试读取</button><button class="btn sm" data-act="usageLedgerPricingClose">关闭价表</button>';
  return `<p class="small muted">价表 v${LOCAL_PRICE.book.version}。内置官方参考价自动生效；这里填写自定义覆盖价（USD / 百万 token）。覆盖行留空表示未知。保存不改写历史，上游费用不参与重算。</p>${LOCAL_PRICE.book.reference ? `<p class="small muted">官方参考价 · ${esc(LOCAL_PRICE.book.reference.verifiedAt || '')} · ${Array.isArray(LOCAL_PRICE.book.reference.entries) ? LOCAL_PRICE.book.reference.entries.length : 0} 条</p>` : ''}<div class="stack">${LOCAL_PRICE.entries.map((p,i)=>`<div class="card stack"><div class="row"><b>模型价目 ${i+1}</b><span class="spacer"></span><button class="btn sm ghost danger-text" data-act="usageLedgerPriceRemove" data-index="${i}">删除此行</button></div><div class="form-grid">${ledgerPriceInput(i,'model','实际上游模型 ID',p.model,'text')}${ledgerPriceSelect(i,'tier','适用上游层级',[['','全部层级（明确通用）'],['standard','standard'],['default','default'],['priority','priority'],['fast','fast'],['ultrafast','ultrafast'],['flex','flex'],['auto','auto']],p.tier)}${ledgerPriceSelect(i,'basis','输入 token 口径',[['includes_cache','输入总数包含缓存'],['excludes_cache','输入总数不包含缓存']],p.basis)}${ledgerPriceSelect(i,'creationMode','创建缓存计价',[['included_input','按普通输入计价（明确不单列）'],['separate','独立计价（缺计数保持未知）']],p.creationMode)}${ledgerPriceInput(i,'input','普通输入 USD / 百万',p.input)}${ledgerPriceInput(i,'cached','读取缓存 USD / 百万',p.cached)}${ledgerPriceInput(i,'creation','创建缓存 USD / 百万',p.creation)}${ledgerPriceInput(i,'output','输出 USD / 百万',p.output)}</div><details class="adv"><summary>长上下文费率（可选）</summary><p class="muted small">仅当输入 token 超过阈值时使用以下费率；未配置的费率仍为未知。留空阈值表示关闭此档。</p><div class="form-grid">${ledgerPriceInput(i,'threshold','输入阈值（1–10¹²）',p.threshold)}${ledgerPriceInput(i,'longInput','长上下文普通输入',p.longInput)}${ledgerPriceInput(i,'longCached','长上下文读取缓存',p.longCached)}${ledgerPriceInput(i,'longCreation','长上下文创建缓存',p.longCreation)}${ledgerPriceInput(i,'longOutput','长上下文输出',p.longOutput)}</div></details></div>`).join('')||'<p class="muted small">未设置自定义覆盖，使用内置官方参考价。</p>'}</div><div class="row"><button class="btn sm" data-act="usageLedgerPriceAdd">添加模型价目</button><button class="btn sm primary" data-act="usageLedgerPriceSave">保存价表</button><button class="btn sm" data-act="usageLedgerPricingReload">重新读取</button><button class="btn sm" data-act="usageLedgerReprice" ${LOCAL_PRICE.dirty?'disabled':''}>重算历史估值</button><button class="btn sm" data-act="usageLedgerPricingClose">关闭价表</button></div><p class="small muted" id="ledgerPriceNote" aria-live="polite">${esc(LOCAL_PRICE.note|| (LOCAL_PRICE.dirty?'当前改稿尚未保存':''))}</p>`;
}
function ledgerPricingShell(){return `<div class="card stack"><h3 class="card-title">本机模型价表</h3><div id="ledgerPriceContent" class="stack">${ledgerPriceBody()}</div></div>`;}
function drawLedgerPricing(){if(ledgerPriceCurrent(LOCAL_PRICE.epoch)&&$('#ledgerPriceContent'))$('#ledgerPriceContent').innerHTML=ledgerPriceBody();}
async function openLedgerPricing(button,reload=false){
  const epoch=LOCAL_PRICE.epoch;if(!ledgerPriceCurrent(epoch))return;if(reload&&LOCAL_PRICE.dirty&&!await confirm('重新读取会替换未保存的价表改稿，继续？'))return;
  LOCAL_PRICE.open=true;if(LOCAL_PRICE.book&&!reload){drawLedgerPricing();return;}
  const request=++LOCAL_PRICE.request,revision=LOCAL_PRICE.revision;drawLedgerPricing();
  try{const book=await busy(button,()=>api('/api/local/usage-ledger/pricing'));if(!ledgerPriceCurrent(epoch,request))return;LOCAL_PRICE.book=book;
    if(LOCAL_PRICE.revision===revision){LOCAL_PRICE.entries=book.entries.map(ledgerPriceDraft);LOCAL_PRICE.dirty=false;LOCAL_PRICE.note='';}else{LOCAL_PRICE.note='已更新价表版本；读取期间的改稿仍未保存。';}drawLedgerPricing();$('#officialError').textContent='';
  }catch(error){if(ledgerPriceCurrent(epoch,request))$('#officialError').textContent=error.message;}
}
function ledgerParsePrices(){
  const seen=new Set();if(LOCAL_PRICE.entries.length>512)throw new Error('最多 512 条价目');
  const parse=(v)=>{if(v==='')return null;if(!/^(?:\d+(?:\.\d*)?|\.\d+)(?:[eE][+-]?\d+)?$/.test(v)||!Number.isFinite(Number(v))||Number(v)<0||Number(v)>1e6)throw new Error('费率为 0–1000000，留空表示未知');return Number(v);};
  return LOCAL_PRICE.entries.map((p)=>{if(!/^[A-Za-z0-9][A-Za-z0-9._:/-]{0,199}$/.test(p.model)||p.model.includes('://')||/^(sk-|bearer)/i.test(p.model)||!['','standard','default','priority','fast','ultrafast','flex','auto'].includes(p.tier)||!['includes_cache','excludes_cache'].includes(p.basis))throw new Error('模型 ID、层级或输入口径无效');const key=p.model+'\0'+p.tier;if(seen.has(key))throw new Error('同一模型和层级只能有一条价目');seen.add(key);
    const rates=(prefix)=>({inputUsdPerMillion:parse(p[prefix+'Input']??p.input),cachedInputUsdPerMillion:parse(p[prefix+'Cached']??p.cached),cacheCreationUsdPerMillion:parse(p[prefix+'Creation']??p.creation),outputUsdPerMillion:parse(p[prefix+'Output']??p.output)});
    const base={inputUsdPerMillion:parse(p.input),cachedInputUsdPerMillion:parse(p.cached),cacheCreationUsdPerMillion:parse(p.creation),outputUsdPerMillion:parse(p.output)};
    let threshold=null,longRates=null;if(p.threshold!==''){if(!/^\d+$/.test(p.threshold)||!Number.isSafeInteger(Number(p.threshold))||Number(p.threshold)<1||Number(p.threshold)>1e12)throw new Error('长上下文阈值为 1–10¹² 的整数');threshold=Number(p.threshold);longRates=rates('long');}else if([p.longInput,p.longCached,p.longCreation,p.longOutput].some((v)=>v!==''))throw new Error('填写长上下文费率时须填写阈值');
    if(!['included_input','separate'].includes(p.creationMode))throw new Error('请选择创建缓存计价口径');
    return {model:p.model,tier:p.tier,inputTokenBasis:p.basis,cacheCreationMode:p.creationMode,rates:base,longContextThreshold:threshold,longRates};
  });
}
async function saveLedgerPricing(button){
  const epoch=LOCAL_PRICE.epoch;if(!ledgerPriceCurrent(epoch)||!LOCAL_PRICE.book)return;let entries;try{entries=ledgerParsePrices();}catch(error){$('#officialError').textContent=error.message;return;}
  const request=++LOCAL_PRICE.request,revision=LOCAL_PRICE.revision,expectedVersion=LOCAL_PRICE.book.version;
  try{const book=await busy(button,()=>api('/api/local/usage-ledger/pricing',{expectedVersion,entries}));if(!ledgerPriceCurrent(epoch,request))return;LOCAL_PRICE.book=book;
    if(LOCAL_PRICE.revision===revision){LOCAL_PRICE.entries=book.entries.map(ledgerPriceDraft);LOCAL_PRICE.dirty=false;LOCAL_PRICE.note=`已保存价表 v${book.version}，历史估值尚未重算。`;}else{LOCAL_PRICE.dirty=true;LOCAL_PRICE.note='提交时价表已保存；当前新改稿尚未保存。';}drawLedgerPricing();$('#officialError').textContent='';
  }catch(error){if(ledgerPriceCurrent(epoch,request))$('#officialError').textContent=error.message;}
}
async function repriceLedger(button){
  const epoch=LOCAL_PRICE.epoch;if(!ledgerPriceCurrent(epoch)||!LOCAL_PRICE.book)return;if(LOCAL_PRICE.dirty){$('#officialError').textContent='请先保存价表改稿';return;}
  const expectedVersion=LOCAL_PRICE.book.version;if(!await confirm(`用价表 v${expectedVersion} 重算保留范围内的历史估值？上游返回费用和未知用量保持不变。`))return;const request=++LOCAL_PRICE.request;
  try{await busy(button,()=>api('/api/local/usage-ledger/reprice',{expectedVersion,confirmed:true}));if(!ledgerPriceCurrent(epoch,request))return;LOCAL_PRICE.note=`已按价表 v${expectedVersion} 重算历史估值。`;await loadLocalLedger();if(ledgerPriceCurrent(epoch,request))$('#officialError').textContent='';}
  catch(error){if(ledgerPriceCurrent(epoch,request))$('#officialError').textContent=error.message;}
}
Object.assign(ACTIONS,{
  usageLedgerPricing:(button)=>openLedgerPricing(button),usageLedgerPricingReload:(button)=>openLedgerPricing(button,true),
  usageLedgerPricingClose:()=>{LOCAL_PRICE.open=false;LOCAL_PRICE.request++;drawLedgerPricing();},
  usageLedgerPriceAdd:()=>{if(!LOCAL_PRICE.book||LOCAL_PRICE.entries.length>=512)return;LOCAL_PRICE.entries.push(ledgerPriceDraft());LOCAL_PRICE.revision++;LOCAL_PRICE.dirty=true;LOCAL_PRICE.note='';drawLedgerPricing();},
  usageLedgerPriceRemove:(button)=>{const index=Number(button.dataset.index);if(!Number.isInteger(index)||index<0||index>=LOCAL_PRICE.entries.length)return;LOCAL_PRICE.entries.splice(index,1);LOCAL_PRICE.revision++;LOCAL_PRICE.dirty=true;LOCAL_PRICE.note='';drawLedgerPricing();},
  usageLedgerPriceSave:saveLedgerPricing,usageLedgerReprice:repriceLedger,
});
if(typeof document!=='undefined'){
  const changed=(event)=>{const target=event.target,index=Number(target?.dataset?.priceRow),field=target?.dataset?.priceField;if(!target||target.dataset.priceRow==null||!Number.isInteger(index)||!LOCAL_PRICE.entries[index]||!Object.hasOwn(LOCAL_PRICE.entries[index],field)||!ledgerPriceCurrent(LOCAL_PRICE.epoch))return;LOCAL_PRICE.entries[index][field]=target.value;LOCAL_PRICE.revision++;LOCAL_PRICE.dirty=true;LOCAL_PRICE.note='';if($('#ledgerPriceNote'))$('#ledgerPriceNote').textContent='当前改稿尚未保存';const reprice=document.querySelector?.('[data-act="usageLedgerReprice"]');if(reprice)reprice.disabled=true;};
  document.addEventListener('input',changed);document.addEventListener('change',changed);
}
