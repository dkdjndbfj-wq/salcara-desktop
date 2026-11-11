'use strict';
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const crypto = require('node:crypto');
const zlib = require('node:zlib');
const { EventEmitter } = require('node:events');
const { execFile, execFileSync, spawn } = require('node:child_process');
const execFileAsync = require('node:util').promisify(execFile);
const { createUpdater } = require('../updater.cjs');

function tarEntry(name, data) {
  const header = Buffer.alloc(512);
  header.write(name, 0, 100); header.write('0000644\0', 100);
  header.write('0000000\0', 108); header.write('0000000\0', 116);
  header.write(data.length.toString(8).padStart(11, '0') + '\0', 124);
  header.write('00000000000\0', 136); header.fill(32, 148, 156);
  header.write('0', 156); header.write('ustar\0', 257); header.write('00', 263);
  header.write(header.reduce((sum, byte) => sum + byte, 0).toString(8).padStart(6, '0') + '\0 ', 148);
  return Buffer.concat([header, data, Buffer.alloc((512 - data.length % 512) % 512)]);
}

async function fixture(run) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'salcara-update-lifecycle-'));
  const appDir = path.join(root, 'app'), userData = path.join(root, 'userdata');
  fs.mkdirSync(appDir); fs.mkdirSync(userData);
  fs.writeFileSync(path.join(appDir, '.salcara-install.json'), JSON.stringify({ product: 'salcara-desktop', version: '1.6.1' }));
  fs.writeFileSync(path.join(appDir, 'fixture.exe'), 'old');
  fs.mkdirSync(path.join(appDir,'resources'));
  fs.writeFileSync(path.join(appDir,'resources','SalcaraProbeNode.exe'), 'synthetic-runtime-not-executed');
  const bytes = zlib.gzipSync(Buffer.concat([
    tarEntry('fixture.exe', Buffer.from('new')),
    tarEntry('.salcara-install.json', Buffer.from(JSON.stringify({ product: 'salcara-desktop', version: '1.6.2' }))), Buffer.alloc(1024),
  ]));
  const { publicKey, privateKey } = crypto.generateKeyPairSync('ed25519');
  const name = `Salcara-Bridge-1.6.2-${process.platform}-${process.arch}.tar.gz`;
  const manifest = Buffer.from(JSON.stringify({ schema: 1, product: 'salcara-desktop', repo: 'o/r', version: '1.6.2', files: {
    [`${process.platform}-${process.arch}`]: { name, size: bytes.length, sha256: crypto.createHash('sha256').update(bytes).digest('hex') },
  } }));
  const prefix = 'https://github.com/o/r/releases/download/v1.6.2/';
  const assets = { 'latest.json': manifest, 'latest.json.sig': Buffer.from(crypto.sign(null, manifest, privateKey).toString('base64')), [name]: bytes };
  const previousFetch = global.fetch;
  global.fetch = async url => url.endsWith('/releases/latest')
    ? new Response(JSON.stringify({ tag_name: 'v1.6.2', assets: Object.keys(assets).map(name => ({ name, browser_download_url: prefix + name })) }))
    : new Response(assets[url.slice(prefix.length)]);
  let quit = 0, prepared = 0, committed = 0, aborted = 0, helpers = 0;
  const deps = {
    app: { getVersion: () => '1.6.1', quit() { quit++; } }, shell: {}, platform: process.platform, arch: process.arch,
    execPath: path.join(appDir, 'fixture.exe'), isPackaged: true, repo: 'o/r',
    publicKey: publicKey.export({ format: 'der', type: 'spki' }).subarray(-32).toString('base64'), userData, onChange() {},
    beforeInstall: async () => { prepared++; }, commitInstall: async () => { committed++; }, installAborted: async () => { aborted++; },
    spawnHelper(_file, _args, options) {
      helpers++;
      assert.equal(options.cwd, root, 'helper must not lock the application working directory');
      assert.equal(options.detached, true);
      const child = new EventEmitter(); child.exitCode = null; child.unref = () => {};
      queueMicrotask(() => { child.emit('spawn'); setImmediate(() => { child.exitCode = 0; child.emit('exit', 0); }); });
      return child;
    },
  };
  try { await run({ root, appDir, userData, deps, privateKey, assets, counts: () => ({ quit, prepared, committed, aborted, helpers }) }); }
  finally { global.fetch = previousFetch; await fs.promises.rm(root, { recursive: true, force: true, maxRetries:20, retryDelay:100 }); }
}

