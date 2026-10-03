const { execFileSync } = require('node:child_process');
const path = require('node:path');
const fs = require('node:fs');
const { packager } = require('@electron/packager');

async function nodeLicenseText(root) {
  const cache = path.join(root, 'bin', `NODE-LICENSE-${process.version}.txt`);
  if (fs.existsSync(cache)) return fs.readFileSync(cache, 'utf8');
  const url = `https://raw.githubusercontent.com/nodejs/node/${process.version}/LICENSE`;
  let text;
  try {
    const response = await fetch(url, { signal: AbortSignal.timeout(30000) });
    if (!response.ok) throw new Error('Node runtime license download failed');
    text = await response.text();
  } catch (error) {
    if (process.platform !== 'win32') throw error;
    // Node fetch may not use the system proxy configured on a Windows builder.
    // Use Windows' system downloader only for this fixed official license URL.
    const powershell = path.join(process.env.SystemRoot || 'C:\\Windows', 'System32', 'WindowsPowerShell', 'v1.0', 'powershell.exe');
    text = execFileSync(powershell, ['-NoProfile', '-NonInteractive', '-Command',
      `[Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false); [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12; $ErrorActionPreference = 'Stop'; $download = New-Object Net.WebClient; [Console]::Write($download.DownloadString('${url}')); $download.Dispose()`],
      { encoding: 'utf8', timeout: 30000, windowsHide: true, maxBuffer: 1024 * 1024 });
  }
  if (!text.includes('Node.js is licensed') || !text.includes('Permission is hereby granted')) throw new Error('Invalid Node runtime license response');
  fs.writeFileSync(cache, text, 'utf8');
  return text;
}

async function main() {
  const platform = process.platform;
  const arch = process.arch;
  execFileSync(process.execPath, [path.join(__dirname, 'build-bridge.cjs'), platform, arch], { stdio: 'inherit' });
  execFileSync(process.execPath, [path.join(__dirname, 'collect-licenses.cjs')], { stdio: 'inherit' });
  const root = path.resolve(__dirname, '..');
  const testBuild = require('../package.json').version.includes('-test.');
  const appName = testBuild ? 'Salcara Bridge Test' : 'Salcara Bridge';
  const executable = path.join(root, 'bin', platform === 'win32' ? 'SalcaraBridge.exe' : 'SalcaraBridge');
  const logo = path.resolve(root, '../../../assets/brand-logo.png');
  // Ship a standalone stdio runtime; installing the probe does not require the user to install Node.
  // Packaging currently targets only this builder's native platform/architecture.
  const probeNode = path.join(root, 'bin', platform === 'win32' ? 'SalcaraProbeNode.exe' : 'SalcaraProbeNode');
  fs.copyFileSync(process.execPath, probeNode);
  if (platform !== 'win32') fs.chmodSync(probeNode, 0o755);
  const nodeLicense = path.join(root, 'bin', 'NODE-LICENSE.txt');
  const licenseText = await nodeLicenseText(root);
  if (!licenseText.includes('Node.js is licensed') || !licenseText.includes('Permission is hereby granted')) throw new Error('Invalid Node runtime license response');
  fs.writeFileSync(nodeLicense, licenseText, 'utf8');
  const output = await packager({
    dir: root,
    name: appName,
    executableName: appName,
    // The orb: .ico on Windows, .icns on macOS (packager picks by platform).
    icon: path.join(root, 'icons', 'app'),
    appBundleId: testBuild ? 'top.salcara.bridge.test' : 'top.salcara.bridge',
    platform,
    arch,
    out: process.env.SALCARA_DESKTOP_OUT || path.join(root, 'out'),
    overwrite: true,
    asar: true,
    extraResource: [...['app.ico', 'app.png', 'tray.ico', 'tray.png', 'tray@2x.png'].map((name) => path.join(root, 'icons', name)), executable, probeNode, nodeLicense, path.resolve(root, '../../desktop-companion'), logo, path.resolve(root, '../../../LICENSE'), path.resolve(root, '../README.md'), path.resolve(root, '../docs'), path.resolve(root, '../THIRD-PARTY-NOTICES.md'), path.join(root,'bin/GO-THIRD-PARTY-NOTICES.txt')],
    electronZipDir: process.env.SALCARA_ELECTRON_ZIP_DIR,
    ignore: [/[/\\]bin(?:[/\\]|$)/, /[/\\]out(?:[/\\]|$)/, /[/\\]download-cache(?:[/\\]|$)/],
  });
  for (const directory of output) {
    const target = platform === 'darwin' ? path.join(directory, `${appName}.app`) : directory;
    fs.writeFileSync(path.join(target, '.salcara-install.json'), JSON.stringify({ product: 'salcara-desktop', version: require('../package.json').version }) + '\n');
    console.log(`Packaged: ${directory}`);
  }
}

main().catch((error) => {
  console.error(error);
  process.exitCode = 1;
});
