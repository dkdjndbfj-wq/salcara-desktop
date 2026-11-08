import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
const source=fs.readFileSync(new URL('../internal/console/web/launch-progress.js',import.meta.url),'utf8');
test('progress remains below desktop window controls',()=>{
  const css=fs.readFileSync(new URL('../internal/console/web/launch-progress.css',import.meta.url),'utf8');
  const shell=fs.readFileSync(new URL('../internal/console/web/shell.css',import.meta.url),'utf8');
  const overlay=Number(css.match(/\.launch-progress-back\{[^}]*z-index:(\d+)/)?.[1]);
  const controls=Number(shell.match(/\.win-ctl\s*\{[^}]*z-index:\s*(\d+)/)?.[1]);
  assert.ok(overlay>60&&overlay<controls);
});
function fixture(){
  const roots=[],events=new Map(),nav=new Map();
  const esc=value=>String(value).replaceAll('&','&amp;').replaceAll('<','&lt;').replaceAll('"','&quot;');
  class Node {
    constructor(tag){this.tagName=tag;this.attributes=new Map();this.children=[];this.parentNode=null;this.isConnected=false;this.className='';this.text='';this.raw='';this.style={};this.hidden=false;this.listeners=new Map();}
    setAttribute(name,value){this.attributes.set(name,String(value));}
    appendChild(child){child.parentNode=this;child.isConnected=this.isConnected;this.children.push(child);return child;}
    addEventListener(name,fn){this.listeners.set(name,fn);}
    focus(){document.activeElement=this;}
    contains(node){return this===node||this.children.some(child=>child.contains(node));}
    remove(){this.isConnected=false;if(this.parentNode)this.parentNode.children=this.parentNode.children.filter(child=>child!==this);}
    scrollIntoView(){}
    set textContent(value){this.children=[];this.raw='';this.text=String(value);}
    get textContent(){return this.text+this.children.map(child=>child.textContent).join('');}
    set innerHTML(value){this.children=[];this.text='';this.raw=String(value);}
    get innerHTML(){return this.raw+this.children.map(serialize).join('');}
    querySelector(selector){
      const match=node=>selector==='button'?node.tagName==='button':selector==='h2'?node.tagName==='h2':selector.startsWith('[data-launch-step=')?node.attributes.get('data-launch-step')===selector.match(/="?([^"\]]+)/)?.[1]:false;
      for(const child of this.children){if(match(child))return child;const nested=child.querySelector(selector);if(nested)return nested;}return null;
    }
  }
  function serialize(node){
    let attrs=new Map(node.attributes);if(node.className){attrs.delete('class');attrs=new Map([['class',node.className],...attrs]);}if(node.hidden)attrs.set('hidden','');
    const css=Object.entries(node.style).filter(([,value])=>value!==''&&value!==undefined).map(([name,value])=>`${name.replace(/[A-Z]/g,letter=>'-'+letter.toLowerCase())}:${value}`).join(';');
    if(css)attrs.set('style',css);
    const attributes=[...attrs].map(([name,value])=>value===''?` ${name}`:` ${name}="${esc(value)}"`).join('');
    return `<${node.tagName}${attributes}>${node.raw||esc(node.text)}${node.children.map(serialize).join('')}</${node.tagName}>`;
  }
  const document={activeElement:null,body:{appendChild(root){roots.push(root);root.isConnected=true;const mark=node=>{node.isConnected=true;node.children.forEach(mark);};mark(root);}},
    createElement(tag){return new Node(tag);},
    addEventListener(name,fn){events.set(name,fn);},removeEventListener(name,fn){if(events.get(name)===fn)events.delete(name);}};
  const window={addEventListener(name,fn){nav.set(name,fn);},removeEventListener(name,fn){if(nav.get(name)===fn)nav.delete(name);}};
  const context=vm.createContext({document,window,esc:value=>String(value).replaceAll('&','&amp;').replaceAll('<','&lt;').replaceAll('"','&quot;'),icon:()=>'',setTimeout(){throw Error('fake progress timer');},api(){throw Error('progress must not send API requests');}});
  vm.runInContext(source,context);
  return {context,document,events,nav,roots,begin:(labels=['检查来源','保存选择','应用并启动'])=>context.beginLaunchProgress('codex','<demo@example.test>',labels),html:()=>serialize(roots.at(-1))};
}
test('launch steps advance from explicit acknowledgements, not timers or percentages',()=>{
  const f=fixture(),run=f.begin();assert.match(f.html(),/0 \/ 3/);assert.match(f.html(),/进行中/);assert.doesNotMatch(f.html(),/<demo@/);
  run.step(1);assert.match(f.html(),/1 \/ 3/);run.step(0);assert.match(f.html(),/1 \/ 3/);run.step(2);run.finish();assert.match(f.html(),/3 \/ 3/);assert.match(f.html(),/启动请求已完成/);
  run.fail('late');assert.doesNotMatch(f.html(),/late/);
});
test('phase updates preserve the fixed dialog shell and step nodes',()=>{
  const f=fixture(),run=f.begin(),root=f.roots[0],close=root.querySelector('button'),heading=root.querySelector('h2'),firstStep=root.querySelector('[data-launch-step="local-0"]');
  run.step(1);
  assert.equal(f.roots[0],root);assert.equal(root.querySelector('button'),close);assert.equal(root.querySelector('h2'),heading);assert.equal(root.querySelector('[data-launch-step="local-0"]'),firstStep);
  assert.match(f.html(),/1 \/ 3/);
});
test('closing progress does not cancel work and late updates cannot reopen it',()=>{
  const f=fixture(),run=f.begin();let prevented=0,stopped=0;
  f.events.get('keydown')({key:'Escape',preventDefault(){prevented++;},stopImmediatePropagation(){stopped++;}});
  assert.equal(prevented,1);assert.equal(stopped,1);assert.equal(f.roots[0].isConnected,false);
  run.step(2);run.finish();assert.equal(f.roots.length,1);assert.equal(f.events.size,0);assert.equal(f.nav.size,0);
});
test('replaced operation cannot paint into a newer launch',()=>{
  const f=fixture(),old=f.begin(),current=f.begin(['准备','启动']);old.fail('old failure');assert.doesNotMatch(f.html(),/old failure/);
  assert.equal(f.roots[0].isConnected,false);current.step(1);current.fail('<unsafe>');assert.match(f.html(),/&lt;unsafe>/);assert.match(f.html(),/1 \/ 2/);
  current.finish();assert.doesNotMatch(f.html(),/2 \/ 2/);
});
test('navigation hides presentation and cancelled pre-dispatch phase is not successful',()=>{
  const f=fixture(),run=f.begin();run.cancel();assert.match(f.html(),/未继续/);assert.match(f.html(),/0 \/ 3/);
  f.nav.get('hashchange')();assert.equal(f.roots[0].isConnected,false);
});

