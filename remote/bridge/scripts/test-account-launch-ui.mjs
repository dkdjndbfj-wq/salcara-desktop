import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import crypto from 'node:crypto';
const progressSource = fs.readFileSync(new URL('../internal/console/web/launch-progress.js', import.meta.url), 'utf8');

// Actual shipped handlers, synthetic DOM and HTTP only. Never starts a native
// app, reads credentials, opens a provider or creates an isolated tool profile.
const workbench = fs.readFileSync(new URL('../internal/console/web/workbench.js', import.meta.url), 'utf8');
const claude = fs.readFileSync(new URL('../internal/console/web/claude-desktop.js', import.meta.url), 'utf8');
const clone = value => value === undefined ? undefined : JSON.parse(JSON.stringify(value));
const gate = () => { let resolve, reject; const promise = new Promise((a,b)=>{resolve=a;reject=b;}); return {promise,resolve,reject}; };
const flush = async () => { for(let i=0;i<12;i++)await Promise.resolve(); };
function fixture() {
  const nodes=new Map(),listeners=new Map(),calls=[],notices=[],cards=[];
  const root={children:[],_html:'',querySelector(selector){
    if(selector.startsWith('#'))return nodes.get(selector.slice(1))||null;
    const action=selector.match(/^\[data-act="([^"]+)"\]$/)?.[1];
    return this.children.find(node=>node.dataset.act===action)||null;
  },get innerHTML(){return this._html;},set innerHTML(html){
    for(const node of this.children)node.isConnected=false;nodes.clear();this.children=[];this._html=String(html);
    for(const match of this._html.matchAll(/<([a-z][\w-]*)\b([^>]*)>/gi)){
      const attrs=match[2],get=name=>attrs.match(new RegExp('(?:^|\\s)'+name+'="([^"]*)"'))?.[1];
      const dataset={};for(const entry of attrs.matchAll(/\bdata-([\w-]+)="([^"]*)"/g))dataset[entry[1].replace(/-([a-z])/g,(_,c)=>c.toUpperCase())]=entry[2];
      const node={id:get('id'),dataset,checked:/(?:^|\s)checked(?:\s|$)/.test(attrs),disabled:/(?:^|\s)disabled(?:\s|$)/.test(attrs),isConnected:true,innerHTML:'',value:get('value')||'',hasAttribute:name=>new RegExp('(?:^|\\s)'+name+'(?:[=\\s]|$)').test(attrs)};
      this.children.push(node);if(node.id)nodes.set(node.id,node);
    }
  }};
  const targets=['codex-desktop','codex','claude','claude-desktop','deepseek-harness','hermes'];
  const account=id=>({id,name:'演示 '+id,sourceRevision:'src-v1-'+id.repeat(64),kind:'api',baseUrl:'https://example.test/v1',wire:'auto',models:['gpt-fixture','claude-fixture'],model:'',keyMasked:'fixture-mask'});
  const backend={accounts:[account('a'),account('b')],tools:targets.map(id=>({id,name:id,kind:id==='deepseek-harness'?id:id.split('-')[0],available:true})),bindings:Object.fromEntries(targets.map(id=>[id,{accountId:'a',model:'gpt-fixture',override:false}]))};
  const S={local:clone(backend),route:'accounts'}, ACTIONS={};
  const document={addEventListener:(type,fn)=>{if(!listeners.has(type))listeners.set(type,[]);listeners.get(type).push(fn);},getElementById:id=>nodes.get(id)};
  let intercept=null;
  const context=vm.createContext({S,ACTIONS,document,URL,console,AbortController,crypto:crypto.webcrypto,PAGES:{overview:{},accounts:{}},ICONS:{},TOOL_LABEL:{},
    $:sel=>sel==='#modalRoot'?root:nodes.get(sel.slice(1)),esc:value=>String(value??'').replaceAll('&','&amp;').replaceAll('<','&lt;').replaceAll('"','&quot;'),icon:()=>'',ico:()=>'',
    remoteLampHTML:()=>'',desktopRemoteLamp:()=>({}),renderApprovalsBox(){},localEditor(){},clearTimeout(){},setTimeout(){throw Error('unexpected timer');},
    renderLocalCards:()=>cards.push('api'),toast:(...args)=>notices.push(args),busy:async(_button,action)=>action(),
    api:async(url,body,method,options)=>{calls.push({url,body:clone(body)});if(intercept){const value=await intercept(url,body,options);if(value!==undefined)return value;}
      if(url==='/api/local/accounts')return clone(backend);
      if(url==='/api/local/models')return {models:backend.accounts.find(a=>a.id===body.id)?.models||[]};
      if(url==='/api/local/claude-desktop/status')return {supported:true,managed:false};
      if(url==='/api/local/agent-change/v1'){Object.assign(backend.bindings[body.target],{accountId:body.id,model:body.model,override:body.override});return {ok:true,protocol:body.protocol,operationId:body.operationId};}
      if(url==='/api/local/bind'){Object.assign(backend.bindings[body.target],{accountId:body.id,model:body.model,override:body.override});return {ok:true};}
      if(['/api/local/switch','/api/local/claude-desktop/switch'].includes(url))return {ok:true};
      throw Error('unexpected route '+url);
    },
  });
  vm.runInContext(progressSource,context);
  context.launchAPI=(url,body,progress,options)=>context.api(url,body,undefined,options);
  vm.runInContext(workbench,context);vm.runInContext(claude,context);
  const api=vm.runInContext('({wbOpenAccount,wbAccountModels,WB})',context);
  const button=act=>root.children.find(node=>node.dataset.act===act);
  const ready=()=>assert.equal(nodes.get('wbReady')||nodes.get('cdReady'),undefined);
  const change=async node=>{for(const listener of listeners.get('change')||[])await listener({target:node});};
  return {context,S,ACTIONS,backend,calls,notices,cards,root,nodes,button,ready,change,...api,
    open:(target='codex',id='b')=>api.wbOpenAccount(target,id,{isConnected:true,dataset:{target,id}}),
    go:()=>{const buttonNode=button('cdGo')||button('wbOpenGo');assert.ok(buttonNode);return ACTIONS[buttonNode.dataset.act](buttonNode);},
    setIntercept:fn=>intercept=fn,writes:()=>calls.filter(call=>call.body&&call.url!=='/api/local/models'),
  };
}

