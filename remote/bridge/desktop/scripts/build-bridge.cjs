const { execFileSync } = require('node:child_process');
const fs = require('node:fs');
const path = require('node:path');

const desktopRoot = path.resolve(__dirname, '..');
const bridgeRoot = path.resolve(desktopRoot, '..');
const platform = process.argv[2] || process.platform;
const arch = process.argv[3] || process.arch;
const goos = { win32: 'windows', darwin: 'darwin', linux: 'linux' }[platform];
const goarch = { x64: 'amd64', arm64: 'arm64' }[arch];
if (!goos || !goarch) throw new Error(`Unsupported desktop target: ${platform}/${arch}`);

const output = path.join(desktopRoot, 'bin', platform === 'win32' ? 'SalcaraBridge.exe' : 'SalcaraBridge');
fs.mkdirSync(path.dirname(output), { recursive: true });
const args = ['build', '-trimpath', '-ldflags', `-s -w -X main.version=${require('../package.json').version}${platform === 'win32' ? ' -H=windowsgui' : ''}`, '-o', output, '.'];
execFileSync(process.env.GO_BIN || 'go', args, {
  cwd: bridgeRoot,
  env: { ...process.env, GOOS: goos, GOARCH: goarch, CGO_ENABLED: '0' },
  stdio: 'inherit',
});
console.log(output);
