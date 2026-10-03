// Build artifact generator: preserve notices from the actual compiled modules.
const { execFileSync } = require('node:child_process');
const fs = require('node:fs');
const path = require('node:path');
const root = path.resolve(__dirname, '../..');
const go = process.env.GO_BIN || 'go';
const run = args => execFileSync(go, args, { cwd: root, encoding: 'utf8', maxBuffer: 10*1024*1024 });
const moduleLines = run(['list','-deps','-f','{{if .Module}}{{.Module.Path}}|{{.Module.Version}}|{{.Module.Dir}}{{end}}','.']).trim().split(/\r?\n/).filter(Boolean);
const modules = [...new Set(moduleLines)].map(line=>line.split('|'));
let output = fs.readFileSync(path.join(root, 'THIRD-PARTY-NOTICES.md'), 'utf8');
for (const [name, version, directory] of modules) {
  if (name === 'salcara/bridge' || !directory) continue;
  const names = fs.readdirSync(directory).filter(file => /^(LICENSE|COPYING|NOTICE)([._-].*)?$/i.test(file) && fs.statSync(path.join(directory,file)).isFile());
  if (!names.length) throw new Error(`No license found for compiled module ${name}`);
  output += `\n\n# ${name} ${version}\n`;
  for (const file of names.sort()) output += `\n## ${file}\n\n${fs.readFileSync(path.join(directory,file),'utf8')}\n`;
}
const target = path.join(root, 'desktop/bin/GO-THIRD-PARTY-NOTICES.txt');
fs.mkdirSync(path.dirname(target), {recursive:true});
fs.writeFileSync(target, output);
console.log(`License bundle generated (${modules.length - 1} third-party compiled modules)`);
