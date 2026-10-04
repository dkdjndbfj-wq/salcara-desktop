'use strict';
// Compile an installer around the same payload used by signed tar updates.
// This command neither launches the desktop app nor reads a signing identity.
const fs = require('node:fs');
const path = require('node:path');
const { execFileSync } = require('node:child_process');

const REQUIRED_FILES = ['Salcara Bridge.exe', 'resources/app.asar', 'resources/SalcaraBridge.exe',
  'resources/SalcaraProbeNode.exe', 'resources/desktop-companion/src/stdio.mjs',
  'resources/NODE-LICENSE.txt', 'resources/GO-THIRD-PARTY-NOTICES.txt'];

function inspectPayload(directory, version) {
  if (!/^\d+\.\d+\.\d+$/.test(version)) throw new Error('Only numbered production versions have installers');
  const root = path.resolve(directory);
  let ancestor = root;
  for (;;) {
    const entry = fs.lstatSync(ancestor);
    if (!entry.isDirectory() || entry.isSymbolicLink()) throw new Error('Payload ancestors must be real directories');
    const parent = path.dirname(ancestor);
    if (parent === ancestor) break;
    ancestor = parent;
  }
  function walk(at) {
    for (const name of fs.readdirSync(at)) {
      const file = path.join(at, name), entry = fs.lstatSync(file);
      if (entry.isSymbolicLink() || (!entry.isFile() && !entry.isDirectory())) throw new Error(`Linked or special payload file: ${file}`);
      if (entry.isDirectory()) walk(file);
    }
  }
  walk(root);
  const marker = fs.readFileSync(path.join(root, '.salcara-install.json'), 'utf8').trim();
  if (marker !== JSON.stringify({ product: 'salcara-desktop', version })) throw new Error('Payload marker does not match installer version');
  for (const name of REQUIRED_FILES) if (!fs.statSync(path.join(root, name)).isFile()) throw new Error(`Incomplete payload: ${name}`);
  return root;
}

function compilerPath(env = process.env) {
  const candidates = [env.SALCARA_ISCC, env['ProgramFiles(x86)'] && path.join(env['ProgramFiles(x86)'], 'Inno Setup 6', 'ISCC.exe'),
    env.ProgramFiles && path.join(env.ProgramFiles, 'Inno Setup 6', 'ISCC.exe')].filter(Boolean);
  return candidates.find(file => fs.existsSync(file)) || 'ISCC.exe';
}

function buildInstaller({ version, payloadDirectory, releaseDirectory, compiler = compilerPath(), run = execFileSync }) {
  const payload = inspectPayload(payloadDirectory, version);
  const release = path.resolve(releaseDirectory);
  fs.mkdirSync(release, { recursive: true });
  const script = path.resolve(__dirname, '../installer/windows.iss');
  run(compiler, [`/DAppVersion=${version}`, `/DPayloadDir=${payload}`, `/DReleaseDir=${release}`, script],
    { cwd: path.dirname(script), stdio: 'inherit', windowsHide: true });
  const result = path.join(release, `Salcara-Desktop-${version}-win32-x64-setup.exe`);
  if (!fs.statSync(result).isFile() || fs.statSync(result).size === 0) throw new Error('Installer compiler did not produce a nonempty setup EXE');
  return result;
}

if (require.main === module) {
  try {
    if (process.platform !== 'win32' || process.arch !== 'x64') throw new Error('Build the Windows x64 installer on a Windows x64 runner');
    const root = path.resolve(__dirname, '..'), pkg = require('../package.json');
    const output = process.env.SALCARA_DESKTOP_OUT || path.join(root, 'out');
    console.log(`Installer: ${buildInstaller({ version: pkg.version,
      payloadDirectory: path.join(output, 'Salcara Bridge-win32-x64'), releaseDirectory: path.join(output, 'release') })}`);
  } catch (error) {
    console.error(error.message); process.exitCode = 1;
  }
}

module.exports = { buildInstaller, compilerPath, inspectPayload, REQUIRED_FILES };