test('checking and downloading an update never prepare, commit or quit a running task', async () => {
  await fixture(async f => {
    const u = createUpdater(f.deps);
    assert.equal((await u.check(false)).phase, 'available');
    assert.equal((await u.download()).phase, 'ready');
    assert.deepEqual(f.counts(), { quit: 0, prepared: 0, committed: 0, aborted: 0, helpers: 0 });
  });
});

test('a busy task refuses installation without starting a helper or quitting', async () => {
  await fixture(async f => {
    f.deps.beforeInstall = async () => { throw new Error('任务尚未结束，请完成后再更新'); };
    const u = createUpdater(f.deps); await u.check(true); await u.download();
    const result = await u.install();
    assert.equal(result.phase, 'error'); assert.match(result.error, /任务尚未结束/);
    assert.deepEqual(f.counts(), { quit: 0, prepared: 0, committed: 0, aborted: 1, helpers: 0 });
    assert.equal(fs.existsSync(path.join(f.root, '.app.update', 'install-permit')), false);
    assert.equal(fs.readFileSync(path.join(f.appDir, 'fixture.exe'), 'utf8'), 'old');
  });
});

test('a spawned helper that never acknowledges readiness cannot shut down the app', async () => {
  await fixture(async f => {
    const u = createUpdater(f.deps); await u.check(true); await u.download();
    const result = await u.install();
    assert.equal(result.phase, 'error'); assert.match(result.error, /更新程序未能启动/);
    assert.deepEqual(f.counts(), { quit: 0, prepared: 1, committed: 0, aborted: 1, helpers: 1 });
    assert.equal(fs.existsSync(path.join(f.root, '.app.update', 'install-permit')), false);
    const saved = JSON.parse(fs.readFileSync(path.join(f.userData, 'update.json')));
    assert.equal(saved.installing, ''); assert.equal(saved.token, '');
    assert.match(fs.readFileSync(path.join(f.userData, 'update.log'), 'utf8'), /installation aborted/);
  });
});

test('installation blocked by an active task retries the staged payload without downloading it again', async () => {
  await fixture(async f => {
    let attempts = 0;
    f.deps.beforeInstall = async () => { attempts++; throw new Error('任务尚未结束，请完成后再更新'); };
    const u = createUpdater(f.deps); await u.check(true); await u.download();
    await u.install(); assert.equal(u.state().retry, 'install');
    u.cleanup(); await u.check(false);
    assert.equal(u.state().retry, 'install', 'periodic checks retain the staged retry');
    await u.install(); assert.equal(attempts, 2);
    assert.equal(u.state().retry, 'install'); assert.equal(f.counts().quit, 0);
  });
});

test('automatic updating waits for active tasks then installs the verified payload once without another click', async () => {
  await fixture(async f => {
    let attempts = 0, downloads = 0; const phases = [];
    const fetchAsset = global.fetch;
    global.fetch = (url, options) => { if (url.endsWith('.tar.gz')) downloads++; return fetchAsset(url, options); };
    f.deps.autoRetryDelay = 10; f.deps.autoInstallDelay = 0;
    f.deps.onChange = state => phases.push(state.phase);
    f.deps.beforeInstall = async () => { if (++attempts < 3) { const error = new Error('任务尚未结束'); error.updateBusy = true; throw error; } };
    f.deps.spawnHelper = () => {
      const saved = JSON.parse(fs.readFileSync(path.join(f.userData, 'update.json')));
      fs.writeFileSync(path.join(f.userData, `update-helper-${saved.token}`), saved.token);
      const child = new EventEmitter(); child.exitCode = null; child.unref = () => {};
      queueMicrotask(() => child.emit('spawn')); return child;
    };
    const u = createUpdater(f.deps); await u.check(true); await u.startAutoUpdate();
    try {
      for (let i = 0; i < 100 && !f.counts().quit; i++) await new Promise(resolve => setTimeout(resolve, 10));
      assert.ok(phases.includes('waiting')); assert.equal(attempts, 3);
      assert.equal(downloads, 1); assert.equal(f.counts().quit, 1); assert.equal(f.counts().committed, 1);
    } finally { u.cancel(); }
  });
});

