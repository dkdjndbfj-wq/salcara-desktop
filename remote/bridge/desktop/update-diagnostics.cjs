'use strict';
const fs = require('node:fs');
const path = require('node:path');

// Local, bounded and content-free. Never record tasks, paths, URLs, tokens or
// machine identity. Export is an explicit user action; there is no silent upload.
function createDiagnostics(userData, current) {
  const file = path.join(userData, 'update-events.jsonl');
  function record(event, detail = {}) {
    const entry = { time: new Date().toISOString(), event, current };
    for (const key of ['version', 'code', 'attempt', 'durationMs']) {
      const value = detail[key];
      if (typeof value === 'number' || (typeof value === 'string' && /^[a-zA-Z0-9_.-]{1,64}$/.test(value))) entry[key] = value;
    }
    try {
      fs.mkdirSync(userData, { recursive: true });
      if (fs.existsSync(file) && fs.statSync(file).size > 256 * 1024) fs.renameSync(file, `${file}.previous`);
      fs.appendFileSync(file, JSON.stringify(entry) + '\n', { mode: 0o600 });
    } catch { /* diagnostics must not disrupt the task */ }
  }
  function snapshot() {
    const events = [];
    for (const source of [`${file}.previous`, file]) {
      try {
        if (fs.statSync(source).size > 512 * 1024) continue;
        for (const line of fs.readFileSync(source, 'utf8').split('\n')) { try { events.push(JSON.parse(line)); } catch {} }
      } catch {}
    }
    return { schema: 1, product: 'salcara-desktop-update-diagnostics', current, events: events.slice(-500) };
  }
  return { record, snapshot };
}
module.exports = { createDiagnostics };
