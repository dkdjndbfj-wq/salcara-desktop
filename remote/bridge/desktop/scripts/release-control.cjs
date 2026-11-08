'use strict';
// Sign a short-lived release-control document separately from immutable builds.
// Input is a reviewed JSON file; this command never uploads or overwrites a release.
const fs = require('node:fs');
const path = require('node:path');
const crypto = require('node:crypto');
const { verifyControl } = require('../update-control.cjs');
const { saveJSON } = require('../update-download.cjs');

function signControl(input, privateKey, publicKey, repo, previousRevision = 0) {
  const body = Buffer.from(JSON.stringify({ ...input, schema: 1, product: 'salcara-desktop-control', repo }, null, 2) + '\n');
  const signature = crypto.sign(null, body, privateKey).toString('base64');
  verifyControl({ body, signature, publicKey, repo, previousRevision });
  return { body, signature };
}
if (require.main === module) {
  try {
    const [input, output] = process.argv.slice(2), pkg = require('../package.json');
    if (!input || !output || !process.env.SALCARA_UPDATE_KEY) throw new Error('Usage: SALCARA_UPDATE_KEY=<key> node scripts/release-control.cjs reviewed-control.json new-output-directory');
    if (fs.existsSync(output)) throw new Error('Use a new output directory for each control revision');
    const result = signControl(JSON.parse(fs.readFileSync(input, 'utf8')), crypto.createPrivateKey(fs.readFileSync(process.env.SALCARA_UPDATE_KEY)), pkg.salcaraUpdatePublicKey, pkg.salcaraUpdateRepo);
    fs.mkdirSync(output, { recursive: true });
    fs.writeFileSync(path.join(output, 'control.json'), result.body, { flag: 'wx' });
    fs.writeFileSync(path.join(output, 'control.json.sig'), result.signature + '\n', { flag: 'wx' });
    saveJSON(path.join(output, 'SHA256.json'), { 'control.json': crypto.createHash('sha256').update(result.body).digest('hex'), 'control.json.sig': crypto.createHash('sha256').update(result.signature + '\n').digest('hex') });
    console.log('Verified release control written. Publish both files atomically, with Cache-Control: no-store.');
  } catch (error) { console.error(error.message); process.exitCode = 1; }
}
module.exports = { signControl };
