// No inference: inspect permission acknowledgements in an isolated smoke HOME.
'use strict';
const fs = require('node:fs'), path = require('node:path'), assert = require('node:assert/strict');
const {spawn} = require('node:child_process');
const readline = require('node:readline');
const fixture = path.resolve(process.argv[2] || '');
assert(fixture.startsWith(path.resolve(__dirname, '../output/native-session-smoke-')));
const config = JSON.parse(fs.readFileSync(path.join(fixture, 'bridge-config/config.json')));
const env = Object.fromEntries(Object.entries(process.env).filter(([key]) => ['SYSTEMROOT', 'WINDIR', 'COMSPEC', 'PATH', 'PATHEXT', 'TEMP', 'TMP'].includes(key.toUpperCase())));
Object.assign(env, {CODEX_HOME:path.join(fixture,'codex-home'), HOME:path.join(fixture,'user-profile'), USERPROFILE:path.join(fixture,'user-profile'), APPDATA:path.join(fixture,'appdata'), LOCALAPPDATA:path.join(fixture,'localappdata')});
const child = spawn(config.localToolPaths.codex, ['app-server','--listen','stdio://','-c','model_provider="openai"','-c','openai_base_url="http://127.0.0.1:9/v1"'], {env, cwd:path.join(fixture,'project'), windowsHide:true, stdio:['pipe','pipe','pipe']});
child.stderr.resume();
let sequence = 0; const pending = new Map(), settingsNotes = [];
readline.createInterface({input:child.stdout}).on('line', line => {let value;try {value=JSON.parse(line);}catch{return;}if(value.method==='thread/settings/updated') settingsNotes.push(value.params);const task=pending.get(value.id);if(!task)return;pending.delete(value.id);clearTimeout(task.timer);value.error?task.reject(new Error(JSON.stringify(value.error))):task.resolve(value.result);});
function rpc(method, params) {return new Promise((resolve,reject) => {const id=++sequence,timer=setTimeout(()=>{pending.delete(id);reject(new Error(method+' timeout'));},15000);pending.set(id,{resolve,reject,timer});child.stdin.write(JSON.stringify({id,method,params})+'\n');});}
(async()=>{try {
  await rpc('initialize',{clientInfo:{name:'salcara_permission_probe',version:'1'},capabilities:{experimentalApi:true}});
  child.stdin.write('{"method":"initialized"}\n');
  const list=await rpc('thread/list',{limit:20,modelProviders:[]});assert(list.data.length>=1);
  const id=list.data[0].id,results=[];
  for(const params of [{threadId:id,modelProvider:'openai'}, {threadId:id,modelProvider:'openai',approvalPolicy:'on-request',sandbox:'workspace-write',approvalsReviewer:'user',model:'non-openai-fixture'}]) {
    const value=await rpc('thread/resume',params);
    results.push({approvalPolicy:value.approvalPolicy,approvalsReviewer:value.approvalsReviewer,sandbox:value.sandbox,status:value.thread.status,sameThread:value.thread.id===id,activePermissionProfile:value.activePermissionProfile});
  }
  await rpc('thread/settings/update',{threadId:id,approvalPolicy:'on-request',approvalsReviewer:'user',sandboxPolicy:{type:'workspaceWrite',writableRoots:[path.join(fixture,'project')],networkAccess:false,excludeTmpdirEnvVar:false,excludeSlashTmp:false}});
  const updated=await rpc('thread/resume',{threadId:id});
  results.push({phase:'settings-update',approvalPolicy:updated.approvalPolicy,approvalsReviewer:updated.approvalsReviewer,sandbox:updated.sandbox,status:updated.thread.status});
  const started=await rpc('thread/start',{cwd:path.join(fixture,'project'),modelProvider:'openai',model:'non-openai-fixture',approvalPolicy:'on-request',sandbox:'workspace-write',approvalsReviewer:'user',ephemeral:true});
  results.push({phase:'new-thread',approvalPolicy:started.approvalPolicy,approvalsReviewer:started.approvalsReviewer,sandbox:started.sandbox,status:started.thread.status});
  await rpc('thread/settings/update',{threadId:started.thread.id,approvalPolicy:'on-request',approvalsReviewer:'user',sandboxPolicy:{type:'workspaceWrite',writableRoots:[],networkAccess:false,excludeTmpdirEnvVar:false,excludeSlashTmp:false}});
  try {
    const fresh=await rpc('thread/resume',{threadId:started.thread.id});
    results.push({phase:'new-thread-updated',approvalPolicy:fresh.approvalPolicy,approvalsReviewer:fresh.approvalsReviewer,sandbox:fresh.sandbox,status:fresh.thread.status});
  } catch(error) { results.push({phase:'new-thread-readback-error',error:error.message}); }
  await new Promise(resolve=>setTimeout(resolve,100));
  const applied=settingsNotes.find(note=>note.threadId===started.thread.id)?.threadSettings;
  if(applied) results.push({phase:'new-thread-update-notification',approvalPolicy:applied.approvalPolicy,approvalsReviewer:applied.approvalsReviewer,sandbox:applied.sandboxPolicy});
  for(const permissions of [':workspace-write', ':danger-full-access']) {
    try {
      const fresh=await rpc('thread/start',{cwd:path.join(fixture,'project'),modelProvider:'openai',model:'non-openai-fixture',approvalPolicy:permissions===':workspace-write'?'on-request':'never',permissions,approvalsReviewer:'user',ephemeral:true});
      results.push({phase:'named-profile',permissions,approvalPolicy:fresh.approvalPolicy,approvalsReviewer:fresh.approvalsReviewer,sandbox:fresh.sandbox,status:fresh.thread.status});
    } catch(error) { results.push({phase:'named-profile-error',permissions,error:error.message}); }
  }
  const report={modelRequests:0,results};fs.writeFileSync(path.join(fixture,'permission-proof.json'),JSON.stringify(report,null,2));console.log(JSON.stringify(report,null,2));
}finally{child.stdin.end();await new Promise(resolve=>{child.once('exit',resolve);setTimeout(()=>{if(child.exitCode===null)child.kill();resolve();},2000).unref();});}})().catch(error=>{console.error(error.message);process.exitCode=1;});
