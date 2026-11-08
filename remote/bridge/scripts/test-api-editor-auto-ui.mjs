import test from 'node:test';
import assert from 'node:assert/strict';
import {readFile} from 'node:fs/promises';
import vm from 'node:vm';
const source=await readFile(new URL('../internal/console/web/app.js',import.meta.url),'utf8');
const transport=source.slice(source.indexOf('function localTransportForDraft('),source.indexOf('function toolForm('));
function setup(prior=null) {
  const values=new Map();
  const context=vm.createContext({URL,S:{localDraftTransport:prior,localDraftModels:[]},$:id=>values.get(id)||{value:''}});
  vm.runInContext(transport,context);
  return {context, values, get:url=>JSON.parse(JSON.stringify(context.localTransportForDraft(url)))};
}
test('API editing exposes explicit protocols while retaining automatic provider detection',()=>{
  const editor=source.slice(source.indexOf('function localEditor('),source.indexOf('function localTransportForDraft('));
  assert.match(editor,/data-editor-wire/);
  assert.match(editor,/id="laWire" data-native hidden/);
  assert.match(editor,/id="laAuth"/);
  const h=setup();
  assert.deepEqual(h.get('https://api.anthropic.com/v1'),{authMode:'api-key',wire:'anthropic'});
  assert.deepEqual(h.get('https://api.deepseek.com'),{authMode:'bearer',wire:'chat'});
  for(const url of ['https://salcara.top/v1','https://api.anthropic.com.evil.test','https://evil.test/?api.anthropic.com','invalid']) assert.deepEqual(h.get(url),{authMode:'bearer',wire:'auto'});
});
test('editing name or key preserves working explicit protocol and authentication',()=>{
  const h=setup({baseUrl:'https://api.example.test',authMode:'api-key',wire:'chat'});
  assert.deepEqual(h.get('https://api.example.test/v1/'),{authMode:'api-key',wire:'chat'});
  assert.deepEqual(h.get('https://different.example.test'),{authMode:'bearer',wire:'auto'});
  assert.deepEqual(h.get('https://api.example.test/other'),{authMode:'bearer',wire:'auto'});
  assert.deepEqual(h.get('https://api.example.test'),{authMode:'api-key',wire:'chat'});
});
test('new form resolves automatic choices and preserves explicit protocol selections',()=>{
  const h=setup();
  h.values.set('#laBase',{value:'https://API.ANTHROPIC.COM/v1'});
  h.values.set('#laName',{value:'fixture'});
  const form=h.context.localForm();
  assert.equal(form.authMode,'api-key');assert.equal(form.wire,'anthropic');assert.equal(form.name,'fixture');
  h.values.set('#laWire',{value:'chat'});
  h.values.set('#laAuth',{value:'bearer'});
  assert.equal(h.context.localForm().wire,'chat');assert.equal(h.context.localForm().authMode,'bearer');
});
