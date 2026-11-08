import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
const source=fs.readFileSync(new URL('../internal/console/web/codex-account-name.js',import.meta.url),'utf8');
function gate(){let resolve;return {promise:new Promise(r=>resolve=r),resolve};}
function fixture(){
  const nodes=new Map(),calls=[],notices=[],listeners={};let sequence=0,intercept;
  const row={id:'fixture',name:'developer@example.test',identityEpoch:2};
  const context=vm.createContext({TextEncoder,document:{addEventListener:(name,fn)=>listeners[name]=fn},ACTIONS:{},OFFICIAL_PANEL:{epoch:0},S:{route:'accounts',local:{accounts:[{officialAccount:row}]}},
    $:id=>nodes.get(id),officialLibraryAccounts:rows=>rows,
    officialPanel(){context.OFFICIAL_PANEL.epoch++;nodes.clear();for(const id of ['#officialPanel','#officialNameInput','#officialError'])nodes.set(id,{value:'',textContent:'',focus(){}});},
    closeOfficialPanel(){context.OFFICIAL_PANEL.epoch++;nodes.clear();},busy:async(_b,fn)=>fn(),toast:v=>notices.push(v),
    officialMutationToken:()=>++sequence,officialMutationCurrent:v=>v===sequence,refreshOfficialCards:async()=>{},
    api:async(path,body)=>{calls.push({path,body});return intercept?intercept(path,body):{ok:true};},});
  vm.runInContext(source,context);
  return {context,nodes,calls,notices,row,listeners,open:()=>context.ACTIONS.officialRename({dataset:{id:'fixture'}}),save:()=>context.ACTIONS.officialNameSave({}),reset:()=>context.ACTIONS.officialNameDefault({}),intercept:fn=>intercept=fn};
}
test('rename freezes exact identity and keeps email as default without needing extra setup',async()=>{
 const f=fixture();f.open();assert.equal(f.nodes.get('#officialNameInput').value,'developer@example.test');
 f.nodes.get('#officialNameInput').value='工作账号';await f.save();assert.deepEqual(JSON.parse(JSON.stringify(f.calls[0])),{path:'/api/local/codex/rename',body:{id:'fixture',identityEpoch:2,name:'工作账号'}});assert.equal(f.notices.length,1);
 f.open();await f.reset();assert.equal(f.calls[1].body.name,'');
});
test('duplicate save does not post twice and late completion cannot close a new dialog',async()=>{
 const f=fixture(),pending=gate();f.open();f.intercept(()=>pending.promise);const first=f.save();await f.save();assert.equal(f.calls.length,1);
 f.open();const next=f.nodes.get('#officialPanel');pending.resolve({ok:true});await first;assert.equal(f.nodes.get('#officialPanel'),next);assert.equal(f.notices.length,0);
});
test('old UI cannot rename a replaced identity and missing identity cannot open',async()=>{
 const f=fixture();f.open();f.row.identityEpoch=3;await f.save();assert.equal(f.calls[0].body.identityEpoch,2);
 delete f.row.identityEpoch;assert.throws(f.open,/刷新账号列表/);
});
test('invalid values and ambiguous replies do not claim success or automatically retry',async()=>{
 const f=fixture();f.open();f.nodes.get('#officialNameInput').value='bad\u0000name';await assert.rejects(f.save(),/格式无效/);assert.equal(f.calls.length,0);
 f.nodes.get('#officialNameInput').value='开发';f.intercept(()=>({saved:true,ok:false}));await f.save();assert.equal(f.calls.length,1);assert.match(f.nodes.get('#officialError').textContent,/列表同步未完成/);assert.equal(f.notices.length,0);
});
test('UTF-8 and code-point limits reject oversized Chinese or emoji before dispatch',async()=>{
 const f=fixture();f.open();for(const value of ['中'.repeat(107),'🎨'.repeat(81),'a'.repeat(121)]) {
   f.nodes.get('#officialNameInput').value=value;await assert.rejects(f.save(),/格式无效/);
 }
 assert.equal(f.calls.length,0);f.nodes.get('#officialNameInput').value='中'.repeat(106);await f.save();assert.equal(f.calls.length,1);
});
test('Escape closes only the nested name dialog, never the underlying API library',()=>{
 const f=fixture();let stopped=0;f.open();f.listeners.keydown({key:'Escape',preventDefault(){},stopImmediatePropagation(){stopped++;}});
 assert.equal(stopped,1);assert.equal(f.context.S.route,'accounts');assert.equal(f.nodes.size,0);
 f.listeners.keydown({key:'Escape',preventDefault(){},stopImmediatePropagation(){stopped++;}});assert.equal(stopped,1);
});
