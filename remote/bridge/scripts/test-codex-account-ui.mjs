import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

const source = await readFile(new URL('../internal/console/web/codex-accounts.js', import.meta.url), 'utf8');
const appSource = await readFile(new URL('../internal/console/web/app.js', import.meta.url), 'utf8');
const accountDTOSource = await readFile(new URL('../internal/codexaccounts/types.go', import.meta.url), 'utf8');
function deferred() { let resolve; const promise = new Promise((done) => { resolve = done; }); return { promise, resolve }; }
function harness(api) {
  const nodes = new Map(), listeners = {};
  const node = (id) => {
    if (!nodes.has(id)) nodes.set(id, { value: '', innerHTML: '', textContent: '', dataset: {}, click() {}, hidden: true });
    return nodes.get(id);
  };
  const context = vm.createContext({
    URL, TextEncoder, copyText: async () => {},
    api, $: node, ACTIONS: {}, S: { route: 'other' }, icon: () => '', esc: (v) => String(v),
    busy: async (_button, fn) => fn(), confirm: () => true, renderLocalCards() {}, toast() {},
    setTimeout: () => 1, clearTimeout() {},
    MutationObserver: class { observe() {} }, document: { addEventListener(name, fn) { (listeners[name] ??= []).push(fn); } },
  });
  vm.runInContext(source, context);
  // These tests isolate actions and HTML generation. The DOM reconciliation
  // itself is exercised separately in test-official-card-state-ui.mjs.
  context.patchOfficialObservationHTML = (current, html) => { if (current) current.outerHTML = html; };
  return { context, node, listeners, run: (code) => vm.runInContext(code, context) };
}

test('official subscription displays only fresh identity-bound verification as remaining time',()=>{
  const h=harness(async()=>({})),now=Date.now();
  h.context.info={id:'fixture',identityEpoch:4,subscription:{status:'active',source:'openai_subscription_api',verified:true,freshness:'fresh',identityEpoch:4,activeUntil:new Date(now+86400000).toISOString(),freshUntil:new Date(now+300000).toISOString()}};
  assert.match(h.run('officialSubscriptionSummary(info)'),/订阅剩余/);
  h.context.info.subscription.freshUntil=new Date(now-1).toISOString();assert.match(h.run('officialSubscriptionSummary(info)'),/待核验/);
  h.context.info.subscription.freshUntil=new Date(now+300000).toISOString();h.context.info.subscription.identityEpoch=3;assert.doesNotMatch(h.run('officialSubscriptionSummary(info)'),/剩余/);
  h.context.info.subscription.source='import_hint';assert.match(h.run('officialSubscriptionSummary(info)'),/未验证/);
  h.context.info.subscription.source='openai_subscription_api';h.context.info.subscription.identityEpoch=4;h.context.info.subscription.activeUntil=new Date(now-1).toISOString();assert.match(h.run('officialSubscriptionSummary(info)'),/待核验/);
});

test('subscription verification freezes vault-only identity, coalesces clicks and ignores a late navigation',async()=>{
  const wait=deferred(),calls=[];const h=harness(async(path,body)=>{calls.push({path,body});return wait.promise;});
  h.context.S.route='accounts';h.context.S.local={accounts:[]};h.context.S.officialRows=[{id:'vault',identityEpoch:5}];
  let refresh=0;h.context.refreshOfficialCards=async()=>{refresh++};
  const button={dataset:{id:'vault',identityEpoch:'5'},isConnected:true};
  const pending=h.context.ACTIONS.officialSubscriptionRefresh(button);
  await h.context.ACTIONS.officialSubscriptionRefresh(button);assert.equal(calls.length,1);
  assert.deepEqual(JSON.parse(JSON.stringify(calls[0].body)),{id:'vault',identityEpoch:5});
  h.context.S.route='overview';wait.resolve({ok:true});await pending;assert.equal(refresh,0);
});

test('a stale subscription button cannot query a replaced account',async()=>{
  const h=harness(async()=>{throw Error('must not request')});h.context.S.local={accounts:[{officialAccount:{id:'vault',identityEpoch:6}}]};
  await h.context.ACTIONS.officialSubscriptionRefresh({dataset:{id:'vault',identityEpoch:'5'},isConnected:true});
});

test('service members use explicit pools, fallback only when empty, and exclude disabled clients', () => {
  const h = harness(async () => ({}));
  assert.equal(h.run(`codexAPIConfiguredMembers({clients:[
    {accountId:'anchor',accountIds:['text'],imageAccountIds:['image']},
    {accountId:'fallback'},
    {accountId:'disabled',disabled:true},
    {accountId:'text',accountIds:['text'],imageAccountIds:['image']}
  ]})`), 3);
  const stats = h.run(`codexAPIHTTPStats({statistics:[{requests:4,inflight:1,httpErrors:2},{requests:6,inflight:2,httpErrors:0},{requests:-1,inflight:'3'}]})`);
  assert.deepEqual(JSON.parse(JSON.stringify(stats)), {requests:10,inflight:3,httpErrors:2});
});

test('service card accurately distinguishes restarted runtime from running intent', () => {
  const h = harness(async () => ({}));
  h.run(`CODEX_API_OVERVIEW.status={enabled:true,running:false,requiresStart:true,baseUrl:'http://127.0.0.1:1/codex-api/v1',clients:[],statistics:[]}`);
  const html = h.run('codexAPIOverviewHTML()');
  assert.match(html, /待启动/);
  assert.match(html, /启动本次服务/);
  assert.match(html, /data-enabled="true"/);
  assert.doesNotMatch(html, /运行中|Token|余额|OAuth|实例网关/);
  h.run('CODEX_API_OVERVIEW.error=true');
  assert.match(h.run('codexAPIOverviewHTML()'), /状态待刷新/);
  assert.match(h.run('codexAPIOverviewHTML()'), /data-act="codexAPIOverviewToggle"[^>]*disabled/);
});

test('a late service read cannot restore old state after a newer read', async () => {
  const old = deferred(); let calls=0;
  const h = harness(async () => ++calls === 1 ? old.promise : {running:false,clients:[]});
  const pending = h.run('loadCodexAPIOverview()');
  await h.run('loadCodexAPIOverview()');
  old.resolve({running:true}); await pending;
  assert.equal(h.run('CODEX_API_OVERVIEW.status.running'), false);
});

test('service start coalesces card and modal clicks without reopening a replaced modal', async () => {
  const changed=deferred(); let writes=0, reads=0;
  const h=harness(async (path) => {
    if(path.endsWith('/enabled')) {writes++; return changed.promise;}
    if(path==='/api/local/codex-api') {reads++; return {running:true,clients:[]};}
    throw Error('stale modal reopened');
  });
  const first=h.context.ACTIONS.codexAPIEnabled({dataset:{enabled:'true'}});
  await h.context.ACTIONS.codexAPIOverviewToggle({dataset:{enabled:'true'}});
  h.run('disposeOfficialPanel()'); changed.resolve({ok:true}); await first;
  assert.equal(writes,1); assert.equal(reads,1);
  assert.equal(h.run('CODEX_API_OVERVIEW.pending'),false);
});

test('created API key is masked, copied only from current panel, and wiped on disposal', async () => {
  const h=harness(async () => ({})), copied=[];
  h.context.copyText=async (text)=>copied.push(text);
  h.run("showCreatedAPIKey('synthetic-once-only-key')");
  assert.equal(h.node('#apiCreatedKey').type,'password');
  assert.equal(h.node('#apiCreatedKeyBox').hidden,false);
  await h.context.ACTIONS.codexAPIKeyCopy();
  assert.deepEqual(copied,['synthetic-once-only-key']);
  assert.doesNotMatch(h.run('codexAPIOverviewHTML()'),/synthetic-once-only-key/);
  h.run('disposeOfficialPanel()');
  assert.equal(h.node('#apiCreatedKey').value,'');
  await assert.rejects(h.context.ACTIONS.codexAPIKeyCopy(),/仅在创建或重置/);
});

test('disposing after modal removal also clears the detached key node', () => {
  const h=harness(async()=>({}));
  h.run("showCreatedAPIKey('synthetic-detached-key')");
  const detached=h.node('#apiCreatedKey');
  h.context.$=id=>id==='#apiCreatedKey'?null:h.node(id);
  h.run('disposeOfficialPanel()');
  assert.equal(detached.value,'');
  assert.equal(h.run('OFFICIAL_PANEL.keyElement'),null);
});

test('official plan chips show verified tiers and label unverified imported hints', () => {
  const h=harness(async () => ({}));
  for(const [family,label] of [['plus','Plus'],['pro','Pro'],['business','Business']]) {
    const html=h.run(`officialPlanBadge({usage:{source:'openai_codex_usage_api',status:'fresh',freshUntil:'2099-01-01T00:00:00Z',verified:true,plan:{verified:true,family:'${family}'}}})`);
    assert.match(html,new RegExp('>'+label+'<'));
    assert.match(html,new RegExp('plan-'+family));
    assert.doesNotMatch(html,/未验证/);
  }
  assert.match(h.run("officialPlanBadge({plan:'pro'})"),/Pro<small>未验证/);
  assert.match(h.run('officialPlanBadge({})'),/套餐未知/);
});

