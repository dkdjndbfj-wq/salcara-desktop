'use strict';
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const zlib = require('node:zlib');
const crypto = require('node:crypto');
const { spawn, spawnSync } = require('node:child_process');
const { validateArchive } = require('../update-archive.cjs');
const { createUpdater, verifyManifest, swapScript } = require('../updater.cjs');

function tarEntry(name, type = '0', data = Buffer.from('test')) {
  const header = Buffer.alloc(512);
  header.write(name, 0, 100); header.write('0000644\0', 100); header.write('0000000\0',108); header.write('0000000\0',116);
  header.write(data.length.toString(8).padStart(11,'0')+'\0',124); header.write('00000000000\0',136);
  header.fill(32,148,156); header.write(type,156); header.write('ustar\0',257); header.write('00',263);
  const sum = header.reduce((total,byte)=>total+byte,0);
  header.write(sum.toString(8).padStart(6,'0')+'\0 ',148);
  return Buffer.concat([header, data, Buffer.alloc((512-data.length%512)%512)]);
}

test('USTAR validation rejects traversal, links, devices, extended overrides and duplicate paths', async () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(),'salcara-archive-test-'));
  const archive = path.join(root,'fixture.tar.gz');
  try {
    fs.writeFileSync(archive,zlib.gzipSync(Buffer.concat([tarEntry('./safe/file'),Buffer.alloc(1024)])));
    await validateArchive(archive);
    for (const entries of [[tarEntry('../escape')],[tarEntry('/absolute')],[tarEntry('C:stream')],[tarEntry('a\\b')],
      [tarEntry('link','2')],[tarEntry('device','3')],[tarEntry('pax','x')],[tarEntry('a'),tarEntry('A')], [tarEntry('a'),tarEntry('a/b')], [tarEntry('NUL.txt')]]) {
      fs.writeFileSync(archive,zlib.gzipSync(Buffer.concat([...entries,Buffer.alloc(1024)])));
      await assert.rejects(validateArchive(archive));
    }
  } finally { fs.rmSync(root,{recursive:true,force:true}); }
});

test('signed manifest must belong to the desktop product', () => {
  const {publicKey,privateKey}=crypto.generateKeyPairSync('ed25519');
  const body=Buffer.from(JSON.stringify({ schema:1, product:'salcara-hub', version:'1.7.0', files:{} }));
  assert.throws(()=>verifyManifest({manifest:body,signature:crypto.sign(null,body,privateKey).toString('base64'),publicKey:publicKey.export({format:'der',type:'spki'}).subarray(-32).toString('base64'),current:'1.6.0',platform:'win32',arch:'x64'}), /不属于桌面/);
});

test('startup acknowledgement requires exact installed version and token-scoped receipt', () => {
  const root=fs.mkdtempSync(path.join(os.tmpdir(),'salcara-ack-test-'));
  const token='a'.repeat(64), receipt=path.join(root,`update-ready-${token}`);
  fs.writeFileSync(path.join(root,'update.json'), JSON.stringify({installing:'1.6.0', token,receipt}));
  const updater=createUpdater({app:{getVersion:()=> '1.6.0'},shell:{},platform:process.platform,arch:process.arch,isPackaged:false,userData:root,onChange(){}});
  assert.deepEqual(updater.afterRestart(),{version:'1.6.0',ok:true});
  assert.equal(fs.readFileSync(receipt,'utf8'),token);
  assert.equal(updater.afterRestart(),null);
  fs.rmSync(root,{recursive:true,force:true,maxRetries:10,retryDelay:100});
});

test('cleanup leaves recovery backups untouched', () => {
  const root=fs.mkdtempSync(path.join(os.tmpdir(),'salcara-cleanup-test-'));
  const appDir=path.join(root,'Salcara Bridge');fs.mkdirSync(appDir);
  const backup=appDir+'.old-123';fs.mkdirSync(backup);fs.writeFileSync(path.join(backup,'user-file'),'keep');
  const updater=createUpdater({app:{getVersion:()=> '1.6.0'},shell:{},platform:process.platform,arch:process.arch,execPath:path.join(appDir,'app.exe'),isPackaged:true,userData:path.join(root,'ud'),onChange(){}});
  updater.cleanup();assert.equal(fs.readFileSync(path.join(backup,'user-file'),'utf8'),'keep');
  fs.rmSync(root,{recursive:true,force:true,maxRetries:10,retryDelay:100});
});

test('Windows swap waits for healthy acknowledgement and rolls back a dead new process', {skip:process.platform!=='win32',timeout:30000}, async () => {
  const root=fs.mkdtempSync(path.join(os.tmpdir(),'salcara-win-update-'));
  const ps=path.join(process.env.SystemRoot,'System32','WindowsPowerShell','v1.0','powershell.exe');
  for (const succeeds of [true,false]) {
    const scope=path.join(root,succeeds?'success':'rollback');fs.mkdirSync(scope);
    const appDir=path.join(scope,'Salcara Bridge'),stage=path.join(scope,'.Salcara Bridge.update'),newDir=path.join(stage,'app'),backupDir=appDir+'.old-123';
    fs.mkdirSync(appDir);fs.mkdirSync(newDir,{recursive:true});
    const launch=path.join(appDir,'fixture.exe');fs.copyFileSync(process.execPath,launch);fs.copyFileSync(process.execPath,path.join(newDir,'fixture.exe'));
    fs.writeFileSync(path.join(appDir,'VERSION'),'old');fs.writeFileSync(path.join(newDir,'VERSION'),'new');
    const permit=path.join(stage,'permit'),receipt=path.join(scope,'receipt'),log=path.join(scope,'update.log'),token='synthetic-test-token';fs.writeFileSync(permit,token);
    const boot=path.join(scope,'boot.cjs');
    fs.writeFileSync(boot, `const fs=require('fs');const path=require('path');if(fs.readFileSync(path.join(path.dirname(process.execPath),'VERSION'),'utf8')==='new'){${succeeds?`fs.writeFileSync(${JSON.stringify(receipt)},${JSON.stringify(token)});`:''}}process.exit(0);`);
    const old=spawn(process.execPath,['-e','setTimeout(()=>{},300)'],{windowsHide:true,stdio:'ignore'});
    const script=path.join(scope,'swap.ps1');fs.writeFileSync(script,'\ufeff'+swapScript({platform:'win32',pid:old.pid,appDir,newDir,backupDir,launch,log,permit,receipt,token}));
    const result=spawnSync(ps,['-NoProfile','-NonInteractive','-ExecutionPolicy','Bypass','-File',script],{windowsHide:true,timeout:20000,env:{...process.env,NODE_OPTIONS:`--require "${boot.replace(/\\/g,'/')}"`}});
    assert.equal(fs.readFileSync(path.join(appDir,'VERSION'),'utf8'),succeeds?'new':'old',result.stderr?.toString());
    assert.equal(fs.existsSync(backupDir),false);
    assert.match(fs.readFileSync(log,'utf8'),succeeds?/updated and healthy/:/rolled back/);
    // Let exit callbacks close synthetic process handles before cleanup.
    await new Promise((resolve) => setTimeout(resolve, 300));
  }
  await fs.promises.rm(root,{recursive:true,force:true,maxRetries:20,retryDelay:100});
});