const safePath = '/api/local/agent-change/v1';
const receipt = body => ({ok:true,protocol:body.protocol,operationId:body.operationId});
test('all six Agent entries stage selection without checkbox, history reads or writes',async()=>{
  for(const target of ['codex-desktop','codex','claude','claude-desktop','deepseek-harness','hermes']){
    const f=fixture();await f.open(target);f.ready();assert.match(f.root.innerHTML,/会先停止该 Agent 的当前任务，再应用所选 API。/);
    assert.equal(f.writes().length,0);assert.equal(f.backend.bindings[target].accountId,'a');
    assert.equal(f.button('wbApplyGo').disabled,false);assert.equal((f.button('cdGo')||f.button('wbOpenGo')).disabled,false);
    assert.equal(f.calls.some(c=>/session|history/.test(c.url)),false);
  }
});
test('one bottom click submits one versioned transaction with source CAS for every target',async()=>{
  for(const target of ['codex-desktop','codex','claude','claude-desktop','deepseek-harness','hermes'])for(const applyOnly of [false,true]){
    const f=fixture();await f.open(target);f.WB.accountOpen.override=true;
    await (applyOnly?f.ACTIONS.wbApplyGo(f.button('wbApplyGo')):f.go());
    const writes=f.writes();assert.equal(writes.length,1);assert.equal(writes[0].url,safePath);
    assert.equal(writes[0].body.protocol,'safe-agent-change.v1');assert.match(writes[0].body.operationId,/^[a-f0-9-]{36}$/);
    assert.equal(writes[0].body.target,target);assert.equal(writes[0].body.id,'b');assert.equal(writes[0].body.expectedSourceRevision,'src-v1-'+'b'.repeat(64));
    assert.equal(writes[0].body.applyOnly,applyOnly);assert.equal(writes[0].body.override,true);
    assert.equal('taskReady' in writes[0].body,false);assert.equal('confirmed' in writes[0].body,false);assert.equal(f.root.innerHTML,'');
  }
});
test('both buttons disable throughout stopping and stage updates stay inside original panel',async()=>{
  const f=fixture(),wait=gate();await f.open();const title=f.nodes.get('wbOpenTitle');
  f.context.launchAPI=async(path,body,progress)=>{progress.event({type:'phase',step:'stop'});await wait.promise;progress.event({type:'phase',step:'profile'});return receipt(body);};
  const pending=f.go();await flush();assert.equal(f.button('wbApplyGo').disabled,true);assert.equal(f.button('wbOpenGo').disabled,true);
  assert.equal(f.nodes.get('wbOpenTitle'),title);assert.match(f.nodes.get('switchError').innerHTML,/正在停止/);
  assert.equal(f.context.visibleLaunchProgress,undefined);await f.go();wait.resolve();await pending;
});
test('duplicate clicks and another card cannot interleave with a safe transaction',async()=>{
  const f=fixture(),wait=gate();await f.open();f.setIntercept((url,body)=>url===safePath?wait.promise.then(()=>receipt(body)):undefined);
  const pending=f.go();await flush();await f.go();await f.open('claude','a');assert.equal(f.WB.accountOpen.id,'b');assert.equal(f.writes().length,1);
  wait.resolve();await pending;assert.equal(f.writes().length,1);
});
test('close during stop aborts the request and never applies or revives the panel',async()=>{
  const f=fixture(),wait=gate();await f.open();let signal,applied=false;
  f.setIntercept((url,body,options)=>url===safePath?wait.promise.then(()=>{signal=options.signal;if(signal.aborted)throw Object.assign(Error('cancelled'),{cancelled:true});applied=true;return receipt(body);}):undefined);
  const pending=f.go();await flush();f.WB.cancelPending();f.root.innerHTML='';wait.resolve();await pending;
  assert.equal(signal.aborted,true);assert.equal(applied,false);assert.equal(f.backend.bindings.codex.accountId,'a');assert.equal(f.root.innerHTML,'');
});
test('closing before freshness resolves never submits the transaction',async()=>{
  const f=fixture();await f.open();const wait=gate();f.setIntercept(url=>url==='/api/local/accounts'?wait.promise:undefined);
  const pending=f.go();f.WB.cancelPending();f.root.innerHTML='';wait.resolve(clone(f.backend));await pending;assert.equal(f.writes().length,0);
});
test('old backend and stop failures fail closed with preserved selection and no fallback',async()=>{
  for(const code of ['not_found','safe_agent_change_stop_timeout','safe_agent_change_stop_unknown']){
    const f=fixture();await f.open();f.WB.accountOpen.override=true;
    f.setIntercept(url=>{if(url===safePath)throw Object.assign(Error(code),{status:code==='not_found'?404:409,code});});
    await f.go();assert.equal(f.writes().length,1);assert.equal(f.backend.bindings.codex.accountId,'a');assert.equal(f.WB.accountOpen.id,'b');assert.equal(f.WB.accountOpen.override,true);
    assert.match(f.root.innerHTML,new RegExp(code));assert.match(f.root.innerHTML,/wbRecheck/);assert.equal(f.button('wbApplyGo').disabled,true);
  }
});
test('missing or mismatched receipts remain uncertain and cannot replay via recheck',async()=>{
  for(const response of [{},{ok:false},{ok:true},{ok:true,protocol:'older',operationId:'wrong'}]){
    const f=fixture();await f.open();f.setIntercept(url=>url===safePath?response:undefined);await f.go();
    assert.equal(f.WB.accountOpen.executionUnknown,true);assert.doesNotMatch(f.root.innerHTML,/data-act="wbRecheck"/);
    await f.ACTIONS.wbRecheck();await f.go();assert.equal(f.writes().length,1);
  }
});
test('changed source revision, URL and identity prevent writes before stopping',async()=>{
  for(const change of [{sourceRevision:'changed'},{baseUrl:'https://other.test'},{officialAccount:{id:'new',mode:'codex',identityEpoch:2}}]){
    const f=fixture();await f.open();Object.assign(f.backend.accounts[1],change);await f.go();assert.equal(f.writes().length,0);assert.match(f.root.innerHTML,/账号或工具已变化/);
  }
});
test('unsupported, vault and web-only sources cannot be launched from a forged entry',async()=>{
  for(const flag of [{libraryOnly:true},{mode:'chatgpt_web_session'},{officialAccount:{mode:'chatgpt_web_session'}}]){
    const f=fixture();Object.assign(f.S.local.accounts[1],flag);await assert.rejects(f.open(),/不能用于/);assert.equal(f.writes().length,0);
  }
  const f=fixture();f.S.local.accounts[1].officialAccount={id:'official',mode:'codex'};await assert.rejects(f.open('hermes'),/不能用于/);
});
test('missing revision or unavailable fresh tool keeps both buttons disabled',async()=>{
  for(const change of ['revision','tool']){
    const f=fixture();if(change==='revision')delete f.backend.accounts[1].sourceRevision;else f.backend.tools.find(t=>t.id==='codex').available=false;
    await f.open();await f.go();assert.equal(f.writes().length,0);assert.equal(f.button('wbApplyGo').disabled,true);
  }
});
test('late source reads and late completion cannot replace new route or dialog owners',async()=>{
  const f=fixture(),wait=gate();f.setIntercept(url=>url==='/api/local/accounts'?wait.promise:undefined);const opening=f.open();f.root.innerHTML='<div id="newTitle">keep</div>';wait.resolve(clone(f.backend));await opening;assert.match(f.root.innerHTML,/keep/);
  for(const failed of [false,true]){
    const g=fixture(),pending=gate();await g.open();g.setIntercept((url,body)=>url===safePath?pending.promise.then(()=>receipt(body)):undefined);
    const launch=g.go();await flush();g.root.innerHTML='<div id="newTitle">keep</div>';if(failed)pending.reject(Error('late failure'));else pending.resolve();await launch;assert.match(g.root.innerHTML,/keep/);
  }
});
test('model reload and override toggle remain read-only until a bottom action',async()=>{
  const f=fixture();await f.open();const toggle=f.root.children.find(n=>n.dataset.wbOverride);toggle.checked=true;await f.change(toggle);
  await f.wbAccountModels(f.WB.accountOpen,false);assert.equal(f.WB.accountOpen.override,true);assert.equal(f.backend.bindings.codex.override,false);assert.equal(f.writes().length,0);
});