function planBadgeFixture() {
  return { id:'same-official-id',provider:'codex',mode:'legacy_codex_cli',epoch:1,identityEpoch:1,status:'ready',usage:{verified:true,status:'fresh',source:'openai_codex_usage_api',freshUntil:'2099-01-01T00:00:00Z',plan:{verified:true,family:'pro'},rateLimit:{limitReached:true},resetCreditsAvailable:2} };
}
function renderPlanBadge(h,row) { h.context.planRow=row;return h.run('officialPlanBadge(planRow)'); }
function badgeAction(html) { return html.match(/data-act="([^"]+)"/)?.[1]; }
test('plan action fixture follows the production serialized Account mode, not the provider name',()=>{
 const producedMode=accountDTOSource.match(/a := Account\{[^\n]*\bMode: "([^"]+)"/)?.[1];
 assert.equal(producedMode,'legacy_codex_cli','review UI contract when the Account DTO mode changes');
 const h=harness(async()=>({})),row=planBadgeFixture();assert.equal(row.mode,producedMode);assert.notEqual(row.mode,row.provider);
 assert.equal(badgeAction(renderPlanBadge(h,row)),'officialResetCredit');
 assert.equal(badgeAction(renderPlanBadge(h,{...row,mode:'codex'})),'officialUsageRefresh');
});
test('independent plan badge advertises concrete recovery actions without changing tier or account ID',async()=>{
 const h=harness(async()=>({}));
 for(const [status,action,label] of [['needs_login','officialRelogin','重新授权'],['expired','officialRelogin','重新授权'],['save_failed','officialRetrySave','重试保存'],['refresh_failed','officialUsageRefresh','刷新账号额度与状态']]) {
  const row={...planBadgeFixture(),status},html=renderPlanBadge(h,row);
  assert.match(html,/^<button type="button"/);assert.equal(badgeAction(html),action);assert.match(html,/plan-pro/);assert.match(html,/>Pro<\/button>/);
  assert.match(html,/data-id="same-official-id"/);assert.match(html,new RegExp('aria-label="Pro：'+label));assert.doesNotMatch(html,/恢复成功|解封|更改套餐/);
 }
 await h.context.ACTIONS.officialRelogin({dataset:{id:'same-official-id'}});
 assert.match(h.node('#modalRoot').innerHTML,/data-act="officialLogin"[^>]*data-id="same-official-id"/);
 assert.match(renderPlanBadge(h,{plan:'pro'}),/^<span /);
 assert.doesNotMatch(renderPlanBadge(h,{id:'',plan:'pro'}),/data-act=/);
});
test('plan reset affordance requires fresh verified exact limit and available numeric credits',()=>{
 const h=harness(async()=>({})),ready=planBadgeFixture();
 assert.equal(badgeAction(renderPlanBadge(h,ready)),'officialResetCredit');
 assert.match(renderPlanBadge(h,ready),/确认消耗一次官方额度重置次数/);
 const changed=[{mode:'chatgpt_web_session'},{mode:undefined},{mode:'codex'},{status:'unknown'},
  {usage:{...ready.usage,verified:false}},{usage:{...ready.usage,status:'stale'}},{usage:{...ready.usage,source:'import_hint'}},
  {usage:{...ready.usage,freshUntil:'2000-01-01T00:00:00Z'}},{usage:{...ready.usage,freshUntil:undefined}},
  {usage:{...ready.usage,rateLimit:{limitReached:false}}},{usage:{...ready.usage,rateLimit:{primary:{remainingPercent:0}}}},
  ...[0,null,-1,'2',NaN,Infinity].map(count=>({usage:{...ready.usage,resetCreditsAvailable:count}}))];
 for(const patch of changed)assert.equal(badgeAction(renderPlanBadge(h,{...ready,...patch})),'officialUsageRefresh');
});
test('clicking a limited-account plan badge never consumes without existing explicit confirmation',async()=>{
 const calls=[];let confirmations=0;
 const h=harness(async(path,body)=>{calls.push({path,body});if(path.endsWith('/prepare'))return {request:{state:'prepared',redeemRequestId:'synthetic-redemption'}};if(path.endsWith('/consume'))return {ok:true};return {accounts:[]};});
 h.context.confirm=()=>{confirmations++;return false;};
 const action=badgeAction(renderPlanBadge(h,planBadgeFixture()));assert.equal(calls.length,0,'render must not redeem or check provider');
 await h.context.ACTIONS[action]({dataset:{id:'same-official-id'}});
 assert.equal(confirmations,1);assert.equal(calls.length,1);assert.equal(calls[0].path,'/api/local/codex/reset-credit/prepare');assert.equal(calls[0].body.id,'same-official-id');
 h.context.confirm=()=>{confirmations++;return true;};await h.context.ACTIONS[action]({dataset:{id:'same-official-id'}});
 assert.equal(confirmations,2);const consumed=calls.filter(call=>call.path.endsWith('/consume'));assert.equal(consumed.length,1);assert.equal(consumed[0].body.redeemRequestId,'synthetic-redemption');assert.equal(consumed[0].body.id,'same-official-id');
});
test('read-only web-session plan badge refreshes usage without redeeming or attaching API',async()=>{
 const calls=[];const h=harness(async(path,body)=>{calls.push({path,body});return {ok:true,accounts:[]};});
 const html=renderPlanBadge(h,{...planBadgeFixture(),mode:'chatgpt_web_session'});assert.equal(badgeAction(html),'officialUsageRefresh');
 await h.context.ACTIONS[badgeAction(html)]({dataset:{id:'same-official-id'}});
 assert.equal(calls[0].path,'/api/local/codex/usage/refresh');assert.equal(calls[0].body.id,'same-official-id');
 assert.ok(calls.every(call=>!/reset-credit|attach|\/login|\/clients/.test(call.path)));
 const grouped=h.run("apiServicePlanGroupsHTML({groups:[{label:'Pro',tone:'pro',count:1,windows:[]}],unknown:0,unavailable:0})");
 assert.doesNotMatch(grouped,/officialRelogin|officialResetCredit|officialRetrySave|officialUsageRefresh/);
});

test('explicit advanced creation exposes its one-time key even if every subsequent GET fails', async () => {
  let created = false;
  const h=harness(async path => {
    if(path.endsWith('/clients')) { created = true; return {id:'synthetic-client',key:'synthetic-created-key'}; }
    if (!created && path === '/api/local/codex-api') return { clients: [] };
    if (!created && path === '/api/local/accounts') return { accounts: [{ id: 'account' }] };
    throw Error('list unavailable');
  });
  h.run("apiClientPanel={epoch:OFFICIAL_PANEL.epoch,element:$('#officialPanel'),mode:'editor',clientId:'',clients:[],accounts:[{id:'account'}]}");
  h.node('#apiClientAccount').value='account';
  h.node('#apiClientName').value='explicit'; h.node('#apiClientConcurrency').value='16';
  await h.context.ACTIONS.codexAPISave({dataset:{id:''}});
  assert.equal(h.node('#apiCreatedKey').value,'synthetic-created-key');
  assert.equal(h.node('#apiCreatedKey').type,'password');
  assert.match(h.node('#modalRoot').innerHTML,/客户端密钥/);
  assert.doesNotMatch(h.node('#modalRoot').innerHTML,/synthetic-created-key/,'no secret in HTML attributes');
});

test('saving and rotating share one write gate across editor buttons and modal changes', async () => {
  const saved=deferred(); let writes=0;
  const h=harness(async path => {if(path.endsWith('/rotate')) {writes++;return saved.promise;}return {clients:[],accounts:[]};});
  h.run("apiClientPanel={epoch:OFFICIAL_PANEL.epoch,element:$('#officialPanel'),mode:'editor',clientId:'client',clients:[],accounts:[{id:'account'}]}");
  h.node('#apiClientAccount').value='account'; h.node('#apiClientConcurrency').value='16';
  const first=h.context.ACTIONS.codexAPIRotate({dataset:{id:'client'}});
  await h.context.ACTIONS.codexAPISave({dataset:{id:'client'}});
  await h.context.ACTIONS.codexAPIRotate({dataset:{id:'client',rotate:'true'}});
  assert.equal(writes,1);
  saved.resolve({key:'synthetic-rotation-key'});await first;
  assert.equal(h.node('#apiCreatedKey').value,'synthetic-rotation-key');
  assert.equal(h.run('apiClientWritePending'),false);
});

