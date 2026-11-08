'use strict';

function corePolicy(state) {
  return { required: state.required === true, minimumSupportedVersion: state.minimumSupportedVersion || '',
    targetVersion: state.version || '', reason: state.updateReason || '' };
}

function summary(state, configured) {
  return { current: state.current, configured, phase: state.phase, required: state.required === true,
    minimumSupportedVersion: state.minimumSupportedVersion || '', updateReason: state.updateReason || '',
    error: state.error || state.distributionReason || state.failureReason || '', policyError: state.policyError || '',
    autoBlocked: state.autoBlocked === true, distributionReason: state.distributionReason || '',
    update: state.version ? { version: state.version } : null };
}

function announcementKey(state) {
  return state.version ? `${state.version}:${state.required ? state.minimumSupportedVersion : 'optional'}` : '';
}

// Serialise policy writes: an earlier optional check must never arrive after a
// later required check. A failed write stays pending for the next health retry.
function createPolicySync({ apply, onError }) {
  let desired = null, applied = '', running = null;
  function sync(policy, force = false) {
    desired = JSON.stringify(policy);
    if (force) applied = '';
    if (!running) running = Promise.resolve().then(async () => {
      while (desired !== applied) {
        const key = desired;
        try { await apply(JSON.parse(key)); applied = key; onError(''); }
        catch { onError('无法同步更新限制，请重启 Salcara 后重试；也可从发布页下载安装。'); break; }
      }
    }).finally(() => { running = null; });
    return running;
  }
  return { sync };
}

module.exports = { corePolicy, summary, announcementKey, createPolicySync };
