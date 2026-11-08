'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

function fixture() {
  const context = vm.createContext({ ACTIONS: {}, document: { addEventListener() {} }, addEventListener() {} });
  vm.runInContext(fs.readFileSync(path.resolve(__dirname, '../../internal/console/web/account-test.js'), 'utf8'), context);
  return context;
}

test('SSE parser preserves Unicode and diagnostic events at every byte boundary', () => {
  const { accountTestParseEvents: parse } = fixture();
  const source = ': heartbeat\r\n\r\ndata: {"choices":[{"delta":{"content":"连接成功🙂"}}]}\r\n\r\ndata: [DONE]\r\n\r\nevent: diagnostic\r\ndata: {"state":\r\ndata: "completed"}\r\n\r\n';
  const bytes = new TextEncoder().encode(source);
  for (let boundary = 1; boundary < bytes.length; boundary++) {
    const decoder = new TextDecoder();
    let buffer = '', emitted = [];
    for (const chunk of [bytes.slice(0, boundary), bytes.slice(boundary)]) {
      buffer += decoder.decode(chunk, { stream: true });
      buffer = parse(buffer, (kind, data) => emitted.push([kind, data]));
    }
    buffer = parse(buffer + decoder.decode(), (kind, data) => emitted.push([kind, data]), true);
    assert.equal(buffer, '');
    assert.equal(emitted.length, 3);
    assert.equal(JSON.parse(emitted[0][1]).choices[0].delta.content, '连接成功🙂');
    assert.equal(emitted[1][1], '[DONE]');
    assert.equal(emitted[2][0], 'diagnostic');
    assert.equal(JSON.parse(emitted[2][1]).state, 'completed');
  }
});

test('SSE parser refuses truncated and oversized frames instead of confirming success', () => {
  const { accountTestParseEvents: parse } = fixture();
  let events = [];
  const partial = parse('data: [DONE]\n\nevent: diagnostic\ndata: {"state":"comple', (kind, data) => events.push([kind, data]));
  assert.equal(events.length, 1);
  assert.throws(() => parse(partial, () => {}, true), /中途断开/);
  assert.throws(() => parse('data: ' + 'x'.repeat(262144), () => {}), /过大/);
});

test('late send, stop and clear actions are harmless after the test window has closed', async () => {
  const { ACTIONS } = fixture();
  ACTIONS.accountTestClose();
  ACTIONS.accountTestStop();
  ACTIONS.accountTestClear();
  await ACTIONS.accountTestSend();
});
