'use strict';

// Static layout contracts complement the rendered viewport regression. They do
// not measure browser geometry or replace screenshot/interaction acceptance.
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const web = path.resolve(__dirname, '../../internal/console/web');
const html = fs.readFileSync(path.join(web, 'index.html'), 'utf8');
const css = fs.readFileSync(path.join(web, 'shell.css'), 'utf8');
const nav = /<nav\b[^>]*\bid="nav"[^>]*>([\s\S]*?)<\/nav>/.exec(html)?.[1];
assert.ok(nav, 'the actual persistent navigation is present');

function firstRule(selector) {
  const escaped = selector.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  const match = new RegExp(`${escaped}\\s*\\{([^}]+)\\}`).exec(css);
  assert.ok(match, `missing layout rule: ${selector}`);
  return match[1];
}

test('all ten primary destinations remain real, individually reachable links', () => {
  const links = [...nav.matchAll(/<a\b([^>]*)>([\s\S]*?)<\/a>/g)];
  const routes = links.map(([, attrs]) => /\bdata-route="([^"]+)"/.exec(attrs)?.[1]);
  assert.deepEqual(routes, ['home', 'overview', 'sessions', 'accounts', 'login', 'mcp', 'setup', 'plugins', 'usage', 'settings']);
  for (const [, attrs, content] of links) {
    assert.match(attrs, /\bhref="#(?:[a-z]+)?"/);
    assert.doesNotMatch(attrs, /\b(?:hidden|disabled)\b|tabindex="-1"/);
    assert.match(content, /<b>[^<]+<\/b>/, 'each destination retains a visible label');
  }
});

test('navigation has one count-independent row with bounded horizontal scrolling', () => {
  const rule = firstRule('.desktop-nav');
  assert.match(rule, /display:\s*flex\s*;/);
  assert.match(rule, /flex-wrap:\s*nowrap\s*;/);
  assert.match(rule, /min-width:\s*0\s*;/);
  assert.match(rule, /min-height:\s*0\s*;/);
  assert.match(rule, /overflow-x:\s*auto\s*;/);
  assert.match(rule, /overflow-y:\s*hidden\s*;/);
  assert.doesNotMatch(rule, /grid-template-columns|repeat\(/);
});

test('navigation keeps usable item widths and visible labels and focus in the scrolling row', () => {
  const link = firstRule('.desktop-nav a');
  assert.match(link, /flex:\s*1\s+0\s+72px\s*;/, 'desktop links grow but never shrink below the touch target');
  assert.match(link, /min-height:\s*44px\s*;/);
  assert.match(firstRule('.desktop-nav a b'), /white-space:\s*nowrap\s*;/);
  assert.match(firstRule('.desktop-nav a:focus-visible'), /outline-offset:\s*-4px\s*;/);
  const compact = css.slice(css.indexOf('@media (max-width: 700px)'));
  assert.match(compact, /\.desktop-nav a\s*\{[^}]*flex:\s*1\s+0\s+66px\s*;/);
  assert.doesNotMatch(css, /\.desktop-nav[^{}]*\{[^}]*flex-wrap:\s*wrap\s*;/);
});

test('the content row remains separate from navigation with and without an update banner', () => {
  assert.match(firstRule('.app-shell'), /grid-template:\s*52px\s+80px\s+minmax\(0,1fr\)\s+26px\s*\//);
  assert.match(firstRule('.app-shell:has(.desktop-update-banner:not([hidden]))'), /grid-template-rows:\s*52px\s+80px\s+auto\s+minmax\(0,1fr\)\s+26px\s*;/);
  assert.match(firstRule('.main'), /min-height:\s*0\s*;/);
  assert.match(firstRule('.main'), /overflow:\s*hidden\s*;/);
});