test('import checks preserve unknown and refreshable credentials unless explicitly filtered', async () => {
  const imported=[], statuses=['verified','invalid','unknown','needs_refresh','limited','quota_only'];
  const h=harness(async(path,body)=>{
    if(path.endsWith('/check')) return {accounts:statuses.map((status,index)=>({index,status}))};
    if(path.endsWith('/import')) {imported.push(body);return {ok:true};}
    return {accounts:[]};
  });
  h.run("OFFICIAL_PANEL.credentials='synthetic-batch';setOfficialImportPreview({accounts:Array.from({length:6},(_,i)=>({name:'item-'+i}))})");
  await h.context.ACTIONS.officialImportCheck();
  h.context.ACTIONS.officialImportFilter({dataset:{filter:'invalid'}});
  assert.deepEqual(h.run('[...OFFICIAL_PANEL.importSelection]').join(','),'0,2,3,4,5');
  h.context.ACTIONS.officialImportFilter({dataset:{filter:'verified'}});
  await h.context.ACTIONS.officialImport({});
  assert.deepEqual(JSON.parse(JSON.stringify(imported)),[{credentials:'synthetic-batch',format:'json',selection:[0]}]);
});

test('closed or replaced import reviews ignore late check results and never import while checking', async () => {
  const checked=deferred();let writes=0,checks=0;
  const h=harness(async path=>{if(path.endsWith('/check')) {checks++;return checked.promise;}writes++;return {};});
  h.run("OFFICIAL_PANEL.credentials='first';setOfficialImportPreview({accounts:[{name:'first'}]})");
  const first=h.context.ACTIONS.officialImportCheck();
  await h.context.ACTIONS.officialImportCheck();await h.context.ACTIONS.officialImport({});
  assert.equal(checks,1);assert.equal(writes,0);
  h.run("disposeOfficialPanel();OFFICIAL_PANEL.credentials='second';setOfficialImportPreview({accounts:[{name:'second'}]})");
  checked.resolve({accounts:[{index:0,status:'invalid'}]});await first;
  assert.equal(h.run('OFFICIAL_PANEL.importPreview.accounts[0].name'),'second');
  assert.equal(h.run('OFFICIAL_PANEL.importPreview.accounts[0].status'),undefined);
});

test('malformed check indices and temporary failures keep selection without leaking upstream errors', async () => {
  for(const result of [null,{accounts:[{index:1,status:'invalid'}]},{accounts:[{index:0,status:'unexpected'}]}]) {
    const h=harness(async()=>{if(result)return result;throw Error('sensitive-provider-response');});
    h.run("OFFICIAL_PANEL.credentials='fixture';setOfficialImportPreview({accounts:[{name:'fixture'}]})");
    await h.context.ACTIONS.officialImportCheck();
    assert.equal(h.run('OFFICIAL_PANEL.importSelection.size'),1);
    assert.equal(h.run('OFFICIAL_PANEL.importChecking'),false);
    assert.match(h.node('#officialImportCheckError').textContent,/仍保留/);
    assert.doesNotMatch(h.node('#officialImportCheckError').textContent,/sensitive/);
  }
});

test('double login does not open another SDK listener; closing cancels pending result', async () => {
  const started = deferred(); let starts = 0; const cancelled = [];
  const h = harness(async (path, body) => {
    if (path.endsWith('/login')) { starts++; return started.promise; }
    if (path.endsWith('/cancel')) { cancelled.push(body.id); return {}; }
    throw new Error('unexpected endpoint');
  });
  const first = h.context.ACTIONS.officialLogin({ dataset: {} });
  await h.context.ACTIONS.officialLogin({ dataset: {} });
  assert.equal(starts, 1);
  h.run('disposeOfficialPanel()');
  started.resolve({ id: 'synthetic-login-session' });
  await first;
  assert.deepEqual(cancelled, ['synthetic-login-session']);
  assert.equal(h.run('OFFICIAL_PANEL.session'), null);
});

test('late file preview cannot replace newer exact credential bytes', async () => {
  const older = deferred(), received = [];
  const h = harness(async (_path, body) => { received.push(body.files[0]); return { accounts: [{ name: 'fixture' }] }; });
  const change = (file) => Promise.all(h.listeners.change.map((fn) => fn({ target: { id: 'officialFile', value: 'fixture', files: [file] } })));
  const old = change({ size: 1, text: () => older.promise });
  const exact = '{"access_token":"first","access_token":"second"}';
  await change({ size: exact.length, text: async () => exact });
  older.resolve('older'); await old;
  assert.equal(received[0], exact, 'frontend must not parse/stringify away duplicate fields');
  assert.equal(h.run('OFFICIAL_PANEL.files[0]'), exact);
  assert.equal(received.length, 1, 'obsolete file read must not issue a preview');
});

test('device login sends its mode and presents only its verified link and code', async () => {
  const copied = [], starts = [];
  let cancelled = false;
  const h = harness(async (path, body) => {
    if (path.endsWith('/login')) { starts.push(body); return { id: 'device-session' }; }
    if (path.endsWith('/status')) return cancelled ? { id: 'device-session', mode: 'device', status: 'cancelled' } : { id: 'device-session', mode: 'device', status: 'pending', verificationURL: 'https://auth.openai.com/codex/device', userCode: 'ABCD-1234', expiresAt: '2099-01-01T00:00:00Z' };
    if (path.endsWith('/cancel')) { cancelled = true; return { ok: true }; }
    throw new Error(path);
  });
  h.context.copyText = async (value) => copied.push(value);
  await h.context.ACTIONS.officialLogin({ dataset: { mode: 'device', id: 'replace-account' } });
  assert.deepEqual(JSON.parse(JSON.stringify(starts)), [{ mode: 'device', replaceId: 'replace-account' }]);
  const html = h.node('#officialLoginState').innerHTML;
  assert.match(html, /ABCD-1234/);
  assert.match(html, /复制授权链接/);
  assert.doesNotMatch(html, /officialCallbackURL/);
  assert.match(html, /id="officialAuthorizationURL" readonly/);
  assert.equal(h.node('#officialBrowserLogin').disabled, true);
  assert.equal(h.node('#officialDeviceLogin').disabled, true);
  assert.equal(h.node('#officialDeviceLogin').className, 'btn primary');
  assert.equal(h.node('#officialBrowserLogin').className, 'btn');
  for (const tab of ['oauth','token','file']) assert.equal(h.node('#officialTab-' + tab).disabled, true);
  assert.equal(h.node('#accountAdd-apikey').disabled, true);
  await h.context.ACTIONS.officialTab({ dataset: { tab: 'token' } });
  assert.equal(h.run('OFFICIAL_PANEL.session'), 'device-session', 'tab switching cannot silently cancel an authorization');
  await h.context.ACTIONS.officialLoginCopy();
  await h.context.ACTIONS.officialLoginCopyCode();
  assert.deepEqual(copied, ['https://auth.openai.com/codex/device', 'ABCD-1234']);
  await h.context.ACTIONS.officialLoginCancel({});
  assert.equal(h.node('#officialBrowserLogin').disabled, false);
  assert.equal(h.node('#officialTab-token').disabled, false);
  await assert.rejects(h.context.ACTIONS.officialLoginCopy(), /已失效/);
});

test('adding accounts opens dedicated tabs without reading another account list', async () => {
  const h = harness(async () => { throw new Error('adding must not fetch another manager'); });
  await h.context.ACTIONS.officialAccounts();
  const html = h.node('#modalRoot').innerHTML;
  assert.match(html, /id="officialTitle">添加凭据<\/h3>/);
  for (const tab of ['oauth','token','file']) assert.match(html, new RegExp('id="officialTab-' + tab + '"'));
  for (const tab of ['apikey','codex']) assert.match(html, new RegExp('id="accountAdd-' + tab + '"'));
  assert.doesNotMatch(html, /officialAccountRows|更多操作/);
  await h.context.ACTIONS.officialTab({ dataset: { tab: 'token', id: 'replace-me' } });
  assert.equal(h.run('OFFICIAL_PANEL.replaceId'), 'replace-me');
  let opens = 0; h.context.ACTIONS.localNew = () => { opens++; };
  await h.context.ACTIONS.accountAddTab({ dataset: { tab: 'apikey' } });
  assert.equal(opens, 1, 'API Key uses the existing editor');
});

test('unified library deduplicates safe vault rows without mutating callable account collection', () => {
  const h = harness(async () => ({}));
  h.context.S.local = { accounts: [{ id: 'api', name: 'API' }, { id: 'attached', name: 'Official', officialAccount: { id: 'vault-a' } }] };
  h.context.S.officialRows = [{ id: 'vault-a' }, { id: 'web', mode: 'chatgpt_web_session' }, { id: 'web', mode: 'chatgpt_web_session' }];
  const rows = h.run('officialLibraryAccounts(S.local.accounts)');
  assert.equal(rows.length, 3);
  assert.equal(h.context.S.local.accounts.length, 2);
  assert.equal(rows[2].libraryOnly, true);
});

