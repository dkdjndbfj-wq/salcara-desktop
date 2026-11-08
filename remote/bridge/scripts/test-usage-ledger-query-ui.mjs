import test from 'node:test';
import assert from 'node:assert/strict';
import {readFile} from 'node:fs/promises';
import vm from 'node:vm';

const sources=await Promise.all(['usage-ledger.js','usage-ledger-query.js'].map((name)=>readFile(new URL('../internal/console/web/'+name,import.meta.url),'utf8')));
function deferred(){let resolve,reject;const promise=new Promise((yes,no)=>{resolve=yes;reject=no;});return {promise,resolve,reject};}
const number=(sum=null,known=0,unknown=1)=>({sum,knownRecords:known,unknownRecords:unknown,complete:unknown===0});
const costs=(amount=null)=>({source:'provider',currencies:amount==null?[]:[{currency:'USD',amount,versions:[1]}],knownRecords:amount==null?0:1,unknownRecords:amount==null?1:0,complete:amount!=null});
function query(model='synthetic-model',amount=null){const summary={records:2,states:{completed:2},httpStatuses:{200:2},inputTokens:number(0,1,1),outputTokens:number(),totalTokens:number(),providerCosts:costs(amount),estimatedCosts:costs()};const group={key:'synthetic-group',summary};return {records:[{id:'synthetic-record',model,time:1,state:'completed',httpStatus:200,durationMs:1,tokens:{inputTokens:0},usageFinal:false,providerCost:amount==null?null:{currency:'USD',amount,source:'provider',version:1}}],total:2,revision:'synthetic-revision',limit:100,offset:0,status:{count:2,pending:0,retention:{maxRecords:1000,days:30},records:[]},summary,byModel:[group],byAccount:[{...group,key:'acct-'+'a'.repeat(24)}],byClient:[{...group,key:'client-'+'b'.repeat(24)}],byRoute:[group],trend:[{time:1,summary}],accountNames:{['acct-'+'a'.repeat(24)]:'Account <img src=x>'},clientNames:{['client-'+'b'.repeat(24)]:'Client <script>x</script>'}};}
function harness(){
 const nodes=new Map(),events=new Map(),requests=[],pending=[],scroller={scrollTop:0};let currentQuery=query(),book={version:1,updatedAt:0,entries:[]},accepted=true;
 const decode=(v)=>String(v).replace(/&quot;/g,'"').replace(/&#39;/g,"'").replace(/&lt;/g,'<').replace(/&gt;/g,'>').replace(/&amp;/g,'&');
 const attrs=(text)=>Object.fromEntries([...text.matchAll(/([\w-]+)="([^"]*)"/g)].map((m)=>[m[1],decode(m[2])]));
 function node(id,initial={}){const n={id,isConnected:true,value:'',disabled:false,textContent:'',dataset:{},style:{},getBoundingClientRect:()=>({height:480}),...initial};let html='';Object.defineProperty(n,'innerHTML',{get:()=>html,set:(value)=>{html=String(value);for(const match of html.matchAll(/<(input|select|div|p|button)\b([^>]*)>/g)){const a=attrs(match[2]);if(a.id){const child=node(a.id,{value:a.value||'',max:a.max,min:a.min,dataset:{priceRow:a['data-price-row'],priceField:a['data-price-field']}});if(match[1]==='select'){const rest=html.slice(match.index+match[0].length),body=rest.split('</select>')[0],options=[...body.matchAll(/<option\b([^>]*)>/g)];const selected=options.find((m)=>/\bselected\b/.test(m[1]))||options[0];child.value=selected?attrs(selected[1]).value||'':'';}nodes.set('#'+a.id,child);}if(a['data-act'])nodes.set('act:'+a['data-act'],node('action',{disabled:/\bdisabled\b/.test(match[2])}));}}});return n;}
 const context=vm.createContext({URLSearchParams,OFFICIAL_PANEL:{epoch:0},ACTIONS:{},usageHead(){},$:(key)=>nodes.get(key)||null,compact:String,esc:(v)=>String(v).replace(/[&<>"']/g,(c)=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c])),confirm:()=>accepted,
  document:{addEventListener:(name,fn)=>{if(!events.has(name))events.set(name,[]);events.get(name).push(fn);},querySelector:(selector)=>selector.includes('usageLedgerReprice')?nodes.get('act:usageLedgerReprice'):null},
  officialPanel(){context.OFFICIAL_PANEL.epoch++;for(const n of nodes.values())n.isConnected=false;nodes.clear();scroller.scrollTop=0;nodes.set('#usageLedgerContent',node('usageLedgerContent',{closest:selector=>selector==='.modal-body'?scroller:null}));nodes.set('#officialError',node('officialError'));},
  busy:async(button,fn)=>{button.disabled=true;try{return await fn();}finally{if(button.isConnected)button.disabled=false;}},
  api:async(path,body)=>{requests.push({path,body});if(pending.length)return pending.shift().promise;if(path.includes('/pricing')){if(body)book={version:body.expectedVersion+1,updatedAt:2,entries:structuredClone(body.entries)};return structuredClone(book);}if(path.endsWith('/reprice'))return {ok:true};if(path.includes('/usage-ledger?'))return structuredClone({...currentQuery.status,accountNames:currentQuery.accountNames,clientNames:currentQuery.clientNames});return structuredClone(currentQuery);},
 });
 for(const source of sources)vm.runInContext(source,context);
 return {nodes,requests,scroller,action:(name,dataset={})=>context.ACTIONS[name]({dataset,isConnected:true,disabled:false}),pause(){const d=deferred();pending.push(d);return d;},setQuery:(q)=>currentQuery=q,setBook:(b)=>book=b,confirm:(v)=>accepted=v,close(){context.OFFICIAL_PANEL.epoch++;for(const n of nodes.values())n.isConnected=false;nodes.clear();},input(id,value){const n=nodes.get('#'+id);assert.ok(n,'missing real rendered input '+id);n.value=String(value);for(const fn of events.get('input')||[])fn({target:n});},change(id,value){const n=nodes.get('#'+id);assert.ok(n);n.value=String(value);for(const fn of events.get('change')||[])fn({target:n});},html:()=>nodes.get('#usageLedgerContent')?.innerHTML||'',priceHTML:()=>nodes.get('#ledgerPriceContent')?.innerHTML||'',error:()=>nodes.get('#officialError')?.textContent||''};
}

test('real UI uses SQLite query and nullable summaries, observed zero remains zero',async()=>{const h=harness();await h.action('usageLedger');assert.match(h.requests[0].path,/usage-ledger\/query\?/);assert.match(h.html(),/已知部分/);assert.doesNotMatch(h.html(),/价表|费用|估值|上游 未知/);h.setQuery(query('zero-model',0));await h.action('usageLedgerRefresh');assert.doesNotMatch(h.html(),/上游 USD 0|费用|估值/);assert.doesNotMatch(h.html(),/实际扣费\s*USD 0/);});
test('offset pagination sends revision and rejected stale page cannot append',async()=>{const h=harness();await h.action('usageLedger');const pending=h.pause(),more=h.action('usageLedgerMore');assert.match(h.requests.at(-1).path,/offset=1/);assert.match(h.requests.at(-1).path,/revision=synthetic-revision/);pending.reject(new Error('本机记录或价表已更新，请重新读取'));await more;assert.match(h.error(),/已更新/);assert.equal((h.html().match(/synthetic-model/g)||[]).length,1);});
test('newer filter read beats older success and failure within the same panel',async()=>{const h=harness();await h.action('usageLedger');const old=h.pause(),read=h.action('usageLedgerRefresh');h.setQuery(query('newest-filter'));await h.action('usageLedgerRefresh');old.resolve(query('retired-filter'));await read;assert.match(h.html(),/newest-filter/);assert.doesNotMatch(h.html(),/retired-filter/);const oldError=h.pause(),second=h.action('usageLedgerRefresh');await h.action('usageLedgerRefresh');oldError.reject(new Error('retired error'));await second;assert.equal(h.error(),'');});
test('closed query errors never recreate or pollute reopened ledger',async()=>{const h=harness(),pending=h.pause(),opening=h.action('usageLedger');h.close();await h.action('usageLedger');const content=h.html();pending.reject(new Error('retired panel'));await opening;assert.equal(h.error(),'');assert.equal(h.html(),content);});
test('invalid filters do not send requests and explicit zero HTTP filter is retained',async()=>{const h=harness();await h.action('usageLedger');h.input('ledgerModel','sk-synthetic-key');const count=h.requests.length;await h.action('usageLedgerFilter');assert.equal(h.requests.length,count);assert.match(h.error(),/无效/);h.input('ledgerModel','gpt-fixture');h.input('ledgerHTTP','0');await h.action('usageLedgerFilter');assert.match(h.requests.at(-1).path,/httpStatus=0/);assert.match(h.requests.at(-1).path,/model=gpt-fixture/);});
test('all grouping controls show current labels safely and distinguish UTC dates',async()=>{const h=harness();await h.action('usageLedger');for(const dimension of ['model','account','client','route','date'])h.change('ledgerDimension',dimension);assert.match(h.html(),/1970-01-01/);h.change('ledgerDimension','account');assert.match(h.html(),/Account &lt;img/);assert.doesNotMatch(h.html(),/Account <img/);h.change('ledgerDimension','client');assert.match(h.html(),/Client &lt;script/);});

test('unreadable database exposes explicit recovery controls without manufacturing empty statistics',async()=>{const h=harness();const current=query();current.status.readError='synthetic database unreadable';h.setQuery(current);const pending=h.pause(),opening=h.action('usageLedger');pending.reject(new Error('synthetic database unavailable'));await opening;assert.match(h.error(),/database unavailable/);assert.match(h.html(),/usageLedgerClear/);assert.match(h.html(),/synthetic database unreadable/);assert.match(h.html(),/已保存筛选结果 未知/);assert.match(h.html(),/数据库读取失败，记录数未知/);assert.doesNotMatch(h.html(),/暂无本机请求记录/);});

const accountA='acct-'+'a'.repeat(24),accountB='acct-'+'b'.repeat(24),accountOld='acct-'+'c'.repeat(24);
function accountQuery(){
 const q=query();q.accountNames={[accountA]:'同名来源',[accountB]:'同名来源'};
 q.accountSources={[accountA]:{kind:'api_key',host:'relay-a.example'},[accountB]:{kind:'official_account'}};
 q.summary.cachedInputTokens=number(0,2,0);q.summary.cacheCreationInputTokens=number();
 q.byAccount=[{key:accountA,summary:{...q.summary,inputTokens:number(345,2,0)}},{key:accountB,summary:{...q.summary,inputTokens:number(678,2,0)}},{key:accountOld,summary:q.summary}];return q;
}
test('per-account cards separate same names and show server aggregates rather than one loaded page',async()=>{
 const h=harness();h.setQuery(accountQuery());await h.action('usageLedger');
 assert.equal((h.html().match(/id="ledgerAccount"/g)||[]).length,1);
 assert.ok(h.html().indexOf('id="ledgerAccount"')<h.html().indexOf('<details'));
 for(const id of [accountA,accountB,accountOld])assert.match(h.html(),new RegExp('data-account-id="'+id+'"'));
 assert.match(h.html(),/relay-a\.example/);assert.match(h.html(),/官方账号/);assert.match(h.html(),/历史 \/ 未命名账号/);
 assert.match(h.html(),/<b>345<\/b>/);assert.match(h.html(),/<b>678<\/b>/);
 assert.match(h.html(),/读缓存<\/span><b>0<\/b>/);assert.match(h.html(),/建缓存<\/span><b>未知<\/b>/);
 assert.match(h.html(),/不是账号额度或账单/);
});
test('account selection uses exact ID, clears manual override and preserves applied model scope',async()=>{
 const h=harness();h.setQuery(accountQuery());await h.action('usageLedger');h.input('ledgerModel','gpt-fixture');await h.action('usageLedgerFilter');
 h.input('ledgerAccountID',accountOld);h.input('ledgerModel','unsaved-model');
 await h.action('usageLedgerAccount',{accountId:accountB});
 const request=new URL('http://localhost'+h.requests.at(-1).path);
 assert.equal(request.searchParams.get('accountId'),accountB);assert.equal(request.searchParams.get('model'),'gpt-fixture');
 assert.equal(request.searchParams.has('offset'),false);assert.equal(h.nodes.get('#ledgerAccountID').value,'');
 assert.equal(h.nodes.get('#ledgerAccount').value,accountB);
 const count=h.requests.length;await h.action('usageLedgerAccount',{accountId:'sk-never-send'});assert.equal(h.requests.length,count);
});
test('failed account switch keeps prior identity and pagination scope; late older reads cannot replace new account',async()=>{
 const h=harness();h.setQuery(accountQuery());await h.action('usageLedger');await h.action('usageLedgerAccount',{accountId:accountA});
 const failed=h.pause(),switching=h.action('usageLedgerAccount',{accountId:accountB});failed.reject(Error('not connected'));await switching;
 assert.equal(h.nodes.get('#ledgerAccount').value,accountA);assert.match(h.error(),/not connected/);
 await h.action('usageLedgerMore');assert.equal(new URL('http://localhost'+h.requests.at(-1).path).searchParams.get('accountId'),accountA);
 const old=h.pause(),stale=h.action('usageLedgerAccount',{accountId:accountB});await h.action('usageLedgerAccount',{accountId:accountA});
 old.resolve(query('retired-model'));await stale;assert.equal(h.nodes.get('#ledgerAccount').value,accountA);assert.doesNotMatch(h.html(),/retired-model/);
});

test('a pending account selection removes previous totals and old pagination cannot override it',async()=>{
 const h=harness();h.setQuery(accountQuery());await h.action('usageLedger');await h.action('usageLedgerAccount',{accountId:accountA});
 const pending=h.pause(),switching=h.action('usageLedgerAccount',{accountId:accountB});
 assert.equal(h.nodes.get('#ledgerAccount').value,accountB);assert.match(h.html(),/正在读取/);
 assert.equal(h.nodes.get('#usageLedgerContent').style.minHeight,'480px');
 assert.doesNotMatch(h.html(),/synthetic-model|stat-value|已保存筛选结果|usageLedgerMore/);
 const before=h.requests.length;await h.action('usageLedgerMore');assert.equal(h.requests.length,before);
 pending.resolve(query('selected-b'));await switching;assert.match(h.html(),/selected-b/);assert.equal(h.nodes.get('#ledgerAccount').value,accountB);
 assert.equal(h.nodes.get('#usageLedgerContent').style.minHeight,'');
});

test('another exact account can replace a pending selection without flashing its predecessor',async()=>{
 const h=harness();h.setQuery(accountQuery());await h.action('usageLedger');await h.action('usageLedgerAccount',{accountId:accountA});
 const old=h.pause(),first=h.action('usageLedgerAccount',{accountId:accountB});
 const newer=h.pause(),second=h.action('usageLedgerAccount',{accountId:accountOld});
 assert.equal(h.nodes.get('#ledgerAccount').value,accountOld);assert.match(h.html(),/正在读取/);
 old.resolve(query('retired-b'));await first;assert.doesNotMatch(h.html(),/retired-b|stat-value/);
 newer.resolve(query('selected-history'));await second;assert.equal(h.nodes.get('#ledgerAccount').value,accountOld);assert.match(h.html(),/selected-history/);
});

test('switching from scrolled records brings the loading account selector into view without collapsing the dialog',async()=>{
 const h=harness();h.setQuery(accountQuery());await h.action('usageLedger');
 h.scroller.scrollTop=360;
 const pending=h.pause(),switching=h.action('usageLedgerAccount',{accountId:accountB});
 assert.equal(h.scroller.scrollTop,0,'the actual modal scroll container must expose the new loading controls');
 assert.equal(h.nodes.get('#usageLedgerContent').style.minHeight,'480px');
 assert.equal(h.nodes.get('#ledgerAccount').value,accountB);assert.match(h.html(),/role="status">正在读取/);
 pending.resolve(query('selected-b'));await switching;
 assert.equal(h.scroller.scrollTop,0);assert.equal(h.nodes.get('#usageLedgerContent').style.minHeight,'');
 h.scroller.scrollTop=240;await h.action('usageLedgerRefresh');assert.equal(h.scroller.scrollTop,240,'ordinary same-scope refresh preserves reading position');
 await h.action('usageLedgerMore');assert.equal(h.scroller.scrollTop,240,'pagination preserves reading position');
});
test('historical account choices survive scoped reads without retaining deleted current labels',async()=>{
 const h=harness();h.setQuery(accountQuery());await h.action('usageLedger');const scoped=query();scoped.accountNames={};scoped.byAccount=[{key:accountA,summary:scoped.summary}];h.setQuery(scoped);
 await h.action('usageLedgerAccount',{accountId:accountA});assert.match(h.html(),new RegExp('value="'+accountOld+'"'));assert.doesNotMatch(h.html(),/同名来源|relay-a\.example/);
 h.change('ledgerAccount',accountOld);for(let i=0;i<8;i++)await Promise.resolve();
 assert.equal(new URL('http://localhost'+h.requests.at(-1).path).searchParams.get('accountId'),accountOld);
 assert.equal(h.nodes.get('#ledgerAccount').value,accountOld);
});

test('single-account view keeps selection and detailed totals without duplicate account cards',async()=>{
 const h=harness();h.setQuery(accountQuery());await h.action('usageLedger');
 assert.match(h.html(),/class="card ledger-account-card"/);
 await h.action('usageLedgerAccount',{accountId:accountA});
 assert.equal(h.nodes.get('#ledgerAccount').value,accountA);
 assert.match(h.html(),/统计范围：同名来源 · relay-a\.example/);
 assert.doesNotMatch(h.html(),/class="card ledger-account-card"|class="ledger-account-grid"|usageLedgerAccountsMore/);
 for(const label of ['输入 token','输出 token','读缓存 token','建缓存 token','推理 token','总 token'])assert.match(h.html(),new RegExp('stat-label">'+label));
 assert.doesNotMatch(h.html(),/SQLite/);
 assert.match(h.html(),/记录本机请求和 Token 用量，不保存提示词、正文或密钥/);
 await h.action('usageLedgerAccount',{accountId:''});
 assert.match(h.html(),/class="card ledger-account-card"/);
 assert.equal(h.nodes.get('#ledgerAccount').value,'');
});

test('Token record screen has no pricing/reprice entry and viewing retains stored monetary metadata',async()=>{const h=harness();const saved=query('history-preserved',.25);h.setQuery(saved);await h.action('usageLedger');assert.match(h.html(),/history-preserved|输入|输出|缓存/);assert.doesNotMatch(h.html(),/usageLedgerPricing|usageLedgerReprice|费用 \/ 估值|USD/);assert.equal(saved.records[0].providerCost.amount,.25);assert.ok(h.requests.every(r=>!r.body&&!r.path.includes('pricing')&&!r.path.includes('reprice')));const html=await readFile(new URL('../internal/console/web/index.html',import.meta.url),'utf8');assert.doesNotMatch(html,/<script src="usage-ledger-pricing.js"/);});