test('same markup in a replacement modal is a new owner and rejects late completion',async()=>{
  const f=fixture(),wait=gate();await f.open();const oldTitle=f.nodes.get('wbOpenTitle');
  f.setIntercept((url,body)=>url===safePath?wait.promise.then(()=>receipt(body)):undefined);
  const pending=f.go();await flush();const markup=f.root.innerHTML;f.root.innerHTML=markup;
  assert.notEqual(f.nodes.get('wbOpenTitle'),oldTitle);wait.resolve();await pending;
  assert.equal(f.root.innerHTML,markup);assert.equal(f.notices.some(([message])=>message==='已打开'),false);
});

test('A to B to A rejects an old model result even when account and route match again',async()=>{
  const f=fixture(),wait=gate();await f.open('codex','a');const old=f.WB.accountOpen;
  f.setIntercept(url=>url==='/api/local/models'?wait.promise:undefined);
  const loading=f.wbAccountModels(old,false);await flush();f.root.innerHTML='';
  await f.open('codex','b');f.root.innerHTML='';await f.open('codex','a');
  const owner=f.WB.accountOpen,node=f.nodes.get('wbOpenTitle'),markup=f.root.innerHTML;
  wait.reject(Error('obsolete-model-error'));await loading;
  assert.notEqual(owner,old);assert.equal(f.WB.accountOpen,owner);assert.equal(f.nodes.get('wbOpenTitle'),node);assert.equal(f.root.innerHTML,markup);
  assert.doesNotMatch(f.root.innerHTML,/obsolete-model-error/);assert.equal(f.writes().length,0);
});

