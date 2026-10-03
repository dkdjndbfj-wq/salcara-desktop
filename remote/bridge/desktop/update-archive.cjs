'use strict';
const fs = require('node:fs');
const zlib = require('node:zlib');
const path = require('node:path');

// The publisher emits USTAR. Validate the entire archive before invoking tar:
// no absolute paths, traversal, alternate streams, devices, links or PAX/GNU
// overrides. Bound expanded bytes as well as signed compressed bytes.
async function validateArchive(archive, signal) {
  signal?.throwIfAborted();
  const input = fs.createReadStream(archive);
  const gzip = zlib.createGunzip();
  input.on('error', (error) => gzip.destroy(error));
  input.pipe(gzip);
  let buffered = Buffer.alloc(0), remaining = 0, count = 0, expanded = 0, ended = false;
  const seen = new Map();
  function text(block, start, size) {
    const field = block.subarray(start, start + size);
    const zero = field.indexOf(0);
    return field.subarray(0, zero < 0 ? size : zero).toString('utf8');
  }
  try {
    for await (const chunk of gzip) {
      signal?.throwIfAborted();
      expanded += chunk.length;
      if (expanded > 4 * 1024 * 1024 * 1024) throw new Error('Expanded update archive too large');
      buffered = Buffer.concat([buffered, chunk]);
      while (buffered.length >= 512) {
        const block = buffered.subarray(0, 512); buffered = buffered.subarray(512);
        if (remaining) { remaining -= 512; continue; }
        if (block.every((byte) => byte === 0)) { ended = true; continue; }
        if (ended) throw new Error('Data after archive end');
        let sum = 0;
        for (let i = 0; i < 512; i++) sum += i >= 148 && i < 156 ? 32 : block[i];
        const expected = text(block, 148, 8).trim();
        if (!/^[0-7]+$/.test(expected) || sum !== Number.parseInt(expected, 8)) throw new Error('Invalid tar checksum');
        const sizeText = text(block, 124, 12).trim();
        if (!/^[0-7]+$/.test(sizeText)) throw new Error('Invalid tar size');
        const size = Number.parseInt(sizeText, 8);
        if (!Number.isSafeInteger(size) || size > 4 * 1024 * 1024 * 1024) throw new Error('Invalid tar size');
        const magic = text(block, 257, 6);
        if (magic !== 'ustar') throw new Error('Update archive must use USTAR');
        const type = block[156] === 0 ? '0' : String.fromCharCode(block[156]);
        if (type !== '0' && type !== '5') throw new Error('Archive links and extended entries are not allowed');
        if (type === '5' && size !== 0) throw new Error('Invalid directory entry');
        let name = [text(block, 345, 155), text(block, 0, 100)].filter(Boolean).join('/');
        if (name.startsWith('./')) name = name.slice(2);
        if (name === '.' || name === '') { if (type !== '5') throw new Error('Invalid root entry'); continue; }
        if (type === '5' && name.endsWith('/')) name = name.slice(0, -1);
        const segments = name.split('/');
        if (segments.some((segment) => !segment || segment === '.' || segment === '..' || /[\\:\x00-\x1f\x7f]/.test(segment)
          || /[ .]$/.test(segment) || /^(con|prn|aux|nul|com[1-9]|lpt[1-9])(?:\.|$)/i.test(segment))) throw new Error('Unsafe archive path');
        const normalized = name.toLowerCase();
        if (seen.has(normalized)) throw new Error('Duplicate archive path');
        for (let i = 1; i < segments.length; i++) if (seen.get(segments.slice(0, i).join('/').toLowerCase()) === '0') throw new Error('Archive file used as a directory');
        seen.set(normalized, type);
        if (++count > 100000) throw new Error('Too many archive entries');
        remaining = Math.ceil(size / 512) * 512;
      }
    }
    if (buffered.length || remaining || !ended || !count) throw new Error('Incomplete update archive');
  } finally { input.destroy(); gzip.destroy(); }
}

function assertPrivateTree(root) {
  const absolute = path.resolve(root);
  let at = absolute;
  for (;;) {
    const st = fs.lstatSync(at);
    if (st.isSymbolicLink() || !st.isDirectory()) throw new Error('Update folder cannot contain linked directories');
    const parent = path.dirname(at); if (parent === at) break; at = parent;
  }
}

module.exports = { validateArchive, assertPrivateTree };
