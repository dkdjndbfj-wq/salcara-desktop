// Real installed Codex CLI, synthetic loopback upstream, explicit fixture HOME.
const { spawn } = require('node:child_process');
const fs = require('node:fs');
const path = require('node:path');
const assert = require('node:assert/strict');
const readline = require('node:readline');
const [exe, gatewayExe, protocol = 'chat'] = process.argv.slice(2);
assert(exe && gatewayExe && path.isAbsolute(exe) && path.isAbsolute(gatewayExe));
assert(['chat', 'anthropic'].includes(protocol));
const fixture = path.resolve(__dirname, '../output/protocol-smoke-' + protocol + '-' + Date.now());
assert(fixture.startsWith(path.resolve(__dirname, '../output') + path.sep));
fs.mkdirSync(fixture, { recursive: true });
const home = path.join(fixture, 'codex-home');
fs.mkdirSync(home, { recursive: true });
let gateway, child, rpcID = 0;
const pending = new Map(), turns = new Map(), notifications = [];
const deadline = setTimeout(() => { console.error('protocol fixture timed out'); child?.kill(); gateway?.kill(); process.exitCode = 1; }, 45000);
function rpc(method, params) {
  return new Promise((resolve, reject) => { const id=++rpcID; pending.set(id,{resolve,reject}); child.stdin.write(JSON.stringify({jsonrpc:'2.0',id,method,params})+'\n'); });
}
function waitTurn(id) {
  const old=notifications.find(x=>x.method==='turn/completed' && x.params.turn.id===id);
  if (old) return Promise.resolve(old.params.turn);
  return new Promise(resolve=>turns.set(id,resolve));
}
async function main() {
  gateway=spawn(gatewayExe,[protocol],{windowsHide:true,stdio:['ignore','pipe','pipe']}); gateway.stderr.resume();
  const base = await new Promise((resolve,reject)=> { const lines=readline.createInterface({input:gateway.stdout}); lines.once('line',line=>{ lines.close(); resolve(line); }); gateway.once('error',reject); });
  assert(String(base).startsWith('http://127.0.0.1:'));
  fs.writeFileSync(path.join(home,'config.toml'),`model = "non-openai-fixture"\nmodel_provider = "openai"\nopenai_base_url = "${base}/gateway/codex/v1"\ncli_auth_credentials_store = "file"\n`);
  fs.writeFileSync(path.join(home,'auth.json'),JSON.stringify({auth_mode:'apikey',OPENAI_API_KEY:'synthetic-loopback-credential'}));
  child=spawn(exe,['app-server','--listen','stdio://'],{cwd:fixture,windowsHide:true,env:{...process.env,CODEX_HOME:home,OPENAI_API_KEY:'synthetic-loopback-credential',OPENAI_BASE_URL:'',SUB2API_API_KEY:''},stdio:['pipe','pipe','pipe']});
  child.stderr.resume();
  readline.createInterface({input:child.stdout}).on('line',line=> {
    let item;try{item=JSON.parse(line);}catch{return;}
    if(item.id!==undefined && pending.has(item.id)){ const p=pending.get(item.id);pending.delete(item.id);if(item.error)p.reject(new Error(item.error.message));else p.resolve(item.result);return;}
    notifications.push(item);
    if(item.method==='turn/completed'){ const id=item.params.turn.id;turns.get(id)?.(item.params.turn);turns.delete(id); }
    // Permission should never be needed for fixture-only workspace edits.
    if(item.id!==undefined && item.method){ child.stdin.write(JSON.stringify({jsonrpc:'2.0',id:item.id,result:{decision:'decline'}})+'\n'); }
  });
  await rpc('initialize',{clientInfo:{name:'salcara_protocol_fixture',version:'1.3.0'},capabilities:{experimentalApi:true}});
  child.stdin.write(JSON.stringify({jsonrpc:'2.0',method:'initialized'})+'\n');
  const started=await rpc('thread/start',{model:'non-openai-fixture',modelProvider:'openai',cwd:fixture,approvalPolicy:'never',sandbox:'workspace-write',ephemeral:false});
  const id=started.thread.id;
  const first=await rpc('turn/start',{threadId:id,input:[{type:'text',text:'First fixture-only conversation message. Reply only.'}]});
  assert.equal((await waitTurn(first.turn.id)).status,'completed');
  const second=await rpc('turn/start',{threadId:id,input:[{type:'text',text:'Second turn in the SAME fixture conversation: apply the synthetic patch to fixture-result.txt.'}]});
  assert.equal((await waitTurn(second.turn.id)).status,'completed');
  const stats=await (await fetch(base+'/fixture-stats')).json();
  const commandExecuted = notifications.some(x => x.method==='item/completed' && x.params?.item?.type==='commandExecution' && x.params.item.exitCode===0);
  if (!commandExecuted && !fs.existsSync(path.join(fixture,'fixture-result.txt'))) {
    console.error(JSON.stringify({stats, fixture, notificationTypes:notifications.map(x=>x.method).filter(Boolean), errors:notifications.filter(x=>x.method==='error')}));
  }
  assert(commandExecuted || (fs.existsSync(path.join(fixture,'fixture-result.txt')) && fs.readFileSync(path.join(fixture,'fixture-result.txt'),'utf8').includes('fixture-patch-marker')), 'official CLI did not execute converted local tool');
  assert(!notifications.some(x=>x.method==='error'), 'official CLI encountered transport errors/retries');
  assert(stats.modelOK && stats.calls>=3 && stats.patchCalls===1 && stats.historyOK, JSON.stringify(stats));
  const sessions=await rpc('thread/list',{limit:100,modelProviders:null,sourceKinds:[]});
  assert(sessions.data.some(t=>t.id===id), 'same original provider lost fixture session');
  console.log(`PASS ${protocol}: official Codex CLI, 2 turns SAME session, converted local tool executed, original model retained, no transport retries; ${stats.calls} synthetic requests, no paid API`);
}
main().catch(e=>{console.error(e.message);process.exitCode=1;}).finally(()=>{clearTimeout(deadline);child?.stdin.end();child?.kill();gateway?.kill();});
