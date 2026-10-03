// Runs an installed official CLI against an explicitly selected fixture HOME.
// Seed uses a synthetic turn against the loopback-only rejecting mock provider.
// No real provider/key or paid inference is used. No auth/body is ever printed.
const { spawn } = require('node:child_process');
const path = require('node:path');
const assert = require('node:assert/strict');
const [exe, root, stage = 'check'] = process.argv.slice(2);
assert(exe && root && path.isAbsolute(root), 'explicit fixture paths required');
assert(root.includes('shared-smoke-home'), 'never run against a real Codex home');
const child = spawn(exe, ['app-server', '--listen', 'stdio://'], {
  cwd: path.dirname(root), windowsHide: true,
  env: { ...process.env, CODEX_HOME: root, OPENAI_API_KEY: '', OPENAI_BASE_URL: '', SUB2API_API_KEY: '' },
  stdio: ['pipe', 'pipe', 'pipe'],
});
let nextID = 0, buffer = '';
const pending = new Map();
child.stderr.resume();
child.stdout.on('data', data => {
  buffer += data.toString();
  while (buffer.includes('\n')) {
    const end = buffer.indexOf('\n');
    const line = buffer.slice(0,end); buffer = buffer.slice(end+1);
    let item; try { item = JSON.parse(line); } catch { continue; }
    const task = pending.get(item.id);
    if (!task) continue;
    pending.delete(item.id);
    if (item.error) task.reject(new Error('RPC failed: '+task.method+' ('+item.error.code+'): '+item.error.message));
    else task.resolve(item.result);
  }
});
function rpc(method, params) {
  return new Promise((resolve,reject)=> {
    const id=++nextID; pending.set(id,{ resolve,reject,method });
    child.stdin.write(JSON.stringify({ jsonrpc:'2.0',id,method,params })+'\n');
  });
}
const timeout=setTimeout(()=>{ child.kill(); console.error('Fixture RPC timed out'); process.exitCode=1; },25000);
async function main() {
  await rpc('initialize',{ clientInfo:{ name:'salcara_history_smoke',version:'1.2.0' },capabilities:{ experimentalApi:true } });
  child.stdin.write(JSON.stringify({jsonrpc:'2.0',method:'initialized'})+'\n');
  const cfg=await rpc('config/read',{ includeLayers:false });
  assert.equal(cfg.config.openai_base_url,'http://127.0.0.1:47833/v1');
  assert.equal(cfg.config.model_provider || 'openai','openai');
  if (stage==='seed') {
    const started=await rpc('thread/start',{model:'smoke-codex',modelProvider:'openai',cwd:path.dirname(root),ephemeral:false});
    await rpc('thread/name/set',{threadId:started.thread.id,name:'Salcara shared-history fixture'});
    const turn=await rpc('turn/start',{threadId:started.thread.id,input:[{type:'text',text:'Local fixture history check only. Do not call tools.'}]});
    try { await rpc('turn/interrupt',{threadId:started.thread.id,turnId:turn.turn.id}); } catch { /* mock can finish before interruption */ }
    await new Promise(resolve=>setTimeout(resolve,1500));
    console.log('Official CLI: fixture thread seeded and interrupted using loopback mock only');
  } else {
    const list=await rpc('thread/list',{limit:100,modelProviders:null,sourceKinds:[]});
    console.log('Fixture thread count: '+list.data.length+'; fields: '+Object.keys(list.data[0]||{}).join(','));
    assert(list.data.some(t=>t.name==='Salcara shared-history fixture' || String(t.preview).includes('Local fixture history check')), 'original thread missing with current provider filtering');
    console.log('Official CLI: original fixture thread remains visible; same HOME and provider');
  }
}
main().catch(error=>{ console.error('Official CLI fixture validation: '+error.message); process.exitCode=1; }).finally(()=>{ clearTimeout(timeout); child.stdin.end(); child.kill(); });
