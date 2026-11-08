'use strict';

const own = (value, key) => Object.prototype.hasOwnProperty.call(value, key);
const policyKeys = ['minimumSupportedVersion', 'updateReason', 'controlURL'];

function parseReleaseArgs(args) {
  const names = { '--notes': 'notesFile', '--minimum-supported-version': 'minimumSupportedVersion', '--update-reason': 'updateReason', '--mirror-base-url': 'mirrorBaseURL', '--control-url': 'controlURL' };
  const result = {};
  for (let i = 0; i < args.length; i++) {
    const key = own(names, args[i]) ? names[args[i]] : null;
    if (!key) throw new Error(`未知发布参数：${args[i]}`);
    if (own(result, key)) throw new Error(`发布参数不能重复：${args[i]}`);
    const value = args[++i];
    if (typeof value !== 'string' || !value.trim() || value.startsWith('--')) throw new Error(`发布参数缺少值：${args[i - 1]}`);
    result[key] = value.trim();
  }
  return result;
}

function validVersion(value) { return typeof value === 'string' && value.trim() === value && /^\d+\.\d+\.\d+$/.test(value); }
function isNewer(left, right) {
  const a = left.split('.').map(BigInt), b = right.split('.').map(BigInt);
  for (let i = 0; i < 3; i++) if (a[i] !== b[i]) return a[i] > b[i];
  return false;
}

function validateReleasePolicy(policy, version) {
  if (!validVersion(version)) throw new Error('发布版本必须是三段数字版本号');
  const result = {};
  if (own(policy, 'controlURL')) {
    const value = policy.controlURL;
    if (typeof value !== 'string' || value.length > 2048 || !require('../update-download.cjs').secureURL(value) || new URL(value).search) throw new Error('发布控制地址必须使用不含查询参数的 HTTPS URL');
    result.controlURL = value;
  }
  if (own(policy, 'minimumSupportedVersion')) {
    const minimum = policy.minimumSupportedVersion;
    if (!validVersion(minimum) || isNewer(minimum, version)) throw new Error('最低支持版本必须是三段数字版本号，且不能高于本次发布版本');
    result.minimumSupportedVersion = minimum;
  }
  if (own(policy, 'updateReason')) {
    if (typeof policy.updateReason !== 'string' || !policy.updateReason.trim() || policy.updateReason.length > 1000) throw new Error('更新原因必须是 1 至 1000 字符的文字');
    if (!own(result, 'minimumSupportedVersion')) throw new Error('填写更新原因时必须指定最低支持版本');
    result.updateReason = policy.updateReason;
  }
  return result;
}

// Every platform of one release must carry the same signed compatibility policy.
// Omission preserves it; an explicit conflict fails before any archive is replaced.
function createReleaseManifest({ version, repo, notes = '', date, options = {}, previous = null }) {
  if (previous !== null && (!previous || previous.schema !== 1 || previous.product !== 'salcara-desktop' || previous.repo !== repo || !validVersion(previous.version) || !previous.files || typeof previous.files !== 'object' || Array.isArray(previous.files))) {
    throw new Error('已有更新清单不属于本项目或格式不正确，请先核对发布目录');
  }
  const requested = Object.fromEntries(policyKeys.filter(key => own(options, key)).map(key => [key, options[key]]));
  if (previous && previous.version === version) {
    const existing = validateReleasePolicy(previous, version);
    for (const key of policyKeys) {
      if (own(requested, key) && requested[key] !== existing[key]) throw new Error('同一版本的更新策略不一致；各平台必须使用相同的最低支持版本和更新原因');
    }
    return { ...previous, notes: notes || previous.notes || '', files: { ...previous.files } };
  }
  const carried = previous ? validateReleasePolicy(previous, version) : {};
  return { schema: 1, product: 'salcara-desktop', repo, version, notes, date, ...validateReleasePolicy({ ...carried, ...requested }, version), files: {} };
}

module.exports = { parseReleaseArgs, validateReleasePolicy, createReleaseManifest };
