'use strict';
const fs = require('node:fs');
const crypto = require('node:crypto');
const { setTimeout: delay } = require('node:timers/promises');

class DownloadError extends Error {
  constructor(message, permanent = false) { super(message); this.name = 'DownloadError'; this.updatePermanent = permanent; }
}
function regularFile(file) {
  try { const stat = fs.lstatSync(file); if (!stat.isFile() || stat.isSymbolicLink()) throw new DownloadError('更新缓存不是普通文件', true); return stat; }
  catch (error) { if (error.code === 'ENOENT') return null; throw error; }
}
function saveJSON(file, value) {
  const temporary = `${file}.${crypto.randomBytes(8).toString('hex')}.tmp`;
  let fd;
  try {
    fd = fs.openSync(temporary, 'wx', 0o600);
    fs.writeFileSync(fd, JSON.stringify(value)); fs.fsyncSync(fd); fs.closeSync(fd); fd = undefined;
    fs.renameSync(temporary, file);
  } finally { if (fd !== undefined) fs.closeSync(fd); try { fs.unlinkSync(temporary); } catch {} }
}
async function hashFile(file, signal) {
  regularFile(file);
  const hash = crypto.createHash('sha256');
  for await (const chunk of fs.createReadStream(file, { signal })) hash.update(chunk);
  return hash.digest('hex');
}
function secureURL(value) {
  try { const u = new URL(value); return u.protocol === 'https:' && !u.username && !u.password && !u.hash; } catch { return false; }
}

// Only resume a regular file tied to this signed payload and a strong ETag.
// A server ignoring Range is a safe full restart, never an appended 200 body.
async function downloadTo(urls, dest, options) {
  const { size, sha256, signal, onProgress = () => {}, onRetry = () => {}, attempts = 4,
    idleTimeout = 30000, totalTimeout = 45 * 60 * 1000, retryDelay = 1000 } = options;
  urls = Array.isArray(urls) ? urls : [urls];
  if (!urls.length || urls.some(url => !secureURL(url))) throw new DownloadError('更新下载地址无效', true);
  const metaPath = `${dest}.resume.json`;
  let meta = null;
  try { if (regularFile(metaPath)?.size <= 4096) meta = JSON.parse(fs.readFileSync(metaPath, 'utf8')); } catch (e) { if (e.updatePermanent) throw e; }
  const matches = meta?.size === size && meta?.sha256 === sha256;
  const existing = regularFile(dest);
  if (existing?.size === size && matches && await hashFile(dest, signal) === sha256) { onProgress(size); return; }
  if (existing && (!matches || existing.size >= size)) fs.unlinkSync(dest);
  const deadline = Date.now() + totalTimeout;
  for (let attempt = 0; attempt < attempts; attempt++) {
    signal?.throwIfAborted();
    const url = urls[attempt % urls.length];
    const cachedStat = regularFile(dest);
    let received = cachedStat?.size || 0;
    const resumable = received > 0 && meta?.url === url && meta?.sha256 === sha256 && meta?.size === size && /^"[^"\r\n]+"$/.test(meta?.etag || '');
    if (!resumable && cachedStat) { fs.unlinkSync(dest); received = 0; }
    const controller = new AbortController();
    const abort = () => controller.abort(signal.reason);
    signal?.addEventListener('abort', abort, { once: true });
    let timer, totalTimer, reader, out;
    const timedOut = () => controller.abort(new DownloadError('下载连接超时，正在恢复下载'));
    const touch = () => { clearTimeout(timer); timer = setTimeout(timedOut, idleTimeout); };
    touch(); totalTimer = setTimeout(timedOut, Math.max(1, deadline - Date.now()));
    try {
      const headers = { 'User-Agent': 'SalcaraBridge', 'Accept-Encoding': 'identity' };
      if (resumable) { headers.Range = `bytes=${received}-`; headers['If-Range'] = meta.etag; }
      const res = await fetch(url, { headers, signal: controller.signal, redirect: 'follow' });
      if (res.url && !secureURL(res.url)) throw new DownloadError('更新下载被重定向到不安全地址', true);
      if (!res.ok || !res.body) {
        await res.body?.cancel();
        if (res.status === 416 && received) { fs.unlinkSync(dest); meta = null; throw new DownloadError('下载缓存已变化，重新下载'); }
        throw new DownloadError(`下载服务返回 HTTP ${res.status}`, ![408, 429].includes(res.status) && res.status < 500);
      }
      reader = res.body.getReader();
      const etag = res.headers.get('etag');
      if (res.status === 206) {
        const range = /^bytes (\d+)-(\d+)\/(\d+)$/.exec(res.headers.get('content-range') || '');
        if (!resumable || !range || Number(range[1]) !== received || Number(range[2]) !== size - 1 || Number(range[3]) !== size || etag !== meta.etag) {
          throw new DownloadError('服务器返回了不一致的续传范围', true);
        }
      } else if (res.status === 200) { if (received) { fs.unlinkSync(dest); received = 0; } }
      else throw new DownloadError('更新下载响应无效', true);
      const length = res.headers.get('content-length');
      if (length !== null && Number(length) !== size - received) throw new DownloadError('下载文件大小与签名不符', true);
      if (res.headers.get('content-encoding') && res.headers.get('content-encoding') !== 'identity') throw new DownloadError('下载服务错误地转换了安装包', true);
      meta = { size, sha256, url, etag: /^"[^"\r\n]+"$/.test(etag || '') ? etag : '' };
      saveJSON(metaPath, meta);
      out = await fs.promises.open(dest, received ? 'a' : 'wx', 0o600);
      onProgress(received); touch();
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        touch(); received += value.length;
        if (received > size) throw new DownloadError('下载文件超过签名大小', true);
        let offset = 0;
        while (offset < value.length) {
          const result = await out.write(value, offset, value.length - offset);
          if (!result.bytesWritten) throw new DownloadError('无法写入更新缓存', true);
          offset += result.bytesWritten;
        }
        onProgress(received);
      }
      await out.sync(); await out.close(); out = null;
      if (received !== size) throw new DownloadError('下载连接中断，正在恢复下载');
      if (await hashFile(dest, signal) !== sha256) throw new DownloadError('安装包校验失败，已拒绝安装', true);
      return;
    } catch (error) {
      signal?.throwIfAborted();
      if (['ENOSPC', 'EACCES', 'EPERM', 'EROFS'].includes(error.code)) error.updatePermanent = true;
      if (error.updatePermanent || attempt + 1 >= attempts || Date.now() >= deadline) throw error;
      onRetry({ attempt: attempt + 1, attempts, delay: retryDelay * 2 ** attempt });
    } finally {
      clearTimeout(timer); clearTimeout(totalTimer); signal?.removeEventListener('abort', abort);
      await reader?.cancel().catch(() => {}); await out?.close();
    }
    await delay(retryDelay * 2 ** attempt, undefined, { signal });
  }
}

function requireSpace(directory, bytes, statfs = fs.statfsSync) {
  const info = statfs(directory);
  if (Number(info.bavail) * Number(info.bsize) < bytes) {
    const error = new DownloadError(`磁盘空间不足，请至少腾出 ${Math.ceil(bytes / 1048576)} MB 后重试`, true);
    error.code = 'ENOSPC'; throw error;
  }
}
module.exports = { downloadTo, hashFile, requireSpace, saveJSON, secureURL, DownloadError };