test('cancelling automatic installation stops the timer, and non-busy errors never retry themselves', async () => {
  await fixture(async f => {
    f.deps.autoInstallDelay = 40; f.deps.autoRetryDelay = 10;
    const u = createUpdater(f.deps); await u.check(true); await u.startAutoUpdate(); u.cancel();
    await new Promise(resolve => setTimeout(resolve, 60));
    assert.equal(f.counts().prepared, 0); assert.equal(f.counts().quit, 0); assert.equal(u.state().autoPaused, true);
    let attempts = 0;
    f.deps.beforeInstall = async () => { attempts++; throw new Error('核心身份不匹配'); };
    await u.startAutoUpdate(); await new Promise(resolve => setTimeout(resolve, 60));
    assert.equal(attempts, 1); assert.equal(u.state().phase, 'error'); assert.equal(u.state().automatic, false);
  });
});

test('legacy unmarked staging is retained by both startup cleanup and a later download', async () => {
  await fixture(async f => {
    const stage = path.join(f.root, '.app.update'); fs.mkdirSync(stage);
    fs.writeFileSync(path.join(stage, 'retain.txt'), 'legacy interrupted download');
    const u = createUpdater(f.deps); u.cleanup();
    assert.equal(fs.readFileSync(path.join(stage, 'retain.txt'), 'utf8'), 'legacy interrupted download');
    await u.check(true); assert.equal((await u.download()).phase, 'ready');
    const retained = fs.readdirSync(f.root).filter(name => name.startsWith('.app.update.saved-'));
    assert.equal(retained.length, 1);
    assert.equal(fs.readFileSync(path.join(f.root, retained[0], 'retain.txt'), 'utf8'), 'legacy interrupted download');
    assert.equal(JSON.parse(fs.readFileSync(path.join(stage, '.salcara-update.json'))).product, 'salcara-desktop-update');
  });
});

test('an expired or rejected final commit revokes the helper permit and keeps the app alive', async () => {
  await fixture(async f => {
    f.deps.spawnHelper = (_file, _args, options) => {
      assert.equal(options.cwd, f.root);
      const child = new EventEmitter(); child.exitCode = null; child.unref = () => {};
      const saved = JSON.parse(fs.readFileSync(path.join(f.userData, 'update.json')));
      queueMicrotask(() => { child.emit('spawn'); fs.writeFileSync(path.join(f.userData, `update-helper-${saved.token}`), saved.token); });
      return child;
    };
    f.deps.commitInstall = async () => { throw new Error('更新准备已取消或过期，请重试'); };
    const u = createUpdater(f.deps); await u.check(true); await u.download();
    const result = await u.install(); assert.equal(result.phase, 'error');
    assert.equal(f.counts().quit, 0); assert.equal(f.counts().aborted, 1);
    assert.equal(fs.existsSync(path.join(f.root, '.app.update', 'install-permit')), false);
    assert.equal(fs.readFileSync(path.join(f.appDir, 'fixture.exe'), 'utf8'), 'old');
  });
});

