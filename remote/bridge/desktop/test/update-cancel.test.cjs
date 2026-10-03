'use strict';
const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const os=require('node:os');
const path=require('node:path');
const crypto=require('node:crypto');
const zlib=require('node:zlib');
const {createUpdater}=require('../updater.cjs');
const {validateArchive}=require('../update-archive.cjs');

test('cancelling verification never publishes a ready installation', async () => {
  const root=fs.mkdtempSync(path.join(os.tmpdir(),'salcara-cancel-test-'));
  const appDir=path.join(root,'Salcara Bridge');fs.mkdirSync(appDir);fs.writeFileSync(path.join(appDir,'.salcara-install.json'),JSON.stringify({product:'salcara-desktop',version:'1.6.0'}));
  const bytes=zlib.gzipSync(Buffer.alloc(1024));
  const {publicKey,privateKey}=crypto.generateKeyPairSync('ed25519');
  const name='Salcara-Bridge-1.6.1-'+process.platform+'-'+process.arch+'.tar.gz';
  const body=Buffer.from(JSON.stringify({schema:1,product:'salcara-desktop',repo:'o/r',version:'1.6.1',files:{[`${process.platform}-${process.arch}`]:{name,size:bytes.length,sha256:crypto.createHash('sha256').update(bytes).digest('hex')}}}));
  const prefix='https://github.com/o/r/releases/download/v1.6.1/';
  const assets={'latest.json':body,'latest.json.sig':Buffer.from(crypto.sign(null,body,privateKey).toString('base64')),[name]:bytes};
  const saved=global.fetch;let updater;
  global.fetch=async (url)=>url.endsWith('/releases/latest')?new Response(JSON.stringify({tag_name:'v1.6.1',assets:Object.keys(assets).map((name)=>({name,browser_download_url:prefix+name}))})):new Response(assets[url.slice(prefix.length)]);
  try {
    updater=createUpdater({app:{getVersion:()=> '1.6.0'},shell:{},platform:process.platform,arch:process.arch,execPath:path.join(appDir,'fixture.exe'),isPackaged:true,repo:'o/r',publicKey:publicKey.export({format:'der',type:'spki'}).subarray(-32).toString('base64'),userData:path.join(root,'ud'),onChange:(state)=>{if(state.phase==='verifying')updater.cancel();}});
    assert.equal((await updater.check(true)).canInstall,true);
    assert.equal((await updater.download()).phase,'available');
    assert.equal(fs.existsSync(path.join(root,'.Salcara Bridge.update')),false);
    const controller=new AbortController();controller.abort();await assert.rejects(validateArchive(path.join(root,'not-opened'),controller.signal),{name:'AbortError'});
  } finally {global.fetch=saved;fs.rmSync(root,{recursive:true,force:true});}
});