test('cleanup only submits the confirmed revision snapshot and coalesces double clicks', async () => {
  const deleting = deferred(), calls = [];
  const h = harness(async (path, body) => {
    calls.push({ path, body });
    if (path.endsWith('/preview')) return { accounts: [{ id: 'invalid', name: 'Fixture', epoch: 7, identityEpoch: 3 }] };
    if (path.endsWith('/cleanup')) return deleting.promise;
    return { accounts: [] };
  });
  await h.context.ACTIONS.officialCleanup();
  assert.equal(calls.length, 1, 'preview cannot remove credentials');
  const deletingOnce = h.context.ACTIONS.officialCleanupConfirm({});
  await h.context.ACTIONS.officialCleanupConfirm({});
  assert.equal(calls.filter(call => call.path.endsWith('/cleanup')).length, 1);
  assert.deepEqual(JSON.parse(JSON.stringify(calls[1].body)), { accounts: [{ id: 'invalid', epoch: 7, identityEpoch: 3 }] });
  deleting.resolve({ ok: true, deleted: [], skipped: [{ id: 'invalid', reason: 'changed' }], syncFailed: false });
  await deletingOnce;
  assert.match(h.node('#officialCleanupState').textContent, /0 个账号，1 个状态已变化，已保留/);
  assert.equal(h.run('OFFICIAL_PANEL.cleanup'), null);
});

test('closed cleanup previews and results cannot repaint another modal', async () => {
  for (const stage of ['preview', 'delete']) {
    const waiting = deferred();
    const row = { id: 'invalid', name: 'Fixture', epoch: 7, identityEpoch: 3 };
    const h = harness(async path => path.endsWith('/preview') ? (stage === 'preview' ? waiting.promise : { accounts: [row] }) : waiting.promise);
    const opening = h.context.ACTIONS.officialCleanup();
    let running = opening;
    if (stage === 'delete') { await opening; running = h.context.ACTIONS.officialCleanupConfirm({}); }
    await h.context.ACTIONS.officialAccounts();
    const html = h.node('#modalRoot').innerHTML;
    waiting.resolve(stage === 'preview' ? { accounts: [row] } : { ok: true, deleted: [row], skipped: [], syncFailed: false });
    await running;
    assert.equal(h.node('#modalRoot').innerHTML, html);
    assert.equal(h.run('OFFICIAL_PANEL.cleanup'), null);
  }
});

test('cleanup config retry retains the exact original confirmation rather than current accounts', async () => {
  const bodies = [];
  const h = harness(async (path, body) => {
    if (path.endsWith('/preview')) return { accounts: [{ id: 'invalid', name: 'Fixture', epoch: 7, identityEpoch: 3 }] };
    if (path.endsWith('/cleanup')) { bodies.push(JSON.parse(JSON.stringify(body))); return { ok: true, deleted: [{ id: 'invalid' }], skipped: [], syncFailed: bodies.length === 1 }; }
    return { accounts: [{ id: 'replacement', name: 'New' }] };
  });
  await h.context.ACTIONS.officialCleanup();
  await h.context.ACTIONS.officialCleanupConfirm({});
  assert.match(h.node('#officialCleanupState').innerHTML, /重试同步/);
  await h.context.ACTIONS.officialCleanupConfirm({});
  assert.deepEqual(bodies, [0, 1].map(() => ({ accounts: [{ id: 'invalid', epoch: 7, identityEpoch: 3 }] })));
  assert.equal(h.run('OFFICIAL_PANEL.cleanup'), null);
});

test('empty malformed or unknown cleanup results never auto-delete or blindly retry', async () => {
  for (const accounts of [[], null, [{ id: 'bad', name: 'Bad', epoch: 0, identityEpoch: 1 }]]) {
    let calls = 0;
    const h = harness(async () => { calls++; return { accounts }; });
    await h.context.ACTIONS.officialCleanup();
    await h.context.ACTIONS.officialCleanupConfirm({});
    assert.equal(calls, 1);
  }
  let deletes = 0;
  const h = harness(async path => {
    if (path.endsWith('/preview')) return { accounts: [{ id: 'invalid', name: 'Fixture', epoch: 7, identityEpoch: 3 }] };
    deletes++; throw new Error('transport lost');
  });
  await h.context.ACTIONS.officialCleanup();
  await h.context.ACTIONS.officialCleanupConfirm({});
  await h.context.ACTIONS.officialCleanupConfirm({});
  assert.equal(deletes, 1);
  assert.match(h.node('#officialCleanupState').innerHTML, /重新检查/);
});

test('cleanup action is hidden until the authoritative library reports an eligible account', async () => {
  let rows = [{ id: 'healthy', status: 'ready' }];
  const h = harness(async () => ({ accounts: rows }));
  h.context.S.route = 'accounts';
  let html = '';
  h.run('officialAccountActions')({ insertAdjacentHTML(_, value) { html += value; } });
  assert.match(html, /officialCleanup[^>]*hidden/);
  await h.run('loadOfficialLibrary()');
  assert.equal(h.node('#officialCleanupAction').hidden, true);
  rows = [{ id: 'bad', cleanup: { eligible: true, reason: 'workspace_revoked', recoverable: false } }];
  await h.run('loadOfficialLibrary()');
  assert.equal(h.node('#officialCleanupAction').hidden, false);
  h.run('officialMutationToken()');
  assert.equal(h.node('#officialCleanupAction').hidden, true, 'mutation invalidates the stale candidate summary');
  rows = [];
  await h.run('loadOfficialLibrary()');
  assert.equal(h.node('#officialCleanupAction').hidden, true);
});

test('empty cleanup preview hides stale eligibility and fences an older library read', async () => {
  const older = deferred();
  const rows = [{ id: 'recovered', cleanup: { eligible: true, reason: 'workspace_revoked', recoverable: false } }];
  let reads = 0;
  const h = harness(async (path) => {
    if (path.endsWith('/preview')) return { accounts: [] };
    return ++reads === 1 ? { accounts: rows } : older.promise;
  });
  h.context.S.route = 'accounts';
  await h.run('loadOfficialLibrary()');
  assert.equal(h.node('#officialCleanupAction').hidden, false);
  const pending = h.run('loadOfficialLibrary()');
  await h.context.ACTIONS.officialCleanup();
  assert.match(h.node('#officialCleanupState').innerHTML, /没有可清理/);
  assert.equal(h.node('#officialCleanupAction').hidden, true);
  older.resolve({ accounts: rows });
  await pending;
  assert.equal(h.node('#officialCleanupAction').hidden, true, 'late old eligibility must not revive the button');
});

test('older empty cleanup preview cannot invalidate a newer authoritative library result', async () => {
  const older = deferred();
  const rows = [{ id: 'failed', cleanup: { eligible: true, reason: 'workspace_revoked', recoverable: false } }];
  const h = harness(async (path) => path.endsWith('/preview') ? older.promise : { accounts: rows });
  h.context.S.route = 'accounts';
  const preview = h.context.ACTIONS.officialCleanup();
  await h.run('loadOfficialLibrary()');
  older.resolve({ accounts: [] });
  await preview;
  assert.equal(h.node('#officialCleanupAction').hidden, false, 'newer library result wins');
});

test('recoverable cleanup candidates require explicit selection and submit the frozen observation revision', async () => {
  const requests = [], revision = 'a'.repeat(64);
  const h = harness(async (path, body) => {
    if (path.endsWith('/preview')) return { accounts: [{ id: 'quota', name: 'Limited permission', epoch: 8, identityEpoch: 3, reason: 'usage_forbidden', recoverable: true, revision }] };
    if (path.endsWith('/cleanup')) { requests.push(body); return { ok: true, deleted: [], skipped: [], syncFailed: false }; }
    return { accounts: [] };
  });
  await h.context.ACTIONS.officialCleanup();
  assert.match(h.node('#officialCleanupState').innerHTML, /额度访问受限 · 可恢复/);
  assert.doesNotMatch(h.node('#officialCleanupState').innerHTML, / checked/);
  assert.equal(h.node('#officialCleanupConfirm').disabled, true);
  await h.context.ACTIONS.officialCleanupConfirm({});
  assert.equal(requests.length, 0);
  for (const listener of h.listeners.change) await listener({ target: { dataset: { cleanupId: 'quota' }, checked: true, closest: () => h.node('#officialCleanupState') } });
  assert.equal(h.node('#officialCleanupConfirm').disabled, false);
  await h.context.ACTIONS.officialCleanupConfirm({});
  assert.deepEqual(JSON.parse(JSON.stringify(requests)), [{ accounts: [{ id: 'quota', epoch: 8, identityEpoch: 3, revision }] }]);
});

