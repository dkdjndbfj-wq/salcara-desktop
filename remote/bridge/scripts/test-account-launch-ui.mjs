import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';

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
  const targets=['codex-desktop','codex','claude','claude-desktop'];
  const account=id=>({id,name:'演示 '+id,sourceRevision:'src-v1-'+id.repeat(64),kind:'api',baseUrl:'https://example.test/v1',wire:'auto',models:['gpt-fixture','claude-fixture'],model:'',keyMasked:'fixture-mask'});
  const backend={accounts:[account('a'),account('b')],tools:targets.map(id=>({id,name:id,kind:id.split('-')[0],available:true})),bindings:Object.fromEntries(targets.map(id=>[id,{accountId:'a',model:'gpt-fixture',override:false}]))};
  const S={local:clone(backend),route:'accounts'}, ACTIONS={};
  const document={addEventListener:(type,fn)=>{if(!listeners.has(type))listeners.set(type,[]);listeners.get(type).push(fn);},getElementById:id=>nodes.get(id)};
  let intercept=null;
  const context=vm.createContext({S,ACTIONS,document,URL,console,PAGES:{overview:{},accounts:{}},ICONS:{},TOOL_LABEL:{},
    $:sel=>sel==='#modalRoot'?root:nodes.get(sel.slice(1)),esc:value=>String(value??'').replaceAll('&','&amp;').replaceAll('<','&lt;').replaceAll('"','&quot;'),icon:()=>'',ico:()=>'',
    remoteLampHTML:()=>'',desktopRemoteLamp:()=>({}),renderApprovalsBox(){},localEditor(){},clearTimeout(){},setTimeout(){throw Error('unexpected timer');},
    renderLocalCards:()=>cards.push('api'),toast:(...args)=>notices.push(args),busy:async(_button,action)=>action(),
    api:async(url,body)=>{calls.push({url,body:clone(body)});if(intercept){const value=await intercept(url,body);if(value!==undefined)return value;}
      if(url==='/api/local/accounts')return clone(backend);
      if(url==='/api/local/models')return {models:backend.accounts.find(a=>a.id===body.id)?.models||[]};
      if(url==='/api/local/claude-desktop/status')return {supported:true,managed:false};
      if(url==='/api/local/bind'){Object.assign(backend.bindings[body.target],{accountId:body.id,model:body.model,override:body.override});return {ok:true};}
      if(['/api/local/switch','/api/local/claude-desktop/switch'].includes(url))return {ok:true};
      throw Error('unexpected route '+url);
    },
  });
  vm.runInContext(workbench,context);vm.runInContext(claude,context);
  const api=vm.runInContext('({wbOpenAccount,wbAccountModels,WB})',context);
  const button=act=>root.children.find(node=>node.dataset.act===act);
  const ready=()=>{const node=nodes.get('wbReady')||nodes.get('cdReady');assert.ok(node);node.checked=true;};
  const change=async node=>{for(const listener of listeners.get('change')||[])await listener({target:node});};
  return {context,S,ACTIONS,backend,calls,notices,cards,root,nodes,button,ready,change,...api,
    open:(target='codex',id='b')=>api.wbOpenAccount(target,id,{isConnected:true,dataset:{target,id}}),
    go:()=>{const buttonNode=button('cdGo')||button('wbOpenGo');assert.ok(buttonNode);return ACTIONS[buttonNode.dataset.act](buttonNode);},
    setIntercept:fn=>intercept=fn,writes:()=>calls.filter(call=>call.body&&call.url!=='/api/local/models'),
  };
}

test('all four API-card entries only stage the named API; confirmation starts unchecked',async()=>{
  for(const target of ['codex-desktop','codex','claude','claude-desktop']){
    const f=fixture();await f.open(target);assert.match(f.root.innerHTML,/演示 b/);assert.match(f.root.innerHTML,/同系列工具任务已经结束/);
    assert.equal(f.backend.bindings[target].accountId,'a');assert.equal(f.writes().length,0);
    assert.equal((f.nodes.get('cdReady')||f.nodes.get('wbReady')).checked,false);await f.go();assert.equal(f.writes().length,0);
  }
});

