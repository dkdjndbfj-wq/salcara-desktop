import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

const code = await Promise.all(['usage-data.js', 'usage.js'].map(name => readFile(new URL('../internal/console/web/' + name, import.meta.url), 'utf8')));
const esc = value => String(value ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const options = [
  {id:'api-a',name:'同名来源',host:'same.example.test',kind:'api'},
  {id:'api-b',name:'同名来源',host:'same.example.test',kind:'api'},
  {id:'official-a',name:'工作账号',host:'',kind:'official'},
];
const activity = (count, model = 'model-' + count) => ({remaining:count, daily_usage:[{date:'2026-10-14',total_tokens:count,requests:1}],today:{total_tokens:count,requests:1},model_stats:[{model,total_tokens:count,requests:1}]});
function payload(id = '', count = 10, days = 14) {
  return {days,rangeEnd:'2026-10-14',selectedSource:id,options,sources:[{...options.find(o => o.id === (id || 'api-a')),ok:true,usage:activity(count)}]};
}
function harness({ lifecycle = false } = {}) {
  const nodes = new Map(), calls = [], queue = [], listeners = new Map(), timers = new Map();
  let now = new Date(2026, 9, 14, 23, 59, 50).getTime(), timerID = 0;
  class Clock extends Date { constructor(...args) { super(...(args.length ? args : [now])); } static now() { return now; } }
  const listen = (name, fn) => { const handlers = listeners.get(name) || []; handlers.push(fn); listeners.set(name, handlers); };
  const document = { hidden: false, addEventListener: listen };
  const make = () => ({innerHTML:'',value:'',dataset:{},events:{},classes:new Set(),style:{},clientWidth:640,addEventListener(type,fn){this.events[type]=fn;},setAttribute(){},classList:{add(){},remove(){}}});
  for (const key of ['#headActions','#usSource','#usRefresh','#view','#usChart']) nodes.set(key, make());
  const tabs = [7,14,30].map(days => ({...make(),dataset:{days:String(days)}}));
  const chart = nodes.get('#usChart'), parts = new Map(['.us-guide','.us-dot','.us-hit','.us-tip'].map(key => [key,make()]));
  chart.querySelector = selector => selector === 'svg' ? {querySelector:key=>parts.get(key),getBoundingClientRect:()=>({left:0,width:640,height:230})} : parts.get(selector);
  const state = {route:'usage',routeRevision:1};
  const context = vm.createContext({Date:lifecycle?Clock:Date,AbortController,
    setTimeout:lifecycle?(fn,delay=0)=>{const id=++timerID;timers.set(id,{fn,at:now+delay});return id;}:setTimeout,
    clearTimeout:lifecycle?id=>timers.delete(id):clearTimeout,...(lifecycle?{document}:{}),
    console,S:state,RENDER_usage:null,$:key=>nodes.get(key),$$:()=>tabs,icon:()=>'',ico:()=>'',esc,addEventListener:listen,fetch:async(url,init)=>{calls.push({url,init});return queue.length ? queue.shift() : {ok:true,json:async()=>payload()};}});
  for (const script of code) vm.runInContext(script,context);
  const viewState = vm.runInContext('USAGE_VIEW',context);
  context.usageHead();
  const settle = async () => { for(let i=0;i<12;i++) await Promise.resolve(); };
  return {context,nodes,calls,state,viewState,document,settle,
    emit(name,event={}){for(const listener of listeners.get(name)||[])listener(event);},
    async advance(ms){now+=ms;for(const [id,row]of [...timers])if(row.at<=now&&timers.delete(id))row.fn();await settle();},
    summarize:vm.runInContext('USAGE.summarize',context),
    fmtMoney:vm.runInContext('USAGE.fmtMoney',context),
    dataLoad:vm.runInContext('USAGE.load',context),
    render(data){context.drawUsage(nodes.get('#view'),data);return nodes.get('#view').innerHTML;},
    pause(){let resolve;const promise=new Promise(r=>resolve=r);queue.push(promise);return {resolve(body,status=200){resolve({ok:status<400,status,json:async()=>body});}};},
    choose(id){nodes.get('#usSource').value=id;return nodes.get('#usSource').events.change({target:nodes.get('#usSource')});},
    days(n){const button=tabs.find(b=>b.dataset.days===String(n));return button.events.click();},
    html:()=>nodes.get('#view').innerHTML,
  };
}

test('same host and name remain two exact choices with all metrics scoped to the chosen ID', async () => {
  const h=harness(), a=h.pause();const initial=h.context.RENDER_usage(h.nodes.get('#view'));a.resolve(payload());await initial;
  assert.match(h.nodes.get('#usSource').innerHTML,/api-a/);assert.match(h.nodes.get('#usSource').innerHTML,/api-b/);assert.match(h.nodes.get('#usSource').innerHTML,/工作账号/);
  const b=h.pause(), switching=h.choose('api-b');assert.doesNotMatch(h.html(),/model-10/);b.resolve(payload('api-b',222));await switching;
  assert.match(h.calls.at(-1).url,/source=api-b/);assert.equal(h.viewState.data.balance.value,222);
  assert.equal(h.viewState.data.today.tokens,222);assert.equal(h.viewState.data.rangeTokens,222);assert.equal(h.viewState.data.models[0].model,'model-222');
  assert.equal(h.viewState.data.sources.length,1);assert.doesNotMatch(h.html(),/model-10|消费|花费/);
});

test('official selection uses only its owned ledger and no upstream money or full-machine fallback', () => {
  const h=harness(), p=payload('official-a');p.sources=[{...options[2],ok:true,usage:{},localUsage:activity(42)}, {...options[1],ok:true,usage:activity(900)}, {id:'local',kind:'local',ok:true,usage:activity(800)}];
  const result=h.summarize(p);assert.equal(result.sources.length,1);assert.equal(result.balance,null);assert.equal(result.rangeTokens,42);assert.equal(result.today.tokens,42);
  assert.equal(result.models[0].tokens,42);assert.equal(result.curveLocal,true);assert.equal(result.modelsLocal,true);assert.equal(result.todayLocal,true);
  assert.match(h.render(result),/该账号的本机记录/);assert.doesNotMatch(h.html(),/model-900|model-800|\$42/);
});

test('single source with no statistics cannot inherit another source or global records', () => {
  const h=harness(),p=payload('api-a');p.sources=[{...options[0],ok:true,usage:{remaining:5}},{...options[1],ok:true,usage:activity(900)},{id:'local',kind:'local',ok:true,usage:activity(800)}];
  const d=h.summarize(p);assert.equal(d.balance.value,5);assert.equal(d.rangeTokens,null);assert.equal(d.today,null);assert.equal(d.models.length,0);assert.equal(d.hasDaily,false);
});

test('account ledger fallback fills missing fields without adding its copy of upstream Tokens', () => {
  const h=harness(),p=payload('api-a',20);p.sources[0].localUsage=activity(200);delete p.sources[0].usage.model_stats;
  const d=h.summarize(p);assert.equal(d.rangeTokens,20);assert.equal(d.today.tokens,20);assert.equal(d.models[0].tokens,200);assert.equal(d.modelsLocal,true);assert.equal(d.curveLocal,false);assert.equal(d.balance.value,20);
});

test('an online relay without a balance cannot mask a real saved API balance', () => {
  const h=harness(),p=payload('',20);p.sources.unshift({id:'relay',name:'中转站',kind:'relay',ok:true,usage:{today:{total_tokens:3,requests:1}}});
  const d=h.summarize(p);assert.equal(d.balance.value,20);assert.equal(d.balance.name,'同名来源');assert.equal(d.today.tokens,23);
  p.sources[0].usage.remaining=0;assert.equal(h.summarize(p).balance.value,0,'a real zero relay balance still takes precedence');
});

test('balances preserve cents and normalize currency units without turning unknown values into zero', () => {
  const h=harness();
  assert.equal(h.fmtMoney(123.45,'USD'),'$123.45');
  assert.equal(h.fmtMoney(-123.45,'USD'),'$-123.45');
  assert.equal(h.fmtMoney(123.45,' usd '),'$123.45');
  assert.equal(h.fmtMoney(123.45,' cny '),'123.45 CNY');
  assert.equal(h.fmtMoney(123.45,'   '),'$123.45');
  assert.equal(h.fmtMoney(0,'USD'),'$0.00');
  assert.equal(h.fmtMoney('0','USD'),'$0.00');
  for(const value of [null,undefined,false,'not a number']) assert.equal(h.fmtMoney(value,'USD'),'—');
});

test('real zero balances render as money while absent balances remain explicitly unavailable', () => {
  const h=harness();
  for(const remaining of [0,'0',null]) {
    const p=payload('api-a');p.sources[0].usage={remaining};
    const d=h.summarize(p),html=h.render(d);
    if(remaining===null) {
      assert.equal(d.balance,null);assert.match(html,/服务商未返回余额/);assert.doesNotMatch(html,/\$0\.00/);
    } else {
      assert.equal(d.balance.value,0);assert.match(html,/\$0\.00/);assert.doesNotMatch(html,/服务商未返回余额/);
    }
  }
});

test('multi-account overview identifies the chosen balance source and never calls it an aggregate', () => {
  const h=harness(),p=payload();
  p.sources=[{...options[0],name:'开发账号',ok:true,usage:{remaining:12.34,unit:'USD'}},{...options[1],name:'备用账号',ok:true,usage:{remaining:99.50,unit:'CNY'}}];
  const d=h.summarize(p),html=h.render(d);
  assert.equal(d.balance.value,12.34);assert.equal(d.balance.name,'开发账号');
  assert.match(html,/来源余额/);assert.match(html,/title="开发账号 · same\.example\.test">开发账号/);
  assert.doesNotMatch(html,/总余额|\$111\.84/);
  p.selectedSource='api-b';const selected=h.summarize(p);
  assert.equal(selected.balance.value,99.50);assert.equal(selected.balance.unit,'CNY');
  const selectedHTML=h.render(selected);assert.match(selectedHTML,/剩余金额/);assert.doesNotMatch(selectedHTML,/来源余额|开发账号|\$12\.34/);
});

test('balance cards and source rows render provider names, hosts and currency units as inert text', () => {
  const h=harness(),p=payload('api-a');
  const name='开发 "账号" <img src=x>',host='api.test"><script>bad</script>',unit='<svg onload=&#97;&#108;&#101;&#114;&#116;(1)>';
  p.sources[0]={...p.sources[0],name,host,usage:{remaining:123.45,unit}};
  const html=h.render(h.summarize(p));
  assert.match(html,/开发 &quot;账号&quot; &lt;img src=x&gt;/);
  assert.match(html,/api\.test&quot;&gt;&lt;script&gt;bad&lt;\/script&gt;/);
  const inertUnit='&lt;SVG ONLOAD=&amp;#97;&amp;#108;&amp;#101;&amp;#114;&amp;#116;(1)&gt;';
  assert.ok(html.split(inertUnit).length>=4,'the card text, card tooltip and source-row balance all escape the unit');
  assert.doesNotMatch(html,/<img\b|<script\b|<svg onload/i);
});

test('missing Token observations remain unknown, including trend gaps and model rows', () => {
  const h=harness(),p=payload('official-a');p.sources[0]={...options[2],ok:true,localUsage:{daily_usage:[{date:'2026-10-13',total_tokens:10,requests:1},{date:'2026-10-14',total_tokens:null,requests:2}],today:{total_tokens:null,requests:2},model_stats:[{model:'unknown-model',total_tokens:null,requests:2}]}};
  const d=h.summarize(p);assert.equal(d.today.tokens,null);assert.equal(d.rangeTokens,null);assert.equal(d.series.at(-1).tokens,null);assert.equal(d.models[0].tokens,null);
  assert.match(h.render(d),/unknown-model/);assert.doesNotMatch(h.html(),/NaN|width:0\.0%/);
});

test('partial token fields stay unknown and cached subcounts do not inflate a complete base total', () => {
  const h=harness();
  for (const [observation,expected] of [[{total_tokens:null,input_tokens:7,output_tokens:null},null],[{input_tokens:7},null],[{input_tokens:10,output_tokens:5,cache_read_tokens:8,reasoning_tokens:3},15]]) {
    const p=payload('api-a',1,1);p.sources[0].usage={today:observation,daily_usage:[{date:'2026-10-14',...observation}],model_stats:[{model:'partial-fields',...observation}]};
    const d=h.summarize(p);assert.equal(d.today.tokens,expected);assert.equal(d.rangeTokens,expected);assert.equal(d.models[0].tokens,expected);
  }
});

test('out-of-range upstream rows cannot block owned fallback or claim a known zero', () => {
  const h=harness(),p=payload('api-a',20,1);p.sources[0].usage.daily_usage=[{date:'2026-09-01',total_tokens:20}];
  let d=h.summarize(p);assert.equal(d.rangeTokens,null);assert.equal(d.sources[0].range.known,false);assert.equal(d.sources[0].range.tokens,null);
  p.sources[0].localUsage=activity(9);d=h.summarize(p);assert.equal(d.rangeTokens,9);assert.equal(d.sources[0].range.tokens,9);assert.equal(d.curveLocal,true);
});

test('response rangeEnd controls day axis independently of browser clock', () => {
  const h=harness(),p=payload('api-a',20,3);p.rangeEnd='2024-03-01';p.sources[0].usage.daily_usage=[{date:'2024-02-29',total_tokens:21},{date:'2024-03-01',total_tokens:22}];
  const d=h.summarize(p);assert.equal(d.series[0].date,'2024-02-28');assert.equal(d.series[1].date,'2024-02-29');assert.equal(d.rangeTokens,43);
});

test('pending or incomplete owned records retain their partial label', () => {
  const h=harness(),p=payload('official-a');p.sources[0]={...options[2],ok:true,usage:{},localUsage:{...activity(0),partial:true}};
  const d=h.summarize(p);assert.equal(d.partial,true);assert.match(h.render(d),/部分记录/);assert.equal(d.balance,null);
});

test('rapid A to B ignores late A success and aborts its request', async () => {
  const h=harness(),a=h.pause(),first=h.choose('api-a'),b=h.pause(),second=h.choose('api-b');
  assert.equal(h.calls[0].init.signal.aborted,true);b.resolve(payload('api-b',22));await second;a.resolve(payload('api-a',11));await first;
  assert.equal(h.viewState.data.selectedSource,'api-b');assert.match(h.html(),/model-22/);assert.doesNotMatch(h.html(),/model-11/);
});

test('late failure cannot replace a newer scope and failed selection retains no old numbers', async () => {
  const h=harness(),a=h.pause(),first=h.choose('api-a'),b=h.pause(),second=h.choose('api-b');
  b.resolve(payload('api-b',22));await second;a.resolve({error:'late A error'},503);await first;assert.doesNotMatch(h.html(),/late A error/);
  const failure=h.pause(),third=h.choose('official-a');failure.resolve({error:'本机记录读取失败',options},503);await third;
  assert.match(h.html(),/本机记录读取失败/);assert.doesNotMatch(h.html(),/model-22|\$22/);assert.equal(h.viewState.source,'official-a');assert.equal(h.viewState.data,null);
});

test('deleted selection stays explicit with remaining choices instead of becoming overview', async () => {
  const h=harness(),p=h.pause(),request=h.choose('api-a');p.resolve({error:'用量来源已不存在',options:options.slice(1)},404);await request;
  assert.equal(h.viewState.source,'api-a');assert.match(h.nodes.get('#usSource').innerHTML,/来源已不可用/);assert.match(h.nodes.get('#usSource').innerHTML,/api-b/);assert.match(h.html(),/已不存在/);
});

test('date changes use a new request and do not show the old period', async () => {
  const h=harness(),a=h.pause(),first=h.choose('api-a'),b=h.pause(),second=h.days(30);
  b.resolve(payload('api-a',30,30));await second;a.resolve(payload('api-a',14,14));await first;
  assert.match(h.calls[1].url,/days=30/);assert.equal(h.viewState.data.days,30);assert.match(h.html(),/近 30 天 Token/);assert.doesNotMatch(h.html(),/model-14/);
});

test('navigation epoch prevents both old success and failure from painting a new page', async () => {
  for (const failed of [false,true]) {
    const h=harness(),p=h.pause(),request=h.choose('api-a');h.state.route='accounts';h.state.routeRevision++;h.nodes.get('#view').innerHTML='account page';
    p.resolve(failed?{error:'old error'}:payload('api-a'),failed?503:200);await request;assert.equal(h.html(),'account page');
  }
});

test('reentering usage does not flash cached credentials data before a fresh read', async () => {
  const h=harness(),a=h.pause(),first=h.choose('api-a');a.resolve(payload('api-a',13));await first;h.state.routeRevision++;
  const b=h.pause(),next=h.context.RENDER_usage(h.nodes.get('#view'));assert.doesNotMatch(h.html(),/model-13|\$13/);b.resolve(payload('api-a',14));await next;assert.match(h.html(),/model-14/);
});

test('loader rejects mismatched scope even if an otherwise valid total was returned', async () => {
  const h=harness(),p=h.pause(),request=h.choose('api-b');p.resolve(payload('',999));await request;
  assert.match(h.html(),/来源与当前选择不一致/);assert.equal(h.viewState.data,null);
});

test('loader rejects a cached response for another date range', async () => {
  const h=harness(),p=h.pause(),request=h.choose('api-a');p.resolve(payload('api-a',999,7));await request;
  assert.match(h.html(),/时间范围与当前选择不一致/);assert.equal(h.viewState.data,null);
});

test('ledger error is visible with unknown activity while the selected balance remains real', () => {
  const h=harness(),p=payload('api-a');p.sources[0]={...options[0],ok:true,usage:{remaining:5},localUsageError:'本机记录读取失败'};
  const d=h.summarize(p);assert.equal(d.today,null);assert.equal(d.rangeTokens,null);assert.match(h.render(d),/本机记录读取失败/);assert.match(h.html(),/\$5\.00/);
});

test('hiding aborts the pending selection and its late reply cannot paint after showing again', async () => {
  for (const failed of [false,true]) {
    const h=harness({lifecycle:true}),old=h.pause(),reading=h.choose('api-a');
    h.document.hidden=true;h.emit('visibilitychange');
    assert.equal(h.calls[0].init.signal.aborted,true);assert.equal(h.viewState.data,null);
    const fresh=h.pause();h.document.hidden=false;h.emit('visibilitychange');
    assert.equal(h.calls.length,2);assert.match(h.calls[1].url,/source=api-a/);
    fresh.resolve(payload('api-a',22));await h.settle();
    old.resolve(failed?{error:'old hidden failure'}:payload('api-a',11),failed?503:200);await reading;
    assert.match(h.html(),/model-22/);assert.doesNotMatch(h.html(),/model-11|old hidden failure/);
  }
});

test('returning to a visible window clears old account data before checking changed credentials', async () => {
  const h=harness({lifecycle:true}),first=h.pause(),reading=h.choose('api-a');first.resolve(payload('api-a',23));await reading;
  assert.match(h.html(),/model-23/);
  h.document.hidden=true;h.emit('visibilitychange');assert.doesNotMatch(h.html(),/model-23|\$23/);
  await h.advance(60000);assert.equal(h.calls.length,1,'hidden windows do not refresh at midnight');
  const next=h.pause();h.document.hidden=false;h.emit('visibilitychange');
  next.resolve({error:'账号身份已变化',options},409);await h.settle();
  assert.match(h.html(),/账号身份已变化/);assert.equal(h.viewState.source,'api-a');assert.equal(h.viewState.data,null);
});

test('a page opened while hidden waits for visibility and never leaves a timeout error', async () => {
  const h=harness({lifecycle:true});h.document.hidden=true;await h.choose('official-a');
  await h.advance(60000);assert.equal(h.calls.length,0);assert.doesNotMatch(h.html(),/超时/);
  const next=h.pause();h.document.hidden=false;h.emit('visibilitychange');
  next.resolve(payload('official-a',17));await h.settle();assert.equal(h.calls.length,1);assert.match(h.html(),/model-17/);
});

test('visible midnight refresh preserves the exact selection and replaces yesterday totals', async () => {
  const h=harness({lifecycle:true}),first=h.pause(),reading=h.choose('api-b');first.resolve(payload('api-b',24));await reading;
  const next=h.pause();await h.advance(10050);
  assert.equal(h.calls.length,2);assert.match(h.calls[1].url,/source=api-b/);assert.doesNotMatch(h.html(),/model-24|\$24/);
  const current=payload('api-b',0);current.rangeEnd='2026-10-15';current.sources[0].usage.daily_usage[0].date='2026-10-15';
  next.resolve(current);await h.settle();assert.equal(h.viewState.data.today.tokens,0);assert.equal(h.viewState.data.series.at(-1).date,'2026-10-15');
});

test('a retired day timer cannot load or overwrite another route', async () => {
  const h=harness({lifecycle:true}),first=h.pause(),reading=h.choose('api-a');first.resolve(payload('api-a',8));await reading;
  h.state.route='accounts';h.state.routeRevision++;h.nodes.get('#view').innerHTML='current accounts';
  await h.advance(10050);assert.equal(h.calls.length,1);assert.equal(h.html(),'current accounts');
});

test('a response delivered after the timeout cannot republish old numbers', async () => {
  const h=harness({lifecycle:true}),next=h.pause(),reading=h.choose('api-a');
  await h.advance(30000);assert.equal(h.calls[0].init.signal.aborted,true);
  next.resolve(payload('api-a',88));await reading;assert.match(h.html(),/读取超时/);assert.doesNotMatch(h.html(),/model-88|\$88/);assert.equal(h.viewState.data,null);
});

test('a restored browser page rereads instead of remaining on its suspended skeleton', async () => {
  const h=harness({lifecycle:true}),first=h.pause(),reading=h.choose('api-a');first.resolve(payload('api-a',9));await reading;
  h.emit('pagehide');assert.equal(h.viewState.data,null);
  const next=h.pause();h.emit('pageshow',{persisted:true});next.resolve(payload('api-a',10));await h.settle();
  assert.equal(h.calls.length,2);assert.match(h.html(),/model-10/);
});

test('actual shell route advances its epoch even when closing the panel', async () => {
  const shell=await readFile(new URL('../internal/console/web/shell.js',import.meta.url),'utf8');const start=shell.indexOf('route = async function ()'),end=shell.indexOf('\nfunction goHome',start);
  const state={route:'usage',routeRevision:1};let disposed=0,closed=0;
  const modal={innerHTML:'previous dialog'};
  const ctx=vm.createContext({route:null,S:state,location:{hash:''},PAGES:{},RENDER:{},$:selector=>selector==='#modalRoot'?modal:null,$$:()=>[],accountDailyDispose(){disposed++;},closePanel(){closed++;}});
  vm.runInContext(shell.slice(start,end),ctx);await ctx.route();assert.equal(state.routeRevision,2);assert.equal(state.route,'home');assert.equal(disposed,1);assert.equal(closed,1);
  assert.equal(modal.innerHTML,'');
});