test('workspace loss defaults to selected but malformed expanded cleanup metadata cannot delete', async () => {
  const revision = 'b'.repeat(64), base = { id: 'lost', name: 'Lost workspace', epoch: 7, identityEpoch: 3, reason: 'workspace_revoked', recoverable: false, revision };
  const h = harness(async () => ({ accounts: [base] }));
  await h.context.ACTIONS.officialCleanup();
  assert.match(h.node('#officialCleanupState').innerHTML, /checked/);
  assert.match(h.node('#officialCleanupState').innerHTML, /工作空间权限已移除/);
  for (const row of [{...base, revision: ''}, {...base, reason: 'unknown'}, {...base, reason: 'usage_refresh_failed', recoverable:true}, {...base, recoverable: 'false'}, {...base, revision: undefined}]) {
    let calls = 0;
    const bad = harness(async () => { calls++; return { accounts: [row] }; });
    await bad.context.ACTIONS.officialCleanup();
    await bad.context.ACTIONS.officialCleanupConfirm({});
    assert.equal(calls, 1);
    assert.equal(bad.run('OFFICIAL_PANEL.cleanup'), null);
  }
});

test('a detached cleanup checkbox cannot change the replacement panel selection', async () => {
  const row = { id: 'retry', name: 'Retry', epoch: 7, identityEpoch: 3, reason: 'quota_exhausted', recoverable: true, revision: 'c'.repeat(64) };
  const h = harness(async () => ({ accounts: [row] }));
  await h.context.ACTIONS.officialCleanup();
  for (const listener of h.listeners.change) await listener({ target: { dataset: { cleanupId: 'retry' }, checked: true, closest: () => ({}) } });
  assert.equal(h.node('#officialCleanupConfirm').disabled, true);
});

test('network quota failures without a backend cleanup verdict never show the cleanup action', async () => {
  const h = harness(async () => ({ accounts: [{ id: 'offline', status: 'refresh_failed', usage: {status:'unavailable',errorCode:'network'} }] }));
  h.context.S.route = 'accounts';
  await h.run('loadOfficialLibrary()');
  assert.equal(h.node('#officialCleanupAction').hidden, true);
  h.run("S.officialRows[0].cleanup={eligible:true,reason:'usage_refresh_failed',recoverable:true};syncOfficialCleanupAction()");
  assert.equal(h.node('#officialCleanupAction').hidden, true, 'obsolete broad failure metadata must not enable cleanup');
});

test('single add entry has two kinds and active authorization cannot silently switch kind', async () => {
  const h = harness(async () => ({}));
  let html = '', opens = 0;
  h.run('officialAccountActions')({ insertAdjacentHTML(_, value) { html += value; } });
  assert.doesNotMatch(html, /添加 Codex/);
  assert.match(html, /officialCleanup/);
  h.context.ACTIONS.localNew = () => { opens++; };
  h.run('OFFICIAL_PANEL.loginPending = true');
  await h.context.ACTIONS.accountAddTab({ dataset: { tab: 'apikey' } });
  assert.equal(opens, 0);
  h.run('OFFICIAL_PANEL.loginPending = false');
  await h.context.ACTIONS.accountAddTab({ dataset: { tab: 'apikey' } });
  assert.equal(opens, 1);
});

test('a vault read started before deletion cannot resurrect removed library cards', async () => {
  const old = deferred(); let calls = 0;
  const h = harness(async path => {
    if (path === '/api/local/codex/accounts') return ++calls === 1 ? old.promise : { accounts: [] };
    return { accounts: [] };
  });
  h.context.S = { route: 'accounts', local: { accounts: [] }, officialRows: [] };
  const reading = h.run('loadOfficialLibrary()');
  await h.context.ACTIONS.officialDelete({ dataset: { id: 'old' } });
  old.resolve({ accounts: [{ id: 'old', name: 'Removed' }] }); await reading;
  assert.equal(h.context.S.officialRows.length, 0);
  assert.equal(h.node('#modalRoot').innerHTML, '', 'deleting a card must not open the add-account dialog');
});

test('cached page initial local read cannot resurrect cards after confirmed cleanup', async () => {
  const delayed = deferred(); let localReads = 0;
  const h = harness(async path => {
    if (path === '/api/local/accounts') return ++localReads === 1 ? delayed.promise : { accounts: [] };
    if (path.endsWith('/preview')) return { accounts: [{ id: 'invalid', name: 'Old', epoch: 7, identityEpoch: 3 }] };
    if (path.endsWith('/cleanup')) return { ok: true, deleted: [{ id: 'invalid' }], skipped: [], syncFailed: false };
    return { accounts: [] };
  });
  h.context.S = { route: 'accounts', local: { accounts: [{ id: 'old' }] } };
  h.context.loadState = async () => {};
  h.context.renderAccountsPage = () => {};
  h.context.RENDER_accounts = null;
  vm.runInContext(appSource.slice(appSource.indexOf('RENDER_accounts = function (view)'), appSource.indexOf('function localEditor(')), h.context);
  h.context.RENDER_accounts({ isConnected: true });
  await h.context.ACTIONS.officialCleanup();
  await h.context.ACTIONS.officialCleanupConfirm({});
  delayed.resolve({ accounts: [{ id: 'old' }] });
  await delayed.promise; await Promise.resolve();
  assert.equal(h.context.S.local.accounts.length, 0);
});

test('obsolete mutation cannot invalidate a newer in-flight local refresh', async () => {
  const newerRead = deferred(); let reads = 0;
  const h = harness(async () => { reads++; return newerRead.promise; });
  h.context.S.local = { accounts: [{ id: 'old' }] };
  h.run('globalThis.olderMutation = officialMutationToken(); globalThis.newerMutation = officialMutationToken()');
  const latest = h.run('refreshOfficialCards(newerMutation)');
  const revision = h.context.S.localAccountsRevision;
  await h.run('refreshOfficialCards(olderMutation)');
  assert.equal(reads, 1, 'obsolete mutation started another read');
  assert.equal(h.context.S.localAccountsRevision, revision, 'obsolete mutation invalidated the newest read');
  newerRead.resolve({ accounts: [] }); await latest;
  assert.equal(h.context.S.local.accounts.length, 0);
});

test('saving an unchanged account pool closes its own dialog instead of opening account import', async () => {
  const h = harness(async () => ({ accounts: [], ok: true }));
  h.node('#modalRoot').innerHTML = 'pool editor';
  h.node('#officialQuotaReserve').value = '10';
  h.node('#officialPoolEnabled').value = 'true';
  h.node('#officialPoolGroup').value = 'demo';
  await h.context.ACTIONS.officialPoolSave({ dataset: { id: 'demo' } });
  assert.equal(h.node('#modalRoot').innerHTML, '');
  assert.equal(h.run('OFFICIAL_PANEL.epoch'), 1);
});

test('browser polling preserves manual callback input and submits to its current session', async () => {
  const received = [];
  const h = harness(async (path, body) => {
    if (path.endsWith('/login')) return { id: 'browser-session' };
    if (path.endsWith('/status')) return { id: 'browser-session', mode: 'browser', status: 'pending', authorizationURL: 'https://auth.openai.com/oauth/authorize?state=fixture', expiresAt: '2099-01-01T00:00:00Z' };
    if (path.endsWith('/callback')) { received.push(body); return {}; }
    throw new Error(path);
  });
  await h.context.ACTIONS.officialLogin({ dataset: {} });
  let paints = 0;
  const node = h.node('#officialLoginState');
  Object.defineProperty(node, 'innerHTML', { set() { paints++; h.node('#officialCallbackURL').value = ''; } });
  h.node('#officialCallbackURL').value = 'http://localhost:1455/auth/callback?state=fixture&code=fixture';
  await h.run('pollOfficialLogin(OFFICIAL_PANEL.session, OFFICIAL_PANEL.epoch)');
  assert.equal(paints, 0);
  await h.context.ACTIONS.officialLoginCallback({});
  assert.equal(received[0].id, 'browser-session');
  assert.match(received[0].callbackURL, /code=fixture/);
  assert.equal(h.node('#officialCallbackURL').value, '');
});

test('untrusted or expired authorization URLs never become clickable or copyable', () => {
  const h = harness(async () => ({}));
  for (const row of [
    { authorizationURL: 'https://auth.openai.com.evil.test/oauth/authorize' },
    { authorizationURL: 'https://user@auth.openai.com/oauth/authorize' },
    { authorizationURL: 'https://auth.openai.com/oauth/authorize', expiresAt: '2000-01-01T00:00:00Z' },
    { mode: 'device', verificationURL: 'https://auth.openai.com/oauth/authorize', userCode: 'ABCD' },
  ]) {
    h.context.row = { status: 'pending', ...row };
    assert.doesNotMatch(h.run('officialLoginProgressHTML(row)'), /href=|officialLoginCopy|officialCallbackURL/);
  }
});