test('backend plan keeps every phase and failure stays on the active operation',()=>{
  const f=fixture(),run=f.begin(['提交启动请求']);
  run.event({type:'plan',steps:Array.from({length:14},(_,i)=>({id:'s'+i,label:'真实步骤 '+i}))});
  run.event({type:'phase',step:'s0',label:'核对来源'});
  run.event({type:'phase',step:'s1',label:'等待原工具退出'});
  assert.match(f.html(),/真实步骤 13/);assert.match(f.html(),/2 \/ 15/);
  run.fail(new Error('窗口仍在运行'));
  assert.match(f.html(),/class="failed" data-launch-step="s1"/);
  assert.match(f.html(),/窗口仍在运行/);assert.doesNotMatch(f.html(),/启动请求已完成/);
});

test('substeps update the current row without prematurely finishing it',()=>{
  const f=fixture(),run=f.begin([]);
  run.event({type:'phase',step:'profile',label:'写入配置',detail:'写入连接'});
  run.event({type:'phase',step:'profile',label:'写入配置',detail:'同步目录'});
  assert.match(f.html(),/0 \/ 1/);assert.match(f.html(),/同步目录/);assert.doesNotMatch(f.html(),/写入连接/);
  run.event({type:'phase',step:'start',label:'创建进程'});assert.match(f.html(),/1 \/ 2/);
});