test('periodic updates check every two hours; an automatic update window does not stop or focus tasks', () => {
  const main = fs.readFileSync(path.join(__dirname, '../main.cjs'), 'utf8');
  assert.match(main, /setInterval\(\(\) => void checkForUpdate\(false\), 2 \* 3600 \* 1000\)/);
  const popup = main.slice(main.indexOf('function openUpdateWindow('), main.indexOf('async function stopForUpdate('));
  assert.match(popup, /if \(quiet\) updateWin\.showInactive\(\)/);
  assert.doesNotMatch(popup, /stopForUpdate|beforeInstall|commitInstall|\.kill\(|app\.quit\(/);
});

test('helper cleanup removes only marked stopped helpers and preserves live or unrelated files', async () => {
  await fixture(async f => {
    const records=[];
    for(const [token,extra,pid] of [['a'.repeat(64),false,0],['b'.repeat(64),true,0],['c'.repeat(64),false,process.pid]]) {
      const directory=path.join(f.userData,'updater-helpers',token);fs.mkdirSync(directory,{recursive:true});
      fs.writeFileSync(path.join(directory,'.salcara-helper.json'),JSON.stringify({product:'salcara-desktop-update-helper',token}));
      fs.writeFileSync(path.join(directory,'runtime.exe'),'synthetic runtime');fs.writeFileSync(path.join(directory,'bootstrap.cjs'),'synthetic helper');
      if(extra)fs.writeFileSync(path.join(directory,'retain.txt'),'unrelated data');
      records.push({token,pid});
    }
    fs.writeFileSync(path.join(f.userData,'update.json'),JSON.stringify({helpers:records}));
    fs.writeFileSync(path.join(f.userData,'retain-config.json'),'user data');
    createUpdater(f.deps).cleanup();
    assert.equal(fs.existsSync(path.join(f.userData,'updater-helpers','a'.repeat(64))),false);
    assert.equal(fs.readFileSync(path.join(f.userData,'updater-helpers','b'.repeat(64),'retain.txt'),'utf8'),'unrelated data');
    assert.equal(fs.existsSync(path.join(f.userData,'updater-helpers','c'.repeat(64),'runtime.exe')),true);
    assert.equal(fs.readFileSync(path.join(f.userData,'retain-config.json'),'utf8'),'user data');
  });
});

async function waitWindowsFixtureHelper(record, executable, { signal, stop = false } = {}) {
  assert.ok(Number.isInteger(record.pid) && record.pid > 0, 'helper has a real process identity');
  const expected = `'${executable.replace(/'/g, "''")}'`;
  // Capture a process handle, not a fixed observation window or a reused PID.
  // The executable lives in this fixture's unique token directory. Never stop
  // an unrelated process if the original helper has already exited.
  // An already-exited process is success. Without an explicit exit code,
  // Windows PowerShell preserves Get-Process's false $? and exits with 1.
  const command = `$ErrorActionPreference='Stop'; $p=Get-Process -Id ${record.pid} -ErrorAction SilentlyContinue; if($p){try{if($p.Path -ne ${expected}){exit 0}; $null=$p.Handle; ${stop ? '$p.Kill();' : ''}$p.WaitForExit()}catch{if(-not $p.HasExited){throw}}}; exit 0`;
  await execFileAsync(path.join(process.env.SystemRoot, 'System32/WindowsPowerShell/v1.0/powershell.exe'),
    ['-NoProfile', '-NonInteractive', '-Command', command], { windowsHide: true, signal, ...(stop ? { timeout: 10000 } : {}) });
}

async function fullWindowsUpdate(t, acknowledge, holdDirectoryMs = 0, holdRollbackMs = 0) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'salcara-full-win-update-'));
  const appDir = path.join(root, 'app'), build = path.join(root, 'build'), userData = path.join(root, 'userdata');
  fs.mkdirSync(appDir); fs.mkdirSync(build); fs.mkdirSync(userData);
  for (const [dir, version] of [[appDir, '1.6.1'], [build, '1.6.2']]) {
    fs.copyFileSync(process.execPath, path.join(dir, 'fixture.exe'));
    fs.writeFileSync(path.join(dir, '.salcara-install.json'), JSON.stringify({ product:'salcara-desktop', version }));
    fs.writeFileSync(path.join(dir, 'VERSION'), version);
  }
  fs.mkdirSync(path.join(appDir,'resources'));
  fs.copyFileSync(process.execPath,path.join(appDir,'resources','SalcaraProbeNode.exe'));
  const archive = path.join(root, 'fixture.tar.gz');
  execFileSync(path.join(process.env.SystemRoot, 'System32', 'tar.exe'), ['--format', 'ustar', '-czf', archive, '-C', build, '.'], {windowsHide:true});
  const updaterPath = path.resolve(__dirname, '../updater.cjs');
  const boot = path.join(root, 'boot.cjs');
  const rollbackHolderExecutable = path.join(root, 'rollback-holder.exe');
  const rollbackHolderRecord = path.join(root, 'rollback-holder.json');
  const rollbackObservation = path.join(root, 'rollback-observation.json');
  if (holdRollbackMs) fs.copyFileSync(process.execPath, rollbackHolderExecutable);
  const holdRollback = `
    const fs=require('node:fs');
    fs.writeFileSync(${JSON.stringify(rollbackHolderRecord)},JSON.stringify({pid:process.pid}));
    process.stdout.write('ready');
    setTimeout(()=>{
      fs.writeFileSync(${JSON.stringify(rollbackObservation)},JSON.stringify({
        version:fs.readFileSync(${JSON.stringify(path.join(appDir, 'VERSION'))},'utf8'),
        backupVersions:fs.readdirSync(${JSON.stringify(root)}).filter(name=>name.startsWith('app.old-')).map(name=>fs.readFileSync(require('node:path').join(${JSON.stringify(root)},name,'VERSION'),'utf8')),
        completed:/updated and healthy|rolled back/.test(fs.readFileSync(${JSON.stringify(path.join(userData, 'update.log'))},'utf8')),
      }));
      process.exit(0);
    },${holdRollbackMs});
  `;
  fs.writeFileSync(boot, `if(require('node:path').basename(process.execPath)==='fixture.exe'){
    const version=require('node:fs').readFileSync(require('node:path').join(require('node:path').dirname(process.execPath),'VERSION'),'utf8');
    if(version==='1.6.2'&&!${JSON.stringify(acknowledge)}){
      if(${holdRollbackMs}){
        const holder=require('node:child_process').spawn(${JSON.stringify(rollbackHolderExecutable)},['-e',${JSON.stringify(holdRollback)}],{cwd:${JSON.stringify(appDir)},detached:true,windowsHide:true,stdio:['ignore','pipe','ignore'],env:{...process.env,NODE_OPTIONS:''}});
        holder.once('error',()=>process.exit(2));holder.stdout.once('data',()=>process.exit(1));
      }else process.exit(1);
    }else{
      const u=require(${JSON.stringify(updaterPath)}).createUpdater({app:{getVersion:()=>version},shell:{},platform:process.platform,arch:process.arch,isPackaged:false,userData:${JSON.stringify(userData)},onChange(){}});u.afterRestart();process.exit(0);
    }
  }`);
  const host = path.join(root, 'host.cjs');
  fs.writeFileSync(host, `
    const fs=require('node:fs'),path=require('node:path'),crypto=require('node:crypto');
    const bytes=fs.readFileSync(${JSON.stringify(archive)}),{publicKey,privateKey}=crypto.generateKeyPairSync('ed25519');
    const name='Salcara-Bridge-1.6.2-win32-x64.tar.gz',prefix='https://github.com/o/r/releases/download/v1.6.2/';
    const manifest=Buffer.from(JSON.stringify({schema:1,product:'salcara-desktop',repo:'o/r',version:'1.6.2',files:{'win32-x64':{name,size:bytes.length,sha256:crypto.createHash('sha256').update(bytes).digest('hex')}}}));
    const assets={'latest.json':manifest,'latest.json.sig':Buffer.from(crypto.sign(null,manifest,privateKey).toString('base64')),[name]:bytes};
    global.fetch=async url=>url.endsWith('/releases/latest')?new Response(JSON.stringify({tag_name:'v1.6.2',assets:Object.keys(assets).map(name=>({name,browser_download_url:prefix+name}))})):new Response(assets[url.slice(prefix.length)]);
    const u=require(${JSON.stringify(updaterPath)}).createUpdater({app:{getVersion:()=> '1.6.1',quit:()=>process.exit(0)},shell:{},platform:process.platform,arch:process.arch,execPath:${JSON.stringify(path.join(appDir,'fixture.exe'))},isPackaged:true,repo:'o/r',publicKey:publicKey.export({format:'der',type:'spki'}).subarray(-32).toString('base64'),userData:${JSON.stringify(userData)},onChange(){},beforeInstall:async()=>{},commitInstall:async()=>{}});
    (async()=>{await u.check(true);await u.download();const result=await u.install();throw new Error(JSON.stringify({phase:result.phase,error:result.error,log:fs.readFileSync(path.join(${JSON.stringify(userData)},'update.log'),'utf8')}));})().catch(error=>{console.error(error.message);process.exit(1);});
  `);
  let hostChild, helperRecord, helperExecutable, helperFinished = false;
  let holder, holderExit, releaseHolder, heldObservation;
  try {
    if (holdDirectoryMs) {
      holder = spawn(process.execPath, ['-e', "process.stdout.write('ready');setInterval(()=>{},1000)"], { cwd: appDir, windowsHide: true, stdio: ['ignore', 'pipe', 'pipe'] });
      holderExit = new Promise(resolve => holder.once('exit', resolve));
      await new Promise((resolve, reject) => { holder.once('error', reject); holder.stdout.once('data', resolve); });
    }
    hostChild = spawn(process.execPath, [host], { cwd:appDir, windowsHide:true, signal:t.signal, env:{...process.env, NODE_OPTIONS:`--require "${boot.replace(/\\/g,'/')}"`}, stdio:['ignore','pipe','pipe'] });
    let errors = ''; hostChild.stdout.resume(); hostChild.stderr.on('data', chunk => { errors += chunk; });
    const code = await new Promise((resolve,reject) => {hostChild.once('error',reject);hostChild.once('exit',resolve);});
    assert.equal(code, 0, errors);
    if (holder) {
      releaseHolder = setTimeout(() => {
        try {
          heldObservation = {
            version: fs.readFileSync(path.join(appDir, 'VERSION'), 'utf8'),
            completed: /updated and healthy|rolled back/.test(fs.readFileSync(path.join(userData, 'update.log'), 'utf8')),
          };
        } catch (error) { heldObservation = { error: error.message }; }
        finally { holder.kill(); }
      }, holdDirectoryMs);
    }
    const saved = JSON.parse(fs.readFileSync(path.join(userData, 'update.json')));
    assert.equal(saved.helpers.length, 1);
    helperRecord = saved.helpers[0];
    assert.match(helperRecord.token, /^[0-9a-f]{64}$/);
    helperExecutable = path.join(userData, 'updater-helpers', helperRecord.token, 'runtime.exe');
    // The helper legitimately retries an occupied install directory for up
    // to 60 seconds. The test's existing 90-second timeout still bounds the
    // entire transaction; assertions run only after the supervisor exits.
    await waitWindowsFixtureHelper(helperRecord, helperExecutable, { signal: t.signal });
    helperFinished = true;
    if (holder) {
      assert.deepEqual(heldObservation, { version: '1.6.1', completed: false });
      await holderExit;
    }
    if (holdRollbackMs) {
      // The new app has exited, but another fixture process still holds its
      // directory. Assert rollback waits rather than abandoning the backup.
      await waitWindowsFixtureHelper(JSON.parse(fs.readFileSync(rollbackHolderRecord)), rollbackHolderExecutable, { signal: t.signal });
      assert.deepEqual(JSON.parse(fs.readFileSync(rollbackObservation)), { version: '1.6.2', backupVersions: ['1.6.1'], completed: false });
      assert.match(fs.readFileSync(path.join(userData, 'update.log'), 'utf8'), /rollback folder busy; waiting/);
    }
    assert.equal(fs.readFileSync(path.join(appDir,'VERSION'),'utf8'), acknowledge?'1.6.2':'1.6.1', fs.readFileSync(path.join(userData,'update.log'),'utf8'));
    assert.match(fs.readFileSync(path.join(userData,'update.log'),'utf8'), acknowledge?/helper acknowledged readiness[\s\S]*updated and healthy/:/helper acknowledged readiness[\s\S]*rolled back/);
    assert.equal(fs.readdirSync(root).some(name => name.startsWith('app.old-')), false);
  } finally {
    clearTimeout(releaseHolder);
    if (holder && holder.exitCode === null) holder.kill();
    if (holderExit) await holderExit;
    if (hostChild && hostChild.exitCode === null) hostChild.kill();
    if (!helperRecord) {
      try {
        const saved = JSON.parse(fs.readFileSync(path.join(userData, 'update.json')));
        const record = saved.helpers?.[0];
        if (/^[0-9a-f]{64}$/.test(record?.token || '') && Number.isInteger(record.pid) && record.pid > 0) {
          helperRecord = record;
          helperExecutable = path.join(userData, 'updater-helpers', record.token, 'runtime.exe');
        }
      } catch { /* failed before a helper was published */ }
    }
    if (!helperFinished && helperRecord && helperExecutable) {
      // A timed-out test must not delete staging underneath a live updater.
      // Terminate only the supervisor from this exact fixture before cleanup.
      await waitWindowsFixtureHelper(helperRecord, helperExecutable, { stop: true });
    }
    if (fs.existsSync(rollbackHolderRecord)) {
      await waitWindowsFixtureHelper(JSON.parse(fs.readFileSync(rollbackHolderRecord)), rollbackHolderExecutable, { stop: true });
    }
    // Only synthetic processes/files in this mkdtemp fixture are cleaned up.
    await fs.promises.rm(root,{recursive:true,force:true,maxRetries:30,retryDelay:100});
  }
}

