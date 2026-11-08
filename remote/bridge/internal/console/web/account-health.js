'use strict';
/* Small, read-only account health badges for the API library.  The health
   endpoint is intentionally advisory; routing remains authoritative in the
   Bridge.  Every request is tied to the rendered list and its source
   revision, so a late response cannot decorate a different account. */
(() => {
  const HEALTH = { seq: 0, fetchSeq: 0, data: new Map(), signature: '', root: null };
  const originalRender = window.renderLocalCards;
  if (typeof originalRender !== 'function') return;

  const signature = () => JSON.stringify((S.local?.accounts || []).map(a => [a.id, a.sourceRevision || '', a.officialAccount?.identityEpoch || 0]));
  const healthRootCurrent = (owner) => owner && owner.seq === HEALTH.seq && owner.route === S.route && owner.routeRevision === S.routeRevision && owner.root === $('#localAccountList') && owner.root?.isConnected !== false && signature() === owner.signature;

  function label(entry) {
    const remaining = Number(entry?.cooldownRemainingSec);
    if (entry?.available === false && Number.isFinite(remaining) && remaining > 0) {
      const mins = Math.max(1, Math.ceil(remaining / 60));
      return { text: `冷却 ${mins} 分钟`, cls: 'cooldown', title: '当前账号暂时冷却，仍可能被服务拒绝' };
    }
    if (entry?.available === false) return { text: '暂不可用', cls: 'unavailable', title: '当前账号暂不可用，稍后会自动重试' };
    if (entry && Number.isFinite(Number(entry.score))) return { text: '可用', cls: 'available', title: '账号健康检查通过' };
    return { text: '状态未知', cls: 'unknown', title: '尚未收到健康检查结果' };
  }

  function apply(owner) {
    if (!healthRootCurrent(owner)) return;
    for (const card of owner.root.querySelectorAll('[data-account-id]')) {
      const id = card.dataset.accountId;
      const entry = HEALTH.data.get(id);
      let badge = card.querySelector('[data-account-health]');
      // Healthy/default pool entries need no extra chrome; only surface a
      // degraded state that can change the user's account selection decision.
      if (!entry || entry.available !== false) {
        badge?.remove();
        continue;
      }
      const info = label(entry);
      if (!badge) {
        badge = document.createElement('span');
        badge.dataset.accountHealth = id;
        const ident = card.querySelector('.api-provider-ident') || card.querySelector('.key-face') || card;
        ident.appendChild(badge);
      }
      badge.className = `account-health-badge ${info.cls}`;
      badge.title = info.title;
      badge.textContent = info.text;
      if (entry?.available === false && Number(entry?.cooldownRemainingSec) > 0) {
        badge.innerHTML = `${esc(info.text)} <button type="button" class="account-health-reset" data-act="accountHealthReset" data-id="${esc(id)}" aria-label="解除 ${esc(entry.accountName || id)} 冷却">解除</button>`;
      }
    }
  }

  async function refresh(owner) {
    const fetchSeq = ++HEALTH.fetchSeq;
    try {
      const rows = await api('/api/local/accounts/health');
      if (!healthRootCurrent(owner) || fetchSeq !== HEALTH.fetchSeq) return;
      HEALTH.data = new Map((Array.isArray(rows) ? rows : []).filter(row => row?.accountId).map(row => [row.accountId, row]));
      apply(owner);
    } catch (_) {
      // Health is supplementary.  Keep cards usable when the optional
      // endpoint is unavailable during startup or an older Bridge is running.
    }
  }

  window.renderLocalCards = function renderLocalCardsWithHealth(...args) {
    const result = originalRender.apply(this, args);
    const root = $('#localAccountList');
    if (S.route !== 'accounts' || !root) return result;
    const currentSignature = signature();
    if (HEALTH.signature !== currentSignature) {
      HEALTH.data.clear();
      HEALTH.signature = currentSignature;
      HEALTH.fetchSeq++;
    }
    const owner = { seq: ++HEALTH.seq, route: S.route, routeRevision: S.routeRevision, root, signature: currentSignature };
    HEALTH.root = root;
    apply(owner);
    void refresh(owner);
    return result;
  };

  ACTIONS.accountHealthReset = async button => {
    if (!button?.isConnected || button.disabled) return;
    await busy(button, async () => {
      const owner = { seq: HEALTH.seq, route: S.route, routeRevision: S.routeRevision, root: $('#localAccountList'), signature: signature() };
      await api('/api/local/accounts/health/reset', { accountId: button.dataset.id });
      if (!healthRootCurrent(owner)) return;
      HEALTH.fetchSeq++;
      toast('已解除冷却', 'ok');
      await refresh(owner);
    });
  };
})();
