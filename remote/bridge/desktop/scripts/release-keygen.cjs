// One-time: create the Ed25519 key that signs auto-updates.
//   npm run release:keygen
// The private key is written OUTSIDE the project (default ~/.salcara/update-signing-key.pem,
// or SALCARA_UPDATE_KEY); the public key goes into package.json (salcaraUpdatePublicKey), so every
// build made after this only installs updates signed with this key.
const crypto = require('node:crypto');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');

const keyFile = process.env.SALCARA_UPDATE_KEY || path.join(os.homedir(), '.salcara', 'update-signing-key.pem');
const pkgFile = path.resolve(__dirname, '..', 'package.json');

if (fs.existsSync(keyFile)) {
  console.error(`已经有签名私钥：${keyFile}\n不会覆盖它。换钥匙会让已经安装的旧版本无法自动更新。`);
  process.exit(1);
}
const { publicKey, privateKey } = crypto.generateKeyPairSync('ed25519');
fs.mkdirSync(path.dirname(keyFile), { recursive: true });
fs.writeFileSync(keyFile, privateKey.export({ type: 'pkcs8', format: 'pem' }), { mode: 0o600, flag: 'wx' });
const raw = publicKey.export({ type: 'spki', format: 'der' }).subarray(-32).toString('base64');
const pkg = JSON.parse(fs.readFileSync(pkgFile, 'utf8'));
pkg.salcaraUpdatePublicKey = raw;
fs.writeFileSync(pkgFile, JSON.stringify(pkg, null, 2) + '\n');
console.log(`签名私钥：${keyFile}
公钥已写入 package.json：${raw}

请务必备份私钥（比如存进密码管理器），并且不要提交到 Git。
私钥丢了，已经装了这个公钥的用户就只能手动下载新版。`);