test('a failed target is quarantined across app restarts and fresh checks until an explicit retry', async () => {
  await fixture(async f => {
    const u = createUpdater(f.deps); await u.check(true);
    const file = path.join(f.userData, 'update.json'), saved = JSON.parse(fs.readFileSync(file));
    fs.writeFileSync(file, JSON.stringify({ ...saved, installing: '1.6.2' }));
    assert.deepEqual(u.afterRestart(), { version: '1.6.2', ok: false });
    const restored = createUpdater(f.deps); await restored.check(false);
    assert.equal(restored.state().autoBlocked, true);
    await restored.startAutoUpdate({ manual: false });
    assert.equal(restored.state().phase, 'available'); assert.equal(f.counts().prepared, 0);
    f.deps.autoInstallDelay = 60000;
    await restored.startAutoUpdate({ manual: true });
    assert.equal(restored.state().phase, 'ready'); assert.equal(restored.state().autoBlocked, false);
    restored.cancel();
  });
});

test('restart keeps the verified archive but re-extracts staging before installation', async () => {
  await fixture(async f => {
    const u = createUpdater(f.deps); await u.check(false); await u.download();
    const staged = path.join(f.root, '.app.update', 'app', 'fixture.exe');
    fs.writeFileSync(staged, 'changed while closed');
    const restored = createUpdater(f.deps); restored.cleanup();
    global.fetch = () => { throw new Error('verified cache must work offline'); };
    assert.equal((await restored.download()).phase, 'ready');
    assert.equal(fs.readFileSync(staged, 'utf8'), 'new');
  });
});

