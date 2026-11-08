'use strict';
const crypto = require('node:crypto');
const VERSION = /^\d+\.\d+\.\d+$/;
const PREFIX = Buffer.from('302a300506032b6570032100', 'hex');

function verifyControl({ body, signature, publicKey, repo, previousRevision = 0, now = Date.now() }) {
  const raw = Buffer.from(publicKey || '', 'base64'), sig = Buffer.from(String(signature).trim(), 'base64');
  if (raw.length !== 32 || sig.length !== 64 || Buffer.byteLength(body) > 65536) throw new Error('发布控制签名无效');
  const key = crypto.createPublicKey({ key: Buffer.concat([PREFIX, raw]), format: 'der', type: 'spki' });
  if (!crypto.verify(null, Buffer.from(body), key, sig)) throw new Error('发布控制签名无效');
  const data = JSON.parse(body);
  if (data.schema !== 1 || data.product !== 'salcara-desktop-control' || data.repo !== repo ||
      !Number.isSafeInteger(data.revision) || data.revision < 1 || data.revision < previousRevision ||
      !Number.isSafeInteger(data.issuedAt) || !Number.isSafeInteger(data.expiresAt) || data.issuedAt > now + 300000 ||
      data.expiresAt <= now || data.expiresAt <= data.issuedAt || data.expiresAt - data.issuedAt > 7 * 86400000 ||
      typeof data.paused !== 'boolean' || !Array.isArray(data.blockedVersions) || data.blockedVersions.length > 100 ||
      data.blockedVersions.some(v => !VERSION.test(v)) || typeof data.reason !== 'string' || data.reason.length > 1000) throw new Error('发布控制格式、时效或修订号无效');
  if (data.rollout && (!VERSION.test(data.rollout.version) || !Number.isInteger(data.rollout.percent) || data.rollout.percent < 0 || data.rollout.percent > 100 ||
      typeof data.rollout.salt !== 'string' || !data.rollout.salt || data.rollout.salt.length > 128)) throw new Error('灰度发布配置无效');
  return data;
}
function decision(control, version, installationId, required = false) {
  if (!control) return '';
  if (control.paused || control.blockedVersions.includes(version)) return control.reason || '此版本已暂停分发，等待修复版本';
  // A required compatibility upgrade must be offered to every affected client.
  if (!required && control.rollout?.version === version) {
    const bucket = crypto.createHash('sha256').update(`${control.rollout.salt}:${installationId}`).digest().readUInt32BE() % 10000;
    if (bucket >= control.rollout.percent * 100) return '新版本正在分批发布，将在开放后自动更新';
  }
  return '';
}
module.exports = { verifyControl, decision };
