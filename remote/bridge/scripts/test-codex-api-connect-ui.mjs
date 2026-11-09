import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
const source = fs.readFileSync(new URL('../internal/console/web/codex-api-connect.js', import.meta.url), 'utf8');
const context = vm.createContext({ URL, ACTIONS: {}, document: { addEventListener() {} }, esc: value => String(value).replaceAll('<','&lt;') });
vm.runInContext(source, context);
const base = 'http://127.0.0.1:9999/codex-api/v1';
test('five protocol configurations keep the correct SDK prefix, route and auth', () => {
  for (const [kind, suffix, auth] of [['chat','/v1','Authorization: Bearer'],['responses','/v1','Authorization: Bearer'],['anthropic','','x-api-key:'],['gemini','','x-goog-api-key:'],['ollama','','Authorization: Bearer']]) {
    const result = context.apiConnectionSpec(base, kind, 'team/my-model');
    assert.equal(result.baseUrl, 'http://127.0.0.1:9999/codex-api' + suffix);
    assert.match(result.example, new RegExp(auth));
    assert.match(result.example, /YOUR_SALCARA_CLIENT_KEY/);
    if (kind === 'gemini') assert.match(result.route, /models\/team%2Fmy-model:generateContent$/);
    if (kind === 'ollama') assert.match(result.example, /"stream":false/);
  }
});
test('URL credentials and unsupported URL shapes cannot be copied as a valid configuration', () => {
  for (const url of ['javascript:alert(1)', 'http://key:secret@localhost/v1', base + '?key=private', base + '#unsafe', 'http://localhost/not-api']) assert.throws(() => context.apiConnectionSpec(url, 'chat'));
  assert.throws(() => context.apiConnectionSpec(base, 'unknown'));
  assert.match(context.apiConnectionHTML({}), /接口地址待确认/);
});
test('model names are safely quoted in examples and only the explicit placeholder represents the key', () => {
  const result = context.apiConnectionSpec(base, 'chat', "model'$(not-a-command)");
  assert.match(result.example, /'\\''/);
  assert.match(result.example, /YOUR_SALCARA_CLIENT_KEY/);
  const html = context.apiConnectionHTML({ baseUrl: base });
  assert.match(html, /不会被自动重置/);
  assert.doesNotMatch(html, /apiClientAccount|选择上游/);
});
