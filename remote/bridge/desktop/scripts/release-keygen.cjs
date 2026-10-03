// One-time: create the Ed25519 key that signs auto-updates.
//   npm run release:keygen
// The private key is written OUTSIDE the project (default ~/.salcara/update-signing-key.pem,
// or SALCARA_UPDATE_KEY); the public key goes into package.json (salcaraUpdatePublicKey), so every
// build made after this only installs updates signed with this key.
const crypto = require('node:crypto');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { execFileSync } = require('node:child_process');

const keyFile = process.env.SALCARA_UPDATE_KEY || path.join(os.homedir(), '.salcara', 'signing', 'desktop', 'update-signing-key.pem');
const pkgFile = path.resolve(__dirname, '..', 'package.json');
const pkg = JSON.parse(fs.readFileSync(pkgFile, 'utf8'));
if (pkg.salcaraUpdatePublicKey) { console.error('已经配置发布公钥，不会替换签名身份。'); process.exit(1); }
if (path.resolve(keyFile).startsWith(path.resolve(__dirname, '../../../..') + path.sep)) { console.error('签名私钥必须保存在仓库外。'); process.exit(1); }

if (fs.existsSync(keyFile)) {
  console.error(`已经有签名私钥：${keyFile}\n不会覆盖它。换钥匙会让已经安装的旧版本无法自动更新。`);
  process.exit(1);
}
const { publicKey, privateKey } = crypto.generateKeyPairSync('ed25519');
fs.mkdirSync(path.dirname(keyFile), { recursive: true });
if (process.platform === 'win32') {
  const ps = path.join(process.env.SystemRoot || 'C:\\Windows', 'System32', 'WindowsPowerShell', 'v1.0', 'powershell.exe');
  // Apply the protected ACL before writing the identity. Do not inherit broad
  // directory grants; no private key bytes are printed or placed in a command.
  const quote = (s) => "'" + s.replace(/'/g, "''") + "'";
  const script = `$id=[Security.Principal.WindowsIdentity]::GetCurrent(); $acl=New-Object Security.AccessControl.DirectorySecurity; $acl.SetAccessRuleProtection($true,$false); $rule=New-Object Security.AccessControl.FileSystemAccessRule($id.User,'FullControl','ContainerInherit,ObjectInherit','None','Allow'); $acl.AddAccessRule($rule); [IO.Directory]::SetAccessControl(${quote(path.dirname(keyFile))},$acl)`;
  execFileSync(ps, ['-NoProfile', '-NonInteractive', '-Command', script], { windowsHide: true, stdio: 'pipe' });
}
fs.writeFileSync(keyFile, privateKey.export({ type: 'pkcs8', format: 'pem' }), { mode: 0o600, flag: 'wx' });
const raw = publicKey.export({ type: 'spki', format: 'der' }).subarray(-32).toString('base64');
pkg.salcaraUpdatePublicKey = raw;
fs.writeFileSync(pkgFile, JSON.stringify(pkg, null, 2) + '\n');
console.log(`签名身份已创建，私钥安全保存在仓库外。
公钥已写入 package.json。

请务必备份私钥（比如存进密码管理器），并且不要提交到 Git。
私钥丢了，已经装了这个公钥的用户就只能手动下载新版。`);