test('A to B to A rejects a delayed Claude capability response from an earlier owner',async()=>{
  const f=fixture(),wait=gate();let statusReads=0;
  f.setIntercept(url=>url==='/api/local/claude-desktop/status'&&++statusReads===1?wait.promise:undefined);
  const opening=f.open('claude-desktop','a');await flush();const old=f.WB.accountOpen;f.root.innerHTML='';
  await f.open('claude-desktop','b');f.root.innerHTML='';await f.open('claude-desktop','a');
  const owner=f.WB.accountOpen,node=f.nodes.get('cdTitle');wait.resolve({supported:true,managed:true,message:'obsolete-policy'});await opening;
  assert.notEqual(owner,old);assert.equal(f.WB.accountOpen,owner);assert.equal(f.nodes.get('cdTitle'),node);assert.equal(owner.status.managed,false);
  assert.doesNotMatch(f.root.innerHTML,/obsolete-policy/);assert.equal(f.writes().length,0);
});

test('managed or unsupported Claude and forged action identities cannot submit',async()=>{
  for(const status of [{supported:false},{supported:true,managed:true}]){
    const f=fixture();f.setIntercept(url=>url==='/api/local/claude-desktop/status'?status:undefined);await f.open('claude-desktop');await f.go();await f.ACTIONS.wbApplyGo(f.button('wbApplyGo'));assert.equal(f.writes().length,0);
  }
  const f=fixture();await f.open();
  for(const dataset of [{id:'a',target:'codex'},{id:'b',target:'hermes'}])await f.ACTIONS.wbOpenGo({dataset,isConnected:true});
  assert.equal(f.writes().length,0);
});

