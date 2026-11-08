'use strict';
// Aggregate explicitly exported diagnostics. No collection or network upload.
const fs = require('node:fs');
const crypto = require('node:crypto');
function summarize(files) {
  const seen = new Set(), counts = {}, versions = {};
  for (const file of files) {
    if (fs.statSync(file).size > 1024 * 1024) throw new Error('Diagnostics export is too large');
    const bytes = fs.readFileSync(file), digest = crypto.createHash('sha256').update(bytes).digest('hex');
    if (seen.has(digest)) continue; seen.add(digest);
    const data = JSON.parse(bytes);
    if (data.schema !== 1 || data.product !== 'salcara-desktop-update-diagnostics' || !Array.isArray(data.events) || data.events.length > 500) throw new Error('Invalid diagnostics export');
    for (const event of data.events) {
      if (!['installed', 'rolled_back', 'download_failed', 'download_retry', 'quarantined', 'manual_retry'].includes(event.event)) continue;
      counts[event.event] = (counts[event.event] || 0) + 1;
      if (/^\d+\.\d+\.\d+$/.test(event.version || '')) {
        const target = versions[event.version] ||= {};
        target[event.event] = (target[event.event] || 0) + 1;
      }
    }
  }
  return { exports: seen.size, counts, versions, scope: 'Only supplied diagnostic samples; not a fleet-wide success rate' };
}
if (require.main === module) {
  try { if (!process.argv[2]) throw new Error('Usage: node scripts/update-report.cjs diagnostics.json [...]'); console.log(JSON.stringify(summarize(process.argv.slice(2)), null, 2)); }
  catch (error) { console.error(error.message); process.exitCode = 1; }
}
module.exports = { summarize };