test('launch progress follows actual binding and switch acknowledgements without replay',async()=>{
  const f=fixture(),events=[],binding=gate(),launch=gate();
  f.context.beginLaunchProgress=(target,name,steps)=>{events.push(['begin',target,steps.length]);return {step:n=>events.push(['step',n]),finish:()=>events.push(['finish']),fail:()=>events.push(['fail']),cancel:()=>events.push(['cancel'])};};
  await f.open('codex');f.ready();
  f.setIntercept(url=>url==='/api/local/bind'?binding.promise:url==='/api/local/switch'?launch.promise:undefined);
  const pending=f.go();await flush();assert.deepEqual(events,[['begin','codex',3],['step',1]]);
  binding.resolve({ok:true});await flush();assert.deepEqual(events.at(-1),['step',2]);
  launch.resolve({ok:true});await pending;assert.deepEqual(events.at(-1),['finish']);
  assert.equal(f.calls.filter(call=>call.url==='/api/local/switch').length,1);
});

test('progress reports uncertain launch failure and never retries the mutation',async()=>{
  const f=fixture(),events=[];
  f.context.beginLaunchProgress=()=>({step:n=>events.push(n),finish:()=>events.push('finished'),fail:()=>events.push('failed'),cancel:()=>events.push('cancelled')});
  await f.open('claude-desktop');f.ready();f.setIntercept(url=>{if(url==='/api/local/claude-desktop/switch')throw Error('response lost');});
  await f.go();assert.deepEqual(events,[1,'failed']);assert.equal(f.calls.filter(call=>call.url==='/api/local/claude-desktop/switch').length,1);
});

test('empty or negative success envelopes never report launched or replay a mutation',async()=>{
  for(const target of ['codex','claude-desktop'])for(const phase of target==='codex'?['bind','switch']:['switch'])for(const result of [{},{ok:false},{ok:'true'}]) {
    const f=fixture(),events=[];f.context.beginLaunchProgress=()=>({step(){},finish:()=>events.push('done'),fail:()=>events.push('failed'),cancel(){}});
    await f.open(target);f.ready();f.setIntercept(url=>url.endsWith('/'+phase)?result:undefined);await f.go();
    assert.deepEqual(events,['failed']);assert.match(f.root.innerHTML,/结果待确认/);assert.equal(f.calls.filter(c=>c.url.endsWith('/'+phase)).length,1);
    if(phase==='bind')assert.equal(f.calls.some(c=>c.url.endsWith('/switch')),false);
  }
});

