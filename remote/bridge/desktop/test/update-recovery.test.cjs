'use strict';
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs'), os = require('node:os'), path = require('node:path');
const { spawnSync } = require('node:child_process');
const { swapScript } = require('../updater.cjs');

test('Windows recovery restores the exact old directory after interruption between the two renames', { skip: process.platform !== 'win32', timeout: 30000 }, async () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'salcara-powerloss-'));
  const appDir = path.join(root, 'app'), backupDir = appDir + '.old-123', newDir = path.join(root, '.app.update', 'app');
  fs.mkdirSync(backupDir); fs.mkdirSync(newDir, { recursive: true });
  fs.copyFileSync(process.execPath, path.join(backupDir, 'fixture.exe'));
  fs.writeFileSync(path.join(backupDir, 'VERSION'), 'old'); fs.writeFileSync(path.join(newDir, 'VERSION'), 'new');
  const permit = path.join(root, '.app.update', 'permit'), receipt = path.join(root, 'receipt'), log = path.join(root, 'update.log'), token = 'test-only-token';
  fs.writeFileSync(permit, token);
  const boot = path.join(root, 'exit.cjs'); fs.writeFileSync(boot, 'process.exit(0)');
  const script = path.join(root, 'recovery.ps1');
  fs.writeFileSync(script, '\ufeff' + swapScript({ platform: 'win32', pid: 2147483000, appDir, backupDir, newDir, launch: path.join(appDir, 'fixture.exe'), log, permit, receipt, token }));
  const ps = path.join(process.env.SystemRoot, 'System32/WindowsPowerShell/v1.0/powershell.exe');
  try {
    const result = spawnSync(ps, ['-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass', '-File', script],
      { windowsHide: true, timeout: 20000, env: { ...process.env, NODE_OPTIONS: `--require "${boot.replace(/\\/g, '/')}"` } });
    assert.equal(result.status, 1, result.stderr?.toString());
    assert.equal(fs.readFileSync(path.join(appDir, 'VERSION'), 'utf8'), 'old');
    assert.equal(fs.readFileSync(path.join(newDir, 'VERSION'), 'utf8'), 'new');
    assert.match(fs.readFileSync(log, 'utf8'), /rolled back interrupted swap/);
  } finally { await new Promise(resolve => setTimeout(resolve, 1000)); await fs.promises.rm(root, { recursive: true, force: true, maxRetries: 30, retryDelay: 100 }); }
});

test('Windows recovery completes a previously acknowledged swap without launching or changing the installed payload', { skip: process.platform !== 'win32', timeout: 30000 }, () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'salcara-powerloss-'));
  const appDir = path.join(root, 'app'), backupDir = appDir + '.old-123', newDir = path.join(root, '.app.update', 'app');
  fs.mkdirSync(appDir); fs.mkdirSync(backupDir); fs.mkdirSync(path.dirname(newDir));
  fs.writeFileSync(path.join(appDir, 'VERSION'), 'new'); fs.writeFileSync(path.join(backupDir, 'VERSION'), 'old');
  const token = 'test-token', permit = path.join(root, '.app.update/permit'), receipt = path.join(root, 'receipt'), log = path.join(root, 'update.log');
  fs.writeFileSync(permit, token); fs.writeFileSync(receipt, token);
  const script = path.join(root, 'recovery.ps1'); fs.writeFileSync(script, '\ufeff' + swapScript({ platform: 'win32', pid: 2147483000, appDir, backupDir, newDir, launch: path.join(appDir, 'absent.exe'), log, permit, receipt, token }));
  try {
    const result = spawnSync(path.join(process.env.SystemRoot, 'System32/WindowsPowerShell/v1.0/powershell.exe'), ['-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass', '-File', script], { windowsHide: true, timeout: 20000 });
    assert.equal(result.status, 0, result.stderr?.toString()); assert.equal(fs.existsSync(backupDir), false);
    assert.equal(fs.readFileSync(path.join(appDir, 'VERSION'), 'utf8'), 'new');
  } finally { fs.rmSync(root, { recursive: true, force: true }); }
});