test('temporary login status failure schedules another poll without starting a new authorization', async () => {
  let statusCalls = 0, starts = 0, retry;
  const h = harness(async (path) => {
    if (path.endsWith('/login')) { starts++; return { id: 'session' }; }
    if (++statusCalls === 1) throw new Error('temporarily offline');
    return { id: 'session', status: 'pending', mode: 'device', verificationURL: 'https://auth.openai.com/codex/device', userCode: 'ABCD' };
  });
  h.context.setTimeout = (fn) => { retry = fn; return 1; };
  await h.context.ACTIONS.officialLogin({ dataset: { mode: 'device' } });
  assert.match(h.node('#officialError').textContent, /重试/);
  assert.match(h.node('#officialLoginState').innerHTML, /data-act="officialLoginCancel"/);
  await retry();
  assert.equal(starts, 1);
  assert.equal(statusCalls, 2);
  assert.equal(h.node('#officialError').textContent, '');
});

test('a lost login session can be restarted instead of polling forever', async () => {
  let timers = 0;
  const h = harness(async (path) => {
    if (path.endsWith('/login')) return { id: 'gone-session' };
    throw new Error('Codex account not found');
  });
  h.context.setTimeout = () => { timers++; return 1; };
  await h.context.ACTIONS.officialLogin({ dataset: { mode: 'browser' } });
  assert.equal(timers, 0);
  assert.equal(h.run('OFFICIAL_PANEL.session'), null);
  assert.equal(h.run('OFFICIAL_PANEL.loginPending'), false);
  assert.match(h.node('#officialLoginState').textContent, /重新授权/);
});

test('multi-file preview preserves exact bytes and one confirmation commits the whole batch', async () => {
  const batches = [];
  const h = harness(async (path, body) => {
    if (path.endsWith('/preview')) { batches.push(body); return { accounts: [{ name: 'one' }, { name: 'two' }] }; }
    if (path.endsWith('/import')) { batches.push(body); return { ok: true }; }
    return { accounts: [] };
  });
  h.node('#officialImportFormat').value = 'refresh_token';
  const files = ['\uFEFFsynthetic-one\r\n', '["synthetic-two"]'];
  await Promise.all(h.listeners.change.map((fn) => fn({ target: { id: 'officialFile', value: '', files: files.map((value) => ({ size: value.length, text: async () => value })) } })));
  await h.context.ACTIONS.officialImport({});
  assert.deepEqual(JSON.parse(JSON.stringify(batches)), [{ files, format: 'refresh_token' }, { files, format: 'refresh_token', selection: [0, 1] }]);
  assert.equal(h.run('OFFICIAL_PANEL.files'), null);
});

test('changing file format invalidates a preview and prevents stale credentials being imported', async () => {
  const calls = [];
  const h = harness(async (path) => { calls.push(path); return { accounts: [{ name: 'one' }] }; });
  const change = (target) => Promise.all(h.listeners.change.map((fn) => fn({ target })));
  await change({ id: 'officialFile', files: [{ size: 3, text: async () => 'one' }] });
  await change({ id: 'officialImportFormat' });
  await h.context.ACTIONS.officialImport({});
  assert.deepEqual(calls, ['/api/local/codex/import/preview']);
  assert.equal(h.run('OFFICIAL_PANEL.files'), null);
});

test('a completed import does not discard a newer preview in the same panel', async () => {
  const imported = deferred();
  const h = harness(async (path) => {
    if (path.endsWith('/preview')) return { accounts: [{ name: 'fixture' }] };
    if (path.endsWith('/import')) return imported.promise;
    return { accounts: [] };
  });
  const choose = (value) => Promise.all(h.listeners.change.map((fn) => fn({ target: { id: 'officialFile', files: [{ size: value.length, text: async () => value }] } })));
  await choose('old');
  const pending = h.context.ACTIONS.officialImport({});
  await choose('new');
  imported.resolve({ ok: true });
  await pending;
  assert.equal(h.run('OFFICIAL_PANEL.files[0]'), 'new');
  assert.equal(h.run('OFFICIAL_PANEL.epoch'), 0);
});

test('oversized file batches fail before any file read or preview request', async () => {
  let reads = 0, requests = 0;
  const h = harness(async () => { requests++; return {}; });
  const file = { size: 600 * 1024, text: async () => { reads++; return ''; } };
  await Promise.all(h.listeners.change.map((fn) => fn({ target: { id: 'officialFile', files: [file, file] } })));
  assert.equal(reads, 0);
  assert.equal(requests, 0);
  assert.match(h.node('#officialError').textContent, /1 MiB/);
});

test('web-session rows expose quota queries but no API association or refresh-token action', async () => {
  const h = harness(async (path) => ({ accounts: path === '/api/local/codex/accounts' ? [{ id: 'web', name: 'Web', mode: 'chatgpt_web_session', status: 'ready' }] : [] }));
  h.context.S = { route: 'accounts', local: { accounts: [] } };
  await h.run('loadOfficialLibrary()');
  const html = h.run('officialAccountCard(officialLibraryAccounts(S.local.accounts)[0], [], () => "")');
  assert.match(html, /网页会话 · 仅查额度/);
  assert.match(html, /data-act="officialUsageRefresh"/);
  assert.doesNotMatch(html, /data-act="officialRelogin"/);
  assert.doesNotMatch(html, /data-act="officialAttach"|data-act="officialRefresh"|data-act="officialPool"/);
  assert.doesNotMatch(html, /<details|更多|data-act="codexAPI"/);
  assert.equal(h.context.S.local.accounts.length, 0, 'quota-only rows must not become callable accounts');
});

test('closing API panel while client is created does not reopen it or expose key elsewhere', async () => {
  const created = deferred(); let statusCalls = 0;
  const h = harness(async (path) => {
    if (path.endsWith('/clients')) return created.promise;
    if (path === '/api/local/codex-api') return { clients: [] };
    if (path === '/api/local/accounts') return { accounts: [{ id: 'fixture-account' }] };
    statusCalls++; throw new Error('closed panel should not request rendering');
  });
  h.run("apiClientPanel={epoch:OFFICIAL_PANEL.epoch,element:$('#officialPanel'),mode:'service',firstClient:true,clients:[],accounts:[{id:'fixture-account'}]}");
  h.node('#apiClientName').value = 'fixture'; h.node('#apiClientAccount').value = 'fixture-account';
  const saving = h.context.ACTIONS.codexAPICreate({ dataset: {} });
  for (let i=0;i<12;i++) await Promise.resolve();
  h.run('disposeOfficialPanel()');
  created.resolve({ id: 'fixture-client', key: 'synthetic-new-client-key' }); await saving;
  assert.equal(statusCalls, 0);
  assert.equal(h.node('#apiCreatedKey').value, '');
});

test('invalid concurrency and duplicate alias are rejected before request', async () => {
  let calls = 0;
  const h = harness(async () => { calls++; });
  h.run("apiClientPanel={epoch:OFFICIAL_PANEL.epoch,element:$('#officialPanel'),mode:'editor',clientId:'fixture',clients:[],accounts:[]}");
  h.node('#apiClientAliases').value = 'friendly = first\nfriendly = second';
  await assert.rejects(h.context.ACTIONS.codexAPISave({ dataset: { id: 'fixture' } }), /不能重复/);
  h.node('#apiClientAliases').value = 'friendly = first'; h.node('#apiClientConcurrency').value = 'not-a-number';
  await assert.rejects(h.context.ACTIONS.codexAPISave({ dataset: { id: 'fixture' } }), /1–128/);
  assert.equal(calls, 0);
});

test('primary API binding opens the editor and account details remain inline', () => {
  const h = harness(async () => ({}));
  const html = h.run(`officialAccountCard({
    id: 'fixture-account', name: 'Fixture',
    officialAccount: { id: 'fixture-account', mode:'legacy_codex_cli', status: 'ready', usage: { source:'openai_codex_usage_api', verified: true, status: 'fresh', freshUntil:'2099-01-01T00:00:00Z', rateLimit:{limitReached:true}, resetCreditsAvailable: 2 } },
    apiService: { clients: [{ clientId: 'fixture-client', primary: true, text: true, image: true }] }
  }, [], (body) => body)`);
  assert.match(html, /data-act="apiCardMembership" data-id="fixture-account"/);
  assert.match(html, />管理 API 服务</);
  assert.doesNotMatch(html, /data-act="officialAPIMember"/);
  assert.match(html, /官方重置次数<\/span><b>2/);
  assert.match(html, /data-act="officialResetCredit"[^>]*>消耗官方重置次数（2）/);
  assert.match(html, /data-act="officialUsageReset"/);
  assert.doesNotMatch(html, /officialDetails|额度与账号详情|<details/);
  assert.equal(h.context.ACTIONS.officialDetails, undefined, 'retired details modal must not reopen');
});

