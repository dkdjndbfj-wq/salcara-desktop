'use strict';
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs'), os = require('node:os'), path = require('node:path'), crypto = require('node:crypto');
const { downloadTo, requireSpace } = require('../update-download.cjs');

function fixture(t) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'salcara-download-'));
  const saved = global.fetch;
  t.after(() => { global.fetch = saved; fs.rmSync(root, { recursive: true, force: true }); });
  const bytes = Buffer.from('this is a signed download payload');
  return { dest: path.join(root, 'payload.tar.gz'), bytes, options: { size: bytes.length, sha256: crypto.createHash('sha256').update(bytes).digest('hex'), retryDelay: 1, idleTimeout: 5000 } };
}
test('a dropped connection resumes with exact Range and If-Range, then verifies the full hash', async t => {
  const f = fixture(t); let requests = 0;
  global.fetch = async (_url, options) => {
    if (++requests === 1) {
      let reads = 0;
      return new Response(new ReadableStream({ pull(c) { if (++reads === 1) c.enqueue(f.bytes.subarray(0, 8)); else setTimeout(() => c.error(new Error('connection lost')), 5); } }), { headers: { ETag: '"fixture"' } });
    }
    assert.equal(options.headers.Range, 'bytes=8-'); assert.equal(options.headers['If-Range'], '"fixture"');
    return new Response(f.bytes.subarray(8), { status: 206, headers: { ETag: '"fixture"', 'Content-Range': `bytes 8-${f.bytes.length - 1}/${f.bytes.length}` } });
  };
  await downloadTo('https://example.test/pkg', f.dest, f.options);
  assert.equal(requests, 2); assert.deepEqual(fs.readFileSync(f.dest), f.bytes);
});
test('cancellation persists a resumable partial file across a fresh downloader instance', async t => {
  const f = fixture(t), controller = new AbortController();
  global.fetch = async (_url, options) => new Response(new ReadableStream({ start(c) { c.enqueue(f.bytes.subarray(0, 7)); options.signal.addEventListener('abort', () => c.error(options.signal.reason)); } }), { headers: { ETag: '"restart"' } });
  await assert.rejects(downloadTo('https://example.test/pkg', f.dest, { ...f.options, signal: controller.signal, onProgress(n) { if (n) controller.abort(); } }), { name: 'AbortError' });
  assert.equal(fs.statSync(f.dest).size, 7);
  global.fetch = async (_url, opts) => { assert.equal(opts.headers.Range, 'bytes=7-'); return new Response(f.bytes, { headers: { ETag: '"new"' } }); };
  await downloadTo('https://example.test/pkg', f.dest, f.options);
  assert.deepEqual(fs.readFileSync(f.dest), f.bytes, 'an ignored Range must overwrite, not append');
});
test('an idle server times out and retries only a bounded number of times', async t => {
  const f = fixture(t); let count = 0;
  global.fetch = async (_url, opts) => { count++; return new Promise((_, reject) => opts.signal.addEventListener('abort', () => reject(opts.signal.reason))); };
  await assert.rejects(downloadTo('https://example.test/pkg', f.dest, { ...f.options, attempts: 3, idleTimeout: 15 }), /超时/);
  assert.equal(count, 3);
});
test('signed mirrors are tried after transient HTTP errors; corrupt bytes never retry installation', async t => {
  const f = fixture(t), seen = [];
  global.fetch = async url => { seen.push(url); return url.includes('primary') ? new Response('', { status: 503 }) : new Response(f.bytes); };
  await downloadTo(['https://primary.test/pkg', 'https://cdn.test/pkg'], f.dest, f.options);
  assert.deepEqual(seen, ['https://primary.test/pkg', 'https://cdn.test/pkg']);
  fs.unlinkSync(f.dest); let attempts = 0;
  global.fetch = async () => { attempts++; return new Response(Buffer.alloc(f.bytes.length)); };
  await assert.rejects(downloadTo('https://primary.test/pkg', f.dest, f.options), /校验失败/);
  assert.equal(attempts, 1);
});
test('a valid complete cache needs no network; insufficient disk fails before extraction', async t => {
  const f = fixture(t); global.fetch = async () => new Response(f.bytes);
  await downloadTo('https://example.test/pkg', f.dest, f.options);
  global.fetch = () => { throw new Error('must not download'); };
  await downloadTo('https://example.test/pkg', f.dest, f.options);
  assert.throws(() => requireSpace(path.dirname(f.dest), 2000, () => ({ bavail: 1, bsize: 512 })), { code: 'ENOSPC' });
});
test('bad content ranges and redirected plaintext are rejected', async t => {
  const f = fixture(t);
  global.fetch = async () => new Response(f.bytes, { status: 206, headers: { 'Content-Range': `bytes 0-${f.bytes.length - 1}/${f.bytes.length}` } });
  await assert.rejects(downloadTo('https://example.test/pkg', f.dest, f.options), /续传范围/);
  global.fetch = async () => ({ url: 'http://example.test/pkg', ok: true });
  await assert.rejects(downloadTo('https://example.test/pkg', f.dest, f.options), /不安全地址/);
});
test('a zero-byte file left by interruption is replaced safely on retry', async t => {
  const f = fixture(t); fs.writeFileSync(f.dest, '');
  fs.writeFileSync(f.dest + '.resume.json', JSON.stringify({size:f.options.size,sha256:f.options.sha256,url:'https://example.test/pkg',etag:'"old"'}));
  global.fetch = async () => new Response(f.bytes);
  await downloadTo('https://example.test/pkg', f.dest, f.options);
  assert.deepEqual(fs.readFileSync(f.dest), f.bytes);
});
