// Turn a packaged build into an auto-update release for this platform.
//   npm run package
//   npm run release -- [--notes notes.md]
// Writes to out/release/:
//   Salcara-Bridge-<version>-<platform>-<arch>.tar.gz
//   latest.json       (merged with entries already there for the same version, e.g. from another OS)
//   latest.json.sig   (Ed25519 signature of latest.json with the private key from release:keygen)
// Upload all three to the GitHub release tagged v<version> of salcaraUpdateRepo.
const crypto = require('node:crypto');
const { execFileSync } = require('node:child_process');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { verifyManifest } = require('../updater.cjs');

const root = path.resolve(__dirname, '..');
const pkg = require('../package.json');
const version = pkg.version;
if (!/^\d+\.\d+\.\d+$/.test(version)) { console.error(`测试版（${version}）不发布自动更新。`); process.exit(1); }
if (!pkg.salcaraUpdateRepo) { console.error('package.json 里还没有填写 salcaraUpdateRepo（GitHub 仓库，例如 owner/repo）。'); process.exit(1); }
if (!pkg.salcaraUpdatePublicKey) { console.error('还没有签名公钥，请先运行 npm run release:keygen。'); process.exit(1); }
const keyFile = process.env.SALCARA_UPDATE_KEY || path.join(os.homedir(), '.salcara', 'update-signing-key.pem');
if (!fs.existsSync(keyFile)) { console.error(`找不到签名私钥：${keyFile}`); process.exit(1); }

const platform = process.platform, arch = process.arch;
const appName = 'Salcara Bridge';
const outRoot = process.env.SALCARA_DESKTOP_OUT || path.join(root, 'out');
const built = path.join(outRoot, `${appName}-${platform}-${arch}`);
if (!fs.existsSync(built)) { console.error(`找不到打包结果：${built}\n请先运行 npm run package。`); process.exit(1); }

let notes = '';
const notesArg = process.argv.indexOf('--notes');
if (notesArg > 0 && process.argv[notesArg + 1]) notes = fs.readFileSync(path.resolve(process.argv[notesArg + 1]), 'utf8').trim();

const releaseDir = path.join(outRoot, 'release');
fs.mkdirSync(releaseDir, { recursive: true });
const name = `Salcara-Bridge-${version}-${platform}-${arch}.tar.gz`;
const archive = path.join(releaseDir, name);
fs.rmSync(archive, { force: true });
const tar = platform === 'win32' ? path.join(process.env.SystemRoot || 'C:\\Windows', 'System32', 'tar.exe') : 'tar';
// macOS: the archive holds the .app; elsewhere: the contents of the app folder.
const contents = platform === 'darwin' ? [`${appName}.app`] : ['.'];
execFileSync(tar, ['-czf', archive, '-C', built, ...contents], { stdio: 'inherit' });
const bytes = fs.readFileSync(archive);
const file = { name, sha256: crypto.createHash('sha256').update(bytes).digest('hex'), size: bytes.length };

const manifestFile = path.join(releaseDir, 'latest.json');
let manifest = { version, notes, date: new Date().toISOString(), files: {} };
try {
  const old = JSON.parse(fs.readFileSync(manifestFile, 'utf8'));
  if (old && old.version === version && old.files && typeof old.files === 'object') manifest = { ...old, notes: notes || old.notes || '', files: { ...old.files } };
} catch { /* first platform */ }
manifest.files[`${platform}-${arch}`] = file;
const body = Buffer.from(JSON.stringify(manifest, null, 2) + '\n');
const signature = crypto.sign(null, body, crypto.createPrivateKey(fs.readFileSync(keyFile))).toString('base64');
// Check with the public key the app ships, exactly as the app will.
verifyManifest({ manifest: body, signature, publicKey: pkg.salcaraUpdatePublicKey, current: '0.0.0', platform, arch, tag: `v${version}` });
fs.writeFileSync(manifestFile, body);
fs.writeFileSync(manifestFile + '.sig', signature + '\n');

console.log(`
已生成（${(file.size / 1048576).toFixed(1)} MB）：
  ${archive}
  ${manifestFile}
  ${manifestFile}.sig

在 GitHub 仓库 ${pkg.salcaraUpdateRepo} 新建发布 v${version}（不要勾选 pre-release），上传这三个文件。
要同时支持其他系统：把 latest.json 复制到那台电脑的 out/release/ 里，在那里打包后再运行一次 npm run release，然后上传新的 latest.json、latest.json.sig 和那边的安装包。`);
