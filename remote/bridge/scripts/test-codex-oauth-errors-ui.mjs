import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

const source = await readFile(new URL('../internal/console/web/codex-accounts.js', import.meta.url), 'utf8');
const context = vm.createContext({
  URL, TextEncoder, ACTIONS: {}, S: {}, $: () => ({}), icon: () => '', esc: value => String(value),
  document: { addEventListener() {} }, MutationObserver: class { observe() {} },
});
vm.runInContext(source, context);
const render = row => context.officialLoginProgressHTML(row);

test('blocked callback ports offer device authorization without revealing diagnostics', () => {
  const html = render({ status: 'failed', errorCode: 'callback_unavailable', error: 'private bind details' });
  assert.match(html, /本机回调不可用/);
  assert.match(html, /设备码授权/);
  assert.doesNotMatch(html, /private bind details/);
});

test('expired login offers a new authorization and cannot retain stale links', () => {
  const html = render({ status: 'failed', errorCode: 'expired', authorizationURL: 'https://auth.openai.com/oauth/authorize?state=old', userCode: 'OLD' });
  assert.match(html, /授权已过期/);
  assert.doesNotMatch(html, /href|state=|OLD/);
});

test('browser authorization explains automatic return and manual callback recovery', () => {
  const html = render({ status: 'pending', mode: 'browser', id: 'synthetic', authorizationURL: 'https://auth.openai.com/oauth/authorize?state=synthetic' });
  assert.match(html, /等待浏览器完成登录/);
  assert.match(html, /自动连接并添加账号/);
  assert.match(html, /localhost 回调地址/);
  assert.match(html, /打开授权页/);
});

test('device authorization provides a one-time code and says when to use it', () => {
  const html = render({ status: 'pending', mode: 'device', id: 'synthetic', verificationURL: 'https://auth.openai.com/codex/device', userCode: 'SYNTHETIC-CODE' });
  assert.match(html, /等待设备码确认/);
  assert.match(html, /一次性设备码/);
  assert.match(html, /SYNTHETIC-CODE/);
  assert.match(html, /不依赖本机回调/);
  assert.match(html, /打开授权页/);
});

test('authorization start errors are actionable and never expose provider diagnostics', () => {
  const html = context.officialLoginStartFailureHTML(new Error('<secret> synthetic timeout details'));
  assert.match(html, /授权启动失败/);
  assert.match(html, /检查网络后重试/);
  assert.doesNotMatch(html, /<secret>|synthetic timeout details/);
});

test('browser and device authorization show the correct connection startup state', () => {
  const browser = context.officialLoginStartingHTML('browser');
  const device = context.officialLoginStartingHTML('device');
  assert.match(browser, /正在连接授权服务/);
  assert.match(browser, /准备安全登录页面/);
  assert.match(device, /正在申请一次性设备码/);
  assert.match(device, /即将显示授权页和设备码/);
});

test('unknown codes and provider text never become UI markup or classify the failure', () => {
  const error = '<script>synthetic-token</script> callback_unavailable expired';
  assert.match(render({ status: 'failed', errorCode: error, error }), /登录失败/);
  assert.match(render({ status: 'failed', error }), /登录失败/);
  assert.match(render({ status: 'cancelled', errorCode: 'expired', error }), /已取消/);
  assert.match(render({ status: 'complete', errorCode: 'callback_unavailable', error }), /已登录/);
  for (const html of [render({ status: 'failed', errorCode: error, error }), render({ status: 'failed', error }), render({ status: 'cancelled', errorCode: 'expired', error }), render({ status: 'complete', errorCode: 'callback_unavailable', error })]) {
    assert.doesNotMatch(html, /<script>|synthetic-token|callback_unavailable|expired/);
  }
});