test('healthy account footer is minimal while actionable recovery remains visible only when needed', () => {
  const h = harness(async () => ({}));
  const render = (status, mode = 'legacy_codex_cli', libraryOnly = false) => {
    h.context.compactAccount = { id:'fixture',name:'Fixture',libraryOnly,officialAccount:{id:'vault',status,mode} };
    return h.run('officialAccountCard(compactAccount,[],()=>"")').match(/<div class="official-card-actions">(.*?)<\/div>/s)[1];
  };
  const ready = render('ready');
  for(const action of ['accountTest','officialRename','officialUsageRefresh','officialDelete']) assert.match(ready,new RegExp(`data-act="${action}"`));
  assert.equal((ready.match(/<button/g)||[]).length,4);
  assert.doesNotMatch(ready,/officialRelogin|officialRefresh"|officialPool|officialUsageReset|officialReimport/);
  for(const status of ['needs_login','refresh_failed']) assert.match(render(status),/officialRelogin/);
  assert.match(render('expired'),/officialRefresh/);
  assert.match(render('expired','chatgpt_web_session'),/officialRelogin/);
  assert.match(render('save_failed'),/officialRetrySave/);
  assert.match(render('ready','legacy_codex_cli',true),/officialAttach/);
  assert.doesNotMatch(render('ready','chatgpt_web_session',true),/officialAttach/);
});

test('card maintenance entries open pool settings with the local ID and clear only the credential cache', async () => {
  const calls = [];
  const account = { id: 'official-local', name: 'Fixture', poolGroup: 'coding', priority: 7,
    officialAccount: { id: 'vault-account', status: 'ready', mode: 'legacy_codex_cli' } };
  const h = harness(async (path, body) => {
    calls.push({ path, body });
    if (path === '/api/local/accounts') return { accounts: [account] };
    if (path === '/api/local/codex/usage/reset') return { ok: true };
    throw Error('unexpected maintenance request: ' + path);
  });
  h.context.cardAccount = account;
  const html = h.run('officialAccountCard(cardAccount, [], body => body)');
  const button = (action) => {
    const match = html.match(new RegExp(`data-act="${action}" data-id="([^"]+)"`));
    assert.ok(match, action + ' must be reachable from the card');
    return { dataset: { id: match[1] } };
  };
  await h.context.ACTIONS.officialPool(button('officialPool'));
  assert.match(h.node('#officialPoolContent').innerHTML, /data-act="officialPoolSave" data-id="official-local"/);
  assert.match(h.node('#officialPoolContent').innerHTML, /id="officialPoolGroup"[^>]*value="coding"/);
  h.context.confirm = () => false;
  await h.context.ACTIONS.officialUsageReset(button('officialUsageReset'));
  assert.equal(calls.length, 1, 'cancelled cache reset must not write');
  h.context.confirm = () => true;
  await h.context.ACTIONS.officialUsageReset(button('officialUsageReset'));
  assert.deepEqual(JSON.parse(JSON.stringify(calls[1])), { path: '/api/local/codex/usage/reset', body: { id: 'vault-account' } });
  assert.ok(calls.every(({ path }) => !path.includes('/reset-credit/')), 'local cache maintenance must never consume official reset credits');
});

test('unlinked and web-session cards retain cache maintenance without offering an unusable pool editor', () => {
  const h = harness(async () => ({}));
  for (const [libraryOnly, mode] of [[true, 'legacy_codex_cli'], [true, 'chatgpt_web_session'], [false, 'chatgpt_web_session']]) {
    h.context.cardAccount = { id: 'local', name: 'Fixture', libraryOnly, officialAccount: { id: 'vault', status: 'ready', mode } };
    const html = h.run('officialAccountCard(cardAccount, [], body => body)');
    assert.doesNotMatch(html, /data-act="officialPool"/);
    assert.match(html, /data-act="officialUsageReset" data-id="vault"/);
  }
});

test('browser authorization and every account import mode expose controls without nested folds', async () => {
  const h = harness(async () => ({ accounts: [{ id: 'fixture-account', officialAccount: { id: 'fixture' } }] }));
  const browser = h.run(`officialLoginProgressHTML({id:'fixture-session',mode:'browser',status:'pending',authorizationURL:'https://auth.openai.com/oauth/authorize?state=fixture',expiresAt:'2099-01-01T00:00:00Z'})`);
  assert.match(browser, /id="officialAuthorizationURL"/);
  assert.match(browser, /id="officialCallbackURL"/);
  assert.match(browser, /data-act="officialLoginCallback"/);
  assert.doesNotMatch(browser, /<details|<summary|hidden/);
  for (const mode of ['openOfficialAccounts()', 'openOfficialPaste()', 'openOfficialFiles()', "openOfficialPaste('fixture')", "openOfficialFiles('fixture')"]) {
    await h.run(mode);
    const html = h.node('#modalRoot').innerHTML;
    assert.match(html, /role="tablist"/);
    assert.doesNotMatch(html, /<details|<summary|更多操作/);
  }
  await h.run("openOfficialPool('fixture-account')");
  assert.match(h.node('#officialPoolContent').innerHTML, /id="officialPoolStrategy"/);
  assert.doesNotMatch(h.node('#officialPoolContent').innerHTML, /<details|<summary/);
  h.run("setOfficialImportPreview({accounts:[{name:'Fixture',mode:'legacy_codex_cli'}]})");
  assert.match(h.node('#officialImportPreview').innerHTML, /检测可用性|剔除失效|导入 1 个账号/);
  assert.doesNotMatch(h.node('#officialImportPreview').innerHTML, /<details|<summary/);
});

test('inline quota details retain additional limit identity without duplicating main quota', () => {
  const h = harness(async () => ({}));
  h.context.esc = value => String(value).replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('"', '&quot;');
  const html = h.run(`officialUsageDetails({usage:{verified:true,status:'fresh',rateLimit:{primary:{remainingPercent:91,windowMinutes:300}},codeReviewRateLimit:{primary:{remainingPercent:28,windowMinutes:10080}},additionalRateLimits:[{normalModelSlug:'synthetic<img>',primary:{remainingPercent:64,windowMinutes:300,resetsAt:'2099-01-01T00:00:00Z'}}],spendControl:{reached:true},errorCode:'synthetic<img>'}})`);
  assert.match(html, /代码审查 · 周额度/);
  assert.match(html, /28%|64%/);
  assert.match(html, /模型 synthetic&lt;img> · 5 小时额度/);
  assert.match(html, /已达消费上限|最近刷新：synthetic&lt;img>/);
  assert.doesNotMatch(html, /91%|<img>|<details/);
});

test('official account dollar projections stay hidden even when retained history has estimates', () => {
  const h = harness(async () => ({}));
  for (const info of [{}, { costEstimate: { status: 'estimated', totalUSD: 20, windowUsedUSD: 8, remainingUSD: 12, freshUntil: '2099-01-01T00:00:00Z' } }, { plan: 'pro', usage: { credits: { balance: '999' } } }]) {
    h.context.costInfo = info;
    assert.equal(h.run('officialCostEstimateHTML(costInfo)'), '');
  }
});

test('quota cards show percentages and reset times without API-equivalent dollar values', () => {
  const h = harness(async () => ({}));
  const html = h.run(`officialAccountCard({id:'fixture',name:'Fixture',officialAccount:{id:'fixture',status:'ready',usage:{verified:true,status:'fresh',freshUntil:'2099-01-01T00:00:00Z',rateLimit:{primary:{windowMinutes:300,remainingPercent:80,resetsAt:'2099-01-01T00:00:00Z'}}},costEstimate:{windows:{primary:{status:'partial',freshUntil:'2099-01-01T00:00:00Z',windowResetAt:'2099-01-01T00:00:00Z',windowSeconds:18000,windowUsedUSD:0.000012,totalUSD:0.00006,remainingUSD:0.000048}}}}},[],()=>"")`);
  assert.doesNotMatch(html, /\$|official-quota-cost|本窗已用|剩余等值/);
  assert.match(html, /official-quota-percent|80%|重置于/);
  assert.doesNotMatch(html, /officialDetails|<details|class="official-card-costs"/);
});

test('quota bars use actual windows, never invent a five-hour limit from a plan', () => {
  const h = harness(async () => ({}));
  for (const plan of ['business', 'pro', 'plus', 'enterprise', 'future-plan']) {
    h.context.quotaInfo = { plan, usage: { verified:true,status:'fresh',freshUntil:'2099-01-01T00:00:00Z',rateLimit:{secondary:{windowSeconds:604800,remainingPercent:70,resetsAt:'2099-01-01T00:00:00Z'}}},costEstimate:{windows:{secondary:{status:'partial',freshUntil:'2099-01-01T00:00:00Z',windowResetAt:'2099-01-01T00:00:00Z',windowSeconds:604800,windowUsedUSD:3,totalUSD:10,remainingUSD:7},primary:{status:'partial',freshUntil:'2099-01-01T00:00:00Z',windowResetAt:'2099-01-01T00:00:00Z',windowSeconds:18000,windowUsedUSD:1,totalUSD:5}}} };
    let html = h.run('officialQuotaHTML(quotaInfo)');
    assert.match(html, /周额度|剩余 70%/); assert.doesNotMatch(html,/5 小时|\$|official-quota-cost/);
    h.context.quotaInfo.usage.rateLimit = {};
    html = h.run('officialQuotaHTML(quotaInfo)');
    assert.doesNotMatch(html, /official-quota-track|\$|5 小时|100%/);
  }
});

test('inline estimates are isolated by window length/reset and hide stale or unknown amounts', () => {
  const h = harness(async () => ({}));
  const current = {windowMinutes:300,remainingPercent:80,resetsAt:'2099-01-01T00:00:00Z'};
  const estimate = {status:'partial',freshUntil:'2099-01-01T00:00:00Z',windowResetAt:current.resetsAt,windowSeconds:18000,windowUsedUSD:2,totalUSD:10};
  for(const change of [{windowSeconds:604800},{windowResetAt:'2099-01-02T00:00:00Z'},{freshUntil:'2000-01-01T00:00:00Z'},{status:'unknown'},{totalUSD:null}]) {
    h.context.quotaInfo={usage:{verified:true,status:'fresh',freshUntil:'2099-01-01T00:00:00Z',rateLimit:{primary:current}},costEstimate:{windows:{primary:{...estimate,...change}}}};
    const html=h.run('officialQuotaHTML(quotaInfo)');
    assert.match(html,/5 小时额度/);assert.doesNotMatch(html,/official-quota-cost|\$\d/);
  }
});

test('reset-credit confirmation cannot consume after another mutation wins', async () => {
  const calls = [];
  const h = harness(async (path) => {
    calls.push(path);
    if (path.endsWith('/reset-credit/prepare')) return { request: { state: 'prepared', redeemRequestId: 'redeem-1' } };
    throw new Error(`stale confirmation reached ${path}`);
  });
  // A second mutation may finish while the native confirmation dialog is open.
  h.context.confirm = () => { h.run('OFFICIAL_PANEL.mutation += 1'); return true; };
  await h.context.ACTIONS.officialResetCredit({ dataset: { id: 'fixture-account' } });
  assert.deepEqual(calls, ['/api/local/codex/reset-credit/prepare']);
});

test('legacy official membership delegates the exact local ID without an unsafe direct write', async () => {
  const h = harness(async () => { throw Error('legacy membership wrote directly'); });
  const received = [];
  h.context.ACTIONS.apiCardMembership = async button => received.push(button.dataset.id);
  await h.context.ACTIONS.officialAPIMember({ dataset: { id:'vault-account', accountId:'official-local' } });
  await h.context.ACTIONS.officialAPIMember({ dataset: { id:'official-local' } });
  assert.deepEqual(received, ['official-local', 'official-local']);
});

for (const action of ['officialAttach', 'officialRefresh', 'officialUsageRefresh', 'officialUsageReset', 'officialResetCredit', 'officialPoolSave', 'officialDelete']) {
  test(`late ${action} result cannot reopen another account panel`, async () => {
    const completed = deferred(); let listReads = 0;
    const h = harness(async (path) => {
      if (path === '/api/local/accounts') return { accounts: [] };
      if (path === '/api/local/codex/accounts') { listReads++; return { accounts: [] }; }
      return completed.promise;
    });
    h.node('#officialQuotaReserve').value = '25';
    h.node('#officialPoolEnabled').value = 'true';
    h.node('#officialPoolGroup').value = 'fixture';
    const pending = h.context.ACTIONS[action]({ dataset: { id: 'fixture-account' } });
    h.run('disposeOfficialPanel()');
    const closedEpoch = h.run('OFFICIAL_PANEL.epoch');
    completed.resolve({ ok: true });
    await pending;
    assert.equal(h.run('OFFICIAL_PANEL.epoch'), closedEpoch, 'completed action opened a new panel');
    assert.equal(listReads, 0, 'closed panel queried account rows again');
  });
}

test('older account mutation cannot repaint after a newer mutation completes', async () => {
  const older = deferred(); let refreshes = 0, listReads = 0;
  const h = harness(async (path) => {
    if (path === '/api/local/codex/refresh') return ++refreshes === 1 ? older.promise : { ok: true };
    if (path === '/api/local/accounts') return { accounts: [] };
    if (path === '/api/local/codex/accounts') { listReads++; return { accounts: [] }; }
    throw new Error(`unexpected endpoint ${path}`);
  });
  h.context.S.route = 'accounts';
  const first = h.context.ACTIONS.officialRefresh({ dataset: { id: 'old' } });
  const second = h.context.ACTIONS.officialRefresh({ dataset: { id: 'new' } });
  await second;
  const latestEpoch = h.run('OFFICIAL_PANEL.epoch');
  older.resolve({ ok: true });
  await first;
  assert.equal(h.run('OFFICIAL_PANEL.epoch'), latestEpoch, 'older response reopened the panel');
  assert.equal(listReads, 1, 'stale response performed an extra account read');
});

test('editing quota reserve while save waits preserves the new draft', async () => {
  const completed = deferred();
  const h = harness(async (path) => path === '/api/local/accounts' ? { accounts: [] } : completed.promise);
  h.node('#officialQuotaReserve').value = '25';
  h.node('#officialPoolEnabled').value = 'true';
  h.node('#officialPoolGroup').value = 'fixture';
  const pending = h.context.ACTIONS.officialPoolSave({ dataset: { id: 'fixture-account' } });
  h.node('#officialQuotaReserve').value = '50';
  completed.resolve({ ok: true });
  await pending;
  assert.equal(h.run('OFFICIAL_PANEL.epoch'), 0, 'old save replaced the new draft');
  assert.equal(h.node('#officialQuotaReserve').value, '50');
});

test('pool policy fields are validated, sent, and a changed draft survives a late save', async () => {
  const completed = deferred(); let sent;
  const h = harness(async (path, body) => {
    if (path === '/api/local/accounts/pool') { sent = body; return completed.promise; }
    if (path === '/api/local/accounts') return { accounts: [] };
    throw new Error('unexpected endpoint');
  });
  Object.assign(h.node('#officialQuotaReserve'), { value: '15' });
  Object.assign(h.node('#officialPoolEnabled'), { value: 'true' });
  Object.assign(h.node('#officialPoolGroup'), { value: 'coding' });
  Object.assign(h.node('#officialPoolStrategy'), { value: 'custom' });
  Object.assign(h.node('#officialPoolPriority'), { value: '7' });
  Object.assign(h.node('#officialPoolWeight'), { value: '3' });
  Object.assign(h.node('#officialPoolBackup'), { value: 'true' });
  Object.assign(h.node('#officialPoolConcurrency'), { value: '4' });
  const pending = h.context.ACTIONS.officialPoolSave({ dataset: { id: 'fixture-account' } });
  h.node('#officialPoolPriority').value = '8';
  completed.resolve({ ok: true });
  await pending;
  assert.deepEqual(JSON.parse(JSON.stringify(sent)), { id: 'fixture-account', enabled: true, group: 'coding', strategy: 'custom', priority: 7, weight: 3, backup: true, maxConcurrency: 4, quotaReservePercent: 15 });
  assert.equal(h.node('#officialPoolPriority').value, '8');
});

test('Codex account add and import methods share the compact key-library dialog shell', async () => {
  const h = harness(async () => ({}));
  await h.context.ACTIONS.accountAddTab({ dataset: { tab: 'codex' } });
  let html = h.node('#modalRoot').innerHTML;
  assert.match(html, /class="modal account-modal key-editor account-add-modal" data-key-editor/);
  assert.match(html, /id="officialTitle">添加凭据<\/h3>/);
  assert.match(html, /id="accountAdd-codex"/);
  assert.match(html, /id="officialTab-oauth"/);
  assert.match(html, /只保存在这台电脑/);
  assert.doesNotMatch(html, /width:\s*680px/);

  h.context.ACTIONS.officialTab({ dataset: { tab: 'token' } });
  html = h.node('#modalRoot').innerHTML;
  assert.match(html, /class="modal account-modal key-editor account-add-modal" data-key-editor/);
  assert.match(html, /id="officialPasteText"/);
  assert.match(html, /id="accountAdd-codex"/);
  assert.match(html, /id="officialTab-token"[^>]*class="active"/);
});

test('switching add-account types respects the API draft guard and selecting API again preserves it', async () => {
  const h = harness(async () => ({}));
  h.context.S.localEditorDraft = { input: h.node('#laID') };
  let closes = 0, opens = 0;
  h.context.closeLocalEditor = async () => { closes++; return false; };
  h.context.openOfficialAccounts = () => { opens++; };
  await h.context.ACTIONS.accountAddTab({ dataset: { tab: 'apikey' } });
  assert.equal(closes, 0); assert.equal(opens, 0);
  await h.context.ACTIONS.accountAddTab({ dataset: { tab: 'codex' } });
  assert.equal(closes, 1); assert.equal(opens, 0);
  h.context.closeLocalEditor = async () => { closes++; return true; };
  await h.context.ACTIONS.accountAddTab({ dataset: { tab: 'codex' } });
  assert.equal(closes, 2); assert.equal(opens, 1);
});
