'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.resolve(__dirname, '../../internal/console/web/shell.js'), 'utf8');
const start = source.indexOf("addEventListener('pointermove'");
const end = source.indexOf('\n(function blinkLoop', start);
assert.ok(start >= 0 && end > start, 'orb tracking block exists');

function fixture(reduced = false) {
  const events = new Map(), frames = [], styles = new Map();
  let nextFrame = 0, rectReads = 0, panelOpen = false;
  const document = {
    hidden: false,
    body: { classList: { contains: (name) => name === 'panel-open' && panelOpen } },
    addEventListener: (type, handler) => events.set(`document:${type}`, handler),
  };
  const orb = {
    classList: { add() {}, remove() {} },
    getBoundingClientRect() { rectReads += 1; return { left: 400, top: 200, width: 224, height: 224 }; },
    style: { setProperty: (name, value) => styles.set(name, value) },
  };
  const face = { style: { setProperty: (name, value) => styles.set(name, value) } };
  const context = vm.createContext({
    matchMedia: () => ({ matches: reduced, addEventListener() {} }),
    SHELL: { mouse: { x: 512, y: 384 }, eye: { x: 0, y: 0 }, tilt: { x: 0, y: 0 } },
    orb, document,
    $: (selector) => selector === '#orbFace' ? face : null,
    addEventListener: (type, handler) => events.set(type, handler),
    requestAnimationFrame: (callback) => { const id = ++nextFrame; frames.push({ id, callback }); return id; },
    cancelAnimationFrame: (id) => { const index = frames.findIndex((frame) => frame.id === id); if (index >= 0) frames.splice(index, 1); },
  });
  vm.runInContext(source.slice(start, end), context, { filename: 'shell.js' });
  return {
    context, events, frames, styles,
    rectReads: () => rectReads,
    setPanelOpen: (value) => { panelOpen = value; },
    flush(limit = 100) {
      let count = 0;
      while (frames.length) {
        assert.ok(count < limit, 'eye tracking should settle instead of scheduling frames forever');
        frames.shift().callback(); count += 1;
      }
      return count;
    },
  };
}

test('orb eye tracking settles and waits for a new pointer position between animations', () => {
  const f = fixture();
  const initialFrames = f.flush();
  assert.ok(initialFrames > 0 && initialFrames < 100);
  assert.equal(f.frames.length, 0, 'idle home screen should not keep an animation-frame loop alive');
  assert.equal(f.rectReads(), 1, 'layout is read once while the pointer target stays still');

  f.events.get('pointermove')({ clientX: 900, clientY: 120 });
  const movedFrames = f.flush();
  assert.ok(movedFrames > 0 && movedFrames < 100);
  assert.equal(f.frames.length, 0);
  assert.equal(f.rectReads(), 2, 'a new pointer target measures the orb only once');
  assert.ok(f.styles.has('--ex'));
});

test('visibility changes cancel hidden work and schedule one bounded resume', () => {
  const f = fixture(); f.flush();
  f.context.document.hidden = true;
  f.events.get('document:visibilitychange')();
  assert.equal(f.frames.length, 0);
  f.context.document.hidden = false;
  f.events.get('document:visibilitychange')();
  assert.ok(f.frames.length <= 1);
  f.flush();
  assert.equal(f.frames.length, 0);
});

test('reduced motion avoids pointer-driven animation frames', () => {
  const f = fixture(true);
  f.events.get('pointermove')({ clientX: 900, clientY: 120 });
  f.events.get('resize')();
  assert.equal(f.frames.length, 0);
  assert.equal(f.rectReads(), 0);
});