test('disk preflight fails before any archive download or core shutdown', async () => {
  await fixture(async f => {
    f.deps.statfs = () => ({ bavail: 0, bsize: 4096 });
    const u = createUpdater(f.deps); await u.check(false);
    let downloads = 0; const fetch = global.fetch;
    global.fetch = (...args) => { downloads++; return fetch(...args); };
    const result = await u.download();
    assert.equal(result.phase, 'error'); assert.match(result.error, /磁盘空间不足/);
    assert.equal(downloads, 0); assert.equal(f.counts().prepared, 0);
  });
});

test('a withdrawn staged release cannot start a helper even via explicit retry', async () => {
  await fixture(async f => {
    const u = createUpdater(f.deps); await u.check(false); await u.download();
    f.deps.controlURL = 'https://example.test/control.json';
    const body = Buffer.from(JSON.stringify({ schema: 1, product: 'salcara-desktop-control', repo: 'o/r', revision: 1, issuedAt: Date.now(), expiresAt: Date.now() + 60000, paused: false, blockedVersions: ['1.6.2'], reason: '此版本已撤回' }));
    global.fetch = async url => new Response(url.endsWith('.sig') ? crypto.sign(null, body, f.privateKey).toString('base64') : body);
    const result = await u.startAutoUpdate({ manual: true });
    assert.equal(result.automatic, false); assert.match(result.distributionReason, /撤回/);
    assert.equal(f.counts().prepared, 0); assert.equal(f.counts().helpers, 0);
  });
});

test('Windows full updater survives an app-directory cwd, verifies the archive and relaunches', { skip:process.platform !== 'win32', timeout:90000 }, t => fullWindowsUpdate(t, true));
test('Windows full updater restores the old payload when the new version fails startup', { skip:process.platform !== 'win32', timeout:90000 }, t => fullWindowsUpdate(t, false));
test('Windows full updater waits for directory contention before verifying rollback', { skip:process.platform !== 'win32', timeout:90000 }, t => fullWindowsUpdate(t, false, 22000));
test('Windows full updater retries rollback while the failed new app directory remains occupied', { skip:process.platform !== 'win32', timeout:90000 }, t => fullWindowsUpdate(t, false, 0, 3000));