test('fallback warnings persist and unexecuted future phases are not successful',()=>{
  const f=fixture(),run=f.begin([]);
  run.event({type:'plan',steps:[{id:'catalog',label:'读取模型'},{id:'confirm',label:'启动确认'}]});
  run.event({type:'phase',step:'catalog',state:'warning',detail:'HTTP 503，使用已保存目录'});
  run.finish();assert.match(f.html(),/有提示/);assert.match(f.html(),/HTTP 503/);
  assert.match(f.html(),/class="skipped" data-launch-step="confirm"/);
});

test('an uncertain connection loss never claims the remaining operations did not execute',()=>{
  const f=fixture(),run=f.begin([]);
  run.event({type:'plan',steps:[{id:'start',label:'创建进程'},{id:'record',label:'保存状态'}]});
  run.event({type:'phase',step:'start'});run.fail(Object.assign(new Error('连接中断'),{uncertain:true}));
  assert.match(f.html(),/启动结果未确认/);assert.match(f.html(),/class="unknown" data-launch-step="start"/);
  assert.match(f.html(),/class="waiting" data-launch-step="record"/);assert.doesNotMatch(f.html(),/未继续/);
});

function transport(records, options={}){
  let calls=0;const events=[];
  const text=records.map((event,index)=>JSON.stringify({sequence:index+1,event})).join('\n')+'\n';
  const bytes=new TextEncoder().encode(text);
  const context=vm.createContext({AbortController,TextDecoder,Error,TypeError,SyntaxError,setTimeout:()=>1,clearTimeout(){},
    fetch:async()=>{calls++;return options.response||new Response(new ReadableStream({start(controller){
      // Split UTF-8 and JSON records to exercise the actual streaming decoder.
      for(let at=0;at<bytes.length;at+=7)controller.enqueue(bytes.slice(at,at+7));controller.close();
    }}),{headers:{'Content-Type':'application/x-ndjson'}});}});
  vm.runInContext(source,context);
  return {run:()=>context.launchAPI('/synthetic/launch',{id:'fixture'},{event:e=>events.push(e)}),events,calls:()=>calls};
}
test('stream decoder preserves chunked phases and starts only one POST',async()=>{
  const f=transport([{type:'phase',step:'stop',label:'等待原工具退出'},{type:'result',status:200,data:{ok:true,result:{pid:1}}}]);
  assert.equal((await f.run()).result.pid,1);assert.equal(f.events[0].label,'等待原工具退出');assert.equal(f.calls(),1);
});
test('stream preserves authoritative already-started failure',async()=>{
  const f=transport([{type:'phase',step:'record'},{type:'result',status:409,data:{error:'保存失败',started:true,result:{pid:1}}}]);
  await assert.rejects(f.run(),e=>e.started===true&&e.result.pid===1&&e.message==='保存失败');assert.equal(f.calls(),1);
});
test('EOF without terminal receipt is uncertain and never replayed',async()=>{
  const f=transport([{type:'phase',step:'start'}]);await assert.rejects(f.run(),e=>e.uncertain===true);assert.equal(f.calls(),1);
});
test('JSON compatibility and authorization errors keep their actual status',async()=>{
  const f=transport([],{response:new Response(JSON.stringify({error:'会话已过期'}),{status:401,headers:{'Content-Type':'application/json'}})});
  await assert.rejects(f.run(),/会话已过期/);assert.equal(f.calls(),1);
});
