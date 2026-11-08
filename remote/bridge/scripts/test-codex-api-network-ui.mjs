import test from 'node:test';
import assert from 'node:assert/strict';
import {readFile} from 'node:fs/promises';
import vm from 'node:vm';
const source=await readFile(new URL('../internal/console/web/codex-api-network.js',import.meta.url),'utf8');
const deferred=()=>{let resolve,reject;const promise=new Promise((yes,no)=>{resolve=yes;reject=no});return{promise,resolve,reject}};
async function settle(){for(let i=0;i<12;i++)await Promise.resolve()}
function harness(handler){
 const nodes=new Map(),requests=[],listeners={};let observer;
 const context=vm.createContext({OFFICIAL_PANEL:{epoch:1},ACTIONS:{},api:async(path,body)=>{requests.push({path,body});return handler(body)},busy:async(_b,fn)=>fn(),esc:String,icon:()=>'',
  $:(id)=>nodes.get(id)||null,document:{body:{},addEventListener:(name,fn)=>{listeners[name]=fn}},MutationObserver:class{constructor(fn){observer=fn}observe(){}}});
 const createPanel=()=>{
  const parent={isConnected:true,childElementCount:1,querySelector:(id)=>nodes.get(id)||null,insertAdjacentHTML:(_p,html)=>{
   const settings={isConnected:true,insertAdjacentHTML(){nodes.set('#apiNetworkEffective',{isConnected:true,textContent:''})},remove(){nodes.delete('#apiNetworkSettings')}};nodes.set('#apiNetworkSettings',settings);
   for(const id of ['apiNetworkPorts','apiNetworkHost','apiNetworkScope','apiNetworkStatus'])nodes.set('#'+id,{isConnected:true,value:html.match(new RegExp('id="'+id+'"[^>]*value="([^"]*)"'))?.[1]||'',textContent:''});
   if(html.includes('id="apiNetworkScope"'))nodes.get('#apiNetworkScope').value=html.includes('value="lan" selected')?'lan':'loopback';
   nodes.set('#apiNetworkSettings textarea',{value:''});
  }};nodes.set('#apiServiceAdvanced',parent);return parent;
 };
 createPanel();vm.runInContext(source,context);observer();
 return {nodes,requests,context,close(){context.OFFICIAL_PANEL.epoch++;for(const n of nodes.values())n.isConnected=false;nodes.clear()},reopen(){createPanel();observer()},action:()=>context.ACTIONS.codexAPINetworkSave({}),retry:()=>context.ACTIONS.codexAPINetworkRetry(),change:(id)=>listeners.input({target:{id}})};
}
const state={revision:'fixture-revision',bindScope:'loopback',ports:[8317],clientHost:'127.0.0.1',listeningPorts:1,baseUrls:['http://127.0.0.1:8317/v1']};
test('network controls load only private console settings and carry revision',async()=>{
 const h=harness((body)=>({...state,revision:body?'new-revision':state.revision}));await settle();
 assert.equal(h.nodes.get('#apiNetworkPorts').value,'8317');await h.action();
 assert.equal(h.requests[1].body.revision,'fixture-revision');assert.equal(h.requests[1].body.ports[0],8317);
 assert.ok(h.requests.every(r=>r.path==='/api/local/codex-api/network'));
});
test('invalid ports never send network mutation',async()=>{
 const h=harness(()=>state);await settle();
 for(const input of ['0','70000','8317,8317','1.5','-1','Infinity','1000,']){
  h.nodes.get('#apiNetworkPorts').value=input;await assert.rejects(h.action());
 }
 assert.equal(h.requests.length,1);
});
test('retired panel read and save cannot fill replacement panel',async()=>{
 const oldRead=deferred();let reads=0;
 const h=harness(()=>++reads===1?oldRead.promise:state);h.close();h.reopen();await settle();
 oldRead.resolve({...state,ports:[9999]});await settle();assert.equal(h.nodes.get('#apiNetworkPorts').value,'8317');
 const save=deferred();const j=harness(body=>body?save.promise:state);await settle();const pending=j.action();j.close();j.reopen();await settle();
 save.resolve({...state,revision:'old-save'});await pending;assert.equal(j.nodes.get('#apiNetworkStatus').textContent,'');
});
test('late network save preserves edited draft and marks it unsaved',async()=>{
 const wait=deferred();const h=harness(body=>body?wait.promise:state);await settle();
 const pending=h.action();h.nodes.get('#apiNetworkPorts').value='8318';h.change('apiNetworkPorts');
 wait.resolve({...state,revision:'new-revision'});await pending;
 assert.equal(h.nodes.get('#apiNetworkPorts').value,'8318');assert.match(h.nodes.get('#apiNetworkStatus').textContent,/尚未保存/);
});
test('network read failure supports retry',async()=>{
 let reads=0;const h=harness(()=>++reads===1?Promise.reject(new Error('denied')):state);await settle();await h.retry();await settle();
 assert.equal(reads,2);assert.equal(h.nodes.get('#apiNetworkPorts').value,'8317');
});
