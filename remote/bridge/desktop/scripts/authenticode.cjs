'use strict';
const fs = require('node:fs');
const path = require('node:path');
const { execFileSync } = require('node:child_process');

function signingOptions(env = process.env) {
  const thumbprint = env.SALCARA_AUTHENTICODE_THUMBPRINT || '';
  if (!/^[a-fA-F0-9]{40}$/.test(thumbprint)) throw new Error('A trusted Windows code-signing certificate thumbprint is required');
  const timestamp = env.SALCARA_AUTHENTICODE_TIMESTAMP || 'https://timestamp.digicert.com';
  const u = new URL(timestamp);
  if (u.protocol !== 'https:' || u.username || u.password || u.hash) throw new Error('HTTPS timestamp service required');
  const executable = env.SALCARA_SIGNTOOL;
  if (!executable || !path.isAbsolute(executable) || !fs.statSync(executable).isFile()) throw new Error('SALCARA_SIGNTOOL must point to Windows SDK signtool.exe');
  return { thumbprint: thumbprint.toUpperCase(), timestamp, executable };
}
function verify(file, options, run = execFileSync) {
  const stat = fs.lstatSync(file);
  if (!stat.isFile() || stat.isSymbolicLink()) throw new Error('Only regular artifacts can be verified');
  run(options.executable, ['verify', '/pa', '/all', '/tw', file], { stdio: 'inherit', windowsHide: true, timeout: 120000 });
  const ps = path.join(process.env.SystemRoot || 'C:\\Windows', 'System32/WindowsPowerShell/v1.0/powershell.exe');
  // The path goes through an environment value, never PowerShell source.
  const output = run(ps, ['-NoProfile', '-NonInteractive', '-Command',
    "$s=Get-AuthenticodeSignature -LiteralPath $env:SALCARA_SIGNATURE_FILE; if ($s.Status -ne 'Valid' -or -not $s.TimeStamperCertificate) { throw 'Trusted timestamped signature required' }; [Console]::Write($s.SignerCertificate.Thumbprint)"],
    { encoding: 'utf8', windowsHide: true, timeout: 120000, env: { ...process.env, SALCARA_SIGNATURE_FILE: path.resolve(file) } });
  if (String(output).trim().toUpperCase() !== options.thumbprint) throw new Error('Artifact signer does not match the release identity');
}
function sign(file, options = signingOptions(), run = execFileSync) {
  run(options.executable, ['sign', '/sha1', options.thumbprint, '/s', 'My', '/fd', 'SHA256', '/tr', options.timestamp, '/td', 'SHA256', file],
    { stdio: 'inherit', windowsHide: true, timeout: 120000 });
  verify(file, options, run);
}
function ownedFiles(root) {
  return [path.join(root, 'Salcara Bridge.exe'), path.join(root, 'resources/SalcaraBridge.exe')];
}
if (require.main === module) {
  try {
    if (process.platform !== 'win32') throw new Error('Windows signing must run on Windows');
    const [action, target] = process.argv.slice(2);
    if (!['sign', 'verify'].includes(action) || !target) throw new Error('Usage: authenticode.cjs sign|verify <payload-directory|setup.exe>');
    const options = signingOptions(), root = path.resolve(target);
    for (const file of fs.statSync(root).isDirectory() ? ownedFiles(root) : [root]) (action === 'sign' ? sign : verify)(file, options);
  } catch (error) { console.error(error.message); process.exitCode = 1; }
}
module.exports = { sign, verify, ownedFiles, signingOptions };