test('server CAS rejection preserves selected source and never automatically retries',async()=>{
  const f=fixture();await f.open();f.WB.accountOpen.override=true;
  f.setIntercept(url=>{if(url===safePath)throw Object.assign(Error('source CAS rejected'),{status:409,code:'safe_agent_change_source_changed'});});
  await f.go();await f.go();await flush();
  assert.equal(f.writes().length,1);assert.equal(f.WB.accountOpen.id,'b');assert.equal(f.WB.accountOpen.override,true);assert.equal(f.backend.bindings.codex.accountId,'a');assert.match(f.root.innerHTML,/source CAS rejected/);
});

test('matching success receipt with uncertain submitted result remains locked without success toast',async()=>{
  const f=fixture();await f.open();f.setIntercept((url,body)=>url===safePath?{...receipt(body),result:{uncertain:true,submitted:true}}:undefined);
  await f.go();await f.go();await f.ACTIONS.wbRecheck();
  assert.equal(f.writes().length,1);assert.equal(f.WB.accountOpen.id,'b');assert.equal(f.WB.accountOpen.executionUnknown,true);
  assert.match(f.root.innerHTML,/配置\/启动已提交/);assert.equal(f.notices.some(([text])=>text==='已打开'),false);
});

test('already applied or started error envelopes cannot be replayed from the current panel',async()=>{
  for(const flag of ['applied','started']){
    const f=fixture();await f.open();f.setIntercept(url=>{if(url===safePath)throw Object.assign(Error('submitted-state-write-failed'),{[flag]:true});});
    await f.go();await f.go();await f.ACTIONS.wbRecheck();
    assert.equal(f.writes().length,1);assert.equal(f.WB.accountOpen.executionUnknown,true);assert.doesNotMatch(f.root.innerHTML,/data-act="wbRecheck"/);
  }
});