test('Agent-page launches reuse progress and reject unconfirmed success envelopes',async()=>{
  for(const target of ['codex-desktop','codex','claude','claude-desktop'])for(const ok of [true,false]) {
    const f=fixture(),events=[];f.context.beginLaunchProgress=()=>({step(){},finish:()=>events.push('done'),fail:()=>events.push('failed'),cancel(){}});
    await f.ACTIONS.wbOpen({dataset:{target}});
    assert.equal(f.writes().length,0);f.ready();
    if(!ok)f.setIntercept(url=>url.endsWith('/switch')?{}:undefined);
    await f.go();assert.deepEqual(events,[ok?'done':'failed']);assert.equal(f.calls.filter(c=>c.url.endsWith('/switch')).length,1);
  }
});
test('official account scope, vault-only and quota-only are enforced outside button rendering',async()=>{
  for(const target of ['claude','claude-desktop']){const f=fixture();f.S.local.accounts[1].officialAccount={id:'official',mode:'codex'};await assert.rejects(f.open(target),/不能用于/);}
  for(const flag of [{libraryOnly:true},{mode:'chatgpt_web_session'},{officialAccount:{mode:'chatgpt_web_session'}}]){const f=fixture();Object.assign(f.S.local.accounts[1],flag);await assert.rejects(f.open(),/不能用于/);}
  for(const target of ['codex','codex-desktop']){const f=fixture();f.S.local.accounts[1].officialAccount={id:'official',mode:'codex'};Object.assign(f.backend.accounts[1],f.S.local.accounts[1]);await f.open(target);assert.match(f.root.innerHTML,/演示 b/);}
});
test('fresh discovery can recover missing cached tools without falling back to a CLI',async()=>{
  const f=fixture();f.S.local.tools=[];await f.open('codex-desktop');assert.match(f.root.innerHTML,/打开 Codex/);assert.equal(f.WB.accountOpen.target,'codex-desktop');assert.equal(f.writes().length,0);
});
test('fresh unavailable or now-protected sources cannot be confirmed from a stale card',async()=>{
  for(const mutation of ['missing','unavailable','quota','revision']){const f=fixture();if(mutation==='missing')f.backend.accounts.pop();else if(mutation==='unavailable')f.backend.tools.find(t=>t.id==='codex').available=false;else if(mutation==='revision')delete f.backend.accounts[1].sourceRevision;else f.backend.accounts[1].officialAccount={mode:'chatgpt_web_session'};
    await f.open();assert.equal(f.WB.accountOpen?.verified,false);f.ready();await f.go();assert.equal(f.writes().length,0);
  }
});
test('duplicate opening and out-of-order card discovery never replace the new selection',async()=>{
  const f=fixture(),first=gate();let reads=0;f.setIntercept(url=>{if(url==='/api/local/accounts'&&++reads===1)return first.promise;});
  const a=f.open('codex','a');await f.open('codex','a');assert.equal(reads,1);await f.open('codex','b');first.resolve(clone(f.backend));await a;
  assert.equal(f.WB.accountOpen.id,'b');assert.match(f.root.innerHTML,/演示 b/);assert.equal(f.writes().length,0);
});
test('closed or navigated discovery cannot restore the modal or overwrite a new view',async()=>{
  for(const close of [true,false]){const f=fixture(),pending=gate();f.setIntercept(url=>url==='/api/local/accounts'?pending.promise:undefined);const opening=f.open();if(close)f.root.innerHTML='';else f.S.route='overview';pending.resolve(clone(f.backend));await opening;assert.equal(f.WB.accountOpen.verified,false);if(close)assert.equal(f.root.innerHTML,'');assert.equal(f.writes().length,0);}
});
test('model reload and picker override are staged without binding or changing the saved model',async()=>{
  const f=fixture();await f.open();const checkbox=f.root.children.find(node=>node.dataset.wbOverride);checkbox.checked=true;await f.change(checkbox);
  await f.wbAccountModels(f.WB.accountOpen,false);assert.equal(f.backend.bindings.codex.accountId,'a');assert.equal(f.backend.bindings.codex.model,'gpt-fixture');assert.equal(f.backend.bindings.codex.override,false);assert.equal(f.WB.accountOpen.override,true);assert.equal(f.writes().length,0);
});
test('confirmation posts frozen card ID, staged override and keepModel through original tool switch',async()=>{
  const f=fixture();await f.open('codex-desktop');f.WB.accountOpen.override=true;f.ready();await f.go();
  assert.deepEqual(f.writes().map(call=>call.url),['/api/local/bind','/api/local/switch']);assert.ok(f.writes().every(call=>call.body.id==='b'&&call.body.expectedSourceRevision==='src-v1-'+'b'.repeat(64)));
  assert.equal(f.writes()[0].body.override,true);assert.equal(f.writes()[1].body.keepModel,true);assert.equal(f.writes()[1].body.target,'codex-desktop');assert.equal(f.root.innerHTML,'');assert.ok(f.cards.includes('api'));
});
test('Claude Desktop only uses dedicated status and switch without model override or generic bind',async()=>{
  const f=fixture();await f.open('claude-desktop');f.ready();await f.go();const writes=f.writes();assert.equal(writes.length,1);assert.equal(writes[0].url,'/api/local/claude-desktop/switch');assert.equal(writes[0].body.id,'b');assert.equal('model'in writes[0].body,false);assert.equal(writes[0].body.confirmed,true);assert.equal(f.root.innerHTML,'');
});
test('same ID with changed source revision, URL or official identity invalidates confirmation',async()=>{
  for(const mutation of ['revision','url','identity']){const f=fixture();await f.open();if(mutation==='revision')f.backend.accounts[1].sourceRevision='different-key-same-mask';else if(mutation==='url')f.backend.accounts[1].baseUrl='https://other.example.test';else f.backend.accounts[1].officialAccount={id:'official',identityEpoch:2};f.ready();await f.go();assert.equal(f.writes().length,0);assert.equal(f.WB.accountOpen.verified,false);assert.match(f.root.innerHTML,/重新打开后确认/);}
});
test('double confirmation and another card cannot interleave with an in-flight bind',async()=>{
  const f=fixture(),pending=gate();await f.open();f.ready();f.setIntercept(url=>url==='/api/local/bind'?pending.promise:undefined);const first=f.go();await flush();await f.go();await f.open('claude','a');assert.equal(f.WB.accountOpen.id,'b');assert.equal(f.writes().length,1);pending.resolve({ok:true});await first;assert.equal(f.writes().filter(c=>c.url==='/api/local/switch').length,1);
});
test('closing during bind cancels the subsequent restart and does not re-open its dialog',async()=>{
  const f=fixture(),pending=gate();await f.open();f.ready();f.setIntercept(url=>url==='/api/local/bind'?pending.promise:undefined);const start=f.go();await flush();f.root.innerHTML='';pending.resolve({ok:true});await start;assert.equal(f.calls.some(c=>c.url==='/api/local/switch'),false);assert.equal(f.root.innerHTML,'');
});
test('late restart success or failure never closes or repaints an unrelated modal',async()=>{
  for(const failed of [true,false]){const f=fixture(),pending=gate();await f.open();f.ready();f.setIntercept(url=>url==='/api/local/switch'?pending.promise:undefined);const start=f.go();await flush();f.root.innerHTML='<div id="other">new modal</div>';if(failed)pending.reject(Error('late failure'));else pending.resolve({ok:true});await start;assert.match(f.root.innerHTML,/new modal/);assert.equal(f.notices.some(([text])=>text==='已打开'||text==='late failure'),false);}
});
test('deleted Claude account during model refresh safely invalidates the modal',async()=>{
  const f=fixture();await f.open('claude-desktop');f.backend.accounts.pop();await f.wbAccountModels(f.WB.accountOpen,false);assert.equal(f.WB.accountOpen,null);assert.equal(f.root.innerHTML,'');assert.equal(f.writes().length,0);
});
test('an older account model read cannot overwrite or bind a newer account-card dialog',async()=>{
  const f=fixture(),pending=gate();await f.open('codex','a');
  f.setIntercept(url=>url==='/api/local/models'?pending.promise:undefined);
  const old=f.wbAccountModels(f.WB.accountOpen,true);await flush();
  assert.equal(f.calls.at(-1).url,'/api/local/models');assert.equal(f.calls.at(-1).body.id,'a');
  await f.open('codex','b');const latest=f.WB.accountOpen,revision=f.S.localAccountsRevision;
  pending.resolve({models:['old-catalog']});await old;
  assert.equal(f.WB.accountOpen,latest);assert.equal(f.WB.accountOpen.id,'b');assert.equal(f.S.localAccountsRevision,revision);
  assert.match(f.root.innerHTML,/演示 b/);assert.equal(f.writes().length,0);
});
test('unmanaged or unavailable Claude capabilities and mismatched button IDs cannot switch',async()=>{
  for(const status of [{supported:false},{supported:true,managed:true}]){const f=fixture();f.setIntercept(url=>url.endsWith('/status')?status:undefined);await f.open('claude-desktop');f.ready();await f.go();assert.equal(f.writes().length,0);}
  const f=fixture();await f.open();f.ready();await f.ACTIONS.wbOpenGo({dataset:{id:'a',target:'codex'},isConnected:true});assert.equal(f.writes().length,0);
});
test('source CAS rejection between fresh read and bind/switch never retries or switches a replacement key',async()=>{
  for(const target of ['codex','claude-desktop'])for(const phase of target==='codex'?['bind','switch']:['switch']){
    const f=fixture();await f.open(target);f.ready();let attempted=0;
    f.setIntercept((url,body)=>{
      if(url==='/api/local/bind'&&phase==='switch'){f.backend.accounts[1].sourceRevision='src-v1-'+'c'.repeat(64);return {ok:true};}
      if(url.endsWith('/'+phase)&&body){attempted++;assert.equal(body.expectedSourceRevision,'src-v1-'+'b'.repeat(64));f.backend.accounts[1].sourceRevision='src-v1-'+'c'.repeat(64);throw Error('账号来源已变化，请重新确认');}
    });
    await f.go();assert.equal(attempted,1);assert.match(f.root.innerHTML,/账号来源已变化/);assert.equal(f.calls.filter(c=>c.url.endsWith('/switch')).length,phase==='bind'?0:1);
  }
});
test('binding completion cannot replace an identical but newly opened account-card modal',async()=>{
  const f=fixture(),pending=gate();await f.open('codex','b');f.ready();const oldTitle=f.nodes.get('wbOpenTitle'),originalHTML=f.root.innerHTML;
  f.setIntercept(url=>url==='/api/local/bind'?pending.promise:undefined);const binding=f.go();await flush();
  assert.equal(f.writes().length,1);assert.equal(f.writes()[0].url,'/api/local/bind');
  // A replaced dialog can have identical markup/IDs; ownership follows the
  // original node, never its text or selector alone.
  f.root.innerHTML=originalHTML;const newTitle=f.nodes.get('wbOpenTitle'),latest=f.WB.accountOpen;
  assert.ok(newTitle);assert.notEqual(oldTitle,newTitle);assert.equal(latest.verified,true);
  pending.resolve({ok:true});await binding;
  assert.equal(f.WB.accountOpen,latest);assert.equal(f.nodes.get('wbOpenTitle'),newTitle);
  assert.equal(f.calls.some(c=>c.url==='/api/local/switch'),false);assert.equal(f.notices.some(([text])=>text==='已打开'),false);
});
test('a late Claude capability response cannot repaint a later Codex card dialog',async()=>{
  const f=fixture(),pending=gate();f.setIntercept(url=>url.endsWith('/status')?pending.promise:undefined);const old=f.open('claude-desktop');await flush();await f.open('codex','a');pending.resolve({supported:true});await old;
  assert.equal(f.WB.accountOpen.target,'codex');assert.match(f.root.innerHTML,/演示 a/);assert.doesNotMatch(f.root.innerHTML,/cdTitle/);assert.equal(f.writes().length,0);
});
test('early confirmation on cached source is cleared when fresh source first arrives',async()=>{
  const f=fixture(),pending=gate();f.setIntercept(url=>url==='/api/local/accounts'?pending.promise:undefined);const opening=f.open();
  const checkbox=f.nodes.get('wbReady');checkbox.checked=true;await f.change(checkbox);assert.equal(f.WB.accountOpen.ready,true);
  f.backend.accounts[1].sourceRevision='src-v1-'+'c'.repeat(64);f.backend.accounts[1].baseUrl='https://new-source.example.test/v1';pending.resolve(clone(f.backend));await opening;
  assert.equal(f.WB.accountOpen.ready,false);assert.equal(f.nodes.get('wbReady').checked,false);assert.equal(f.WB.accountOpen.sourceRevision,'src-v1-'+'c'.repeat(64));await f.go();assert.equal(f.writes().length,0);
});
