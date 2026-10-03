// Loopback-only, no real credentials or paid inference. Used for launcher UI QA.
const http = require('node:http');
const server = http.createServer((req, res) => {
  res.setHeader('Content-Type', 'application/json');
  if (req.method === 'GET' && req.url === '/v1/models') {
    res.end(JSON.stringify({ data: [{ id: 'smoke-codex', object: 'model' }, { id: 'smoke-claude', object: 'model' }] }));
    return;
  }
  res.statusCode = 501;
  res.end(JSON.stringify({ error: { message: 'Local launcher smoke fixture: inference intentionally disabled', type: 'not_implemented' } }));
});
server.listen(47833, '127.0.0.1', () => console.log('Local smoke provider: http://127.0.0.1:47833'));
process.on('SIGINT', () => server.close());
